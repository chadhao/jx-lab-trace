package access

// ===== N-028 · FIX-12（D4 单测）=====
//
//	D1 SDK 分支请求构造断言：端点＝官方 passport 常量、参数含
//	  grant_type/client_id/client_secret/code、★ 不含 redirect_uri、
//	  Content-Type=application/x-www-form-urlencoded（官方 app-login 文档）。
//	D1 passport 响应形状解析：官方文档示例向量（OAuth2 平铺成功体，无 code/msg 外层）
//	  ＋ 错误体两形状（error/error_description、code/msg）均映射 ErrAuthCodeRejected。
//
// ★ 为什么必须配形状断言（本文件头部判据）：SDK 分支「dev 桩绕过、无法构造真实
//   SDK code ⇒ 只有 mock HTTP 逐字段能兜住端点/参数偏差」——与 AuthorizeURL 三处
//   偏差同款风险（见 feishu_test.go 头注）。

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

// TestFeishu_ExchangeSDK_官方端点与请求构造 ★ D4.1：
// ① 默认端点常量＝passport.feishu.cn/suite/passport/oauth/token（未覆写时不走 v2）；
// ② mock HTTP 逐字段断言请求构造（POST / form / 四参数齐 / ★ 无 redirect_uri）。
func TestFeishu_ExchangeSDK_官方端点与请求构造(t *testing.T) {
	// ① 官方端点常量断言（形状断言的第一层：域名与路径）
	c0 := &FeishuClient{AppID: "cli_x", AppSecret: "sec_x"}
	def := c0.passportTokenEndpoint()
	u, err := url.Parse(def)
	if err != nil {
		t.Fatalf("passport 端点不可解析: %v", err)
	}
	if u.Scheme != "https" || u.Host != "passport.feishu.cn" {
		t.Fatalf("★ SDK token 端点应为 https://passport.feishu.cn，实际 %s://%s", u.Scheme, u.Host)
	}
	if u.Path != "/suite/passport/oauth/token" {
		t.Fatalf("★ SDK token 路径应为 /suite/passport/oauth/token，实际 %s", u.Path)
	}
	if def == feishuTokenURL {
		t.Fatal("★ SDK 分支绝不能落到网页 v2 端点")
	}

	// ② mock 逐字段断言请求构造
	var got struct {
		Method      string
		Path        string
		ContentType string
		Form        url.Values
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/suite/passport/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		got.Method = r.Method
		got.Path = r.URL.Path
		got.ContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			got.Form, _ = url.ParseQuery(string(raw))
		} else {
			got.Form = url.Values{}
		}
		_, _ = w.Write([]byte(`{"access_token":"sdk-at-1","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"open_id":"ou_sdk_1","name":"李四"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &FeishuClient{
		AppID: "cli_sdk", AppSecret: "sec_sdk",
		PassportTokenURLOverride: srv.URL + "/suite/passport/oauth/token",
		UserInfoURLOverride:      srv.URL + "/userinfo",
	}
	ident, err := c.ExchangeSDK(context.Background(), "sdk-code-1")
	if err != nil {
		t.Fatalf("ExchangeSDK 失败: %v", err)
	}
	if ident.OpenID != "ou_sdk_1" || ident.Name != "李四" {
		t.Fatalf("身份解析错: %+v", ident)
	}

	if got.Method != http.MethodPost {
		t.Errorf("★ SDK token 请求应 POST，实际 %s", got.Method)
	}
	if !strings.HasPrefix(got.ContentType, "application/x-www-form-urlencoded") {
		t.Errorf("★ 官方要求 Content-Type=x-www-form-urlencoded，实际 %q", got.ContentType)
	}
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "cli_sdk",
		"client_secret": "sec_sdk",
		"code":          "sdk-code-1",
	} {
		if got.Form.Get(k) != want {
			t.Errorf("★ token 请求体 %s 应为 %q，实际 %q（全量 %v）", k, want, got.Form.Get(k), got.Form)
		}
	}
	// ★★★ 核心契约：移动端登录不传 redirect_uri
	if _, exists := got.Form["redirect_uri"]; exists {
		t.Errorf("★★ SDK 分支不得传 redirect_uri（官方：移动端登录非必填；网页端点正是根因），实际 %v", got.Form)
	}
}

// TestFeishu_ExchangeSDK_Passport响应形状解析 ★ D4.2：
// 官方 app-login 文档 Response Sample 作成功向量（平铺无外层 code/msg）；
// 错误体两种形状（OAuth2 error / code+msg 并集）均 ⇒ ErrAuthCodeRejected（exchange 映射 401）。
func TestFeishu_ExchangeSDK_Passport响应形状解析(t *testing.T) {
	// 官方 Response Sample（open.feishu.cn document · get access_token）
	const officialSample = `{"access_token":"Q7JWnaIM_kRChuLfreHmpArjOEayt.5XUBJcZr.V0Gst4FdQCtvrd9sAViLXQnQgkpL19brGOjKZQTxb","token_type":"Bearer","expires_in":3600,"refresh_token":"oQ0mMq6MCcueAv0pwx2fQQhxqv__CbLu6G8ySFwafeKww2Def2BJdOkW3.9gCFM.LBQgFri901QaqeuL","refresh_expires_in":864000}`

	t.Run("官方成功样例", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/t", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(officialSample)) })
		mux.HandleFunc("/u", func(w http.ResponseWriter, r *http.Request) {
			// user_info 应拿到该 Bearer
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer Q7JWnaIM_") {
				http.Error(w, `{"code":99991663,"msg":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"open_id":"ou_passport","name":"王五"}}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := &FeishuClient{AppID: "a", AppSecret: "b",
			PassportTokenURLOverride: srv.URL + "/t", UserInfoURLOverride: srv.URL + "/u"}
		ident, err := c.ExchangeSDK(context.Background(), "c1")
		if err != nil {
			t.Fatalf("官方成功样例应解析通过: %v", err)
		}
		if ident.OpenID != "ou_passport" || ident.Name != "王五" {
			t.Fatalf("身份解析错: %+v", ident)
		}
	})

	t.Run("错误体两形状", func(t *testing.T) {
		cases := []struct {
			name, resp string
		}{
			{"OAuth2风格", `{"error":"invalid_grant","error_description":"The authorization code is not found."}`},
			{"code+msg风格", `{"code":20029,"msg":"redirect_uri unmatch"}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(tc.resp))
				}))
				defer srv.Close()
				c := &FeishuClient{AppID: "a", AppSecret: "b",
					PassportTokenURLOverride: srv.URL, UserInfoURLOverride: srv.URL}
				_, err := c.ExchangeSDK(context.Background(), "bad")
				if err == nil {
					t.Fatal("应报错但成功了")
				}
				if !strings.Contains(err.Error(), "换 token 被拒") {
					t.Errorf("错误信息应含「换 token 被拒」，实际 %q", err.Error())
				}
				// 两形状必须走同一哨兵（exchange 端点据此映射 401）
				if !strings.Contains(err.Error(), ErrAuthCodeRejected.Error()) {
					t.Errorf("★ 应包裹 ErrAuthCodeRejected（401 契约依赖），实际 %q", err.Error())
				}
			})
		}
	})

	// 回归钉：网页流 Exchange 仍走 v2 JSON＋redirect_uri（兼容红线的 access 层镜像）
	t.Run("网页流Exchange仍带redirect_uri", func(t *testing.T) {
		var body map[string]string
		var ct string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/userinfo" {
				_, _ = w.Write([]byte(`{"code":0,"data":{"open_id":"ou_web","name":"张三"}}`))
				return
			}
			ct = r.Header.Get("Content-Type")
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			_, _ = w.Write([]byte(`{"code":0,"access_token":"web-at"}`))
		}))
		defer srv.Close()
		c := &FeishuClient{AppID: "cli_a", AppSecret: "sec_b",
			TokenURLOverride: srv.URL, UserInfoURLOverride: srv.URL + "/userinfo"}
		if _, err := c.Exchange(context.Background(), "code-1", "http://h/cb"); err != nil {
			t.Fatalf("Exchange 失败: %v", err)
		}
		if body["redirect_uri"] != "http://h/cb" {
			t.Errorf("★ 网页流必须仍传 redirect_uri，实际 %v", body)
		}
		if !strings.HasPrefix(ct, "application/json") {
			t.Errorf("★ 网页流 Content-Type 仍应 json，实际 %q", ct)
		}
	})
}
