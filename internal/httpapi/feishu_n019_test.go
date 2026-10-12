package httpapi

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// ══════════════════════════════════════════════════════════════════════════
// N-019：URL 校验（challenge）豁免签名校验（MIMO-NEXT-FIX-04 D2 五条测试）。
// ══════════════════════════════════════════════════════════════════════════

// feishuAESEncryptForTest 按飞书 Encrypt Key **加密**约定构造真实密文：
// key=sha256(encryptKey)、随机 IV 前置、PKCS7 填充、base64 —— 与
// feishuAESDecrypt 互逆（官方算法，D2-2 要求用真实密文而非手写桩）。
func feishuAESEncryptForTest(plain []byte, encryptKey string) string {
	keySum := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(keySum[:])
	if err != nil {
		panic(err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		panic(err)
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	return base64.StdEncoding.EncodeToString(append(iv, ct...))
}

// postEvent 以指定签名头（空串 ⇒ 完全不设签名头）投递 body。
func postEvent(e *echo.Echo, body []byte, ts, nonce, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/feishu/event", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-Lark-Request-Timestamp", ts)
		req.Header.Set("X-Lark-Request-Nonce", nonce)
		req.Header.Set("X-Lark-Signature", sig)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// D2-1 ★ url_verification（明文、**无签名头**）⇒ 200 回显 challenge，且 1 秒内返回。
func TestN019_Challenge明文无签名_200回显毫秒级(t *testing.T) {
	const key = "enc-key-1"
	_, e := newEventServer(key, "tok-1")
	body := []byte(`{"schema":"2.0","type":"url_verification","challenge":"ch-plain-1","token":"tok-1"}`)

	start := time.Now()
	rec := postEvent(e, body, "", "", "")
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("★明文 challenge（无签名头）应 200 —— 官方口径 URL 校验不参与验签，实际 %d body=%s",
			rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["challenge"] != "ch-plain-1" {
		t.Fatalf("★challenge 应原样回显，实际 %q", out["challenge"])
	}
	if elapsed > time.Second {
		t.Fatalf("★官方要求 challenge 1 秒内返回，实际耗时 %v", elapsed)
	}
}

// D2-2 ★ url_verification（**加密**：按官方算法构造真实密文）⇒ 200 回显。
func TestN019_Challenge加密_200回显(t *testing.T) {
	const key = "enc-key-1"
	_, e := newEventServer(key, "tok-1")
	plain := []byte(`{"schema":"2.0","type":"url_verification","challenge":"ch-enc-9","token":"tok-1"}`)
	body := []byte(`{"encrypt":"` + feishuAESEncryptForTest(plain, key) + `"}`)

	// 飞书后台已配置 Encrypt Key ⇒ 真实 challenge 是加密形态；且不带签名头。
	rec := postEvent(e, body, "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("★加密 challenge 应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["challenge"] != "ch-enc-9" {
		t.Fatalf("★challenge 应原样回显，实际 %q", out["challenge"])
	}
}

// D2-3 ★ challenge token 错误 ⇒ 4xx，且响应体与签名失败**不同**。
func TestN019_ChallengeToken错_4xx且与签名失败响应体不同(t *testing.T) {
	const key = "enc-key-1"
	_, e := newEventServer(key, "tok-1")

	// ① challenge + token 错（无签名头）⇒ 4xx、点名 token
	badTok := []byte(`{"type":"url_verification","challenge":"c","token":"wrong-tok"}`)
	rec1 := postEvent(e, badTok, "", "", "")
	if rec1.Code < 400 || rec1.Code >= 500 {
		t.Fatalf("★challenge token 错应 4xx，实际 %d body=%s", rec1.Code, rec1.Body.String())
	}
	tokBody := rec1.Body.String()
	if !strings.Contains(tokBody, "token") {
		t.Fatalf("token 失败响应体应点名 token：%s", tokBody)
	}
	if strings.Contains(tokBody, "签名验证失败") {
		t.Fatalf("★token 失败响应体不得与签名失败混淆：%s", tokBody)
	}

	// ② 真实事件 + 签名错 ⇒ 403 签名失败（对照组）
	evt := []byte(`{"schema":"2.0","header":{"event_type":"contact.user.created_v3","token":"tok-1"},` +
		`"event":{"object":{"open_id":"ou_n019_c","name":"n"}}}`)
	rec2 := postEvent(e, evt, "1", "n", "deadbeef")
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("★签名错应 403，实际 %d", rec2.Code)
	}
	sigBody := rec2.Body.String()
	if !strings.Contains(sigBody, "签名验证失败") {
		t.Fatalf("签名失败响应体应点名签名：%s", sigBody)
	}
	if tokBody == sigBody {
		t.Fatalf("★★token 失败与签名失败响应体必须不同：%s", tokBody)
	}
}

// D2-4 ★ 回归：真实事件的验签**不被** challenge 豁免波及。
func TestN019_事件路径验签不被challenge豁免波及(t *testing.T) {
	const key = "enc-key-1"
	_, e := newEventServer(key, "tok-1")

	// ① created_v3 + 签名错 ⇒ 403 签名验证失败（豁免不波及事件）
	evt := []byte(`{"schema":"2.0","header":{"event_type":"contact.user.created_v3","token":"tok-1"},` +
		`"event":{"object":{"open_id":"ou_n019_x","name":"n"}}}`)
	rec1 := postEvent(e, evt, "1", "n", "deadbeef")
	if rec1.Code != http.StatusForbidden {
		t.Fatalf("★真实事件签名错应 403（不得被豁免波及），实际 %d body=%s", rec1.Code, rec1.Body.String())
	}
	if !strings.Contains(rec1.Body.String(), "签名验证失败") {
		t.Fatalf("应点名签名失败：%s", rec1.Body.String())
	}

	// ② 同为 created_v3、签名对 ⇒ 通过验签**进入事件处理器**
	//（用缺 open_id 的变体钉住「已进 handler」——完整 upsert 200 由 DB 集成用例
	//  TestTC_FeishuEvent_UserCreated_Upsert幂等（服务器 JX_TEST_DB=1）承担）
	noOpen := []byte(`{"schema":"2.0","header":{"event_type":"contact.user.created_v3","token":"tok-1"},` +
		`"event":{"object":{}}}`)
	rec2 := postEvent(e, noOpen, "2", "n2", signFeishu("2", "n2", key, noOpen))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("★签名对的 created 应通过验签进入处理器（缺 open_id ⇒ 400），实际 %d body=%s",
			rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "open_id") {
		t.Fatalf("应点名缺 open_id：%s", rec2.Body.String())
	}

	// ③ 真实事件（未知类型）、签名对 ⇒ 200 正常处理（不报错）
	unk := []byte(`{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"tok-1"},"event":{}}`)
	rec3 := postEvent(e, unk, "3", "n3", signFeishu("3", "n3", key, unk))
	if rec3.Code != http.StatusOK {
		t.Fatalf("★签名对的真实事件应 200 正常处理，实际 %d body=%s", rec3.Code, rec3.Body.String())
	}
	if !strings.Contains(rec3.Body.String(), "ignored") {
		t.Fatalf("应返回 ignored：%s", rec3.Body.String())
	}
}
