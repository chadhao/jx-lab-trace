package store

// ===== M8 追溯 · 持久化（D4 正向 / D5 反向 / D6 批次档案）=====
//
// ★★ 本文件零写业务表（A17 / §6-13）：只含 SELECT —— 连 s_audit_log 都不写
//	（一期追溯不记访问日志，访问日志属 M9）。
// ★★ 正反互为逆（§1-2 / A9）：两向都以 b_feed_record 为承重墙 ——
//	正向 车次/吨袋 → b_feed_record → 生产批 → 成品批 → 成品袋 → 出货单；
//	反向 成品批 → 来源生产批 → b_feed_record → 吨袋/车次（+ 现行检测结果）。
// ★★ 未命中 ≠ 报错（§6-12 / A10）：正向查无流向 ⇒ HTTP 200 + 空数组 + has_flow:false。
// ★「当时检测结果」= 该车次的**现行检测单**（复用 currentInspectionIDTx，已作废不算）
//	+ 显式标注 is_current=true（§6-9 已知信息缺口，不假装历史快照）；
//	紧急放行判定复用 urgentEffectiveTx（§6-10）；让步判据 = 现行单 conclusion='CONCESSION'
//	（§6-11，与批 6 truckReleasedTx 同一词表）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/codec"
)

// ===== 读模型 =====

// TraceForwardResult 是正向追溯的返回体。
type TraceForwardResult struct {
	Source    TraceSource     `json:"source"`
	Batches   []TraceBatch    `json:"batches"`
	FgLots    []TraceFgLot    `json:"fg_lots"`
	Shipments []TraceShipment `json:"shipments"`
	HasFlow   bool            `json:"has_flow"`
}

// TraceSource 是正向查询的入口对象（车次）。
type TraceSource struct {
	TruckLotID int64  `json:"truck_lot_id,omitempty"`
	TruckCode  string `json:"truck_code,omitempty"`
	CustomerID int64  `json:"customer_id,omitempty"`
	Customer   string `json:"customer,omitempty"`
	MaterialID int64  `json:"material_id,omitempty"`
	Material   string `json:"material,omitempty"`
	ArriveDate string `json:"arrive_date,omitempty"`
	Status     string `json:"status,omitempty"`
}

// TraceBatch 是追溯链上的生产批。
type TraceBatch struct {
	BatchID   int64  `json:"batch_id"`
	Code      string `json:"code"`
	Human     string `json:"human"`
	BatchDate string `json:"batch_date"`
	Status    string `json:"status"`
}

// TraceFgLot 是追溯链上的成品批。
type TraceFgLot struct {
	FgLotID        int64    `json:"fg_lot_id"`
	Code           string   `json:"code"`
	Human          string   `json:"human"`
	OutputMaterial string   `json:"output_material"`
	NetWeight      *float64 `json:"net_weight"`
	Status         string   `json:"status"`
}

// TraceShipment 是追溯链上的出货单。
type TraceShipment struct {
	ShipmentID int64      `json:"shipment_id"`
	ShipmentNo string     `json:"shipment_no"`
	Status     string     `json:"status"`
	ShipAt     *time.Time `json:"ship_at"`
	PlateNo    string     `json:"plate_no"`
	Customer   string     `json:"customer"`
}

// TraceBackwardResult 是反向追溯的返回体。
type TraceBackwardResult struct {
	FgLot   TraceFgLot   `json:"fg_lot"`
	Batches []TraceBatch `json:"batches"`
	Feeds   []TraceFeed  `json:"feeds"`
	HasFlow bool         `json:"has_flow"`
}

// TraceFeed 是反向追溯的一条投料（含车次的现行检测结果）。
type TraceFeed struct {
	BagID      int64            `json:"bag_id"`
	BagCode    string           `json:"bag_code"`
	BagHuman   string           `json:"bag_human"`
	TruckLotID int64            `json:"truck_lot_id"`
	TruckCode  string           `json:"truck_code"`
	Customer   string           `json:"customer"`
	FeedWeight *float64         `json:"feed_weight"`
	FedAt      *time.Time       `json:"fed_at"`
	Inspection *TraceInspection `json:"inspection"`
}

