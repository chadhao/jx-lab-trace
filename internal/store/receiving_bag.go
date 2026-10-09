package store

// ===== M3 收货与打码 · 持久化（D4 过磅与袋码 / D5 标签打印 / D6 作废与退车）=====
//
// ★★ 袋码只在「过磅确认后」生成（spec#generation.B：不按预报袋数预打）：
//	预报阶段 b_bag 一行都没有；过磅后按**实际**袋数 N 批量生成。
// ★ 袋重 = 车净重 ÷ 袋数，落 weight_allocated；★ weight_is_allocated **恒 1**
//	（重量是摊算值，不是实测）。
// ★ 作废统一走 b_obj_void（uk_void_entity 保证一对象只作废一次），被作废袋
//	b_bag.status 置「作废」；★ 已取样 / 已投料的袋拒绝作废；
//	★ 车次 bag_count 按**有效袋**计（作废袋不计）。
// ★ 退车必须先有质检「退货」处置判定，否则拒绝；通过后车次转「已退货」
//	且该车**全部**袋码作废。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/chadhao/jx-lab-trace/internal/codec"
)

// ===== 过磅（D4）=====

// WeighInput 是过磅入参（吨，最多 3 位小数）。
type WeighInput struct {
	Gross tons `json:"gross_weight"`
	Tare  tons `json:"tare_weight"`
}

