package access

import (
	"context"
	"encoding/json"
	"errors"
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

	// ★ N-028 D1 移动端（SDK）token 端点覆写（形状断言测试用；生产不设）。
	PassportTokenURLOverride string

	// ★ D3 组织同步端点覆写（形状断言测试用；生产不设）。
	TenantTokenURLOverride   string
	DeptChildrenURLOverride  string
	DeptUsersURLOverrideBase string // 前缀；实际路径拼 department_id
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

	// ★★ N-028 D1 移动端（SDK）token 端点（官方「app-login/get-access_token」，2026-10-21 核对）：
	//   移动端登录 → passport.feishu.cn/suite/passport/oauth/token，★ 不传 redirect_uri；
	//   Request Header Content-Type=application/x-www-form-urlencoded。
	//   ★★ 动因：exchange 曾用网页 OAuth v2 端点（带 redirect_uri）处理移动端 SDK 的 code
	//   ⇒ 换出 open_id 语义错位 ⇒ AUTH_NO_ROLE（绑定/通讯录/映射均已排查排除）。
	feishuPassportTokenURL = "https://passport.feishu.cn/suite/passport/oauth/token"

	// ★ D3 组织同步（2026-10-12 按官方文档核对）：
	//   租户凭据 https://open.feishu.cn/document/server-docs/authentication-management/access-token/tenant_access_token_internal
	//   部门子列表 https://open.feishu.cn/document/server-docs/contact-v3/department/children
	//   部门用户   https://open.feishu.cn/document/server-docs/contact-v3/user/find_by_department
	feishuTenantTokenURL   = "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal"
	feishuDeptChildrenURL  = "https://open.feishu.cn/open-apis/contact/v3/departments/%s/children"
	feishuDeptUsersURLBase = "https://open.feishu.cn/open-apis/contact/v3/users/find_by_department"

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
func (f *FeishuClient) passportTokenEndpoint() string {
	if f.PassportTokenURLOverride != "" {
		return f.PassportTokenURLOverride
	}
	return feishuPassportTokenURL
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

// ErrAuthCodeRejected 是「飞书明确拒绝该授权码」（无效 / 过期 / 已使用）的哨兵。
// ★ exchange 端点据此映射 401；callback 对一切错误仍回友好 HTML 502（行为不变）。
var ErrAuthCodeRejected = errors.New("飞书拒绝了授权码")

// Exchange 用授权 code 换 open_id（网页流 / 回调主流程的第二步）。
// ★ N-028 兼容红线：本函数行为一字不改 —— v2 端点 JSON 报文 ＋ redirect_uri。
func (f *FeishuClient) Exchange(ctx context.Context, code, redirectURI string) (Identity, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     f.AppID,
		"client_secret": f.AppSecret,
		"code":          code,
		"redirect_uri":  redirectURI,
	})
	return f.exchangeAt(ctx, f.tokenEndpoint(), string(body), "application/json")
}

// ExchangeSDK 移动端（SDK）授权码换 open_id（N-028 D1）。
//
// ★ 官方「app-login/get-access_token」：移动端登录 ⇒
//
//	POST https://passport.feishu.cn/suite/passport/oauth/token
//	Content-Type=application/x-www-form-urlencoded
//	参数 grant_type / client_id / client_secret / code —— ★ 不传 redirect_uri（网页登录才必填）。
//
// ★ 挑战码（code_verifier）为 P1 后续扩展，本次不做。
// ★ user_info 流程与网页流共用（/open-apis/authen/v1/user_info 两种 token 均兼容；
//
//	若真机实测不兼容再改用移动端文档指定的用户信息端点并开议题回执）。
func (f *FeishuClient) ExchangeSDK(ctx context.Context, code string) (Identity, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", f.AppID)
	form.Set("client_secret", f.AppSecret)
	form.Set("code", code)
	// ★ 不传 redirect_uri（官方明确移动端登录非必填）。
	return f.exchangeAt(ctx, f.passportTokenEndpoint(), form.Encode(), "application/x-www-form-urlencoded")
}

