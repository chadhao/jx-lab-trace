package store

// ===== M10 报表 · 库侧 TC（TC-M10-01~04 的库侧；接口侧见 internal/httpapi/m10_test.go）=====
//
// ★ TC-M10-01 是**纯单测**（结构化机检，不连库）：按附加铁律 9，判据 = SQL 写关键字
//	＋ 业务表名的**结构化匹配**、**限定本批新增文件**，不用裸词；判据自身带正反探针。
// ★ 其余是 DB 集成测试（JX_TEST_DB=1 才跑）；空数据两态口径见 rpt.go 头注。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ===== TC-M10-01 ★★ 架构门禁：M10 零写业务表 =====

// TC-M10-01 ★★：扫描本批新增的 M10 文件（internal/store/rpt.go · internal/httpapi/rpt.go），
// 结构化判据「SQL 写关键字 ＋ 业务表名（b_* / m_*）」零命中（A13 / §6-9）。
// ★ s_audit_log 是唯一合法写，且落在 audit.go（不在被扫描文件内）⇒ 由判据表名单直接排除。
func TestTC_M10_01_Store_ZeroWriteStructuredCheck(t *testing.T) {
	// ★★ 判据自身先被打过（铁律 9：正探针保留 / 负探针消除）
	re := regexp.MustCompile(`(?is)\b(INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+([bm]_[a-z0-9_]+)`)
	pos := []string{
		`INSERT INTO b_share_report (report_no) VALUES ('x')`,
		`UPDATE b_sample_retention SET status = '在库'`,
		`DELETE FROM m_customer WHERE id = 1`,
		"insert\ninto\nb_inspection_result\nSELECT 1", // 跨行
	}
	for _, s := range pos {
		if !re.MatchString(s) {
			t.Fatalf("★ 判据失去真阳性（应命中未命中）: %q", s)
		}
	}
	neg := []string{
		`SELECT COUNT(*) FROM b_production_batch`,
		`INSERT INTO s_audit_log (entity) VALUES ('rpt.x')`, // 允许的唯一写
		`UPDATE s_role_permission SET level = 'READ'`,       // 非本批文件目标
		`// 这里讨论 DELETE FROM b_x 但不是代码`,                     // ← 仍应命中：结构化关键字+表名（宁可严）
	}
	for _, s := range neg[:3] {
		if re.MatchString(s) {
			t.Fatalf("★ 判据出现假阳性: %q", s)
		}
	}

	files := []string{"rpt.go", filepath.Join("..", "httpapi", "rpt.go")}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		if ms := re.FindAllString(string(b), -1); len(ms) > 0 {
			t.Fatalf("★★★ M10 零写被破坏（%s 命中 %d 处写业务表）: %v", f, len(ms), ms)
		}
	}
}

// ===== TC-M10-02 正常：质量趋势数值与历史一致 =====

// TC-M10-02：查某客户的质量趋势 ⇒ 数值与该客户历史检测结果一致（独立 SQL 复算比对）。
func TestTC_M10_02_Store_QualityTrendMatchesHistory(t *testing.T) {
	st, cust, inMat, _ := m9Fixture(t)
	ctx := context.Background()

	truck, _ := m6Pass(t, st, cust, inMat, 1) // 一条合格结论

	res, err := st.RunRpt(ctx, RptQualityTrend, RptFilters{CustomerID: cust})
	if err != nil {
		t.Fatalf("查质量趋势失败: %v", err)
	}
	if !res.HasData {
		t.Fatalf("★★ 有结论数据时 has_data 应 true，实际 %+v", res)
	}

	// 独立复算（只看本车次，作用域明确）
	var total, qualified int
	if err := st.db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(conclusion = '合格'), 0)
  FROM b_inspection
 WHERE target_type = '车次' AND target_id = ? AND conclusion IS NOT NULL`, truck.ID).
		Scan(&total, &qualified); err != nil {
		t.Fatalf("复算检测结论失败: %v", err)
	}
	if total == 0 {
		t.Fatalf("夹具应有 ≥1 张已出结论的单")
	}

	// 汇总行必须等于各桶之和（桶按月，夹具日期必落在某一个桶内）
	sumTotal, sumQualified := 0, 0
	for _, row := range res.Rows {
		sumTotal += row["total"].(int)
		sumQualified += row["qualified"].(int)
	}
	if sumTotal != total || sumQualified != qualified {
		t.Fatalf("★★★ 质量趋势与历史不一致：桶合计 total=%d qualified=%d，实际历史 %d/%d",
			sumTotal, sumQualified, total, qualified)
	}
	// 让步不计入合格（词表同批 6）
	if sumQualified > sumTotal {
		t.Fatalf("合格数不得大于总数")
	}
	_ = fmt.Sprint(res.Name)
}

// ===== TC-M10-03 ★ 空数据：数据未接入 ≠ 本条件下无记录 =====

// TC-M10-03 ★★：① 数据源整体无数据 ⇒ has_data=false + note「数据未接入」；
// ② 有数据但筛选无命中 ⇒ has_data=true + rows 空 + note「本条件下无记录」；
// ★★ 两态文案必须不同（A15 / §6-15）。
func TestTC_M10_03_Store_EmptyDataTwoStates(t *testing.T) {
	st, cust, inMat, outMat := m9Fixture(t)
	ctx := context.Background()
	m6Batch(t, st, cust, inMat, outMat) // 本客户有批 ⇒ 数据源非空

	// ① 维度内整体无数据（不存在的客户 ⇒ 产量源计数 0）
	off, err := st.RunRpt(ctx, RptOutputYield, RptFilters{CustomerID: 999999999})
	if err != nil {
		t.Fatalf("查产量报表失败: %v", err)
	}
	if off.HasData {
		t.Fatalf("★★★ 数据源整体无数据时 has_data 应 false，实际 %+v", off)
	}
	if off.Note != RptNoteNoData {
		t.Fatalf("★★★ 应提示「%s」，实际 %q", RptNoteNoData, off.Note)
	}
	if len(off.Rows) != 0 {
		t.Fatalf("无数据时 rows 应空")
	}

	// ② 有数据 + 不可能命中的时间窗
	none, err := st.RunRpt(ctx, RptOutputYield, RptFilters{
		CustomerID: cust, From: "2000-01-01", To: "2000-01-02",
	})
	if err != nil {
		t.Fatalf("查产量报表失败: %v", err)
	}
	if !none.HasData {
		t.Fatalf("★★★ 有数据源但筛选无命中时 has_data 应 true，实际 %+v", none)
	}
	if len(none.Rows) != 0 {
		t.Fatalf("不可能命中的筛选 rows 应空，实际 %d 行", len(none.Rows))
	}
	if none.Note != RptNoteNoMatch {
		t.Fatalf("★★★ 应提示「%s」，实际 %q", RptNoteNoMatch, none.Note)
	}

	// ★ 两态文案必须不同
	if off.Note == none.Note {
		t.Fatalf("★★★ 两态文案必须不同（防「错误表现为正确」）: 都是 %q", off.Note)
	}
}

// ===== TC-M10-04 正常：导出数据与屏幕一致的库侧基础 =====

// TC-M10-04（库侧）：客户对账行数与吨位 = 独立 SQL 复算（CSV 与屏幕一致的根基；
// BOM / CSV 序列化 / 导出审计见接口侧 TestTC_M10_04_HTTP_ExportCSVWithAudit）。
func TestTC_M10_04_Store_ExportRowsMatchQuery(t *testing.T) {
	st, cust, inMat, outMat := m9Fixture(t)
	ctx := context.Background()

	m9Chain(t, st, cust, inMat, outMat, false) // 含已出场的出货单

	res, err := st.RunRpt(ctx, RptCustomerRecon, RptFilters{CustomerID: cust})
	if err != nil {
		t.Fatalf("查客户对账失败: %v", err)
	}
	if !res.HasData || len(res.Rows) == 0 {
		t.Fatalf("夹具有已出场出货单，报表应有行: %+v", res)
	}

	// 独立复算：本客户已出场且未撤销的出货单数与袋净重合计
	var orders int
	var tons float64
	if err := st.db.QueryRowContext(ctx, `
