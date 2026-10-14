package httpapi

// ===== N-023 收货批次化 · 接口级 TC =====
//
// ★ receiver 角色（recv.notice.create = ALL/READ，recv.notice.edit = ALL）。
// ★ DB 集成测试：本机未设 JX_TEST_DB=1 一律 Skip；服务器经 run_tc_server.sh 执行。
// ★ 覆盖任务包 D5-8：新/改端点**请求响应逐字段形状断言**；
//	D5-1/2/3：三元组 400 含已有批次、关闭幂等+关闭后 400、L1/L2/L3 口径。

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/store"
)

// n023Env 建主数据 + receiver 登录（独立账号，避免与 m3 互相清理）。
func n023Env(t *testing.T) (*env, string) {
	t.Helper()
	st := requireDB(t)
	n023HTTWipe(t, st)
	ctx := t.Context()
	_, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES ('9304', 'N023接口测试客户', '启用', 1, 1, 'ou_test_n023_http')`)
	if err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	_, err = st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES ('9304', 'N023接口测试原料', '原料', '启用', 1, 1, 'ou_test_n023_http')`)
	if err != nil {
		t.Fatalf("建测试物料失败: %v", err)
	}
	t.Cleanup(func() { n023HTTWipe(t, st) })
	bindRole(t, st, "ou_test_n023_http", "receiver")
	e := newEnv(t, time.Hour)
	return e, e.login("ou_test_n023_http")
}

func n023HTTWipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := t.Context()
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略）: %v", err)
		}
	}
	q(`DELETE FROM b_obj_void WHERE entity='b_bag' AND entity_id IN
	     (SELECT id FROM b_bag WHERE created_by='ou_test_n023_http')`)
	q(`DELETE FROM b_bag WHERE created_by = 'ou_test_n023_http'`)
	q(`DELETE FROM b_truck_lot WHERE created_by = 'ou_test_n023_http'`)
	q(`DELETE FROM b_arrival_notice WHERE created_by = 'ou_test_n023_http'`)
	q(`DELETE FROM m_incoming_batch WHERE created_by = 'ou_test_n023_http'`)
	q(`DELETE FROM m_customer WHERE code='9304' AND created_by LIKE 'ou\_test\_%'`)
	q(`DELETE FROM m_material WHERE code='9304' AND created_by LIKE 'ou\_test\_%'`)
}

func n023MasterIDs(t *testing.T, st *store.Store) (int64, int64) {
	t.Helper()
	ctx := t.Context()
	var cust, mat int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_customer WHERE code='9304' AND is_current=1`).Scan(&cust); err != nil {
		t.Fatalf("查测试客户失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code='9304' AND is_current=1`).Scan(&mat); err != nil {
		t.Fatalf("查测试物料失败: %v", err)
	}
	return cust, mat
}

// n023CreateBatch POST /api/recv/batches，返回 row。
func n023CreateBatch(t *testing.T, e *env, ck string, custID, matID int64, cbn string) map[string]interface{} {
	t.Helper()
	body := fmt.Sprintf(
		`{"customer_id":%d,"material_id":%d,"customer_batch_no":%q}`, custID, matID, cbn)
	resp, out := e.do("POST", "/api/recv/batches", ck, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预填批次应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	row, _ := out["row"].(map[string]interface{})
	if row == nil {
		t.Fatalf("预填批次应返回 row: %v", out)
	}
	return row
}

// D5-8 形状断言 ①：POST /api/recv/batches 请求/响应逐字段。
func TestN023_Shape_CreateBatch(t *testing.T) {
	e, ck := n023Env(t)
	cust, mat := n023MasterIDs(t, e.st)
	row := n023CreateBatch(t, e, ck, cust, mat, "N023-SHAPE-01")

	wantRow := []string{"id", "our_batch_no", "customer_batch_no", "customer_id",
		"customer_code", "customer_name", "material_id", "material_code", "material_name",
		"status", "closed_at", "closed_by", "created_at", "created_by",
		"notice_count", "arrived_count", "fed_done_count"}
	for _, k := range wantRow {
		if _, ok := row[k]; !ok {
			t.Errorf("批次响应缺字段 %q（实得 keys: %v）", k, keysOf(row))
		}
	}
	for _, gone := range []string{"eta", "est_bag_count"} {
		if _, ok := row[gone]; ok {
			t.Errorf("批次响应不应含 %q", gone)
		}
	}
	if row["status"] != "回货中" {
		t.Errorf("新批次状态应 回货中，实际 %v", row["status"])
	}
	ourNo := fmt.Sprint(row["our_batch_no"])
	if len(ourNo) != 27 || ourNo[:2] != "1F" {
		t.Errorf("our_batch_no 应 27 位 F 码，实际 %q", ourNo)
	}

	// 三元组重复 ⇒ 400 且含 existing（D5-1）
	resp, out := e.do("POST", "/api/recv/batches", ck,
		fmt.Sprintf(`{"customer_id":%d,"material_id":%d,"customer_batch_no":"N023-SHAPE-01"}`, cust, mat))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("重复三元组应 400，实际 %d body=%v", resp.StatusCode, out)
	}
	existing, _ := out["existing"].([]interface{})
	if len(existing) != 1 {
		t.Fatalf("400 响应应含 existing[1]，实际 %v", out)
	}
	e0, _ := existing[0].(map[string]interface{})
	if fmt.Sprint(e0["our_batch_no"]) != ourNo || fmt.Sprint(e0["customer_batch_no"]) != "N023-SHAPE-01" {
		t.Fatalf("existing 应含已有批次标识，实际 %v", e0)
	}
	if _, ok := out["error"]; !ok {
		t.Errorf("400 响应应含 error 文案: %v", out)
	}
}

