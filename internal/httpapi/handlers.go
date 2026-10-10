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
func (s *Server) handleFeishuCallback(c echo.Context) error {
	if s.Feishu == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "未配置飞书应用凭据")
	}
	code := c.QueryParam("code")
	if code == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "缺少 code")
	}
	ident, err := s.Feishu.Exchange(c.Request().Context(), code, s.callbackURI(c))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "飞书换 open_id 失败："+err.Error())
	}
	name := ident.Name
	if name == "" {
		name = ident.OpenID
	}
	if _, err := s.openSession(c, ident.OpenID, name, "feishu_login"); err != nil {
		return err
	}
	return c.Redirect(http.StatusFound, "/")
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

// openSession 建会话的统一路径（dev 桩与飞书回调共用）：
// 查角色 ⇒ 无角色即拒绝 ⇒ 落库 ⇒ 下发 cookie ⇒ 装载主体 ⇒ 记审计。
// 返回会话过期时间；响应体由调用方决定（dev 登录回 JSON，飞书回调回跳转）。
func (s *Server) openSession(c echo.Context, openID, name, how string) (time.Time, error) {
	var zero time.Time
	ctx := c.Request().Context()
	roles, err := s.Store.RolesOf(ctx, openID)
	if err != nil {
		return zero, echo.NewHTTPError(http.StatusInternalServerError, "查询角色失败")
	}
	if len(roles) == 0 {
		// ★ UC-M0-01：未映射任何角色 ⇒ 拒绝（deny by default）
		return zero, echo.NewHTTPError(http.StatusForbidden, map[string]interface{}{
			"error":   "未映射任何角色，拒绝登录",
			"open_id": openID,
			"hint":    "deny by default：s_user_role 中无该 open_id 的绑定",
		})
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
