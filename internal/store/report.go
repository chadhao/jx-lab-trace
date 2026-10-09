package store

// ===== M9 报告分享 · 持久化（D1 生成 / D2 刷新 / D3 生命周期 / D4 访问日志同步）=====
//
// ★★ report_no 非追踪码（§6-1）：RP + YYMMDD + '-' + 当日 3 位序号，取号 =
//	GET_LOCK 命名锁 + 同一事务前缀取最大 +1（禁「锁外先查 max 再 +1」），超 999
//	明确报错不进位（ErrReportSeqOverflow）—— 与 M7 nextShipNo 同一手法。
// ★★ token：crypto/rand 32 字节 base64url（43 字符，≥32）；uk_report_token 冲突
//	重试 ≤5 次；★ 不得用时间戳 / 序号 / report_no 派生（可猜测 = 链接可枚举）。
// ★★ 有效性 = 文件在不在 served/（§6-2）：撤销 / 过期 ⇒ 快照移入 _inactive/
//	⇒ 公网静态服务天然 404，不依赖查库。离开「有效」态一律**先移文件再改库**
//	（失败即中止，fail-closed），改库失败则把文件移回（undo）。
// ★★ 刷新 = 新行新 token 新链接 + 旧行置已撤销 + 旧快照移出（§6-4），
//	★ 绝不改同一行 / 复用同一 token。
// ★★ expires_at 语义 =「到期日（含）」存该日 23:59:59.999，判过期 = now > expires_at
//	（§6-6）；缺省 = 生成时刻 + 30 天（同一语义，缺省不产生 NULL）。
// ★★ 访问日志同步用 **byte offset**（§6-8）：reportd 按 O_APPEND 追加、同步只推进
//	偏移，★ 不 rename 日志文件 —— rename 会让 reportd 持旧 fd 继续写 ⇒ 丢行。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/chadhao/jx-lab-trace/internal/codec"
)

// M9 的哨兵错误（httpapi 据此映射状态码）。
var (
	ErrReportNotFound    = errors.New("报告不存在")
	ErrReportBadInput    = errors.New("报告输入不合法")
	ErrReportState       = errors.New("报告当前状态不允许该操作")
	ErrReportSeqBusy     = errors.New("报告编号取号繁忙，请重试")
	ErrReportSeqOverflow = errors.New("当日报告编号序号已达上限 999，请人工决策（超限不自动进位）")
	ErrReportTokenBusy   = errors.New("报告链接令牌生成冲突，请重试")
	ErrReportForbidden   = errors.New("报告内容命中对外禁用词，已拒绝生成")
	ErrReportIO          = errors.New("报告快照文件读写失败")
)

// 状态 / 范围字面量（★ 逐字取自 spec/schema.sql 列注释的中文原词）。
const (
	ReportStatusActive  = "有效"
	ReportStatusRevoked = "已撤销"
	ReportStatusExpired = "已过期"

	ReportScopeBatch = "按批次"

	// ReportSeqMax 报告编号当日序号上限（3 位，不自动进位）。
	ReportSeqMax = 999

	// reportTokenAttempts token 冲突重试上限（§6-1 D1）。
	reportTokenAttempts = 5
)

// ===== 读模型 =====

// Report 是 b_share_report 的读模型。
type Report struct {
	ID           int64       `json:"id"`
	ReportNo     string      `json:"report_no"`
	Title        string      `json:"title"`
	ScopeType    string      `json:"scope_type"`
	Scope        ReportScope `json:"scope"`
	ScopeJSON    string      `json:"-"`
	SnapshotPath string      `json:"snapshot_path"`
	URL          string      `json:"url"`
	Token        string      `json:"token"`
	GeneratedAt  time.Time   `json:"generated_at"`
	ExpiresAt    *time.Time  `json:"expires_at"`
	GeneratedBy  string      `json:"generated_by"`
	Status       string      `json:"status"`
	CreatedAt    time.Time   `json:"created_at"`
	CreatedBy    string      `json:"created_by"`
	// BatchHuman 由 scope 反查生产批人读行（列表展示用，不入库）。
	BatchHuman string `json:"batch_human"`
}

// ReportScope 是快照范围（一期只有按批次）。
type ReportScope struct {
	BatchID int64 `json:"batch_id"`
}

// ReportAccess 是 b_share_access 的一行（访问日志读入口）。
type ReportAccess struct {
	ID         int64     `json:"id"`
	ReportID   int64     `json:"report_id"`
	AccessedAt time.Time `json:"accessed_at"`
	IP         string    `json:"ip"`
	UA         string    `json:"ua"`
	CreatedAt  time.Time `json:"created_at"`
}

// AccessSyncResult 是 access/sync 的返回体（§6-8 / D4）。
type AccessSyncResult struct {
	Parsed   int   `json:"parsed"`
	Inserted int   `json:"inserted"`
	Skipped  int   `json:"skipped"`
	Offset   int64 `json:"offset"`
}

// ===== 入参 =====

