// Package httpapi：HTTP 路由与处理器。
//
// ★ 受保护入口一律在**本包的路由表**里集中声明（判据②③ 的「受保护入口」边界）：
//
//	形如 `access.RequirePerm(st, permission.SysPermEdit)` —— 参数必须是
//	internal/permission 的常量，禁止裸写字符串（Go 类型 + 门禁双保险）。
package httpapi

import (
	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/config"
	"github.com/chadhao/jx-lab-trace/internal/permission"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
)

// Server 装配路由。
type Server struct {
	Cfg     *config.Config
	Store   *store.Store
	Version string
	Feishu  *access.FeishuClient
	Opts    access.Opts
}

// New 构造 Server。
func New(cfg *config.Config, st *store.Store, version string) *Server {
	return &Server{
		Cfg:     cfg,
		Store:   st,
		Version: version,
		Feishu:  access.NewFeishuClient(cfg.FeishuAppID, cfg.FeishuAppSecret),
		Opts: access.Opts{
			TTL:        cfg.SessionTTL,
			CookieName: cfg.CookieName,
			Secure:     cfg.SecureCookie,
		},
	}
}

// Handler 装配完整路由表。
func (s *Server) Handler() *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = jsonErrorHandler(e)

	// ① 会话中间件：解析 cookie → 装载主体 → 滑动续期（服务端 + cookie 同批）
	e.Use(access.SessionMiddleware(s.Store, s.Opts))

	// —— 免权限入口 ——
	e.GET("/healthz", s.handleHealthz)

	// —— 认证 ——
	auth := e.Group("/api/auth")
	if s.Cfg.DevMode {
		// dev 免登桩（本批必有；真实飞书凭据到位后仍保留，便于本地联调）
		auth.POST("/dev-login", s.handleDevLogin)
	}
	auth.GET("/feishu/start", s.handleFeishuStart)
	auth.GET("/feishu/callback", s.handleFeishuCallback)
	auth.POST("/logout", s.handleLogout, access.RequireLogin())

	// —— 需要登录、但不额外要权限点 ——
	e.GET("/api/me", s.handleMe, access.RequireLogin())

	// —— 受权限点保护的入口（★ 集中声明；参数必须是权限点常量） ——
	admin := e.Group("/api/admin")
	admin.GET("/permission-points", s.handleListPermissionPoints,
		access.RequirePerm(s.Store, permission.SysPermEdit))
	admin.PATCH("/permission-points/:code", s.handlePatchPermissionPoint,
		access.RequirePerm(s.Store, permission.SysPermEdit))
	admin.GET("/audit-log", s.handleListAudit,
		access.RequirePerm(s.Store, permission.SysAuditView))

	// ★ 刻意**不提供**任何「新增 / 删除权限点」的入口（A8 / TC-M0-09）：
	//   权限点字典由代码注册，后台只能启用/停用（见上 PATCH）。

	// —— 前端（go:embed 内嵌的构建产物） ——
	e.GET("/*", s.handleSPA)
	return e
}
