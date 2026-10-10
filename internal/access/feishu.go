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

	// ★ 可覆写以便**形状断言测试**（默认取官方端点；生产不设）。
	// ★★ 动因（2026-10-10）：本文件原先无任何测试，导致 `AuthorizeURL` 的
	//   域名/参数名/缺失必填项**三处偏差一路溜到联调前才被发现**。见下方常量注释。
	AuthorizeURLOverride string
	TokenURLOverride     string
	UserInfoURLOverride  string
}

// ★★★ 官方端点（据 open.feishu.cn 开发文档，2026-08-25 更新）：
//
//	获取授权码   https://accounts.feishu.cn/open-apis/authen/v1/authorize
//	            必填 client_id / response_type=code / redirect_uri
//	获取 token   https://open.feishu.cn/open-apis/authen/v2/oauth/token   ★ v2 已被官方标为
//	            历史版本（v3 = https://accounts.feishu.cn/oauth/v3/token），本版沿用 v2 可用。
//	获取用户信息 https://open.feishu.cn/open-apis/authen/v1/user_info
//
// ★★ 2026-10-10 修正（WorkBuddy 依官方文档核对，原实现三处偏差）：
//
//	① 域名：原 `open.feishu.cn` ⇒ 授权端点在 **`accounts.feishu.cn`**；
//	② 参数名：原 `app_id=` ⇒ 官方为 **`client_id=`**；
//	③ 缺 **`response_type=code`**（官方标注**必填**）。
//
// ★ 三者都不会在 dev 桩下暴露（dev 不经过这条路），**只有真联调才撞得到** —— 故必须由
// 「形状断言用例」兜住，不能靠"看着对"。
const (
	feishuAuthorizeURL = "https://accounts.feishu.cn/open-apis/authen/v1/authorize"
	feishuTokenURL     = "https://open.feishu.cn/open-apis/authen/v2/oauth/token"
	feishuUserInfoURL  = "https://open.feishu.cn/open-apis/authen/v1/user_info"

	// feishuScope 是免登所需的用户授权范围：取用户基本信息（含姓名）。
	// ★ 官方：`scope` 为空格分隔、区分大小写；**若未在开发者后台申请对应权限，授权时报 20027**。
	feishuScope = "contact:user.base:readonly"
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

// httpClient 返回可用客户端（★ HTTP 为 nil 时兜底，避免调用方忘了设就 nil panic）。
func (f *FeishuClient) httpClient() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// authorizeEndpoint / tokenEndpoint / userInfoEndpoint 便于测试覆写。
func (f *FeishuClient) authorizeEndpoint() string {
	if f.AuthorizeURLOverride != "" {
		return f.AuthorizeURLOverride
	}
	return feishuAuthorizeURL
}
func (f *FeishuClient) tokenEndpoint() string {
	if f.TokenURLOverride != "" {
		return f.TokenURLOverride
	}
	return feishuTokenURL
}
func (f *FeishuClient) userInfoEndpoint() string {
	if f.UserInfoURLOverride != "" {
		return f.UserInfoURLOverride
	}
	return feishuUserInfoURL
}

// AuthorizeURL 生成飞书授权跳转地址。
//
// ★ 参数名与取值**必须与官方一致**（见上方端点注释）：client_id / response_type=code /
// redirect_uri / scope / state。★ 少一个必填项或写错参数名，都会在真联调时被飞书拒掉，
// 而**dev 桩下完全看不出来**。
func (f *FeishuClient) AuthorizeURL(redirectURI, state string) string {
	q := url.Values{}
	q.Set("client_id", f.AppID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", feishuScope)
	if state != "" {
		q.Set("state", state)
	}
	return f.authorizeEndpoint() + "?" + q.Encode()
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.tokenEndpoint(), strings.NewReader(string(body)))
	if err != nil {
		return ident, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.httpClient().Do(req)
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

	ureq, err := http.NewRequestWithContext(ctx, http.MethodGet, f.userInfoEndpoint(), nil)
	if err != nil {
		return ident, err
	}
	ureq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uresp, err := f.httpClient().Do(ureq)
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