// GenerateReportInput 是生成报告的入参（scope_type 一期只支持「按批次」）。
type GenerateReportInput struct {
	ScopeType string      `json:"scope_type"`
	Scope     ReportScope `json:"scope"`
	Title     string      `json:"title"`
	ExpiresAt string      `json:"expires_at"`
}

// ===== 目录与链接（★ 有效性 = 文件在不在 served/）=====

// reportDir 快照根目录：JX_REPORT_DIR，缺省 ~/jx-lab-trace/reports。
func reportDir() string {
	if v := strings.TrimSpace(os.Getenv("JX_REPORT_DIR")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "reports"
	}
	return filepath.Join(home, "jx-lab-trace", "reports")
}

// reportPublicBase 对外链接前缀：JX_REPORT_PUBLIC_BASE，缺省 http://127.0.0.1:18090/r。
func reportPublicBase() string {
	if v := strings.TrimSpace(os.Getenv("JX_REPORT_PUBLIC_BASE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:18090/r"
}

func snapshotAbs(rel string) string {
	return filepath.Join(reportDir(), filepath.FromSlash(rel))
}

// moveSnapshotOut 把快照移出可服务目录（撤销 / 过期 / 刷新的旧快照）。
// 返回 undo（改库失败时把文件移回）。源文件已不在 ⇒ 视为已移走（幂等）。
func moveSnapshotOut(rel string) (func(), error) {
	noop := func() {}
	if strings.TrimSpace(rel) == "" {
		return noop, nil
	}
	abs := snapshotAbs(rel)
	if _, err := os.Stat(abs); err != nil {
		if os.IsNotExist(err) {
			return noop, nil
		}
		return noop, fmt.Errorf("%w：读取快照失败: %v", ErrReportIO, err)
	}
	dst := filepath.Join(reportDir(), "_inactive", filepath.Base(abs))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return noop, fmt.Errorf("%w：%v", ErrReportIO, err)
	}
	if err := os.Rename(abs, dst); err != nil {
		return noop, fmt.Errorf("%w：%v", ErrReportIO, err)
	}
	return func() { _ = os.Rename(dst, abs) }, nil
}

// writeSnapshot 原子落盘：先写 *.tmp 再 rename（防半截页面被读到）。
func writeSnapshot(token string, html []byte) (rel string, err error) {
	served := filepath.Join(reportDir(), "served")
	if err := os.MkdirAll(served, 0o755); err != nil {
		return "", fmt.Errorf("%w：%v", ErrReportIO, err)
	}
	final := filepath.Join(served, token+".html")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, html, 0o644); err != nil {
		return "", fmt.Errorf("%w：%v", ErrReportIO, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("%w：%v", ErrReportIO, err)
	}
	return "served/" + token + ".html", nil
}

func removeSnapshot(rel string) {
	if strings.TrimSpace(rel) != "" {
		_ = os.Remove(snapshotAbs(rel))
	}
}

// ===== token / 有效期 =====

// newReportToken 生成 ≥32 字符的 URL-safe 长随机串（crypto/rand，不可猜测）。
func newReportToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成报告令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// endOfDay 到期日（含）的语义落点：该日 23:59:59.999（§6-6）。
func endOfDay(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 999000000, d.Location())
}

// parseExpiresAt 解析有效期入参：**只取日期部分并落该日 23:59:59.999**；
// 空 ⇒ 生成时刻 + 30 天（同一语义，缺省不产生 NULL）。
func parseExpiresAt(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return endOfDay(now.AddDate(0, 0, 30)), nil
	}
	day := strings.Fields(raw)[0] // 去掉可能带的时间部分（到期日语义按日计）
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, day, time.Local); err == nil {
			return endOfDay(t), nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", day, time.Local); err == nil {
		return endOfDay(t), nil
	}
	return time.Time{}, fmt.Errorf("%w：expires_at 须为 YYYY-MM-DD（到期日），实际 %q", ErrReportBadInput, raw)
}

// ===== 取号（GET_LOCK + 同事务，§6-1）=====

// reportSeqLockName 报告编号空间的命名锁名（≤64 字符）。
func reportSeqLockName(now time.Time) string {
	return "jxlab.report." + now.Format("060102")
}

