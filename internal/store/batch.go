package store

// ===== N-023 来料批次 · 持久化 =====
//
// ★ 业务口径（MIMO-NEXT-FIX-07 §0，用户五轮定案）：
//	① 批次 = 客户下单预填（同客户同产品多台车）；我方批次号 = 27 位追踪码新码类 F
//	   （DATE=预填日 + 三段序=HH/0MM/0SS 创建时刻 ⇒ 含时间戳、天然唯一；uk_our_batch_no 兜底）；
//	   唯一三元组 = (customer_id, material_id, customer_batch_no) ⇒ 重复预报 400 并列出已有批次。
//	④ 「回完货」= 手工关闭（幂等）；关闭后不能再预报新车次（CreateNotice 拒），
//	   已有关联车次/到货/检测数据不受影响。
//	⑤ 投料进度仅展示：按 truck_lot 聚合 b_bag.status='已投料'，**不驱动批次状态**。
//
// ★ L1 统计口径（分母写明，回执同步）：
//	车次数   = 批次下状态 ∈ {预报, 已到货} 的预报行数（已取消/空号不计 —— 与「有效车次」同义）
//	已到货   = 批次下 b_truck_lot 行数（经 notice 关联）
//	已投完   = 有效袋（b_bag.status <> '作废'）> 0 且全部有效袋 status='已投料' 的车次数
//	          （未过磅/未生成袋的车、袋全作废的车均不计入；三者呈 已投完 ≤ 已到货 ≤ 车次数 漏斗）

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/chadhao/jx-lab-trace/internal/codec"
)

// 批次状态字面量（与 spec/schema.sql 列注释、spec/enums.json 一致）。
const (
	BatchStatusOpen   = "回货中"
	BatchStatusClosed = "已关闭"
)

// N-023 哨兵错误（httpapi 据此映射状态码）。
var (
	// ErrBatchClosed ⇒ 400：批次已关闭，不能再预报新车次。
	ErrBatchClosed = errors.New("批次已关闭，不能再预报新车次")
	// ErrBatchDuplicate ⇒ 400（带已有批次列表）：三元组重复预填。
	ErrBatchDuplicate = errors.New("该客户+产品+客户批次号已存在批次")
)

// BatchDuplicateError 携带已有批次（响应体供前端跳转）。
type BatchDuplicateError struct {
	Existing []IncomingBatch
}

func (e *BatchDuplicateError) Error() string { return ErrBatchDuplicate.Error() }

// Unwrap 使 errors.Is(err, ErrBatchDuplicate) 成立。
func (e *BatchDuplicateError) Unwrap() error { return ErrBatchDuplicate }

// IncomingBatch 是来料批次的读模型（含 L1 统计）。
type IncomingBatch struct {
	ID                int64      `json:"id"`
	OurBatchNo        string     `json:"our_batch_no"`
	CustomerBatchNo   string     `json:"customer_batch_no"`
	CustomerID        int64      `json:"customer_id"`
	CustomerCode      string     `json:"customer_code"`
	CustomerName      string     `json:"customer_name"`
	MaterialID        int64      `json:"material_id"`
	MaterialCode      string     `json:"material_code"`
	MaterialName      string     `json:"material_name"`
	Status            string     `json:"status"`
	ClosedAt          *time.Time `json:"closed_at"`
	ClosedBy          string     `json:"closed_by"`
	CreatedAt         *time.Time `json:"created_at"`
	CreatedBy         string     `json:"created_by"`
	NoticeCount       int        `json:"notice_count"`   // 车次数（预报 + 已到货）
	ArrivedCount      int        `json:"arrived_count"`  // 已到货
	FedDoneCount      int        `json:"fed_done_count"` // 已投完（有效袋全部已投料）
	LegacyNoticeCount int        `json:"-"`              // 内部：L3 旧版预报计数（不入批次行）
}

