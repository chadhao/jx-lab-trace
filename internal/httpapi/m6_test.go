package httpapi

// ===== M6 生产与谱系 · 接口级 TC（TC-M6-01~09 双层形态的接口侧）=====
//
// ★ 账号：production（6 个 prod.* 写入全 ALL、prod.rework = INIT）
//	＋ receiver（收货链路造车造袋）＋ qc（取样 / 建单 / 出结论 / 紧急放行发起）
//	＋ management（紧急放行审批；★ 对 prod.rework 只有 READ ⇒ 发起返工应 403，
//	  用来锁死「LevelInit 不是 LevelAll」这条口径）。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。
// ★ 夹具纪律（§6-25）：清理**按账号精确匹配**、**先子表后父表**；
//	断言限定本用例作用域，不全库 COUNT(*)。
// ★ 库侧 9 条 TC 见 internal/store/m6_test.go（双层形态）。

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/codec"
	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	m6HTTPOpenID  = "ou_test_m6_http" // production + receiver + qc
	m6MgmtOpenID  = "ou_test_m6_mgmt" // management
	m6HCustCode   = "9407"
	m6HInMatCode  = "9407" // 原料
	m6HOutMatCode = "9408" // 成品
	m6HDate       = "2026-10-01"
	m6HDateSeg    = "261001"
)

// m6HTTPWipe 清掉 M6 接口测试数据（★ 先子后父、按账号 / 测试客户精确匹配）。
func m6HTTPWipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	me := m6HTTPOpenID
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
	q(`DELETE FROM b_rework WHERE new_batch_id IN (SELECT id FROM b_production_batch WHERE created_by = ?)
	       OR src_batch_id IN (SELECT id FROM b_production_batch WHERE created_by = ?)`, me, me)
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
		me, m6HCustCode)
	q(`DELETE FROM b_truck_lot WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, m6HCustCode)
	q(`DELETE FROM b_arrival_notice WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, m6HCustCode)
	q(`DELETE FROM s_audit_log WHERE actor_open_id IN (?, ?)`, me, m6MgmtOpenID)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m6HCustCode)
	q(`DELETE FROM m_material WHERE code IN (?, ?) AND created_by LIKE 'ou\_test\_%'`,
		m6HInMatCode, m6HOutMatCode)
}

// m6HTTPEnv 建测试主数据 + 起服务 + 绑角色；返回 env 与 production 主账号 cookie。
func m6HTTPEnv(t *testing.T) (*env, string) {
	t.Helper()
	st := requireDB(t)
	m6HTTPWipe(t, st)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M6接口测试客户', '启用', 1, 1, ?)`, m6HCustCode, m6HTTPOpenID); err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M6接口测试原料', '原料', '启用', 1, 1, ?)`, m6HInMatCode, m6HTTPOpenID); err != nil {
		t.Fatalf("建测试原料失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M6接口测试成品', '成品', '启用', 1, 1, ?)`, m6HOutMatCode, m6HTTPOpenID); err != nil {
		t.Fatalf("建测试成品失败: %v", err)
	}
	t.Cleanup(func() { m6HTTPWipe(t, st) })

	bindRole(t, st, m6HTTPOpenID, "production")
	bindRole(t, st, m6HTTPOpenID, "receiver")
	bindRole(t, st, m6HTTPOpenID, "qc")
	bindRole(t, st, m6MgmtOpenID, "management")

	e := newEnv(t, time.Hour)
	return e, e.login(m6HTTPOpenID)
}

