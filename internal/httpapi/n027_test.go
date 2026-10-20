package httpapi

// ===== N-027 · FIX-11（D6 测试）=====
//
//	D1 REQ-03：POST /api/auth/feishu/exchange —— 成功（Set-Cookie＋{"ok":true}）/
//	  401（code 无效）/ 403（角色未映射带 userMessage）/ 502（飞书上游错）/
//	  bootstrap 触发（清库态首个 exchange 登录者获 sysadmin）/ dev 模式不受影响 / callback 回归
//	D2 B-req-2：建单带车辆 ⇒ 单头落库并可查；谱系车次预填
//	D3 B-req-1：明细 weight_allocated ＋ 单头 net_weight_total（作废袋不计口径）
//	D4 B-req-3：C 码进 /trace/batch；E 码 backward 谱系；错误类型码 400 文案
//	D5 B-req-4：未投料作废成功 / 已投料 409 / 已完成 409 / 权限点回归
//
// ★ DB 集成测试：本机未设 JX_TEST_DB=1 一律 Skip（docs/05）。
// ★ 夹具纪律（§6-19）：先子后父、按账号精确匹配、断言限定本用例作用域。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/codec"
	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	n027OpenID   = "ou_test_n027_main"   // production+receiver+sales（B-req 全链）
	n027MgmtID   = "ou_test_n027_mgmt"   // management（void 回归用）
	n027NoProdID = "ou_test_n027_noprod" // 无 prod.batch.create（权限 403 回归）
	n027CustCode = "9427"
	n027InMat    = "9427"
	n027OutMat   = "9428"
	n027Date     = "2026-10-20"
	n027DateSeg  = "261020"
)

// n027Wipe 清掉 N-027 接口测试数据（先子后父、按账号 / 测试客户精确匹配）。
func n027Wipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	me := n027OpenID
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
	q(`DELETE FROM b_shipment_item WHERE shipment_id IN
	       (SELECT id FROM b_shipment WHERE created_by IN (?, ?))`, me, n027MgmtID)
	q(`DELETE FROM b_shipment WHERE created_by IN (?, ?)`, me, n027MgmtID)
	q(`DELETE FROM b_fg_bag WHERE fg_lot_id IN (SELECT id FROM b_fg_lot WHERE created_by = ?)`, me)
	q(`DELETE FROM b_fg_lot WHERE created_by = ?`, me)
	q(`DELETE FROM b_feed_record WHERE batch_id IN (SELECT id FROM b_production_batch WHERE created_by = ?)`, me)
	q(`DELETE FROM b_batch_operation WHERE batch_id IN (SELECT id FROM b_production_batch WHERE created_by = ?)`, me)
	q(`DELETE FROM b_production_batch WHERE created_by = ?`, me)
	q(`DELETE FROM b_obj_void WHERE entity = 'b_bag'
	     AND entity_id IN (SELECT id FROM b_bag WHERE created_by = ?)`, me)
	q(`DELETE FROM b_bag WHERE created_by = ?
	     OR truck_lot_id IN (SELECT id FROM b_truck_lot
	                           WHERE customer_id IN (SELECT id FROM m_customer WHERE code = ?))`,
		me, n027CustCode)
	q(`DELETE FROM b_truck_lot WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, n027CustCode)
	q(`DELETE FROM s_audit_log WHERE actor_open_id IN (?, ?, ?)`,
		me, n027MgmtID, n027NoProdID)
	q(`DELETE FROM m_vehicle WHERE plate_no IN ('N027TEST','N027PRE') AND created_by LIKE 'ou\_test\_%'`)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, n027CustCode)
	q(`DELETE FROM m_material WHERE code IN (?, ?) AND created_by LIKE 'ou\_test\_%'`,
		n027InMat, n027OutMat)
}

// n027Env 建测试主数据 + 起服务 + 绑角色；返回 env 与主账号 cookie。
func n027Env(t *testing.T) (*env, string) {
	t.Helper()
	st := requireDB(t)
	n027Wipe(t, st)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'N027测试客户', '启用', 1, 1, ?)`, n027CustCode, n027OpenID); err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'N027测试原料', '原料', '启用', 1, 1, ?)`, n027InMat, n027OpenID); err != nil {
		t.Fatalf("建测试原料失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'N027测试成品', '成品', '启用', 1, 1, ?)`, n027OutMat, n027OpenID); err != nil {
		t.Fatalf("建测试成品失败: %v", err)
	}
	t.Cleanup(func() { n027Wipe(t, st) })

	bindRole(t, st, n027OpenID, "production")
	bindRole(t, st, n027OpenID, "receiver")
	bindRole(t, st, n027OpenID, "sales")
	bindRole(t, st, n027MgmtID, "management")
	bindRole(t, st, n027NoProdID, "sales") // 有角色但无 prod.* —— 权限 403 回归

	e := newEnv(t, 12*time.Hour) // ★ TTL 12h ⇒ cookie Max-Age=43200（对齐 exchange 契约）
	return e, e.login(n027OpenID)
}

