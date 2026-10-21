package httpapi

// ===== N-029 · APP 业务 API 批次 A+B · 接口级 TC =====
//
// ★ 覆盖任务包 D4：① 权限矩阵（每端点 × 有权/无权/未登录）② 形状断言（逐字段，
//	含结构化拦截的 reason/owner/contact）③ 等价性回归（app 与 Web 同一操作 ⇒ 库内结果一致）
//	④ home 各角色可见域 + 计数正确性 ⑤ 既有测试全绿（Web 端零改动，另有单测钉住错误结构）。
// ★ 按 docs/05：DB 集成测试本机未设 JX_TEST_DB=1 一律 Skip；无库单测（01）本机可跑。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/permission"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
)

const (
	n029RecvOpenID  = "ou_test_n029_recv"
	n029QcOpenID    = "ou_test_n029_qc"
	n029ProdOpenID  = "ou_test_n029_prod"
	n029SalesOpenID = "ou_test_n029_sales"
	n029SysOpenID   = "ou_test_n029_sys"
	n029CustCode    = "9402"
	n029MatCode     = "9402"
	n029Prefix      = "ou_test_n029"
)

// n029BlockKeys 结构化拦截的四个必有字段（D0）。
var n029BlockKeys = []string{"error", "reason", "owner", "contact"}

// n029AssertBlock 断言 body 恰为结构化拦截四字段且逐字段非空。
func n029AssertBlock(t *testing.T, when string, body map[string]interface{}) {
	t.Helper()
	for _, k := range n029BlockKeys {
		v, ok := body[k]
		if !ok {
			t.Fatalf("%s：响应缺字段 %q（body=%v）", when, k, body)
		}
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			t.Fatalf("%s：字段 %q 必须是非空字符串（body=%v）", when, k, body)
		}
	}
	if len(body) != len(n029BlockKeys) {
		t.Fatalf("%s：拦截响应应恰为四字段，实际 %d 字段（body=%v）", when, len(body), body)
	}
}

// n029AssertKeys 断言 body 含全部给定字段（形状断言用）。
func n029AssertKeys(t *testing.T, when string, body map[string]interface{}, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := body[k]; !ok {
			t.Fatalf("%s：响应缺字段 %q（body=%v）", when, k, body)
		}
	}
}

// ===== 无库单测（本机可跑）：结构化拦截与 Web 端零改动 =====

// TC-N029-01 形状：appIntercept 把 401/403 改写为 D0 四字段；
// ★ 不挂该中间件的 Web 路由错误结构保持原样（point/need/hint 不丢）。
func TestTC_N029_01_AppInterceptShape_NoDB(t *testing.T) {
	newEcho := func() *echo.Echo {
		e := echo.New()
		e.HideBanner = true
		e.HTTPErrorHandler = jsonErrorHandler(e)
		return e
	}
	do := func(e *echo.Echo, path string) (int, map[string]interface{}) {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		out := map[string]interface{}{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// ① APP 路由（挂 appIntercept）：401 ⇒ 四字段
	e := newEcho()
	e.GET("/api/app/x", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusUnauthorized, "未登录或会话已失效")
	}, appIntercept())
	code, body := do(e, "/api/app/x")
	if code != http.StatusUnauthorized {
		t.Fatalf("401 实际 %d", code)
	}
	n029AssertBlock(t, "app 401", body)
	if body["error"] != "未登录" {
		t.Fatalf("app 401 error 应为「未登录」，实际 %v", body["error"])
	}

	// ② APP 路由：403（RequirePerm 原形状）⇒ 改写为四字段且不带 point/need/hint
	e = newEcho()
	e.GET("/api/app/y", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusForbidden, map[string]interface{}{
			"error": "无权访问该入口", "point": "recv.weigh", "need": "ALL",
			"hint": "未映射角色、该角色在本权限点上为 NONE，或级别不足（deny by default）",
		})
	}, appIntercept())
	code, body = do(e, "/api/app/y")
	if code != http.StatusForbidden {
		t.Fatalf("403 实际 %d", code)
	}
	n029AssertBlock(t, "app 403", body)
	if body["error"] != "无权限" {
		t.Fatalf("app 403 error 应为「无权限」，实际 %v", body["error"])
	}
	if _, hasPoint := body["point"]; hasPoint {
		t.Fatalf("APP 拦截不应外泄 point/need/hint（形状耦合）：body=%v", body)
	}

	// ③ ★ Web 路由（不挂 appIntercept）：403 结构**原样保留**（零改动）
	e = newEcho()
	e.GET("/api/web/y", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusForbidden, map[string]interface{}{
			"error": "无权访问该入口", "point": "recv.weigh", "need": "ALL", "hint": "h",
		})
	})
	code, body = do(e, "/api/web/y")
	if code != http.StatusForbidden {
		t.Fatalf("Web 403 实际 %d", code)
	}
	for _, k := range []string{"error", "point", "need", "hint"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("★ Web 端错误结构被改动：缺 %q（body=%v）", k, body)
		}
	}
	if _, hasOwner := body["owner"]; hasOwner {
		t.Fatalf("★ Web 端错误结构被改动：混入了 APP 拦截字段（body=%v）", body)
	}

	// ④ appErr 复用 recvErr 分类 ⇒ 同一 store 错误在 APP 侧得到 400 + 四字段
	err := appErr(store.ErrReprintReason, ownerRecv, contactRecv)
	he, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("appErr 应返回 *echo.HTTPError")
	}
	if he.Code != http.StatusBadRequest {
		t.Fatalf("ErrReprintReason 应 400，实际 %d", he.Code)
	}
	m, ok := he.Message.(map[string]interface{})
	if !ok {
		t.Fatalf("appErr 消息应为 map（结构化通道），实际 %T", he.Message)
	}
	n029AssertBlock(t, "appErr 400", map[string]interface{}{
		"error": fmt.Sprint(m["error"]), "reason": fmt.Sprint(m["reason"]),
		"owner": fmt.Sprint(m["owner"]), "contact": fmt.Sprint(m["contact"]),
	})
	if !strings.Contains(fmt.Sprint(m["error"]), "补打必须填写原因") {
		t.Fatalf("error 应保留 store 原文，实际 %v", m["error"])
	}
}

// ===== DB 集成测试 =====