// TraceInspection 是投料吨袋所属车次的检测结果（★ 现行单，非历史快照）。
type TraceInspection struct {
	InspectionID  int64                `json:"inspection_id,omitempty"`
	InspectionNo  string               `json:"inspection_no,omitempty"`
	Conclusion    string               `json:"conclusion"`
	IsCurrent     bool                 `json:"is_current"`
	UrgentRelease bool                 `json:"urgent_release"`
	UrgentRecords []TraceUrgentRecord  `json:"urgent_records,omitempty"`
	Results       []TraceInspResultRow `json:"results"`
}

// TraceInspResultRow 是一条检测结果数值。
type TraceInspResultRow struct {
	ItemName  string   `json:"item_name"`
	State     string   `json:"state"`
	ValueNum  *float64 `json:"value_num"`
	ValueText string   `json:"value_text"`
	Unit      string   `json:"unit"`
	Judge     string   `json:"judge"`
}

// TraceUrgentRecord 是紧急放行的一笔留痕（init / approve）。
type TraceUrgentRecord struct {
	Action      string     `json:"action"`
	ActorOpenID string     `json:"actor_open_id"`
	ActorName   string     `json:"actor_name"`
	Reason      string     `json:"reason"`
	At          *time.Time `json:"at"`
}

// ConcessionSource 是让步接收的来源（车次 + 现行单）。
type ConcessionSource struct {
	TruckLotID   int64  `json:"truck_lot_id"`
	TruckCode    string `json:"truck_code"`
	InspectionNo string `json:"inspection_no"`
	Conclusion   string `json:"conclusion"`
}

// BatchArchive 是 D6 批次档案的一页汇总（内部档案，让步显著标注）。
type BatchArchive struct {
	Batch             TraceBatch         `json:"batch"`
	CustomerName      string             `json:"customer_name"`
	InputMaterial     string             `json:"input_material"`
	PlannedOutput     string             `json:"planned_output_material"`
	Remark            string             `json:"remark"`
	Feeds             []FeedRecord       `json:"feeds"`
	Operations        []BatchOperation   `json:"operations"`
	FgLots            []ArchiveFgLot     `json:"fg_lots"`
	Shipments         []TraceShipment    `json:"shipments"`
	ConcessionUsed    bool               `json:"concession_used"`
	ConcessionSources []ConcessionSource `json:"concession_sources"`
}

// ArchiveFgLot 是档案里的成品批（含成品袋）。
type ArchiveFgLot struct {
	FgLot FgLot   `json:"fg_lot"`
	Bags  []FgBag `json:"bags"`
}

// ===== 码解析 =====

// normalizeTraceBagCode 解析正向查询的吨袋码（T='B'）。
func normalizeTraceBagCode(raw string) (codec.Parsed, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return codec.Parsed{}, fmt.Errorf("%w：bag_code 不能为空", ErrProdBadInput)
	}
	full := strings.ToUpper(s)
	if strings.Contains(s, "-") {
		f, err := codec.FromHuman(s)
		if err != nil {
			return codec.Parsed{}, err
		}
		full = f
	}
	p, err := codec.Parse(full)
	if err != nil {
		return codec.Parsed{}, err
	}
	if p.Seg.T != "B" {
		return codec.Parsed{}, fmt.Errorf("%w：正向追溯扫吨袋码（B），实际 %s", ErrProdBadInput, p.Seg.T)
	}
	return p, nil
}

// normalizeTraceFgCode 解析反向查询的成品批码（T='D'）。
func normalizeTraceFgCode(raw string) (codec.Parsed, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return codec.Parsed{}, fmt.Errorf("%w：fg_code 不能为空", ErrProdBadInput)
	}
	full := strings.ToUpper(s)
	if strings.Contains(s, "-") {
		f, err := codec.FromHuman(s)
		if err != nil {
			return codec.Parsed{}, err
		}
		full = f
	}
	p, err := codec.Parse(full)
	if err != nil {
		return codec.Parsed{}, err
	}
	if p.Seg.T != "D" {
		return codec.Parsed{}, fmt.Errorf("%w：反向追溯扫成品批码（D），实际 %s", ErrProdBadInput, p.Seg.T)
	}
	return p, nil
}

// ===== D4 · 正向追溯 =====

