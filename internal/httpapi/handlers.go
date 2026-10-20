package httpapi

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/chadhao/jx-lab-trace/internal/webui"
	"github.com/labstack/echo/v4"
)

// ---- 免权限 ----

func (s *Server) handleHealthz(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"version": s.Version,
		"dev":     s.Cfg.DevMode,
	})
}

// ---- 认证 ----

// handleDevLogin dev 模式免登桩（D4）：
// 用请求体 open_id（缺省 = JX_DEV_OPEN_ID）直接建会话，跳过飞书。
// ★ 未映射任何角色 ⇒ **拒绝**（deny by default，不是默认放行）。
func (s *Server) handleDevLogin(c echo.Context) error {
	if !s.Cfg.DevMode {
		return echo.NewHTTPError(http.StatusNotFound, "dev 登录未启用")
	}
	var body struct {
		OpenID string `json:"open_id"`
	}
	if err := c.Bind(&body); err != nil && !errors.Is(err, io.EOF) {
		// 空 body 允许（用 JX_DEV_OPEN_ID）；其余解析错误拒绝
		return echo.NewHTTPError(http.StatusBadRequest, "请求体不是合法 JSON")
	}
	openID := strings.TrimSpace(body.OpenID)
	if openID == "" {
		openID = s.Cfg.DevOpenID
	}
	if openID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "缺少 open_id")
	}
	expires, err := s.openSession(c, openID, openID, "dev_login")
	if errors.Is(err, ErrLoginDenied) {
		// ★ dev 桩的拒绝**保持 JSON 不变**（给开发/测试用，不是真实用户第一屏）。
		return echo.NewHTTPError(http.StatusForbidden, map[string]interface{}{
			"error":   "未映射任何角色，拒绝登录",
			"open_id": openID,
			"hint":    "deny by default：s_user_role 中无该 open_id 的绑定",
		})
	}
	if err != nil {
		return err
	}
	p := access.PrincipalFrom(c)
	if p == nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "会话主体缺失")
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"open_id":    p.OpenID,
		"name":       p.Name,
		"roles":      orEmpty(p.Roles),
		"sys_roles":  orEmpty(p.SysRoles),
		"expires_at": expires.Format(time.RFC3339),
	})
}

// handleFeishuStart 真实模式第一步：跳飞书授权。
func (s *Server) handleFeishuStart(c echo.Context) error {
	if s.Feishu == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"未配置飞书应用凭据（JX_FEISHU_APP_ID / JX_FEISHU_APP_SECRET）—— 本批使用 dev 桩登录")
	}
	redirect := s.callbackURI(c)
	state := c.QueryParam("state")
	return c.Redirect(http.StatusFound, s.Feishu.AuthorizeURL(redirect, state))
}

// handleFeishuCallback 真实模式第二步：code 换 open_id → 查角色 → 建会话。
// ★ 未映射角色 ⇒ 拒绝（与 dev 桩同一套 deny by default 判定）。
// ★★ D1（2026-10-12）：真实用户的**拒绝路径一律回极简 HTML 页**，不再回裸 JSON。
//
//	那是新用户可能看到的第一屏，必须独立成立（不依赖前端路由/JS）。
//	dev 桩（handleDevLogin）的拒绝**仍保持 JSON** —— 那是给开发/测试的。
func (s *Server) handleFeishuCallback(c echo.Context) error {
	if s.Feishu == nil {
		// 未配置凭据时真实用户也可能误入本回调 —— 同样给友好页（不再裸 JSON/503）。
		return s.renderLoginDeniedHTML(c, http.StatusServiceUnavailable,
			"登录服务暂未就绪",
			"系统尚未配置飞书登录应用。请联系管理员完成配置后再试。")
	}
	code := c.QueryParam("code")
	if code == "" {
		return s.renderLoginDeniedHTML(c, http.StatusBadRequest,
			"登录未完成",
			"飞书跳转时没有带登录凭证（code）。请回到首页，点「用飞书登录」重试。")
	}
	ident, err := s.Feishu.Exchange(c.Request().Context(), code, s.callbackURI(c))
	if err != nil {
		// ★ 不把内部错误详情甩给用户（err 里可能有上游 code/msg）——记服务端日志即可。
		c.Logger().Warn("飞书换 open_id 失败: ", err)
		return s.renderLoginDeniedHTML(c, http.StatusBadGateway,
			"飞书身份校验未通过",
			"暂时无法确认你的飞书身份。请稍后重试；若持续失败，请联系管理员。")
	}
	name := ident.Name
	if name == "" {
		name = ident.OpenID
	}
	if _, err := s.openSession(c, ident.OpenID, name, "feishu_login"); err != nil {
		if errors.Is(err, ErrLoginDenied) {
			return s.renderLoginDeniedHTML(c, http.StatusForbidden,
				"暂无法登录本系统",
				"你的账号还没有被分配系统角色。请联系管理员为你开通后再登录。")
		}
		return err
	}
	return c.Redirect(http.StatusFound, "/")
}