// BatchInput 是预填批次的入参。
type BatchInput struct {
	CustomerID      int64  `json:"customer_id"`
	MaterialID      int64  `json:"material_id"`
	CustomerBatchNo string `json:"customer_batch_no"`
}

// BatchFilter 是 L3 批次侧筛选（空串/0 = 不限）。
type BatchFilter struct {
	CustomerID      int64
	MaterialID      int64
	CustomerBatchNo string
	OurBatchNo      string
	Status          string
	Limit           int
}

const incomingBatchSelect = `
SELECT b.id, b.our_batch_no, b.customer_batch_no,
       b.customer_id, COALESCE(c.code,''), COALESCE(c.name,''),
       b.material_id, COALESCE(m.code,''), COALESCE(m.name,''),
       b.status, b.closed_at, COALESCE(b.closed_by,''),
       b.created_at, b.created_by
FROM m_incoming_batch b
LEFT JOIN m_customer c ON c.id = b.customer_id AND c.is_current = 1
LEFT JOIN m_material m ON m.id = b.material_id AND m.is_current = 1`

func scanIncomingBatch(sc interface{ Scan(...interface{}) error }) (IncomingBatch, error) {
	var b IncomingBatch
	var closedAt, createdAt sql.NullTime
	err := sc.Scan(&b.ID, &b.OurBatchNo, &b.CustomerBatchNo,
		&b.CustomerID, &b.CustomerCode, &b.CustomerName,
		&b.MaterialID, &b.MaterialCode, &b.MaterialName,
		&b.Status, &closedAt, &b.ClosedBy, &createdAt, &b.CreatedBy)
	if err != nil {
		return IncomingBatch{}, err
	}
	if closedAt.Valid {
		t := closedAt.Time
		b.ClosedAt = &t
	}
	if createdAt.Valid {
		t := createdAt.Time
		b.CreatedAt = &t
	}
	return b, nil
}

