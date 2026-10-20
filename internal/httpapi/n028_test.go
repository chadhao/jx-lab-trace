package httpapi

// ===== N-028 · FIX-12（D4 测试）=====
//
//	D1 来源分支路由：source 空/其它 ⇒ v2 端点（回归，含 redirect_uri）；
//	  source="sdk" ⇒ passport 端点（form 报文、★ 无 redirect_uri）。
//	D2 拒绝路径日志：ErrLoginDenied ⇒ Warn 日志含实际换到的 open_id；
//	  ★ 响应 403 userMessage 契约一字不改（REQ-03 红线）。
//	D3 兼容红线：source=sdk 的 feishu_login 同走 openSession/bootstrap 链
//	  （清库后 APP 首登场景照常拿 sysadmin）。
//
// ★ DB 集成测试：本机未设 JX_TEST_DB=1 一律 Skip（docs/05）。
// ★ 无法构造真实 SDK code ⇒ 请求构造/响应形状由 access 层单测逐字段钉住
//	（internal/access/feishu_sdk_test.go）；真机端到端由 APP 侧部署后重试验证。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/access"
)

// n028Rec 记录假飞书两端点的命中与请求（字段由 handler goroutine 写、断言时读）。
type n028Rec struct {
	mu           sync.Mutex
	v2Hits       int
	v2Body       map[string]string
	passportHits int
	passportCT   string
	passportForm url.Values
}

func (r *n028Rec) snapshot() (v2Hits, ph int, v2Body map[string]string, ct string, form url.Values) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.v2Hits, r.passportHits, r.v2Body, r.passportCT, r.passportForm
}

// n028FakeFeishu 起假飞书：v2(/v2/token) 与 passport(/passport/token) 双端点分别记录。
// tokenOK=false ⇒ 两 token 端点均回 invalid_grant ⇒ exchange 映射 401（错误分支回归，
// 无需走到 openSession）；tokenOK=true ⇒ 正常换 token ＋ user_info 返回 openID/name。
func n028FakeFeishu(t *testing.T, openID, name string, tokenOK bool) (*access.FeishuClient, *n028Rec, func()) {
	t.Helper()
	rec := &n028Rec{}
	tokResp := `{"error":"invalid_grant","error_description":"The authorization code is not found.","code":20003}`
	if tokenOK {
		tokResp = `{"access_token":"n028-at","token_type":"Bearer","expires_in":3600}`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/token", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(raw, &m)
		rec.mu.Lock()
		rec.v2Hits++
		rec.v2Body = m
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokResp))
	})
	mux.HandleFunc("/passport/token", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		rec.mu.Lock()
		rec.passportHits++
		rec.passportCT = r.Header.Get("Content-Type")
		rec.passportForm = form
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokResp))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer n028-at" {
			http.Error(w, `{"code":99991663,"msg":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0,
			"data": map[string]string{"open_id": openID, "name": name},
		})
	})
	srv := httptest.NewServer(mux)
	client := &access.FeishuClient{
		AppID:                    "cli_test_n028",
		AppSecret:                "sec_test_n028",
		TokenURLOverride:         srv.URL + "/v2/token",
		PassportTokenURLOverride: srv.URL + "/passport/token",
		UserInfoURLOverride:      srv.URL + "/userinfo",
	}
	return client, rec, srv.Close
}

// TC-N028-01 ★ source 空/其它 ⇒ 仍走 v2 端点（网页流回归：redirect_uri 照传）。
func TestTC_N028_01_Exchange_EmptyOrOtherSource_UsesV2Endpoint(t *testing.T) {
	e := newEnv(t, 12*time.Hour)
	fake, rec, closeFn := n028FakeFeishu(t, "", "", false)
	defer closeFn()
	e.api.Feishu = fake

	// ① 无 source（N-027 既有请求形态）
	status, _, body := n027Exchange(t, e, `{"code":"n028-web-1"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("假 code 应 401，实际 %d body=%s", status, body)
	}
	// ② source=其它值 ⇒ 同样网页流
	status, _, body = n027Exchange(t, e, `{"code":"n028-web-2","source":"web"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("source=web 假 code 应 401，实际 %d body=%s", status, body)
	}

	v2Hits, ph, v2Body, _, _ := rec.snapshot()
	if v2Hits != 2 {
		t.Fatalf("★ source 空/其它都应走 v2 端点，实际命中 %d 次", v2Hits)
	}
	if ph != 0 {
		t.Fatalf("★ 非 sdk 不得触碰 passport 端点，实际命中 %d 次", ph)
	}
	// 回归：网页流仍带 redirect_uri（缺省回落本服务 callback URI）
	if v2Body["redirect_uri"] == "" || !strings.Contains(v2Body["redirect_uri"], "/api/auth/feishu/callback") {
		t.Fatalf("★ 网页流必须仍传 redirect_uri，实际 %v", v2Body)
	}
	if v2Body["code"] == "" {
		t.Fatalf("★ v2 请求体应含 code，实际 %v", v2Body)
	}
}

// TC-N028-02 ★ source="sdk" ⇒ passport 端点（form 报文、含四参数、★ 无 redirect_uri）。
func TestTC_N028_02_Exchange_SourceSDK_UsesPassportEndpoint(t *testing.T) {
	e := newEnv(t, 12*time.Hour)
	fake, rec, closeFn := n028FakeFeishu(t, "", "", false)
	defer closeFn()
	e.api.Feishu = fake

	status, _, body := n027Exchange(t, e, `{"code":"n028-sdk-1","state":"csrf-opaque","source":"sdk"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("SDK 假 code 应 401（错误分支回归），实际 %d body=%s", status, body)
	}

	v2Hits, ph, _, ct, form := rec.snapshot()
	if ph != 1 {
		t.Fatalf("★ source=sdk 应命中 passport 端点 1 次，实际 %d", ph)
	}
	if v2Hits != 0 {
		t.Fatalf("★ source=sdk 不得走网页 v2 端点（根因所在），实际命中 %d 次", v2Hits)
	}
	if !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		t.Fatalf("★ 官方要求 passport 请求 Content-Type=x-www-form-urlencoded，实际 %q", ct)
	}
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "cli_test_n028",
		"client_secret": "sec_test_n028",
		"code":          "n028-sdk-1",
	} {
		if form.Get(k) != want {
			t.Fatalf("★ passport 请求体 %s 应为 %q，实际 %q（全量 %v）", k, want, form.Get(k), form)
		}
	}
	if _, exists := form["redirect_uri"]; exists {
		t.Fatalf("★★ SDK 分支不得传 redirect_uri，实际 %v", form)
	}
}