// handleFeishuExchange Applink 授权码直换会话（N-027 D1，APP 侧 JSON 契约）：
//
//	请求  {"code":"<飞书授权码>","state":"<APP 原样回传>","redirect_uri":"<可选>"}
//	成功  Set-Cookie: jx_sid（openSession 下发）+ {"ok":true}
//	失败  401 code 无效/过期 · 403 角色未映射（带 userMessage）· 502 飞书上游错误
//
// ★ 复用与 callback 完全相同的两条链：s.Feishu.Exchange（code→open_id）＋
//
//	openSession(how="feishu_login")（deny-by-default、首登 bootstrap、审计、TTL 12h）。
//
// ★ state：APP 侧 CSRF 防护（生成并内存保存）——后端不接受/不校验其内容，
//
//	作为不透明字符串忽略，响应不回显。
//
// ★ redirect_uri：飞书 code 换身份接口可能校验与 authorize 时一致（实测假 code 只回
//
//	code=20003 invalid_grant，一致性校验在 code 有效后才暴露 ⇒ 无法离线证伪）——
//	故入参带可选 redirect_uri（APP 回传其 applink authorize 所用 deep-link）；
//	缺省回落本服务的 callback URI（与现有 callback 同一缺省）。
//
// ★ PKCE code_verifier：飞书 v2 oauth/token 官方文档未列该参数（实测传入被静默忽略）
//
//	⇒ 待飞书能力，APP 侧 P1 暂缓（不阻塞上线）。
//
// ★ 限速：与现有 auth 端点同策略 —— 现状无专门限速，本端点不新造。
// ★★★ callback 与 JX_DEV_MODE 行为一字不改（兼容性红线）：本函数不触碰二者。
func (s *Server) handleFeishuExchange(c echo.Context) error {
	if s.Feishu == nil {
		// ★ map 形态绕过统一错误处理器的 5xx 打码（对外要给可行动的文案，但不泄内部细节）。
		return echo.NewHTTPError(http.StatusBadGateway, map[string]interface{}{
			"error": "登录服务暂未就绪（未配置飞书凭据）",
		})
	}
	var body struct {
		Code        string `json:"code"`
		State       string `json:"state"`
		RedirectURI string `json:"redirect_uri"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		return echo.NewHTTPError(http.StatusUnauthorized, "授权码无效或已过期")
	}
	redirect := strings.TrimSpace(body.RedirectURI)
	if redirect == "" {
		redirect = s.callbackURI(c)
	}
	ident, err := s.Feishu.Exchange(c.Request().Context(), code, redirect)
	if err != nil {
		// ★ 详情只进服务端日志（err 里可能有上游 code/msg），对外一律不外泄。
		c.Logger().Warn("飞书 exchange 换 open_id 失败: ", err)
		if errors.Is(err, access.ErrAuthCodeRejected) {
			return echo.NewHTTPError(http.StatusUnauthorized, "授权码无效或已过期")
		}
		return echo.NewHTTPError(http.StatusBadGateway, map[string]interface{}{
			"error": "飞书身份校验未通过",
		})
	}
	name := ident.Name
	if name == "" {
		name = ident.OpenID
	}
	if _, err := s.openSession(c, ident.OpenID, name, "feishu_login"); err != nil {
		if errors.Is(err, ErrLoginDenied) {
			// ★ ErrLoginDenied 覆盖「停用」与「无角色」两种；userMessage 分开给，
			//   避免停用账号被误导去「申请角色」。
			userMsg := "您的账号尚未分配角色，请联系综合运营部"
			if st, e := s.Store.UserStatus(c.Request().Context(), ident.OpenID); e == nil && st == "停用" {
				userMsg = "您的账号已停用，请联系综合运营部"
			}
			return echo.NewHTTPError(http.StatusForbidden, map[string]interface{}{
				"error":       "未映射任何角色或账号已停用，拒绝登录",
				"userMessage": userMsg,
			})
		}
		return err
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"ok": true})
}

// renderLoginDeniedHTML 输出**极简服务端渲染**的登录拒绝页（D1）。
// ★ 无 JS、无外部资源、无前端路由依赖 —— 独立成立。
func (s *Server) renderLoginDeniedHTML(c echo.Context, status int, title, reason string) error {
	html := `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + title + ` · 江熙新材实验检测数据追踪</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;
  margin:0;padding:2rem;background:#f6f7f9;color:#1f2937;line-height:1.6}
.card{max-width:28rem;margin:4rem auto;background:#fff;border:1px solid #e5e7eb;
  border-radius:8px;padding:1.75rem}
h1{font-size:1.25rem;margin:0 0 .75rem}
p{margin:.5rem 0}
.hint{color:#6b7280;font-size:.9rem}
a.btn{display:inline-block;margin-top:1.25rem;padding:.5rem 1rem;background:#2563eb;
  color:#fff;text-decoration:none;border-radius:6px}
</style>
</head>
<body>
<main class="card">
<h1>` + title + `</h1>
<p>` + reason + `</p>
<p class="hint">遇到问题？请联系管理员。</p>
<a class="btn" href="/">返回首页</a>
</main>
</body>
</html>`
	return c.HTML(status, html)
}

// handleLogout 登出：立即失效（写 revoked_at），且重启后仍失效。
func (s *Server) handleLogout(c echo.Context) error {
	p := access.PrincipalFrom(c)
	if p == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "未登录")
	}
	ck, err := c.Cookie(s.Cfg.CookieName)
	if err == nil && ck.Value != "" {
		if err := s.Store.RevokeSession(c.Request().Context(), ck.Value); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "登出失败")
		}
		if err := s.appendAudit(c, p, audit.Entry{
			Entity: "s_session", Action: "logout",
			Field: "session_id", NewValue: "revoked",
		}); err != nil {
			return err
		}
	}
	access.ClearSessionCookie(c, s.Opts)
	return c.JSON(http.StatusOK, map[string]string{"result": "ok"})
}

