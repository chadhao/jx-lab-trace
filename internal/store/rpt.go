package store

// ===== M10 报表 · 只读持久化（D5）=====
//
// ★★ 本文件**零写业务表**（A13 / TC-M10-01 结构化机检的扫描对象之一）：
//	只允许 SELECT；唯一合法的写是 s_audit_log，且它在 audit.go 落（不在本文件）。
// ★★ 空数据 ≠ 0（A15 / TC-M10-03）：
//	· probe（数据源计数）= 0 ⇒ has_data=false + note「数据未接入」；
//	· probe > 0 但筛选无命中 ⇒ has_data=true + rows:[] + note「本条件下无记录」；
//	★ 两态文案必须不同，禁止用 0 / 空数组冒充「结果为 0」。
// ★★ probe 口径（我方定案，回执已说明）：**维度筛选（customer_id）计入 probe，
//	时间窗口（from/to）不计入** —— 「数据源整体无数据」按维度内整体计，
//	时间窗口只决定本页有没有记录。sample-expiry 无客户维度 ⇒ probe 为全源计数。
// ★ P4：报表层与业务层解耦 —— 独立文件、只读、可单独替换。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/codec"
)

// M10 的哨兵错误（httpapi 据此映射状态码）。
var (
	ErrRptBadInput = errors.New("报表参数不合法")
	ErrRptUnknown  = errors.New("报表不存在")
)

// 空数据两态文案（★ 必须不同，A15）。
const (
	RptNoteNoData  = "数据未接入"
	RptNoteNoMatch = "本条件下无记录"
)

// 一期 5 张报表名（§5 / D5，逐字）。
const (
	RptQualityTrend  = "quality-trend"
	RptCustomerRecon = "customer-recon"
	RptOutputYield   = "output-yield"
	RptSampleExpiry  = "sample-expiry"
	RptNonconform    = "nonconform-stat"
)

// RptColumn 是一列的展示定义（导出 CSV 用 Label）。
type RptColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// RptFilters 是 5 张报表的通用筛选（非法值 ⇒ 400）。
type RptFilters struct {
	From       string `json:"from"`
	To         string `json:"to"`
	CustomerID int64  `json:"customer_id"`
	Bucket     string `json:"bucket"` // quality-trend：month / week，缺省 month
}

// RptResult 是报表返回体（★ 固定含 has_data 与 note）。
type RptResult struct {
	Name    string                   `json:"name"`
	Title   string                   `json:"title"`
	Columns []RptColumn              `json:"columns"`
	Rows    []map[string]interface{} `json:"rows"`
	HasData bool                     `json:"has_data"`
	Note    string                   `json:"note,omitempty"`
}

// ValidateRptFilters 校验通用筛选（from/to 须为 YYYY-MM-DD 或空；customer_id 非负）。
func ValidateRptFilters(f RptFilters) error {
	if f.CustomerID < 0 {
		return fmt.Errorf("%w：customer_id 须为非负整数", ErrRptBadInput)
	}
	for _, v := range []struct{ name, val string }{{"from", f.From}, {"to", f.To}} {
		v := v
		val := strings.TrimSpace(v.val)
		if val == "" {
			continue
		}
		if _, err := time.ParseInLocation("2006-01-02", val, time.Local); err != nil {
			return fmt.Errorf("%w：%s 须为 YYYY-MM-DD，实际 %q", ErrRptBadInput, v.name, v.val)
		}
	}
	if f.Bucket != "" && f.Bucket != "month" && f.Bucket != "week" {
		return fmt.Errorf("%w：bucket 只接受 month / week，实际 %q", ErrRptBadInput, f.Bucket)
	}
	return nil
}