// n029Wipe 清本批测试数据（★ 只删本前缀账号与测试客户 9402 的行；先袋后车防撞码）。
func n029Wipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	q := func(sql string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sql, args...); err != nil {
			t.Logf("n029 清理失败（忽略，继续）: %v", err)
		}
	}
	q(`DELETE FROM b_obj_void WHERE entity = 'b_bag'
	     AND entity_id IN (SELECT id FROM b_bag WHERE created_by LIKE 'ou\_test\_n029%')`)
	q(`DELETE FROM b_inspection WHERE created_by LIKE 'ou\_test\_n029%'`)
	q(`DELETE FROM b_label_print WHERE created_by LIKE 'ou\_test\_n029%'`)
	q(`DELETE FROM b_bag WHERE created_by LIKE 'ou\_test\_n029%'
	     OR truck_lot_id IN (SELECT id FROM b_truck_lot
	                           WHERE customer_id IN (SELECT id FROM m_customer WHERE code = ?))`,
		n029CustCode)
	q(`DELETE FROM b_truck_lot WHERE created_by LIKE 'ou\_test\_n029%'
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, n029CustCode)
	q(`DELETE FROM b_arrival_notice WHERE created_by LIKE 'ou\_test\_n029%'
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, n029CustCode)
	q(`DELETE FROM m_incoming_batch WHERE created_by LIKE 'ou\_test\_n029%'
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, n029CustCode)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, n029CustCode)
	q(`DELETE FROM m_material WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, n029MatCode)
}

// n029Env 建固定测试主数据 + 起服务 + 按角色登录（cookie 表以角色名为键）。
func n029Env(t *testing.T) (*env, map[string]string) {
	t.Helper()
	st := requireDB(t)
	n029Wipe(t, st)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'N029测试客户', '启用', 1, 1, ?)`, n029CustCode, n029RecvOpenID); err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'N029测试原料', '原料', '启用', 1, 1, ?)`, n029MatCode, n029RecvOpenID); err != nil {
		t.Fatalf("建测试物料失败: %v", err)
	}
	t.Cleanup(func() { n029Wipe(t, st) })

	bindRole(t, st, n029RecvOpenID, "receiver")
	bindRole(t, st, n029QcOpenID, "qc")
	bindRole(t, st, n029ProdOpenID, "production")
	bindRole(t, st, n029SalesOpenID, "sales")
	bindRole(t, st, n029SysOpenID, "sysadmin")

	e := newEnv(t, time.Hour)
	cks := map[string]string{
		"receiver":   e.login(n029RecvOpenID),
		"qc":         e.login(n029QcOpenID),
		"production": e.login(n029ProdOpenID),
		"sales":      e.login(n029SalesOpenID),
		"sysadmin":   e.login(n029SysOpenID),
	}
	return e, cks
}

