package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/config"
	"github.com/labstack/echo/v4"
)

// ══════════════════════════════════════════════════════════════════════════
// D1：飞书回调拒绝路径回**极简 HTML**，不再回裸 JSON（纯单测，不依赖库）。
// ══════════════════════════════════════════════════════════════════════════

func newCallbackServer(t *testing.T) (*Server, *echo.Echo) {
	t.Helper()
	cfg := &config.Config{
		DevMode:    false,
		CookieName: cookieName,
	}
	s := &Server{
		Cfg: cfg,
		Feishu: &access.FeishuClient{
			AppID: "cli_test", AppSecret: "sec_test",
			// 换 token 打到必然失败的地址（缺 code 路径不会用到）
			TokenURLOverride:    "http://127.0.0.1:1/t",
			UserInfoURLOverride: "http://127.0.0.1:1/u",
		},
		Version: "test",
	}
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = jsonErrorHandler(e)
	e.GET("/api/auth/feishu/callback", s.handleFeishuCallback)
	return s, e
}

// ★ 验收：curl 直打回调（无 code）⇒ Content-Type: text/html 且页面含「联系管理员」。
func TestFeishuCallback_无code_返回HTML友好页(t *testing.T) {
	_, e := newCallbackServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/feishu/callback", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("★无 code 应 400，实际 %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("★拒绝路径 Content-Type 必须是 text/html，实际 %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "联系管理员") {
		t.Fatalf("★页面必须含「联系管理员」，实际 %q", body)
	}
	if !strings.Contains(body, `href="/"`) {
		t.Fatalf("★页面必须有指回 / 的链接")
	}
	// 极简：不得引用外部 JS/前端路由依赖
	if strings.Contains(body, "<script") {
		t.Fatalf("★页面不应依赖 JS（新用户第一屏必须独立成立）")
	}
}

// 伪造 code（token 接口不可达）⇒ 也走 HTML，且不泄露内部错误细节。
func TestFeishuCallback_换token失败_返回HTML(t *testing.T) {
	_, e := newCallbackServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/feishu/callback?code=forged", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("★换 token 失败应 502，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("★Content-Type 必须 text/html，实际 %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "联系管理员") {
		t.Fatalf("★页面必须含「联系管理员」")
	}
	// 不得回裸 JSON 形态
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Fatalf("★不得再回裸 JSON：%q", body)
	}
}

// dev 桩拒绝保持 JSON —— 通过对比同一哨兵的呈现路径（dev handler 需 DB，此处只验 HTML 不外溢到 dev）。
// 完整 dev JSON 形状由既有 TC-M0-02（需 DB）钉住。

// ══════════════════════════════════════════════════════════════════════════
// D4：challenge 往返、签名错 ⇒ 403、未配置 ⇒ 404（纯单测）。
// ══════════════════════════════════════════════════════════════════════════

func newEventServer(encryptKey, verifyToken string) (*Server, *echo.Echo) {
	cfg := &config.Config{
		DevMode:                 false,
		CookieName:              cookieName,
		FeishuEncryptKey:        encryptKey,
		FeishuVerificationToken: verifyToken,
	}
	s := &Server{Cfg: cfg, Version: "test"}
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = jsonErrorHandler(e)
	e.POST("/feishu/event", s.handleFeishuEvent)
	return s, e
}

func signFeishu(ts, nonce, key string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(ts + nonce + key))
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// ★ 未配置密钥 ⇒ 端点 404（明确拒绝，不静默降级）。
func TestFeishuEvent_未配置密钥_404(t *testing.T) {
	_, e := newEventServer("", "")
	req := httptest.NewRequest(http.MethodPost, "/feishu/event", strings.NewReader(`{"type":"url_verification","challenge":"c1"}`))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("★未配置密钥应 404，实际 %d", rec.Code)
	}
}

// ★ challenge 往返：验签通过 + url_verification ⇒ 原样回 challenge。
func TestFeishuEvent_Challenge往返(t *testing.T) {
	const key = "enc-key-1"
	_, e := newEventServer(key, "tok-1")
	body := []byte(`{"schema":"2.0","type":"url_verification","challenge":"ch-abc","token":"tok-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/feishu/event", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lark-Request-Timestamp", "1728700000")
	req.Header.Set("X-Lark-Request-Nonce", "n1")
	req.Header.Set("X-Lark-Signature", signFeishu("1728700000", "n1", key, body))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("challenge 应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["challenge"] != "ch-abc" {
		t.Fatalf("★challenge 应原样往返，实际 %q", out["challenge"])
	}
}

// ★ 签名错 ⇒ 403，且响应体与 token 失败**不同**。
func TestFeishuEvent_签名错_403且与token失败响应体不同(t *testing.T) {
	const key = "enc-key-1"
	_, e := newEventServer(key, "tok-1")
	body := []byte(`{"type":"url_verification","challenge":"c","token":"tok-1"}`)

	// ① 签名错
	req := httptest.NewRequest(http.MethodPost, "/feishu/event", strings.NewReader(string(body)))
	req.Header.Set("X-Lark-Request-Timestamp", "1")
	req.Header.Set("X-Lark-Request-Nonce", "n")
	req.Header.Set("X-Lark-Signature", "deadbeef")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("★签名错应 403，实际 %d", rec.Code)
	}
	sigBody := rec.Body.String()
	if !strings.Contains(sigBody, "签名验证失败") {
		t.Fatalf("签名失败响应体应点名签名：%s", sigBody)
	}

	// ② 验签通过但 token 错 ⇒ 同 403、**不同响应体**
	badTok := []byte(`{"type":"url_verification","challenge":"c","token":"wrong-tok"}`)
	req2 := httptest.NewRequest(http.MethodPost, "/feishu/event", strings.NewReader(string(badTok)))
	req2.Header.Set("X-Lark-Request-Timestamp", "1")
	req2.Header.Set("X-Lark-Request-Nonce", "n")
	req2.Header.Set("X-Lark-Signature", signFeishu("1", "n", key, badTok))
	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("token 错应 403，实际 %d", rec2.Code)
	}
	if rec2.Body.String() == sigBody {
		t.Fatalf("★★验签失败与 token 失败响应体必须不同：%s", sigBody)
	}
	if !strings.Contains(rec2.Body.String(), "token") {
		t.Fatalf("token 失败响应体应点名 token：%s", rec2.Body.String())
	}
}

// 未知事件类型 ⇒ 200 ignored（不报错）。
func TestFeishuEvent_未知类型_忽略(t *testing.T) {
	const key = "k"
	_, e := newEventServer(key, "")
	body := []byte(`{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":""},"event":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/feishu/event", strings.NewReader(string(body)))
	req.Header.Set("X-Lark-Request-Timestamp", "2")
	req.Header.Set("X-Lark-Request-Nonce", "n2")
	req.Header.Set("X-Lark-Signature", signFeishu("2", "n2", key, body))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("未知类型应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("应返回 ignored：%s", rec.Body.String())
	}
}