// m6MasterIDs 取测试客户 / 原料 / 成品的当前行 id。
func m6MasterIDs(t *testing.T, st *store.Store) (int64, int64, int64) {
	t.Helper()
	ctx := context.Background()
	var cust, inMat, outMat int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_customer WHERE code = ? AND is_current = 1`, m6HCustCode).Scan(&cust); err != nil {
		t.Fatalf("查测试客户失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, m6HInMatCode).Scan(&inMat); err != nil {
		t.Fatalf("查测试原料失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, m6HOutMatCode).Scan(&outMat); err != nil {
		t.Fatalf("查测试成品失败: %v", err)
	}
	return cust, inMat, outMat
}

// m6HBatch 建一个生产批（固定链根日期，可重复执行）。
func m6HBatch(t *testing.T, e *env, ck string, custID, inMatID, outMatID int64) map[string]interface{} {
	t.Helper()
	resp, body := e.do("POST", "/api/prod/batches", ck, fmt.Sprintf(
		`{"customer_id":%d,"input_material_id":%d,"planned_output_material_id":%d,"batch_date":%q,"remark":"M6夹具"}`,
		custID, inMatID, outMatID, m6HDate))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建生产批应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	if row == nil {
		t.Fatalf("建批未返回 row: %v", body)
	}
	return row
}

// m6HTruck 造车 + 袋（走 M3 接口链路），返回车 id 与袋行。
func m6HTruck(t *testing.T, e *env, ck string, custID, matID int64, bags int) (int64, []map[string]interface{}) {
	t.Helper()
	truck := m3ArriveViaHTTP(t, e, ck, custID, matID, bags)
	id := int64(numOf(truck["id"]))
	return id, m3WeighAndBags(t, e, ck, id, "35000", "5000", bags)
}

// m6HPass 造「合格车次」：取样 → 建组 → 建单 → 出合格结论（全走接口）。
func m6HPass(t *testing.T, e *env, ck string, truckID int64, bagCode string) {
	t.Helper()
	resp, body := e.do("POST", "/api/sample/take", ck, fmt.Sprintf(`{"code":%q}`, bagCode))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("扫码取样应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	samples, _ := body["samples"].([]interface{})
	var incID int64
	for _, it := range samples {
		m, _ := it.(map[string]interface{})
		if fmt.Sprint(m["role"]) == "份样" {
			incID = numOf(m["id"])
		}
	}
	if incID == 0 {
		t.Fatalf("未取到份样: %v", body)
	}
	m5CreateGroup(t, e, ck, truckID, incID)

	resp, body = e.do("POST", "/api/insp", ck,
		fmt.Sprintf(`{"target_type":"车次","target_id":%d}`, truckID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建检测单应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	insp, _ := body["row"].(map[string]interface{})
	resp, body = e.do("POST", fmt.Sprintf("/api/insp/%d/conclusion", numOf(insp["id"])), ck,
		`{"conclusion":"合格"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("出合格结论应 200，实际 %d body=%v", resp.StatusCode, body)
	}
}

