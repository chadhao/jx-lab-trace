package httpapi

// ══════════════════════════════════════════════════════════════════════════
// N-026 · 取样权限职责更正：sample.take 由 qc 移交 production
//
// 对应 BACKEND-REQ-01 §四 验收 5 条（MIMO-NEXT-FIX-10 D2）：
//
//	1. production 身份 POST /api/sample/take ⇒ 200，sampled_by 为该生产账号
//	2. qc 身份 POST /api/sample/take ⇒ 403
//	3. qc 身份 GET /api/sample/samples ⇒ 200
//	4. 重启后 GET /api/admin/permission-matrix ⇒ sample.take: production=ALL, qc=READ
//	5. 全新空库跑迁移 ⇒ 种子落库即新值（由 spec seed + migrations_sync 钉住；
//	   共享测试库上 INSERT IGNORE 不覆盖旧行 ⇒ 用例先归位两格再断言）
//
// ★ 需要真实库 ⇒ JX_TEST_DB=1 门控（docs/05）。
// ══════════════════════════════════════════════════════════════════════════

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/permission"
	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	n026ProdOpenID = "ou_test_n026_prod" // production（取样写）
	n026QcOpenID   = "ou_test_n026_qc"   // qc（取样读 / 留样写）
	n026RecvOpenID = "ou_test_n026_recv" // receiver（收货链路造车/袋）
	n026AdminID    = "ou_test_n026_admin"
	n026CustCode   = "9426"
	n026MatCode    = "9426"
)

// n026Wipe 清掉 N-026 测试数据（先子后父、按账号精确）。
func n026Wipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	me := n026ProdOpenID
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
		me, n026CustCode)
	q(`DELETE FROM b_truck_lot WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, n026CustCode)
	q(`DELETE FROM b_arrival_notice WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, n026CustCode)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, n026CustCode)
	q(`DELETE FROM m_material WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, n026MatCode)
}

// n026SeedTake 将 sample.take 两格归位为新种子值（production=ALL / qc=READ）。
// ★ 共享测试库上既有迁移是 INSERT IGNORE（只补缺不覆盖）⇒ 用例自足归位；
//
//	正式运行库生效路径见 D3（空库迁移 / WorkBuddy 走 PUT 矩阵）。
func n026SeedTake(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, cell := range []struct{ role, level string }{
		{"production", "ALL"},
		{"qc", "READ"},
	} {
		if _, err := st.DB().ExecContext(ctx,
			`UPDATE s_role_permission SET level=? WHERE point_code='sample.take' AND role_code=?`,
			cell.level, cell.role); err != nil {
			t.Fatalf("归位 sample.take×%s=%s 失败: %v", cell.role, cell.level, err)
		}
	}
}

// n026Env 建主数据 + 三角色账号 + 起服务；返回 env 与 production cookie。
func n026Env(t *testing.T) (*env, string) {
	t.Helper()
	st := requireDB(t)
	n026SeedTake(t, st)
	n026Wipe(t, st)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'N026接口测试客户', '启用', 1, 1, ?)`, n026CustCode, n026RecvOpenID); err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'N026接口测试原料', '原料', '启用', 1, 1, ?)`, n026MatCode, n026RecvOpenID); err != nil {
		t.Fatalf("建测试物料失败: %v", err)
	}
	t.Cleanup(func() { n026Wipe(t, st) })

	bindRole(t, st, n026ProdOpenID, "production")
	bindRole(t, st, n026QcOpenID, "qc")
	bindRole(t, st, n026RecvOpenID, "receiver")
	bindRole(t, st, n026AdminID, "sysadmin")

	e := newEnv(t, time.Hour)
	return e, e.login(n026ProdOpenID)
}

// n026Bags 用 receiver 造一个车次 + n 个袋码。
func n026Bags(t *testing.T, e *env, n int) []map[string]interface{} {
	t.Helper()
	cust, mat := n026MasterIDs(t, e)
	recvCK := e.login(n026RecvOpenID)
	truck := m3ArriveViaHTTP(t, e, recvCK, cust, mat)
	id := int64(numOf(truck["id"]))
	return m3WeighAndBags(t, e, recvCK, id, "35000", "5000", n)
}

func n026MasterIDs(t *testing.T, e *env) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var cust, mat int64
	if err := e.st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_customer WHERE code = ? AND is_current = 1`, n026CustCode).Scan(&cust); err != nil {
		t.Fatalf("查测试客户失败: %v", err)
	}
	if err := e.st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, n026MatCode).Scan(&mat); err != nil {
		t.Fatalf("查测试物料失败: %v", err)
	}
	return cust, mat
}

// TC-N026-01 验收①：production 身份 POST /api/sample/take ⇒ 200，sampled_by 为该生产账号。
func TestTC_N026_01_ProductionCanTake(t *testing.T) {
	e, prodCK := n026Env(t)
	bags := n026Bags(t, e, 1)
	code := fmt.Sprint(bags[0]["code"])

	resp, body := e.do("POST", "/api/sample/take", prodCK, fmt.Sprintf(`{"code":%q}`, code))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("production 取样应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if numOf(body["created"]) != 2 {
		t.Fatalf("应生成 2 条样品（份样+保留样），实际 %v", body["created"])
	}
	samples, _ := body["samples"].([]interface{})
	if len(samples) != 2 {
		t.Fatalf("samples 应 2 条，实际 %d", len(samples))
	}
	for _, it := range samples {
		m, _ := it.(map[string]interface{})
		if got := fmt.Sprint(m["sampled_by"]); got != n026ProdOpenID {
			t.Fatalf("★ sampled_by 应为生产账号 %q，实际 %q（sample_no=%v）",
				n026ProdOpenID, got, m["sample_no"])
		}
	}
	// 库侧二次确认（不信任响应体）
	var dbBy string
	if err := e.st.DB().QueryRowContext(context.Background(),
		`SELECT IFNULL(sampled_by,'') FROM b_sample WHERE created_by=? ORDER BY id LIMIT 1`,
		n026ProdOpenID).Scan(&dbBy); err != nil {
		t.Fatalf("查库 sampled_by 失败: %v", err)
	}
	if dbBy != n026ProdOpenID {
		t.Fatalf("★ 库内 sampled_by 应为 %q，实际 %q", n026ProdOpenID, dbBy)
	}
}