// n027MasterIDs 取测试客户 / 原料 / 成品当前行 id。
func n027MasterIDs(t *testing.T, st *store.Store) (cust, inMat, outMat int64) {
	t.Helper()
	ctx := context.Background()
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_customer WHERE code = ? AND is_current = 1`, n027CustCode).Scan(&cust); err != nil {
		t.Fatalf("查测试客户失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, n027InMat).Scan(&inMat); err != nil {
		t.Fatalf("查测试原料失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, n027OutMat).Scan(&outMat); err != nil {
		t.Fatalf("查测试成品失败: %v", err)
	}
	return cust, inMat, outMat
}

// n027GenCode 生成指定类型追踪码。
func n027GenCode(t *testing.T, typ, seq1, seq2, seq3 string) string {
	t.Helper()
	code, err := codec.Generate(codec.Segments{
		T: typ, BT: "CG", Customer: n027CustCode, Material: n027OutMat,
		Date: n027DateSeg, SEQ1: seq1, SEQ2: seq2, SEQ3: seq3,
	})
	if err != nil {
		t.Fatalf("生成 %s 码失败: %v", typ, err)
	}
	return code
}

// n027BatchFixture 直写库造生产批（上游对象，§5 允许），返回 id 与全码。
func n027BatchFixture(t *testing.T, st *store.Store, custID, outMatID int64, seq1 string) (int64, string) {
	t.Helper()
	code := n027GenCode(t, "C", seq1, "000", "000")
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_production_batch
  (code, customer_id, input_material_id, planned_output_material_id, batch_date, status, created_by)
VALUES (?,?,?,?,?, '进行中', ?)`, code, custID, 1, outMatID, n027Date, n027OpenID)
	if err != nil {
		t.Fatalf("造生产批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id, code
}

// n027LotFixture 直写库造成品批，返回 id 与全码。
func n027LotFixture(t *testing.T, st *store.Store, batchID, custID, outMatID int64, seq2 string) (int64, string) {
	t.Helper()
	code := n027GenCode(t, "D", "01", seq2, "000")
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_fg_lot (code, batch_id, customer_id, output_material_id, qty_bag, status, created_by)
VALUES (?,?,?,?, 0, '在库', ?)`, code, batchID, custID, outMatID, n027OpenID)
	if err != nil {
		t.Fatalf("造成品批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id, code
}

// n027BagFixture 直写库造 n 个成品袋（可带摊算重），返回袋码数组。
func n027BagFixture(t *testing.T, st *store.Store, lotID int64, lotCode string, weights []float64) []string {
	t.Helper()
	ctx := context.Background()
	p, err := codec.Parse(lotCode)
	if err != nil {
		t.Fatalf("成品批码不可解析: %v", err)
	}
	out := make([]string, 0, len(weights))
	for i, w := range weights {
		seq := i + 1
		code, err := codec.Generate(codec.Segments{
			T: "E", BT: p.Seg.BT, Customer: p.Seg.Customer, Material: p.Seg.Material,
			Date: p.Seg.Date, SEQ1: p.Seg.SEQ1, SEQ2: p.Seg.SEQ2, SEQ3: fmt.Sprintf("%03d", seq),
		})
		if err != nil {
			t.Fatalf("生成成品袋码失败: %v", err)
		}
		if _, err := st.DB().ExecContext(ctx, `
INSERT INTO b_fg_bag (code, fg_lot_id, bag_seq, weight_allocated, status, created_by)
VALUES (?,?,?,?, '在库', ?)`, code, lotID, seq, w, n027OpenID); err != nil {
			t.Fatalf("造成品袋失败: %v", err)
		}
		out = append(out, code)
	}
	return out
}

// n027Ship 建出货单（失败即 Fatal），返回 row。
func n027Ship(t *testing.T, e *env, ck string, payload string) map[string]interface{} {
	t.Helper()
	resp, body := e.do("POST", "/api/ship/shipments", ck, payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建出货单应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	if row == nil {
		t.Fatalf("建出货单未返回 row: %v", body)
	}
	return row
}

// ══════════════════════════════════════════════════════════════════════════
// D1 REQ-03 · exchange 端点
// ══════════════════════════════════════════════════════════════════════════

// n027FakeFeishu 起假飞书（token+user_info 覆写），返回 client 与关闭函数。
// tokenBody 非空 ⇒ token 端点原样返回该 body（造 401/502 分支）。
func n027FakeFeishu(t *testing.T, openID, name, tokenBody string) (*access.FeishuClient, func()) {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			if tokenBody != "" {
				_, _ = w.Write([]byte(tokenBody))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"n027-at","code":0}`))
		case "/user_info":
			if r.Header.Get("Authorization") != "Bearer n027-at" {
				http.Error(w, `{"code":99991663,"msg":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 0,
				"data": map[string]string{"open_id": openID, "name": name},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	return &access.FeishuClient{
		AppID:               "cli_test_n027",
		AppSecret:           "sec_test_n027",
		TokenURLOverride:    fake.URL + "/token",
		UserInfoURLOverride: fake.URL + "/user_info",
	}, fake.Close
}

// n027Exchange POST /api/auth/feishu/exchange，返回状态码、响应头与响应体。
func n027Exchange(t *testing.T, e *env, body string) (int, http.Header, string) {
	t.Helper()
	resp, _ := e.do("POST", "/api/auth/feishu/exchange", "", body)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(raw)
}

// respCookiesFromHeader 从响应头解析 Set-Cookie 列表。
func respCookiesFromHeader(hdr http.Header) []*http.Cookie {
	rec := httptest.NewRecorder()
	for _, v := range hdr.Values("Set-Cookie") {
		rec.Header().Add("Set-Cookie", v)
	}
	return rec.Result().Cookies()
}

// TC-N027-01 ★ exchange 成功：Set-Cookie（jx_sid HttpOnly SameSite=Lax Max-Age=43200）＋ {"ok":true}。
func TestTC_N027_01_Exchange_Success(t *testing.T) {
	st := requireDB(t)
	bindRole(t, st, n027OpenID, "production")
	e := newEnv(t, 12*time.Hour)
	fake, closeFn := n027FakeFeishu(t, n027OpenID, "N027主账号", "")
	defer closeFn()
	e.api.Feishu = fake

	status, hdr, body := n027Exchange(t, e,
		`{"code":"n027-ok","state":"csrf-opaque"}`)
	if status != http.StatusOK {
		t.Fatalf("exchange 成功应 200，实际 %d body=%s", status, body)
	}
	if strings.TrimSpace(body) != `{"ok":true}` {
		t.Fatalf("★ 成功响应体应 {\"ok\":true}，实际 %s", body)
	}
	if strings.Contains(body, "csrf-opaque") {
		t.Fatalf("★ state 不得回显，实际 %s", body)
	}
	var ck *http.Cookie
	for _, c := range respCookiesFromHeader(hdr) {
		if c.Name == cookieName {
			ck = c
		}
	}
	if ck == nil || ck.Value == "" {
		t.Fatalf("★ 成功必须下发 %s cookie，实际头 %v", cookieName, hdr)
	}
	if !ck.HttpOnly {
		t.Fatal("★ cookie 必须 HttpOnly")
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Fatal("★ cookie 必须 SameSite=Lax")
	}
	if ck.MaxAge != 43200 {
		t.Fatalf("★ TTL 12h ⇒ Max-Age 应 43200，实际 %d", ck.MaxAge)
	}
	// 会话可用
	meResp, meBody := e.do("GET", "/api/me", cookieName+"="+ck.Value, "")
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("exchange 会话访问 /api/me 应 200，实际 %d body=%v", meResp.StatusCode, meBody)
	}
	if meBody["open_id"] != n027OpenID {
		t.Fatalf("会话主体错乱: %v", meBody)
	}
}

// TC-N027-02 ★ exchange 401：飞书拒授权码（invalid_grant）⇒ 401，无 cookie。
func TestTC_N027_02_Exchange_InvalidCode_401(t *testing.T) {
	e := newEnv(t, 12*time.Hour)
	fake, closeFn := n027FakeFeishu(t, "", "",
		`{"error":"invalid_grant","error_description":"The authorization code is not found.","code":20003}`)
	defer closeFn()
	e.api.Feishu = fake

	status, hdr, body := n027Exchange(t, e, `{"code":"expired","state":"x"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("无效 code 应 401，实际 %d body=%s", status, body)
	}
	if !strings.Contains(body, "授权码无效或已过期") {
		t.Fatalf("401 文案应点明授权码无效，实际 %s", body)
	}
	for _, c := range respCookiesFromHeader(hdr) {
		if c.Name == cookieName && c.Value != "" {
			t.Fatal("★ 401 不得下发会话 cookie")
		}
	}
}

// TC-N027-03 ★ exchange 403：角色未映射 ⇒ 403 带 userMessage。
func TestTC_N027_03_Exchange_NoRole_403_UserMessage(t *testing.T) {
	st := requireDB(t)
	unmapped := "ou_test_n027_unmapped"
	_, _ = st.DB().ExecContext(context.Background(),
		`DELETE FROM s_user_role WHERE open_id = ?`, unmapped)
	e := newEnv(t, 12*time.Hour)
	fake, closeFn := n027FakeFeishu(t, unmapped, "无角色用户", "")
	defer closeFn()
	e.api.Feishu = fake

	status, _, body := n027Exchange(t, e, `{"code":"n027-norole"}`)
	if status != http.StatusForbidden {
		t.Fatalf("无角色 exchange 应 403，实际 %d body=%s", status, body)
	}
	var parsed struct {
		Error       string `json:"error"`
		UserMessage string `json:"userMessage"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("403 响应不是 JSON: %v body=%s", err, body)
	}
	if parsed.UserMessage != "您的账号尚未分配角色，请联系综合运营部" {
		t.Fatalf("★ 403 必须带契约 userMessage，实际 %q（body=%s）", parsed.UserMessage, body)
	}
	if n025Count(t, st,
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ?`, unmapped) != 0 {
		t.Fatal("★ 403 不得创建角色绑定")
	}
}

// TC-N027-04 ★ exchange 502：飞书上游错（响应不可解析）⇒ 502，详情不外泄。
func TestTC_N027_04_Exchange_Upstream_502(t *testing.T) {
	e := newEnv(t, 12*time.Hour)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not json at all", http.StatusInternalServerError)
	}))
	defer broken.Close()
	e.api.Feishu = &access.FeishuClient{
		AppID:            "cli_test_n027",
		AppSecret:        "sec_test_n027",
		TokenURLOverride: broken.URL,
	}

	status, _, body := n027Exchange(t, e, `{"code":"n027-upstream"}`)
	if status != http.StatusBadGateway {
		t.Fatalf("上游错应 502，实际 %d body=%s", status, body)
	}
	if strings.Contains(body, "not json at all") {
		t.Fatalf("★ 502 详情不得外泄，实际 %s", body)
	}
}