// m6HFeed 投一袋（失败即 Fatal）。
func m6HFeed(t *testing.T, e *env, ck string, batchID int64, bagCode string) map[string]interface{} {
	t.Helper()
	resp, body := e.do("POST", fmt.Sprintf("/api/prod/batches/%d/feeds", batchID), ck,
		fmt.Sprintf(`{"bag_code":%q,"feed_weight":1.25,"operator":"投料工"}`, bagCode))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("投料应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	return row
}

// m6HFeeds 查某批投料行。
func m6HFeeds(t *testing.T, e *env, ck string, batchID int64) []map[string]interface{} {
	t.Helper()
	resp, body := e.do("GET", fmt.Sprintf("/api/prod/batches/%d/feeds", batchID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查投料明细应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	raw, _ := body["rows"].([]interface{})
	out := make([]map[string]interface{}, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]interface{})
		out = append(out, m)
	}
	return out
}

// TC-M6-01 正常：建生产批 ⇒ 码形如 1C-CG-9407-9408-261001-01-000-000-X。
func TestTC_M6_01_HTTP_BatchCodeShape(t *testing.T) {
	e, ck := m6HTTPEnv(t)
	cust, inMat, outMat := m6MasterIDs(t, e.st)

	row := m6HBatch(t, e, ck, cust, inMat, outMat)
	code, _ := row["code"].(string)
	p, err := codec.Parse(code)
	if err != nil {
		t.Fatalf("批码不可解析: %v", err)
	}
	if p.Seg.T != "C" || p.Seg.BT != "CG" || p.Seg.Customer != m6HCustCode ||
		p.Seg.Material != m6HOutMatCode || p.Seg.Date != m6HDateSeg ||
		p.Seg.SEQ1 != "01" || p.Seg.SEQ2 != "000" || p.Seg.SEQ3 != "000" {
		t.Fatalf("★★ 批码段位错乱: %+v", p.Seg)
	}
	if fmt.Sprint(row["status"]) != "进行中" {
		t.Fatalf("状态应 进行中，实际 %v", row["status"])
	}
	// 读入口（READ 级）：management 对 prod.batch.create 是 READ ⇒ GET 200（A12 级别核对）
	mgmt := e.login(m6MgmtOpenID)
	if resp, body := e.do("GET", "/api/prod/batches", mgmt, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("management 读生产批应 200，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TC-M6-02 ★ 正常：扫 3 个吨袋码 ⇒ 投料 3 行且关联到该批。
func TestTC_M6_02_HTTP_FeedThreeBags(t *testing.T) {
	e, ck := m6HTTPEnv(t)
	cust, inMat, outMat := m6MasterIDs(t, e.st)
	truck, bags := m6HTruck(t, e, ck, cust, inMat, 3)
	m6HPass(t, e, ck, truck, bags[0]["code"].(string))
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := int64(numOf(batch["id"]))

	for _, b := range bags {
		m6HFeed(t, e, ck, batchID, b["code"].(string))
	}
	rows := m6HFeeds(t, e, ck, batchID)
	if len(rows) != 3 {
		t.Fatalf("★★ 投料应 3 行，实际 %d", len(rows))
	}
	for _, r := range rows {
		if numOf(r["batch_id"]) != batchID {
			t.Fatalf("★ 投料行 batch_id 应 %d，实际 %v", batchID, r["batch_id"])
		}
	}
	// 重复投同一袋 ⇒ 409
	resp, body := e.do("POST", fmt.Sprintf("/api/prod/batches/%d/feeds", batchID), ck,
		fmt.Sprintf(`{"bag_code":%q}`, bags[0]["code"]))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复投料应 409，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TC-M6-03 ★ 异常：「待检」（无结论）车次的袋投料 ⇒ 拒绝（409），库中无新增行。
func TestTC_M6_03_HTTP_FeedRejectedWithoutConclusion(t *testing.T) {
	e, ck := m6HTTPEnv(t)
	cust, inMat, outMat := m6MasterIDs(t, e.st)
	_, bags := m6HTruck(t, e, ck, cust, inMat, 1)
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := int64(numOf(batch["id"]))

	resp, body := e.do("POST", fmt.Sprintf("/api/prod/batches/%d/feeds", batchID), ck,
		fmt.Sprintf(`{"bag_code":%q}`, bags[0]["code"]))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("★★ 待检袋投料应 409，实际 %d body=%v", resp.StatusCode, body)
	}
	if rows := m6HFeeds(t, e, ck, batchID); len(rows) != 0 {
		t.Fatalf("被拒后投料行应 0，实际 %d", len(rows))
	}
}

// TC-M6-04 边界：紧急放行（init+approve 两人）⇒ 允许；只有 init ⇒ 仍拒。
func TestTC_M6_04_HTTP_FeedUrgentRelease(t *testing.T) {
	e, ck := m6HTTPEnv(t)
	cust, inMat, outMat := m6MasterIDs(t, e.st)
	mgmt := e.login(m6MgmtOpenID)
	truck, bags := m6HTruck(t, e, ck, cust, inMat, 1)
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := int64(numOf(batch["id"]))

	// 发起（qc 的 INIT 级）
	if resp, body := e.do("POST", "/api/insp/urgent-release/init", ck,
		fmt.Sprintf(`{"entity":"b_truck_lot","entity_id":%d,"reason":"料急先行"}`, truck)); resp.StatusCode != http.StatusOK {
		t.Fatalf("发起紧急放行应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	// 只有 init ⇒ 拒
	if resp, body := e.do("POST", fmt.Sprintf("/api/prod/batches/%d/feeds", batchID), ck,
		fmt.Sprintf(`{"bag_code":%q}`, bags[0]["code"])); resp.StatusCode != http.StatusConflict {
		t.Fatalf("★★ 只有 init 应 409，实际 %d body=%v", resp.StatusCode, body)
	}
	// 另一人审批 ⇒ 生效 ⇒ 允许
	if resp, body := e.do("POST", "/api/insp/urgent-release/approve", mgmt,
		fmt.Sprintf(`{"entity":"b_truck_lot","entity_id":%d,"reason":"同意"}`, truck)); resp.StatusCode != http.StatusOK {
		t.Fatalf("审批紧急放行应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	m6HFeed(t, e, ck, batchID, bags[0]["code"].(string))
	if rows := m6HFeeds(t, e, ck, batchID); len(rows) != 1 {
		t.Fatalf("生效后投料应 1 行，实际 %d", len(rows))
	}
}

// TC-M6-05 ★ 多对多：A 车料投两批、B 车料并入批 2 ⇒ 谱系如实记录。
func TestTC_M6_05_HTTP_GenealogyManyToMany(t *testing.T) {
	e, ck := m6HTTPEnv(t)
	cust, inMat, outMat := m6MasterIDs(t, e.st)
	truckA, bagsA := m6HTruck(t, e, ck, cust, inMat, 2)
	truckB, bagsB := m6HTruck(t, e, ck, cust, inMat, 2)
	m6HPass(t, e, ck, truckA, bagsA[0]["code"].(string))
	m6HPass(t, e, ck, truckB, bagsB[0]["code"].(string))

	b1 := m6HBatch(t, e, ck, cust, inMat, outMat)
	b2 := m6HBatch(t, e, ck, cust, inMat, outMat)
	id1, id2 := int64(numOf(b1["id"])), int64(numOf(b2["id"]))

	m6HFeed(t, e, ck, id1, bagsA[0]["code"].(string)) // A → 批 1
	m6HFeed(t, e, ck, id2, bagsA[1]["code"].(string)) // A → 批 2
	m6HFeed(t, e, ck, id2, bagsB[0]["code"].(string)) // B → 批 2

	if n := len(m6HFeeds(t, e, ck, id1)); n != 1 {
		t.Fatalf("批 1 投料应 1 行，实际 %d", n)
	}
	if n := len(m6HFeeds(t, e, ck, id2)); n != 2 {
		t.Fatalf("★★ 批 2 投料应 2 行（两台车的袋），实际 %d", n)
	}
	// 反向谱系：袋码 ⇒ 去向（多对多如实显示）
	resp, body := e.do("GET", "/api/prod/genealogy/bags/"+bagsA[1]["code"].(string), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("谱系反查应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	feeds, _ := row["feeds"].([]interface{})
	if len(feeds) != 1 {
		t.Fatalf("A 车第 2 袋去向应 1 条，实际 %d", len(feeds))
	}
}

// TC-M6-06 正常：一个批记 2 段作业（两个班组）⇒ 2 条作业段，批仍为 1 个。
func TestTC_M6_06_HTTP_TwoOperations(t *testing.T) {
	e, ck := m6HTTPEnv(t)
	cust, inMat, outMat := m6MasterIDs(t, e.st)
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := int64(numOf(batch["id"]))

	for i, body := range []string{
		`{"team_id":11,"operator":"甲班","start_at":"2026-10-01 08:00:00","end_at":"2026-10-01 12:00:00","output_weight":1.5}`,
		`{"team_id":22,"operator":"乙班","start_at":"2026-10-01 13:00:00","end_at":"2026-10-01 18:00:00"}`,
	} {
		resp, out := e.do("POST", fmt.Sprintf("/api/prod/batches/%d/operations", batchID), ck, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 段作业应 200，实际 %d body=%v", i+1, resp.StatusCode, out)
		}
	}
	resp, out := e.do("GET", fmt.Sprintf("/api/prod/batches/%d/operations", batchID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查作业段应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	rows, _ := out["rows"].([]interface{})
	if len(rows) != 2 {
		t.Fatalf("★★ 应 2 条作业段，实际 %d", len(rows))
	}
	if numOf(out["count"]) != 2 {
		t.Fatalf("count 应 2，实际 %v", out["count"])
	}
	// 批仍为 1 个（本作用域）
	resp, out = e.do("GET", "/api/prod/batches", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查生产批列表应 200，实际 %d", resp.StatusCode)
	}
	all, _ := out["rows"].([]interface{})
	if len(all) != 1 {
		t.Fatalf("★★ 跨班组记多段，批仍应 1 个，实际 %d", len(all))
	}
}

// TC-M6-07 ★ 正常：成品批码序1 = 来源生产批序、日期段 = 生产批 batch_date。
func TestTC_M6_07_HTTP_FgLotCodeSeq1AndDate(t *testing.T) {
	e, ck := m6HTTPEnv(t)
	cust, inMat, outMat := m6MasterIDs(t, e.st)
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := int64(numOf(batch["id"]))

	resp, body := e.do("POST", fmt.Sprintf("/api/prod/batches/%d/fg-lots", batchID), ck,
		`{"pack_spec":"吨袋","net_weight":20}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("生成成品批应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	lot, _ := body["row"].(map[string]interface{})
	lotID := int64(numOf(lot["id"]))
	code, _ := lot["code"].(string)
	p, err := codec.Parse(code)
	if err != nil {
		t.Fatalf("成品批码不可解析: %v", err)
	}
	bp, err := codec.Parse(fmt.Sprint(batch["code"]))
	if err != nil {
		t.Fatalf("生产批码不可解析: %v", err)
	}
	if p.Seg.T != "D" || p.Seg.SEQ1 != bp.Seg.SEQ1 || p.Seg.SEQ2 != "001" {
		t.Fatalf("★★ 成品批码段位错乱: %+v（批 %+v）", p.Seg, bp.Seg)
	}
	if p.Seg.Date != m6HDateSeg {
		t.Fatalf("★★ 日期段应为生产批 batch_date %s，实际 %s", m6HDateSeg, p.Seg.Date)
	}
	// 物料不一致 ⇒ 400
	if resp, body := e.do("POST", fmt.Sprintf("/api/prod/batches/%d/fg-lots", batchID), ck,
		fmt.Sprintf(`{"output_material_id":%d}`, inMat)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★ 物料不一致应 400，实际 %d body=%v", resp.StatusCode, body)
	}

	// 成品袋：段位全部继承
	resp, body = e.do("POST", fmt.Sprintf("/api/prod/fg-lots/%d/bags", lotID), ck, `{"count":3}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("生成成品袋应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	rawRows, _ := body["rows"].([]interface{})
	if len(rawRows) != 3 {
		t.Fatalf("应生成 3 个成品袋，实际 %d", len(rawRows))
	}
	for i, r := range rawRows {
		m, _ := r.(map[string]interface{})
		ep, err := codec.Parse(fmt.Sprint(m["code"]))
		if err != nil {
			t.Fatalf("成品袋码不可解析: %v", err)
		}
		if ep.Seg.T != "E" || ep.Seg.SEQ1 != bp.Seg.SEQ1 || ep.Seg.SEQ2 != "001" ||
			ep.Seg.SEQ3 != fmt.Sprintf("%03d", i+1) || ep.Seg.Date != m6HDateSeg {
			t.Fatalf("★★ 成品袋码段位错乱（第 %d 袋）: %+v", i+1, ep.Seg)
		}
	}
	// 打印留痕 + 补打
	resp, body = e.do("POST", fmt.Sprintf("/api/prod/fg-lots/%d/print", lotID), ck, `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("打印应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if resp, body := e.do("POST", fmt.Sprintf("/api/prod/fg-lots/%d/print", lotID), ck, `{}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复打印应 409，实际 %d body=%v", resp.StatusCode, body)
	}
	if resp, body := e.do("POST", fmt.Sprintf("/api/prod/fg-lots/%d/print", lotID), ck,
		`{"bag_seqs":[1],"is_reprint":true}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("★ 补打缺原因应 409，实际 %d body=%v", resp.StatusCode, body)
	}
	if resp, body := e.do("POST", fmt.Sprintf("/api/prod/fg-lots/%d/print", lotID), ck,
		`{"bag_seqs":[1],"is_reprint":true,"reason":"标签破损"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("补打应 200，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TC-M6-08 正常：返工 ⇒ 新批号（独立取号、日期段 = 当日）+ b_rework 关联原批。
func TestTC_M6_08_HTTP_ReworkNewBatch(t *testing.T) {
	e, ck := m6HTTPEnv(t)
	cust, inMat, outMat := m6MasterIDs(t, e.st)
	mgmt := e.login(m6MgmtOpenID)
	src := m6HBatch(t, e, ck, cust, inMat, outMat)
	srcID := int64(numOf(src["id"]))

	// 缺 reason ⇒ 400
	if resp, body := e.do("POST", "/api/prod/rework", ck,
		fmt.Sprintf(`{"src_batch_id":%d}`, srcID)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★ 返工缺 reason 应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	// ★ management 只有 READ ⇒ LevelInit 守卫应 403（锁死「不是 LevelAll」）
	if resp, body := e.do("POST", "/api/prod/rework", mgmt,
		fmt.Sprintf(`{"src_batch_id":%d,"reason":"管理层发起"}`, srcID)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★★ management（READ）发起返工应 403，实际 %d body=%v", resp.StatusCode, body)
	}

	resp, body := e.do("POST", "/api/prod/rework", ck,
		fmt.Sprintf(`{"src_batch_id":%d,"reason":"客户要求返工"}`, srcID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("返工应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	if numOf(row["id"]) == srcID {
		t.Fatalf("★★ 返工必须是新批")
	}
	np, err := codec.Parse(fmt.Sprint(row["code"]))
	if err != nil {
		t.Fatalf("新批码不可解析: %v", err)
	}
	today := time.Now().Format("060102")
	if np.Seg.Date != today {
		t.Fatalf("★★ 新批链根日期 = 新建当日，日期段应 %s，实际 %s", today, np.Seg.Date)
	}
	if fmt.Sprint(row["code"]) == fmt.Sprint(src["code"]) {
		t.Fatalf("★★ 不得复用原批码")
	}

	resp, body = e.do("GET", fmt.Sprintf("/api/prod/rework?src_batch_id=%d", srcID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查返工关联应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	rows, _ := body["rows"].([]interface{})
	if len(rows) != 1 {
		t.Fatalf("b_rework 应 1 行，实际 %d", len(rows))
	}
}

// TC-M6-09 ★ 变异向：同一生产批必须能挂多个袋（谱系不得是「批只挂一个袋」）。
func TestTC_M6_09_HTTP_GenealogyNotSingleBag(t *testing.T) {
	e, ck := m6HTTPEnv(t)
	cust, inMat, outMat := m6MasterIDs(t, e.st)
	truckA, bagsA := m6HTruck(t, e, ck, cust, inMat, 2)
	truckB, bagsB := m6HTruck(t, e, ck, cust, inMat, 1)
	m6HPass(t, e, ck, truckA, bagsA[0]["code"].(string))
	m6HPass(t, e, ck, truckB, bagsB[0]["code"].(string))
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := int64(numOf(batch["id"]))

	m6HFeed(t, e, ck, batchID, bagsA[0]["code"].(string))
	m6HFeed(t, e, ck, batchID, bagsA[1]["code"].(string))
	m6HFeed(t, e, ck, batchID, bagsB[0]["code"].(string))

	rows := m6HFeeds(t, e, ck, batchID)
	if len(rows) != 3 {
		t.Fatalf("★★★ 「批只挂一个袋」是错的：同一批应挂 3 个袋，实际 %d 行", len(rows))
	}
	bags, trucks := map[int64]bool{}, map[int64]bool{}
	for _, r := range rows {
		bags[numOf(r["bag_id"])] = true
		trucks[numOf(r["truck_id"])] = true
	}
	if len(bags) != 3 || len(trucks) != 2 {
		t.Fatalf("★★ 谱系应覆盖 3 袋 / 2 车，实际 %d / %d", len(bags), len(trucks))
	}
}

// ===== A13（接口侧）· 更正 / 删除必填原因，且 batch_id / bag_id 无修改路径 =====

func TestM6HTTPFeedCorrectDeleteGuards(t *testing.T) {
	e, ck := m6HTTPEnv(t)
	cust, inMat, outMat := m6MasterIDs(t, e.st)
	truck, bags := m6HTruck(t, e, ck, cust, inMat, 1)
	m6HPass(t, e, ck, truck, bags[0]["code"].(string))
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := int64(numOf(batch["id"]))
	feed := m6HFeed(t, e, ck, batchID, bags[0]["code"].(string))
	feedID := numOf(feed["id"])

	// 缺 reason ⇒ 400（更正 / 删除同）
	if resp, body := e.do("PATCH", fmt.Sprintf("/api/prod/feeds/%d", feedID), ck,
		`{"feed_weight":2.0}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★ 更正缺 reason 应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	if resp, body := e.do("DELETE", fmt.Sprintf("/api/prod/feeds/%d", feedID), ck,
		`{}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★ 删除缺 reason 应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	// ★ 请求体里带 batch_id / bag_id 也不生效（无修改路径）
	resp, body := e.do("PATCH", fmt.Sprintf("/api/prod/feeds/%d", feedID), ck,
		fmt.Sprintf(`{"feed_weight":2.0,"reason":"录错量","batch_id":999999,"bag_id":999999}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("更正应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	if numOf(row["batch_id"]) != batchID {
		t.Fatalf("★★ batch_id 不得被改动：应 %d，实际 %v", batchID, row["batch_id"])
	}
	if numOf(row["bag_id"]) != numOf(feed["bag_id"]) {
		t.Fatalf("★★ bag_id 不得被改动：应 %v，实际 %v", feed["bag_id"], row["bag_id"])
	}
	if w, _ := row["feed_weight"].(float64); w != 2.0 {
		t.Fatalf("feed_weight 应 2.0，实际 %v", row["feed_weight"])
	}

	// 删除：成功 ⇒ 行消失、袋回置在库（查袋状态走收货读入口）
	if resp, body := e.do("DELETE", fmt.Sprintf("/api/prod/feeds/%d", feedID), ck,
		`{"reason":"投错批"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if rows := m6HFeeds(t, e, ck, batchID); len(rows) != 0 {
		t.Fatalf("删除后投料行应 0，实际 %d", len(rows))
	}
	var status string
	if err := e.st.DB().QueryRowContext(context.Background(),
		`SELECT status FROM b_bag WHERE code = ?`, bags[0]["code"]).Scan(&status); err != nil {
		t.Fatalf("读袋状态失败: %v", err)
	}
	if status != "在库" {
		t.Fatalf("★★ 删除后袋状态应回置「在库」，实际 %q", status)
	}
}

// ===== A12（接口侧）· 权限级别逐点核对 =====

// A12：6 个 prod.* 在路由层被消费且级别正确 ——
//
//	写入 ALL（production 200 / 无权 403）、读 READ（management 200）、
//	返工 INIT（production 发起 200 / management READ 发起 403，见 TC-M6-08）。
func TestM6HTTPPermLevels(t *testing.T) {
	e, ck := m6HTTPEnv(t)
	cust, inMat, outMat := m6MasterIDs(t, e.st)
	mgmt := e.login(m6MgmtOpenID)

	// 权限摘要：production 应拿到 6 个点的级别
	resp, body := e.do("GET", "/api/prod/perm-summary", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("perm-summary 应 200，实际 %d", resp.StatusCode)
	}
	points, _ := body["points"].(map[string]interface{})
	want := map[string]string{
		"prod.batch.create": "", "prod.feed.scan": "", "prod.feed.correct": "",
		"prod.op.log": "", "prod.fg.gen": "", "prod.rework": "",
	}
	for code := range want {
		v, ok := points[code]
		if !ok {
			t.Fatalf("★★ perm-summary 缺少 %s：%v", code, points)
		}
		if fmt.Sprint(v) == "NONE" {
			t.Fatalf("★ production 在 %s 上不应是 NONE，实际 %v", code, v)
		}
	}
	// production 的 prod.rework 必须含 INIT（LevelInit 守卫能过）
	if fmt.Sprint(points["prod.rework"]) != "INIT" {
		t.Fatalf("★★ production 的 prod.rework 应 INIT，实际 %v", points["prod.rework"])
	}

	// 写入口 ALL：未登录 ⇒ 401；management 对 prod.batch.create 是 READ ⇒ POST 403 / GET 200
	if resp, _ := e.do("POST", "/api/prod/batches", "", `{}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录建批应 401，实际 %d", resp.StatusCode)
	}
	if resp, body := e.do("POST", "/api/prod/batches", mgmt,
		fmt.Sprintf(`{"customer_id":%d,"input_material_id":%d,"planned_output_material_id":%d}`,
			cust, inMat, outMat)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★★ management（READ）建批应 403，实际 %d body=%v", resp.StatusCode, body)
	}
	// 读入口 READ：management 可读
	if resp, body := e.do("GET", "/api/prod/batches", mgmt, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("management 读生产批应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	// 谱系读入口（prod.feed.scan/READ）：management 可读
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	_ = batch
	if resp, body := e.do("GET", "/api/prod/genealogy/bags/1BCG0000000000000000000000000", mgmt, ""); resp.StatusCode == http.StatusForbidden {
		t.Fatalf("management 读谱系不应 403，实际 body=%v", body)
	}
}