// WeighTruck 录毛重 / 皮重 ⇒ 净重 = 毛重 − 皮重（DECIMAL(18,3)，单位吨）。
func (s *Store) WeighTruck(ctx context.Context, id int64, in WeighInput, actor MDActor) (TruckLot, error) {
	netMilli := in.Gross.Milli() - in.Tare.Milli()
	if in.Gross.Milli() <= 0 {
		return TruckLot{}, fmt.Errorf("%w：毛重必须大于 0", ErrRecvBadInput)
	}
	if in.Tare.Milli() < 0 {
		return TruckLot{}, fmt.Errorf("%w：皮重不能为负", ErrRecvBadInput)
	}
	if netMilli <= 0 {
		return TruckLot{}, fmt.Errorf("%w：净重 = 毛重 − 皮重 必须大于 0", ErrRecvBadInput)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TruckLot{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	cur, err := scanTruck(tx.QueryRowContext(ctx, truckSelect+` WHERE t.id = ? FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return TruckLot{}, fmt.Errorf("%w：车次 #%d", ErrRecvNotFound, id)
	}
	if err != nil {
		return TruckLot{}, err
	}
	if cur.Status == TruckStatusReturned {
		return TruckLot{}, fmt.Errorf("%w：车次已退货，不能过磅", ErrTruckState)
	}
	var bags int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_bag WHERE truck_lot_id = ?`, id).Scan(&bags); err != nil {
		return TruckLot{}, fmt.Errorf("统计袋数失败: %w", err)
	}
	if bags > 0 {
		return TruckLot{}, ErrWeighAfterBags
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE b_truck_lot SET gross_weight = ?, tare_weight = ?, net_weight = ? WHERE id = ?`,
		in.Gross.String(), in.Tare.String(), milliStr(netMilli), id); err != nil {
		return TruckLot{}, fmt.Errorf("过磅失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_truck_lot", EntityID: id, Action: "weigh",
		Field: "net_weight", OldValue: fmtFloat(cur.NetWeight), NewValue: milliStr(netMilli),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return TruckLot{}, err
	}
	if err := tx.Commit(); err != nil {
		return TruckLot{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetTruck(ctx, id)
}

// ===== 批量袋码（D4）=====

// Bag 是原料吨袋的读模型。
type Bag struct {
	ID                int64      `json:"id"`
	Code              string     `json:"code"`
	Human             string     `json:"human"`
	TruckLotID        int64      `json:"truck_lot_id"`
	BagSeq            int        `json:"bag_seq"`
	WeightAllocated   *float64   `json:"weight_allocated"`
	WeightIsAllocated int        `json:"weight_is_allocated"`
	Status            string     `json:"status"`
	CreatedAt         *time.Time `json:"created_at"`
	CreatedBy         string     `json:"created_by"`
}

const bagSelect = `
SELECT b.id, b.code, b.truck_lot_id, b.bag_seq, b.weight_allocated,
       b.weight_is_allocated, b.status, b.created_at, b.created_by
FROM b_bag b`

func scanBag(sc interface{ Scan(...interface{}) error }) (Bag, error) {
	var b Bag
	var w sql.NullFloat64
	var createdAt sql.NullTime
	err := sc.Scan(&b.ID, &b.Code, &b.TruckLotID, &b.BagSeq, &w,
		&b.WeightIsAllocated, &b.Status, &createdAt, &b.CreatedBy)
	if err != nil {
		return Bag{}, err
	}
	if w.Valid {
		v := w.Float64
		b.WeightAllocated = &v
	}
	if createdAt.Valid {
		t := createdAt.Time
		b.CreatedAt = &t
	}
	if p, err := codec.ToHuman(b.Code); err == nil {
		b.Human = p
	}
	return b, nil
}

// ListBags 列某车次的袋（按袋序）。
func (s *Store) ListBags(ctx context.Context, truckLotID int64) ([]Bag, error) {
	rows, err := s.db.QueryContext(ctx, bagSelect+` WHERE truck_lot_id = ? ORDER BY bag_seq`, truckLotID)
	if err != nil {
		return nil, fmt.Errorf("查询袋码失败: %w", err)
	}
	defer rows.Close()
	out := []Bag{}
	for rows.Next() {
		b, err := scanBag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBag 读单个袋。
func (s *Store) GetBag(ctx context.Context, id int64) (Bag, error) {
	b, err := scanBag(s.db.QueryRowContext(ctx, bagSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Bag{}, fmt.Errorf("%w：袋 #%d", ErrRecvNotFound, id)
	}
	return b, err
}

// GenerateBags 过磅确认后按**实际**袋数 N 批量生成 N 个袋码 B。
//
// ★★ 预报阶段不预打（spec#generation.B）：本函数只在过磅（有净重）后可调用；
//
//	该车已生成过袋码 ⇒ 拒绝重复生成（作废多余袋走 VoidBag，不回炉重打）。
func (s *Store) GenerateBags(ctx context.Context, truckLotID int64, count int, actor MDActor) ([]Bag, error) {
	if count <= 0 {
		return nil, fmt.Errorf("%w：袋数必须大于 0", ErrRecvBadInput)
	}
	if count > BagSeqMax {
		return nil, fmt.Errorf("%w：袋数 %d 超过上限 %d（袋序定宽 3 位）", ErrRecvBadInput, count, BagSeqMax)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	t, err := scanTruck(tx.QueryRowContext(ctx, truckSelect+` WHERE t.id = ? FOR UPDATE`, truckLotID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w：车次 #%d", ErrRecvNotFound, truckLotID)
	}
	if err != nil {
		return nil, err
	}
	if t.Status == TruckStatusReturned {
		return nil, fmt.Errorf("%w：车次已退货，不能生成袋码", ErrTruckState)
	}
	if t.NetWeight == nil {
		return nil, ErrNotWeighed
	}
	var existing int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_bag WHERE truck_lot_id = ?`, truckLotID).Scan(&existing); err != nil {
		return nil, fmt.Errorf("统计袋数失败: %w", err)
	}
	if existing > 0 {
		return nil, ErrBagsExist
	}

	// 车码里就带着 BT / 客户 / 物料 / 链根日期 / 车序 ⇒ 袋码前缀直接复用（同链同前缀）
	parsed, err := codec.Parse(t.Code)
	if err != nil {
		return nil, fmt.Errorf("%w：车码不可解析：%v", ErrRecvBadInput, err)
	}
	netMilli := int64(math.Round(*t.NetWeight * 1000))
	allocMilli := divMilliRound(netMilli, int64(count))

	ids := make([]int64, 0, count)
	for i := 1; i <= count; i++ {
		code, err := codec.Generate(codec.Segments{
			T:        "B",
			BT:       parsed.Seg.BT,
			Customer: parsed.Seg.Customer,
			Material: parsed.Seg.Material,
			Date:     parsed.Seg.Date,
			SEQ1:     parsed.Seg.SEQ1,
			SEQ2:     pad(i, 3),
			SEQ3:     codec.Placeholder,
		})
		if err != nil {
			return nil, fmt.Errorf("生成袋码失败: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO b_bag (code, truck_lot_id, bag_seq, weight_allocated, weight_is_allocated, status, created_by)
VALUES (?,?,?,?,1,?,?)`,
			code, truckLotID, i, milliStr(allocMilli), BagStatusInStock, actor.OpenID)
		if err != nil {
			if isDuplicateErr(err) {
				return nil, fmt.Errorf("%w：袋码 %s 已存在（作废码永久不重用）", ErrRecvBadInput, code)
			}
			return nil, fmt.Errorf("生成袋码失败: %w", err)
		}
		bid, _ := res.LastInsertId()
		ids = append(ids, bid)
	}
	// bag_count = 有效袋数（此时尚无作废，故 = N）
	if _, err := tx.ExecContext(ctx,
		`UPDATE b_truck_lot SET bag_count = ? WHERE id = ?`, count, truckLotID); err != nil {
		return nil, fmt.Errorf("更新袋数失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_truck_lot", EntityID: truckLotID, Action: "bag_gen",
		NewValue:    fmt.Sprintf("%d 个袋码，摊算袋重 %s 吨", count, milliStr(allocMilli)),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	return s.ListBags(ctx, truckLotID)
}

// ===== 标签打印与补打（D5）=====

// LabelItem 是一条打印记录（含人读行，供版式页与历史回看）。
type LabelItem struct {
	ID        int64      `json:"id"`
	Code      string     `json:"code"`
	Human     string     `json:"human"`
	Kind      string     `json:"kind"` // truck / bag
	IsReprint int        `json:"is_reprint"`
	Reason    string     `json:"reason"`
	PrintedBy string     `json:"printed_by"`
	PrintedAt *time.Time `json:"printed_at"`
}

// LabelPrintInput 是打印入参。
type LabelPrintInput struct {
	Codes   []string `json:"codes"`
	Reprint bool     `json:"is_reprint"`
	Reason  string   `json:"reason"`
}

// PrintLabels 批量打印：逐码留痕（b_label_print），返回可渲染的标签项。
//
// ★ 补打必须填原因（is_reprint=1 + reason 非空）—— 且**该码已打印过**却没走补打
//
//	⇒ 一并拒绝（否则「一物两码」可以从后门绕过）。
//
// ★ 作废对象拒绝打印（作废码永久不重用，不能把死码重新贴回实物）。
func (s *Store) PrintLabels(ctx context.Context, in LabelPrintInput, actor MDActor) ([]LabelItem, error) {
	if len(in.Codes) == 0 {
		return nil, fmt.Errorf("%w：没有要打印的码", ErrRecvBadInput)
	}
	reason := strings.TrimSpace(in.Reason)
	isReprint := in.Reprint
	if isReprint && reason == "" {
		return nil, ErrReprintReason
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	out := make([]LabelItem, 0, len(in.Codes))
	for _, raw := range in.Codes {
		code := strings.ToUpper(strings.TrimSpace(raw))
		kind, err := labelObjectKind(ctx, tx, code)
		if err != nil {
			return nil, err
		}
		var printed int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM b_label_print WHERE code = ?`, code).Scan(&printed); err != nil {
			return nil, fmt.Errorf("查询打印历史失败: %w", err)
		}
		effective := isReprint || printed > 0
		if effective && reason == "" {
			return nil, fmt.Errorf("%w：码 %s 已打印过，再次打印必须走补打并填写原因", ErrReprintReason, code)
		}
		flag := 0
		if effective {
			flag = 1
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO b_label_print (code, printed_by, is_reprint, reason, created_by)
VALUES (?,?,?,?,?)`, code, actor.Name, flag, nullStr(reason), actor.OpenID)
		if err != nil {
			return nil, fmt.Errorf("写打印记录失败: %w", err)
		}
		pid, _ := res.LastInsertId()
		human, err := codec.ToHuman(code)
		if err != nil {
			return nil, err
		}
		out = append(out, LabelItem{
			ID: pid, Code: code, Human: human, Kind: kind,
			IsReprint: flag, Reason: reason, PrintedBy: actor.Name,
		})
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_label_print", Action: "print",
		NewValue:    fmt.Sprintf("%d 张（补打 %d 张）", len(out), countReprint(out)),
		Reason:      reason,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	return out, nil
}

func countReprint(items []LabelItem) int {
	n := 0
	for _, it := range items {
		if it.IsReprint == 1 {
			n++
		}
	}
	return n
}

// labelObjectKind 解析码并确认对象在库、未作废；返回对象类别。
func labelObjectKind(ctx context.Context, tx *sql.Tx, code string) (string, error) {
	p, err := codec.Parse(code)
	if err != nil {
		return "", err
	}
	switch p.Seg.T {
	case "A":
		var one int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM b_truck_lot WHERE code = ?`, code).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w：车码 %s", ErrCodeUnknown, code)
		} else if err != nil {
			return "", err
		}
		return "车次", nil
	case "B":
		var status string
		if err := tx.QueryRowContext(ctx,
			`SELECT status FROM b_bag WHERE code = ?`, code).Scan(&status); errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w：袋码 %s", ErrCodeUnknown, code)
		} else if err != nil {
			return "", err
		}
		if status == BagStatusVoid {
			return "", fmt.Errorf("%w：袋码 %s", ErrAlreadyVoid, code)
		}
		return "原料吨袋", nil
	default:
		return "", ErrCodeUnsupported
	}
}

// ListLabelPrints 查某个码的打印留痕（补打历史）。
func (s *Store) ListLabelPrints(ctx context.Context, code string) ([]LabelItem, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, code, is_reprint, COALESCE(reason,''), printed_by, printed_at
FROM b_label_print WHERE code = ? ORDER BY id DESC LIMIT 200`, code)
	if err != nil {
		return nil, fmt.Errorf("查询打印记录失败: %w", err)
	}
	return collectLabelItems(rows)
}

// GetLabelItems 按打印记录 id 取标签项（标签版式页按 ids 渲染）。
func (s *Store) GetLabelItems(ctx context.Context, ids []int64) ([]LabelItem, error) {
	if len(ids) == 0 {
		return []LabelItem{}, nil
	}
	ph := make([]string, 0, len(ids))
	args := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		ph = append(ph, "?")
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, code, is_reprint, COALESCE(reason,''), printed_by, printed_at
FROM b_label_print WHERE id IN (`+strings.Join(ph, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("查询打印记录失败: %w", err)
	}
	return collectLabelItems(rows)
}

func collectLabelItems(rows *sql.Rows) ([]LabelItem, error) {
	defer rows.Close()
	out := []LabelItem{}
	for rows.Next() {
		var it LabelItem
		var at sql.NullTime
		if err := rows.Scan(&it.ID, &it.Code, &it.IsReprint, &it.Reason, &it.PrintedBy, &at); err != nil {
			return nil, err
		}
		if at.Valid {
			t := at.Time
			it.PrintedAt = &t
		}
		it.Human, _ = codec.ToHuman(it.Code)
		if p, err := codec.Parse(it.Code); err == nil {
			if spec, ok := codec.ObjectOf(p.Seg.T[0]); ok {
				it.Kind = spec.Name
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ===== 袋作废与退车（D6）=====

// VoidBag 对单个袋执行作废（填原因、留痕、码永不重用）。
//
// ★★ 已取样 / 已投料的袋 ⇒ 拒绝（说明现场已经用过它）。
func (s *Store) VoidBag(ctx context.Context, bagID int64, reason string, actor MDActor) (Bag, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Bag{}, ErrVoidReason
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Bag{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	bag, err := scanBag(tx.QueryRowContext(ctx, bagSelect+` WHERE id = ? FOR UPDATE`, bagID))
	if errors.Is(err, sql.ErrNoRows) {
		return Bag{}, fmt.Errorf("%w：袋 #%d", ErrRecvNotFound, bagID)
	}
	if err != nil {
		return Bag{}, err
	}
	if bag.Status == BagStatusVoid {
		return Bag{}, fmt.Errorf("%w：袋 #%d", ErrAlreadyVoid, bagID)
	}

	// ★ 已取样 / 已投料 ⇒ 拒绝作废
	var used int
	if err := tx.QueryRowContext(ctx, `
SELECT (SELECT COUNT(*) FROM b_sample WHERE bag_id = ?)
     + (SELECT COUNT(*) FROM b_feed_record WHERE bag_id = ?)`, bagID, bagID).Scan(&used); err != nil {
		return Bag{}, fmt.Errorf("检查袋使用状态失败: %w", err)
	}
	if used > 0 {
		return Bag{}, ErrBagInUse
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO b_obj_void (entity, entity_id, reason, voided_by, created_by)
VALUES ('b_bag', ?, ?, ?, ?)`, bagID, reason, actor.Name, actor.OpenID); err != nil {
		if isDuplicateErr(err) {
			return Bag{}, fmt.Errorf("%w：袋 #%d", ErrAlreadyVoid, bagID)
		}
		return Bag{}, fmt.Errorf("写作废记录失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE b_bag SET status = ? WHERE id = ?`, BagStatusVoid, bagID); err != nil {
		return Bag{}, fmt.Errorf("作废失败: %w", err)
	}
	if err := refreshBagCount(ctx, tx, bag.TruckLotID); err != nil {
		return Bag{}, err
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_bag", EntityID: bagID, Action: "void",
		Field: "status", OldValue: bag.Status, NewValue: BagStatusVoid, Reason: reason,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return Bag{}, err
	}
	if err := tx.Commit(); err != nil {
		return Bag{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetBag(ctx, bagID)
}

// refreshBagCount 重算车次 bag_count（★ 按**有效袋**计，作废袋不计）。
func refreshBagCount(ctx context.Context, tx *sql.Tx, truckLotID int64) error {
	if truckLotID <= 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
UPDATE b_truck_lot SET bag_count =
  (SELECT COUNT(*) FROM b_bag WHERE truck_lot_id = ? AND status <> '作废')
WHERE id = ?`, truckLotID, truckLotID)
	if err != nil {
		return fmt.Errorf("重算有效袋数失败: %w", err)
	}
	return nil
}

// ReturnTruck 退车登记：★ 必须先有质检「退货」处置判定，否则拒绝。
//
// 通过后车次转「已退货」，且该车**全部**袋码作废（整批一并处理，不再逐袋判断
// 已取样/已投料 —— 退车意味着整车退回，取样/投料的前置本身已不成立）。
func (s *Store) ReturnTruck(ctx context.Context, truckLotID int64, reason string, actor MDActor) (TruckLot, error) {
	reason = strings.TrimSpace(reason)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TruckLot{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	t, err := scanTruck(tx.QueryRowContext(ctx, truckSelect+` WHERE t.id = ? FOR UPDATE`, truckLotID))
	if errors.Is(err, sql.ErrNoRows) {
		return TruckLot{}, fmt.Errorf("%w：车次 #%d", ErrRecvNotFound, truckLotID)
	}
	if err != nil {
		return TruckLot{}, err
	}
	// ★ 批 5 联动：M5 的 D5 让「处置=退货」即把状态写成「已退货」，但退车登记
	//	（作废袋码）仍须可执行 —— 故只在「已退货 **且** 袋已全部作废（bag_count=0）」
	//	时判重复退车；仅状态翻转不拦（否则 API 链路下退车登记永远走不到）。
	if t.Status == TruckStatusReturned && t.BagCount == 0 {
		return TruckLot{}, fmt.Errorf("%w：车次 #%d 已退货", ErrAlreadyVoid, truckLotID)
	}

	// ★ 前置：质检处置 = 退货（M5 的判定在 b_inspection.disposition）
	// ★★ 联动修正（批 5 §6-9）：只认**现行单** —— 排除已被 b_obj_void 作废的检测单，
	//	否则「修正后处置已改合格」的车次仍会被已作废单的旧 退货 判定"复活"。
	var inspID int64
	err = tx.QueryRowContext(ctx, `
SELECT i.id FROM b_inspection i
 WHERE i.target_type = '车次' AND i.target_id = ? AND i.disposition = '退货'
   AND NOT EXISTS (SELECT 1 FROM b_obj_void v
                    WHERE v.entity = 'b_inspection' AND v.entity_id = i.id)
 ORDER BY i.id DESC LIMIT 1`, truckLotID).Scan(&inspID)
	if errors.Is(err, sql.ErrNoRows) {
		return TruckLot{}, ErrNoReturnDecision
	}
	if err != nil {
		return TruckLot{}, fmt.Errorf("查询质检处置失败: %w", err)
	}

	voidReason := reason
	if voidReason == "" {
		voidReason = "退车登记，整车袋码作废"
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO b_obj_void (entity, entity_id, reason, voided_by, created_by)
SELECT 'b_bag', id, ?, ?, ? FROM b_bag WHERE truck_lot_id = ? AND status <> '作废'`,
		voidReason, actor.Name, actor.OpenID, truckLotID); err != nil {
		if isDuplicateErr(err) {
			return TruckLot{}, fmt.Errorf("%w：存在已作废的袋", ErrAlreadyVoid)
		}
		return TruckLot{}, fmt.Errorf("写作废记录失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE b_bag SET status = ? WHERE truck_lot_id = ? AND status <> '作废'`,
		BagStatusVoid, truckLotID); err != nil {
		return TruckLot{}, fmt.Errorf("作废袋码失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE b_truck_lot SET status = ?, bag_count = 0 WHERE id = ?`,
		TruckStatusReturned, truckLotID); err != nil {
		return TruckLot{}, fmt.Errorf("退车失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_truck_lot", EntityID: truckLotID, Action: "return",
		Field: "status", OldValue: t.Status, NewValue: TruckStatusReturned,
		Reason:      voidReason,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return TruckLot{}, err
	}
	if err := tx.Commit(); err != nil {
		return TruckLot{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetTruck(ctx, truckLotID)
}

// ===== 扫码定位（D1 ↔ D2~D4 的分界）=====
//
// ★ 「码本身非法」（codec 的 ErrNotOurs / ErrChecksum）与
//	「格式正确但系统内不存在」（这里的 ErrCodeUnknown）是**两种不同的结果**：
//	前者在码引擎里判，后者必须查库才知道。

// ScanResult 是扫码定位结果。
type ScanResult struct {
	Kind  string    `json:"kind"` // truck / bag
	Code  string    `json:"code"`
	Human string    `json:"human"`
	Truck *TruckLot `json:"truck,omitempty"`
	Bag   *Bag      `json:"bag,omitempty"`
}

// ScanCode 解析码并定位对象。
func (s *Store) ScanCode(ctx context.Context, raw string) (ScanResult, error) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	p, err := codec.Parse(code)
	if err != nil {
		return ScanResult{}, err // ErrNotOurs / ErrChecksum / ErrVersion
	}
	res := ScanResult{Code: code, Human: p.Human}
	switch p.Seg.T {
	case "A":
		t, err := s.GetTruckByCode(ctx, code)
		if err != nil {
			return ScanResult{}, err
		}
		res.Kind = "truck"
		res.Truck = &t
		return res, nil
	case "B":
		b, err := s.GetBagByCode(ctx, code)
		if err != nil {
			return ScanResult{}, err
		}
		res.Kind = "bag"
		res.Bag = &b
		return res, nil
	default:
		return ScanResult{}, ErrCodeUnsupported
	}
}

// GetTruckByCode 按码取车次。
func (s *Store) GetTruckByCode(ctx context.Context, code string) (TruckLot, error) {
	t, err := scanTruck(s.db.QueryRowContext(ctx, truckSelect+` WHERE t.code = ?`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return TruckLot{}, fmt.Errorf("%w：%s", ErrCodeUnknown, code)
	}
	return t, err
}

// GetBagByCode 按码取袋。
func (s *Store) GetBagByCode(ctx context.Context, code string) (Bag, error) {
	b, err := scanBag(s.db.QueryRowContext(ctx, bagSelect+` WHERE code = ?`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return Bag{}, fmt.Errorf("%w：%s", ErrCodeUnknown, code)
	}
	return b, err
}

func fmtFloat(p *float64) string {
	if p == nil {
		return ""
	}
	return milliStr(int64(math.Round(*p * 1000)))
}