// TraceForward 正向追溯：车次 / 吨袋 → 生产批 → 成品批 → 出货单。
//
// ★ 链路首跳必须走 b_feed_record（A8）；未被投料 ⇒ 200 + 空 + has_flow:false（A10）。
func (s *Store) TraceForward(ctx context.Context, truckLotID int64, bagCode string) (TraceForwardResult, error) {
	out := TraceForwardResult{
		Batches: []TraceBatch{}, FgLots: []TraceFgLot{}, Shipments: []TraceShipment{},
	}

	// 入口解析：bag_code 优先（未命中 ≠ 报错 ⇒ 不 404，返回无流向）
	bagID := int64(0)
	truckID := truckLotID
	if strings.TrimSpace(bagCode) != "" {
		p, err := normalizeTraceBagCode(bagCode)
		if err != nil {
			return out, err
		}
		if err := s.db.QueryRowContext(ctx,
			`SELECT id, truck_lot_id FROM b_bag WHERE code = ?`, p.Full).
			Scan(&bagID, &truckID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return out, nil // 袋未命中 ⇒ 无流向（§6-12）
			}
			return out, fmt.Errorf("读取吨袋失败: %w", err)
		}
	}
	if truckID <= 0 && bagID <= 0 {
		return out, nil
	}

	// 车次信息（未命中 ⇒ source 空、has_flow 仍由投料决定）
	var source TraceSource
	{
		var cust, mat sql.NullString
		var arrive sql.NullTime
		truckRowID := truckID
		err := s.db.QueryRowContext(ctx, `
SELECT t.id, t.code, t.customer_id, COALESCE(c.name, ''),
       t.material_id, COALESCE(m.name, ''), t.arrive_date, t.status
  FROM b_truck_lot t
  LEFT JOIN m_customer c ON c.id = t.customer_id
  LEFT JOIN m_material m ON m.id = t.material_id
 WHERE t.id = ?`, truckRowID).
			Scan(&source.TruckLotID, &source.TruckCode, &source.CustomerID, &cust,
				&source.MaterialID, &mat, &arrive, &source.Status)
		if err == nil {
			source.Customer = cust.String
			source.Material = mat.String
			if arrive.Valid {
				source.ArriveDate = arrive.Time.Format("2006-01-02")
			}
			out.Source = source
		} else if !errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("读取车次失败: %w", err)
		}
	}

	// ★★ 第 1 跳：b_feed_record（正反共用的承重墙）
	var batchRows []TraceBatch
	if bagID > 0 {
		rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT b.id, b.code, b.batch_date, b.status
  FROM b_feed_record f
  JOIN b_production_batch b ON b.id = f.batch_id
 WHERE f.bag_id = ?
 ORDER BY b.id`, bagID)
		if err != nil {
			return out, fmt.Errorf("正向追溯查询失败: %w", err)
		}
		batchRows, err = collectTraceBatches(rows)
		if err != nil {
			return out, err
		}
	} else {
		rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT b.id, b.code, b.batch_date, b.status
  FROM b_feed_record f
  JOIN b_bag g ON g.id = f.bag_id
  JOIN b_production_batch b ON b.id = f.batch_id
 WHERE g.truck_lot_id = ?
 ORDER BY b.id`, truckID)
		if err != nil {
			return out, fmt.Errorf("正向追溯查询失败: %w", err)
		}
		batchRows, err = collectTraceBatches(rows)
		if err != nil {
			return out, err
		}
	}
	out.Batches = batchRows
	if len(batchRows) == 0 {
		return out, nil // 无流向：200 + 空 + has_flow:false
	}

	// 成品批（经 b_fg_lot.batch_id）
	batchIDs := make([]interface{}, 0, len(batchRows))
	ph := make([]string, 0, len(batchRows))
	for _, b := range batchRows {
		batchIDs = append(batchIDs, b.BatchID)
		ph = append(ph, "?")
	}
	lotRows, err := s.db.QueryContext(ctx, `
SELECT l.id, l.code, COALESCE(m.name, ''), l.net_weight, l.status
  FROM b_fg_lot l
  LEFT JOIN m_material m ON m.id = l.output_material_id
 WHERE l.batch_id IN (`+strings.Join(ph, ",")+`)
 ORDER BY l.id`, batchIDs...)
	if err != nil {
		return out, fmt.Errorf("查询成品批失败: %w", err)
	}
	for lotRows.Next() {
		var f TraceFgLot
		var nw sql.NullFloat64
		if err := lotRows.Scan(&f.FgLotID, &f.Code, &f.OutputMaterial, &nw, &f.Status); err != nil {
			lotRows.Close()
			return out, fmt.Errorf("读取成品批失败: %w", err)
		}
		f.Human, _ = codec.ToHuman(f.Code)
		if nw.Valid {
			w := nw.Float64
			f.NetWeight = &w
		}
		out.FgLots = append(out.FgLots, f)
	}
	if err := lotRows.Err(); err != nil {
		lotRows.Close()
		return out, err
	}
	lotRows.Close()
	if len(out.FgLots) == 0 {
		out.HasFlow = true // 有投料但尚无成品产出
		return out, nil
	}

	// 出货单（成品袋 → 明细 → 出货单；含已撤销，状态如实呈现）
	lotIDs := make([]interface{}, 0, len(out.FgLots))
	lotPh := make([]string, 0, len(out.FgLots))
	for _, f := range out.FgLots {
		lotIDs = append(lotIDs, f.FgLotID)
		lotPh = append(lotPh, "?")
	}
	shipRows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT s.id, s.shipment_no, s.status, s.ship_at,
       COALESCE(s.plate_no, ''), COALESCE(c.name, '')
  FROM b_fg_bag b
  JOIN b_shipment_item i ON i.fg_bag_id = b.id
  JOIN b_shipment s ON s.id = i.shipment_id
  LEFT JOIN m_customer c ON c.id = s.customer_id
 WHERE b.fg_lot_id IN (`+strings.Join(lotPh, ",")+`)
 ORDER BY s.id`, lotIDs...)
	if err != nil {
		return out, fmt.Errorf("查询出货单失败: %w", err)
	}
	defer shipRows.Close()
	for shipRows.Next() {
		var sh TraceShipment
		var shipAt sql.NullTime
		if err := shipRows.Scan(&sh.ShipmentID, &sh.ShipmentNo, &sh.Status, &shipAt,
			&sh.PlateNo, &sh.Customer); err != nil {
			return out, fmt.Errorf("读取出货单失败: %w", err)
		}
		if shipAt.Valid {
			t := shipAt.Time
			sh.ShipAt = &t
		}
		out.Shipments = append(out.Shipments, sh)
	}
	if err := shipRows.Err(); err != nil {
		return out, err
	}

	out.HasFlow = true
	return out, nil
}

func collectTraceBatches(rows *sql.Rows) ([]TraceBatch, error) {
	defer rows.Close()
	out := []TraceBatch{}
	for rows.Next() {
		var b TraceBatch
		var bd sql.NullTime
		// DATE 列按 []byte/时间两种驱动形态兼容扫描
		var rawBD interface{}
		if err := rows.Scan(&b.BatchID, &b.Code, &rawBD, &b.Status); err != nil {
			return nil, fmt.Errorf("读取生产批失败: %w", err)
		}
		switch v := rawBD.(type) {
		case []byte:
			b.BatchDate = string(v)
		case string:
			b.BatchDate = v
		case time.Time:
			b.BatchDate = v.Format("2006-01-02")
		default:
			_ = bd
		}
		b.Human, _ = codec.ToHuman(b.Code)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ===== D5 · 反向追溯 =====

// TraceBackward 反向追溯：成品批 → 来源生产批 → 投料吨袋 / 车次（含现行检测结果）。
//
// ★ 只读事务（不写任何表）；检测结果 = 现行单 + is_current=true 标注；
//
//	紧急放行判定复用 urgentEffectiveTx（不另写一套）。
func (s *Store) TraceBackward(ctx context.Context, fgLotID int64, fgCode string) (TraceBackwardResult, error) {
	out := TraceBackwardResult{Batches: []TraceBatch{}, Feeds: []TraceFeed{}}

	// 解析成品批
	var lot struct {
		ID     int64
		Code   string
		Status string
	}
	if strings.TrimSpace(fgCode) != "" {
		p, err := normalizeTraceFgCode(fgCode)
		if err != nil {
			return out, err
		}
		err = s.db.QueryRowContext(ctx,
			`SELECT id, code, status FROM b_fg_lot WHERE code = ?`, p.Full).
			Scan(&lot.ID, &lot.Code, &lot.Status)
		if errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("%w：成品批 %s", ErrProdNotFound, p.Full)
		}
		if err != nil {
			return out, fmt.Errorf("读取成品批失败: %w", err)
		}
	} else if fgLotID > 0 {
		err := s.db.QueryRowContext(ctx,
			`SELECT id, code, status FROM b_fg_lot WHERE id = ?`, fgLotID).
			Scan(&lot.ID, &lot.Code, &lot.Status)
		if errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("%w：成品批 #%d", ErrProdNotFound, fgLotID)
		}
		if err != nil {
			return out, fmt.Errorf("读取成品批失败: %w", err)
		}
	} else {
		return out, fmt.Errorf("%w：fg_lot_id / fg_code 至少一个", ErrProdBadInput)
	}

	// 来源生产批
	var batch TraceBatch
	var rawBD interface{}
	err := s.db.QueryRowContext(ctx, `