// n029MasterIDs 取测试客户/物料当前行 id。
func n029MasterIDs(t *testing.T, st *store.Store) (int64, int64) {
	t.Helper()
	var cust, mat int64
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT id FROM m_customer WHERE code = ? AND is_current = 1`, n029CustCode).Scan(&cust); err != nil {
		t.Fatalf("查测试客户失败: %v", err)
	}
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, n029MatCode).Scan(&mat); err != nil {
		t.Fatalf("查测试物料失败: %v", err)
	}
	return cust, mat
}

// n029Batch 预填一个来料批次（走 Web 写入口，属测试数据准备），返回 batch_id。
func n029Batch(t *testing.T, e *env, ck string) int64 {
	t.Helper()
	cust, mat := n029MasterIDs(t, e.st)
	body, _ := json.Marshal(map[string]interface{}{
		"customer_id": cust, "material_id": mat,
		"customer_batch_no": fmt.Sprintf("N029-%d", time.Now().UnixNano()),
	})
	resp, out := e.do("POST", "/api/recv/batches", ck, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预填批次应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	batch, _ := out["row"].(map[string]interface{})
	return numOf(batch["id"])
}

// n029Notice 车次预报（走 Web 写入口，属测试数据准备），返回 notice_id。
func n029Notice(t *testing.T, e *env, ck string, batchID int64, plate string) int64 {
	t.Helper()
	resp, out := e.do("POST", "/api/recv/notices", ck, fmt.Sprintf(
		`{"batch_id": %d, "plate_no": %q, "driver": "司机N029"}`, batchID, plate))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预报登记应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	row, _ := out["row"].(map[string]interface{})
	return numOf(row["id"])
}

// n029Scene 预填批次 + 车次预报，返回 notice_id。
func n029Scene(t *testing.T, e *env, ck, plate string) int64 {
	t.Helper()
	return n029Notice(t, e, ck, n029Batch(t, e, ck), plate)
}

// n029AggCount 取 home in_progress 里某 key 的计数（不存在 ⇒ -1）。
func n029AggCount(t *testing.T, body map[string]interface{}, key string) int {
	t.Helper()
	list, _ := body["in_progress"].([]interface{})
	for _, it := range list {
		m, _ := it.(map[string]interface{})
		if m["key"] == key {
			return int(numOf(m["count"]))
		}
	}
	return -1
}

// n029HasKey 判断 home 聚合/待办列表里是否含某 key。
func n029HasKey(body map[string]interface{}, field, key string) bool {
	list, _ := body[field].([]interface{})
	for _, it := range list {
		m, _ := it.(map[string]interface{})
		if m["key"] == key {
			return true
		}
	}
	return false
}

// TC-N029-02 权限矩阵：六个动作端点 × 有权 / 无权 / 未登录（逐点断言）。
func TestTC_N029_02_PermissionMatrix(t *testing.T) {
	e, cks := n029Env(t)

	type probe struct {
		name   string
		method string
		path   string
		body   string
	}
	probes := []probe{
		{"notices", "GET", "/api/app/recv/notices", ""},
		{"arrive-confirm", "POST", "/api/app/recv/arrive-confirm", `{"notice_id": 999999999}`},
		{"return", "POST", "/api/app/recv/return", `{"notice_id": 999999999, "reason": "x"}`},
		{"weigh", "POST", "/api/app/recv/weigh", `{"truck_lot_id": 999999999, "gross_weight": 1, "tare_weight": 0.5}`},
		{"bag-gen", "POST", "/api/app/recv/bag-gen", `{"truck_lot_id": 999999999, "count": 1}`},
		{"label-reprint", "POST", "/api/app/label/reprint", `{"bag_code": "ZZZ", "reason": "x"}`},
		{"home", "GET", "/api/app/home", ""},
	}
	// ★ 权限矩阵按**库内实况**推导期望：权限矩阵是后台可配的（N-029 验收时实测
	//	sysadmin 已被授予收货域权限，与 spec 种子不同）⇒ 测试钉「端点 ↔ 权限点 ↔ 级别」
	//	这条映射本身，而不是钉死某个角色的格子值。
	//	每个写端点必须至少有一个角色真的被拒（否则 403 路径没被覆盖 ⇒ 判失败）。
	for _, p := range probes {
		denied := 0
		for _, role := range []string{"receiver", "qc", "sales", "sysadmin"} {
			resp, body := e.do(p.method, p.path, cks[role], p.body)
			if p.name == "home" {
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("home：角色 %s 应 200（登录即达），实际 %d body=%v",
						role, resp.StatusCode, body)
				}
				continue
			}
			needLevel := "READ"
			if p.name != "notices" {
				needLevel = "ALL"
			}
			allowed := n029Allows(t, e, n029OpenIDs[role], n029EndpointPerm[p.name], needLevel)
			if allowed {
				// 有权 ⇒ 必须**打穿守卫**（200 或业务 4xx，绝不 401/403）
				if resp.StatusCode == http.StatusUnauthorized ||
					resp.StatusCode == http.StatusForbidden {
					t.Fatalf("%s：角色 %s 有权（%s≥%s）却被守卫拦下（%d body=%v）",
						p.name, role, n029EndpointPerm[p.name], needLevel, resp.StatusCode, body)
				}
				continue
			}
			denied++
			// 无权 ⇒ 403 + 结构化四字段（error=无权限）
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s：角色 %s 应 403，实际 %d body=%v", p.name, role, resp.StatusCode, body)
			}
			n029AssertBlock(t, p.name+"/"+role, body)
			if body["error"] != "无权限" {
				t.Fatalf("%s/%s：error 应为「无权限」，实际 %v", p.name, role, body["error"])
			}
			if body["owner"] != ownerPerm {
				t.Fatalf("%s/%s：owner 应为 %q，实际 %v", p.name, role, ownerPerm, body["owner"])
			}
		}
		if p.name != "notices" && p.name != "home" && denied == 0 {
			t.Fatalf("★ %s：矩阵里没有一个角色被拒 ⇒ 403 路径未被覆盖", p.name)
		}
		// ③ 未登录 ⇒ 401 + 结构化四字段（error=未登录）
		resp, body := e.do(p.method, p.path, "", p.body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s：未登录应 401，实际 %d body=%v", p.name, resp.StatusCode, body)
		}
		n029AssertBlock(t, p.name+"/匿名", body)
		if body["error"] != "未登录" {
			t.Fatalf("%s：匿名 error 应为「未登录」，实际 %v", p.name, body["error"])
		}
	}
}

// n029OpenIDs 角色 → 测试账号（权限矩阵推导用）。
var n029OpenIDs = map[string]string{
	"receiver":   n029RecvOpenID,
	"qc":         n029QcOpenID,
	"production": n029ProdOpenID,
	"sales":      n029SalesOpenID,
	"sysadmin":   n029SysOpenID,
}

// n029EndpointPerm 端点 → 现场权限点（★ 与实现同表，测试侧独立声明以便互相钉住）。
var n029EndpointPerm = map[string]string{
	"notices":        "recv.notice.create",
	"arrive-confirm": "recv.arrive.confirm",
	"return":         "recv.return",
	"weigh":          "recv.weigh",
	"bag-gen":        "recv.bag.gen",
	"label-reprint":  "recv.label.reprint",
}

// n029Allows 查库内该账号在某权限点是否达到给定级别。
func n029Allows(t *testing.T, e *env, openID, point, level string) bool {
	t.Helper()
	lv, err := e.st.LevelsFor(context.Background(), openID, permission.Code(point))
	if err != nil {
		t.Fatalf("查授权失败（%s/%s）: %v", openID, point, err)
	}
	return lv.Allows(level)
}

// n029TodoCount 取 home todos 里某 key 的计数（不存在 ⇒ -1）。
func n029TodoCount(t *testing.T, body map[string]interface{}, key string) int {
	t.Helper()
	list, _ := body["todos"].([]interface{})
	for _, it := range list {
		m, _ := it.(map[string]interface{})
		if m["key"] == key {
			return int(numOf(m["count"]))
		}
	}
	return -1
}

// n029HomeGate home 聚合/待办 → 现场权限点（测试侧独立声明，与实现侧 appAggGate/appTodoGate 互钉）。
var n029HomeGate = map[string]string{
	"notice_open":   "recv.notice.create",
	"batch_open":    "recv.notice.create",
	"bag_in_stock":  "recv.bag.gen",
	"truck_pending": "insp.task.view",
	"prod_running":  "prod.batch.create",
	"fg_bag_ready":  "ship.load.scan",
	"ship_open":     "ship.load.scan",
	"await_receive": "recv.notice.create",
	"await_label":   "recv.bag.gen",
	"await_load":    "ship.load.scan",
}

// TC-N029-03 首页聚合：各角色可见域断言 + 计数正确性 + 最近作业按 actor 过滤。
func TestTC_N029_03_HomeAggregationByRole(t *testing.T) {
	e, cks := n029Env(t)

	// ① receiver：形状逐字段 + 可见域（prod.batch.create=NONE ⇒ 不见生产批）
	resp, body := e.do("GET", "/api/app/home", cks["receiver"], "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("home 应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	n029AssertKeys(t, "home", body,
		"me", "domains", "in_progress", "todos", "recent", "recent_source", "generated_at")
	me, _ := body["me"].(map[string]interface{})
	if me["open_id"] != n029RecvOpenID {
		t.Fatalf("me.open_id 应为 %s，实际 %v", n029RecvOpenID, me["open_id"])
	}
	hasRole := false
	if roles, ok := me["roles"].([]interface{}); ok {
		for _, r := range roles {
			if r == "receiver" {
				hasRole = true
			}
		}
	}
	if !hasRole {
		t.Fatalf("me.roles 应含 receiver，实际 %v", me["roles"])
	}
	if got, _ := body["todos"].([]interface{}); len(got) > 0 {
		firstTodo, _ := got[0].(map[string]interface{})
		n029AssertKeys(t, "todos[0]", firstTodo, "key", "label", "domain", "count")
		for _, k := range []string{"label", "domain", "key"} {
			if s, _ := firstTodo[k].(string); strings.TrimSpace(s) == "" {
				t.Fatalf("todos[0].%s 不应为空", k)
			}
		}
	}
	list, _ := body["in_progress"].([]interface{})
	if len(list) > 0 {
		first, _ := list[0].(map[string]interface{})
		n029AssertKeys(t, "in_progress[0]", first, "key", "label", "domain", "count", "latest")
		// ★ 逐字段必须非空（形状断言）
		for _, k := range []string{"label", "domain", "key"} {
			if s, _ := first[k].(string); strings.TrimSpace(s) == "" {
				t.Fatalf("in_progress[0].%s 不应为空", k)
			}
		}
	}

	// ② 计数正确性：新增一条预报 ⇒ notice_open +1、待收车 +1、latest 指向该预报
	beforeAgg := n029AggCount(t, body, "notice_open")
	beforeTodo := n029TodoCount(t, body, "await_receive")
	noticeID := n029Scene(t, e, cks["receiver"], "湘F·N029H")

	resp, body = e.do("GET", "/api/app/home", cks["receiver"], "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("home(2) 应 200，实际 %d", resp.StatusCode)
	}
	if got := n029AggCount(t, body, "notice_open"); got != beforeAgg+1 {
		t.Fatalf("★ notice_open 计数应 %d，实际 %d（数据源/过滤有误）", beforeAgg+1, got)
	}
	if got := n029TodoCount(t, body, "await_receive"); got != beforeTodo+1 {
		t.Fatalf("★ 待收车计数应 %d，实际 %d", beforeTodo+1, got)
	}
	it, _ := body["in_progress"].([]interface{})
	for _, raw := range it {
		m, _ := raw.(map[string]interface{})
		if m["key"] != "notice_open" {
			continue
		}
		latest, _ := m["latest"].(map[string]interface{})
		if latest == nil {
			t.Fatalf("notice_open 有数据时 latest 不应为 null")
		}
		n029AssertKeys(t, "notice_open.latest", latest, "id", "code", "title", "status", "at")
		if numOf(latest["id"]) != noticeID {
			t.Fatalf("latest.id 应为新建预报 %d，实际 %v", noticeID, latest["id"])
		}
		if latest["status"] != "预报" {
			t.Fatalf("latest.status 应为「预报」，实际 %v", latest["status"])
		}
		if strings.TrimSpace(fmt.Sprint(latest["at"])) == "" {
			t.Fatalf("latest.at 不应为空")
		}
	}

	// ③ 各角色可见域：期望 = 库内该账号在**对应现场权限点**上是否达到 READ 级。
	//	（权限矩阵后台可配、验收库实况 ≠ spec 种子 ⇒ 钉「端点↔权限点↔可见性」这条映射本身）
	for _, role := range []string{"receiver", "qc", "production", "sales", "sysadmin"} {
		resp, body = e.do("GET", "/api/app/home", cks[role], "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("home（%s）应 200，实际 %d body=%v", role, resp.StatusCode, body)
		}
		wantDomains := map[string]bool{}
		for _, key := range appAggOrder {
			want := n029Allows(t, e, n029OpenIDs[role], n029HomeGate[key], access.LevelRead)
			got := n029HasKey(body, "in_progress", key)
			if want != got {
				t.Fatalf("★ 可见性不符：角色 %s 的 %s（权限点 %s，期望可见=%v，实际=%v）",
					role, key, n029HomeGate[key], want, got)
			}
			if got {
				wantDomains[appAggMeta[key].domain] = true
			}
		}
		for _, key := range appTodoOrder {
			want := n029Allows(t, e, n029OpenIDs[role], n029HomeGate[key], access.LevelRead)
			got := n029HasKey(body, "todos", key)
			if want != got {
				t.Fatalf("★ 可见性不符：角色 %s 的 %s（权限点 %s，期望可见=%v，实际=%v）",
					role, key, n029HomeGate[key], want, got)
			}
			if got {
				wantDomains[appTodoMeta[key].domain] = true
			}
		}
		// domains 数组必须与可见项的域集合**完全一致**
		gotDomains := map[string]bool{}
		if arr, ok := body["domains"].([]interface{}); ok {
			for _, d := range arr {
				gotDomains[fmt.Sprint(d)] = true
			}
		}
		if len(gotDomains) != len(wantDomains) {
			t.Fatalf("★ domains 不符（%s）：期望 %v 实际 %v", role, wantDomains, body["domains"])
		}
		for d := range wantDomains {
			if !gotDomains[d] {
				t.Fatalf("★ domains 缺失 %q（%s，实际 %v）", d, role, body["domains"])
			}
		}
	}
	// receiver 至少要看到收货域（本测试以 receiver 建数据，收货域全点为 ALL 是验收口径）
	resp, body = e.do("GET", "/api/app/home", cks["receiver"], "")
	if !n029HasKey(body, "in_progress", "notice_open") {
		t.Fatalf("receiver 应可见 notice_open（recv.notice.create 应达 READ），实际 %v", body["in_progress"])
	}

	// ⑥ 最近作业按 actor 过滤：receiver 有本批操作；sysadmin 只有自己的登录留痕
	resp, body = e.do("GET", "/api/app/home", cks["receiver"], "")
	recent, _ := body["recent"].([]interface{})
	if len(recent) == 0 {
		t.Fatalf("★ receiver 应有本人最近作业（本测试刚做了预填/预报/确认）")
	}
	ownBiz := 0
	for _, raw := range recent {
		m, _ := raw.(map[string]interface{})
		n029AssertKeys(t, "recent[0]", m,
			"id", "at", "entity", "entity_id", "action", "field", "new_value", "reason", "actor_open_id", "actor")
		if m["actor_open_id"] != n029RecvOpenID {
			t.Fatalf("★ recent 只应含本人记录，实际 actor=%v", m["actor_open_id"])
		}
		switch m["entity"] {
		case "m_incoming_batch", "b_arrival_notice", "b_truck_lot":
			ownBiz++
		}
	}
	if ownBiz == 0 {
		t.Fatalf("★ recent 应含本批业务动作，实际 %v", recent)
	}
	resp, body = e.do("GET", "/api/app/home", cks["sysadmin"], "")
	recent, _ = body["recent"].([]interface{})
	for _, raw := range recent {
		m, _ := raw.(map[string]interface{})
		if m["actor_open_id"] != n029SysOpenID {
			t.Fatalf("★ recent 未按 actor 过滤：sysadmin 看到了别人的记录 %v", m)
		}
	}

	// ⑦ 未登录 ⇒ 401 + 结构化四字段
	resp, body = e.do("GET", "/api/app/home", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名 home 应 401，实际 %d", resp.StatusCode)
	}
	n029AssertBlock(t, "home/匿名", body)
}

// TC-N029-04 预报列表：逐字段形状 + 与 Web 端同源（同一 store.ListNotices）。
func TestTC_N029_04_NoticesShapeAndSource(t *testing.T) {
	e, cks := n029Env(t)
	ck := cks["receiver"]
	noticeID := n029Scene(t, e, ck, "湘F·N029N")

	resp, body := e.do("GET", "/api/app/recv/notices", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("notices 应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	n029AssertKeys(t, "notices", body, "count", "notices")
	list, _ := body["notices"].([]interface{})
	if int(numOf(body["count"])) != len(list) {
		t.Fatalf("count 应等于行数：%v vs %d", body["count"], len(list))
	}
	found := false
	for _, raw := range list {
		m, _ := raw.(map[string]interface{})
		n029AssertKeys(t, "notice row", m,
			"notice_id", "notice_no", "batch_id", "our_batch_no", "customer_batch_no",
			"customer_name", "material_name", "seq_no", "plate_no", "driver", "phone",
			"status", "arrive_date", "legacy")
		if numOf(m["notice_id"]) == noticeID {
			found = true
			if m["plate_no"] != "湘F·N029N" {
				t.Fatalf("plate_no 应为 湘F·N029N，实际 %v", m["plate_no"])
			}
			if m["status"] != "预报" {
				t.Fatalf("新建预报 status 应为「预报」，实际 %v", m["status"])
			}
			if numOf(m["seq_no"]) <= 0 {
				t.Fatalf("seq_no（车序）应由后端分配且 >0，实际 %v", m["seq_no"])
			}
			if _, ok := m["our_batch_no"].(string); !ok {
				t.Fatalf("our_batch_no 应为字符串，实际 %T", m["our_batch_no"])
			}
		}
	}
	if !found {
		t.Fatalf("列表应含新建预报 %d", noticeID)
	}

	// mode 过滤（与 Web 同语义）
	for _, q := range []string{"?mode=pending", "?mode=open"} {
		resp, body = e.do("GET", "/api/app/recv/notices"+q, ck, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("notices%s 应 200，实际 %d", q, resp.StatusCode)
		}
		list, _ = body["notices"].([]interface{})
		hit := false
		for _, raw := range list {
			m, _ := raw.(map[string]interface{})
			if numOf(m["notice_id"]) == noticeID {
				hit = true
			}
		}
		if !hit {
			t.Fatalf("notices%s 应含批次预报 %d", q, noticeID)
		}
	}

	// ★ 等价性（数据源同源）：APP 与 Web 返回的 notice_id 集合完全一致
	resp, webBody := e.do("GET", "/api/recv/notices", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Web notices 应 200，实际 %d", resp.StatusCode)
	}
	appIDs := map[int64]bool{}
	for _, raw := range list {
		m, _ := raw.(map[string]interface{})
		appIDs[numOf(m["notice_id"])] = true
	}
	resp, body = e.do("GET", "/api/app/recv/notices", ck, "")
	list, _ = body["notices"].([]interface{})
	for _, raw := range list {
		m, _ := raw.(map[string]interface{})
		appIDs[numOf(m["notice_id"])] = true
	}
	webRows, _ := webBody["rows"].([]interface{})
	if len(webRows) != len(appIDs) {
		t.Fatalf("★ 同源断言：Web %d 行 ≠ APP %d 行", len(webRows), len(appIDs))
	}
	for _, raw := range webRows {
		m, _ := raw.(map[string]interface{})
		if !appIDs[numOf(m["id"])] {
			t.Fatalf("★ Web 行 %v 未出现在 APP 列表", m["id"])
		}
	}
}

// TC-N029-05 结构化拦截三类：无预报 / 无车辆档案 / 无权限（+ 其余 4xx 同形）。
func TestTC_N029_05_StructuredInterceptions(t *testing.T) {
	e, cks := n029Env(t)
	ck := cks["receiver"]

	// ① 无预报：到货确认 / 退车
	for _, p := range []struct {
		name string
		path string
		body string
	}{
		{"arrive-confirm", "/api/app/recv/arrive-confirm", `{"notice_id": 42424242}`},
		{"return", "/api/app/recv/return", `{"notice_id": 42424242, "reason": "取消"}`},
	} {
		resp, body := e.do("POST", p.path, ck, p.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s：无预报应 404，实际 %d body=%v", p.name, resp.StatusCode, body)
		}
		n029AssertBlock(t, p.name+"/无预报", body)
		if body["error"] != "无预报" {
			t.Fatalf("%s：error 应为「无预报」，实际 %v", p.name, body["error"])
		}
		if body["owner"] != ownerNotice {
			t.Fatalf("%s：owner 应为 %q，实际 %v", p.name, ownerNotice, body["owner"])
		}
	}

	// ② 无车辆档案：预报引用了不存在的 vehicle_id
	batchID := n029Batch(t, e, ck)
	cust, mat := n029MasterIDs(t, e.st)
	ins := func(vehicleID int64) int64 {
		t.Helper()
		res, err := e.st.DB().ExecContext(context.Background(), `
INSERT INTO b_arrival_notice (notice_no, batch_id, customer_id, material_id, vehicle_id,
                              plate_no, driver, biz_type, status, created_by)
VALUES (?, ?, ?, ?, ?, '湘F·N029V', '司机N029V', 'CG', '预报', ?)`,
			fmt.Sprintf("N029V-%d", time.Now().UnixNano()), batchID, cust, mat, vehicleID, n029RecvOpenID)
		if err != nil {
			t.Fatalf("造预报失败: %v", err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	badNotice := ins(987654321)
	resp, body := e.do("POST", "/api/app/recv/arrive-confirm", ck,
		fmt.Sprintf(`{"notice_id": %d}`, badNotice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("无车辆档案应 404，实际 %d body=%v", resp.StatusCode, body)
	}
	n029AssertBlock(t, "arrive-confirm/无车辆档案", body)
	if body["error"] != "无车辆档案" {
		t.Fatalf("error 应为「无车辆档案」，实际 %v", body["error"])
	}
	if body["owner"] != ownerVehicle {
		t.Fatalf("owner 应为 %q，实际 %v", ownerVehicle, body["owner"])
	}
	// ★ Web 端不校验车辆档案（行为不变）⇒ 同款预报在 Web 端照常确认成功
	webNotice := ins(987654321)
	resp, body = e.do("POST", "/api/recv/arrivals", ck,
		fmt.Sprintf(`{"notice_id": %d}`, webNotice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("★ Web 端行为被改动（应 200 不校验车辆档案），实际 %d body=%v", resp.StatusCode, body)
	}

	// ③ 无权限：qc 打 arrive-confirm（NONE）
	resp, body = e.do("POST", "/api/app/recv/arrive-confirm", cks["qc"], `{"notice_id": 1}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("无权限应 403，实际 %d body=%v", resp.StatusCode, body)
	}
	n029AssertBlock(t, "arrive-confirm/无权限", body)
	if body["error"] != "无权限" || body["owner"] != ownerPerm {
		t.Fatalf("无权限映射不符：error=%v owner=%v", body["error"], body["owner"])
	}

	// ④ 未登录
	resp, body = e.do("POST", "/api/app/recv/weigh", "",
		`{"truck_lot_id": 1, "gross_weight": 1, "tare_weight": 0.5}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d body=%v", resp.StatusCode, body)
	}
	n029AssertBlock(t, "weigh/未登录", body)

	// ⑤ 其余 4xx 同形：车次不存在（404）、补打缺原因（400）、无质检退货判定（409）
	resp, body = e.do("POST", "/api/app/recv/bag-gen", ck, `{"truck_lot_id": 777777777, "count": 2}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("车次不存在应 404，实际 %d body=%v", resp.StatusCode, body)
	}
	n029AssertBlock(t, "bag-gen/404", body)

	resp, body = e.do("POST", "/api/app/label/reprint", ck,
		`{"bag_code": "BJ0000000000000000000000000"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("补打缺原因应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	n029AssertBlock(t, "reprint/400", body)
	if !strings.Contains(fmt.Sprint(body["error"]), "补打必须填写原因") {
		t.Fatalf("error 应保留 store 原文，实际 %v", body["error"])
	}

	noticeID := n029Scene(t, e, ck, "湘F·N029R")
	resp, out := e.do("POST", "/api/app/recv/arrive-confirm", ck,
		fmt.Sprintf(`{"notice_id": %d}`, noticeID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("到货确认应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	resp, body = e.do("POST", "/api/app/recv/return", ck,
		fmt.Sprintf(`{"notice_id": %d, "reason": "不合格"}`, noticeID))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("无质检退货判定应 409，实际 %d body=%v", resp.StatusCode, body)
	}
	n029AssertBlock(t, "return/409", body)
	if !strings.Contains(fmt.Sprint(body["error"]), "退货") {
		t.Fatalf("error 应含质检退货判定文案，实际 %v", body["error"])
	}
}

// ===== 等价性与场景辅助 =====

// n029TruckSnapshot 读车次关键字段（等价性比对用；不含 id/码等天然序列字段）。
func n029TruckSnapshot(t *testing.T, st *store.Store, id int64) map[string]string {
	t.Helper()
	var status, date, gross, tare, net, bags string
	if err := st.DB().QueryRowContext(context.Background(), `
SELECT status, DATE_FORMAT(arrive_date, '%Y-%m-%d'),
       IFNULL(CAST(gross_weight AS CHAR), ''), IFNULL(CAST(tare_weight AS CHAR), ''),
       IFNULL(CAST(net_weight AS CHAR), ''), CAST(bag_count AS CHAR)
  FROM b_truck_lot WHERE id = ?`, id).
		Scan(&status, &date, &gross, &tare, &net, &bags); err != nil {
		t.Fatalf("读车次快照失败: %v", err)
	}
	return map[string]string{
		"status": status, "arrive_date": date, "gross_weight": gross,
		"tare_weight": tare, "net_weight": net, "bag_count": bags,
	}
}

// n029NoticeSnapshot 读预报状态与链根日期。
func n029NoticeSnapshot(t *testing.T, st *store.Store, id int64) (string, string) {
	t.Helper()
	var status, date string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT status, IFNULL(DATE_FORMAT(arrive_date, '%Y-%m-%d'), '')
		   FROM b_arrival_notice WHERE id = ?`, id).Scan(&status, &date); err != nil {
		t.Fatalf("读预报快照失败: %v", err)
	}
	return status, date
}

