package httpapi

// ===== M3 收货与打码 · 接口级 TC =====
//
// ★ 账号：receiver 角色（对 8 个 recv.* 权限点全部 ALL）。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。
// ★ 固定测试主数据（客户 9302 / 物料 9302）+ 固定链根日期，跑前跑后各清一次，
//	保证车序每次从 01 起算、可重复执行。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	m3HTTPOpenID   = "ou_test_m3_http"
	m3HTTPCustCode = "9302"
	m3HTTPMatCode  = "9302"
	m3HTTPDate     = "2026-10-09"
)

// m3HTTPWipe 清掉 M3 接口测试数据。
//
// ★ 顺序硬约束：**先袋、后车** —— 袋码 uk_bag_code 全局唯一，只删车不删袋会留下
//
//	孤儿袋码，下一轮生成同码袋必然撞号（本批实测踩过）。
//
// ★ 范围硬约束：只删**本账号**（ou_test_m3_http）与**本测试客户 9302** 的行，
//
//	不得用 `LIKE 'ou_test_m3%'` 前缀（会误伤 store 包的行）。
func m3HTTPWipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	me := m3HTTPOpenID
	q := func(sql string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sql, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}

	// ① 作废记录 → ② 检测单 → ③ 袋（含孤儿袋）→ ④ 车 → ⑤ 预报 → ⑥ 标签留痕
	q(`DELETE FROM b_obj_void WHERE entity = 'b_bag'
	     AND entity_id IN (SELECT id FROM b_bag WHERE created_by = ?)`, me)
	q(`DELETE FROM b_inspection WHERE created_by = ?`, me)
	q(`DELETE FROM b_bag WHERE created_by = ?
	     OR truck_lot_id IN (SELECT id FROM b_truck_lot
	                           WHERE customer_id IN (SELECT id FROM m_customer WHERE code = ?))`,
		me, m3HTTPCustCode)
	q(`DELETE FROM b_truck_lot WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, m3HTTPCustCode)
	q(`DELETE FROM b_arrival_notice WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, m3HTTPCustCode)
	q(`DELETE FROM b_label_print WHERE created_by = ?`, me)

	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m3HTTPCustCode)
	q(`DELETE FROM m_material WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m3HTTPMatCode)
}

// m3HTTPEnv 建测试主数据 + 起服务 + 以 receiver 登录。
func m3HTTPEnv(t *testing.T) (*env, string) {
	t.Helper()
	st := requireDB(t)
	m3HTTPWipe(t, st)
	ctx := context.Background()
	res, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M3接口测试客户', '启用', 1, 1, ?)`, m3HTTPCustCode, m3HTTPOpenID)
	if err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	custID, _ := res.LastInsertId()
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M3接口测试原料', '原料', '启用', 1, 1, ?)`, m3HTTPMatCode, m3HTTPOpenID); err != nil {
		t.Fatalf("建测试物料失败: %v", err)
	}
	_ = custID
	t.Cleanup(func() { m3HTTPWipe(t, st) })

	bindRole(t, st, m3HTTPOpenID, "receiver")
	e := newEnv(t, time.Hour)
	return e, e.login(m3HTTPOpenID)
}

