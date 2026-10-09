package httpapi

// ===== M8 追溯 · 接口级 TC（TC-M8-01~05 双层形态的接口侧；库侧见 internal/store/m8_test.go）=====
//
// ★ 账号：receiver+qc+production 并集（收货造车 / 取样建单出结论 / 生产批投料成品 /
//	出货归集 / trace.* = READ）＋ management（紧急放行审批 / trace.* = ALL）。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。
// ★ 上游链路走真实接口（预报→过磅→取样→结论→投料→成品→出货）；
//	检测结果数值行属上游对象夹具 ⇒ 直写库补一行（§5 允许）。
// ★ 口径锚点：正反同走 b_feed_record（A9）；未命中 200+has_flow:false（A10）；
//	反向检测 = 现行单 + is_current（§6-9）；让步判据 conclusion='CONCESSION'（§6-11）；
//	追溯/档案零写业务表（A17）。★ 读入口全挂 trace.* 的 LevelRead（A14）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	m8HTTPOpenID  = "ou_test_m8_http" // receiver + qc + production
	m8HMgmtOpenID = "ou_test_m8_mgmt" // management
	m8HCustCode   = "9415"
	m8HInMatCode  = "9415"
	m8HOutMatCode = "9416"
)

// m8HTTPWipe 清掉 M8 接口测试数据（★ 先子后父、按账号 / 测试客户精确匹配，§6-19）。
func m8HTTPWipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	me := m8HTTPOpenID
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
	q(`DELETE FROM b_shipment_item WHERE shipment_id IN
	       (SELECT id FROM b_shipment WHERE created_by IN (?, ?))`, me, m8HMgmtOpenID)
	q(`DELETE FROM b_shipment WHERE created_by IN (?, ?)`, me, m8HMgmtOpenID)
	q(`DELETE FROM b_fg_bag WHERE fg_lot_id IN (SELECT id FROM b_fg_lot WHERE created_by = ?)`, me)
	q(`DELETE FROM b_fg_lot WHERE created_by = ?`, me)
	q(`DELETE FROM b_feed_record WHERE batch_id IN (SELECT id FROM b_production_batch WHERE created_by = ?)`, me)
	q(`DELETE FROM b_batch_operation WHERE batch_id IN (SELECT id FROM b_production_batch WHERE created_by = ?)`, me)
	q(`DELETE FROM b_production_batch WHERE created_by = ?`, me)
	q(`DELETE FROM b_label_print WHERE created_by = ?`, me)

	q(`DELETE FROM b_inspection_file WHERE inspection_id IN (SELECT id FROM b_inspection WHERE created_by = ?)`, me)
	q(`DELETE FROM b_inspection_result WHERE inspection_id IN (SELECT id FROM b_inspection WHERE created_by = ?)`, me)
	q(`DELETE FROM b_obj_void WHERE entity = 'b_inspection'
	     AND entity_id IN (SELECT id FROM b_inspection WHERE created_by = ?)`, me)
	q(`DELETE FROM b_inspection WHERE created_by = ?`, me)
	q(`DELETE FROM b_sample_destroy WHERE sample_id IN (SELECT id FROM b_sample WHERE created_by = ?)`, me)
	q(`DELETE FROM b_sample_lend WHERE sample_id IN (SELECT id FROM b_sample WHERE created_by = ?)`, me)
	q(`DELETE FROM b_sample_retention WHERE sample_id IN (SELECT id FROM b_sample WHERE created_by = ?)`, me)
	q(`DELETE FROM b_sample WHERE created_by = ?`, me)
	q(`DELETE FROM b_sample_group WHERE created_by = ?`, me)
	q(`DELETE FROM b_obj_void WHERE entity = 'b_bag'
	     AND entity_id IN (SELECT id FROM b_bag WHERE created_by = ?)`, me)
	q(`DELETE FROM b_bag WHERE created_by = ?
	     OR truck_lot_id IN (SELECT id FROM b_truck_lot
	                           WHERE customer_id IN (SELECT id FROM m_customer WHERE code = ?))`,
		me, m8HCustCode)
	q(`DELETE FROM b_truck_lot WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, m8HCustCode)
	q(`DELETE FROM b_arrival_notice WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, m8HCustCode)
	q(`DELETE FROM s_audit_log WHERE actor_open_id IN (?, ?)`, me, m8HMgmtOpenID)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m8HCustCode)
	q(`DELETE FROM m_material WHERE code IN (?, ?) AND created_by LIKE 'ou\_test\_%'`,
		m8HInMatCode, m8HOutMatCode)
}