SELECT COUNT(DISTINCT s.id), COALESCE(SUM(COALESCE(b.weight_allocated, 0)), 0)
  FROM b_shipment s
  JOIN b_shipment_item i ON i.shipment_id = s.id
  JOIN b_fg_bag b ON b.id = i.fg_bag_id
 WHERE s.customer_id = ? AND s.ship_at IS NOT NULL AND s.status <> '已撤销'`, cust).
		Scan(&orders, &tons); err != nil {
		t.Fatalf("复算对账数据失败: %v", err)
	}

	var gotOrders int
	var gotTons float64
	for _, row := range res.Rows {
		if int(row["customer_id"].(int64)) != int(cust) {
			continue
		}
		gotOrders = int(row["orders"].(int))
		gotTons = row["tons"].(float64)
	}
	if gotOrders != orders || gotTons != tons {
		t.Fatalf("★★★ 报表与库不一致：报表 orders=%d tons=%v，库 %d / %v",
			gotOrders, gotTons, orders, tons)
	}
}

// ===== 补充纯单测：has_data 组装的两态文案 =====

func TestRptFinishTwoStatesDiffer(t *testing.T) {
	a := finish(RptOutputYield, "产量与合格率", nil, nil, 0)
	if a.HasData || a.Note != RptNoteNoData {
		t.Fatalf("空源应 false + %q，实际 %+v", RptNoteNoData, a)
	}
	b := finish(RptOutputYield, "产量与合格率", nil, []map[string]interface{}{}, 3)
	if !b.HasData || b.Note != RptNoteNoMatch {
		t.Fatalf("筛选无命中应 true + %q，实际 %+v", RptNoteNoMatch, b)
	}
	if a.Note == b.Note {
		t.Fatalf("两态文案必须不同")
	}
	c := finish(RptOutputYield, "产量与合格率", nil,
		[]map[string]interface{}{{"batch": "x"}}, 3)
	if !c.HasData || c.Note != "" {
		t.Fatalf("有数据有行不应带空数据 note，实际 %+v", c)
	}
}

// 筛选参数校验：非法日期 / 非法 bucket ⇒ 400。
func TestValidateRptFiltersRejectsBadInput(t *testing.T) {
	if err := ValidateRptFilters(RptFilters{From: "banana"}); err == nil {
		t.Fatal("非法 from 应报错")
	}
	if err := ValidateRptFilters(RptFilters{Bucket: "day"}); err == nil {
		t.Fatal("非法 bucket 应报错")
	}
	if err := ValidateRptFilters(RptFilters{CustomerID: -1}); err == nil {
		t.Fatal("负 customer_id 应报错")
	}
	if err := ValidateRptFilters(RptFilters{From: "2026-01-01", To: "2026-01-31", Bucket: "week"}); err != nil {
		t.Fatalf("合法筛选不应报错: %v", err)
	}
}

// 报表名白名单：未知报表 ⇒ ErrRptUnknown（接口侧映射 404）。
func TestRunRptUnknownName(t *testing.T) {
	if _, err := (*Store)(nil).RunRpt(context.Background(), "no-such", RptFilters{}); err == nil {
		t.Fatal("未知报表应报错")
	} else if !strings.Contains(err.Error(), ErrRptUnknown.Error()) {
		t.Fatalf("应为 ErrRptUnknown，实际 %v", err)
	}
}