// nextReportNo 在**持命名锁的事务内**取当日报告编号（超 999 明确报错，不进位）。
func nextReportNo(ctx context.Context, tx *sql.Tx, now time.Time) (string, error) {
	prefix := fmt.Sprintf("RP%s-", now.Format("060102"))
	rows, err := tx.QueryContext(ctx,
		`SELECT report_no FROM b_share_report WHERE report_no LIKE ?`, prefix+"%")
	if err != nil {
		return "", fmt.Errorf("读取报告编号失败: %w", err)
	}
	defer rows.Close()
	max := 0
	for rows.Next() {
		var no string
		if err := rows.Scan(&no); err != nil {
			return "", fmt.Errorf("读取报告编号失败: %w", err)
		}
		n, err := strconv.Atoi(strings.TrimPrefix(no, prefix))
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	next := max + 1
	if next > ReportSeqMax {
		return "", ErrReportSeqOverflow
	}
	return fmt.Sprintf("%s%03d", prefix, next), nil
}

// ===== 行扫描 =====

const reportSelect = `
SELECT r.id, r.report_no, COALESCE(r.title, ''), r.scope_type, COALESCE(r.scope_json, ''),
       COALESCE(r.snapshot_path, ''), COALESCE(r.url, ''), r.token, r.generated_at,
       r.expires_at, COALESCE(r.generated_by, ''), r.status, r.created_at, r.created_by
  FROM b_share_report r`

func scanReport(sc interface{ Scan(...interface{}) error }) (Report, error) {
	var r Report
	var scopeJSON sql.NullString
	var generatedAt, createdAt, expires sql.NullTime
	err := sc.Scan(&r.ID, &r.ReportNo, &r.Title, &r.ScopeType, &scopeJSON,
		&r.SnapshotPath, &r.URL, &r.Token, &generatedAt, &expires,
		&r.GeneratedBy, &r.Status, &createdAt, &r.CreatedBy)
	if err != nil {
		return r, err
	}
	r.ScopeJSON = scopeJSON.String
	if r.ScopeJSON != "" {
		_ = json.Unmarshal([]byte(r.ScopeJSON), &r.Scope)
	}
	if generatedAt.Valid {
		r.GeneratedAt = generatedAt.Time
	}
	if createdAt.Valid {
		r.CreatedAt = createdAt.Time
	}
	if expires.Valid {
		t := expires.Time
		r.ExpiresAt = &t
	}
	return r, nil
}

// hydrateBatchHuman 为列表行补上生产批人读行（scope 反查，行数小）。
func (s *Store) hydrateBatchHuman(ctx context.Context, rows []Report) error {
	cache := map[int64]string{}
	for i := range rows {
		id := rows[i].Scope.BatchID
		if id <= 0 {
			continue
		}
		h, ok := cache[id]
		if !ok {
			var code string
			if err := s.db.QueryRowContext(ctx,
				`SELECT code FROM b_production_batch WHERE id = ?`, id).Scan(&code); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					cache[id] = ""
					continue
				}
				return fmt.Errorf("读取生产批失败: %w", err)
			}
			h, _ = codec.ToHuman(code)
			cache[id] = h
		}
		rows[i].BatchHuman = h
	}
	return nil
}

// ===== 快照内容（白名单投影，见 report_snapshot.go）=====

// buildSnapshotDoc 由批次档案装配**白名单投影**（★ 不是把 BatchArchive 序列化出去）。
func (s *Store) buildSnapshotDoc(ctx context.Context, arch BatchArchive, rep Report) (*snapshotDoc, error) {
	d := &snapshotDoc{
		Title:         rep.Title,
		ReportNo:      rep.ReportNo,
		GeneratedAt:   rep.GeneratedAt.Local().Format("2006-01-02 15:04"),
		ExpiresAt:     "-",
		Customer:      arch.CustomerName,
		InputMaterial: arch.InputMaterial,
		PlannedOutput: arch.PlannedOutput,
		BatchHuman:    arch.Batch.Human,
		BatchDate:     arch.Batch.BatchDate,
	}
	if rep.ExpiresAt != nil {
		d.ExpiresAt = rep.ExpiresAt.Local().Format("2006-01-02 23:59:59")
	}

	// 投料明细：吨袋人读行 · 投料量 · 投料时间（★ 白名单，不带 operator / remark）
	for _, f := range arch.Feeds {
		d.Feeds = append(d.Feeds, snapshotFeed{
			BagHuman:   f.BagHuman,
			FeedWeight: snapTons(f.FeedWeight),
			FedAt:      snapTime(f.FedAt),
		})
	}

	// 作业段：段序 · 班组（m_team 名）· 时段 · 本段产出（★ 不带 operator）
	teamNames := map[int64]string{}
	for _, o := range arch.Operations {
		team := ""
		if o.TeamID != nil && *o.TeamID > 0 {
			if n, ok := teamNames[*o.TeamID]; ok {
				team = n
			} else {
				_ = s.db.QueryRowContext(ctx,
					`SELECT name FROM m_team WHERE id = ?`, *o.TeamID).Scan(&team)
				teamNames[*o.TeamID] = team
			}
		}
		period := "-"
		if o.StartAt != nil || o.EndAt != nil {
			period = snapTime(o.StartAt) + " ~ " + snapTime(o.EndAt)
		}
		d.Operations = append(d.Operations, snapshotOp{
			Seq: o.Seq, Team: team, Period: period, Output: snapTons(o.OutputWeight),
		})
	}

	// 成品批与袋：人读行 · 产出物料 · 净重 · 状态（★ 不带 remark / created_by）
	for _, l := range arch.FgLots {
		var mat string
		_ = s.db.QueryRowContext(ctx,
			`SELECT name FROM m_material WHERE id = ?`, l.FgLot.OutputMaterialID).Scan(&mat)
		lot := snapshotFgLot{
			Human:          l.FgLot.Human,
			OutputMaterial: mat,
			NetWeight:      snapTons(l.FgLot.NetWeight),
		}
		for _, b := range l.Bags {
			lot.Bags = append(lot.Bags, snapshotBag{
				Human: b.Human, NetWeight: snapTons(b.WeightAllocated), Status: b.Status,
			})
		}
		d.FgLots = append(d.FgLots, lot)
	}

	// 出货单：单号 · 状态 · 出场时间 · 车牌 · 客户
	for _, sh := range arch.Shipments {
		d.Shipments = append(d.Shipments, snapshotShip{
			No: sh.ShipmentNo, Status: sh.Status, ShipAt: snapTime(sh.ShipAt),
			PlateNo: sh.PlateNo, Customer: sh.Customer,
		})
	}

	// 检测结果表：项目名 · 数值 · 单位 · 判定（★ 仅此四列，不含单号/操作人/结论字样）
	rows, err := s.reportInspectionRows(ctx, arch)
	if err != nil {
		return nil, err
	}
	d.Inspections = rows
	return d, nil
}