// CreateBatch 预填批次：三元组重复 ⇒ *BatchDuplicateError（携带已有行）。
func (s *Store) CreateBatch(ctx context.Context, in BatchInput, actor MDActor) (IncomingBatch, error) {
	in.CustomerBatchNo = strings.TrimSpace(in.CustomerBatchNo)
	if in.CustomerID <= 0 || in.MaterialID <= 0 {
		return IncomingBatch{}, fmt.Errorf("%w：必须选择客户与原料物料", ErrRecvBadInput)
	}
	if in.CustomerBatchNo == "" {
		return IncomingBatch{}, fmt.Errorf("%w：客户批次号不能为空", ErrRecvBadInput)
	}
	if len(in.CustomerBatchNo) > 64 {
		return IncomingBatch{}, fmt.Errorf("%w：客户批次号最长 64 字符", ErrRecvBadInput)
	}
	// 主数据有效性 + 取 4 位码（F 码的 CUSTOMER/MATERIAL 段）。
	custCode, matCode, err := s.lookupCodes(ctx, in.CustomerID, in.MaterialID, BizTypeCG)
	if err != nil {
		return IncomingBatch{}, err
	}
	// 三元组快速预检（竞态由 uk_batch_triple 兜底）。
	if dup, err := s.batchTripleExists(ctx, in); err != nil {
		return IncomingBatch{}, err
	} else if dup != nil {
		return IncomingBatch{}, dup
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return IncomingBatch{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return IncomingBatch{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// ★ F 码唯一性（见 genBatchCode 注释）：分钟时间戳 + 随机防撞段；
	//	冲突（同分同客户同物料同随机，概率 1/1000）⇒ **原地重掷随机**，时间不前移。
	//	三元组冲突不重试，直接回已有批次。
	var id int64
	var ourNo string
	for attempt := 0; attempt < 5; attempt++ {
		ourNo, err = genBatchCode(custCode, matCode, time.Now())
		if err != nil {
			return IncomingBatch{}, err
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO m_incoming_batch
  (our_batch_no, customer_batch_no, customer_id, material_id, status, created_by)
VALUES (?,?,?,?,?,?)`,
			ourNo, in.CustomerBatchNo, in.CustomerID, in.MaterialID, BatchStatusOpen, actor.OpenID)
		if err == nil {
			id, _ = res.LastInsertId()
			break
		}
		if !isDuplicateErr(err) {
			return IncomingBatch{}, fmt.Errorf("预填批次失败: %w", err)
		}
		// 区分两个唯一键：三元组冲突 ⇒ 返回已有；码冲突 ⇒ 换时刻重试。
		if dup, derr := s.batchTripleExists(ctx, in); derr != nil {
			return IncomingBatch{}, derr
		} else if dup != nil {
			return IncomingBatch{}, dup
		}
		if attempt == 4 {
			return IncomingBatch{}, fmt.Errorf("%w：批次号生成冲突，请重试", ErrSeqBusy)
		}
	}
	if id == 0 {
		return IncomingBatch{}, fmt.Errorf("%w：批次号生成冲突，请重试", ErrSeqBusy)
	}

	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "m_incoming_batch", EntityID: id, Action: "create",
		NewValue:    fmt.Sprintf("%s 客户批次%s", ourNo, in.CustomerBatchNo),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return IncomingBatch{}, err
	}
	if err := tx.Commit(); err != nil {
		return IncomingBatch{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetBatch(ctx, id)
}

// batchTripleExists 查三元组已有批次；命中 ⇒ 包装为 *BatchDuplicateError。
func (s *Store) batchTripleExists(ctx context.Context, in BatchInput) (*BatchDuplicateError, error) {
	rows, err := s.db.QueryContext(ctx, incomingBatchSelect+`
WHERE b.customer_id = ? AND b.material_id = ? AND b.customer_batch_no = ?
ORDER BY b.id`, in.CustomerID, in.MaterialID, in.CustomerBatchNo)
	if err != nil {
		return nil, fmt.Errorf("查询已有批次失败: %w", err)
	}
	defer rows.Close()
	existing := []IncomingBatch{}
	for rows.Next() {
		b, err := scanIncomingBatch(rows)
		if err != nil {
			return nil, err
		}
		existing = append(existing, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return &BatchDuplicateError{Existing: existing}, nil
	}
	return nil, nil
}

// GetBatch 读单个批次（含统计）。
func (s *Store) GetBatch(ctx context.Context, id int64) (IncomingBatch, error) {
	row := s.db.QueryRowContext(ctx, incomingBatchSelect+` WHERE b.id = ?`, id)
	b, err := scanIncomingBatch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return IncomingBatch{}, fmt.Errorf("%w：来料批次 #%d", ErrRecvNotFound, id)
	}
	if err != nil {
		return IncomingBatch{}, err
	}
	stats, err := s.batchStats(ctx, []int64{id})
	if err != nil {
		return IncomingBatch{}, err
	}
	b.attachStats(stats)
	return b, nil
}

// ListBatches 列批次（L1 = status 回货中；空 status = 全部）。
func (s *Store) ListBatches(ctx context.Context, status string) ([]IncomingBatch, error) {
	q := incomingBatchSelect
	args := []interface{}{}
	if st := strings.TrimSpace(status); st != "" {
		q += ` WHERE b.status = ?`
		args = append(args, st)
	}
	q += ` ORDER BY b.id DESC LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询批次失败: %w", err)
	}
	defer rows.Close()
	out := []IncomingBatch{}
	for rows.Next() {
		b, err := scanIncomingBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.fillBatchStats(ctx, out)
}

// ListBatchesAll 是 L3 批次侧查询（多条件）。
func (s *Store) ListBatchesAll(ctx context.Context, f BatchFilter) ([]IncomingBatch, error) {
	q := incomingBatchSelect
	conds := []string{}
	args := []interface{}{}
	if f.CustomerID > 0 {
		conds = append(conds, "b.customer_id = ?")
		args = append(args, f.CustomerID)
	}
	if f.MaterialID > 0 {
		conds = append(conds, "b.material_id = ?")
		args = append(args, f.MaterialID)
	}
	if v := strings.TrimSpace(f.CustomerBatchNo); v != "" {
		conds = append(conds, "b.customer_batch_no LIKE ? ESCAPE '\\'")
		args = append(args, likeEscape(v))
	}
	if v := strings.TrimSpace(f.OurBatchNo); v != "" {
		conds = append(conds, "b.our_batch_no = ?")
		args = append(args, strings.ToUpper(v))
	}
	if v := strings.TrimSpace(f.Status); v != "" {
		conds = append(conds, "b.status = ?")
		args = append(args, v)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	q += fmt.Sprintf(" ORDER BY b.id DESC LIMIT %d", limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询批次失败: %w", err)
	}
	defer rows.Close()
	out := []IncomingBatch{}
	for rows.Next() {
		b, err := scanIncomingBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.fillBatchStats(ctx, out)
}

// CloseBatch 手工关闭批次（幂等：已关闭再关 ⇒ 原样返回，closed_at 不变）。
func (s *Store) CloseBatch(ctx context.Context, id int64, actor MDActor) (IncomingBatch, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IncomingBatch{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM m_incoming_batch WHERE id = ? FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return IncomingBatch{}, fmt.Errorf("%w：来料批次 #%d", ErrRecvNotFound, id)
	}
	if err != nil {
		return IncomingBatch{}, fmt.Errorf("读取批次失败: %w", err)
	}
	if status == BatchStatusClosed {
		// 幂等：直接返回现状（closed_at/by 保持首次关闭的值）。
		if err := tx.Commit(); err != nil {
			return IncomingBatch{}, fmt.Errorf("提交失败: %w", err)
		}
		return s.GetBatch(ctx, id)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE m_incoming_batch
SET status = ?, closed_at = CURRENT_TIMESTAMP(3), closed_by = ?
WHERE id = ? AND status = ?`,
		BatchStatusClosed, actor.OpenID, id, BatchStatusOpen); err != nil {
		return IncomingBatch{}, fmt.Errorf("关闭批次失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "m_incoming_batch", EntityID: id, Action: "close",
		Field: "status", OldValue: BatchStatusOpen, NewValue: BatchStatusClosed,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return IncomingBatch{}, err
	}
	if err := tx.Commit(); err != nil {
		return IncomingBatch{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetBatch(ctx, id)
}

// ===== L1 统计（三条聚合查询，按批次 id 集合一次取齐，避免 N+1）=====

type batchStat struct {
	NoticeCount  int
	ArrivedCount int
	FedDoneCount int
}

func (b *IncomingBatch) attachStats(m map[int64]batchStat) {
	st := m[b.ID]
	b.NoticeCount = st.NoticeCount
	b.ArrivedCount = st.ArrivedCount
	b.FedDoneCount = st.FedDoneCount
}

func (s *Store) fillBatchStats(ctx context.Context, bs []IncomingBatch) error {
	if len(bs) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(bs))
	for _, b := range bs {
		ids = append(ids, b.ID)
	}
	m, err := s.batchStats(ctx, ids)
	if err != nil {
		return err
	}
	for i := range bs {
		bs[i].attachStats(m)
	}
	return nil
}

func (s *Store) batchStats(ctx context.Context, ids []int64) (map[int64]batchStat, error) {
	m := map[int64]batchStat{}
	if len(ids) == 0 {
		return m, nil
	}
	ph := placeholders(len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
		m[id] = batchStat{}
	}

	// 车次数：预报 + 已到货（已取消/空号不算有效车次）。
	rows, err := s.db.QueryContext(ctx, `
SELECT n.batch_id, COUNT(*) FROM b_arrival_notice n
WHERE n.batch_id IN (`+ph+`) AND n.status IN ('预报','已到货')
GROUP BY n.batch_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("统计批次车次数失败: %w", err)
	}
	for rows.Next() {
		var id int64
		var c int
		if err := rows.Scan(&id, &c); err != nil {
			rows.Close()
			return nil, err
		}
		st := m[id]
		st.NoticeCount = c
		m[id] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 已到货：批次下实际生成的车次行。
	rows, err = s.db.QueryContext(ctx, `
SELECT n.batch_id, COUNT(DISTINCT t.id)
FROM b_truck_lot t
JOIN b_arrival_notice n ON t.notice_id = n.id
WHERE n.batch_id IN (`+ph+`)
GROUP BY n.batch_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("统计批次已到货失败: %w", err)
	}
	for rows.Next() {
		var id int64
		var c int
		if err := rows.Scan(&id, &c); err != nil {
			rows.Close()
			return nil, err
		}
		st := m[id]
		st.ArrivedCount = c
		m[id] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 已投完：有效袋 > 0 且全部有效袋已投料 的车次。
	rows, err = s.db.QueryContext(ctx, `
SELECT batch_id, COUNT(*) FROM (
  SELECT n.batch_id AS batch_id, t.id,
         SUM(CASE WHEN g.status <> '作废' THEN 1 ELSE 0 END) AS valid_cnt,
         SUM(CASE WHEN g.status = '已投料' THEN 1 ELSE 0 END) AS fed_cnt
  FROM b_truck_lot t
  JOIN b_arrival_notice n ON t.notice_id = n.id
  JOIN b_bag g ON g.truck_lot_id = t.id
  WHERE n.batch_id IN (`+ph+`)
  GROUP BY n.batch_id, t.id
  HAVING valid_cnt > 0 AND fed_cnt = valid_cnt
) x GROUP BY batch_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("统计批次投料进度失败: %w", err)
	}
	for rows.Next() {
		var id int64
		var c int
		if err := rows.Scan(&id, &c); err != nil {
			rows.Close()
			return nil, err
		}
		st := m[id]
		st.FedDoneCount = c
		m[id] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

// genBatchCode 生成 F 码（含创建时刻时间戳 + 随机防撞段）：
// DATE = YYMMDD（预填日），SEQ1 = HH，SEQ2 = 分钟（3 位左补零），
// SEQ3 = **000-999 随机防撞段**（crypto/rand）。
//
// ★ 唯一口径（服务器 TC 实测修正）：曾用「秒级时间戳 + 冲突后时间+1s 重试」——
//
//	高频预填（同客户同物料 <1s 连续多批次）下，重试写入的**超前时间戳**会级联占位：
//	超前量逐次累积到 +4s 仍撞 ⇒ 5 连败假死。
//	改为「分钟时间戳 + 随机段」后：同分同客户同物料撞率 1/1000，撞了**原地重掷随机**
//	（时间不前移 ⇒ 不产生超前占位、无级联）；uk_our_batch_no 仍作最终兜底。
func genBatchCode(custCode, matCode string, t time.Time) (string, error) {
	buf := make([]byte, 2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成批次号随机段失败: %w", err)
	}
	rnd := int(binary.BigEndian.Uint16(buf)) % 1000 // 0..999
	return codec.Generate(codec.Segments{
		T:        "F",
		BT:       BizTypeCG, // 批次必含客户（DDL customer_id NOT NULL）⇒ 恒客供
		Customer: custCode,
		Material: matCode,
		Date:     yymmdd(t.Format("2006-01-02")),
		SEQ1:     pad(t.Hour(), 2),
		SEQ2:     pad(t.Minute(), 3),
		SEQ3:     pad(rnd, 3),
	})
}

// likeEscape 转义 LIKE 通配符（配 ESCAPE '\\' 使用）。
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}
