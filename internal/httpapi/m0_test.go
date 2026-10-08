package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/config"
	"github.com/chadhao/jx-lab-trace/internal/store"
)

// ===== 测试基座 =====
//
// ★ 这些是**数据库集成测试**（M0 的 TC 大多离不开真实会话/权限表）：
//   按 docs/05：本机 `go test` 只跑纯单测 ⇒ 未设 JX_TEST_DB=1 一律 Skip（有说明，非静默）；
//   测试服务器上以 JX_TEST_DB=1 执行全套 TC；库内一致性另由 check_perm_registry.py
//   判据④ 做只读计数校验（缺 DSN 即判红）⇒ 「全绿却从没连过库」不可能发生。

const (
	testAdminOpenID = "ou_test_m0_admin"
	testUnmappedID  = "ou_test_m0_nomap"
	cookieName      = "jx_sid"
)

var (
	testStore *store.Store
	testDBErr error
)

// TestMain：按 docs/05（用户 2026-10-09 硬约束）——本机 go test 是**纯单测，
// 不得依赖外部服务** ⇒ DB 集成测试需显式 JX_TEST_DB=1（在测试服务器上执行）。
func TestMain(m *testing.M) {
	if os.Getenv("JX_TEST_DB") == "1" {
		_ = config.LoadDotEnv("../../" + config.DotEnvPath())
		if dsn := os.Getenv("JX_DB_DSN"); dsn != "" {
			st, err := store.Open(dsn)
			if err != nil {
				testDBErr = err
			} else if _, err := st.Migrate(context.Background(), ""); err != nil {
				testDBErr = err
				_ = st.Close()
			} else {
				testStore = st
			}
		} else {
			testDBErr = fmt.Errorf("JX_TEST_DB=1 但缺 JX_DB_DSN")
		}
	}
	code := m.Run()
	if testStore != nil {
		cleanupTestRows(testStore)
		_ = testStore.Close()
	}
	os.Exit(code)
}

func requireDB(t *testing.T) *store.Store {
	t.Helper()
	if os.Getenv("JX_TEST_DB") != "1" {
		t.Skip("JX_TEST_DB 未设为 1：按 docs/05 本机只跑纯单测；DB 集成测试在测试服务器上以 JX_TEST_DB=1 执行")
	}
	if testStore == nil {
		if testDBErr != nil {
			t.Fatalf("数据库不可用: %v", testDBErr)
		}
		t.Skip("未配置 JX_DB_DSN：跳过集成测试（门禁 ④ 在无 DSN 时判红）")
	}
	return testStore
}

