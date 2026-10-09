package store

// ===== M4 留样：入库（D4）· 借还（D5）· 销毁（D6）=====
//
// ★★ 保留期限默认值**可配置**（docs/01 D4 / 任务包 §6-9：不得硬编码）：
//	配置机制 = 环境变量（JX_RETENTION_MONTHS_RAW / _INTERMEDIATE / _FG / _ARBITRATION），
//	由 internal/config.Load 解析进 Config，启动时经 Store.SetRetentionDefaults 注入；
//	未配置时的安全缺省即 D4 口径：原料 6 月 · 中间 3 月 · 成品 12 月 · 仲裁 24 月。
//
// ★★ 销毁「发起 ≠ 审批」（§6-11）：
//	InitDestroy 只落「待审批」行（approved_by 空 + 审计记发起人），**不改任何状态**；
//	ApproveDestroy 强制 approved_by 非空（缺失 ⇒ ErrDestroyNeedApprover / 400），
//	审批后才把 b_sample_retention.status 与 b_sample.status 同步置「已销毁」；
//	uk_destroy_sample 保证一个样品只销毁一次。
//
// ★ 保留样与检测样各自独立跟踪（§6-10）：本文件是留样状态的唯一变更入口，
//	不存在「检测完成 ⇒ 清保留样」的路径（批 5 的检测表与本文件零耦合）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/audit"
)

// RetentionDefaults 是留样保留期限的默认月数（按样品类型）。
type RetentionDefaults struct {
	RawMonths          int // 原料样（b_sample.bag_id 非空）
	IntermediateMonths int // 中间样（b_sample.batch_id 非空）
	FGMonths           int // 成品样（b_sample.fg_lot_id 非空）
	ArbitrationMonths  int // 仲裁样（role='仲裁样'，优先级最高）
}

// DefaultRetentionDefaults 返回 docs/01 D4 口径的安全缺省（未配置环境变量时生效）。
func DefaultRetentionDefaults() RetentionDefaults {
	return RetentionDefaults{
		RawMonths: 6, IntermediateMonths: 3, FGMonths: 12, ArbitrationMonths: 24,
	}
}

// SetRetentionDefaults 覆盖生效配置（main 启动时注入；0 值项保持既有缺省）。
func (s *Store) SetRetentionDefaults(d RetentionDefaults) {
	def := s.RetentionDefaults()
	if d.RawMonths > 0 {
		def.RawMonths = d.RawMonths
	}
	if d.IntermediateMonths > 0 {
		def.IntermediateMonths = d.IntermediateMonths
	}
	if d.FGMonths > 0 {
		def.FGMonths = d.FGMonths
	}
	if d.ArbitrationMonths > 0 {
		def.ArbitrationMonths = d.ArbitrationMonths
	}
	s.retention = def
}

// RetentionDefaults 返回当前生效的保留期限配置。
func (s *Store) RetentionDefaults() RetentionDefaults {
	if s.retention == (RetentionDefaults{}) {
		return DefaultRetentionDefaults()
	}
	return s.retention
}

// RetentionKindOf 判定样品类型（★ 次序：role='仲裁样' ⇒ 仲裁；否则看绑定列，
//
//	bag_id ⇒ 原料 · batch_id ⇒ 中间 · fg_lot_id ⇒ 成品 —— 任务包 §6-9）。
func RetentionKindOf(role string, bagID, batchID, fgLotID *int64) (string, error) {
	if role == SampleRoleArbitration {
		return "仲裁", nil
	}
	if bagID != nil {
		return "原料", nil
	}
	if batchID != nil {
		return "中间", nil
	}
	if fgLotID != nil {
		return "成品", nil
	}
	return "", fmt.Errorf("%w：无法判定样品类型（无绑定列）", ErrSampleBadInput)
}

// retentionMonths 返回该类型的默认保留月数。
func (d RetentionDefaults) monthsOf(kind string) (int, error) {
	switch kind {
	case "原料":
		return d.RawMonths, nil
	case "中间":
		return d.IntermediateMonths, nil
	case "成品":
		return d.FGMonths, nil
	case "仲裁":
		return d.ArbitrationMonths, nil
	}
	return 0, fmt.Errorf("%w：未知样品类型 %q", ErrSampleBadInput, kind)
}

