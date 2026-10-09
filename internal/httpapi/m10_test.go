package httpapi

// ===== M10 报表 · 接口级 TC（TC-M10-01~04 双层形态的接口侧；库侧见 internal/store/m10_test.go）=====
//
// ★ 账号：qc（rpt.view=ALL / rpt.export=ALL）走查询与导出；★ 纯 receiver
// （rpt.view=READ / rpt.export=NONE）验证「查得见、导不出」（A14）。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip（TC-M10-01 除外，纯单测）。
// ★ 导出：CSV + UTF-8 BOM + 表头与屏幕一致 + 审计 action=rpt_export（A17 / TC-M10-04）。

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/chadhao/jx-lab-trace/internal/store"
)

// ===== TC-M10-01 ★★ 架构门禁：M10 零写业务表（接口侧）=====

// TC-M10-01（接口侧）：扫 internal/httpapi/rpt.go 与 internal/store/rpt.go ——
// 结构化判据「SQL 写关键字 ＋ 业务表名（b_* / m_*）」零命中（A13 / §6-9）。
func TestTC_M10_01_HTTP_ZeroWriteStructuredCheck(t *testing.T) {
	re := regexp.MustCompile(`(?is)\b(INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+([bm]_[a-z0-9_]+)`)
	// 正反探针（判据自身必须先被打过，铁律 9）
	if !re.MatchString(`INSERT INTO b_fg_bag (code) VALUES ('x')`) {
		t.Fatal("★ 判据失去真阳性")
	}
	if re.MatchString(`SELECT * FROM b_fg_bag`) || re.MatchString(`INSERT INTO s_audit_log (entity) VALUES ('x')`) {
		t.Fatal("★ 判据出现假阳性")
	}

	for _, f := range []string{"rpt.go", filepath.Join("..", "store", "rpt.go")} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		if ms := re.FindAllString(string(b), -1); len(ms) > 0 {
			t.Fatalf("★★★ M10 零写被破坏（%s 命中 %d 处）: %v", f, len(ms), ms)
		}
	}
}

// doRaw 见 m3_test.go（返回 *http.Response + 原始文本，CSV 等非 JSON 响应用）。

// ===== TC-M10-02 正常：质量趋势数值与历史一致 =====