// ---- 登录主体 ----

func (s *Server) handleMe(c echo.Context) error {
	p := access.PrincipalFrom(c)
	if p == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "未登录")
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"open_id":   p.OpenID,
		"name":      p.Name,
		"roles":     orEmpty(p.Roles),
		"sys_roles": orEmpty(p.SysRoles),
	})
}

// ---- 受权限点保护的入口 ----

func (s *Server) handleListPermissionPoints(c echo.Context) error {
	pts, err := s.Store.ListPermissionPoints(c.Request().Context())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "查询权限点失败")
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count":  len(pts),
		"points": orEmptyPoints(pts),
	})
}

// handlePatchPermissionPoint 后台**只能启用/停用**权限点（后台不可增删 ⇒ 无新增/删除入口）。
// 每次改动全量写审计（docs/01 §8.0.2 权限变更审计）。
func (s *Server) handlePatchPermissionPoint(c echo.Context) error {
	p := access.PrincipalFrom(c)
	if p == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "未登录")
	}
	code := c.Param("code")
	pp, id, err := s.Store.PermissionPointByCode(c.Request().Context(), code)
	if err != nil {
		if errors.Is(err, store.ErrPointNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "权限点不存在（字典由代码注册，后台不能凭空新建）")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "查询权限点失败")
	}

	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "请求体不是合法 JSON")
	}
	body.Status = strings.TrimSpace(body.Status)
	if body.Status != "启用" && body.Status != "停用" {
		return echo.NewHTTPError(http.StatusBadRequest, "status 只允许 启用 / 停用")
	}
	if body.Status == pp.Status {
		return c.JSON(http.StatusOK, map[string]interface{}{
			"code": pp.Code, "status": pp.Status, "changed": false,
		})
	}

	old, err := s.Store.SetPermissionPointStatus(c.Request().Context(), id, body.Status)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "更新权限点状态失败")
	}
	if err := s.appendAudit(c, p, audit.Entry{
		Entity: "s_permission_point", EntityID: id, Action: "status",
		Field: "status", OldValue: old, NewValue: body.Status, Reason: strings.TrimSpace(body.Reason),
	}); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"code": pp.Code, "status": body.Status, "old_status": old, "changed": true,
	})
}

func (s *Server) handleListAudit(c echo.Context) error {
	rows, err := s.Store.ListAudit(c.Request().Context(), 100)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "查询审计日志失败")
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows),
		"rows":  orEmptyAudit(rows),
	})
}