func cleanupTestRows(st *store.Store) {
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM s_user_role WHERE open_id LIKE 'ou\_test\_%'`,
		`DELETE FROM s_session WHERE open_id LIKE 'ou\_test\_%'`,
		`DELETE FROM m_user WHERE open_id LIKE 'ou\_test\_%'`,
	} {
		_, _ = st.DB().ExecContext(ctx, q)
	}
}

// env 是一个可“重启”的服务实例（每次 restart 都换**全新的 store 连接与 echo 实例**，
// 进程内不保留任何会话状态 ⇒ 与真实重启等价）。
type env struct {
	t   *testing.T
	cfg *config.Config
	st  *store.Store
	srv *httptest.Server
	ttl time.Duration
}

func newEnv(t *testing.T, ttl time.Duration) *env {
	t.Helper()
	base := requireDB(t)
	// 每个 env 用自己的连接句柄（restart 时重新 Open，模拟进程重启）
	cfg := &config.Config{
		DBDSN:        os.Getenv("JX_DB_DSN"),
		DevMode:      true,
		DevOpenID:    "ou_test_m0_dev_default",
		SessionTTL:   ttl,
		CookieName:   cookieName,
		SecureCookie: false,
		HTTPAddr:     "127.0.0.1:0",
	}
	st, err := store.Open(cfg.DBDSN)
	if err != nil {
		t.Fatalf("打开测试连接失败: %v", err)
	}
	e := &env{t: t, cfg: cfg, st: st, ttl: ttl}
	e.serve()
	t.Cleanup(e.shutdown)
	_ = base // requireDB 已确认库可用；env 使用自己的连接句柄（restart 时替换）
	return e
}

func (e *env) serve() {
	if e.srv != nil {
		e.srv.Close()
	}
	handler := New(e.cfg, e.st, "test").Handler()
	e.srv = httptest.NewServer(handler)
}

// restart 关掉服务与连接，再用**全新连接 + 全新 handler** 起一遍（= 进程重启）。
func (e *env) restart() {
	e.t.Helper()
	e.srv.Close()
	_ = e.st.Close()
	st, err := store.Open(e.cfg.DBDSN)
	if err != nil {
		e.t.Fatalf("重启后重连数据库失败: %v", err)
	}
	e.st = st
	e.serve()
}

func (e *env) shutdown() {
	e.srv.Close()
	_ = e.st.Close()
}

// do 发一次请求；cookie 传原始 cookie 串（可空）。
func (e *env) do(method, path, cookie, body string) (*http.Response, map[string]interface{}) {
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
	out := map[string]interface{}{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp, out
}

// login 走 dev 免登桩，返回原始 cookie 串。
func (e *env) login(openID string) string {
	e.t.Helper()
	body := ""
	if openID != "" {
		b, _ := json.Marshal(map[string]string{"open_id": openID})
		body = string(b)
	}
	resp, out := e.do("POST", "/api/auth/dev-login", "", body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("dev 登录失败：status=%d body=%v", resp.StatusCode, out)
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && c.Value != "" {
			return cookieName + "=" + c.Value
		}
	}
	e.t.Fatalf("登录响应未下发 %s cookie", cookieName)
	return ""
}

func bindRole(t *testing.T, st *store.Store, openID, roleCode string) {
	t.Helper()
	_, err := st.DB().ExecContext(context.Background(),
		`INSERT IGNORE INTO s_user_role (open_id, role_code, granted_by) VALUES (?, ?, 'test')`,
		openID, roleCode)
	if err != nil {
		t.Fatalf("绑定角色失败: %v", err)
	}
}

// ===== TC =====

// TC-M0-01 正常：已映射角色的账号访问其有权的入口 ⇒ 放行。
func TestTC_M0_01_MappedRoleAllowed(t *testing.T) {
	st := requireDB(t)
	bindRole(t, st, testAdminOpenID, "sysadmin")
	e := newEnv(t, time.Hour)
	ck := e.login(testAdminOpenID)

	resp, me := e.do("GET", "/api/me", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me 应 200，实际 %d", resp.StatusCode)
	}
	if me["open_id"] != testAdminOpenID {
		t.Fatalf("open_id 错误: %v", me["open_id"])
	}
	sysRoles, _ := me["sys_roles"].([]interface{})
	if len(sysRoles) == 0 {
		t.Fatalf("sys_roles 应含 sysadmin，实际 %v", me)
	}

	// 受保护入口：sys_perm.edit 授权 = ALL ⇒ 放行
	resp, body := e.do("GET", "/api/admin/permission-points", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("有权访问应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if count, _ := body["count"].(float64); int(count) != 51 {
		t.Fatalf("应返回 51 个权限点，实际 %v", body["count"])
	}

	// cookie 属性（D5）：HttpOnly + SameSite=Lax + Max-Age 与 TTL 同批
	resp2, _ := e.do("POST", "/api/auth/dev-login", "",
		`{"open_id":"`+testAdminOpenID+`"}`)
	for _, c := range resp2.Cookies() {
		if c.Name != cookieName {
			continue
		}
		if !c.HttpOnly {
			t.Fatal("cookie 必须 HttpOnly")
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Fatal("cookie 必须 SameSite=Lax")
		}
		if c.MaxAge != int(e.ttl/time.Second) {
			t.Fatalf("cookie Max-Age 应与服务端 TTL 同批（%d），实际 %d",
				int(e.ttl/time.Second), c.MaxAge)
		}
	}
}

// TC-M0-02 异常：账号未映射任何角色 ⇒ 拒绝（不是“默认放行”）。
func TestTC_M0_02_UnmappedRejected(t *testing.T) {
	st := requireDB(t)
	_, _ = st.DB().ExecContext(context.Background(),
		`DELETE FROM s_user_role WHERE open_id = ?`, testUnmappedID)
	e := newEnv(t, time.Hour)

	// ① 未映射账号连会话都建不起来
	resp, body := e.do("POST", "/api/auth/dev-login", "",
		`{"open_id":"`+testUnmappedID+`"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("未映射账号登录应 403，实际 %d body=%v", resp.StatusCode, body)
	}

	// ② 绕过登录、直接落库的会话（= 角色被移除后的既有会话）访问受保护入口 ⇒ 403
	sid, err := store.NewSessionID()
	if err != nil {
		t.Fatalf("生成会话失败: %v", err)
	}
	if err := st.CreateSession(context.Background(), sid, testUnmappedID, "127.0.0.1",
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("造会话失败: %v", err)
	}
	ck := cookieName + "=" + sid

	resp, body = e.do("GET", "/api/admin/permission-points", ck, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("未映射账号访问受保护入口应 403，实际 %d body=%v", resp.StatusCode, body)
	}
	// 身份接口仍可用（说明拒绝来自权限判定，不是会话失效）
	resp, _ = e.do("GET", "/api/me", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me 应 200（身份仍有效），实际 %d", resp.StatusCode)
	}
}