// m8HTTPEnv 建测试主数据 + 起服务 + 绑角色；返回 env 与主账号 cookie。
func m8HTTPEnv(t *testing.T) (*env, string) {
	t.Helper()
	st := requireDB(t)
	m8HTTPWipe(t, st)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M8接口测试客户', '启用', 1, 1, ?)`, m8HCustCode, m8HTTPOpenID); err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M8接口测试原料', '原料', '启用', 1, 1, ?)`, m8HInMatCode, m8HTTPOpenID); err != nil {
		t.Fatalf("建测试原料失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M8接口测试成品', '成品', '启用', 1, 1, ?)`, m8HOutMatCode, m8HTTPOpenID); err != nil {
		t.Fatalf("建测试成品失败: %v", err)
	}
	t.Cleanup(func() { m8HTTPWipe(t, st) })

	bindRole(t, st, m8HTTPOpenID, "receiver")
	bindRole(t, st, m8HTTPOpenID, "qc")
	bindRole(t, st, m8HTTPOpenID, "production")
	bindRole(t, st, m8HMgmtOpenID, "management")

	e := newEnv(t, time.Hour)
	return e, e.login(m8HTTPOpenID)
}

// m8HMasterIDs 取测试客户 / 原料 / 成品当前行 id。
func m8HMasterIDs(t *testing.T, st *store.Store) (cust, inMat, outMat int64) {
	t.Helper()
	ctx := context.Background()
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_customer WHERE code = ? AND is_current = 1`, m8HCustCode).Scan(&cust); err != nil {
		t.Fatalf("查测试客户失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, m8HInMatCode).Scan(&inMat); err != nil {
		t.Fatalf("查测试原料失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, m8HOutMatCode).Scan(&outMat); err != nil {
		t.Fatalf("查测试成品失败: %v", err)
	}
	return cust, inMat, outMat
}

// m8HConcessionPass 造「让步接收车次」：取样 → 建组 → 建单 → CONCESSION 结论（四字段齐）。
func m8HConcessionPass(t *testing.T, e *env, ck string, truckID int64, bagCode string) {
	t.Helper()
	resp, body := e.do("POST", "/api/sample/take", ck, fmt.Sprintf(`{"code":%q}`, bagCode))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("扫码取样应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	var incID int64
	for _, it := range body["samples"].([]interface{}) {
		m, _ := it.(map[string]interface{})
		if fmt.Sprint(m["role"]) == "份样" {
			incID = numOf(m["id"])
		}
	}
	if incID == 0 {
		t.Fatalf("未取到份样: %v", body)
	}
	m5CreateGroup(t, e, ck, truckID, incID)
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)
	resp, body = e.do("POST", fmt.Sprintf("/api/insp/%d/conclusion", inspID), ck, `{
		"conclusion": "CONCESSION",
		"authorized_by": "质量经理",
		"cust_notified_at": "2026-10-04 09:00:00",
		"cust_contact": "客户质检 王工",
		"cust_channel": "电话"
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("让步结论应 200，实际 %d body=%v", resp.StatusCode, body)
	}
}