// m3MasterIDs 取测试客户 / 物料的当前行 id。
func m3MasterIDs(t *testing.T, st *store.Store) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var cust, mat int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_customer WHERE code = ? AND is_current = 1`, m3HTTPCustCode).Scan(&cust); err != nil {
		t.Fatalf("查测试客户失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, m3HTTPMatCode).Scan(&mat); err != nil {
		t.Fatalf("查测试物料失败: %v", err)
	}
	return cust, mat
}

// doRaw 发请求并返回原始响应体文本（HTML 版式页等非 JSON 响应用）。
func (e *env) doRaw(method, path, cookie, body string) (*http.Response, string) {
	e.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("构造请求失败: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

// m3NoticeBody 造预报请求体。
func m3NoticeBody(custID, matID int64, est int) string {
	b, _ := json.Marshal(map[string]interface{}{
		"customer_id": custID, "material_id": matID, "biz_type": "CG",
		"arrive_date": m3HTTPDate, "plate_no": "湘F·M3", "driver": "司机乙",
		"est_bag_count": est,
	})
	return string(b)
}

// m3ArriveViaHTTP 走接口完成「预报 → 到货确认」，返回车次行。
func m3ArriveViaHTTP(t *testing.T, e *env, ck string, custID, matID int64, est int) map[string]interface{} {
	t.Helper()
	resp, body := e.do("POST", "/api/recv/notices", ck, m3NoticeBody(custID, matID, est))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预报登记应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	notice, _ := body["row"].(map[string]interface{})
	resp, body = e.do("POST", "/api/recv/arrivals", ck,
		fmt.Sprintf(`{"notice_id": %v}`, int64(numOf(notice["id"]))))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("到货确认应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	truck, _ := body["row"].(map[string]interface{})
	if truck == nil {
		t.Fatalf("到货确认未返回车次: %v", body)
	}
	return truck
}

// m3WeighAndBags 过磅 + 按实际袋数生成袋码，返回袋行数组。
func m3WeighAndBags(t *testing.T, e *env, ck string, truckID int64, gross, tare string, bags int) []map[string]interface{} {
	t.Helper()
	resp, body := e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/weigh", truckID), ck,
		fmt.Sprintf(`{"gross_weight": %s, "tare_weight": %s}`, gross, tare))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("过磅应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	resp, body = e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/bags", truckID), ck,
		fmt.Sprintf(`{"count": %d}`, bags))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("生成袋码应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	raw, _ := body["rows"].([]interface{})
	out := make([]map[string]interface{}, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]interface{})
		out = append(out, m)
	}
	return out
}

// TC-M3-07 边界：无预报直接到货 ⇒ 补录 + 立即确认成功，车序照常分配。
func TestTC_M3_07_DirectArrivalWithoutNotice(t *testing.T) {
	e, ck := m3HTTPEnv(t)
	cust, mat := m3MasterIDs(t, e.st)

	body := fmt.Sprintf(`{"notice": %s, "remark": "现场临时派车"}`,
		m3NoticeBody(cust, mat, 0))
	resp, out := e.do("POST", "/api/recv/arrivals", ck, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("无预报直接到货应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	truck, _ := out["row"].(map[string]interface{})
	if truck == nil {
		t.Fatalf("未返回车次: %v", out)
	}
	code := fmt.Sprint(truck["code"])
	if len(code) != 27 || !strings.HasPrefix(code, "1A") {
		t.Fatalf("应生成 27 位车码（1A 开头），实际 %q", code)
	}
	if numOf(truck["seq_no"]) < 1 {
		t.Fatalf("车序应已分配，实际 %v", truck["seq_no"])
	}
	if truck["status"] != "待检" {
		t.Fatalf("车次状态应「待检」，实际 %v", truck["status"])
	}
	if truck["arrive_date"] != m3HTTPDate {
		t.Fatalf("链根日期应 %s，实际 %v", m3HTTPDate, truck["arrive_date"])
	}
	if truck["human"] == "" || !strings.Contains(fmt.Sprint(truck["human"]), "-") {
		t.Fatalf("应返回人读行 human，实际 %v", truck["human"])
	}
}

// TC-M3-08 边界：改预报车牌 ⇒ **车序不变**（forecast_edit_changes_vehicle_seq = false）。
func TestTC_M3_08_EditNoticeKeepsVehicleSeq(t *testing.T) {
	e, ck := m3HTTPEnv(t)
	cust, mat := m3MasterIDs(t, e.st)

	resp, body := e.do("POST", "/api/recv/notices", ck, m3NoticeBody(cust, mat, 0))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预报登记应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	notice, _ := body["row"].(map[string]interface{})
	id := numOf(notice["id"])
	before := numOf(notice["seq_no"])
	if before != 1 {
		t.Fatalf("首车序应 01，实际 %d", before)
	}

	resp, body = e.do("PUT", fmt.Sprintf("/api/recv/notices/%d", id), ck,
		`{"reason":"车牌录错", "plate_no":"湘F·改", "driver":"司机丙", "est_bag_count": 25}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改预报应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	after, _ := body["row"].(map[string]interface{})
	if numOf(after["seq_no"]) != before {
		t.Fatalf("改预报不得改车序：改前 %d，改后 %d", before, numOf(after["seq_no"]))
	}
	if after["plate_no"] != "湘F·改" {
		t.Fatalf("车牌应被更新，实际 %v", after["plate_no"])
	}
	if numOf(after["est_bag_count"]) != 25 {
		t.Fatalf("预计袋数应更新为 25，实际 %v", after["est_bag_count"])
	}

	// 链根日期不允许改（车序与码绑定在该日）
	resp, body = e.do("PUT", fmt.Sprintf("/api/recv/notices/%d", id), ck,
		fmt.Sprintf(`{"reason":"改日期", "arrive_date":"%s"}`, "2026-10-11"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("改链根日期应 400，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TC-M3-11 正常：补打填原因 ⇒ 写记录且 is_reprint=1；★ 原因为空 ⇒ 拒绝。
func TestTC_M3_11_ReprintRequiresReason(t *testing.T) {
	e, ck := m3HTTPEnv(t)
	cust, mat := m3MasterIDs(t, e.st)
	truck := m3ArriveViaHTTP(t, e, ck, cust, mat, 0)
	truckID := numOf(truck["id"])
	bags := m3WeighAndBags(t, e, ck, truckID, "31.5", "1.5", 3)
	if len(bags) != 3 {
		t.Fatalf("应生成 3 个袋码，实际 %d", len(bags))
	}
	code := fmt.Sprint(bags[0]["code"])

	// ① 首次打印
	resp, body := e.do("POST", "/api/recv/labels/print", ck,
		fmt.Sprintf(`{"codes": [%q]}`, code))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("首次打印应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	pageURL, _ := body["page_url"].(string)
	if !strings.HasPrefix(pageURL, "/api/recv/labels/page?ids=") {
		t.Fatalf("应返回版式页地址，实际 %v", body["page_url"])
	}
	resp, page := e.doRaw("GET", pageURL, ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("版式页应 200，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(page, code) || !strings.Contains(page, "-") {
		t.Fatalf("版式页应含二维码内容与人读行，实际片段：%s", trunc(page, 300))
	}
	if !strings.Contains(page, "/api/recv/labels/qr?data=") {
		t.Fatalf("版式页应引用二维码资源")
	}

	// ② 同一码再次走「打印」（未填原因）⇒ 拒绝（防止一物两码从后门绕过）
	resp, body = e.do("POST", "/api/recv/labels/print", ck, fmt.Sprintf(`{"codes": [%q]}`, code))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("重复打印（无原因）应 400，实际 %d body=%v", resp.StatusCode, body)
	}

	// ③ 补打但原因为空 ⇒ 拒绝
	resp, body = e.do("POST", "/api/recv/labels/reprint", ck,
		fmt.Sprintf(`{"codes": [%q], "reason": ""}`, code))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("补打缺原因应 400，实际 %d body=%v", resp.StatusCode, body)
	}

	// ④ 补打填原因 ⇒ 写记录且 is_reprint=1
	resp, body = e.do("POST", "/api/recv/labels/reprint", ck,
		fmt.Sprintf(`{"codes": [%q], "reason": "标签破损"}`, code))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("补打应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	labels, _ := body["labels"].([]interface{})
	if len(labels) != 1 {
		t.Fatalf("补打应返回 1 条记录，实际 %v", body)
	}
	first, _ := labels[0].(map[string]interface{})
	if numOf(first["is_reprint"]) != 1 {
		t.Fatalf("补打记录 is_reprint 应 1，实际 %v", first["is_reprint"])
	}
	if first["reason"] != "标签破损" {
		t.Fatalf("补打记录应带原因，实际 %v", first["reason"])
	}

	// ⑤ 打印历史可查
	resp, body = e.do("GET", "/api/recv/labels/history?code="+code, ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("打印历史应 200，实际 %d", resp.StatusCode)
	}
	rows, _ := body["rows"].([]interface{})
	if len(rows) < 2 {
		t.Fatalf("打印历史应 ≥2 条（首打 + 补打），实际 %v", body)
	}
	found := false
	for _, r := range rows {
		m, _ := r.(map[string]interface{})
		if numOf(m["is_reprint"]) == 1 && m["reason"] == "标签破损" {
			found = true
		}
	}
	if !found {
		t.Fatalf("打印历史里应有 is_reprint=1 且带原因的记录，实际 %v", body)
	}
}

// TC-M3-12 ★ 异常：对**已投料**的袋执行作废 ⇒ 拒绝。
func TestTC_M3_12_FedBagCannotVoid(t *testing.T) {
	e, ck := m3HTTPEnv(t)
	cust, mat := m3MasterIDs(t, e.st)
	truck := m3ArriveViaHTTP(t, e, ck, cust, mat, 0)
	truckID := numOf(truck["id"])
	bags := m3WeighAndBags(t, e, ck, truckID, "10", "1", 2)
	if len(bags) != 2 {
		t.Fatalf("应生成 2 个袋码，实际 %d", len(bags))
	}
	bagID := numOf(bags[0]["id"])

	// 造一条投料记录（M6 尚未实现，直接写库模拟「现场已经用过它」）
	if _, err := e.st.DB().ExecContext(context.Background(), `
INSERT INTO b_feed_record (batch_id, bag_id, operator, created_by)
VALUES (999999, ?, 'tester', ?)`, bagID, m3HTTPOpenID); err != nil {
		t.Fatalf("造投料记录失败: %v", err)
	}

	resp, body := e.do("POST", fmt.Sprintf("/api/recv/bags/%d/void", bagID), ck,
		`{"reason":"录多了"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("已投料袋作废应 409，实际 %d body=%v", resp.StatusCode, body)
	}
	if !strings.Contains(fmt.Sprint(body["error"]), "已取样") &&
		!strings.Contains(fmt.Sprint(body["error"]), "已投料") {
		t.Fatalf("错误提示应说明已取样/已投料，实际 %v", body)
	}
	// 袋状态不得变化
	resp, body = e.do("GET", fmt.Sprintf("/api/recv/trucks/%d/bags", truckID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查袋应 200，实际 %d", resp.StatusCode)
	}
	rows, _ := body["rows"].([]interface{})
	m0, _ := rows[0].(map[string]interface{})
	if m0["status"] != "在库" {
		t.Fatalf("被拒作废的袋状态应仍为「在库」，实际 %v", m0["status"])
	}
}

// TC-M3-13 正常：作废 2 个多余袋 ⇒ 车次 bag_count 按有效袋计；
// ★ 作废码不可再生成（重复生成袋码被拒），作废码永久不重用。
func TestTC_M3_13_VoidBagsCountsValidOnly(t *testing.T) {
	e, ck := m3HTTPEnv(t)
	cust, mat := m3MasterIDs(t, e.st)
	truck := m3ArriveViaHTTP(t, e, ck, cust, mat, 0)
	truckID := numOf(truck["id"])
	bags := m3WeighAndBags(t, e, ck, truckID, "30.5", "0.5", 10)
	if len(bags) != 10 {
		t.Fatalf("应生成 10 个袋码，实际 %d", len(bags))
	}

	// 录错袋数 ⇒ 多余 2 袋走作废（不改正、不删行）
	for _, idx := range []int{8, 9} {
		bagID := numOf(bags[idx]["id"])
		resp, body := e.do("POST", fmt.Sprintf("/api/recv/bags/%d/void", bagID), ck,
			`{"reason":"袋数录错，多余袋作废"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("作废袋应 200，实际 %d body=%v", resp.StatusCode, body)
		}
		row, _ := body["row"].(map[string]interface{})
		if row["status"] != "作废" {
			t.Fatalf("作废后袋状态应「作废」，实际 %v", row["status"])
		}
	}

	// bag_count 按**有效**袋计
	resp, body := e.do("GET", fmt.Sprintf("/api/recv/trucks/%d", truckID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查车次应 200，实际 %d", resp.StatusCode)
	}
	row, _ := body["row"].(map[string]interface{})
	if numOf(row["bag_count"]) != 8 {
		t.Fatalf("bag_count 应按有效袋计为 8，实际 %v", row["bag_count"])
	}

	// 作废码不可再生成：重复调用袋码生成 ⇒ 拒绝
	resp, body = e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/bags", truckID), ck, `{"count": 10}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复生成袋码应 409，实际 %d body=%v", resp.StatusCode, body)
	}

	// 同一袋再次作废 ⇒ 拒绝（uk_void_entity + status 双保险）
	bagID := numOf(bags[8]["id"])
	resp, body = e.do("POST", fmt.Sprintf("/api/recv/bags/%d/void", bagID), ck,
		`{"reason":"再作废一次"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复作废应 409，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TC-M3-14 正常：质检处置为「退货」⇒ 退车登记成功，车次转「已退货」且**全部袋码作废**。
func TestTC_M3_14_ReturnTruckWithDisposition(t *testing.T) {
	e, ck := m3HTTPEnv(t)
	cust, mat := m3MasterIDs(t, e.st)
	truck := m3ArriveViaHTTP(t, e, ck, cust, mat, 0)
	truckID := numOf(truck["id"])
	bags := m3WeighAndBags(t, e, ck, truckID, "21", "1", 5)

	// 质检「退货」处置判定（M5 尚未实现，直接写库落判定）
	if _, err := e.st.DB().ExecContext(context.Background(), `
INSERT INTO b_inspection (inspection_no, target_type, target_id, conclusion, disposition, created_by)
VALUES (?, '车次', ?, '不合格', '退货', ?)`,
		fmt.Sprintf("M3-RET-%d", truckID), truckID, m3HTTPOpenID); err != nil {
		t.Fatalf("造质检处置失败: %v", err)
	}

	resp, body := e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/return", truckID), ck,
		`{"reason":"水分超标，整车退回"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("退车登记应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	if row["status"] != "已退货" {
		t.Fatalf("车次状态应「已退货」，实际 %v", row["status"])
	}
	if numOf(row["bag_count"]) != 0 {
		t.Fatalf("退车后 bag_count 应 0，实际 %v", row["bag_count"])
	}

	// 该车全部袋码作废
	resp, body = e.do("GET", fmt.Sprintf("/api/recv/trucks/%d/bags", truckID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查袋应 200，实际 %d", resp.StatusCode)
	}
	rows, _ := body["rows"].([]interface{})
	if len(rows) != len(bags) {
		t.Fatalf("袋数应保持 %d（作废不删行），实际 %d", len(bags), len(rows))
	}
	for _, r := range rows {
		m, _ := r.(map[string]interface{})
		if m["status"] != "作废" {
			t.Fatalf("退车后所有袋应「作废」，袋 %v 实际 %v", m["bag_seq"], m["status"])
		}
	}
}

// TC-M3-15 ★ 异常：未经质检「退货」判定就直接退车登记 ⇒ **拒绝**。
func TestTC_M3_15_ReturnWithoutDispositionRejected(t *testing.T) {
	e, ck := m3HTTPEnv(t)
	cust, mat := m3MasterIDs(t, e.st)
	truck := m3ArriveViaHTTP(t, e, ck, cust, mat, 0)
	truckID := numOf(truck["id"])
	m3WeighAndBags(t, e, ck, truckID, "11", "1", 3)

	resp, body := e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/return", truckID), ck,
		`{"reason":"先退了再说"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("无退货判定的退车应 409，实际 %d body=%v", resp.StatusCode, body)
	}
	if !strings.Contains(fmt.Sprint(body["error"]), "退货") {
		t.Fatalf("错误提示应说明需要质检退货判定，实际 %v", body)
	}
	// 车次与袋均不应变化
	resp, body = e.do("GET", fmt.Sprintf("/api/recv/trucks/%d", truckID), ck, "")
	row, _ := body["row"].(map[string]interface{})
	if row["status"] != "待检" {
		t.Fatalf("被拒后车次状态应仍为「待检」，实际 %v", row["status"])
	}
}

// A6 全链可用：预报 → 到货确认 → 过磅 → N 个袋码 → 打印 → 扫码定位。
//
// ★ 同时验证 D1 的两种结果可区分：校验位错 ⇒ 400「请重扫」；
//
//	格式正确但系统内不存在 ⇒ 404「格式正确但系统内不存在」。
func TestM3_FullChain_HTTP(t *testing.T) {
	e, ck := m3HTTPEnv(t)
	cust, mat := m3MasterIDs(t, e.st)

	// ① 预报
	resp, body := e.do("POST", "/api/recv/notices", ck, m3NoticeBody(cust, mat, 30))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预报登记应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	notice, _ := body["row"].(map[string]interface{})
	noticeID := numOf(notice["id"])
	if numOf(notice["seq_no"]) != 1 {
		t.Fatalf("首车序应 01，实际 %v", notice["seq_no"])
	}

	// ② 到货确认 ⇒ 车码 A
	resp, body = e.do("POST", "/api/recv/arrivals", ck, fmt.Sprintf(`{"notice_id": %d}`, noticeID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("到货确认应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	truck, _ := body["row"].(map[string]interface{})
	truckID := numOf(truck["id"])
	code := fmt.Sprint(truck["code"])
	if len(code) != 27 {
		t.Fatalf("车码应 27 位，实际 %q", code)
	}

	// 预报已到货，不能重复确认
	resp, body = e.do("POST", "/api/recv/arrivals", ck, fmt.Sprintf(`{"notice_id": %d}`, noticeID))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复确认应 409，实际 %d body=%v", resp.StatusCode, body)
	}

	// ④ 过磅前不能生成袋码
	resp, body = e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/bags", truckID), ck, `{"count": 30}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("未过磅生成袋码应 409，实际 %d body=%v", resp.StatusCode, body)
	}

	// ⑤ 过磅 + 批量袋码
	bags := m3WeighAndBags(t, e, ck, truckID, "35.5", "5.5", 30)
	if len(bags) != 30 {
		t.Fatalf("应生成 30 个袋码，实际 %d", len(bags))
	}
	for i, b := range bags {
		c := fmt.Sprint(b["code"])
		if len(c) != 27 || !strings.HasPrefix(c, "1B") {
			t.Fatalf("袋 %d 码应为 27 位 1B 开头，实际 %q", i+1, c)
		}
		// 前缀 = BT+客户+物料+链根日期+车序（T 位本来就不同：A 车次 / B 袋）
		if c[2:20] != code[2:20] {
			t.Fatalf("袋码前缀应与车码一致（除 T 位）：%s vs %s", c, code)
		}
	}
	if fmt.Sprint(bags[0]["human"]) == "" {
		t.Fatalf("袋码应带人读行: %v", bags[0])
	}

	// ⑥ 打印整批标签
	codes := make([]string, 0, len(bags))
	for _, b := range bags {
		codes = append(codes, fmt.Sprint(b["code"]))
	}
	payload, _ := json.Marshal(map[string]interface{}{"codes": codes})
	resp, body = e.do("POST", "/api/recv/labels/print", ck, string(payload))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("批量打印应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if numOf(body["count"]) != int64(len(codes)) {
		t.Fatalf("打印条数应 %d，实际 %v", len(codes), body["count"])
	}
	pageURL := fmt.Sprint(body["page_url"])
	resp, page := e.doRaw("GET", pageURL, ck, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, fmt.Sprint(bags[29]["code"])) {
		t.Fatalf("版式页应 200 且含末袋码，status=%d", resp.StatusCode)
	}

	// ⑦ 扫码定位
	resp, body = e.do("GET", "/api/recv/scan?code="+code, ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("扫码定位应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	res, _ := body["result"].(map[string]interface{})
	if res["kind"] != "truck" {
		t.Fatalf("应定位到车次，实际 %v", res)
	}

	// 校验位被读错 ⇒ 400「请重扫」
	bad := code[:26] + nextChar(string(code[26]))
	resp, body = e.do("GET", "/api/recv/scan?code="+bad, ck, "")
	if resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(fmt.Sprint(body["error"]), "请重扫") {
		t.Fatalf("校验位错误应 400 且提示「请重扫」，实际 %d body=%v", resp.StatusCode, body)
	}

	// 别系统的码 ⇒ 400「不是本系统的码」
	resp, body = e.do("GET", "/api/recv/scan?code=NOT-A-JX-CODE-000000000000000", ck, "")
	if resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(fmt.Sprint(body["error"]), "不是本系统的码") {
		t.Fatalf("别系统码应 400 且提示「不是本系统的码」，实际 %d body=%v", resp.StatusCode, body)
	}

	// 格式正确但系统内不存在 ⇒ 404（★ 与「码非法」是两种不同结果）
	resp, body = e.do("GET", "/api/recv/scan?code=1ACG0001000226100703000000L", ck, "")
	if resp.StatusCode != http.StatusNotFound ||
		!strings.Contains(fmt.Sprint(body["error"]), "格式正确但系统内不存在") {
		t.Fatalf("系统内不存在应 404 且提示「格式正确但系统内不存在」，实际 %d body=%v", resp.StatusCode, body)
	}
}

// nextChar 返回字母表中的下一个字符（制造校验位不匹配）。
func nextChar(c string) string {
	const al = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for i := 0; i < len(al); i++ {
		if string(al[i]) == c {
			return string(al[(i+1)%len(al)])
		}
	}
	return "0"
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