// ---- 内部工具 ----

// ErrLoginDenied 是「未映射任何角色 / 账号已停用」的统一拒绝哨兵。
// ★ 由调用方决定呈现方式：dev 桩回裸 JSON（开发用）；飞书回调回友好 HTML 页（D1）。
var ErrLoginDenied = errors.New("未映射任何角色或账号已停用，拒绝登录")

// openSession 建会话的统一路径（dev 桩与飞书回调共用）：
// 查状态/角色 ⇒ 停用或无角色即拒绝 ⇒ 落库 ⇒ 下发 cookie ⇒ 装载主体 ⇒ 记审计。
// 返回会话过期时间；响应体由调用方决定（dev 登录回 JSON，飞书回调回跳转/HTML）。
func (s *Server) openSession(c echo.Context, openID, name, how string) (time.Time, error) {
	var zero time.Time
	ctx := c.Request().Context()
	// ★ D4 离职停用：status=停用 ⇒ 拒绝再登录（会话批量失效由事件处理负责）。
	if st, err := s.Store.UserStatus(ctx, openID); err != nil {
		return zero, echo.NewHTTPError(http.StatusInternalServerError, "查询用户状态失败")
	} else if st == "停用" {
		return zero, ErrLoginDenied
	}

	// ★ N-025 D1 首个飞书登录自动 sysadmin（bootstrap-on-first-login）。
	//   条件① how=feishu_login —— dev 桩不进此分支（防 ou_dev_admin 抢占）；
	//   条件③ JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID 未配置 —— 与启动引导互斥；
	//   条件②（s_user_role 中不存在 sysadmin 绑定）在 Store 事务内二次确认（幂等）。
	//   ★ 必须在下面的「无角色即拒绝」之前：清库后首个飞书用户正是靠这一步拿到角色。
	//   ★ Store 内部先 EnsureUser 落账号行、再绑角色（D1 口径「EnsureUser 之后」），
	//     故 m_user 空表时本人账号由本路径一并重建（N-025 D3）。
	bootstrapped := false
	if how == "feishu_login" && s.Cfg.BootstrapSysAdminOpenID == "" {
		bound, err := s.Store.BootstrapFirstSysAdmin(ctx, openID, name,
			store.MDActor{OpenID: openID, Name: name, IP: c.RealIP()})
		if err != nil {
			return zero, echo.NewHTTPError(http.StatusInternalServerError, "引导系统管理员失败")
		}
		bootstrapped = bound
	}

	roles, err := s.Store.RolesOf(ctx, openID)
	if err != nil {
		return zero, echo.NewHTTPError(http.StatusInternalServerError, "查询角色失败")
	}
	if len(roles) == 0 {
		// ★ UC-M0-01：未映射任何角色 ⇒ 拒绝（deny by default）
		return zero, ErrLoginDenied
	}
	if name == "" {
		name = openID
	}
	if err := s.Store.EnsureUser(ctx, openID, name); err != nil {
		return zero, echo.NewHTTPError(http.StatusInternalServerError, "建立用户失败")
	}

	sid, err := store.NewSessionID()
	if err != nil {
		return zero, echo.NewHTTPError(http.StatusInternalServerError, "生成会话失败")
	}
	now := time.Now()
	expires := now.Add(s.Opts.TTL)
	if err := s.Store.CreateSession(ctx, sid, openID, c.RealIP(), expires); err != nil {
		return zero, echo.NewHTTPError(http.StatusInternalServerError, "建立会话失败")
	}
	access.SetSessionCookie(c, sid, expires, s.Opts)

	p := &access.Principal{OpenID: openID, Name: s.Store.UserName(ctx, openID)}
	for _, r := range roles {
		if r.Kind == "system" {
			p.SysRoles = append(p.SysRoles, r.Code)
		} else {
			p.Roles = append(p.Roles, r.Code)
		}
	}
	access.SetPrincipal(c, p)

	// ★ 引导明示（D1）：登录响应带机器可读标记 ＋ 服务端日志点名「已引导为系统管理员」。
	//   飞书回调随后 302 回首页 ⇒ 头在重定向响应上（curl -i / 测试可断言）。
	if bootstrapped {
		c.Response().Header().Set("X-JX-Bootstrap-Sysadmin", "1")
		c.Logger().Infof("登录引导：%s（%s）已引导为系统管理员（action=bootstrap_sysadmin）", openID, name)
	}

	if err := s.appendAudit(c, p, audit.Entry{
		Entity: "s_session", Action: how, Field: "session_id", NewValue: "created",
	}); err != nil {
		return zero, err
	}
	return expires, nil
}