// reportInspectionRows 汇总本批投料车次**现行检测单**的结果行（白名单四列）。
func (s *Store) reportInspectionRows(ctx context.Context, arch BatchArchive) ([]snapshotInspRow, error) {
	out := []snapshotInspRow{}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("开启只读事务失败: %w", err)
	}
	defer tx.Rollback()

	seen := map[int64]bool{}
	for _, f := range arch.Feeds {
		if f.TruckID == 0 || seen[f.TruckID] {
			continue
		}
		seen[f.TruckID] = true
		cur, err := currentInspectionIDTx(ctx, tx, TargetTruck, f.TruckID)
		if err != nil {
			return nil, err
		}
		if !cur.Valid {
			continue
		}
		rs, err := tx.QueryContext(ctx, `
SELECT COALESCE(ti.name, CONCAT('#', r.item_id)), r.value_num, COALESCE(r.value_text, ''),
       COALESCE(r.unit, ''), COALESCE(r.judge, '')
  FROM b_inspection_result r
  LEFT JOIN m_test_item ti ON ti.id = r.item_id
 WHERE r.inspection_id = ? AND r.state <> '未测'
 ORDER BY r.id`, cur.Int64)
		if err != nil {
			return nil, fmt.Errorf("读取检测结果失败: %w", err)
		}
		for rs.Next() {
			var row snapshotInspRow
			var num sql.NullFloat64
			if err := rs.Scan(&row.Item, &num, &row.Value, &row.Unit, &row.Judge); err != nil {
				rs.Close()
				return nil, fmt.Errorf("读取检测结果失败: %w", err)
			}
			var nf *float64
			if num.Valid {
				v := num.Float64
				nf = &v
			}
			row.Value = snapValue(nf, row.Value)
			if strings.TrimSpace(row.Judge) == "" {
				row.Judge = "-"
			}
			out = append(out, row)
		}
		if err := rs.Err(); err != nil {
			rs.Close()
			return nil, err
		}
		rs.Close()
	}
	return out, nil
}

// ===== D1 生成 =====

// GenerateReport 生成一份对外报告快照（写 b_share_report + 落盘 + 审计）。
func (s *Store) GenerateReport(ctx context.Context, in GenerateReportInput, actor MDActor) (Report, error) {
	if err := validateGenerateInput(in); err != nil {
		return Report{}, err
	}
	expires, err := parseExpiresAt(in.ExpiresAt, time.Now())
	if err != nil {
		return Report{}, err
	}
	arch, err := s.GetBatchArchive(ctx, in.Scope.BatchID)
	if err != nil {
		return Report{}, err
	}

	var last error
	for attempt := 1; attempt <= reportTokenAttempts; attempt++ {
		rep, err := s.generateOnce(ctx, in, actor, arch, expires)
		if err == nil {
			return rep, nil
		}
		if isTokenConflict(err) {
			last = err
			continue
		}
		return Report{}, err
	}
	return Report{}, fmt.Errorf("%w：%v", ErrReportTokenBusy, last)
}

func validateGenerateInput(in GenerateReportInput) error {
	if strings.TrimSpace(in.ScopeType) != ReportScopeBatch {
		return fmt.Errorf("%w：scope_type 一期只支持「%s」，实际 %q",
			ErrReportBadInput, ReportScopeBatch, in.ScopeType)
	}
	if in.Scope.BatchID <= 0 {
		return fmt.Errorf("%w：scope.batch_id 须为正整数", ErrReportBadInput)
	}
	return nil
}

func isTokenConflict(err error) bool {
	return isDuplicateErr(err) && strings.Contains(err.Error(), "uk_report_token")
}