// TC-M0-03 边界：服务重启后用原 cookie 访问 ⇒ 仍然有效（A4）。
func TestTC_M0_03_SessionSurvivesRestart(t *testing.T) {
	st := requireDB(t)
	bindRole(t, st, testAdminOpenID, "sysadmin")
	e := newEnv(t, time.Hour)
	ck := e.login(testAdminOpenID)

	if resp, _ := e.do("GET", "/api/me", ck, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("重启前应 200，实际 %d", resp.StatusCode)
	}

	e.restart() // 新连接 + 新 handler = 进程重启；会话只在 s_session 里

	resp, body := e.do("GET", "/api/me", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("★★ 重启后原 cookie 应仍有效，实际 %d body=%v", resp.StatusCode, body)
	}
	resp, body = e.do("GET", "/api/admin/permission-points", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("★★ 重启后受保护入口应 200，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TC-M0-04 边界：持续访问可续命；停止访问超过 TTL 后失效。
func TestTC_M0_04_SlidingExpiry(t *testing.T) {
	st := requireDB(t)
	bindRole(t, st, testAdminOpenID, "sysadmin")
	e := newEnv(t, 1*time.Second)
	ck := e.login(testAdminOpenID)

	// 0.6s < TTL：访问一次（滑动续期）
	time.Sleep(600 * time.Millisecond)
	if resp, _ := e.do("GET", "/api/me", ck, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("TTL 内访问应 200，实际 %d", resp.StatusCode)
	}

	// 之后停止访问超过 TTL ⇒ 失效
	time.Sleep(1400 * time.Millisecond)
	resp, _ := e.do("GET", "/api/me", ck, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("停止访问超 TTL 后应 401，实际 %d", resp.StatusCode)
	}

	// 新会话：从不访问，直接等过 TTL ⇒ 失效（cookie Max-Age 与服务端同批）
	ck2 := e.login(testAdminOpenID)
	time.Sleep(1400 * time.Millisecond)
	if resp, _ := e.do("GET", "/api/me", ck2, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("静置超 TTL 后应 401，实际 %d", resp.StatusCode)
	}
}

// TC-M0-05 异常：登出后重启服务再访问 ⇒ 失效（A5）。
func TestTC_M0_05_LogoutSurvivesRestart(t *testing.T) {
	st := requireDB(t)
	bindRole(t, st, testAdminOpenID, "sysadmin")
	e := newEnv(t, time.Hour)
	ck := e.login(testAdminOpenID)

	if resp, _ := e.do("GET", "/api/me", ck, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("登出前应 200，实际 %d", resp.StatusCode)
	}
	resp, body := e.do("POST", "/api/auth/logout", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登出应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	e.restart()
	resp, body = e.do("GET", "/api/me", ck, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("★★ 登出并重启后原 cookie 应失效，实际 %d body=%v", resp.StatusCode, body)
	}
	resp, body = e.do("GET", "/api/admin/permission-points", ck, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("登出后受保护入口应 401，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TC-M0-06 变异：把某权限点的 level 由 ALL 改成 NONE ⇒ 该入口由通变拒。
func TestTC_M0_06_LevelMutationAllToNone(t *testing.T) {
	st := requireDB(t)
	bindRole(t, st, testAdminOpenID, "sysadmin")
	e := newEnv(t, time.Hour)
	ck := e.login(testAdminOpenID)

	if resp, _ := e.do("GET", "/api/admin/permission-points", ck, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("变异前应 200，实际 %d", resp.StatusCode)
	}

	var oldLevel string
	err := st.DB().QueryRowContext(context.Background(),
		`SELECT level FROM s_role_permission WHERE role_code='sysadmin' AND point_code='sys.perm.edit'`).
		Scan(&oldLevel)
	if err != nil {
		t.Fatalf("读取授权失败: %v", err)
	}
	defer func() {
		if _, err := st.DB().ExecContext(context.Background(),
			`UPDATE s_role_permission SET level = ? WHERE role_code='sysadmin' AND point_code='sys.perm.edit'`,
			oldLevel); err != nil {
			t.Fatalf("还原授权失败: %v", err)
		}
	}()

	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE s_role_permission SET level='NONE' WHERE role_code='sysadmin' AND point_code='sys.perm.edit'`); err != nil {
		t.Fatalf("变异失败: %v", err)
	}

	resp, body := e.do("GET", "/api/admin/permission-points", ck, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★ level=NONE 后应 403，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TC-M0-09 边界：尝试通过后台接口新增一个权限点 ⇒ 被拒（A8：无此通路）。
func TestTC_M0_09_NoBackendCreateEndpoint(t *testing.T) {
	st := requireDB(t)
	bindRole(t, st, testAdminOpenID, "sysadmin")
	e := newEnv(t, time.Hour)
	ck := e.login(testAdminOpenID)

	resp, body := e.do("POST", "/api/admin/permission-points", ck,
		`{"code":"hack.point","module":"x","name":"x"}`)
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("新增权限点接口必须不存在（404/405），实际 %d body=%v", resp.StatusCode, body)
	}

	// 也顺手确认：字典外 code 的写操作同样 404（不能凭空造点）
	resp, body = e.do("PATCH", "/api/admin/permission-points/hack.point", ck,
		`{"status":"启用"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("字典外 code 应 404，实际 %d body=%v", resp.StatusCode, body)
	}

	// 库内仍是 51 个点
	n, err := st.CountRows(context.Background(), "s_permission_point")
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 51 {
		t.Fatalf("权限点数应仍为 51，实际 %d", n)
	}
}

// TC-M0-10 正常：做一次权限相关写操作 ⇒ s_audit_log 有该笔记录（A10）。
func TestTC_M0_10_AuditRowWritten(t *testing.T) {
	st := requireDB(t)
	bindRole(t, st, testAdminOpenID, "sysadmin")
	e := newEnv(t, time.Hour)
	ck := e.login(testAdminOpenID)

	pp, id, err := st.PermissionPointByCode(context.Background(), "md.customer")
	if err != nil {
		t.Fatalf("读取权限点失败: %v", err)
	}
	target := pp.Status
	if target == "停用" {
		target = "启用"
	}
	other := "停用"
	if target == "停用" {
		other = "启用"
	}
	before, err := st.FindAuditByEntity(context.Background(), "s_permission_point", id)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}

	resp, body := e.do("PATCH", "/api/admin/permission-points/md.customer", ck,
		fmt.Sprintf(`{"status":"%s","reason":"TC-M0-10 权限相关写操作"}`, other))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH 应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	t.Cleanup(func() {
		if _, err := st.DB().ExecContext(context.Background(),
			`UPDATE s_permission_point SET status=? WHERE code='md.customer'`, target); err != nil {
			t.Errorf("还原权限点状态失败: %v", err)
		}
	})

	after, err := st.FindAuditByEntity(context.Background(), "s_permission_point", id)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if len(after) <= len(before) {
		t.Fatalf("权限相关写操作后 s_audit_log 应新增记录（前 %d 后 %d）", len(before), len(after))
	}
	row := after[0]
	if row.Action != "status" || row.Field != "status" {
		t.Fatalf("审计动作/字段不符: %+v", row)
	}
	if row.OldValue != target || row.NewValue != other {
		t.Fatalf("审计应记录 旧值→新值（%s→%s），实际 %s→%s",
			target, other, row.OldValue, row.NewValue)
	}
	if row.ActorOpenID != testAdminOpenID {
		t.Fatalf("审计操作者应为 %s，实际 %s", testAdminOpenID, row.ActorOpenID)
	}
	if row.Reason != "TC-M0-10 权限相关写操作" {
		t.Fatalf("审计应记录原因，实际 %q", row.Reason)
	}

	// 还原状态（同时再验证一次审计追加）
	if resp, body := e.do("PATCH", "/api/admin/permission-points/md.customer", ck,
		fmt.Sprintf(`{"status":"%s","reason":"还原"}`, target)); resp.StatusCode != http.StatusOK {
		t.Fatalf("还原 PATCH 应 200，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TestTC_M0_Healthz_NoAuthRequired：免权限入口可用（D6 的最小证明）。
func TestTC_M0_Healthz_NoAuthRequired(t *testing.T) {
	e := newEnv(t, time.Hour)
	resp, body := e.do("GET", "/healthz", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz 应 200，实际 %d", resp.StatusCode)
	}
	if body["status"] != "ok" {
		t.Fatalf("healthz 返回异常: %v", body)
	}
}

// TestTC_M0_DevLoginRequiresRole：dev 桩没有凭空放行的后门（回归护栏）。
func TestTC_M0_DevLoginRequiresRole(t *testing.T) {
	st := requireDB(t)
	bindRole(t, st, testAdminOpenID, "sysadmin")
	e := newEnv(t, time.Hour)

	// 关掉 dev 模式 ⇒ 路由不存在（404；若被 SPA 兜底路由拦下则为 405，同属「无此通路”）
	e.cfg.DevMode = false
	e.serve()
	resp, _ := e.do("POST", "/api/auth/dev-login", "", `{"open_id":"`+testAdminOpenID+`"}`)
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("非 dev 模式不应存在 dev 登录通路，实际 %d", resp.StatusCode)
	}
}
