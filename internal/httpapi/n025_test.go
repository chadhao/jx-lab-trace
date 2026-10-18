package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/config"
	"github.com/chadhao/jx-lab-trace/internal/store"
)

// ══════════════════════════════════════════════════════════════════════════
// N-025 D1 · 首个飞书登录自动 sysadmin（bootstrap-on-first-login）
//
// 四条 TC（任务包 D1「测试」条目）：
//
//	① 无 sysadmin 绑定 ＋ 未配置 JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID ＋ feishu_login
//	   ⇒ 自动绑定 ＋ 审计 bootstrap_sysadmin ＋ 登录响应带引导标记（幂等：重复登录不重复绑）
//	② dev_login 不触发（dev 桩抢不到首个 sysadmin）
//	③ 已有 sysadmin 绑定 ⇒ 不触发
//	④ JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID 已配置 ⇒ 启动引导（Migrate ⑤）行为不变、feishu_login 不触发
//
// ★ 需要真实库 ⇒ 按 docs/05 由 JX_TEST_DB=1 门控（本机未设则 Skip，非静默）；
//   飞书回调用**端点覆写的假飞书**（token + user_info）在进程内跑完整真实路径。
// ══════════════════════════════════════════════════════════════════════════

const (
	n025FirstID  = "ou_test_n025_first_login" // TC① 首个飞书用户
	n025DevID    = "ou_test_n025_dev_login"   // TC② dev 桩身份
	n025SecondID = "ou_test_n025_second"      // TC③ 已有管理员时的第二个飞书用户
	n025KeeperID = "ou_test_n025_keeper"      // TC③ 用来占住 sysadmin 绑定的账号
	n025EnvCfgID = "ou_test_n025_env_admin"   // TC④ 环境变量引导对象
	n025EnvLogin = "ou_test_n025_env_login"   // TC④ 环境变量态下的飞书登录者
)

// n025Env 是一套带**假飞书**的最小服务实例（每次用例独立连接）。
type n025Env struct {
	t   *testing.T
	cfg *config.Config
	st  *store.Store
	api *Server
	web *httptest.Server
}

func newN025Env(t *testing.T, bootstrapSysAdminOpenID string) *n025Env {
	t.Helper()
	requireDB(t)
	cfg := &config.Config{
		DBDSN:                   os.Getenv("JX_DB_DSN"),
		DevMode:                 true,
		DevOpenID:               "ou_test_n025_dev_default",
		SessionTTL:              time.Hour,
		CookieName:              cookieName,
		SecureCookie:            false,
		BootstrapSysAdminOpenID: bootstrapSysAdminOpenID,
		HTTPAddr:                "127.0.0.1:0",
	}
	st, err := store.Open(cfg.DBDSN)
	if err != nil {
		t.Fatalf("打开测试连接失败: %v", err)
	}
	e := &n025Env{t: t, cfg: cfg, st: st}
	e.api = New(cfg, st, "test")
	e.web = httptest.NewServer(e.api.Handler())
	t.Cleanup(func() {
		e.web.Close()
		_ = st.Close()
	})
	return e
}

// call 发一次请求；返回状态码、响应头、cookie 串与响应体（body 已读并关闭）。
func (e *n025Env) call(method, path, cookie, body string) (int, http.Header, string, string) {
	e.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.web.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("构造请求失败: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	// 不跟随重定向：要断言 302 与其响应头（引导标记就挂在重定向上）。
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	ck := ""
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && c.Value != "" {
			ck = cookieName + "=" + c.Value
		}
	}
	return resp.StatusCode, resp.Header, ck, string(raw)
}