// m8HMakeLot 生成成品批 + n 个成品袋，返回 lot 行与袋码数组。
func m8HMakeLot(t *testing.T, e *env, ck string, batchID int64, n int) (map[string]interface{}, []string) {
	t.Helper()
	resp, body := e.do("POST", fmt.Sprintf("/api/prod/batches/%d/fg-lots", batchID), ck,
		`{"pack_spec":"吨袋","net_weight":20}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("生成成品批应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	lot, _ := body["row"].(map[string]interface{})
	if lot == nil {
		t.Fatalf("生成成品批未返回 row: %v", body)
	}
	resp, body = e.do("POST", fmt.Sprintf("/api/prod/fg-lots/%d/bags", numOf(lot["id"])), ck,
		fmt.Sprintf(`{"count":%d}`, n))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("生成成品袋应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	raw, _ := body["rows"].([]interface{})
	codes := make([]string, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]interface{})
		codes = append(codes, fmt.Sprint(m["code"]))
	}
	if len(codes) != n {
		t.Fatalf("应生成 %d 个成品袋，实际 %d", n, len(codes))
	}
	return lot, codes
}

// m8HShip 建出货单（失败即 Fatal），返回 row。
func m8HShip(t *testing.T, e *env, ck string, codes []string) map[string]interface{} {
	t.Helper()
	raw, _ := json.Marshal(map[string]interface{}{"bag_codes": codes, "remark": "M8接口夹具"})
	resp, body := e.do("POST", "/api/ship/shipments", ck, string(raw))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建出货单应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	if row == nil {
		t.Fatalf("建出货单未返回 row: %v", body)
	}
	return row
}

// m8HInspIDByTruck 查车次最新检测单 id。
func m8HInspIDByTruck(t *testing.T, st *store.Store, truckID int64) int64 {
	t.Helper()
	var id int64
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT id FROM b_inspection WHERE target_type = '车次' AND target_id = ?
		 ORDER BY id DESC LIMIT 1`, truckID).Scan(&id); err != nil {
		t.Fatalf("查车次检测单失败: %v", err)
	}
	return id
}

// ===== TC-M8-01 正常：对 A 车正向追溯 =====