// appendAudit 补齐审计公共字段并落库；审计写失败 ⇒ 整笔操作失败（不让「写成功但没留痕」）。
func (s *Server) appendAudit(c echo.Context, p *access.Principal, e audit.Entry) error {
	e.ActorOpenID = p.OpenID
	e.ActorName = p.Name
	e.ActorRole = strings.Join(p.AllRoles(), ",")
	e.IP = c.RealIP()
	if err := s.Store.AppendAudit(c.Request().Context(), e); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "写审计失败，操作已中止")
	}
	return nil
}

func (s *Server) callbackURI(c echo.Context) string {
	scheme := "http"
	if c.Request().TLS != nil || s.Cfg.SecureCookie {
		scheme = "https"
	}
	return scheme + "://" + c.Request().Host + "/api/auth/feishu/callback"
}

// handleSPA 服务 go:embed 内嵌的前端构建产物（无路由命中 ⇒ 回落 index.html）。
func (s *Server) handleSPA(c echo.Context) error {
	path := strings.TrimPrefix(c.Request().URL.Path, "/")
	if path == "" {
		path = "index.html"
	}

	// ★★ 缓存策略（2026-10-10 修，这是一个**通用缺陷**）：
	//   动因：原实现**不设任何缓存头** —— 实测响应只有 `HTTP/1.1 200 OK`，没有
	//   Cache-Control / ETag / Last-Modified ⇒ 浏览器按**启发式规则**缓存 `index.html`
	//   ⇒ 它引用的仍是**旧的内容哈希 JS** ⇒ **发了新版本用户还在跑旧前端**
	//   （实测现场：后台已换新前端，用户浏览器仍报旧行为）。
	//   ⇒ 判据：**带内容哈希的产物可长缓存；入口 `index.html` 与 SPA 兜底必须 no-cache。**
	isHashedAsset := strings.HasPrefix(path, "assets/") && strings.Contains(path, "-")
	if isHashedAsset {
		c.Response().Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		c.Response().Header().Set("Cache-Control", "no-cache, must-revalidate")
	}

	data, err := fs.ReadFile(webui.Files, "dist/"+path)
	if err != nil {
		// SPA 兜底：任何未命中的路径都回 index.html（前端路由接管）。
		data, err = fs.ReadFile(webui.Files, "dist/index.html")
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "前端资源未构建（scripts/build.sh）")
		}
		// ★ 兜底返回的也是 index.html ⇒ 同样必须 no-cache（否则同上被启发式缓存）
		c.Response().Header().Set("Cache-Control", "no-cache, must-revalidate")
		// ★★ 复核修正（N-017 #4）：兜底内容是 index.html，必须按 HTML 报文吐出。
		//   原实现走 mimeOf(path) ⇒ 无扩展名路径给 application/octet-stream ⇒ 浏览器当下载。
		return c.Blob(http.StatusOK, "text/html; charset=utf-8", data)
	}
	return c.Blob(http.StatusOK, mimeOf(path), data)
}

func mimeOf(path string) string {
	switch {
	case strings.HasSuffix(path, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(path, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(path, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(path, ".json"):
		return "application/json; charset=utf-8"
	}
	return "application/octet-stream"
}

// jsonErrorHandler 统一 JSON 错误体：{"error": "..."}（5xx 不外泄内部细节）。
func jsonErrorHandler(e *echo.Echo) echo.HTTPErrorHandler {
	return func(err error, c echo.Context) {
		he, ok := err.(*echo.HTTPError)
		if !ok {
			he = echo.NewHTTPError(http.StatusInternalServerError, "内部错误")
		}
		code := he.Code
		if code >= 500 {
			e.Logger.Error(err)
		}
		if c.Response().Committed {
			return
		}
		switch msg := he.Message.(type) {
		case map[string]interface{}:
			_ = c.JSON(code, msg)
		default:
			text := fmt.Sprint(msg)
			if code >= 500 {
				text = "内部错误"
			}
			_ = c.JSON(code, map[string]string{"error": text})
		}
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEmptyPoints(p []store.PermissionPoint) []store.PermissionPoint {
	if p == nil {
		return []store.PermissionPoint{}
	}
	return p
}

func orEmptyAudit(a []store.AuditRow) []store.AuditRow {
	if a == nil {
		return []store.AuditRow{}
	}
	return a
}
