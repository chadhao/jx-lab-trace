package httpapi

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/labstack/echo/v4"
)

// handleFeishuEvent D4.1：飞书事件回调（免权限，飞书服务器调用）。
//
//	① URL 校验 challenge 应答；② 验签（X-Lark-Signature，HMAC-SHA256，key=JX_FEISHU_ENCRYPT_KEY）；
//	③ 解析事件：contact.user.created_v3 ⇒ upsert；contact.user.deleted_v3 ⇒ 停用+会话失效+审计；其余忽略。
//
// ★ 未配置 JX_FEISHU_ENCRYPT_KEY ⇒ **404**（明确拒绝，不静默降级）。
// ★ 验签失败与 challenge/token 失败必须是 **4xx 且不同响应体**（任务包 §8）。
func (s *Server) handleFeishuEvent(c echo.Context) error {
	if s.Cfg.FeishuEncryptKey == "" {
		return echo.NewHTTPError(http.StatusNotFound, "事件回调未启用（未配置 JX_FEISHU_ENCRYPT_KEY）")
	}

	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, 2<<20))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "读取请求体失败")
	}

	// ② 验签：官方口径 sha256(timestamp + nonce + encrypt_key + body) 的十六进制。
	if !feishuSignatureOK(c.Request(), raw, s.Cfg.FeishuEncryptKey) {
		return echo.NewHTTPError(http.StatusForbidden, map[string]interface{}{
			"error": "签名验证失败",
		})
	}

	// 载荷：Encrypt Key 配置后飞书 body 形如 {"encrypt":"base64..."}；也兼容未加密明文。
	payload, err := feishuDecryptPayload(raw, s.Cfg.FeishuEncryptKey)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, map[string]interface{}{
			"error": "请求体解密失败",
		})
	}

	var evt struct {
		Schema string `json:"schema"`
		Type   string `json:"type"` // 旧格式 url_verification / event_callback
		// URL 校验
		Challenge string `json:"challenge"`
		Token     string `json:"token"`
		// 事件（schema 2.0）
		Header struct {
			EventType string `json:"event_type"`
			Token     string `json:"token"`
		} `json:"header"`
		Event struct {
			Object struct {
				OpenID string `json:"open_id"`
				Name   string `json:"name"`
			} `json:"object"`
		} `json:"event"`
	}
	if err := json.Unmarshal(payload, &evt); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, map[string]string{
			"error": "请求体不是合法事件 JSON",
		})
	}

	// verification_token 一致才认（若配置了）。
	tok := evt.Token
	if tok == "" {
		tok = evt.Header.Token
	}
	if s.Cfg.FeishuVerificationToken != "" && tok != s.Cfg.FeishuVerificationToken {
		// ★ 与验签失败**不同**的响应体（任务包 §8 易错点）。
		return echo.NewHTTPError(http.StatusForbidden, map[string]interface{}{
			"error": "verification token 不匹配",
		})
	}

	// ① URL 校验 challenge 应答。
	if evt.Type == "url_verification" || evt.Challenge != "" {
		if evt.Challenge == "" {
			return echo.NewHTTPError(http.StatusBadRequest, map[string]interface{}{
				"error": "缺少 challenge",
			})
		}
		return c.JSON(http.StatusOK, map[string]string{"challenge": evt.Challenge})
	}

	// ③ 事件处理。
	eventType := evt.Header.EventType
	openID := strings.TrimSpace(evt.Event.Object.OpenID)
	name := strings.TrimSpace(evt.Event.Object.Name)

	switch {
	case eventType == "contact.user.created_v3":
		return s.handleFeishuUserCreated(c, openID, name)
	case eventType == "contact.user.deleted_v3":
		return s.handleFeishuUserDeleted(c, openID)
	case eventType == "":
		// 无 event_type 的未知体：记 debug，200 平滑确认（不报错、不重试风暴）。
		c.Logger().Debug("飞书事件：无 event_type，忽略；body=", string(payload))
		return c.JSON(http.StatusOK, map[string]string{"result": "ignored"})
	default:
		c.Logger().Debug("飞书事件：未识别类型 ", eventType, "，忽略")
		return c.JSON(http.StatusOK, map[string]string{"result": "ignored"})
	}
}

// handleFeishuUserCreated 入职建号：upsert m_user（无角色，同 D3）。
func (s *Server) handleFeishuUserCreated(c echo.Context, openID, name string) error {
	if openID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, map[string]string{
			"error": "created 事件缺 open_id",
		})
	}
	r, err := s.Store.UpsertUser(c.Request().Context(), openID, name, "feishu_event")
	if err != nil {
		c.Logger().Error("飞书入职建号失败: ", err)
		return echo.NewHTTPError(http.StatusInternalServerError, map[string]string{
			"error": "入职建号失败",
		})
	}
	// 审计：系统主体（非人）。
	if err := s.Store.AppendAudit(c.Request().Context(), audit.Entry{
		Entity: "m_user", Action: "upsert", Field: "open_id",
		NewValue:    "created=" + strconv.Itoa(r.Created) + " updated=" + strconv.Itoa(r.Updated) + " skipped=" + strconv.Itoa(r.Skipped),
		ActorOpenID: "feishu", ActorName: "飞书事件", ActorRole: "system",
		Reason: "contact.user.created_v3 入职建号（不绑角色）",
	}); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, map[string]string{
			"error": "写审计失败",
		})
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"result":  "ok",
		"created": r.Created, "updated": r.Updated, "skipped": r.Skipped,
	})
}

