package access

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ══════════════════════════════════════════════════════════════════════════
// 对外报文**形状断言**（COLLAB 判据：对外报文必须配一条形状断言用例）
//
// ★★ 为什么专门为它建这个文件（2026-10-10）：
//   本文件原先**不存在** ⇒ `AuthorizeURL` 的域名/参数名/缺失必填项**三处偏差**
//   一路溜到"准备联调"才被人（WorkBuddy）拿官方文档核对出来。
//   ★ 三者都不会在 dev 桩下暴露 —— dev 不经过这条路；`go test` 全绿也照样绿。
//   ⇒ **判据：「dev 桩绕过」的代码路径 = 联调前必须补形状断言的代码路径。**
// ══════════════════════════════════════════════════════════════════════════

// ★ 官方端点断言：授权端点在 accounts.feishu.cn（不是 open.feishu.cn）。
func TestFeishu_AuthorizeURL_官方域名与必填参数(t *testing.T) {
	c := &FeishuClient{AppID: "cli_test123"}
	got := c.AuthorizeURL("http://office.example.com:5502/api/auth/feishu/callback", "st-abc")

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("URL 不可解析: %v", err)
	}

	// ① 域名与路径（★ 原实现误用 open.feishu.cn）
	if u.Scheme != "https" || u.Host != "accounts.feishu.cn" {
		t.Errorf("★授权端点应为 https://accounts.feishu.cn，实际 %s://%s", u.Scheme, u.Host)
	}
	if u.Path != "/open-apis/authen/v1/authorize" {
		t.Errorf("授权路径错：%s", u.Path)
	}

	q := u.Query()
	// ② 参数名必须是 client_id（★ 原实现误写 app_id）
	if q.Get("client_id") != "cli_test123" {
		t.Errorf("★应带 client_id=cli_test123，实际 client_id=%q", q.Get("client_id"))
	}
	if _, exists := q["app_id"]; exists {
		t.Errorf("★★不应出现 app_id（官方参数名是 client_id）")
	}
	// ③ response_type=code 是**必填**（★ 原实现完全缺失）
	if q.Get("response_type") != "code" {
		t.Errorf("★★缺 response_type=code（官方标注必填），实际 %q", q.Get("response_type"))
	}
	// ④ redirect_uri 原样（会被 url.Values 编码，解析后应还原）
	if q.Get("redirect_uri") != "http://office.example.com:5502/api/auth/feishu/callback" {
		t.Errorf("redirect_uri 错：%q", q.Get("redirect_uri"))
	}
	// ⑤ scope 必须是**已在后台申请的真实权限键**（不是 "openid"）
	if got := q.Get("scope"); got != feishuScope || strings.Contains(got, "openid") {
		t.Errorf("★scope 应为 %q（真实权限键），实际 %q", feishuScope, got)
	}
	// ⑥ state 原样回传
	if q.Get("state") != "st-abc" {
		t.Errorf("state 错：%q", q.Get("state"))
	}
}

// state 为空时不应拼出空的 state=（飞书会当成传了空 state）。
func TestFeishu_AuthorizeURL_空state不拼接(t *testing.T) {
	c := &FeishuClient{AppID: "cli_x"}
	u, _ := url.Parse(c.AuthorizeURL("http://h/cb", ""))
	if _, exists := u.Query()["state"]; exists {
		t.Errorf("空 state 不应出现在 query 里")
	}
}

// ★ Exchange 的**形状断言**：请求体字段名 + Content-Type + Bearer 头。
func TestFeishu_Exchange_报文形状(t *testing.T) {
	var tokenBody map[string]string
	var tokenCT string
	var bearer string

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &tokenBody)
		_, _ = w.Write([]byte(`{"code":0,"access_token":"uat-1"}`))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		bearer = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"code":0,"data":{"open_id":"ou_1","name":"张三"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &FeishuClient{
		AppID: "cli_a", AppSecret: "sec_b",
		TokenURLOverride:    srv.URL + "/token",
		UserInfoURLOverride: srv.URL + "/userinfo",
	}
	ident, err := c.Exchange(context.Background(), "code-1", "http://h/cb")
	if err != nil {
		t.Fatalf("Exchange 失败: %v", err)
	}
	if ident.OpenID != "ou_1" || ident.Name != "张三" {
		t.Errorf("身份解析错：%+v", ident)
	}

	// ★ 字段名断言（少一个或多一个都会在这里红）
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "cli_a",
		"client_secret": "sec_b",
		"code":          "code-1",
		"redirect_uri":  "http://h/cb",
	} {
		if tokenBody[k] != want {
			t.Errorf("★ token 请求体 %s 应为 %q，实际 %q（完整体 %v）", k, want, tokenBody[k], tokenBody)
		}
	}
	if !strings.HasPrefix(tokenCT, "application/json") {
		t.Errorf("★ token 请求 Content-Type 应含 application/json，实际 %q", tokenCT)
	}
	if bearer != "Bearer uat-1" {
		t.Errorf("★ user_info 应带 Bearer 令牌，实际 %q", bearer)
	}
}

// 失败路径：换 token 被拒 / 未返回 open_id ⇒ 必须**明确报错**，不得静默降级。
func TestFeishu_Exchange_失败必须报错(t *testing.T) {
	cases := []struct {
		name, tokenResp, userResp, wantSubstr string
	}{
		{"换token被拒", `{"code":20029,"msg":"redirect_uri unmatch"}`, ``, "换 token 被拒"},
		{"未返回open_id", `{"code":0,"access_token":"x"}`, `{"code":9,"msg":"nope"}`, "未返回 open_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/t", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.tokenResp)) })
			mux.HandleFunc("/u", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.userResp)) })
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := &FeishuClient{AppID: "a", AppSecret: "b",
				TokenURLOverride: srv.URL + "/t", UserInfoURLOverride: srv.URL + "/u"}
			_, err := c.Exchange(context.Background(), "c", "http://h/cb")
			if err == nil {
				t.Fatalf("应报错但成功了")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("错误信息应含 %q，实际 %q", tc.wantSubstr, err.Error())
			}
		})
	}
}

// 凭据为空 ⇒ NewFeishuClient 必须返回 nil（调用方据此明确报错，不静默降级）。
func TestFeishu_凭证缺失返回nil(t *testing.T) {
	for _, tc := range [][2]string{{"", ""}, {"cli_x", ""}, {"", "sec"}} {
		if c := NewFeishuClient(tc[0], tc[1]); c != nil {
			t.Errorf("凭据 %q/%q 不完整时应返回 nil，实际 %+v", tc[0], tc[1], c)
		}
	}
	if c := NewFeishuClient("cli_x", "sec"); c == nil {
		t.Errorf("凭据完整时应返回非 nil")
	}
}