SELECT b.id, b.code, b.batch_date, b.status
  FROM b_production_batch b
  JOIN b_fg_lot l ON l.batch_id = b.id
 WHERE l.id = ?`, lot.ID).Scan(&batch.BatchID, &batch.Code, &rawBD, &batch.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("%w：成品批 #%d 的来源生产批", ErrProdNotFound, lot.ID)
	}
	if err != nil {
		return out, fmt.Errorf("读取生产批失败: %w", err)
	}
	switch v := rawBD.(type) {
	case []byte:
		batch.BatchDate = string(v)
	case string:
		batch.BatchDate = v
	case time.Time:
		batch.BatchDate = v.Format("2006-01-02")
	}
	batch.Human, _ = codec.ToHuman(batch.Code)
	out.Batches = append(out.Batches, batch)
	out.FgLot = TraceFgLot{FgLotID: lot.ID, Code: lot.Code, Status: lot.Status}
	out.FgLot.Human, _ = codec.ToHuman(lot.Code)

	// 只读事务：查询投料 + 每车次现行检测 + 紧急放行判定（复用 M5/M6 实现）
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, fmt.Errorf("开启只读事务失败: %w", err)
	}
	defer tx.Rollback()

	// ★★ 反向首跳与正向同一张表：b_feed_record.batch_id（A9 互为逆）
	feeds, err := s.backwardFeeds(ctx, tx, batch.BatchID)
	if err != nil {
		return out, err
	}
	out.Feeds = feeds
	out.HasFlow = len(feeds) > 0
	return out, nil
}

func (s *Store) backwardFeeds(ctx context.Context, tx *sql.Tx, batchID int64) ([]TraceFeed, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT g.id, g.code, g.truck_lot_id, COALESCE(t.code, ''),
       COALESCE(c.name, ''), f.feed_weight, f.fed_at
  FROM b_feed_record f
  JOIN b_bag g ON g.id = f.bag_id
  LEFT JOIN b_truck_lot t ON t.id = g.truck_lot_id
  LEFT JOIN m_customer c ON c.id = t.customer_id
 WHERE f.batch_id = ?
 ORDER BY f.id`, batchID)
	if err != nil {
		return nil, fmt.Errorf("反向追溯查询失败: %w", err)
	}
	// ★ 先把投料行扫进内存再关闭 rows —— MySQL driver 在同一 Tx 上
	//	不允许 rows 未读完时嵌套发新查询（实测 busy buffer / bad connection）。
	base := []TraceFeed{}
	for rows.Next() {
		var f TraceFeed
		var nw sql.NullFloat64
		var fedAt sql.NullTime
		if err := rows.Scan(&f.BagID, &f.BagCode, &f.TruckLotID, &f.TruckCode,
			&f.Customer, &nw, &fedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("读取投料失败: %w", err)
		}
		f.BagHuman, _ = codec.ToHuman(f.BagCode)
		if nw.Valid {
			w := nw.Float64
			f.FeedWeight = &w
		}
		if fedAt.Valid {
			t := fedAt.Time
			f.FedAt = &t
		}
		base = append(base, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// 第二阶段：rows 已关闭，再逐车次查现行检测单 + 紧急放行（复用 M5/M6 实现）
	out := make([]TraceFeed, 0, len(base))
	for _, f := range base {
		insp, err := traceInspectionTx(ctx, tx, f.TruckLotID)
		if err != nil {
			return nil, err
		}
		f.Inspection = insp
		out = append(out, f)
	}
	return out, nil
}

// traceInspectionTx 读该车次的现行检测单（含结果数值）+ 生效紧急放行留痕。
// ★ 现行单判定复用 currentInspectionIDTx；生效判定复用 urgentEffectiveTx（§6-10）。
func traceInspectionTx(ctx context.Context, tx *sql.Tx, truckLotID int64) (*TraceInspection, error) {
	insp := &TraceInspection{Results: []TraceInspResultRow{}}

	cur, err := currentInspectionIDTx(ctx, tx, TargetTruck, truckLotID)
	if err != nil {
		return nil, err
	}
	if cur.Valid {
		var conclusion sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT inspection_no, conclusion FROM b_inspection WHERE id = ?`, cur.Int64).
			Scan(&insp.InspectionNo, &conclusion)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("读取现行检测单失败: %w", err)
		}
		if conclusion.Valid {
			insp.Conclusion = conclusion.String
		}
		insp.InspectionID = cur.Int64
		insp.IsCurrent = true // ★ 如实标注：现行单，非历史快照（§6-9）

		rRows, err := tx.QueryContext(ctx, `
SELECT COALESCE(t.name, ''), r.state, r.value_num, COALESCE(r.value_text, ''),
       COALESCE(r.unit, ''), COALESCE(r.judge, '')
  FROM b_inspection_result r
  LEFT JOIN m_test_item t ON t.id = r.item_id
 WHERE r.inspection_id = ?
 ORDER BY r.id`, cur.Int64)
		if err != nil {
			return nil, fmt.Errorf("读取检测结果失败: %w", err)
		}
		for rRows.Next() {
			var row TraceInspResultRow
			var vn sql.NullFloat64
			if err := rRows.Scan(&row.ItemName, &row.State, &vn, &row.ValueText,
				&row.Unit, &row.Judge); err != nil {
				rRows.Close()
				return nil, fmt.Errorf("读取检测结果失败: %w", err)
			}
			if vn.Valid {
				v := vn.Float64
				row.ValueNum = &v
			}
			insp.Results = append(insp.Results, row)
		}
		if err := rRows.Err(); err != nil {
			rRows.Close()
			return nil, err
		}
		rRows.Close()
	}

	// 紧急放行：复用 M6 urgentEffectiveTx（init + approve 两笔齐且两人不同才生效）
	urgent, err := urgentEffectiveTx(ctx, tx, "b_truck_lot", truckLotID)
	if err != nil {
		return nil, err
	}
	if urgent {
		insp.UrgentRelease = true
		uRows, err := tx.QueryContext(ctx, `
SELECT action, COALESCE(actor_open_id, ''), COALESCE(actor_name, ''),
       COALESCE(reason, ''), at
  FROM s_audit_log
 WHERE entity = 'b_truck_lot' AND entity_id = ?
   AND action IN ('urgent_release_init', 'urgent_release_approve')
 ORDER BY id ASC`, truckLotID)
		if err != nil {
			return nil, fmt.Errorf("读取紧急放行留痕失败: %w", err)
		}
		for uRows.Next() {
			var r TraceUrgentRecord
			var at sql.NullTime
			if err := uRows.Scan(&r.Action, &r.ActorOpenID, &r.ActorName, &r.Reason, &at); err != nil {
				uRows.Close()
				return nil, fmt.Errorf("读取紧急放行留痕失败: %w", err)
			}
			if at.Valid {
				t := at.Time
				r.At = &t
			}
			insp.UrgentRecords = append(insp.UrgentRecords, r)
		}
		if err := uRows.Err(); err != nil {
			uRows.Close()
			return nil, err
		}
		uRows.Close()
	}
	return insp, nil
}

// ===== D6 · 批次档案 =====

// GetBatchArchive 一页汇总生产批全链（内部档案；让步接收显著标注 D20/UC-M8-03）。
//
// ★ 让步判据 = 投料链上任一车次的**现行检测单** conclusion='CONCESSION'（§6-11）；
//
//	全函数只读（A17）。
func (s *Store) GetBatchArchive(ctx context.Context, batchID int64) (BatchArchive, error) {
	out := BatchArchive{
		Feeds: []FeedRecord{}, Operations: []BatchOperation{},
		FgLots: []ArchiveFgLot{}, Shipments: []TraceShipment{},
		ConcessionSources: []ConcessionSource{},
	}

	var rawBD interface{}
	var custName, inMat, outMat, remark sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT b.id, b.code, b.batch_date, b.status,
       COALESCE(c.name, ''), COALESCE(mi.name, ''), COALESCE(mo.name, ''),
       COALESCE(b.remark, '')
  FROM b_production_batch b
  LEFT JOIN m_customer c ON c.id = b.customer_id
  LEFT JOIN m_material mi ON mi.id = b.input_material_id
  LEFT JOIN m_material mo ON mo.id = b.planned_output_material_id
 WHERE b.id = ?`, batchID).
		Scan(&out.Batch.BatchID, &out.Batch.Code, &rawBD, &out.Batch.Status,
			&custName, &inMat, &outMat, &remark)
	if errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("%w：生产批 #%d", ErrProdNotFound, batchID)
	}
	if err != nil {
		return out, fmt.Errorf("读取生产批失败: %w", err)
	}
	switch v := rawBD.(type) {
	case []byte:
		out.Batch.BatchDate = string(v)
	case string:
		out.Batch.BatchDate = v
	case time.Time:
		out.Batch.BatchDate = v.Format("2006-01-02")
	}
	out.Batch.Human, _ = codec.ToHuman(out.Batch.Code)
	out.CustomerName = custName.String
	out.InputMaterial = inMat.String
	out.PlannedOutput = outMat.String
	out.Remark = remark.String

	// 投料明细 / 作业段（复用 M6 读实现）
	feeds, err := s.ListFeeds(ctx, batchID)
	if err != nil {
		return out, err
	}
	out.Feeds = feeds
	ops, err := s.ListOperations(ctx, batchID)
	if err != nil {
		return out, err
	}
	out.Operations = ops

	// 成品批与成品袋（复用 M6 读实现）
	lots, err := s.ListFgLots(ctx, batchID)
	if err != nil {
		return out, err
	}
	lotIDs := make([]interface{}, 0, len(lots))
	ph := make([]string, 0, len(lots))
	for _, l := range lots {
		bags, err := s.ListFgBags(ctx, l.ID)
		if err != nil {
			return out, err
		}
		out.FgLots = append(out.FgLots, ArchiveFgLot{FgLot: l, Bags: bags})
		lotIDs = append(lotIDs, l.ID)
		ph = append(ph, "?")
	}

	// 出货单（经成品袋明细；含已撤销状态如实呈现）
	if len(lotIDs) > 0 {
		shipRows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT s.id, s.shipment_no, s.status, s.ship_at,
       COALESCE(s.plate_no, ''), COALESCE(c.name, '')
  FROM b_fg_bag b
  JOIN b_shipment_item i ON i.fg_bag_id = b.id
  JOIN b_shipment s ON s.id = i.shipment_id
  LEFT JOIN m_customer c ON c.id = s.customer_id
 WHERE b.fg_lot_id IN (`+strings.Join(ph, ",")+`)
 ORDER BY s.id`, lotIDs...)
		if err != nil {
			return out, fmt.Errorf("查询出货单失败: %w", err)
		}
		for shipRows.Next() {
			var sh TraceShipment
			var shipAt sql.NullTime
			if err := shipRows.Scan(&sh.ShipmentID, &sh.ShipmentNo, &sh.Status, &shipAt,
				&sh.PlateNo, &sh.Customer); err != nil {
				shipRows.Close()
				return out, fmt.Errorf("读取出货单失败: %w", err)
			}
			if shipAt.Valid {
				t := shipAt.Time
				sh.ShipAt = &t
			}
			out.Shipments = append(out.Shipments, sh)
		}
		if err := shipRows.Err(); err != nil {
			shipRows.Close()
			return out, err
		}
		shipRows.Close()
	}

	// ★ 让步接收标注（判据 = 车次现行单 conclusion='CONCESSION'）
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, fmt.Errorf("开启只读事务失败: %w", err)
	}
	defer tx.Rollback()

	seenTruck := map[int64]bool{}
	for _, f := range feeds {
		if f.TruckID == 0 || seenTruck[f.TruckID] {
			continue
		}
		seenTruck[f.TruckID] = true
		cur, err := currentInspectionIDTx(ctx, tx, TargetTruck, f.TruckID)
		if err != nil {
			return out, err
		}
		if !cur.Valid {
			continue
		}
		var conclusion, inspNo string
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(conclusion, ''), inspection_no FROM b_inspection WHERE id = ?`,
			cur.Int64).Scan(&conclusion, &inspNo); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("读取现行检测单失败: %w", err)
		}
		if conclusion == InspConclusionCons {
			out.ConcessionUsed = true
			out.ConcessionSources = append(out.ConcessionSources, ConcessionSource{
				TruckLotID: f.TruckID, TruckCode: f.TruckCode,
				InspectionNo: inspNo, Conclusion: conclusion,
			})
		}
	}
	return out, nil
}