// handleFeishuUserDeleted 离职停用：会话全失效 + 拒再登录 + 审计（安全要求）。
func (s *Server) handleFeishuUserDeleted(c echo.Context, openID string) error {
	if openID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, map[string]string{
			"error": "deleted 事件缺 open_id",
		})
	}
	ctx := c.Request().Context()
	old, err := s.Store.DisableUser(ctx, openID, "feishu_event")
	if err != nil {
		c.Logger().Error("飞书离职停用失败: ", err)
		return echo.NewHTTPError(http.StatusInternalServerError, map[string]string{
			"error": "离职停用失败",
		})
	}
	revoked, err := s.Store.RevokeUserSessions(ctx, openID)
	if err != nil {
		c.Logger().Error("离职会话失效失败: ", err)
		return echo.NewHTTPError(http.StatusInternalServerError, map[string]string{
			"error": "离职会话失效失败",
		})
	}
	if err := s.Store.AppendAudit(ctx, audit.Entry{
		Entity: "m_user", Action: "disable", Field: "status",
		OldValue: old, NewValue: "停用",
		ActorOpenID: "feishu", ActorName: "飞书事件", ActorRole: "system",
		Reason: "contact.user.deleted_v3 离职停用：会话全失效（revoked=" + strconv.FormatInt(revoked, 10) + "）",
	}); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, map[string]string{
			"error": "写审计失败",
		})
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"result":           "ok",
		"disabled":         true,
		"sessions_revoked": revoked,
	})
}

// feishuSignatureOK 校验 X-Lark-Signature。
// 官方：hex(sha256(timestamp + nonce + encrypt_key + rawBody))；头 X-Lark-Request-Timestamp / X-Lark-Request-Nonce / X-Lark-Signature。
func feishuSignatureOK(r *http.Request, rawBody []byte, encryptKey string) bool {
	sig := r.Header.Get("X-Lark-Signature")
	if sig == "" {
		return false
	}
	ts := r.Header.Get("X-Lark-Request-Timestamp")
	nonce := r.Header.Get("X-Lark-Request-Nonce")
	h := sha256.New()
	_, _ = h.Write([]byte(ts + nonce + encryptKey))
	_, _ = h.Write(rawBody)
	want := hex.EncodeToString(h.Sum(nil))
	// 定长比较，避免 == 提前退出的旁路（这里主要求正确性）。
	if len(sig) != len(want) {
		return false
	}
	var diff byte
	for i := range want {
		diff |= sig[i] ^ want[i]
	}
	return diff == 0
}

// feishuDecryptPayload 解析请求体：
//   - {"encrypt": "..."} ⇒ AES-256-CBC 解密（key=sha256(encryptKey)，前 16 字节 IV，PKCS7 去填充）；
//   - 否则按明文 JSON 原样返回（兼容未开加密、仅验签的场景 / 单测）。
func feishuDecryptPayload(raw []byte, encryptKey string) ([]byte, error) {
	var wrapper struct {
		Encrypt string `json:"encrypt"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper.Encrypt != "" {
		return feishuAESDecrypt(wrapper.Encrypt, encryptKey)
	}
	// 明文：仍要求至少是 JSON 对象
	if !json.Valid(raw) {
		return nil, errors.New("body 非 JSON")
	}
	return raw, nil
}

// feishuAESDecrypt 实现飞书 Encrypt Key 解密约定。
func feishuAESDecrypt(b64Cipher, encryptKey string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(b64Cipher)
	if err != nil {
		// 飞书文档有的示例用 raw URL-safe；两种都试。
		data, err = base64.RawStdEncoding.DecodeString(b64Cipher)
		if err != nil {
			return nil, err
		}
	}
	if len(data) < aes.BlockSize || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("密文长度非法")
	}
	keySum := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(keySum[:])
	if err != nil {
		return nil, err
	}
	iv := data[:aes.BlockSize]
	ct := data[aes.BlockSize:]
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
	// PKCS7 去填充
	if len(plain) == 0 {
		return nil, errors.New("空明文")
	}
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(plain) {
		return nil, errors.New("PKCS7 填充非法")
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return nil, errors.New("PKCS7 填充非法")
		}
	}
	return plain[:len(plain)-pad], nil
}