// feishuLogin 起一个假飞书（token + user_info 端点覆写）后走**真实回调路径**
// （handleFeishuCallback → Exchange → openSession(feishu_login)）。
func (e *n025Env) feishuLogin(openID, name string) (int, http.Header, string, string) {
	e.t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"n025-at","code":0}`))
		case "/user_info":
			if r.Header.Get("Authorization") != "Bearer n025-at" {
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
	e.t.Cleanup(fake.Close)
	e.api.Feishu = &access.FeishuClient{
		AppID:               "cli_test",
		AppSecret:           "sec_test",
		TokenURLOverride:    fake.URL + "/token",
		UserInfoURLOverride: fake.URL + "/user_info",
	}
	return e.call(http.MethodGet, "/api/auth/feishu/callback?code=n025-code", "", "")
}

// n025Count 执行标量统计查询（只读）。
func n025Count(t *testing.T, st *store.Store, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("统计查询失败（%s）: %v", query, err)
	}
	return n
}

// n025ClearSysAdminBindings 制造「无 sysadmin」态：快照当前全部 sysadmin 绑定后清空，
// 测试结束把快照插回（★ 不删别人的行，只还原自己清掉的）。
//
// ★ 为什么必须清空：条件②是**全局**判定（s_user_role 中不存在 sysadmin 绑定）——
//
//	测试库常驻 ou_dev_admin 的启动引导绑定，不清就永远触发不了。
//
// ★ 如实声明的并发面：internal/store 包与本包可能**并行**跑同一个库，窗口内它插入
//
//	sysadmin 绑定会让「触发」用例落空 ⇒ TC① 带重试兜底；本函数只保证快照行回得去。
func n025ClearSysAdminBindings(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	rows, err := st.DB().QueryContext(ctx,
		`SELECT open_id, granted_by FROM s_user_role WHERE role_code = 'sysadmin'`)
	if err != nil {
		t.Fatalf("快照 sysadmin 绑定失败: %v", err)
	}
	var snap [][2]string
	for rows.Next() {
		var id, by string
		if err := rows.Scan(&id, &by); err != nil {
			rows.Close()
			t.Fatalf("读取 sysadmin 绑定失败: %v", err)
		}
		snap = append(snap, [2]string{id, by})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 sysadmin 绑定失败: %v", err)
	}
	n025DeleteSysAdminRows(t, st)
	t.Cleanup(func() {
		for _, r := range snap {
			_, _ = st.DB().ExecContext(ctx,
				`INSERT IGNORE INTO s_user_role (open_id, role_code, granted_by) VALUES (?, 'sysadmin', ?)`,
				r[0], r[1])
		}
	})
}

func n025DeleteSysAdminRows(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(),
		`DELETE FROM s_user_role WHERE role_code = 'sysadmin'`); err != nil {
		t.Fatalf("清空 sysadmin 绑定失败: %v", err)
	}
}

// ===== TC① =====

// TC-N025-01 ★ 无绑定 ＋ 未配置环境变量 ＋ feishu_login ⇒ 自动绑定 sysadmin ＋ 审计 ＋ 响应明示。
// ★ 顺带钉住 D3：登录前把 m_user 本人行删掉（清库后的空表路径）⇒ 登录后账号行被重建。
func TestTC_N025_01_FeishuFirstLogin_BootstrapsSysadmin(t *testing.T) {
	st := requireDB(t)
	n025ClearSysAdminBindings(t, st)
	e := newN025Env(t, "")
	ctx := context.Background()

	// 清库态：本人既无账号行也无绑定
	_, _ = st.DB().ExecContext(ctx, `DELETE FROM m_user WHERE open_id = ?`, n025FirstID)
	_, _ = st.DB().ExecContext(ctx, `DELETE FROM s_user_role WHERE open_id = ?`, n025FirstID)

	// 触发（带重试：共享库并发插入 sysadmin 时重开窗口）
	status, hdr, ck, body := 0, http.Header{}, "", ""
	for attempt := 0; attempt < 3; attempt++ {
		n025DeleteSysAdminRows(t, st)
		status, hdr, ck, body = e.feishuLogin(n025FirstID, "首个飞书用户")
		if status == http.StatusFound && hdr.Get("X-JX-Bootstrap-Sysadmin") == "1" {
			break
		}
	}
	if status != http.StatusFound {
		t.Fatalf("首个飞书登录应回 302 首页，实际 %d body=%s", status, body)
	}
	if loc := hdr.Get("Location"); loc != "/" {
		t.Fatalf("登录后应重定向回首页，实际 Location=%q", loc)
	}
	if hdr.Get("X-JX-Bootstrap-Sysadmin") != "1" {
		t.Fatalf("★ 登录响应必须明示已引导（X-JX-Bootstrap-Sysadmin=1），实际头=%v", hdr)
	}
	if ck == "" {
		t.Fatal("登录响应必须下发会话 cookie")
	}

	// 绑定落库：恰好 1 条，来源可辨识
	if n := n025Count(t, st,
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ? AND role_code = 'sysadmin'`, n025FirstID); n != 1 {
		t.Fatalf("★ 应绑定 1 条 sysadmin，实际 %d", n)
	}
	var grantedBy string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT granted_by FROM s_user_role WHERE open_id = ? AND role_code = 'sysadmin'`, n025FirstID).
		Scan(&grantedBy); err != nil || grantedBy != "bootstrap_login" {
		t.Fatalf("绑定来源应为 bootstrap_login，实际 %q（err=%v）", grantedBy, err)
	}

	// 审计：action=bootstrap_sysadmin，actor ＝ 该用户本人
	if n := n025Count(t, st,
		`SELECT COUNT(*) FROM s_audit_log WHERE action = 'bootstrap_sysadmin' AND actor_open_id = ?`,
		n025FirstID); n < 1 {
		t.Fatalf("★ 应有 bootstrap_sysadmin 审计（actor=本人），实际 %d 条", n)
	}

	// D3：m_user 空表 ⇒ 本人账号行被登录路径重建
	var name string
	if err := st.DB().QueryRowContext(ctx, `SELECT name FROM m_user WHERE open_id = ?`, n025FirstID).
		Scan(&name); err != nil {
		t.Fatalf("★ 清库后飞书登录应重建本人账号行: %v", err)
	}
	if name != "首个飞书用户" {
		t.Fatalf("账号姓名应取自飞书身份，实际 %q", name)
	}

	// 登录后主体：/api/me 直接能看到 sysadmin
	mStatus, _, _, mBody := e.call(http.MethodGet, "/api/me", ck, "")
	if mStatus != http.StatusOK {
		t.Fatalf("/api/me 应 200，实际 %d body=%s", mStatus, mBody)
	}
	var me struct {
		OpenID   string   `json:"open_id"`
		Name     string   `json:"name"`
		SysRoles []string `json:"sys_roles"`
	}
	if err := json.Unmarshal([]byte(mBody), &me); err != nil {
		t.Fatalf("/api/me 响应不是 JSON: %v", err)
	}
	if me.OpenID != n025FirstID || me.Name != "首个飞书用户" {
		t.Fatalf("主体身份错乱: %+v", me)
	}
	found := false
	for _, r := range me.SysRoles {
		if r == "sysadmin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("★ sys_roles 应含 sysadmin，实际 %v", me.SysRoles)
	}

	// 幂等：重复飞书登录不再引导（无标记头、绑定不增、审计不增）
	audits := n025Count(t, st,
		`SELECT COUNT(*) FROM s_audit_log WHERE action = 'bootstrap_sysadmin' AND actor_open_id = ?`,
		n025FirstID)
	status2, hdr2, _, body2 := e.feishuLogin(n025FirstID, "首个飞书用户")
	if status2 != http.StatusFound {
		t.Fatalf("重复登录应仍 302，实际 %d body=%s", status2, body2)
	}
	if hdr2.Get("X-JX-Bootstrap-Sysadmin") != "" {
		t.Fatalf("★ 重复登录不得再次引导，实际带头 %v", hdr2)
	}
	if n := n025Count(t, st,
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ? AND role_code = 'sysadmin'`, n025FirstID); n != 1 {
		t.Fatalf("重复登录后绑定应仍为 1 条，实际 %d", n)
	}
	if n := n025Count(t, st,
		`SELECT COUNT(*) FROM s_audit_log WHERE action = 'bootstrap_sysadmin' AND actor_open_id = ?`,
		n025FirstID); n != audits {
		t.Fatalf("重复登录不得再写审计：%d → %d", audits, n)
	}
}