// RetentionRow 是 b_sample_retention 的一行（带样品编号）。
type RetentionRow struct {
	ID             int64  `json:"id"`
	SampleID       int64  `json:"sample_id"`
	SampleNo       string `json:"sample_no"`
	Role           string `json:"role"`
	Location       string `json:"location"`
	StoredAt       string `json:"stored_at"`
	RetentionUntil string `json:"retention_until"`
	Status         string `json:"status"`
	CreatedBy      string `json:"created_by,omitempty"`
}

// RetainInput 是留样入库入参。
type RetainInput struct {
	SampleID       int64  `json:"sample_id"`
	Location       string `json:"location"`        // 三层文本，如「化验室-留样柜A-第3层」
	RetentionUntil string `json:"retention_until"` // 可空 ⇒ 按类型取默认
}

// RetainSample 留样入库：登记位置与保留期限（★ 期限默认按类型、可配置）。
func (s *Store) RetainSample(ctx context.Context, in RetainInput, actor MDActor) (RetentionRow, error) {
	location := strings.TrimSpace(in.Location)
	if location == "" {
		return RetentionRow{}, fmt.Errorf("%w：location 必填（三层文本）", ErrSampleBadInput)
	}
	sample, err := s.GetSample(ctx, in.SampleID)
	if err != nil {
		return RetentionRow{}, err
	}
	if sample.Role != SampleRoleRetention && sample.Role != SampleRoleArbitration {
		return RetentionRow{}, fmt.Errorf("%w：只有保留样/仲裁样可以登记留样（%s 是%s）",
			ErrSampleBadInput, sample.SampleNo, sample.Role)
	}
	if sample.Status == SampleStatusDestroyed {
		return RetentionRow{}, fmt.Errorf("%w：样品已销毁", ErrSampleState)
	}
	if sample.Status != SampleStatusInStock {
		return RetentionRow{}, fmt.Errorf("%w：样品当前「%s」，须在库才能入库", ErrSampleState, sample.Status)
	}

	until := strings.TrimSpace(in.RetentionUntil)
	if until == "" {
		kind, err := RetentionKindOf(sample.Role, sample.BagID, sample.BatchID, sample.FgLotID)
		if err != nil {
			return RetentionRow{}, err
		}
		months, err := s.RetentionDefaults().monthsOf(kind)
		if err != nil {
			return RetentionRow{}, err
		}
		until = time.Now().AddDate(0, months, 0).Format("2006-01-02")
	} else if _, err := time.Parse("2006-01-02", until); err != nil {
		return RetentionRow{}, fmt.Errorf("%w：retention_until 须为 YYYY-MM-DD", ErrSampleBadInput)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return RetentionRow{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return RetentionRow{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_sample_retention WHERE sample_id = ?`, in.SampleID).Scan(&exists); err != nil {
		return RetentionRow{}, err
	}
	if exists > 0 {
		return RetentionRow{}, fmt.Errorf("%w：%s", ErrRetentionExists, sample.SampleNo)
	}

	res, err := tx.ExecContext(ctx, `
INSERT INTO b_sample_retention (sample_id, location, retention_until, status, created_by)
VALUES (?,?,?, '在库', ?)`, in.SampleID, location, until, actor.OpenID)
	if err != nil {
		return RetentionRow{}, fmt.Errorf("留样入库失败: %w", err)
	}
	id, _ := res.LastInsertId()

	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_sample_retention", EntityID: id, Action: "retain_in",
		NewValue:    sample.SampleNo + " → " + location + " 保留至 " + until,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return RetentionRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return RetentionRow{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.getRetentionByID(ctx, id)
}

func (s *Store) getRetentionByID(ctx context.Context, id int64) (RetentionRow, error) {
	var r RetentionRow
	err := s.db.QueryRowContext(ctx, `
SELECT r.id, r.sample_id, b.sample_no, b.role, r.location,
       DATE_FORMAT(r.stored_at, '%Y-%m-%d %H:%i:%s'),
       IFNULL(DATE_FORMAT(r.retention_until, '%Y-%m-%d'), ''),
       r.status, r.created_by
  FROM b_sample_retention r JOIN b_sample b ON b.id = r.sample_id
 WHERE r.id = ?`, id).
		Scan(&r.ID, &r.SampleID, &r.SampleNo, &r.Role, &r.Location, &r.StoredAt,
			&r.RetentionUntil, &r.Status, &r.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return RetentionRow{}, fmt.Errorf("%w：留样记录 #%d", ErrSampleNotFound, id)
	}
	if err != nil {
		return RetentionRow{}, fmt.Errorf("读取留样记录失败: %w", err)
	}
	return r, nil
}

// ListRetention 列留样记录；dueBefore 非空 ⇒ 只看「保留期限 ≤ 该日」（★ 到期可检索，
// 走 idx_retention_until），默认不含已销毁。
func (s *Store) ListRetention(ctx context.Context, dueBefore string, includeDestroyed bool) ([]RetentionRow, error) {
	q := `
SELECT r.id, r.sample_id, b.sample_no, b.role, r.location,
       DATE_FORMAT(r.stored_at, '%Y-%m-%d %H:%i:%s'),
       IFNULL(DATE_FORMAT(r.retention_until, '%Y-%m-%d'), ''),
       r.status, r.created_by
  FROM b_sample_retention r JOIN b_sample b ON b.id = r.sample_id
 WHERE 1=1`
	args := []interface{}{}
	if !includeDestroyed {
		q += ` AND r.status <> '已销毁'`
	}
	if strings.TrimSpace(dueBefore) != "" {
		if _, err := time.Parse("2006-01-02", strings.TrimSpace(dueBefore)); err != nil {
			return nil, fmt.Errorf("%w：due_before 须为 YYYY-MM-DD", ErrSampleBadInput)
		}
		q += ` AND r.retention_until <= ?`
		args = append(args, strings.TrimSpace(dueBefore))
	}
	q += ` ORDER BY r.retention_until, r.id LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询留样记录失败: %w", err)
	}
	defer rows.Close()
	var out []RetentionRow
	for rows.Next() {
		var r RetentionRow
		if err := rows.Scan(&r.ID, &r.SampleID, &r.SampleNo, &r.Role, &r.Location,
			&r.StoredAt, &r.RetentionUntil, &r.Status, &r.CreatedBy); err != nil {
			return nil, fmt.Errorf("读取留样记录失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ===== D5 · 借还 =====

// LendRow 是 b_sample_lend 的一行（带样品编号）。
type LendRow struct {
	ID         int64  `json:"id"`
	SampleID   int64  `json:"sample_id"`
	SampleNo   string `json:"sample_no"`
	LentAt     string `json:"lent_at"`
	LentTo     string `json:"lent_to"`
	Purpose    string `json:"purpose"`
	ReturnedAt string `json:"returned_at,omitempty"`
	Status     string `json:"status"` // 样品当前状态
}

// LendInput 是借出登记入参。
type LendInput struct {
	SampleID int64  `json:"sample_id"`
	LentTo   string `json:"lent_to"`
	Purpose  string `json:"purpose"`
}

// LendSample 借出留样：状态「在库」→「已借出」（b_sample 与 b_sample_retention 同步）。
func (s *Store) LendSample(ctx context.Context, in LendInput, actor MDActor) (LendRow, error) {
	lentTo := strings.TrimSpace(in.LentTo)
	if lentTo == "" {
		return LendRow{}, fmt.Errorf("%w：lent_to 必填", ErrSampleBadInput)
	}
	sample, err := s.GetSample(ctx, in.SampleID)
	if err != nil {
		return LendRow{}, err
	}
	if sample.Role != SampleRoleRetention && sample.Role != SampleRoleArbitration {
		return LendRow{}, fmt.Errorf("%w：只有留样（保留样/仲裁样）可以借出", ErrSampleBadInput)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return LendRow{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return LendRow{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	status, err := lockedRetentionStatus(ctx, tx, in.SampleID)
	if err != nil {
		return LendRow{}, err
	}
	if status != SampleStatusInStock {
		return LendRow{}, fmt.Errorf("%w：留样当前「%s」，不可借出", ErrLendState, status)
	}

	res, err := tx.ExecContext(ctx, `
INSERT INTO b_sample_lend (sample_id, lent_to, purpose, created_by)
VALUES (?,?,?,?)`, in.SampleID, lentTo, nullIfEmptyStr(in.Purpose), actor.OpenID)
	if err != nil {
		return LendRow{}, fmt.Errorf("借出登记失败: %w", err)
	}
	lendID, _ := res.LastInsertId()
	if err := setSampleStatusTx(ctx, tx, in.SampleID, SampleStatusLent); err != nil {
		return LendRow{}, err
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_sample_lend", EntityID: lendID, Action: "lend",
		NewValue:    sample.SampleNo + " 借给 " + lentTo,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: in.Purpose,
	}); err != nil {
		return LendRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return LendRow{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetLend(ctx, lendID)
}

// ReturnLend 归还登记：写 returned_at，状态「已借出」→「在库」（两表同步）。
func (s *Store) ReturnLend(ctx context.Context, lendID int64, actor MDActor) (LendRow, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return LendRow{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return LendRow{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var sampleID int64
	var returnedAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT sample_id, returned_at FROM b_sample_lend WHERE id = ? FOR UPDATE`, lendID).
		Scan(&sampleID, &returnedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return LendRow{}, fmt.Errorf("%w：借还记录 #%d", ErrSampleNotFound, lendID)
	}
	if err != nil {
		return LendRow{}, fmt.Errorf("读取借还记录失败: %w", err)
	}
	if returnedAt.Valid {
		return LendRow{}, fmt.Errorf("%w：该记录已归还", ErrLendState)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE b_sample_lend SET returned_at = NOW(3) WHERE id = ?`, lendID); err != nil {
		return LendRow{}, fmt.Errorf("归还登记失败: %w", err)
	}
	if err := setSampleStatusTx(ctx, tx, sampleID, SampleStatusInStock); err != nil {
		return LendRow{}, err
	}
	var sampleNo string
	_ = tx.QueryRowContext(ctx, `SELECT sample_no FROM b_sample WHERE id = ?`, sampleID).Scan(&sampleNo)
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_sample_lend", EntityID: lendID, Action: "return",
		NewValue:    sampleNo + " 归还入柜",
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return LendRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return LendRow{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetLend(ctx, lendID)
}

// GetLend 按 id 取借还记录。
func (s *Store) GetLend(ctx context.Context, id int64) (LendRow, error) {
	var r LendRow
	var returnedAt sql.NullTime
	var purpose sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT l.id, l.sample_id, b.sample_no,
       DATE_FORMAT(l.lent_at, '%Y-%m-%d %H:%i:%s'), l.lent_to,
       IFNULL(l.purpose, ''), l.returned_at, b.status
  FROM b_sample_lend l JOIN b_sample b ON b.id = l.sample_id
 WHERE l.id = ?`, id).
		Scan(&r.ID, &r.SampleID, &r.SampleNo, &r.LentAt, &r.LentTo, &purpose, &returnedAt, &r.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return LendRow{}, fmt.Errorf("%w：借还记录 #%d", ErrSampleNotFound, id)
	}
	if err != nil {
		return LendRow{}, fmt.Errorf("读取借还记录失败: %w", err)
	}
	r.Purpose = purpose.String
	if returnedAt.Valid {
		t := returnedAt.Time
		r.ReturnedAt = t.Format("2006-01-02 15:04:05")
	}
	return r, nil
}

// ListLends 列借还记录（openOnly=只看未归还）。
func (s *Store) ListLends(ctx context.Context, openOnly bool) ([]LendRow, error) {
	q := `
SELECT l.id, l.sample_id, b.sample_no,
       DATE_FORMAT(l.lent_at, '%Y-%m-%d %H:%i:%s'), l.lent_to,
       IFNULL(l.purpose, ''), l.returned_at, b.status
  FROM b_sample_lend l JOIN b_sample b ON b.id = l.sample_id`
	if openOnly {
		q += ` WHERE l.returned_at IS NULL`
	}
	q += ` ORDER BY l.id DESC LIMIT 200`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("查询借还记录失败: %w", err)
	}
	defer rows.Close()
	var out []LendRow
	for rows.Next() {
		var r LendRow
		var returnedAt sql.NullTime
		var purpose sql.NullString
		if err := rows.Scan(&r.ID, &r.SampleID, &r.SampleNo, &r.LentAt, &r.LentTo,
			&purpose, &returnedAt, &r.Status); err != nil {
			return nil, fmt.Errorf("读取借还记录失败: %w", err)
		}
		r.Purpose = purpose.String
		if returnedAt.Valid {
			t := returnedAt.Time
			r.ReturnedAt = t.Format("2006-01-02 15:04:05")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// lockedRetentionStatus 在事务内读取留样状态（无留样行 ⇒ 错误）。
func lockedRetentionStatus(ctx context.Context, tx *sql.Tx, sampleID int64) (string, error) {
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT status FROM b_sample_retention WHERE sample_id = ? FOR UPDATE`, sampleID).
		Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w：该样品尚未登记留样入库", ErrSampleState)
	}
	if err != nil {
		return "", fmt.Errorf("读取留样状态失败: %w", err)
	}
	return status, nil
}

// setSampleStatusTx 同步 b_sample.status 与 b_sample_retention.status（任务包 §6-13）。
func setSampleStatusTx(ctx context.Context, tx *sql.Tx, sampleID int64, status string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE b_sample SET status = ? WHERE id = ?`, status, sampleID); err != nil {
		return fmt.Errorf("更新样品状态失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE b_sample_retention SET status = ? WHERE sample_id = ?`, status, sampleID); err != nil {
		return fmt.Errorf("更新留样状态失败: %w", err)
	}
	return nil
}

// ===== D6 · 销毁（发起 ≠ 审批）=====

// DestroyRow 是 b_sample_destroy 的一行（带样品编号与双当事人）。
type DestroyRow struct {
	ID          int64  `json:"id"`
	SampleID    int64  `json:"sample_id"`
	SampleNo    string `json:"sample_no"`
	DestroyedAt string `json:"destroyed_at"`
	DestroyedBy string `json:"destroyed_by"`
	ApprovedBy  string `json:"approved_by"` // 空 = 尚未审批（待审批态）
	Reason      string `json:"reason"`
	Pending     bool   `json:"pending"`
}

// DestroyInitInput 是销毁发起入参。
type DestroyInitInput struct {
	SampleID    int64  `json:"sample_id"`
	DestroyedBy string `json:"destroyed_by"`
	Reason      string `json:"reason"`
}

// InitDestroy 销毁**发起**：填执行人与原因，落「待审批」行（approved_by 空），
// **不改任何状态**；审计记发起人。重复发起 ⇒ uk_destroy_sample ⇒ 409。
func (s *Store) InitDestroy(ctx context.Context, in DestroyInitInput, actor MDActor) (DestroyRow, error) {
	destroyedBy := strings.TrimSpace(in.DestroyedBy)
	reason := strings.TrimSpace(in.Reason)
	if destroyedBy == "" {
		return DestroyRow{}, fmt.Errorf("%w：destroyedBy（销毁执行人）必填", ErrSampleBadInput)
	}
	if reason == "" {
		return DestroyRow{}, fmt.Errorf("%w：reason（销毁原因）必填", ErrSampleBadInput)
	}
	sample, err := s.GetSample(ctx, in.SampleID)
	if err != nil {
		return DestroyRow{}, err
	}
	if sample.Role != SampleRoleRetention && sample.Role != SampleRoleArbitration {
		return DestroyRow{}, fmt.Errorf("%w：只有留样（保留样/仲裁样）可以销毁", ErrSampleBadInput)
	}
	if sample.Status == SampleStatusDestroyed {
		return DestroyRow{}, fmt.Errorf("%w：%s 已销毁", ErrDestroyAlready, sample.SampleNo)
	}
	if sample.Status != SampleStatusInStock {
		return DestroyRow{}, fmt.Errorf("%w：样品当前「%s」，须在库才能发起销毁", ErrSampleState, sample.Status)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return DestroyRow{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return DestroyRow{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// 状态在锁内复查（并发发起只允许一笔）
	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM b_sample WHERE id = ? FOR UPDATE`, in.SampleID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return DestroyRow{}, fmt.Errorf("%w：#%d", ErrSampleNotFound, in.SampleID)
	}
	if err != nil {
		return DestroyRow{}, err
	}
	if status != SampleStatusInStock {
		return DestroyRow{}, fmt.Errorf("%w：样品当前「%s」", ErrSampleState, status)
	}

	// approved_by 列 NOT NULL：待审批行以空串占位，审批时必须填非空（uk 保证只发一次）
	res, err := tx.ExecContext(ctx, `
INSERT INTO b_sample_destroy (sample_id, destroyed_by, approved_by, reason, created_by)
VALUES (?,?, '', ?, ?)`, in.SampleID, destroyedBy, reason, actor.OpenID)
	if err != nil {
		if isDuplicateErr(err) {
			return DestroyRow{}, fmt.Errorf("%w：%s", ErrDestroyAlready, sample.SampleNo)
		}
		return DestroyRow{}, fmt.Errorf("销毁发起失败: %w", err)
	}
	destroyID, _ := res.LastInsertId()

	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_sample_destroy", EntityID: destroyID, Action: "destroy_init",
		Field:       "sample",
		NewValue:    sample.SampleNo + " 发起销毁：执行人 " + destroyedBy,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: reason,
	}); err != nil {
		return DestroyRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return DestroyRow{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetDestroyBySample(ctx, in.SampleID)
}

// ApproveDestroy 销毁**审批**：approved_by 必填非空（缺失 ⇒ ErrDestroyNeedApprover / 400）；
// 审批后同步置「已销毁」，审计记审批人。未经发起 ⇒ 409；已审批 ⇒ 409（一对象只销毁一次）。
func (s *Store) ApproveDestroy(ctx context.Context, sampleID int64, approvedBy string, actor MDActor) (DestroyRow, error) {
	approver := strings.TrimSpace(approvedBy)
	if approver == "" {
		return DestroyRow{}, ErrDestroyNeedApprover
	}
	sample, err := s.GetSample(ctx, sampleID)
	if err != nil {
		return DestroyRow{}, err
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return DestroyRow{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return DestroyRow{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var destroyID int64
	var curApproved, reason string
	err = tx.QueryRowContext(ctx,
		`SELECT id, approved_by, IFNULL(reason,'') FROM b_sample_destroy
		  WHERE sample_id = ? FOR UPDATE`, sampleID).
		Scan(&destroyID, &curApproved, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return DestroyRow{}, fmt.Errorf("%w：%s", ErrDestroyNotInit, sample.SampleNo)
	}
	if err != nil {
		return DestroyRow{}, err
	}
	if curApproved != "" {
		return DestroyRow{}, fmt.Errorf("%w：%s（审批人 %s）", ErrDestroyAlready, sample.SampleNo, curApproved)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE b_sample_destroy SET approved_by = ?, destroyed_at = NOW(3) WHERE id = ?`,
		approver, destroyID); err != nil {
		return DestroyRow{}, fmt.Errorf("销毁审批失败: %w", err)
	}
	if err := setSampleStatusTx(ctx, tx, sampleID, SampleStatusDestroyed); err != nil {
		return DestroyRow{}, err
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_sample_destroy", EntityID: destroyID, Action: "destroy_approve",
		Field:    "approved_by",
		OldValue: "", NewValue: approver,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: reason,
	}); err != nil {
		return DestroyRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return DestroyRow{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetDestroyBySample(ctx, sampleID)
}

// GetDestroyBySample 按样品取销毁记录（含发起人 + 审批人）。
func (s *Store) GetDestroyBySample(ctx context.Context, sampleID int64) (DestroyRow, error) {
	var r DestroyRow
	err := s.db.QueryRowContext(ctx, `
SELECT d.id, d.sample_id, b.sample_no,
       DATE_FORMAT(d.destroyed_at, '%Y-%m-%d %H:%i:%s'),
       d.destroyed_by, d.approved_by, IFNULL(d.reason, '')
  FROM b_sample_destroy d JOIN b_sample b ON b.id = d.sample_id
 WHERE d.sample_id = ?`, sampleID).
		Scan(&r.ID, &r.SampleID, &r.SampleNo, &r.DestroyedAt, &r.DestroyedBy, &r.ApprovedBy, &r.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return DestroyRow{}, fmt.Errorf("%w：样品 #%d 无销毁记录", ErrSampleNotFound, sampleID)
	}
	if err != nil {
		return DestroyRow{}, fmt.Errorf("读取销毁记录失败: %w", err)
	}
	r.Pending = r.ApprovedBy == ""
	return r, nil
}

// ListDestroys 列销毁记录（pendingOnly=只看待审批）。
func (s *Store) ListDestroys(ctx context.Context, pendingOnly bool) ([]DestroyRow, error) {
	q := `
SELECT d.id, d.sample_id, b.sample_no,
       DATE_FORMAT(d.destroyed_at, '%Y-%m-%d %H:%i:%s'),
       d.destroyed_by, d.approved_by, IFNULL(d.reason, '')
  FROM b_sample_destroy d JOIN b_sample b ON b.id = d.sample_id`
	if pendingOnly {
		q += ` WHERE d.approved_by = ''`
	}
	q += ` ORDER BY d.id DESC LIMIT 200`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("查询销毁记录失败: %w", err)
	}
	defer rows.Close()
	var out []DestroyRow
	for rows.Next() {
		var r DestroyRow
		if err := rows.Scan(&r.ID, &r.SampleID, &r.SampleNo, &r.DestroyedAt,
			&r.DestroyedBy, &r.ApprovedBy, &r.Reason); err != nil {
			return nil, fmt.Errorf("读取销毁记录失败: %w", err)
		}
		r.Pending = r.ApprovedBy == ""
		out = append(out, r)
	}
	return out, rows.Err()
}
