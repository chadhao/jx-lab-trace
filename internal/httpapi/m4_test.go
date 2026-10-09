package httpapi

// ===== M4 取样与留样 · 接口级 TC =====
//
// ★ 账号：qc（sample.take/retain.in/retain.lend=ALL、destroy.init=INIT、
//	destroy.approve=NONE）＋ management（destroy.approve=APPROVE）＋
//	receiver（借收货链路造车/袋，与批 3 同手法）。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。
// ★ 固定测试主数据（客户 9402 / 物料 9402），跑前跑后各清一次（先子后父、按账号精确）。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/codec"
	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	m4HTTPOpenID   = "ou_test_m4_http"
	m4MgmtOpenID   = "ou_test_m4_mgmt"
	m4HTTPCustCode = "9402"
	m4HTTPMatCode  = "9402"
	m4HTTPDate     = "2026-10-09"
	m4HTTPDateSeg  = "261009"
)

// m4HTTPWipe 清掉 M4 接口测试数据。
//
// ★ 顺序：销毁 → 借还 → 留样 → 样品 → 取样组 → 作废 → 袋 → 车 → 预报 → 标签
//
//	→ 生产批 → 成品批 → 主数据（先子后父）。
//
// ★ 范围：只删本账号（ou_test_m4_http）与本测试客户 9402 的行（禁前缀通配）。
func m4HTTPWipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	me := m4HTTPOpenID
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
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
		me, m4HTTPCustCode)
	q(`DELETE FROM b_truck_lot WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, m4HTTPCustCode)
	q(`DELETE FROM b_arrival_notice WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, m4HTTPCustCode)
	q(`DELETE FROM b_label_print WHERE created_by = ?`, me)
	q(`DELETE FROM b_production_batch WHERE created_by = ?`, me)
	q(`DELETE FROM b_fg_lot WHERE created_by = ?`, me)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m4HTTPCustCode)
	q(`DELETE FROM m_material WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m4HTTPMatCode)
}

// m4HTTPEnv 建测试主数据 + 起服务 + 绑角色；返回 env 与 qc 的 cookie。
func m4HTTPEnv(t *testing.T) (*env, string) {
	t.Helper()
	st := requireDB(t)
	m4HTTPWipe(t, st)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M4接口测试客户', '启用', 1, 1, ?)`, m4HTTPCustCode, m4HTTPOpenID); err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M4接口测试原料', '原料', '启用', 1, 1, ?)`, m4HTTPMatCode, m4HTTPOpenID); err != nil {
		t.Fatalf("建测试物料失败: %v", err)
	}
	t.Cleanup(func() { m4HTTPWipe(t, st) })

	// qc 才有取样/留样权限；receiver 提供收货链路（造车/袋）——多角色取并集
	bindRole(t, st, m4HTTPOpenID, "qc")
	bindRole(t, st, m4HTTPOpenID, "receiver")
	bindRole(t, st, m4MgmtOpenID, "management")

	e := newEnv(t, time.Hour)
	return e, e.login(m4HTTPOpenID)
}