// TC-N026-02 验收②：qc 身份 POST /api/sample/take ⇒ 403。
func TestTC_N026_02_QcTakeForbidden(t *testing.T) {
	e, _ := n026Env(t)
	bags := n026Bags(t, e, 1)
	code := fmt.Sprint(bags[0]["code"])
	qcCK := e.login(n026QcOpenID)

	resp, body := e.do("POST", "/api/sample/take", qcCK, fmt.Sprintf(`{"code":%q}`, code))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★★ qc 取样应 403（N-026：写权限归 production），实际 %d body=%v",
			resp.StatusCode, body)
	}
	// 未产生任何样品行
	var n int
	if err := e.st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM b_sample WHERE created_by=?`, n026QcOpenID).Scan(&n); err != nil {
		t.Fatalf("查样品行失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("403 后不得落样品行，实际 %d", n)
	}
}

// TC-N026-03 验收③：qc 身份 GET /api/sample/samples ⇒ 200。
func TestTC_N026_03_QcListSamplesReadOK(t *testing.T) {
	e, prodCK := n026Env(t)
	// production 先取一条样（列表非空路径）
	bags := n026Bags(t, e, 1)
	code := fmt.Sprint(bags[0]["code"])
	resp, body := e.do("POST", "/api/sample/take", prodCK, fmt.Sprintf(`{"code":%q}`, code))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("前置 production 取样应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	qcCK := e.login(n026QcOpenID)
	resp, body = e.do("GET", "/api/sample/samples", qcCK, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("★★ qc 查样品列表应 200（READ），实际 %d body=%v", resp.StatusCode, body)
	}
	if numOf(body["count"]) < 1 {
		t.Fatalf("列表应含 production 刚取的样品，实际 count=%v", body["count"])
	}
}

// TC-N026-04 验收④：重启后 GET /api/admin/permission-matrix
// ⇒ sample.take: production=ALL, qc=READ。
func TestTC_N026_04_MatrixAfterRestart(t *testing.T) {
	e, _ := n026Env(t)
	adminCK := e.login(n026AdminID)

	e.restart()
	adminCK = e.login(n026AdminID)

	resp, body := e.do("GET", "/api/admin/permission-matrix", adminCK, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查矩阵应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	cells, _ := body["cells"].([]interface{})
	if len(cells) == 0 {
		t.Fatalf("矩阵响应缺 cells，body=%v", body)
	}
	got := map[string]string{}
	for _, r := range cells {
		m, _ := r.(map[string]interface{})
		if fmt.Sprint(m["point_code"]) != "sample.take" {
			continue
		}
		got[fmt.Sprint(m["role_code"])] = fmt.Sprint(m["level"])
	}
	if got["production"] != "ALL" || got["qc"] != "READ" {
		t.Fatalf("★★ sample.take 矩阵应 production=ALL / qc=READ，实际 production=%q qc=%q（sample.take 格=%v）",
			got["production"], got["qc"], got)
	}
}

// TC-N026-05 引擎口径：两角色在 sample.take 上的 Allows 判定与种子一致。
// （空库迁移落新值由 spec seed + migrations 逐字节副本 + INSERT IGNORE 机制背书；
//
//	共享库上由 n026SeedTake 归位，本条钉引擎行为不随实现漂移。）
func TestTC_N026_05_EngineAllows(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	prodLevels, err := st.LevelsFor(ctx, n026ProdOpenID, permission.SampleTake)
	if err != nil {
		// 账号尚未 EnsureUser 时 LevelsFor 可能空集合——先绑角色再查
		bindRole(t, st, n026ProdOpenID, "production")
		bindRole(t, st, n026QcOpenID, "qc")
		n026SeedTake(t, st)
		prodLevels, err = st.LevelsFor(ctx, n026ProdOpenID, permission.SampleTake)
		if err != nil {
			t.Fatalf("查 production 级别失败: %v", err)
		}
	}
	qcLevels, err := st.LevelsFor(ctx, n026QcOpenID, permission.SampleTake)
	if err != nil {
		t.Fatalf("查 qc 级别失败: %v", err)
	}
	if !prodLevels.Allows("ALL") {
		t.Fatalf("production 应 Allows(ALL)，实际 %v", prodLevels)
	}
	if !prodLevels.Allows("READ") {
		t.Fatalf("production(ALL) 应 Allows(READ)，实际 %v", prodLevels)
	}
	if qcLevels.Allows("ALL") {
		t.Fatalf("qc 不得 Allows(ALL)（N-026：写已移交 production），实际 %v", qcLevels)
	}
	if !qcLevels.Allows("READ") {
		t.Fatalf("qc 应 Allows(READ)，实际 %v", qcLevels)
	}
}