// TC-N027-05 ★★ bootstrap：清库态首个 exchange 登录者获 sysadmin（对齐 N-025 机制）。
func TestTC_N027_05_Exchange_BootstrapSysadmin(t *testing.T) {
	st := requireDB(t)
	n025ClearSysAdminBindings(t, st)
	firstID := "ou_test_n027_boot_first"
	ctx := context.Background()
	_, _ = st.DB().ExecContext(ctx, `DELETE FROM m_user WHERE open_id = ?`, firstID)
	_, _ = st.DB().ExecContext(ctx, `DELETE FROM s_user_role WHERE open_id = ?`, firstID)
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(ctx, `DELETE FROM s_user_role WHERE open_id = ?`, firstID)
		_, _ = st.DB().ExecContext(ctx, `DELETE FROM m_user WHERE open_id = ?`, firstID)
	})

	e := newEnv(t, 12*time.Hour)
	fake, closeFn := n027FakeFeishu(t, firstID, "N027首登", "")
	defer closeFn()
	e.api.Feishu = fake

	var status int
	var hdr http.Header
	var body string
	for attempt := 0; attempt < 3; attempt++ { // 共享库并发兜底（同 N-025 TC①）
		n025DeleteSysAdminRows(t, st)
		status, hdr, body = n027Exchange(t, e, `{"code":"n027-boot"}`)
		if status == http.StatusOK && hdr.Get("X-JX-Bootstrap-Sysadmin") == "1" {
			break
		}
	}
	if status != http.StatusOK {
		t.Fatalf("bootstrap exchange 应 200，实际 %d body=%s", status, body)
	}
	if hdr.Get("X-JX-Bootstrap-Sysadmin") != "1" {
		t.Fatalf("★ 首登 exchange 必须明示已引导，实际头=%v", hdr)
	}
	if n025Count(t, st,
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ? AND role_code = 'sysadmin'`, firstID) != 1 {
		t.Fatal("★ 首登 exchange 应绑定 sysadmin")
	}
	if n025Count(t, st,
		`SELECT COUNT(*) FROM s_audit_log WHERE action = 'bootstrap_sysadmin' AND actor_open_id = ?`, firstID) < 1 {
		t.Fatal("★ 应有 bootstrap_sysadmin 审计")
	}
	// 登录审计 how=feishu_login 照常
	if n025Count(t, st,
		`SELECT COUNT(*) FROM s_audit_log WHERE action = 'feishu_login' AND actor_open_id = ?`, firstID) < 1 {
		t.Fatal("★ exchange 登录审计 action 应为 feishu_login")
	}
}

// TC-N027-06 ★ dev 模式不受影响：dev-login 照常可用；★ callback 回归 302。
func TestTC_N027_06_Exchange_DevModeAndCallbackUnchanged(t *testing.T) {
	st := requireDB(t)
	bindRole(t, st, n027OpenID, "production")
	e := newEnv(t, 12*time.Hour) // DevMode=true

	// ① dev 登录不受 exchange 引入影响
	resp, body := e.do("POST", "/api/auth/dev-login", "",
		fmt.Sprintf(`{"open_id":%q}`, n027OpenID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("★ dev 登录行为不得改变，应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	// ② callback 回归：假飞书下 GET callback 仍 302 首页
	fake, closeFn := n027FakeFeishu(t, n027OpenID, "N027主账号", "")
	defer closeFn()
	e.api.Feishu = fake
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/auth/feishu/callback?code=n027-cb", nil)
	cbResp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("callback 请求失败: %v", err)
	}
	defer cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusFound {
		raw, _ := io.ReadAll(cbResp.Body)
		t.Fatalf("★ callback 行为一字不改：应 302，实际 %d body=%s", cbResp.StatusCode, raw)
	}
	if loc := cbResp.Header.Get("Location"); loc != "/" {
		t.Fatalf("★ callback 仍应重定向回 /，实际 %q", loc)
	}
}

// ══════════════════════════════════════════════════════════════════════════
// D2 B-req-2 · 建出货单携带车辆
// ══════════════════════════════════════════════════════════════════════════

// TC-N027-07 ★ 建单带 vehicle_id/plate_no/driver ⇒ 单头落库并可查。
func TestTC_N027_07_CreateShipment_WithVehicle(t *testing.T) {
	e, ck := n027Env(t)
	cust, _, outMat := n027MasterIDs(t, e.st)
	ctx := context.Background()

	// 车辆档案
	vehRes, err := e.st.DB().ExecContext(ctx, `
INSERT INTO m_vehicle (plate_no, status, version, is_current, created_by)
VALUES ('N027TEST', '启用', 1, 1, ?)`, n027OpenID)
	if err != nil {
		t.Fatalf("造车辆档案失败: %v", err)
	}
	vehID, _ := vehRes.LastInsertId()

	batch, _ := n027BatchFixture(t, e.st, cust, outMat, "01")
	lot, lotCode := n027LotFixture(t, e.st, batch, cust, outMat, "001")
	bags := n027BagFixture(t, e.st, lot, lotCode, []float64{1.1})

	payload := fmt.Sprintf(
		`{"bag_codes":[%q],"vehicle_id":%d,"plate_no":"N027TEST","driver":"司机甲","remark":"B-req-2"}`,
		bags[0], vehID)
	row := n027Ship(t, e, ck, payload)

	if numOf(row["vehicle_id"]) != vehID {
		t.Fatalf("★ 单头 vehicle_id 应落库 %d，实际 %v", vehID, row["vehicle_id"])
	}
	if row["plate_no"] != "N027TEST" {
		t.Fatalf("★ 单头 plate_no 应落库，实际 %v", row["plate_no"])
	}
	if row["driver"] != "司机甲" {
		t.Fatalf("★ 单头 driver 应落库，实际 %v", row["driver"])
	}
	// 详情可查
	shipID := numOf(row["id"])
	resp, body := e.do("GET", fmt.Sprintf("/api/ship/shipments/%d", shipID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查出货单应 200，实际 %d", resp.StatusCode)
	}
	got, _ := body["row"].(map[string]interface{})
	if got["plate_no"] != "N027TEST" || got["driver"] != "司机甲" || numOf(got["vehicle_id"]) != vehID {
		t.Fatalf("★ 详情应回显车辆三字段，实际 %v", got)
	}
}

// TC-N027-08 ★ 谱系车次预填：入参不带车辆 ⇒ 从袋的谱系车次自动预填。
func TestTC_N027_08_CreateShipment_TruckContextPrefill(t *testing.T) {
	e, ck := n027Env(t)
	cust, inMat, outMat := n027MasterIDs(t, e.st)
	ctx := context.Background()

	// 车次 + 袋（谱系上游，直写库）
	vehRes, err := e.st.DB().ExecContext(ctx, `
INSERT INTO m_vehicle (plate_no, default_driver, status, version, is_current, created_by)
VALUES ('N027PRE', '预填司机', '启用', 1, 1, ?)`, n027OpenID)
	if err != nil {
		t.Fatalf("造车辆失败: %v", err)
	}
	vehID, _ := vehRes.LastInsertId()
	truckRes, err := e.st.DB().ExecContext(ctx, `
INSERT INTO b_truck_lot (code, customer_id, material_id, vehicle_id, driver, arrive_date, arrive_at, status, created_by)
VALUES (?,?,?,?,?,?,?, '合格', ?)`,
		n027GenCode(t, "A", "01", "000", "000"), cust, inMat, vehID, "预填司机",
		n027Date, n027Date+" 08:00:00", n027OpenID)
	if err != nil {
		t.Fatalf("造车次失败: %v", err)
	}
	truckID, _ := truckRes.LastInsertId()
	bagCode := n027GenCode(t, "B", "01", "001", "000")
	bagRes, err := e.st.DB().ExecContext(ctx, `
INSERT INTO b_bag (code, truck_lot_id, bag_seq, status, created_by)
VALUES (?,?,1, '已投料', ?)`, bagCode, truckID, n027OpenID)
	if err != nil {
		t.Fatalf("造吨袋失败: %v", err)
	}
	bagID, _ := bagRes.LastInsertId()

	batch, _ := n027BatchFixture(t, e.st, cust, outMat, "01")
	if _, err := e.st.DB().ExecContext(ctx,
		`INSERT INTO b_feed_record (batch_id, bag_id, created_by) VALUES (?,?,?)`,
		batch, bagID, n027OpenID); err != nil {
		t.Fatalf("造投料记录失败: %v", err)
	}
	lot, lotCode := n027LotFixture(t, e.st, batch, cust, outMat, "001")
	bags := n027BagFixture(t, e.st, lot, lotCode, []float64{2.0})

	row := n027Ship(t, e, ck,
		fmt.Sprintf(`{"bag_codes":[%q],"remark":"预填"}`, bags[0]))
	if row["plate_no"] != "N027PRE" {
		t.Fatalf("★ 谱系车次应预填 plate_no，实际 %v", row["plate_no"])
	}
	if row["driver"] != "预填司机" {
		t.Fatalf("★ 谱系车次应预填 driver，实际 %v", row["driver"])
	}
	if numOf(row["vehicle_id"]) != vehID {
		t.Fatalf("★ 谱系车次应预填 vehicle_id，实际 %v", row["vehicle_id"])
	}
}

// ══════════════════════════════════════════════════════════════════════════
// D3 B-req-1 · 出货单净重聚合
// ══════════════════════════════════════════════════════════════════════════

// TC-N027-09 ★ 明细 weight_allocated ＋ 单头 net_weight_total；作废袋不计口径。
func TestTC_N027_09_Shipment_WeightAggregation(t *testing.T) {
	e, ck := n027Env(t)
	cust, _, outMat := n027MasterIDs(t, e.st)
	ctx := context.Background()

	batch, _ := n027BatchFixture(t, e.st, cust, outMat, "01")
	lot, lotCode := n027LotFixture(t, e.st, batch, cust, outMat, "001")
	// 三袋：10.5 + 20.0 + 5.0；随后把第二袋作废
	bags := n027BagFixture(t, e.st, lot, lotCode, []float64{10.5, 20.0, 5.0})
	if _, err := e.st.DB().ExecContext(ctx,
		`UPDATE b_fg_bag SET status = '作废' WHERE code = ?`, bags[1]); err != nil {
		t.Fatalf("作废袋失败: %v", err)
	}

	raw, _ := json.Marshal(map[string]interface{}{"bag_codes": bags, "remark": "B-req-1"})
	row := n027Ship(t, e, ck, string(raw))
	shipID := numOf(row["id"])

	resp, body := e.do("GET", fmt.Sprintf("/api/ship/shipments/%d", shipID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查出货单应 200，实际 %d", resp.StatusCode)
	}
	// 明细逐行 weight_allocated
	items, _ := body["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("明细应 3 行，实际 %d", len(items))
	}
	want := map[string]float64{bags[0]: 10.5, bags[1]: 20.0, bags[2]: 5.0}
	for _, it := range items {
		m, _ := it.(map[string]interface{})
		code := fmt.Sprint(m["bag_code"])
		w, ok := m["weight_allocated"].(float64)
		if !ok {
			t.Fatalf("★ 明细行缺 weight_allocated: %v", m)
		}
		if w != want[code] {
			t.Fatalf("★ 袋 %s weight_allocated 应 %v，实际 %v", code, want[code], w)
		}
	}
	// 单头 net_weight_total：作废袋（20.0）不计 ⇒ 10.5+5.0=15.5
	got, _ := body["row"].(map[string]interface{})
	total, ok := got["net_weight_total"].(float64)
	if !ok {
		t.Fatalf("★ 单头缺 net_weight_total: %v", got)
	}
	if total != 15.5 {
		t.Fatalf("★ net_weight_total 应 15.5（作废袋不计），实际 %v", total)
	}
}

// ══════════════════════════════════════════════════════════════════════════
// D4 B-req-3 · 追溯入口扩展（C/E 码）
// ══════════════════════════════════════════════════════════════════════════

// TC-N027-10 ★ C 码进 /trace/batch；数字 id 仍可用；错误类型码 400 文案。
func TestTC_N027_10_TraceBatch_CCodeEntry(t *testing.T) {
	e, ck := n027Env(t)
	cust, _, outMat := n027MasterIDs(t, e.st)
	batchID, batchCode := n027BatchFixture(t, e.st, cust, outMat, "01")
	human, err := codec.ToHuman(batchCode)
	if err != nil {
		t.Fatalf("转人读行失败: %v", err)
	}

	// ① 全码
	resp, body := e.do("GET", "/api/trace/batch/"+batchCode, ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("C 全码进 /trace/batch 应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	batch, _ := body["batch"].(map[string]interface{})
	if numOf(batch["batch_id"]) != batchID {
		t.Fatalf("★ C 码应解析到批 %d，实际 %v", batchID, batch)
	}

	// ② 人读行
	resp, body = e.do("GET", "/api/trace/batch/"+human, ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("C 人读行进 /trace/batch 应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	// ③ 数字 id（既有行为）
	resp, body = e.do("GET", fmt.Sprintf("/api/trace/batch/%d", batchID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("数字 id 应仍 200，实际 %d body=%v", resp.StatusCode, body)
	}

	// ④ E 码（错误类型）⇒ 400 明确文案
	lot, lotCode := n027LotFixture(t, e.st, batchID, cust, outMat, "001")
	bags := n027BagFixture(t, e.st, lot, lotCode, []float64{1.0})
	resp, body = e.do("GET", "/api/trace/batch/"+bags[0], ck, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("E 码进 /trace/batch 应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "此入口请扫生产批码") {
		t.Fatalf("★ 400 文案应指明入口码型，实际 %s", raw)
	}
}

// TC-N027-11 ★ E 码 backward 谱系（E→D→C→投料链）；D 码既有行为回归。
func TestTC_N027_11_TraceBackward_ECodeEntry(t *testing.T) {
	e, ck := n027Env(t)
	cust, inMat, outMat := n027MasterIDs(t, e.st)
	ctx := context.Background()

	// 上游：车次 → 吨袋 → 投料 → 批 → 成品批 → 成品袋
	truckRes, err := e.st.DB().ExecContext(ctx, `
INSERT INTO b_truck_lot (code, customer_id, material_id, arrive_date, arrive_at, status, created_by)
VALUES (?,?,?,?,?, '合格', ?)`,
		n027GenCode(t, "A", "01", "000", "000"), cust, inMat,
		n027Date, n027Date+" 09:00:00", n027OpenID)
	if err != nil {
		t.Fatalf("造车次失败: %v", err)
	}
	truckID, _ := truckRes.LastInsertId()
	bagCode := n027GenCode(t, "B", "01", "001", "000")
	bagRes, err := e.st.DB().ExecContext(ctx, `
INSERT INTO b_bag (code, truck_lot_id, bag_seq, status, created_by)
VALUES (?,?,1, '已投料', ?)`, bagCode, truckID, n027OpenID)
	if err != nil {
		t.Fatalf("造吨袋失败: %v", err)
	}
	bagID, _ := bagRes.LastInsertId()
	batch, _ := n027BatchFixture(t, e.st, cust, outMat, "01")
	if _, err := e.st.DB().ExecContext(ctx,
		`INSERT INTO b_feed_record (batch_id, bag_id, created_by) VALUES (?,?,?)`,
		batch, bagID, n027OpenID); err != nil {
		t.Fatalf("造投料失败: %v", err)
	}
	lot, lotCode := n027LotFixture(t, e.st, batch, cust, outMat, "001")
	bags := n027BagFixture(t, e.st, lot, lotCode, []float64{3.3})

	// ① E 码入口
	resp, body := e.do("GET", "/api/trace/backward?fg_code="+bags[0], ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("E 码 backward 应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if body["has_flow"] != true {
		t.Fatalf("★ E 码 backward has_flow 应 true，实际 %v", body["has_flow"])
	}
	fgLot, _ := body["fg_lot"].(map[string]interface{})
	if fmt.Sprint(fgLot["code"]) != lotCode {
		t.Fatalf("★ E 码应解析到所属成品批 %s，实际 %v", lotCode, fgLot)
	}
	batches, _ := body["batches"].([]interface{})
	if len(batches) != 1 || numOf(batches[0].(map[string]interface{})["batch_id"]) != batch {
		t.Fatalf("★ E 码 backward 链应含生产批 %d，实际 %v", batch, body["batches"])
	}
	feeds, _ := body["feeds"].([]interface{})
	if len(feeds) != 1 {
		t.Fatalf("★ E 码 backward 应含 1 条投料，实际 %d", len(feeds))
	}

	// ② D 码既有行为回归
	resp, body = e.do("GET", "/api/trace/backward?fg_code="+lotCode, ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("D 码 backward 回归应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	// ③ B 码（错误类型）⇒ 400 文案
	resp, body = e.do("GET", "/api/trace/backward?fg_code="+bagCode, ck, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("B 码进 backward 应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "此入口请扫成品批码") {
		t.Fatalf("★ 400 文案应指明入口码型，实际 %s", raw)
	}
}

// ══════════════════════════════════════════════════════════════════════════
// D5 B-req-4 · 生产批作废
// ══════════════════════════════════════════════════════════════════════════

// TC-N027-12 ★ 未投料作废成功；已投料 409；已完成 409；幂等 409；权限点回归。
func TestTC_N027_12_VoidProductionBatch(t *testing.T) {
	e, ck := n027Env(t)
	cust, inMat, outMat := n027MasterIDs(t, e.st)
	ctx := context.Background()

	// ① 未投料 ⇒ 作废成功
	batch1, _ := n027BatchFixture(t, e.st, cust, outMat, "01")
	resp, body := e.do("PATCH", fmt.Sprintf("/api/prod/batches/%d/void", batch1), ck,
		`{"reason":"N027测试作废"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("未投料作废应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if body["voided"] != true {
		t.Fatalf("作废响应应 voided=true，实际 %v", body)
	}
	row, _ := body["row"].(map[string]interface{})
	if row["status"] != "已作废" {
		t.Fatalf("★ 作废后 status 应「已作废」，实际 %v", row["status"])
	}
	if n025Count(t, e.st, `SELECT COUNT(*) FROM s_audit_log
	     WHERE entity = 'b_production_batch' AND entity_id = ? AND action = 'void'`, batch1) != 1 {
		t.Fatal("★ 作废应写审计 action=void")
	}
	// 谱系入口仍可见（status 如实标注，不隐藏历史）
	if resp, body := e.do("GET", fmt.Sprintf("/api/trace/batch/%d", batch1), ck, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("★ 已作废批谱系档案仍可见，实际 %d body=%v", resp.StatusCode, body)
	}

	// ② 已投料 ⇒ 409
	batch2, _ := n027BatchFixture(t, e.st, cust, outMat, "02")
	truckRes, err := e.st.DB().ExecContext(ctx, `
INSERT INTO b_truck_lot (code, customer_id, material_id, arrive_date, arrive_at, status, created_by)
VALUES (?,?,?,?,?, '合格', ?)`,
		n027GenCode(t, "A", "02", "000", "000"), cust, inMat,
		n027Date, n027Date+" 10:00:00", n027OpenID)
	if err != nil {
		t.Fatalf("造车次失败: %v", err)
	}
	truckID, _ := truckRes.LastInsertId()
	bagCode := n027GenCode(t, "B", "02", "001", "000")
	bagRes, err := e.st.DB().ExecContext(ctx, `
INSERT INTO b_bag (code, truck_lot_id, bag_seq, status, created_by)
VALUES (?,?,1, '已投料', ?)`, bagCode, truckID, n027OpenID)
	if err != nil {
		t.Fatalf("造吨袋失败: %v", err)
	}
	bagID, _ := bagRes.LastInsertId()
	if _, err := e.st.DB().ExecContext(ctx,
		`INSERT INTO b_feed_record (batch_id, bag_id, created_by) VALUES (?,?,?)`,
		batch2, bagID, n027OpenID); err != nil {
		t.Fatalf("造投料失败: %v", err)
	}
	resp, body = e.do("PATCH", fmt.Sprintf("/api/prod/batches/%d/void", batch2), ck,
		`{"reason":"不该成功"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("已投料作废应 409，实际 %d body=%v", resp.StatusCode, body)
	}
	if !strings.Contains(fmt.Sprint(body), "已有投料") {
		t.Fatalf("★ 409 文案应点明已投料，实际 %v", body)
	}

	// ③ 已完成 ⇒ 409
	batch3, _ := n027BatchFixture(t, e.st, cust, outMat, "03")
	if _, err := e.st.DB().ExecContext(ctx,
		`UPDATE b_production_batch SET status = '已完成' WHERE id = ?`, batch3); err != nil {
		t.Fatalf("置已完成失败: %v", err)
	}
	resp, body = e.do("PATCH", fmt.Sprintf("/api/prod/batches/%d/void", batch3), ck, `{}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("已完成作废应 409，实际 %d body=%v", resp.StatusCode, body)
	}

	// ④ 已作废再作废 ⇒ 409（幂等语义：明确报错不静默成功）
	resp, body = e.do("PATCH", fmt.Sprintf("/api/prod/batches/%d/void", batch1), ck, `{}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复作废应 409，实际 %d body=%v", resp.StatusCode, body)
	}

	// ⑤ 权限回归：sales（无 prod.batch.create）⇒ 403
	noProd := e.login(n027NoProdID)
	resp, body = e.do("PATCH", fmt.Sprintf("/api/prod/batches/%d/void", batch1), noProd, `{}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★ 无 prod.* 权限应 403（挂既有权限点），实际 %d body=%v", resp.StatusCode, body)
	}
	// production（prod.batch.create=ALL）已通过 ① —— 权限点挂载正确
}