// TC-N028-03 ★★ D2 拒绝路径日志：ErrLoginDenied ⇒ Warn 含实际换到的 open_id/name；
// 响应 403 userMessage 契约一字不改（REQ-03 红线，对照 TC-N027-03 同文案）。
func TestTC_N028_03_Exchange_Denied_WarnLogContainsOpenID(t *testing.T) {
	st := requireDB(t)
	unmapped := "ou_test_n028_unmapped"
	_, _ = st.DB().ExecContext(context.Background(),
		`DELETE FROM s_user_role WHERE open_id = ?`, unmapped)
	e := newEnv(t, 12*time.Hour)
	fake, _, closeFn := n028FakeFeishu(t, unmapped, "N028无角色用户", true)
	defer closeFn()
	e.api.Feishu = fake

	var logBuf bytes.Buffer
	e.echo.Logger.SetOutput(&logBuf)

	status, _, body := n027Exchange(t, e, `{"code":"n028-norole","source":"sdk"}`)

	out := logBuf.String()
	// ★ 日志断言（D2 排查实锤）：拒绝发生时必须留下 open_id
	if !strings.Contains(out, "exchange") || !strings.Contains(out, "open_id="+unmapped) {
		t.Fatalf("★ D2 拒绝路径 Warn 日志应含 exchange 与 open_id=%s，实际日志: %s", unmapped, out)
	}
	// 响应契约一字不改（先断言日志，失败也不放过响应侧红线）
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
	if parsed.Error != "未映射任何角色或账号已停用，拒绝登录" {
		t.Fatalf("★ 403 error 字段一字不改，实际 %q", parsed.Error)
	}
	if parsed.UserMessage != "您的账号尚未分配角色，请联系综合运营部" {
		t.Fatalf("★ 403 必须带契约 userMessage 不变，实际 %q（body=%s）", parsed.UserMessage, body)
	}
}

// TC-N028-04 ★★ D3-4 兼容红线：source=sdk 的 feishu_login 同走 openSession/bootstrap 链
// ——清库态首个 SDK 登录者照常获 sysadmin（对齐 TC-N027-05，换 source 分支重演）。
func TestTC_N028_04_Exchange_SDKBootstrapSysadmin(t *testing.T) {
	st := requireDB(t)
	n025ClearSysAdminBindings(t, st)
	firstID := "ou_test_n028_boot_first"
	ctx := context.Background()
	_, _ = st.DB().ExecContext(ctx, `DELETE FROM m_user WHERE open_id = ?`, firstID)
	_, _ = st.DB().ExecContext(ctx, `DELETE FROM s_user_role WHERE open_id = ?`, firstID)
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(ctx, `DELETE FROM s_user_role WHERE open_id = ?`, firstID)
		_, _ = st.DB().ExecContext(ctx, `DELETE FROM m_user WHERE open_id = ?`, firstID)
	})

	e := newEnv(t, 12*time.Hour)
	fake, _, closeFn := n028FakeFeishu(t, firstID, "N028SDK首登", true)
	defer closeFn()
	e.api.Feishu = fake

	var status int
	var hdr http.Header
	var body string
	for attempt := 0; attempt < 3; attempt++ { // 共享库并发兜底（同 TC-N027-05）
		n025DeleteSysAdminRows(t, st)
		status, hdr, body = n027Exchange(t, e, `{"code":"n028-boot","source":"sdk"}`)
		if status == http.StatusOK && hdr.Get("X-JX-Bootstrap-Sysadmin") == "1" {
			break
		}
	}
	if status != http.StatusOK {
		t.Fatalf("SDK bootstrap exchange 应 200，实际 %d body=%s", status, body)
	}
	if hdr.Get("X-JX-Bootstrap-Sysadmin") != "1" {
		t.Fatalf("★ source=sdk 首登 exchange 必须明示已引导，实际头=%v", hdr)
	}
	if n025Count(t, st,
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ? AND role_code = 'sysadmin'`, firstID) != 1 {
		t.Fatal("★ 首登 SDK exchange 应绑定 sysadmin")
	}
	if n025Count(t, st,
		`SELECT COUNT(*) FROM s_audit_log WHERE action = 'feishu_login' AND actor_open_id = ?`, firstID) < 1 {
		t.Fatal("★ SDK exchange 登录审计 action 应为 feishu_login")
	}
}