// exchangeAt 是两分支共用链：POST token 端点 → 解析 → user_info 换 Identity。
//
// ★ token 响应形状两种兼容（官方 app-login 文档为准）：
//
//	passport 成功体是 OAuth2 平铺（顶层 access_token/token_type/expires_in，无 code/msg 外层）；
//	v2/错误体带 code/msg 或 error/error_description —— 下方四字段并集解析，两分支共用。
func (f *FeishuClient) exchangeAt(ctx context.Context, tokenURL, body, contentType string) (Identity, error) {
	var ident Identity

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(body))
	if err != nil {
		return ident, err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return ident, fmt.Errorf("飞书换 token 请求失败: %w", err)
	}
	defer resp.Body.Close()

	var tok struct {
		AccessToken string `json:"access_token"`
		Code        int    `json:"code"`
		Msg         string `json:"msg"`
		// ★ v2 端点的 OAuth2 风格错误体（实测 2026-10-20：无效 code 回
		//   {"error":"invalid_grant","error_description":"…","code":20003}）。
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return ident, fmt.Errorf("飞书换 token 响应解析失败: %w", err)
	}
	if tok.AccessToken == "" {
		detail := tok.Msg
		if detail == "" {
			detail = tok.Description
		}
		// ★ 无效/过期/已用的授权码 ⇒ 哨兵 ErrAuthCodeRejected（exchange 端点映射 401）；
		//   callback 仍按「一切错误 ⇒ 友好页 502」处理（行为不变）。
		//   消息保留原文「换 token 被拒」（既有测试与日志习惯依赖该措辞）。
		return ident, fmt.Errorf("飞书换 token 被拒（code=%d msg=%s error=%s）: %w",
			tok.Code, detail, tok.Error, ErrAuthCodeRejected)
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

// ══════════════════════════════════════════════════════════════════════════
// D3 组织同步（2026-10-12）：tenant_access_token → 递归部门 → 逐部门拉用户。
// ★ 服务端同步**必须用 tenant 身份**（应用身份）；user_access_token 的过滤范围不同。
// ══════════════════════════════════════════════════════════════════════════

func (f *FeishuClient) tenantTokenEndpoint() string {
	if f.TenantTokenURLOverride != "" {
		return f.TenantTokenURLOverride
	}
	return feishuTenantTokenURL
}

func (f *FeishuClient) deptChildrenEndpoint(departmentID string) string {
	base := f.DeptChildrenURLOverride
	if base == "" {
		base = feishuDeptChildrenURL
	}
	return strings.Replace(base, "%s", url.PathEscape(departmentID), 1)
}

func (f *FeishuClient) deptUsersEndpoint() string {
	if f.DeptUsersURLOverrideBase != "" {
		return f.DeptUsersURLOverrideBase
	}
	return feishuDeptUsersURLBase
}

// TenantAccessToken 取租户级凭据（官方：POST auth/v3/tenant_access_token/internal，
// 请求体 app_id+app_secret；响应 code/msg/tenant_access_token/expire）。
func (f *FeishuClient) TenantAccessToken(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"app_id":     f.AppID,
		"app_secret": f.AppSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.tenantTokenEndpoint(), strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("飞书取 tenant_access_token 请求失败: %w", err)
	}
	defer resp.Body.Close()

	var tok struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("飞书 tenant_access_token 响应解析失败: %w", err)
	}
	if tok.TenantAccessToken == "" {
		return "", fmt.Errorf("飞书 tenant_access_token 被拒（code=%d msg=%s）", tok.Code, tok.Msg)
	}
	return tok.TenantAccessToken, nil
}

// OrgUser 是通讯录同步需要的最小用户形状（★ 不取手机号等敏感字段——一期用不到就别申请）。
type OrgUser struct {
	OpenID string
	Name   string
}

// DeptNode 是部门树节点（只取递归所需字段）。
type DeptNode struct {
	OpenDepartmentID string
	Name             string
}

// ListChildDepartments 拉某部门的直接子部门（官方：GET contact/v3/departments/{id}/children）。
// departmentID=0 表示根部门；★ 全员通讯录权限范围内才可用。
func (f *FeishuClient) ListChildDepartments(ctx context.Context, tenantToken, departmentID string) ([]DeptNode, error) {
	var out []DeptNode
	pageToken := ""
	for {
		u, err := url.Parse(f.deptChildrenEndpoint(departmentID))
		if err != nil {
			return nil, err
		}
		q := u.Query()
		q.Set("department_id_type", "open_department_id")
		q.Set("fetch_child", "false")
		q.Set("page_size", "50")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		u.RawQuery = q.Encode()

		var page struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				HasMore   bool   `json:"has_more"`
				PageToken string `json:"page_token"`
				Items     []struct {
					OpenDepartmentID string `json:"open_department_id"`
					Name             string `json:"name"`
				} `json:"items"`
			} `json:"data"`
		}
		if err := f.getJSON(ctx, tenantToken, u.String(), &page); err != nil {
			return nil, err
		}
		if page.Code != 0 {
			return nil, fmt.Errorf("飞书列子部门被拒（code=%d msg=%s）", page.Code, page.Msg)
		}
		for _, it := range page.Data.Items {
			if it.OpenDepartmentID == "" {
				continue
			}
			out = append(out, DeptNode{OpenDepartmentID: it.OpenDepartmentID, Name: it.Name})
		}
		if !page.Data.HasMore || page.Data.PageToken == "" {
			return out, nil
		}
		pageToken = page.Data.PageToken
	}
}

// ListUsersByDepartment 拉某部门**直接**下属用户（官方：GET contact/v3/users/find_by_department）。
// ★ 必须 tenant_access_token；department_id_type=open_department_id；分页 page_token/page_size。
func (f *FeishuClient) ListUsersByDepartment(ctx context.Context, tenantToken, departmentID string) ([]OrgUser, error) {
	var out []OrgUser
	pageToken := ""
	for {
		u, err := url.Parse(f.deptUsersEndpoint())
		if err != nil {
			return nil, err
		}
		q := u.Query()
		q.Set("department_id", departmentID)
		q.Set("department_id_type", "open_department_id")
		q.Set("user_id_type", "open_id")
		q.Set("page_size", "50")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		u.RawQuery = q.Encode()

		var page struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				HasMore   bool   `json:"has_more"`
				PageToken string `json:"page_token"`
				Items     []struct {
					OpenID string `json:"open_id"`
					Name   string `json:"name"`
				} `json:"items"`
			} `json:"data"`
		}
		if err := f.getJSON(ctx, tenantToken, u.String(), &page); err != nil {
			return nil, err
		}
		if page.Code != 0 {
			return nil, fmt.Errorf("飞书拉部门用户被拒（code=%d msg=%s）", page.Code, page.Msg)
		}
		for _, it := range page.Data.Items {
			if it.OpenID == "" {
				continue
			}
			out = append(out, OrgUser{OpenID: it.OpenID, Name: it.Name})
		}
		if !page.Data.HasMore || page.Data.PageToken == "" {
			return out, nil
		}
		pageToken = page.Data.PageToken
	}
}

// getJSON 带 Bearer 的 GET（组织同步共用）。
func (f *FeishuClient) getJSON(ctx context.Context, token, rawURL string, dst interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("飞书请求失败: %w", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(dst); err != nil {
		return fmt.Errorf("飞书响应解析失败: %w", err)
	}
	return nil
}