// D5-8 形状断言 ②：POST /api/recv/notices（请求仅 batch+车辆；响应无预估字段）；
// 关闭后 ⇒ 400（D5-2）；L2 口径（D5-3）。
func TestN023_Shape_CreateNoticeAndL2(t *testing.T) {
	e, ck := n023Env(t)
	cust, mat := n023MasterIDs(t, e.st)
	batch := n023CreateBatch(t, e, ck, cust, mat, "N023-SHAPE-02")
	batchID := int64(numOf(batch["id"]))

	// 无 batch_id ⇒ 400
	resp, out := e.do("POST", "/api/recv/notices", ck, `{"plate_no":"湘F·无批次"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 batch_id 应 400，实际 %d body=%v", resp.StatusCode, out)
	}

	// 正常预报
	resp, out = e.do("POST", "/api/recv/notices", ck,
		fmt.Sprintf(`{"batch_id":%d,"plate_no":"湘F·N023","driver":"司机丁","phone":"13800000000"}`, batchID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预报登记应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	notice, _ := out["row"].(map[string]interface{})
	wantNotice := []string{"id", "notice_no", "batch_id", "our_batch_no", "customer_batch_no",
		"customer_id", "material_id", "plate_no", "driver", "phone", "biz_type",
		"seq_no", "arrive_date", "status", "legacy"}
	for _, k := range wantNotice {
		if _, ok := notice[k]; !ok {
			t.Errorf("预报响应缺字段 %q（实得: %v）", k, keysOf(notice))
		}
	}
	for _, gone := range []string{"eta", "est_bag_count"} {
		if _, ok := notice[gone]; ok {
			t.Errorf("N-023 预报响应不应含 %q", gone)
		}
	}
	if numOf(notice["batch_id"]) != batchID {
		t.Errorf("batch_id 应回传 %d，实际 %v", batchID, notice["batch_id"])
	}
	if fmt.Sprint(notice["our_batch_no"]) != fmt.Sprint(batch["our_batch_no"]) {
		t.Errorf("our_batch_no 应从批次带出: notice=%v batch=%v",
			notice["our_batch_no"], batch["our_batch_no"])
	}
	if notice["arrive_date"] != "" && notice["arrive_date"] != nil {
		t.Errorf("创建时 arrive_date 应为空（确认时写入），实际 %v", notice["arrive_date"])
	}
	if numOf(notice["seq_no"]) != 1 {
		t.Errorf("批次内首车序应 1，实际 %v", notice["seq_no"])
	}
	if notice["legacy"] != false {
		t.Errorf("批次预报 legacy 应 false，实际 %v", notice["legacy"])
	}

	// L2：pending=1 含本条（batch_id 非空 且 预报中）
	resp, out = e.do("GET", "/api/recv/notices?pending=1", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("L2 应 200，实际 %d", resp.StatusCode)
	}
	rows, _ := out["rows"].([]interface{})
	found := false
	for _, r := range rows {
		m, _ := r.(map[string]interface{})
		if numOf(m["id"]) == numOf(notice["id"]) {
			found = true
			if fmt.Sprint(m["our_batch_no"]) == "" {
				t.Errorf("L2 行应含所属批次号: %v", m)
			}
		}
	}
	if !found {
		t.Errorf("L2 应包含刚建的批次预报: %v", out)
	}

	// 关闭批次 ⇒ 后续预报 400（D5-2）
	resp, out = e.do("POST", fmt.Sprintf("/api/recv/batches/%d/close", batchID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("关闭应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	// 幂等：再关一次仍 200
	resp, out = e.do("POST", fmt.Sprintf("/api/recv/batches/%d/close", batchID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重复关闭应幂等 200，实际 %d body=%v", resp.StatusCode, out)
	}
	closedRow, _ := out["row"].(map[string]interface{})
	if closedRow["status"] != "已关闭" {
		t.Fatalf("关闭后状态应 已关闭，实际 %v", closedRow["status"])
	}
	resp, out = e.do("POST", "/api/recv/notices", ck,
		fmt.Sprintf(`{"batch_id":%d,"plate_no":"湘F·关后"}`, batchID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("关闭后预报应 400，实际 %d body=%v", resp.StatusCode, out)
	}
	if !strings.Contains(fmt.Sprint(out["error"]), "已关闭") {
		t.Errorf("400 文案应说明批次已关闭，实际 %v", out["error"])
	}
}

// D5-3 ★ L1 口径：?status=回货中 只回未关闭批次，行含统计三字段。
func TestN023_L1_UnclosedOnly(t *testing.T) {
	e, ck := n023Env(t)
	cust, mat := n023MasterIDs(t, e.st)
	open := n023CreateBatch(t, e, ck, cust, mat, "N023-L1-OPEN")
	closed := n023CreateBatch(t, e, ck, cust, mat, "N023-L1-CLOSED")
	resp, out := e.do("POST", fmt.Sprintf("/api/recv/batches/%d/close", numOf(closed["id"])), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("关闭应 200，实际 %d", resp.StatusCode)
	}

	resp, out = e.do("GET", "/api/recv/batches?status=回货中", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("L1 应 200，实际 %d", resp.StatusCode)
	}
	rows, _ := out["rows"].([]interface{})
	ids := map[int64]bool{}
	for _, r := range rows {
		m, _ := r.(map[string]interface{})
		ids[numOf(m["id"])] = true
		for _, k := range []string{"notice_count", "arrived_count", "fed_done_count"} {
			if _, ok := m[k]; !ok {
				t.Errorf("L1 行缺统计字段 %q: %v", k, keysOf(m))
			}
		}
	}
	if !ids[numOf(open["id"])] {
		t.Errorf("L1 应回未关闭批次 %v", open["id"])
	}
	if ids[numOf(closed["id"])] {
		t.Errorf("L1 不应回已关闭批次 %v", closed["id"])
	}
}

// D5-3 ★ L3 口径：search 同时回批次与预报；旧版预报（batch_id NULL）标注 legacy=true。
func TestN023_L3_SearchLegacyFlagged(t *testing.T) {
	e, ck := n023Env(t)
	cust, mat := n023MasterIDs(t, e.st)
	batch := n023CreateBatch(t, e, ck, cust, mat, "N023-L3-01")

	// 批次预报（新）
	if resp, _ := e.do("POST", "/api/recv/notices", ck,
		fmt.Sprintf(`{"batch_id":%d}`, numOf(batch["id"]))); resp.StatusCode != http.StatusOK {
		t.Fatalf("批次预报应 200")
	}
	// 旧版预报（直写库造 batch_id NULL 存量行）
	if _, err := e.st.DB().ExecContext(t.Context(), `
INSERT INTO b_arrival_notice
  (notice_no, customer_id, material_id, biz_type, seq_no, arrive_date, status, created_by)
VALUES ('YB26100999l3', ?, ?, 'CG', 77, '2026-10-09', '预报', 'ou_test_n023_http')`,
		cust, mat); err != nil {
		t.Fatalf("造旧版预报失败: %v", err)
	}

	resp, out := e.do("GET", fmt.Sprintf("/api/recv/search?customer_id=%d", cust), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search 应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	if numOf(out["batch_count"]) < 1 {
		t.Fatalf("search 应含批次: %v", out)
	}
	notices, _ := out["notices"].([]interface{})
	if len(notices) < 2 {
		t.Fatalf("search 预报应含新旧两条: %v", out)
	}
	var sawNew, sawLegacy bool
	for _, r := range notices {
		m, _ := r.(map[string]interface{})
		legacy, _ := m["legacy"].(bool)
		if legacy {
			sawLegacy = true
			if fmt.Sprint(m["our_batch_no"]) != "" {
				t.Errorf("旧版预报不应带批次号: %v", m)
			}
			if numOf(m["seq_no"]) != 77 {
				t.Errorf("旧版预报 seq 应保留 77，实际 %v", m["seq_no"])
			}
		} else if numOf(m["batch_id"]) > 0 {
			sawNew = true
		}
	}
	if !sawNew || !sawLegacy {
		t.Fatalf("search 应同时含新批次预报(legacy=false)与旧版(legacy=true): new=%v legacy=%v out=%v",
			sawNew, sawLegacy, out)
	}

	// legacy=0 ⇒ 只回批次预报
	resp, out = e.do("GET", fmt.Sprintf("/api/recv/search?legacy=0&customer_id=%d", cust), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search legacy=0 应 200")
	}
	notices, _ = out["notices"].([]interface{})
	for _, r := range notices {
		m, _ := r.(map[string]interface{})
		if m["legacy"] == true {
			t.Errorf("legacy=0 不应含旧版预报: %v", m)
		}
	}

	// L2 不含旧版（batch_id NULL 排除，D5-3）
	resp, out = e.do("GET", "/api/recv/notices?pending=1", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("L2 应 200")
	}
	rows, _ := out["rows"].([]interface{})
	for _, r := range rows {
		m, _ := r.(map[string]interface{})
		if numOf(m["batch_id"]) == 0 {
			t.Errorf("L2 不应含旧版预报（batch_id=0）: %v", m)
		}
	}
}

// keysOf 返回 map 键列表（形状断言失败信息用）。
func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