// dateClause 返回时间窗口条件与参数（列名由调用方给，★ 白名单式拼接，不拼用户输入）。
func dateClause(col, from, to string) (string, []interface{}) {
	var b strings.Builder
	var args []interface{}
	if v := strings.TrimSpace(from); v != "" {
		b.WriteString(" AND DATE(" + col + ") >= ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(to); v != "" {
		b.WriteString(" AND DATE(" + col + ") <= ?")
		args = append(args, v)
	}
	return b.String(), args
}

// custClause 三路目标（车次 / 生产批 / 成品批）的客户归属条件。
const custTargetExpr = `COALESCE(tl.customer_id, pb.customer_id, fl.customer_id)`

const custJoins = `
  LEFT JOIN b_truck_lot tl ON i.target_type = '车次' AND tl.id = i.target_id
  LEFT JOIN b_production_batch pb ON i.target_type = '生产批' AND pb.id = i.target_id
  LEFT JOIN b_fg_lot fl ON i.target_type = '成品批' AND fl.id = i.target_id`

const notVoidInsp = ` AND NOT EXISTS (
    SELECT 1 FROM b_obj_void v WHERE v.entity = 'b_inspection' AND v.entity_id = i.id)`

// RunRpt 执行一张报表（只读；未知名 ⇒ 404）。
func (s *Store) RunRpt(ctx context.Context, name string, f RptFilters) (RptResult, error) {
	if err := ValidateRptFilters(f); err != nil {
		return RptResult{}, err
	}
	switch name {
	case RptQualityTrend:
		return s.rptQualityTrend(ctx, f)
	case RptCustomerRecon:
		return s.rptCustomerRecon(ctx, f)
	case RptOutputYield:
		return s.rptOutputYield(ctx, f)
	case RptSampleExpiry:
		return s.rptSampleExpiry(ctx, f)
	case RptNonconform:
		return s.rptNonconformStat(ctx, f)
	default:
		return RptResult{}, fmt.Errorf("%w：%s", ErrRptUnknown, name)
	}
}

// finish 统一组装 has_data / note 两态（★ 文案必须不同）。
func finish(name, title string, cols []RptColumn, rows []map[string]interface{}, sourceCount int) RptResult {
	res := RptResult{Name: name, Title: title, Columns: cols, Rows: rows}
	if sourceCount == 0 {
		res.HasData = false
		res.Note = RptNoteNoData
		res.Rows = []map[string]interface{}{}
		return res
	}
	res.HasData = true
	if len(res.Rows) == 0 {
		res.Note = RptNoteNoMatch
	}
	return res
}

func scanCount(ctx context.Context, q queryRowerOne, query string, args ...interface{}) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

type queryRowerOne interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// ===== 1. quality-trend 质量趋势 =====

func (s *Store) rptQualityTrend(ctx context.Context, f RptFilters) (RptResult, error) {
	bucket := f.Bucket
	if bucket == "" {
		bucket = "month"
	}
	period := `COALESCE(i.test_date, DATE(i.created_at))`
	periodFmt := `DATE_FORMAT(` + period + `, '%Y-%m')`
	if bucket == "week" {
		periodFmt = `DATE_FORMAT(` + period + `, '%x-W%v')`
	}

	where := ` WHERE i.conclusion IS NOT NULL` + notVoidInsp
	var args []interface{}
	if f.CustomerID > 0 {
		where += ` AND ` + custTargetExpr + ` = ?`
		args = append(args, f.CustomerID)
	}
	d, dargs := dateClause(period, f.From, f.To)
	where += d
	args = append(args, dargs...)

	// probe：数据源整体（维度内）计数 —— 时间窗口不参与
	probe, err := scanCount(ctx, s.db, `
SELECT COUNT(*) FROM b_inspection i`+custJoins+where, args...)
	if err != nil {
		return RptResult{}, fmt.Errorf("统计质量趋势数据源失败: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT `+periodFmt+` AS bucket,
       COUNT(*) AS total,
       COALESCE(SUM(i.conclusion = '合格'), 0) AS qualified,
       COALESCE(SUM(i.conclusion = 'CONCESSION'), 0) AS concession,
       COALESCE(SUM(i.conclusion = '不合格'), 0) AS unqualified
  FROM b_inspection i`+custJoins+where+`
 GROUP BY bucket ORDER BY bucket`, args...)
	if err != nil {
		return RptResult{}, fmt.Errorf("查询质量趋势失败: %w", err)
	}
	defer rows.Close()

	cols := []RptColumn{
		{Key: "bucket", Label: "时间桶"},
		{Key: "total", Label: "已出结论单数"},
		{Key: "qualified", Label: "合格"},
		{Key: "concession", Label: "让步"}, // ★ 仅内部报表列名；对外报告页不使用
		{Key: "unqualified", Label: "不合格"},
		{Key: "rate", Label: "合格率"},
	}
	out := []map[string]interface{}{}
	for rows.Next() {
		var b string
		var total, qualified, concession, unqualified int
		if err := rows.Scan(&b, &total, &qualified, &concession, &unqualified); err != nil {
			return RptResult{}, fmt.Errorf("读取质量趋势失败: %w", err)
		}
		rate := interface{}(nil)
		if total > 0 {
			rate = fmt.Sprintf("%.1f%%", float64(qualified)/float64(total)*100)
		}
		out = append(out, map[string]interface{}{
			"bucket": b, "total": total, "qualified": qualified,
			"concession": concession, "unqualified": unqualified, "rate": rate,
		})
	}
	if err := rows.Err(); err != nil {
		return RptResult{}, err
	}
	return finish(RptQualityTrend, "质量趋势", cols, out, probe), nil
}

// ===== 2. customer-recon 客户对账 =====

func (s *Store) rptCustomerRecon(ctx context.Context, f RptFilters) (RptResult, error) {
	where := ` WHERE s.ship_at IS NOT NULL AND s.status <> '已撤销'`
	var args []interface{}
	if f.CustomerID > 0 {
		where += ` AND s.customer_id = ?`
		args = append(args, f.CustomerID)
	}
	d, dargs := dateClause("s.ship_at", f.From, f.To)
	where += d
	args = append(args, dargs...)

	probe, err := scanCount(ctx, s.db,
		`SELECT COUNT(DISTINCT s.id) FROM b_shipment s`+where, args...)
	if err != nil {
		return RptResult{}, fmt.Errorf("统计数据源失败: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT s.customer_id, COALESCE(c.name, '') AS cname,
       COUNT(DISTINCT s.id) AS orders,
       COALESCE(SUM(COALESCE(b.weight_allocated, 0)), 0) AS tons
  FROM b_shipment s
  JOIN m_customer c ON c.id = s.customer_id
  JOIN b_shipment_item i ON i.shipment_id = s.id
  JOIN b_fg_bag b ON b.id = i.fg_bag_id`+where+`
 GROUP BY s.customer_id, c.name ORDER BY tons DESC, s.customer_id`, args...)
	if err != nil {
		return RptResult{}, fmt.Errorf("查询客户对账失败: %w", err)
	}
	defer rows.Close()

	cols := []RptColumn{
		{Key: "customer", Label: "客户"},
		{Key: "orders", Label: "出货单数"},
		{Key: "tons", Label: "出货吨位"},
	}
	out := []map[string]interface{}{}
	for rows.Next() {
		var id int64
		var name string
		var orders int
		var tons float64
		if err := rows.Scan(&id, &name, &orders, &tons); err != nil {
			return RptResult{}, fmt.Errorf("读取客户对账失败: %w", err)
		}
		out = append(out, map[string]interface{}{
			"customer_id": id, "customer": name, "orders": orders, "tons": tons,
		})
	}
	if err := rows.Err(); err != nil {
		return RptResult{}, err
	}
	return finish(RptCustomerRecon, "客户对账", cols, out, probe), nil
}

// ===== 3. output-yield 产量与合格率 =====

func (s *Store) rptOutputYield(ctx context.Context, f RptFilters) (RptResult, error) {
	where := ` WHERE 1 = 1`
	var args []interface{}
	if f.CustomerID > 0 {
		where += ` AND b.customer_id = ?`
		args = append(args, f.CustomerID)
	}
	d, dargs := dateClause("b.batch_date", f.From, f.To)
	where += d
	args = append(args, dargs...)

	probe, err := scanCount(ctx, s.db,
		`SELECT COUNT(*) FROM b_production_batch b`+where, args...)
	if err != nil {
		return RptResult{}, fmt.Errorf("统计数据源失败: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT b.id, b.code, b.batch_date,
       COALESCE(mi.name, ''), COALESCE(mo.name, ''),
       COALESCE((SELECT SUM(f.feed_weight) FROM b_feed_record f WHERE f.batch_id = b.id), 0),
       COALESCE((SELECT SUM(l.net_weight) FROM b_fg_lot l WHERE l.batch_id = b.id), 0)
  FROM b_production_batch b
  LEFT JOIN m_material mi ON mi.id = b.input_material_id
  LEFT JOIN m_material mo ON mo.id = b.planned_output_material_id`+where+`
 ORDER BY b.id`, args...)
	if err != nil {
		return RptResult{}, fmt.Errorf("查询产量与合格率失败: %w", err)
	}
	defer rows.Close()

	cols := []RptColumn{
		{Key: "batch", Label: "生产批"},
		{Key: "batch_date", Label: "生产日期"},
		{Key: "input_material", Label: "投入物料"},
		{Key: "output_material", Label: "产出物料"},
		{Key: "input_qty", Label: "投入量（吨）"},
		{Key: "output_qty", Label: "产出量（吨）"},
		{Key: "yield", Label: "产出率"},
	}
	out := []map[string]interface{}{}
	for rows.Next() {
		var id int64
		var code, batchDate, inMat, outMat string
		var inputQty, outputQty float64
		if err := rows.Scan(&id, &code, &batchDate, &inMat, &outMat, &inputQty, &outputQty); err != nil {
			return RptResult{}, fmt.Errorf("读取产量与合格率失败: %w", err)
		}
		human, _ := codec.ToHuman(code)
		// ★ 投入量 = 0 ⇒ 产出率 null（不得 0 或 Infinity）
		var yield interface{}
		if inputQty != 0 {
			yield = fmt.Sprintf("%.1f%%", outputQty/inputQty*100)
		}
		out = append(out, map[string]interface{}{
			"batch_id": id, "batch": human, "batch_date": batchDate,
			"input_material": inMat, "output_material": outMat,
			"input_qty": inputQty, "output_qty": outputQty, "yield": yield,
		})
	}
	if err := rows.Err(); err != nil {
		return RptResult{}, err
	}
	return finish(RptOutputYield, "产量与合格率", cols, out, probe), nil
}

// ===== 4. sample-expiry 留样到期 =====
//
// ★ 任务包写「按 expires_at 升序」，冻结 schema 的实际列名是
//	spec/schema.sql#b_sample_retention.retention_until（DATE，可空）——
//	列语义不得改（回执已说明这处列名对应关系）。

func (s *Store) rptSampleExpiry(ctx context.Context, f RptFilters) (RptResult, error) {
	// probe：留样数据源整体（无客户维度 ⇒ 全源计数；时间窗口不参与）
	probe, err := scanCount(ctx, s.db, `SELECT COUNT(*) FROM b_sample_retention`)
	if err != nil {
		return RptResult{}, fmt.Errorf("统计数据源失败: %w", err)
	}

	where := ` WHERE 1 = 1`
	var args []interface{}
	d, dargs := dateClause("r.retention_until", f.From, f.To)
	where += d
	args = append(args, dargs...)

	rows, err := s.db.QueryContext(ctx, `
SELECT r.id, s.sample_no, s.role, COALESCE(r.location, ''),
       r.retention_until, r.status, NOW(3)
  FROM b_sample_retention r
  JOIN b_sample s ON s.id = r.sample_id`+where+`
 ORDER BY r.retention_until IS NULL, r.retention_until ASC, r.id`, args...)
	if err != nil {
		return RptResult{}, fmt.Errorf("查询留样到期失败: %w", err)
	}
	defer rows.Close()

	cols := []RptColumn{
		{Key: "sample_no", Label: "留样编号"},
		{Key: "role", Label: "角色"},
		{Key: "location", Label: "位置"},
		{Key: "retention_until", Label: "保留期限"},
		{Key: "status", Label: "状态"},
		{Key: "expired", Label: "是否已到期"},
	}
	out := []map[string]interface{}{}
	for rows.Next() {
		var id int64
		var sampleNo, role, loc, status string
		var until sql.NullTime
		var now time.Time
		if err := rows.Scan(&id, &sampleNo, &role, &loc, &until, &status, &now); err != nil {
			return RptResult{}, fmt.Errorf("读取留样到期失败: %w", err)
		}
		untilStr := ""
		expired := false
		if until.Valid {
			untilStr = until.Time.Format("2006-01-02")
			endOfUntil := time.Date(until.Time.Year(), until.Time.Month(), until.Time.Day(),
				23, 59, 59, 999000000, time.Local)
			expired = now.After(endOfUntil)
		}
		out = append(out, map[string]interface{}{
			"id": id, "sample_no": sampleNo, "role": role, "location": loc,
			"retention_until": untilStr, "status": status, "expired": expired,
		})
	}
	if err := rows.Err(); err != nil {
		return RptResult{}, err
	}
	return finish(RptSampleExpiry, "留样到期", cols, out, probe), nil
}

// ===== 5. nonconform-stat 不合格统计 =====

func (s *Store) rptNonconformStat(ctx context.Context, f RptFilters) (RptResult, error) {
	where := ` WHERE r.judge = '不合格'` + notVoidInsp
	judgeWhere := ` WHERE r.judge IS NOT NULL AND r.judge <> ''` + notVoidInsp
	var args []interface{}
	var jargs []interface{}
	if f.CustomerID > 0 {
		where += ` AND ` + custTargetExpr + ` = ?`
		args = append(args, f.CustomerID)
		judgeWhere += ` AND ` + custTargetExpr + ` = ?`
		jargs = append(jargs, f.CustomerID)
	}
	d, dargs := dateClause("COALESCE(i.test_date, DATE(i.created_at))", f.From, f.To)
	where += d
	args = append(args, dargs...)
	d2, dargs2 := dateClause("COALESCE(i.test_date, DATE(i.created_at))", f.From, f.To)
	judgeWhere += d2
	jargs = append(jargs, dargs2...)

	// probe：不合格结果行整体（维度内）
	probe, err := scanCount(ctx, s.db, `
SELECT COUNT(*) FROM b_inspection_result r
  JOIN b_inspection i ON i.id = r.inspection_id`+custJoins+where, args...)
	if err != nil {
		return RptResult{}, fmt.Errorf("统计数据源失败: %w", err)
	}

	// 分母 = 同范围内**已判定**的结果行（占比 = 不合格项次 / 已判定项次）
	var judged int
	if probe > 0 {
		if judged, err = scanCount(ctx, s.db, `
SELECT COUNT(*) FROM b_inspection_result r
  JOIN b_inspection i ON i.id = r.inspection_id`+custJoins+judgeWhere, jargs...); err != nil {
			return RptResult{}, fmt.Errorf("统计已判定项次失败: %w", err)
		}
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(ti.name, CONCAT('#', r.item_id)) AS item, COUNT(*) AS cnt
  FROM b_inspection_result r
  LEFT JOIN m_test_item ti ON ti.id = r.item_id
  JOIN b_inspection i ON i.id = r.inspection_id`+custJoins+where+`
 GROUP BY item ORDER BY cnt DESC, item`, args...)
	if err != nil {
		return RptResult{}, fmt.Errorf("查询不合格统计失败: %w", err)
	}
	defer rows.Close()

	cols := []RptColumn{
		{Key: "item", Label: "检测项目"},
		{Key: "cnt", Label: "不合格项次"},
		{Key: "pct", Label: "占比"},
	}
	out := []map[string]interface{}{}
	for rows.Next() {
		var item string
		var cnt int
		if err := rows.Scan(&item, &cnt); err != nil {
			return RptResult{}, fmt.Errorf("读取不合格统计失败: %w", err)
		}
		pct := interface{}(nil)
		if judged > 0 {
			pct = fmt.Sprintf("%.1f%%", float64(cnt)/float64(judged)*100)
		}
		out = append(out, map[string]interface{}{"item": item, "cnt": cnt, "pct": pct})
	}
	if err := rows.Err(); err != nil {
		return RptResult{}, err
	}
	return finish(RptNonconform, "不合格统计", cols, out, probe), nil
}