func TestTC_M10_02_HTTP_QualityTrendMatchesHistory(t *testing.T) {
	e, ck := m9HTTPEnv(t)
	cust, inMat, _ := m8HMasterIDs(t, e.st)

	truck, bags := m6HTruck(t, e, ck, cust, inMat, 1)
	m6HPass(t, e, ck, truck, fmt.Sprint(bags[0]["code"]))

	resp, body := e.do("GET",
		fmt.Sprintf("/api/rpt/quality-trend?customer_id=%d", cust), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查质量趋势应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if body["has_data"] != true {
		t.Fatalf("★★ 有结论数据 has_data 应 true，实际 %v", body)
	}
	rows, _ := body["rows"].([]interface{})
	sumTotal, sumQualified := 0, 0
	for _, it := range rows {
		m, _ := it.(map[string]interface{})
		sumTotal += int(m["total"].(float64))
		sumQualified += int(m["qualified"].(float64))
	}

	// 独立复算（本车次作用域）
	var total, qualified int
	if err := e.st.DB().QueryRowContext(context.Background(), `
SELECT COUNT(*), COALESCE(SUM(conclusion = '合格'), 0)
  FROM b_inspection
 WHERE target_type = '车次' AND target_id = ? AND conclusion IS NOT NULL`, truck).
		Scan(&total, &qualified); err != nil {
		t.Fatalf("复算失败: %v", err)
	}
	if total == 0 {
		t.Fatal("夹具应有已出结论的单")
	}
	if sumTotal != total || sumQualified != qualified {
		t.Fatalf("★★★ 质量趋势与历史不一致：报表 %d/%d，库 %d/%d",
			sumTotal, sumQualified, total, qualified)
	}

	// 非法筛选 ⇒ 400
	if resp, body := e.do("GET", "/api/rpt/quality-trend?from=banana", ck, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 from 应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	// 未知报表 ⇒ 404
	if resp, _ := e.do("GET", "/api/rpt/no-such", ck, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知报表应 404，实际 %d", resp.StatusCode)
	}
}

// ===== TC-M10-03 ★ 空数据：数据未接入 ≠ 本条件下无记录 =====

func TestTC_M10_03_HTTP_EmptyDataTwoStates(t *testing.T) {
	e, ck := m9HTTPEnv(t)
	cust, inMat, outMat := m8HMasterIDs(t, e.st)
	m6HBatch(t, e, ck, cust, inMat, outMat) // 本客户有批 ⇒ 数据源非空

	// ① 数据源（维度内）整体无数据
	resp, off := e.do("GET", "/api/rpt/output-yield?customer_id=999999999", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查报表应 200，实际 %d body=%v", resp.StatusCode, off)
	}
	if off["has_data"] != false {
		t.Fatalf("★★★ 无数据源时 has_data 应 false，实际 %v", off)
	}
	if fmt.Sprint(off["note"]) != store.RptNoteNoData {
		t.Fatalf("★★★ 应提示「%s」，实际 %v", store.RptNoteNoData, off["note"])
	}
	if rows, ok := off["rows"].([]interface{}); !ok || len(rows) != 0 {
		t.Fatalf("无数据 rows 应空，实际 %v", off["rows"])
	}

	// ② 有数据源 + 不可能命中的筛选
	resp, none := e.do("GET",
		fmt.Sprintf("/api/rpt/output-yield?customer_id=%d&from=2000-01-01&to=2000-01-02", cust),
		ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查报表应 200，实际 %d body=%v", resp.StatusCode, none)
	}
	if none["has_data"] != true {
		t.Fatalf("★★★ 筛选无命中时 has_data 应 true，实际 %v", none)
	}
	if fmt.Sprint(none["note"]) != store.RptNoteNoMatch {
		t.Fatalf("★★★ 应提示「%s」，实际 %v", store.RptNoteNoMatch, none["note"])
	}
	// ★ 两态文案必须不同
	if fmt.Sprint(off["note"]) == fmt.Sprint(none["note"]) {
		t.Fatalf("★★★ 两态文案必须不同，都是 %v", off["note"])
	}
}

// ===== TC-M10-04 正常：导出 CSV 与屏幕一致 + 有审计 =====

func TestTC_M10_04_HTTP_ExportCSVWithAudit(t *testing.T) {
	e, ck := m9HTTPEnv(t)
	st := e.st
	cust, inMat, outMat := m8HMasterIDs(t, st)
	m9HChain(t, e, ck, cust, inMat, outMat) // 含已出场出货单

	// 屏幕侧
	resp, page := e.do("GET", fmt.Sprintf("/api/rpt/customer-recon?customer_id=%d", cust), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查客户对账应 200，实际 %d body=%v", resp.StatusCode, page)
	}
	pageRows, _ := page["rows"].([]interface{})
	if len(pageRows) == 0 {
		t.Fatalf("夹具应有对账行: %v", page)
	}

	// 导出
	payload := fmt.Sprintf(`{"name":"customer-recon","filters":{"customer_id":%d}}`, cust)
	resp2, raw := e.doRaw("POST", "/api/rpt/export", ck, payload)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("导出应 200，实际 %d body=%s", resp2.StatusCode, raw)
	}
	if ctype := resp2.Header.Get("Content-Type"); !strings.Contains(ctype, "text/csv") {
		t.Fatalf("★ Content-Type 应 text/csv，实际 %q", ctype)
	}
	bom := string([]byte{0xEF, 0xBB, 0xBF})
	if !strings.HasPrefix(raw, bom) {
		t.Fatalf("★★★ CSV 必须带 UTF-8 BOM（Excel 直开），实际前 3 字节 %v", []byte(raw[:3]))
	}
	lines := strings.Split(strings.TrimRight(strings.TrimPrefix(raw, bom), "\n"), "\n")
	if len(lines) != len(pageRows)+1 {
		t.Fatalf("★★★ CSV 行数应 = 表头 1 + 屏幕行数 %d，实际 %d", len(pageRows), len(lines))
	}
	// 表头 = Columns 的 Label（与屏幕列一致）
	cols, _ := page["columns"].([]interface{})
	wantHeader := make([]string, 0, len(cols))
	for _, c := range cols {
		m, _ := c.(map[string]interface{})
		wantHeader = append(wantHeader, fmt.Sprint(m["label"]))
	}
	if got := lines[0]; got != strings.Join(wantHeader, ",") {
		t.Fatalf("★★ 表头应与屏幕列一致：\n  期望 %q\n  实际 %q",
			strings.Join(wantHeader, ","), got)
	}
	// 行内含客户名
	if !strings.Contains(strings.Join(lines, "\n"), "M8接口测试客户") {
		t.Fatalf("★★ CSV 应含客户名，实际:\n%s", strings.Join(lines, "\n"))
	}

	// 审计：action=rpt_export，entity=rpt.customer-recon
	var n int
	if err := st.DB().QueryRowContext(context.Background(), `
SELECT COUNT(*) FROM s_audit_log
 WHERE entity = 'rpt.customer-recon' AND action = 'rpt_export'
   AND actor_open_id = ?`, m8HTTPOpenID).Scan(&n); err != nil {
		t.Fatalf("查导出审计失败: %v", err)
	}
	if n == 0 {
		t.Fatalf("★★★ 导出必须写审计（s_audit_log / rpt_export）")
	}
	// 审计 new_value 记筛选条件
	var nv string
	if err := st.DB().QueryRowContext(context.Background(), `
SELECT COALESCE(new_value, '') FROM s_audit_log
 WHERE entity = 'rpt.customer-recon' AND action = 'rpt_export'
   AND actor_open_id = ? ORDER BY id DESC LIMIT 1`, m8HTTPOpenID).Scan(&nv); err != nil {
		t.Fatalf("读审计 new_value 失败: %v", err)
	}
	if !strings.Contains(nv, "customer_id") {
		t.Fatalf("★ 审计应记筛选条件，实际 %q", nv)
	}

	// 权限：纯 receiver（rpt.export=NONE）⇒ 403（A14）
	recv := e.login(m9HRecvOpenID)
	resp3, raw2 := e.doRaw("POST", "/api/rpt/export", recv, payload)
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("★★ receiver 导出应 403，实际 %d body=%s", resp3.StatusCode, raw2)
	}
	// 纯 receiver 可查（rpt.view=READ）
	if resp, _ := e.do("GET", fmt.Sprintf("/api/rpt/customer-recon?customer_id=%d", cust),
		recv, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("★★ receiver 查报表应 200，实际 %d", resp.StatusCode)
	}
}
