package access

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// FeishuClient 是真实飞书免登的最小客户端（任务包 D4 真实模式）。
// ★ 本批无法联调（用户尚未提供应用凭据，REMAINING.md#U1）——
//
//	dev 桩（JX_DEV_MODE）顶替；本客户端只保证流程完整：授权 → 回调 → 换 open_id。
type FeishuClient struct {
	AppID     string
	AppSecret string
	HTTP      *http.Client
}

const (
	feishuAuthorizeURL = "https://open.feishu.cn/open-apis/authen/v1/authorize"
	feishuTokenURL     = "https://open.feishu.cn/open-apis/authen/v2/oauth/token"
	feishuUserInfoURL  = "https://open.feishu.cn/open-apis/authen/v1/user_info"
)

// NewFeishuClient 构造客户端（凭据为空 ⇒ 返回 nil，调用方须先判空并明确报错，不静默降级）。
func NewFeishuClient(appID, appSecret string) *FeishuClient {
	if appID == "" || appSecret == "" {
		return nil
	}
	return &FeishuClient{
		AppID:     appID,
		AppSecret: appSecret,
		HTTP:      &http.Client{Timeout: 10 * time.Second},
	}
}

// AuthorizeURL 生成飞书授权跳转地址。
func (f *FeishuClient) AuthorizeURL(redirectURI, state string) string {
	q := url.Values{}
	q.Set("app_id", f.AppID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("scope", "openid")
	return feishuAuthorizeURL + "?" + q.Encode()
}

// Identity 是回调换回来的身份。
type Identity struct {
	OpenID string
	Name   string
}

// Exchange 用授权 code 换 open_id（回调主流程的第二步）。
func (f *FeishuClient) Exchange(ctx context.Context, code, redirectURI string) (Identity, error) {
	var ident Identity

	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     f.AppID,
		"client_secret": f.AppSecret,
		"code":          code,
		"redirect_uri":  redirectURI,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, feishuTokenURL, strings.NewReader(string(body)))
	if err != nil {
		return ident, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.HTTP.Do(req)
	if err != nil {
		return ident, fmt.Errorf("飞书换 token 请求失败: %w", err)
	}
	defer resp.Body.Close()

	var tok struct {
		AccessToken string `json:"access_token"`
		Code        int    `json:"code"`
		Msg         string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return ident, fmt.Errorf("飞书换 token 响应解析失败: %w", err)
	}
	if tok.AccessToken == "" {
		return ident, fmt.Errorf("飞书换 token 被拒（code=%d msg=%s）", tok.Code, tok.Msg)
	}

	ureq, err := http.NewRequestWithContext(ctx, http.MethodGet, feishuUserInfoURL, nil)
	if err != nil {
		return ident, err
	}
	ureq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uresp, err := f.HTTP.Do(ureq)
	if err != nil {
		return ident, fmt.Errorf("飞书取用户信息失败: %w", err)
	}
	defer uresp.Body.Close()

	var ui struct {
		Data struct {
			OpenID string `json:"open_id"`
			Name   string `json:"name"`
		} `json:"data"`
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(uresp.Body, 1<<20)).Decode(&ui); err != nil {
		return ident, fmt.Errorf("飞书用户信息解析失败: %w", err)
	}
	if ui.Data.OpenID == "" {
		return ident, fmt.Errorf("飞书未返回 open_id（code=%d msg=%s）", ui.Code, ui.Msg)
	}
	return Identity{OpenID: ui.Data.OpenID, Name: ui.Data.Name}, nil
}