func TestTC_M8_01_HTTP_ForwardTrace(t *testing.T) {
	e, ck := m8HTTPEnv(t)
	cust, inMat, outMat := m8HMasterIDs(t, e.st)
	mgmt := e.login(m8HMgmtOpenID)

	truck, bags := m6HTruck(t, e, ck, cust, inMat, 2)
	m6HPass(t, e, ck, truck, fmt.Sprint(bags[0]["code"]))
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := numOf(batch["id"])
	for _, b := range bags {
		m6HFeed(t, e, ck, batchID, fmt.Sprint(b["code"]))
	}
	lot, fgCodes := m8HMakeLot(t, e, ck, batchID, 2)
	ship := m8HShip(t, e, ck, fgCodes)
	shipID := numOf(ship["id"])

	// 车次入口（A8：链路经 b_feed_record）
	resp, body := e.do("GET", fmt.Sprintf("/api/trace/forward?truck_lot_id=%d", truck), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正向追溯应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if body["has_flow"] != true {
		t.Fatalf("★★★ A 车有投料，has_flow 应 true，实际 %v", body["has_flow"])
	}
	batches := body["batches"].([]interface{})
	if len(batches) != 1 || numOf(batches[0].(map[string]interface{})["batch_id"]) != batchID {
		t.Fatalf("★★ 正向生产批应 [%d]，实际 %v", batchID, batches)
	}
	lotOK := false
	for _, it := range body["fg_lots"].([]interface{}) {
		if numOf(it.(map[string]interface{})["fg_lot_id"]) == numOf(lot["id"]) {
			lotOK = true
		}
	}
	if !lotOK {
		t.Fatalf("★★ 正向应含成品批 %v，实际 %v", lot["id"], body["fg_lots"])
	}
	shipOK := false
	for _, it := range body["shipments"].([]interface{}) {
		m := it.(map[string]interface{})
		if numOf(m["shipment_id"]) == shipID {
			shipOK = true
			if fmt.Sprint(m["status"]) != "已出厂" {
				t.Fatalf("出货单状态应「已出厂」，实际 %v", m["status"])
			}
		}
	}
	if !shipOK {
		t.Fatalf("★★ 正向应含出货单 %d，实际 %v", shipID, body["shipments"])
	}
	if numOf(body["source"].(map[string]interface{})["truck_lot_id"]) != truck {
		t.Fatalf("source 应为 A 车 %d，实际 %v", truck, body["source"])
	}

	// 吨袋码入口（同一条 b_feed_record 链）
	resp, body = e.do("GET", "/api/trace/forward?bag_code="+fmt.Sprint(bags[0]["code"]), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("按袋码正向追溯应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if body["has_flow"] != true || len(body["batches"].([]interface{})) != 1 {
		t.Fatalf("★★ 袋码入口应命中同一生产批，实际 %v", body)
	}
	// management（trace.forward=ALL）同样可读
	if resp, body := e.do("GET", fmt.Sprintf("/api/trace/forward?truck_lot_id=%d", truck), mgmt, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("management 正向追溯应 200，实际 %d body=%v", resp.StatusCode, body)
	}
}

// ===== TC-M8-02 ★★ 一致性：正反互为逆 =====

func TestTC_M8_02_HTTP_BackwardInverse(t *testing.T) {
	e, ck := m8HTTPEnv(t)
	cust, inMat, outMat := m8HMasterIDs(t, e.st)

	truckA, bagsA := m6HTruck(t, e, ck, cust, inMat, 2)
	truckB, bagsB := m6HTruck(t, e, ck, cust, inMat, 1)
	m6HPass(t, e, ck, truckA, fmt.Sprint(bagsA[0]["code"]))
	m6HPass(t, e, ck, truckB, fmt.Sprint(bagsB[0]["code"]))
	b1 := m6HBatch(t, e, ck, cust, inMat, outMat)
	b2 := m6HBatch(t, e, ck, cust, inMat, outMat)
	id1, id2 := numOf(b1["id"]), numOf(b2["id"])
	m6HFeed(t, e, ck, id1, fmt.Sprint(bagsA[0]["code"])) // A → 批 1
	m6HFeed(t, e, ck, id2, fmt.Sprint(bagsA[1]["code"])) // A → 批 2（一车拆多批）
	m6HFeed(t, e, ck, id2, fmt.Sprint(bagsB[0]["code"])) // B → 批 2
	lot1, _ := m8HMakeLot(t, e, ck, id1, 1)
	lot2, _ := m8HMakeLot(t, e, ck, id2, 1)

	// A → 正向
	resp, body := e.do("GET", fmt.Sprintf("/api/trace/forward?truck_lot_id=%d", truckA), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正向追溯应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if body["has_flow"] != true || len(body["batches"].([]interface{})) != 2 {
		t.Fatalf("★★ A 车正向应 2 个生产批，实际 %v", body["batches"])
	}
	fwdBatchIDs := map[int64]bool{}
	for _, it := range body["batches"].([]interface{}) {
		fwdBatchIDs[numOf(it.(map[string]interface{})["batch_id"])] = true
	}
	if !fwdBatchIDs[id1] || !fwdBatchIDs[id2] {
		t.Fatalf("★★ 正向缺生产批：%v（应含 %d、%d）", fwdBatchIDs, id1, id2)
	}

	// → B：对正向结果中的成品批反向 ⇒ 必须能追回 A 车（A→B→A 回环）
	for _, lotID := range []int64{numOf(lot1["id"]), numOf(lot2["id"])} {
		resp, back := e.do("GET", fmt.Sprintf("/api/trace/backward?fg_lot_id=%d", lotID), ck, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("反向追溯应 200，实际 %d body=%v", resp.StatusCode, back)
		}
		if back["has_flow"] != true {
			t.Fatalf("★★★ 反向应有流向（has_flow），实际 %v", back["has_flow"])
		}
		trucks := map[int64]bool{}
		for _, it := range back["feeds"].([]interface{}) {
			trucks[numOf(it.(map[string]interface{})["truck_lot_id"])] = true
		}
		if !trucks[truckA] {
			t.Fatalf("★★★ 正反互为逆被破坏：成品批 %d 反向追不回 A 车 %d（feeds=%v）",
				lotID, truckA, back["feeds"])
		}
		// 反向批集合 ⊆ 正向批集合（同一张 b_feed_record）
		bs := back["batches"].([]interface{})
		if len(bs) != 1 {
			t.Fatalf("反向应 1 个来源批，实际 %v", bs)
		}
		if got := numOf(bs[0].(map[string]interface{})["batch_id"]); !fwdBatchIDs[got] {
			t.Fatalf("★★ 反向批 %d 不在正向集合内", got)
		}
	}
	// fg_code 入口同样命中
	resp, back := e.do("GET", "/api/trace/backward?fg_code="+fmt.Sprint(lot1["code"]), ck, "")
	if resp.StatusCode != http.StatusOK || back["has_flow"] != true {
		t.Fatalf("fg_code 入口应 200 且有流向，实际 %d %v", resp.StatusCode, back)
	}
}

// ===== TC-M8-03 边界：未被投料的车 ⇒ 200 + 空 + has_flow:false =====

func TestTC_M8_03_HTTP_ForwardNoFlow(t *testing.T) {
	e, ck := m8HTTPEnv(t)
	cust, inMat, _ := m8HMasterIDs(t, e.st)

	truck, bags := m6HTruck(t, e, ck, cust, inMat, 1) // 未投料
	cases := []struct {
		name string
		path string
	}{
		{"未投料的车", fmt.Sprintf("/api/trace/forward?truck_lot_id=%d", truck)},
		{"未投料的袋", "/api/trace/forward?bag_code=" + fmt.Sprint(bags[0]["code"])},
		{"不存在的车", "/api/trace/forward?truck_lot_id=99999999"},
	}
	for _, tc := range cases {
		resp, body := e.do("GET", tc.path, ck, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("★★★ %s：无流向不得报错（应 200，不是 404/500），实际 %d body=%v",
				tc.name, resp.StatusCode, body)
		}
		if body["has_flow"] != false {
			t.Fatalf("★★★ %s：has_flow 应 false，实际 %v", tc.name, body["has_flow"])
		}
		for _, key := range []string{"batches", "fg_lots", "shipments"} {
			if rows, ok := body[key].([]interface{}); !ok || len(rows) != 0 {
				t.Fatalf("★★★ %s：数组 %s 应为空，实际 %v", tc.name, key, body[key])
			}
		}
	}
	// 入参缺失 ⇒ 400（这是入参错，不是「无流向」）
	if resp, body := e.do("GET", "/api/trace/forward", ck, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺入参应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	if resp, body := e.do("GET", "/api/trace/forward?truck_lot_id=abc", ck, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 truck_lot_id 应 400，实际 %d body=%v", resp.StatusCode, body)
	}
}

// ===== TC-M8-04 正常：反向追溯含当时检测结果 =====

func TestTC_M8_04_HTTP_BackwardInspection(t *testing.T) {
	e, ck := m8HTTPEnv(t)
	cust, inMat, outMat := m8HMasterIDs(t, e.st)
	mgmt := e.login(m8HMgmtOpenID)

	// 车 1：合格结论 + 一条检测数值（直插夹具结果行，§5）
	truck1, bags1 := m6HTruck(t, e, ck, cust, inMat, 1)
	m6HPass(t, e, ck, truck1, fmt.Sprint(bags1[0]["code"]))
	inspID := m8HInspIDByTruck(t, e.st, truck1)
	if _, err := e.st.DB().ExecContext(context.Background(), `
INSERT INTO b_inspection_result (inspection_id, item_id, state, value_num, unit, judge, created_by)
VALUES (?,?, '已测', 12.345, '%', '合格', ?)`, inspID, 999999, m8HTTPOpenID); err != nil {
		t.Fatalf("造检测数值失败: %v", err)
	}

	// 车 2：无结论，凭生效紧急放行投料（qc 发起 / management 审批，两人不同）
	truck2, bags2 := m6HTruck(t, e, ck, cust, inMat, 1)
	if resp, body := e.do("POST", "/api/insp/urgent-release/init", ck,
		fmt.Sprintf(`{"entity":"b_truck_lot","entity_id":%d,"reason":"料急"}`, truck2)); resp.StatusCode != http.StatusOK {
		t.Fatalf("发起紧急放行应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if resp, body := e.do("POST", "/api/insp/urgent-release/approve", mgmt,
		fmt.Sprintf(`{"entity":"b_truck_lot","entity_id":%d,"reason":"同意"}`, truck2)); resp.StatusCode != http.StatusOK {
		t.Fatalf("审批紧急放行应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := numOf(batch["id"])
	m6HFeed(t, e, ck, batchID, fmt.Sprint(bags1[0]["code"]))
	m6HFeed(t, e, ck, batchID, fmt.Sprint(bags2[0]["code"]))
	lot, _ := m8HMakeLot(t, e, ck, batchID, 1)

	resp, body := e.do("GET", fmt.Sprintf("/api/trace/backward?fg_lot_id=%d", numOf(lot["id"])), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("反向追溯应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	feeds := body["feeds"].([]interface{})
	if len(feeds) != 2 {
		t.Fatalf("反向投料应 2 条，实际 %d", len(feeds))
	}
	f0 := feeds[0].(map[string]interface{})
	if numOf(f0["truck_lot_id"]) != truck1 {
		t.Fatalf("投料顺序错乱: %v", feeds)
	}
	insp0, _ := f0["inspection"].(map[string]interface{})
	if insp0 == nil {
		t.Fatalf("★★★ 投料必须带车次检测结果")
	}
	if insp0["is_current"] != true {
		t.Fatalf("★★★ 必须显式标注 is_current=true（现行单，非历史快照），实际 %v", insp0["is_current"])
	}
	if fmt.Sprint(insp0["conclusion"]) != "合格" {
		t.Fatalf("结论应「合格」，实际 %v", insp0["conclusion"])
	}
	if fmt.Sprint(insp0["inspection_no"]) == "" {
		t.Fatalf("★★ 检测单号应非空")
	}
	hasVal := false
	for _, it := range insp0["results"].([]interface{}) {
		r := it.(map[string]interface{})
		if v, ok := r["value_num"].(float64); ok && v > 12.34 && v < 12.36 {
			hasVal = true
		}
	}
	if !hasVal {
		t.Fatalf("★★★ 检测数值应一并列出，实际 %+v", insp0["results"])
	}
	if insp0["urgent_release"] == true {
		t.Fatalf("★ 合格车次不应标紧急放行")
	}

	// 车 2：无现行单，凭生效紧急放行 ⇒ urgent_release + 两笔留痕
	f1 := feeds[1].(map[string]interface{})
	if numOf(f1["truck_lot_id"]) != truck2 {
		t.Fatalf("投料顺序错乱: %v", feeds)
	}
	insp1, _ := f1["inspection"].(map[string]interface{})
	if insp1 == nil {
		t.Fatalf("★★ 紧急放行车次也应带 inspection 对象")
	}
	if insp1["urgent_release"] != true {
		t.Fatalf("★★★ 生效的紧急放行应标 urgent_release=true，实际 %v", insp1)
	}
	records, _ := insp1["urgent_records"].([]interface{})
	if len(records) != 2 {
		t.Fatalf("★★ 紧急放行应附 init/approve 两笔留痕，实际 %d", len(records))
	}
	acts := map[string]bool{}
	for _, it := range records {
		acts[fmt.Sprint(it.(map[string]interface{})["action"])] = true
	}
	if !acts["urgent_release_init"] || !acts["urgent_release_approve"] {
		t.Fatalf("★ 留痕动作应含 init+approve，实际 %+v", acts)
	}
}

// ===== TC-M8-05 边界：让步接收料产出的成品查档案 =====

func TestTC_M8_05_HTTP_BatchArchiveConcession(t *testing.T) {
	e, ck := m8HTTPEnv(t)
	cust, inMat, outMat := m8HMasterIDs(t, e.st)

	// 让步车次 → 投料
	truck, bags := m6HTruck(t, e, ck, cust, inMat, 1)
	m8HConcessionPass(t, e, ck, truck, fmt.Sprint(bags[0]["code"]))
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := numOf(batch["id"])
	m6HFeed(t, e, ck, batchID, fmt.Sprint(bags[0]["code"]))
	// 作业段（A13 六块之一）
	if resp, body := e.do("POST", fmt.Sprintf("/api/prod/batches/%d/operations", batchID), ck,
		`{"operator":"甲班","start_at":"2026-10-04 08:00:00","end_at":"2026-10-04 12:00:00","output_weight":1.5}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("记作业段应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	lot, fgCodes := m8HMakeLot(t, e, ck, batchID, 1)
	ship := m8HShip(t, e, ck, fgCodes)
	shipID := numOf(ship["id"])

	resp, arch := e.do("GET", fmt.Sprintf("/api/trace/batch/%d", batchID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("读批次档案应 200，实际 %d body=%v", resp.StatusCode, arch)
	}

	// ★ 让步标注（A12 / §6-11）
	if arch["concession_used"] != true {
		t.Fatalf("★★★ 让步接收料产出的档案必须 concession_used=true，实际 %v", arch["concession_used"])
	}
	found := false
	for _, it := range arch["concession_sources"].([]interface{}) {
		c := it.(map[string]interface{})
		if numOf(c["truck_lot_id"]) == truck &&
			fmt.Sprint(c["inspection_no"]) != "" &&
			fmt.Sprint(c["conclusion"]) == "CONCESSION" {
			found = true
		}
	}
	if !found {
		t.Fatalf("★★ 让步来源应列出车次与现行单，实际 %+v", arch["concession_sources"])
	}

	// 六块齐（A13）：批 / 投料 / 作业段 / 成品批+袋 / 出货单 / 标注
	b, _ := arch["batch"].(map[string]interface{})
	if b == nil || numOf(b["batch_id"]) != batchID {
		t.Fatalf("批基本信息错乱: %v", arch["batch"])
	}
	if fmt.Sprint(arch["customer_name"]) == "" {
		t.Fatalf("档案应带客户名")
	}
	if rows, ok := arch["feeds"].([]interface{}); !ok || len(rows) != 1 {
		t.Fatalf("投料明细错乱: %v", arch["feeds"])
	}
	if rows, ok := arch["operations"].([]interface{}); !ok || len(rows) != 1 {
		t.Fatalf("作业段错乱: %v", arch["operations"])
	}
	fgLots, ok := arch["fg_lots"].([]interface{})
	if !ok || len(fgLots) != 1 {
		t.Fatalf("成品批错乱: %v", arch["fg_lots"])
	}
	fg0, _ := fgLots[0].(map[string]interface{})
	if bags, ok := fg0["bags"].([]interface{}); !ok || len(bags) != 1 {
		t.Fatalf("成品袋错乱: %v", fg0["bags"])
	}
	_ = lot
	shipOK := false
	for _, it := range arch["shipments"].([]interface{}) {
		if numOf(it.(map[string]interface{})["shipment_id"]) == shipID {
			shipOK = true
		}
	}
	if !shipOK {
		t.Fatalf("★ 档案应含出货单 %d，实际 %+v", shipID, arch["shipments"])
	}

	// 反例：纯合格料的批 ⇒ 无让步标注
	truck2, bags2 := m6HTruck(t, e, ck, cust, inMat, 1)
	m6HPass(t, e, ck, truck2, fmt.Sprint(bags2[0]["code"]))
	batch2 := m6HBatch(t, e, ck, cust, inMat, outMat)
	batch2ID := numOf(batch2["id"])
	m6HFeed(t, e, ck, batch2ID, fmt.Sprint(bags2[0]["code"]))
	resp, arch2 := e.do("GET", fmt.Sprintf("/api/trace/batch/%d", batch2ID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("读批次档案 2 应 200，实际 %d body=%v", resp.StatusCode, arch2)
	}
	if arch2["concession_used"] != false {
		t.Fatalf("★★ 合格料的批不应标让步: %v", arch2["concession_sources"])
	}
}

// ===== A17（接口侧）· 追溯零写业务表 =====

func TestM8HTTPTraceZeroWrite(t *testing.T) {
	e, ck := m8HTTPEnv(t)
	cust, inMat, outMat := m8HMasterIDs(t, e.st)

	truck, bags := m6HTruck(t, e, ck, cust, inMat, 1)
	m6HPass(t, e, ck, truck, fmt.Sprint(bags[0]["code"]))
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := numOf(batch["id"])
	m6HFeed(t, e, ck, batchID, fmt.Sprint(bags[0]["code"]))
	lot, _ := m8HMakeLot(t, e, ck, batchID, 1)

	counts := func() (shipN, itemN, auditN, feedN int) {
		t.Helper()
		for _, q := range []struct {
			sql string
			dst *int
		}{
			{`SELECT COUNT(*) FROM b_shipment`, &shipN},
			{`SELECT COUNT(*) FROM b_shipment_item`, &itemN},
			{`SELECT COUNT(*) FROM s_audit_log`, &auditN},
			{`SELECT COUNT(*) FROM b_feed_record`, &feedN},
		} {
			if err := e.st.DB().QueryRowContext(context.Background(), q.sql).Scan(q.dst); err != nil {
				t.Fatalf("统计失败: %v", err)
			}
		}
		return
	}
	s0, i0, a0, f0 := counts()

	for _, path := range []string{
		fmt.Sprintf("/api/trace/forward?truck_lot_id=%d", truck),
		fmt.Sprintf("/api/trace/backward?fg_lot_id=%d", numOf(lot["id"])),
		fmt.Sprintf("/api/trace/batch/%d", batchID),
	} {
		if resp, body := e.do("GET", path, ck, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s 应 200，实际 %d body=%v", path, resp.StatusCode, body)
		}
	}

	s1, i1, a1, f1 := counts()
	if s1 != s0 || i1 != i0 || a1 != a0 || f1 != f0 {
		t.Fatalf("★★★ M8 零写被破坏：ship %d→%d item %d→%d audit %d→%d feed %d→%d",
			s0, s1, i0, i1, a0, a1, f0, f1)
	}
}

// ===== A14（接口侧）· trace.* 读入口级别核对 =====

func TestM8HTTPPermLevels(t *testing.T) {
	e, ck := m8HTTPEnv(t)

	// 未登录 ⇒ 401
	if resp, _ := e.do("GET", "/api/trace/forward?truck_lot_id=1", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录正向追溯应 401，实际 %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/api/trace/backward?fg_lot_id=1", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录反向追溯应 401，实际 %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/api/trace/batch/1", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录读档案应 401，实际 %d", resp.StatusCode)
	}
	// receiver（trace.* = READ）三个读入口全部放行（200 或业务 404，★ 不得 403）
	checks := []string{
		"/api/trace/forward?truck_lot_id=1",
		"/api/trace/backward?fg_lot_id=1",
		"/api/trace/batch/1",
	}
	for _, path := range checks {
		resp, body := e.do("GET", path, ck, "")
		if resp.StatusCode == http.StatusForbidden {
			t.Fatalf("★★ receiver 读 %s 不应 403（LevelRead 应放行），实际 %d body=%v",
				path, resp.StatusCode, body)
		}
	}
}