// ===== TC② =====

// TC-N025-02 ★ dev 桩登录不触发引导（防 ou_dev_admin 抢占首个 sysadmin）。
func TestTC_N025_02_DevLogin_DoesNotBootstrap(t *testing.T) {
	st := requireDB(t)
	n025ClearSysAdminBindings(t, st) // 条件②满足：此刻系统确处「无 sysadmin」态
	e := newN025Env(t, "")

	status, _, _, body := e.call(http.MethodPost, "/api/auth/dev-login", "",
		`{"open_id":"`+n025DevID+`"}`)
	if status != http.StatusForbidden {
		t.Fatalf("无角色 dev 登录应 403，实际 %d body=%s", status, body)
	}
	if !strings.Contains(body, "未映射任何角色") {
		t.Fatalf("拒绝报文应点明未映射角色，实际 %s", body)
	}
	if n := n025Count(t, st,
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ?`, n025DevID); n != 0 {
		t.Fatalf("★ dev 登录不得创建任何角色绑定，实际 %d 条", n)
	}
	if n := n025Count(t, st,
		`SELECT COUNT(*) FROM s_audit_log WHERE action = 'bootstrap_sysadmin' AND actor_open_id = ?`,
		n025DevID); n != 0 {
		t.Fatalf("★ dev 登录不得写 bootstrap_sysadmin 审计，实际 %d 条", n)
	}
}

// ===== TC③ =====

// TC-N025-03 ★ 已有 sysadmin 绑定 ⇒ 第二个飞书用户登录不触发（条件②不满足）。
func TestTC_N025_03_ExistingSysadmin_NoSecondBootstrap(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	bindRole(t, st, n025KeeperID, "sysadmin")
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(ctx,
			`DELETE FROM s_user_role WHERE open_id = ? AND role_code = 'sysadmin'`, n025KeeperID)
	})
	e := newN025Env(t, "")

	status, hdr, _, body := e.feishuLogin(n025SecondID, "第二个飞书用户")
	if status != http.StatusForbidden {
		t.Fatalf("无角色飞书登录应 403（deny by default），实际 %d body=%s", status, body)
	}
	if hdr.Get("X-JX-Bootstrap-Sysadmin") != "" {
		t.Fatalf("★ 已有 sysadmin 时不得引导，实际头=%v", hdr)
	}
	if !strings.Contains(body, "联系管理员") {
		t.Fatalf("拒绝应走友好 HTML 页，实际 %s", body)
	}
	if n := n025Count(t, st,
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ?`, n025SecondID); n != 0 {
		t.Fatalf("★ 不得为第二用户创建绑定，实际 %d 条", n)
	}
	if n := n025Count(t, st,
		`SELECT COUNT(*) FROM s_audit_log WHERE action = 'bootstrap_sysadmin' AND actor_open_id = ?`,
		n025SecondID); n != 0 {
		t.Fatalf("★ 不得写 bootstrap_sysadmin 审计，实际 %d 条", n)
	}
	// 原有 sysadmin 绑定不受影响
	if n := n025Count(t, st,
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ? AND role_code = 'sysadmin'`, n025KeeperID); n != 1 {
		t.Fatalf("既有 sysadmin 绑定应保持 1 条，实际 %d", n)
	}
}

// ===== TC④ =====

// TC-N025-04 ★ JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID 已配置：
// 启动引导（Migrate ⑤，mimo 既有 TC 依赖）行为不变；feishu_login **不**重复触发（条件③不满足）。
func TestTC_N025_04_EnvBootstrap_Configured_MutuallyExclusive(t *testing.T) {
	st := requireDB(t)
	n025ClearSysAdminBindings(t, st) // 保证条件②也满足 —— 只剩条件③在拦
	e := newN025Env(t, n025EnvCfgID)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(ctx,
			`DELETE FROM s_user_role WHERE open_id = ? AND role_code = 'sysadmin'`, n025EnvCfgID)
		_, _ = st.DB().ExecContext(ctx, `DELETE FROM m_user WHERE open_id = ?`, n025EnvCfgID)
	})

	// ① 环境变量已配置 ⇒ 飞书登录不引导（即便此刻全库没有任何 sysadmin 绑定）
	status, hdr, _, body := e.feishuLogin(n025EnvLogin, "环境变量态用户")
	if status != http.StatusForbidden {
		t.Fatalf("★ 环境变量已配置时无角色飞书登录应 403，实际 %d body=%s", status, body)
	}
	if hdr.Get("X-JX-Bootstrap-Sysadmin") != "" {
		t.Fatalf("★ 环境变量已配置时不得引导，实际头=%v", hdr)
	}
	if n := n025Count(t, st,
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ?`, n025EnvLogin); n != 0 {
		t.Fatalf("不得创建绑定，实际 %d 条", n)
	}
	if n := n025Count(t, st,
		`SELECT COUNT(*) FROM s_audit_log WHERE action = 'bootstrap_sysadmin' AND actor_open_id = ?`,
		n025EnvLogin); n != 0 {
		t.Fatalf("不得写 bootstrap_sysadmin 审计，实际 %d 条", n)
	}

	// ② 启动引导本身一字未改：Migrate ⑤ 仍按老口径建绑定（granted_by='bootstrap'）
	if _, err := st.Migrate(ctx, n025EnvCfgID); err != nil {
		t.Fatalf("启动迁移失败: %v", err)
	}
	var grantedBy string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT granted_by FROM s_user_role WHERE open_id = ? AND role_code = 'sysadmin'`, n025EnvCfgID).
		Scan(&grantedBy); err != nil {
		t.Fatalf("★ 启动引导应照旧建绑定: %v", err)
	}
	if grantedBy != "bootstrap" {
		t.Fatalf("启动引导来源应仍为 bootstrap（与登录引导 bootstrap_login 区分），实际 %q", grantedBy)
	}
}