// generateOnce 单次生成（token 冲突时由上层重试）：取号 → 插行 → 渲染 → 落盘 → 审计 → 提交。
func (s *Store) generateOnce(ctx context.Context, in GenerateReportInput, actor MDActor,
	arch BatchArchive, expires time.Time) (Report, error) {

	now := time.Now()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	lock := reportSeqLockName(now)
	if err := acquireSeqLock(ctx, conn, lock); err != nil {
		return Report{}, err
	}
	defer releaseSeqLock(conn, lock)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Report{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	reportNo, err := nextReportNo(ctx, tx, now)
	if err != nil {
		return Report{}, err
	}
	token, err := newReportToken()
	if err != nil {
		return Report{}, err
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "检验报告"
	}
	scopeJSON, _ := json.Marshal(in.Scope)
	snapPath := "served/" + token + ".html"
	repURL := reportPublicBase() + "/" + token + ".html"

	res, err := tx.ExecContext(ctx, `
INSERT INTO b_share_report
  (report_no, title, scope_type, scope_json, snapshot_path, url, token,
   generated_at, expires_at, generated_by, status, created_by)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		reportNo, nullStr(title), in.ScopeType, string(scopeJSON), snapPath, repURL, token,
		now, expires, nullStr(actor.OpenID), ReportStatusActive, actor.OpenID)
	if err != nil {
		if isDuplicateErr(err) {
			if strings.Contains(err.Error(), "uk_report_no") {
				return Report{}, fmt.Errorf("%w：报告编号 %s 已存在", ErrReportSeqBusy, reportNo)
			}
		}
		return Report{}, err
	}
	id, _ := res.LastInsertId()

	rep := Report{
		ID: id, ReportNo: reportNo, Title: title, ScopeType: in.ScopeType,
		Scope: in.Scope, ScopeJSON: string(scopeJSON), SnapshotPath: snapPath,
		URL: repURL, Token: token, GeneratedAt: now, ExpiresAt: &expires,
		GeneratedBy: actor.OpenID, Status: ReportStatusActive, CreatedAt: now, CreatedBy: actor.OpenID,
	}

	// 渲染 + 落盘（★ 仍在事务内：提交失败则移除文件）
	doc, err := s.buildSnapshotDoc(ctx, arch, rep)
	if err != nil {
		return Report{}, err
	}
	html, err := renderSnapshot(doc)
	if err != nil {
		return Report{}, err
	}
	if err := guardSnapshot(html); err != nil {
		return Report{}, err
	}
	rel, err := writeSnapshot(token, html)
	if err != nil {
		return Report{}, err
	}
	rep.SnapshotPath = rel

	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_share_report", EntityID: id, Action: "report_generate",
		NewValue:    fmt.Sprintf("%s 生成快照（批次 #%d）", reportNo, in.Scope.BatchID),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		removeSnapshot(rel)
		return Report{}, err
	}
	if err := tx.Commit(); err != nil {
		removeSnapshot(rel)
		return Report{}, fmt.Errorf("提交失败: %w", err)
	}
	return rep, nil
}

// ===== D2 刷新（新 token 新链接 + 旧行置已撤销）=====

// RefreshReport 重新生成快照：新建一行（新 report_no / 新 token / 新快照），
// 旧行置「已撤销」并把旧快照移出 served/。★ 不改同一行、不复用同一 token。
// 返回新报告与被替代的旧编号。
func (s *Store) RefreshReport(ctx context.Context, id int64, actor MDActor) (Report, string, error) {
	old, err := s.GetReport(ctx, id)
	if err != nil {
		return Report{}, "", err
	}
	if old.Status != ReportStatusActive {
		return Report{}, "", fmt.Errorf("%w：仅「有效」报告可刷新，当前 %q", ErrReportState, old.Status)
	}
	if old.Scope.BatchID <= 0 {
		return Report{}, "", fmt.Errorf("%w：原报告缺少批次范围", ErrReportBadInput)
	}
	arch, err := s.GetBatchArchive(ctx, old.Scope.BatchID)
	if err != nil {
		return Report{}, "", err
	}

	// ★ 先移旧快照（fail-closed）；改库失败则移回
	undo, err := moveSnapshotOut(old.SnapshotPath)
	if err != nil {
		return Report{}, "", err
	}

	expires := time.Time{}
	if old.ExpiresAt != nil {
		expires = *old.ExpiresAt
	} else {
		expires = endOfDay(time.Now().AddDate(0, 0, 30))
	}
	in := GenerateReportInput{ScopeType: old.ScopeType, Scope: old.Scope, Title: old.Title}

	var last error
	for attempt := 1; attempt <= reportTokenAttempts; attempt++ {
		rep, _, err := s.refreshOnce(ctx, in, old, actor, arch, expires)
		if err == nil {
			return rep, old.ReportNo, nil
		}
		if isTokenConflict(err) {
			last = err
			continue
		}
		undo()
		return Report{}, "", err
	}
	undo()
	return Report{}, "", fmt.Errorf("%w：%v", ErrReportTokenBusy, last)
}

// refreshOnce 单次刷新事务：插入新行 + 旧行置已撤销 + 两笔审计 + 新快照落盘。
func (s *Store) refreshOnce(ctx context.Context, in GenerateReportInput, old Report,
	actor MDActor, arch BatchArchive, expires time.Time) (Report, string, error) {

	now := time.Now()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Report{}, "", fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	lock := reportSeqLockName(now)
	if err := acquireSeqLock(ctx, conn, lock); err != nil {
		return Report{}, "", err
	}
	defer releaseSeqLock(conn, lock)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Report{}, "", fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	reportNo, err := nextReportNo(ctx, tx, now)
	if err != nil {
		return Report{}, "", err
	}
	token, err := newReportToken()
	if err != nil {
		return Report{}, "", err
	}
	title := old.Title
	if title == "" {
		title = "检验报告"
	}
	snapPath := "served/" + token + ".html"
	repURL := reportPublicBase() + "/" + token + ".html"

	res, err := tx.ExecContext(ctx, `
INSERT INTO b_share_report
  (report_no, title, scope_type, scope_json, snapshot_path, url, token,
   generated_at, expires_at, generated_by, status, created_by)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		reportNo, nullStr(title), old.ScopeType, old.ScopeJSON, snapPath, repURL, token,
		now, expires, nullStr(actor.OpenID), ReportStatusActive, actor.OpenID)
	if err != nil {
		return Report{}, "", err
	}
	id, _ := res.LastInsertId()
	rep := Report{
		ID: id, ReportNo: reportNo, Title: title, ScopeType: old.ScopeType,
		Scope: old.Scope, ScopeJSON: old.ScopeJSON, SnapshotPath: snapPath,
		URL: repURL, Token: token, GeneratedAt: now, ExpiresAt: &expires,
		GeneratedBy: actor.OpenID, Status: ReportStatusActive, CreatedAt: now, CreatedBy: actor.OpenID,
	}

	// 旧行置已撤销 + 快照路径改指 _inactive（文件已在事务前移走）
	newOldPath := "_inactive/" + filepath.Base(filepath.FromSlash(old.SnapshotPath))
	if old.SnapshotPath == "" {
		newOldPath = ""
	}
	upd, err := tx.ExecContext(ctx, `
UPDATE b_share_report SET status = ?, snapshot_path = ? WHERE id = ? AND status = ?`,
		ReportStatusRevoked, nullStr(newOldPath), old.ID, ReportStatusActive)
	if err != nil {
		return Report{}, "", fmt.Errorf("更新旧报告失败: %w", err)
	}
	if n, _ := upd.RowsAffected(); n != 1 {
		return Report{}, "", fmt.Errorf("%w：旧报告状态已变化", ErrReportState)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_share_report", EntityID: old.ID, Action: "report_refresh_supersede",
		NewValue:    fmt.Sprintf("由 %s 刷新替代", reportNo),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return Report{}, "", err
	}

	doc, err := s.buildSnapshotDoc(ctx, arch, rep)
	if err != nil {
		return Report{}, "", err
	}
	html, err := renderSnapshot(doc)
	if err != nil {
		return Report{}, "", err
	}
	if err := guardSnapshot(html); err != nil {
		return Report{}, "", err
	}
	rel, err := writeSnapshot(token, html)
	if err != nil {
		return Report{}, "", err
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_share_report", EntityID: id, Action: "report_generate",
		NewValue:    fmt.Sprintf("%s 由 %s 刷新生成", reportNo, old.ReportNo),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		removeSnapshot(rel)
		return Report{}, "", err
	}
	if err := tx.Commit(); err != nil {
		removeSnapshot(rel)
		return Report{}, "", fmt.Errorf("提交失败: %w", err)
	}
	return rep, rel, nil
}

// ===== D3 生命周期 =====

// SetReportExpires 改有效期（语义见 §6-6；不移动快照）。
func (s *Store) SetReportExpires(ctx context.Context, id int64, raw string, actor MDActor) (Report, error) {
	rep, err := s.GetReport(ctx, id)
	if err != nil {
		return Report{}, err
	}
	expires, err := parseExpiresAt(raw, time.Now())
	if err != nil {
		return Report{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Report{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE b_share_report SET expires_at = ? WHERE id = ?`, expires, id); err != nil {
		return Report{}, fmt.Errorf("更新有效期失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_share_report", EntityID: id, Action: "report_set_expires",
		OldValue: formatTimePtr(rep.ExpiresAt), NewValue: expires.Format("2006-01-02 15:04:05.999"),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return Report{}, err
	}
	if err := tx.Commit(); err != nil {
		return Report{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetReport(ctx, id)
}

// RevokeReport 撤销（★ 单步生效，无发起 / 审批两段式；reason 必填）。
// ★ 必须把快照移出 served/ ⇒ 公网侧天然 404（A4 / §6-2）。
func (s *Store) RevokeReport(ctx context.Context, id int64, reason string, actor MDActor) (Report, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Report{}, fmt.Errorf("%w：reason 必填", ErrReportBadInput)
	}
	rep, err := s.GetReport(ctx, id)
	if err != nil {
		return Report{}, err
	}
	if rep.Status != ReportStatusActive {
		return Report{}, fmt.Errorf("%w：仅「有效」报告可撤销，当前 %q", ErrReportState, rep.Status)
	}

	undo, err := moveSnapshotOut(rep.SnapshotPath)
	if err != nil {
		return Report{}, err
	}

	newPath := "_inactive/" + filepath.Base(filepath.FromSlash(rep.SnapshotPath))
	if rep.SnapshotPath == "" {
		newPath = ""
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		undo()
		return Report{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
UPDATE b_share_report SET status = ?, snapshot_path = ? WHERE id = ? AND status = ?`,
		ReportStatusRevoked, nullStr(newPath), id, ReportStatusActive)
	if err != nil {
		undo()
		return Report{}, fmt.Errorf("撤销报告失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		undo()
		return Report{}, fmt.Errorf("%w：状态已变化", ErrReportState)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_share_report", EntityID: id, Action: "report_revoke", Reason: reason,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		undo()
		return Report{}, err
	}
	if err := tx.Commit(); err != nil {
		undo()
		return Report{}, fmt.Errorf("提交失败: %w", err)
	}
	rep.Status = ReportStatusRevoked
	rep.SnapshotPath = newPath
	return rep, nil
}

// SweepExpiredReports 命令式过期清扫（幂等：第二次返回 swept=0）。
// 判定 = now > expires_at（§6-6）；离开有效态的行移出快照。
func (s *Store) SweepExpiredReports(ctx context.Context, actor MDActor) (int, []string, error) {
	now := time.Now()
	rows, err := s.db.QueryContext(ctx, `
SELECT id, report_no, COALESCE(snapshot_path, '') FROM b_share_report
 WHERE status = ? AND expires_at IS NOT NULL AND expires_at < ?
 ORDER BY id`, ReportStatusActive, now)
	if err != nil {
		return 0, nil, fmt.Errorf("查询过期报告失败: %w", err)
	}
	type pending struct {
		id  int64
		no  string
		rel string
	}
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.no, &p.rel); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("读取过期报告失败: %w", err)
		}
		list = append(list, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, nil, err
	}
	rows.Close()

	swept, nos := 0, []string{}
	for _, p := range list {
		undo, err := moveSnapshotOut(p.rel)
		if err != nil {
			return swept, nos, err
		}
		newPath := ""
		if p.rel != "" {
			newPath = "_inactive/" + filepath.Base(filepath.FromSlash(p.rel))
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			undo()
			return swept, nos, fmt.Errorf("开启事务失败: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
UPDATE b_share_report SET status = ?, snapshot_path = ? WHERE id = ? AND status = ?`,
			ReportStatusExpired, nullStr(newPath), p.id, ReportStatusActive)
		if err != nil {
			tx.Rollback()
			undo()
			return swept, nos, fmt.Errorf("更新报告状态失败: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			if err := appendAuditTx(ctx, tx, audit.Entry{
				Entity: "b_share_report", EntityID: p.id, Action: "report_expire",
				NewValue:    p.no,
				ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
			}); err != nil {
				tx.Rollback()
				undo()
				return swept, nos, err
			}
			if err := tx.Commit(); err != nil {
				undo()
				return swept, nos, fmt.Errorf("提交失败: %w", err)
			}
			swept++
			nos = append(nos, p.no)
		} else {
			tx.Rollback()
			undo()
		}
	}
	return swept, nos, nil
}

func formatTimePtr(p *time.Time) string {
	if p == nil {
		return ""
	}
	return p.Local().Format("2006-01-02 15:04:05.999")
}

// ===== 读入口（★ 全部挂 rpt.view 的 LevelRead，§6-7）=====

// ListReports 报告列表（status 可空）。
func (s *Store) ListReports(ctx context.Context, status string) ([]Report, error) {
	q := reportSelect
	args := []interface{}{}
	if st := strings.TrimSpace(status); st != "" {
		q += ` WHERE r.status = ?`
		args = append(args, st)
	}
	q += ` ORDER BY r.id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("读取报告列表失败: %w", err)
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, fmt.Errorf("读取报告列表失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.hydrateBatchHuman(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetReport 单条报告。
func (s *Store) GetReport(ctx context.Context, id int64) (Report, error) {
	row := s.db.QueryRowContext(ctx, reportSelect+` WHERE r.id = ?`, id)
	r, err := scanReport(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Report{}, fmt.Errorf("%w：#%d", ErrReportNotFound, id)
	}
	if err != nil {
		return Report{}, fmt.Errorf("读取报告失败: %w", err)
	}
	if err := s.hydrateBatchHuman(ctx, []Report{r}); err != nil {
		return Report{}, err
	}
	return r, nil
}

// ListReportAccess 某报告的访问日志（倒序）。
func (s *Store) ListReportAccess(ctx context.Context, id int64) ([]ReportAccess, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, report_id, accessed_at, COALESCE(ip, ''), COALESCE(ua, ''), created_at
  FROM b_share_access WHERE report_id = ? ORDER BY accessed_at DESC, id DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("读取访问日志失败: %w", err)
	}
	defer rows.Close()
	out := []ReportAccess{}
	for rows.Next() {
		var a ReportAccess
		var accessed, created sql.NullTime
		if err := rows.Scan(&a.ID, &a.ReportID, &accessed, &a.IP, &a.UA, &created); err != nil {
			return nil, fmt.Errorf("读取访问日志失败: %w", err)
		}
		if accessed.Valid {
			a.AccessedAt = accessed.Time
		}
		if created.Valid {
			a.CreatedAt = created.Time
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ===== D4 访问日志同步（byte offset，幂等、不回放）=====

func accessLogPaths() (logPath, offsetPath string) {
	root := reportDir()
	return filepath.Join(root, "access.log"), filepath.Join(root, "access.log.offset")
}

func readAccessOffset(p string) int64 {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func writeAccessOffset(p string, off int64) error {
	if err := os.WriteFile(p, []byte(strconv.FormatInt(off, 10)), 0o644); err != nil {
		return fmt.Errorf("%w：写同步偏移失败: %v", ErrReportIO, err)
	}
	return nil
}

// SyncReportAccess 把 reportd 追加的 access.log 同步入库。
//
// ★ 选 **byte offset** 而非 rename 滚动（§6-8 推荐项）：reportd 以 O_APPEND
//
//	持有 access.log 句柄持续追加，rename 会让它继续写旧 inode ⇒ 新行丢失。
//	同步只**读**日志 + 推进 offset 文件，不动日志本身 ⇒ 不丢行、不回放。
//
// ★ 幂等：(report_id, accessed_at, ip) 去重；★ token 未命中 ⇒ skipped 不中断。
func (s *Store) SyncReportAccess(ctx context.Context, actor MDActor) (AccessSyncResult, error) {
	logPath, offPath := accessLogPaths()
	out := AccessSyncResult{Offset: readAccessOffset(offPath)}

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil // 无访问 ⇒ 0 行，offset 不变
		}
		return out, fmt.Errorf("%w：打开访问日志失败: %v", ErrReportIO, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return out, fmt.Errorf("%w：%v", ErrReportIO, err)
	}
	if st.Size() < out.Offset {
		out.Offset = 0 // 日志被重建（截断）⇒ 从头同步
	}
	if _, err := f.Seek(out.Offset, 0); err != nil {
		return out, fmt.Errorf("%w：%v", ErrReportIO, err)
	}
	buf := make([]byte, st.Size()-out.Offset)
	if len(buf) > 0 {
		if _, err := f.Read(buf); err != nil {
			return out, fmt.Errorf("%w：%v", ErrReportIO, err)
		}
	}

	// 只处理**完整行**（以 \n 结尾）；末尾半行留给下一次
	data := string(buf)
	complete := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			complete = i + 1
		}
	}
	if complete == 0 {
		return out, nil
	}

	tokenIDs := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSuffix(data[:complete], "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out.Parsed++
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			out.Skipped++
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, fields[0])
		if err != nil {
			out.Skipped++
			continue
		}
		ts = ts.Truncate(time.Millisecond) // DATETIME(3) 精度对齐 ⇒ 去重键稳定
		token, ip := fields[1], fields[2]
		ua := ""
		if len(fields) >= 5 {
			ua = fields[4]
		}

		id, ok := tokenIDs[token]
		if !ok {
			if err := s.db.QueryRowContext(ctx,
				`SELECT id FROM b_share_report WHERE token = ?`, token).Scan(&id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					out.Skipped++ // 未命中的 token（本就 404）⇒ 不写库、不中断
					continue
				}
				return out, fmt.Errorf("按令牌查报告失败: %w", err)
			}
			tokenIDs[token] = id
		}

		var dup int
		if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM b_share_access
 WHERE report_id = ? AND accessed_at = ? AND COALESCE(ip, '') = ?`,
			id, ts, ip).Scan(&dup); err != nil {
			return out, fmt.Errorf("访问日志去重检查失败: %w", err)
		}
		if dup > 0 {
			out.Skipped++
			continue
		}
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO b_share_access (report_id, accessed_at, ip, ua, created_by)
VALUES (?,?,?,?,?)`, id, ts, nullStr(ip), nullStr(ua), actor.OpenID); err != nil {
			return out, fmt.Errorf("写入访问日志失败: %w", err)
		}
		out.Inserted++
	}

	out.Offset += int64(complete)
	if err := writeAccessOffset(offPath, out.Offset); err != nil {
		return out, err
	}
	return out, nil
}

// ReportDir 暴露快照根目录（cmd / 部署脚本自检用）。
func ReportDir() string { return reportDir() }