// n029BagSnapshot 读一车的袋快照（按 bag_seq 升序）。
func n029BagSnapshot(t *testing.T, st *store.Store, truckID int64) []map[string]string {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(), `
SELECT bag_seq, code, IFNULL(CAST(weight_allocated AS CHAR), ''), status
  FROM b_bag WHERE truck_lot_id = ? ORDER BY bag_seq`, truckID)
	if err != nil {
		t.Fatalf("读袋快照失败: %v", err)
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var seq, alloc, status, code string
		if err := rows.Scan(&seq, &code, &alloc, &status); err != nil {
			t.Fatalf("读袋快照行失败: %v", err)
		}
		out = append(out, map[string]string{
			"bag_seq": seq, "code": code, "weight_allocated": alloc, "status": status,
		})
	}
	return out
}

// n029VoidCount 统计某车被作废的袋数（退车等价性用）。
func n029VoidCount(t *testing.T, st *store.Store, truckID int64) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(), `
SELECT COUNT(*) FROM b_obj_void v
  JOIN b_bag b ON b.id = v.entity_id
 WHERE v.entity = 'b_bag' AND b.truck_lot_id = ?`, truckID).Scan(&n); err != nil {
		t.Fatalf("统计作废失败: %v", err)
	}
	return n
}

// TC-N029-06 等价性回归：同一操作走 Web 端与 APP 端 ⇒ 数据库结果一致（同一 store 方法）。
func TestTC_N029_06_WebAppEquivalence(t *testing.T) {
	e, cks := n029Env(t)
	ck := cks["receiver"]

	// —— 场景 A：Web 端（预报 → 到货 → 过磅 → 打码 → 补打）——
	noticeA := n029Scene(t, e, ck, "湘F·N029W")
	resp, out := e.do("POST", "/api/recv/arrivals", ck, fmt.Sprintf(`{"notice_id": %d}`, noticeA))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Web 到货确认应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	rowA, _ := out["row"].(map[string]interface{})
	idA := numOf(rowA["id"])
	if resp, out = e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/weigh", idA), ck,
		`{"gross_weight": 10.5, "tare_weight": 3.5}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("Web 过磅应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	if resp, out = e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/bags", idA), ck,
		`{"count": 3}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("Web 打码应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	rowsA, _ := out["rows"].([]interface{})
	if len(rowsA) != 3 {
		t.Fatalf("Web 应生成 3 袋，实际 %d", len(rowsA))
	}
	codeA, _ := rowsA[0].(map[string]interface{})["code"].(string)
	if resp, out = e.do("POST", "/api/recv/labels/reprint", ck,
		fmt.Sprintf(`{"codes": [%q], "reason": "N029 标签破损"}`, codeA)); resp.StatusCode != http.StatusOK {
		t.Fatalf("Web 补打应 200，实际 %d body=%v", resp.StatusCode, out)
	}

	// —— 场景 B：APP 端（同一批动作）——
	noticeB := n029Scene(t, e, ck, "湘F·N029C")
	resp, out = e.do("POST", "/api/app/recv/arrive-confirm", ck, fmt.Sprintf(`{"notice_id": %d}`, noticeB))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 到货确认应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	n029AssertKeys(t, "arrive-confirm", out,
		"confirmed", "truck_lot_id", "truck_code", "truck_human",
		"notice_id", "arrive_date", "seq_no", "plate_no", "status")
	codeB := fmt.Sprint(out["truck_code"])
	if len(codeB) != 27 {
		t.Fatalf("★ 车次码应 27 位（机制不变），实际 %d：%s", len(codeB), codeB)
	}
	if fmt.Sprint(out["arrive_date"]) == "" {
		t.Fatalf("arrive_date 不应为空")
	}
	idB := numOf(out["truck_lot_id"])

	if resp, out = e.do("POST", "/api/app/recv/weigh", ck, fmt.Sprintf(
		`{"truck_lot_id": %d, "gross_weight": 10.5, "tare_weight": 3.5}`, idB)); resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 过磅应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	if out["tare_source"] != "input" {
		t.Fatalf("显式皮重时 tare_source 应为 input，实际 %v", out["tare_source"])
	}
	resp, out = e.do("POST", "/api/app/recv/bag-gen", ck,
		fmt.Sprintf(`{"truck_lot_id": %d, "count": 3}`, idB))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 打码应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	if int(numOf(out["generated"])) != 3 {
		t.Fatalf("APP 应生成 3 袋，实际 %v", out["generated"])
	}
	bagsB, _ := out["bags"].([]interface{})
	if len(bagsB) != 3 {
		t.Fatalf("APP bags 应 3 行，实际 %d", len(bagsB))
	}
	codeB0, _ := bagsB[0].(map[string]interface{})["code"].(string)
	if resp, out = e.do("POST", "/api/app/label/reprint", ck,
		fmt.Sprintf(`{"bag_code": %q, "reason": "N029 标签破损"}`, codeB0)); resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 补打应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	if int(numOf(out["count"])) != 1 {
		t.Fatalf("APP 补打应 1 条记录，实际 %v", out["count"])
	}

	// —— 数据库结果逐字段一致 ——
	snapA, snapB := n029TruckSnapshot(t, e.st, idA), n029TruckSnapshot(t, e.st, idB)
	for _, k := range []string{"status", "arrive_date", "gross_weight", "tare_weight", "net_weight", "bag_count"} {
		if snapA[k] != snapB[k] {
			t.Fatalf("★ 等价性：b_truck_lot.%s Web=%q APP=%q", k, snapA[k], snapB[k])
		}
	}
	if snapA["net_weight"] != "7.000" {
		t.Fatalf("净重应 7.000（10.5-3.5，实测口径），实际 %q", snapA["net_weight"])
	}
	stA, arrA := n029NoticeSnapshot(t, e.st, noticeA)
	stB, arrB := n029NoticeSnapshot(t, e.st, noticeB)
	if stA != "已到货" || stB != "已到货" {
		t.Fatalf("预报状态应均为「已到货」，实际 Web=%q APP=%q", stA, stB)
	}
	if arrA == "" || arrA != arrB {
		t.Fatalf("★ 链根日期应一致且非空，Web=%q APP=%q", arrA, arrB)
	}

	bagA, bagB := n029BagSnapshot(t, e.st, idA), n029BagSnapshot(t, e.st, idB)
	if len(bagA) != len(bagB) || len(bagA) != 3 {
		t.Fatalf("袋数不一致：Web=%d APP=%d", len(bagA), len(bagB))
	}
	for i := range bagA {
		if bagA[i]["bag_seq"] != bagB[i]["bag_seq"] ||
			bagA[i]["weight_allocated"] != bagB[i]["weight_allocated"] ||
			bagA[i]["status"] != bagB[i]["status"] {
			t.Fatalf("★ 等价性：袋 #%d Web=%v APP=%v", i+1, bagA[i], bagB[i])
		}
		if len(bagA[i]["code"]) != 27 || len(bagB[i]["code"]) != 27 {
			t.Fatalf("吨袋码应 27 位：%q / %q", bagA[i]["code"], bagB[i]["code"])
		}
	}
	if bagA[0]["weight_allocated"] != "2.333" {
		t.Fatalf("摊算重量应 2.333（7.000÷3，half-up），实际 %q", bagA[0]["weight_allocated"])
	}

	// 补打留痕一致
	for _, code := range []string{codeA, codeB0} {
		var isReprint, n int
		var reason string
		if err := e.st.DB().QueryRowContext(context.Background(),
			`SELECT COUNT(*), IFNULL(MAX(is_reprint), 0), IFNULL(MAX(reason), '')
			   FROM b_label_print WHERE code = ?`, code).Scan(&n, &isReprint, &reason); err != nil {
			t.Fatalf("读补打记录失败: %v", err)
		}
		if n < 1 || isReprint != 1 {
			t.Fatalf("码 %s 补打留痕不符：n=%d is_reprint=%d", code, n, isReprint)
		}
		if reason != "N029 标签破损" {
			t.Fatalf("★ 补打原因应一致，实际 %q", reason)
		}
	}
}

// TC-N029-07 过磅皮重：显式入参 / 取该车历史 / 无历史要求人工录入（三口径逐一钉住）。
func TestTC_N029_07_WeighTareHistory(t *testing.T) {
	e, cks := n029Env(t)
	ck := cks["receiver"]

	// ① 同车牌第一车：Web 端过磅（皮重 3.25）→ 形成历史
	notice1 := n029Scene(t, e, ck, "湘F·N029T")
	resp, out := e.do("POST", "/api/recv/arrivals", ck, fmt.Sprintf(`{"notice_id": %d}`, notice1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Web 到货应 200，实际 %d", resp.StatusCode)
	}
	id1 := numOf(out["row"].(map[string]interface{})["id"])
	if resp, out = e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/weigh", id1), ck,
		`{"gross_weight": 12, "tare_weight": 3.25}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("Web 过磅应 200，实际 %d body=%v", resp.StatusCode, out)
	}

	// ② 同车牌第二车：APP 只传毛重 ⇒ 皮重取历史（tare_source=history）
	notice2 := n029Scene(t, e, ck, "湘F·N029T")
	resp, out = e.do("POST", "/api/app/recv/arrive-confirm", ck, fmt.Sprintf(`{"notice_id": %d}`, notice2))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 到货应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	id2 := numOf(out["truck_lot_id"])
	resp, out = e.do("POST", "/api/app/recv/weigh", ck,
		fmt.Sprintf(`{"truck_lot_id": %d, "gross_weight": 12}`, id2))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 过磅（历史皮重）应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	if out["tare_source"] != "history" {
		t.Fatalf("★ tare_source 应为 history，实际 %v", out["tare_source"])
	}
	hist, _ := out["tare_history"].(map[string]interface{})
	if hist == nil || numOf(hist["truck_id"]) != id1 {
		t.Fatalf("tare_history 应指向第一车 %d，实际 %v", id1, hist)
	}
	snap := n029TruckSnapshot(t, e.st, id2)
	if snap["tare_weight"] != "3.250" || snap["net_weight"] != "8.750" {
		t.Fatalf("★ 库内皮重/净重应 3.250 / 8.750（净重＝毛−皮，实测口径不变），实际 %v", snap)
	}

	// ③ 新车牌无历史且不传皮重 ⇒ 结构化 400（不猜值）
	notice3 := n029Scene(t, e, ck, "湘F·N029U")
	resp, out = e.do("POST", "/api/app/recv/arrive-confirm", ck, fmt.Sprintf(`{"notice_id": %d}`, notice3))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 到货应 200，实际 %d", resp.StatusCode)
	}
	id3 := numOf(out["truck_lot_id"])
	resp, out = e.do("POST", "/api/app/recv/weigh", ck,
		fmt.Sprintf(`{"truck_lot_id": %d, "gross_weight": 12}`, id3))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("无历史且缺皮重应 400，实际 %d body=%v", resp.StatusCode, out)
	}
	n029AssertBlock(t, "weigh/缺皮重", out)
	if !strings.Contains(fmt.Sprint(out["reason"]), "历史皮重") {
		t.Fatalf("reason 应说明历史皮重口径，实际 %v", out["reason"])
	}

	// ④ 显式皮重 ⇒ tare_source=input（与 Web 端同源）
	notice4 := n029Scene(t, e, ck, "湘F·N029S")
	resp, out = e.do("POST", "/api/app/recv/arrive-confirm", ck, fmt.Sprintf(`{"notice_id": %d}`, notice4))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 到货应 200，实际 %d", resp.StatusCode)
	}
	id4 := numOf(out["truck_lot_id"])
	resp, out = e.do("POST", "/api/app/recv/weigh", ck,
		fmt.Sprintf(`{"truck_lot_id": %d, "gross_weight": 12, "tare_weight": 4}`, id4))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 过磅应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	if out["tare_source"] != "input" {
		t.Fatalf("显式皮重应 tare_source=input，实际 %v", out["tare_source"])
	}
	if s := n029TruckSnapshot(t, e.st, id4); s["tare_weight"] != "4.000" {
		t.Fatalf("库内皮重应 4.000，实际 %v", s)
	}
}

// TC-N029-08 建袋打码 / 退车 / 补打：正例 + 边界 + 与 Web 端终态一致。
func TestTC_N029_08_BagGenReturnAndReprint(t *testing.T) {
	e, cks := n029Env(t)
	ck := cks["receiver"]

	// ① 建袋正例：袋序由后端分配 1..3、摊算重量、吨袋码
	notice := n029Scene(t, e, ck, "湘F·N029G")
	resp, out := e.do("POST", "/api/app/recv/arrive-confirm", ck, fmt.Sprintf(`{"notice_id": %d}`, notice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 到货应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	id := numOf(out["truck_lot_id"])
	if resp, out = e.do("POST", "/api/app/recv/weigh", ck, fmt.Sprintf(
		`{"truck_lot_id": %d, "gross_weight": 9, "tare_weight": 3}`, id)); resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 过磅应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	resp, out = e.do("POST", "/api/app/recv/bag-gen", ck,
		fmt.Sprintf(`{"truck_lot_id": %d, "count": 3}`, id))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 打码应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	bags, _ := out["bags"].([]interface{})
	if len(bags) != 3 {
		t.Fatalf("应 3 袋，实际 %d", len(bags))
	}
	for i, raw := range bags {
		m, _ := raw.(map[string]interface{})
		n029AssertKeys(t, "bag row", m,
			"bag_seq", "code", "human", "weight_allocated", "weight_is_allocated", "status")
		if numOf(m["bag_seq"]) != int64(i+1) {
			t.Fatalf("★ 袋序应由后端分配 %d，实际 %v", i+1, m["bag_seq"])
		}
		if len(fmt.Sprint(m["code"])) != 27 {
			t.Fatalf("吨袋码应 27 位，实际 %v", m["code"])
		}
		if m["status"] != "在库" {
			t.Fatalf("袋状态应「在库」，实际 %v", m["status"])
		}
		if fmt.Sprint(m["weight_allocated"]) != "2" {
			t.Fatalf("摊算重量应 2（6÷3），实际 %v", m["weight_allocated"])
		}
	}
	// count 缺失 ⇒ 结构化 400
	resp, out = e.do("POST", "/api/app/recv/bag-gen", ck,
		fmt.Sprintf(`{"truck_lot_id": %d}`, id))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 count 应 400，实际 %d body=%v", resp.StatusCode, out)
	}
	n029AssertBlock(t, "bag-gen/缺count", out)
	// 重复打码 ⇒ 409 结构化
	resp, out = e.do("POST", "/api/app/recv/bag-gen", ck,
		fmt.Sprintf(`{"truck_lot_id": %d, "count": 2}`, id))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复打码应 409，实际 %d body=%v", resp.StatusCode, out)
	}
	n029AssertBlock(t, "bag-gen/重复", out)

	// ② 补打正例（带原因 ⇒ 200；同码可再打）
	bagCode, _ := bags[0].(map[string]interface{})["code"].(string)
	resp, out = e.do("POST", "/api/app/label/reprint", ck,
		fmt.Sprintf(`{"bag_code": %q, "reason": "现场标签磨损"}`, bagCode))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("补打应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	n029AssertKeys(t, "reprint", out, "count", "labels", "page_url")
	lbls, _ := out["labels"].([]interface{})
	first, _ := lbls[0].(map[string]interface{})
	if numOf(first["is_reprint"]) != 1 {
		t.Fatalf("补打记录 is_reprint 应为 1，实际 %v", first["is_reprint"])
	}
	if first["reason"] != "现场标签磨损" {
		t.Fatalf("补打原因应留痕，实际 %v", first["reason"])
	}
	// 缺 bag_code ⇒ 结构化 400
	resp, out = e.do("POST", "/api/app/label/reprint", ck, `{"reason": "x"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 bag_code 应 400，实际 %d", resp.StatusCode)
	}
	n029AssertBlock(t, "reprint/缺bag_code", out)

	// ③ 退车：伪造现行质检「退货」判定后 ⇒ 200，终态与 Web 端一致
	n029FakeReturnDecision(t, e.st, id, "QC-N029-APP")
	resp, out = e.do("POST", "/api/app/recv/return", ck,
		fmt.Sprintf(`{"notice_id": %d, "reason": "复检不合格"}`, notice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("APP 退车应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	n029AssertKeys(t, "return", out, "returned", "truck_lot_id", "truck_code", "notice_id", "status", "bag_count")
	if out["status"] != "已退货" || numOf(out["bag_count"]) != 0 {
		t.Fatalf("退车终态应 已退货/0 袋，实际 %v", out)
	}
	if n := n029VoidCount(t, e.st, id); n != 3 {
		t.Fatalf("★ 整车 3 袋应全部作废，实际 %d", n)
	}

	// Web 端同款流程终态一致
	noticeW := n029Scene(t, e, ck, "湘F·N029W2")
	resp, out = e.do("POST", "/api/recv/arrivals", ck, fmt.Sprintf(`{"notice_id": %d}`, noticeW))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Web 到货应 200，实际 %d", resp.StatusCode)
	}
	idW := numOf(out["row"].(map[string]interface{})["id"])
	if resp, out = e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/weigh", idW), ck,
		`{"gross_weight": 9, "tare_weight": 3}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("Web 过磅应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	if resp, out = e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/bags", idW), ck,
		`{"count": 3}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("Web 打码应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	n029FakeReturnDecision(t, e.st, idW, "QC-N029-WEB")
	resp, out = e.do("POST", fmt.Sprintf("/api/recv/trucks/%d/return", idW), ck,
		`{"reason": "复检不合格"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Web 退车应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	appSnap, webSnap := n029TruckSnapshot(t, e.st, id), n029TruckSnapshot(t, e.st, idW)
	if appSnap["status"] != webSnap["status"] || appSnap["bag_count"] != webSnap["bag_count"] {
		t.Fatalf("★ 退车终态不一致：APP=%v Web=%v", appSnap, webSnap)
	}
	if n029VoidCount(t, e.st, id) != n029VoidCount(t, e.st, idW) {
		t.Fatalf("★ 作废袋数不一致：APP=%d Web=%d",
			n029VoidCount(t, e.st, id), n029VoidCount(t, e.st, idW))
	}
}

// n029FakeReturnDecision 造一条现行质检「退货」判定（M5 前置，属测试数据准备）。
func n029FakeReturnDecision(t *testing.T, st *store.Store, truckID int64, no string) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_inspection (inspection_no, target_type, target_id, conclusion, disposition, created_by)
VALUES (?, '车次', ?, '不合格', '退货', ?)`, no, truckID, n029RecvOpenID); err != nil {
		t.Fatalf("造质检退货判定失败: %v", err)
	}
}