// m4HTTPMasterIDs 取测试客户/物料当前行 id。
func m4HTTPMasterIDs(t *testing.T, st *store.Store) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var cust, mat int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_customer WHERE code = ? AND is_current = 1`, m4HTTPCustCode).Scan(&cust); err != nil {
		t.Fatalf("查测试客户失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, m4HTTPMatCode).Scan(&mat); err != nil {
		t.Fatalf("查测试物料失败: %v", err)
	}
	return cust, mat
}

// m4HTTPBags 走接口造一个车次 + n 个袋码，返回袋行。
func m4HTTPBags(t *testing.T, e *env, ck string, custID, matID int64, n int) []map[string]interface{} {
	t.Helper()
	truck := m3ArriveViaHTTP(t, e, ck, custID, matID, n)
	id := int64(numOf(truck["id"]))
	return m3WeighAndBags(t, e, ck, id, "35000", "5000", n)
}

// m4HTTPBatchFixture 直写库造生产批（批 6 对象，任务包 §5 允许夹具）。
func m4HTTPBatchFixture(t *testing.T, st *store.Store, custID, matID int64) (int64, string) {
	t.Helper()
	code, err := codec.Generate(codec.Segments{
		T: "C", BT: "CG", Customer: m4HTTPCustCode, Material: m4HTTPMatCode,
		Date: m4HTTPDateSeg, SEQ1: "01", SEQ2: "000", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成生产批码失败: %v", err)
	}
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_production_batch
  (code, customer_id, input_material_id, planned_output_material_id, batch_date, status, created_by)
VALUES (?,?,?,?,?, '进行中', ?)`, code, custID, matID, matID, m4HTTPDate, m4HTTPOpenID)
	if err != nil {
		t.Fatalf("造生产批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id, code
}

// m4HTTPFgFixture 直写库造成品批。
func m4HTTPFgFixture(t *testing.T, st *store.Store, custID, matID, batchID int64) (int64, string) {
	t.Helper()
	code, err := codec.Generate(codec.Segments{
		T: "D", BT: "CG", Customer: m4HTTPCustCode, Material: m4HTTPMatCode,
		Date: m4HTTPDateSeg, SEQ1: "01", SEQ2: "001", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成成品批码失败: %v", err)
	}
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_fg_lot
  (code, batch_id, customer_id, output_material_id, status, created_by)
VALUES (?,?,?,?, '在库', ?)`, code, batchID, custID, matID, m4HTTPOpenID)
	if err != nil {
		t.Fatalf("造成品批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id, code
}

// TC-M4-01 正常：扫吨袋码取样一次 ⇒ 2 条样品（份样+保留样），编号按 §6-4 可推导。
func TestTC_M4_01_HTTP_ScanTakeCreatesTwoSamples(t *testing.T) {
	e, ck := m4HTTPEnv(t)
	cust, mat := m4HTTPMasterIDs(t, e.st)
	bags := m4HTTPBags(t, e, ck, cust, mat, 2)
	code := fmt.Sprint(bags[0]["code"])

	resp, body := e.do("POST", "/api/sample/take", ck, fmt.Sprintf(`{"code":%q}`, code))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("扫码取样应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if numOf(body["created"]) != 2 {
		t.Fatalf("应生成 2 条样品，实际 %v", body["created"])
	}
	human, err := codec.ToHuman(code)
	if err != nil {
		t.Fatalf("转人读行失败: %v", err)
	}
	parts := strings.Split(human, "-")
	parent := strings.Join(parts[:7], "-")
	if got := fmt.Sprint(body["parent"]); got != parent {
		t.Fatalf("父码应 %q，实际 %q", parent, got)
	}
	samples, _ := body["samples"].([]interface{})
	got := map[string]string{}
	for _, it := range samples {
		m, _ := it.(map[string]interface{})
		got[fmt.Sprint(m["role"])] = fmt.Sprint(m["sample_no"])
	}
	if got["份样"] != parent+"-I01" {
		t.Fatalf("份样编号应 %q，实际 %q", parent+"-I01", got["份样"])
	}
	if got["保留样"] != parent+"-R01" {
		t.Fatalf("保留样编号应 %q，实际 %q", parent+"-R01", got["保留样"])
	}
}

// TC-M4-08 ★ 扫生产批码 ⇒ 中间样绑生产批（batch_id 非空、bag_id 空）。
func TestTC_M4_08_HTTP_IntermediateBindsBatch(t *testing.T) {
	e, ck := m4HTTPEnv(t)
	cust, mat := m4HTTPMasterIDs(t, e.st)
	batchID, batchCode := m4HTTPBatchFixture(t, e.st, cust, mat)

	resp, body := e.do("POST", "/api/sample/take", ck, fmt.Sprintf(`{"code":%q}`, batchCode))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("扫生产批码取样应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	samples, _ := body["samples"].([]interface{})
	if len(samples) != 2 {
		t.Fatalf("应生成 2 条样品，实际 %d", len(samples))
	}
	for _, it := range samples {
		m, _ := it.(map[string]interface{})
		if numOf(m["batch_id"]) != batchID {
			t.Fatalf("%s 应绑生产批 %d，实际 %v", m["sample_no"], batchID, m["batch_id"])
		}
		if _, hasBag := m["bag_id"]; hasBag && m["bag_id"] != nil {
			t.Fatalf("%s 不得绑吨袋，实际 %v", m["sample_no"], m["bag_id"])
		}
	}
}

// TC-M4-09 ★ 扫成品批码 ⇒ 成品样绑成品批（fg_lot_id 非空）。
func TestTC_M4_09_HTTP_FinishedGoodsBindsFgLot(t *testing.T) {
	e, ck := m4HTTPEnv(t)
	cust, mat := m4HTTPMasterIDs(t, e.st)
	batchID, _ := m4HTTPBatchFixture(t, e.st, cust, mat)
	fgID, fgCode := m4HTTPFgFixture(t, e.st, cust, mat, batchID)

	resp, body := e.do("POST", "/api/sample/take", ck, fmt.Sprintf(`{"code":%q}`, fgCode))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("扫成品批码取样应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	samples, _ := body["samples"].([]interface{})
	if len(samples) != 2 {
		t.Fatalf("应生成 2 条样品，实际 %d", len(samples))
	}
	for _, it := range samples {
		m, _ := it.(map[string]interface{})
		if numOf(m["fg_lot_id"]) != fgID {
			t.Fatalf("%s 应绑成品批 %d，实际 %v", m["sample_no"], fgID, m["fg_lot_id"])
		}
	}
}

// TC-M4-07 异常（接口级）+ A12 发起≠审批：
//
//	① 审批不填 approved_by ⇒ 400；
//	② qc 走审批入口 ⇒ 403（APPROVE 级守卫，qc 为 NONE）；
//	③ management（APPROVE）才可批 ⇒ 200，状态同步「已销毁」。
func TestTC_M4_07_HTTP_DestroyInitApproveSplit(t *testing.T) {
	e, ck := m4HTTPEnv(t)
	cust, mat := m4HTTPMasterIDs(t, e.st)
	bags := m4HTTPBags(t, e, ck, cust, mat, 1)
	code := fmt.Sprint(bags[0]["code"])

	// 取样 + 留样入库（qc 有 retain.in）
	resp, body := e.do("POST", "/api/sample/take", ck, fmt.Sprintf(`{"code":%q}`, code))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取样应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	var retentionID int64
	for _, it := range body["samples"].([]interface{}) {
		m, _ := it.(map[string]interface{})
		if fmt.Sprint(m["role"]) == "保留样" {
			retentionID = numOf(m["id"])
		}
	}
	if retentionID == 0 {
		t.Fatalf("未取到保留样 id: %v", body)
	}
	resp, body = e.do("POST", "/api/sample/retention", ck,
		fmt.Sprintf(`{"sample_id":%d,"location":"化验室-留样柜A-第5层"}`, retentionID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("留样入库应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	// 发起（qc 有 INIT）
	resp, body = e.do("POST", "/api/sample/destroy/init", ck,
		fmt.Sprintf(`{"sample_id":%d,"destroyed_by":"张三","reason":"超过保留期"}`, retentionID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("销毁发起应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	// ① qc 走审批入口 ⇒ 403（A12：approve 级别守卫，qc=NONE 不得绕过；
	//	守卫在 handler 之前 —— qc 连「空审批人校验」都触达不到）
	resp, body = e.do("POST", "/api/sample/destroy/approve", ck,
		fmt.Sprintf(`{"sample_id":%d,"approved_by":"李四"}`, retentionID))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("qc 走审批入口应 403，实际 %d body=%v", resp.StatusCode, body)
	}
	// 状态仍是在库（未被审批）
	if st := m4SampleStatus(t, e.st, retentionID); st != "在库" {
		t.Fatalf("被 403 拒绝后状态应仍「在库」，实际 %q", st)
	}

	// management（APPROVE）才进得了审批 handler
	mgmtCK := e.login(m4MgmtOpenID)

	// ② ★★ 不填审批人 ⇒ 400（A8 / TC-M4-07，由有审批权的账号触发）
	resp, body = e.do("POST", "/api/sample/destroy/approve", mgmtCK,
		fmt.Sprintf(`{"sample_id":%d,"approved_by":""}`, retentionID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("审批人为空应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	if st := m4SampleStatus(t, e.st, retentionID); st != "在库" {
		t.Fatalf("空审批人被拒后状态应仍「在库」，实际 %q", st)
	}

	// ③ management 填了审批人 ⇒ 200，两表同步「已销毁」
	resp, body = e.do("POST", "/api/sample/destroy/approve", mgmtCK,
		fmt.Sprintf(`{"sample_id":%d,"approved_by":"李四"}`, retentionID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("management 审批应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if st := m4SampleStatus(t, e.st, retentionID); st != "已销毁" {
		t.Fatalf("审批后状态应「已销毁」，实际 %q", st)
	}
	row, _ := body["row"].(map[string]interface{})
	if fmt.Sprint(row["approved_by"]) != "李四" || fmt.Sprint(row["destroyed_by"]) != "张三" {
		t.Fatalf("销毁记录须留发起人+审批人，实际 %v", row)
	}
}

// m4SampleStatus 查样品当前状态（库侧断言用）。
func m4SampleStatus(t *testing.T, st *store.Store, sampleID int64) string {
	t.Helper()
	var s string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT status FROM b_sample WHERE id = ?`, sampleID).Scan(&s); err != nil {
		t.Fatalf("读样品状态失败: %v", err)
	}
	return s
}
