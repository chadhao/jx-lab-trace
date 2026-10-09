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
		access.RequirePerm(s.Store, permission.SysPermEdit, access.LevelRead))
	admin.PATCH("/permission-points/:code", s.handlePatchPermissionPoint,
		access.RequirePerm(s.Store, permission.SysPermEdit, access.LevelAll))
	admin.GET("/audit-log", s.handleListAudit,
		access.RequirePerm(s.Store, permission.SysAuditView, access.LevelRead))

	// ★ 批 2 · 权限矩阵（D4）：读也要权限点 ——「权限页自身也要有权限」。
	//	写用 LevelAll ⇒ 只读（READ）角色连改都改不了。
	admin.GET("/permission-matrix", s.handleGetMatrix,
		access.RequirePerm(s.Store, permission.SysPermEdit, access.LevelRead))
	admin.PUT("/permission-matrix", s.handleSaveMatrix,
		access.RequirePerm(s.Store, permission.SysPermEdit, access.LevelAll))

	// ★ 批 2 · 角色管理 + 用户绑定（D5，sys.user.manage）
	admin.GET("/roles", s.handleListRoles,
		access.RequirePerm(s.Store, permission.SysUserManage, access.LevelRead))
	admin.POST("/roles", s.handleCreateRole,
		access.RequirePerm(s.Store, permission.SysUserManage, access.LevelAll))
	admin.PATCH("/roles/:code", s.handlePatchRole,
		access.RequirePerm(s.Store, permission.SysUserManage, access.LevelAll))
	admin.DELETE("/roles/:code", s.handleDeleteRole,
		access.RequirePerm(s.Store, permission.SysUserManage, access.LevelAll))
	admin.GET("/user-roles", s.handleListUserRoles,
		access.RequirePerm(s.Store, permission.SysUserManage, access.LevelRead))
	admin.GET("/user-roles/:open_id", s.handleGetUserRoles,
		access.RequirePerm(s.Store, permission.SysUserManage, access.LevelRead))
	admin.POST("/user-roles", s.handleBindUserRole,
		access.RequirePerm(s.Store, permission.SysUserManage, access.LevelAll))
	admin.DELETE("/user-roles/:open_id/:role_code", s.handleUnbindUserRole,
		access.RequirePerm(s.Store, permission.SysUserManage, access.LevelAll))

	// ★ 刻意**不提供**任何「新增 / 删除权限点」的入口（A8 / TC-M0-09）：
	//   权限点字典由代码注册，后台只能启用/停用（见上 PATCH）。

	// —— 批 2 · M1 主数据（8 类，每类 6 条路由） ——
	s.mountMasterData(e)

	// —— 批 3 · M3 收货与打码（★ 8 个 recv.* 权限点在此消费） ——
	s.mountReceiving(e)

	// —— 批 4 · M4 取样与留样（★ 5 个 sample.* 权限点在此消费） ——
	s.mountSampling(e)

	// —— 批 5 · M5 检测（★ 11 个 insp.* 权限点在此消费） ——
	s.mountInsp(e)

	// —— 批 6 · M6 生产与谱系（★ 6 个 prod.* 权限点在此消费） ——
	s.mountProd(e)

	// —— 批 7 · M7 出货（★ 4 个 ship.* 权限点在此消费） ——
	s.mountShip(e)

	// —— 批 7 · M8 追溯（★ 3 个 trace.* 权限点在此消费） ——
	s.mountTrace(e)

	// —— 前端（go:embed 内嵌的构建产物） ——
	e.GET("/*", s.handleSPA)
	return e
}

// mdGuard 是一类主数据的读/写守卫（读 = READ 级，写 = ALL 级）。
type mdGuard struct {
	read  echo.MiddlewareFunc
	write echo.MiddlewareFunc
}

// mdGuards 集中声明主数据的受保护入口守卫。
//
// ★★ 这里必须**逐条写出 permission.XXX 常量**：scripts/check_perm_registry.py
//
//	判据③ 要求 RequirePerm 的实参能静态核验成已登记的权限点常量，
//	传变量（如 g.Perm）会被判「无法静态核验」⇒ 假红。宁可写长，不可写活。
func (s *Server) mdGuards() map[string]mdGuard {
	return map[string]mdGuard{
		"customers": {
			read:  access.RequirePerm(s.Store, permission.MdCustomer, access.LevelRead),
			write: access.RequirePerm(s.Store, permission.MdCustomer, access.LevelAll),
		},
		"compositions": {
			read:  access.RequirePerm(s.Store, permission.MdMaterial, access.LevelRead),
			write: access.RequirePerm(s.Store, permission.MdMaterial, access.LevelAll),
		},
		"material-types": {
			read:  access.RequirePerm(s.Store, permission.MdMaterial, access.LevelRead),
			write: access.RequirePerm(s.Store, permission.MdMaterial, access.LevelAll),
		},
		"materials": {
			read:  access.RequirePerm(s.Store, permission.MdMaterial, access.LevelRead),
			write: access.RequirePerm(s.Store, permission.MdMaterial, access.LevelAll),
		},
		"test-items": {
			read:  access.RequirePerm(s.Store, permission.MdTestItem, access.LevelRead),
			write: access.RequirePerm(s.Store, permission.MdTestItem, access.LevelAll),
		},
		"test-item-limits": {
			read:  access.RequirePerm(s.Store, permission.MdTestItem, access.LevelRead),
			write: access.RequirePerm(s.Store, permission.MdTestItem, access.LevelAll),
		},
		"vehicles": {
			read:  access.RequirePerm(s.Store, permission.MdVehicle, access.LevelRead),
			write: access.RequirePerm(s.Store, permission.MdVehicle, access.LevelAll),
		},
		"teams": {
			read:  access.RequirePerm(s.Store, permission.MdTeam, access.LevelRead),
			write: access.RequirePerm(s.Store, permission.MdTeam, access.LevelAll),
		},
	}
}

// mountMasterData 装配 /api/md/{entity} 的 6 条路由（× 8 类）。
func (s *Server) mountMasterData(e *echo.Echo) {
	guards := s.mdGuards()
	g := e.Group("/api/md")
	for _, entity := range []string{
		"customers", "compositions", "material-types", "materials",
		"test-items", "test-item-limits", "vehicles", "teams",
	} {
		gd, ok := guards[entity]
		if !ok {
			continue // 守卫缺席 ⇒ 该实体不挂路由（宁可 404，不可无守卫）
		}
		g.GET("/"+entity, s.handleMDList(entity), gd.read)
		g.POST("/"+entity, s.handleMDCreate(entity), gd.write)
		g.GET("/"+entity+"/:id", s.handleMDGet(entity), gd.read)
		g.GET("/"+entity+"/:id/history", s.handleMDHistory(entity), gd.read)
		g.PUT("/"+entity+"/:id", s.handleMDUpdate(entity), gd.write)
		g.PATCH("/"+entity+"/:id/status", s.handleMDStatus(entity), gd.write)
	}
	// ★ 判定限解析入口（D3 / A4）：独立路径，避免与 /test-item-limits/:id 撞路由。
	if gd, ok := guards["test-item-limits"]; ok {
		e.GET("/api/limits/resolve", s.handleResolveLimit, gd.read)
	}
}
