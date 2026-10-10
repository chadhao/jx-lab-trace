// Package access：会话中间件、身份主体（Principal）、权限守卫与 cookie 口径。
//
// ★ 会话三条硬约束（任务包 D5 / docs/04 UC-M0-02）：
//
//	① 会话**落库**（s_session），不用内存 map —— 重启进程原 cookie 仍有效；
//	② 每次访问滑动续期，且 **cookie Max-Age 与服务端 expires_at 同批续**；
//	③ 登出写 revoked_at ⇒ 立即失效，且重启后仍失效。
package access

import (
	"context"
	"net/http"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/permission"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
)

// Principal 是请求上下文中的登录主体。
type Principal struct {
	OpenID   string   `json:"open_id"`
	Name     string   `json:"name"`
	Roles    []string `json:"roles"`     // 业务角色
	SysRoles []string `json:"sys_roles"` // 系统角色
}

// AllRoles 返回业务角色 ∪ 系统角色（审计 actor_role 用）。
func (p Principal) AllRoles() []string {
	out := make([]string, 0, len(p.Roles)+len(p.SysRoles))
	out = append(out, p.Roles...)
	out = append(out, p.SysRoles...)
	return out
}

// Opts 是会话中间件的运行参数（由 config 装配）。
type Opts struct {
	TTL        time.Duration
	CookieName string
	Secure     bool
}

const ctxPrincipal = "jx.access.principal"

// PrincipalFrom 取当前请求的登录主体；未登录 ⇒ nil。
func PrincipalFrom(c echo.Context) *Principal {
	v := c.Get(ctxPrincipal)
	if v == nil {
		return nil
	}
	p, ok := v.(*Principal)
	if !ok {
		return nil
	}
	return p
}

// SetPrincipal 把登录主体写入请求上下文（建会话成功后由 handler 调用，
// 使同一次请求内后续逻辑可直接读取主体）。
func SetPrincipal(c echo.Context, p *Principal) {
	c.Set(ctxPrincipal, p)
}

// SetSessionCookie 下发/刷新会话 cookie。
// ★ HttpOnly + SameSite=Lax；★ Max-Age 与服务端 expires_at **同批**（同一次调用给出同一 TTL）。
func SetSessionCookie(c echo.Context, sessionID string, expiresAt time.Time, o Opts) {
	ck := &http.Cookie{
		Name:     o.CookieName,
		Value:    sessionID,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(o.TTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   o.Secure,
	}
	http.SetCookie(c.Response(), ck)
}

// ClearSessionCookie 立刻让浏览器侧会话失效（登出 / 会话已失效时）。
func ClearSessionCookie(c echo.Context, o Opts) {
	http.SetCookie(c.Response(), &http.Cookie{
		Name:     o.CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   o.Secure,
	})
}

// SessionMiddleware 解析会话 cookie ⇒ 装载主体 ⇒ **滑动续期（服务端 + cookie 同批）**。
// 没有 cookie 或会话无效 ⇒ 放行到后续 handler（由 RequirePerm / RequireLogin 决定 401/403）。
func SessionMiddleware(st *store.Store, o Opts) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			ck, err := c.Cookie(o.CookieName)
			if err != nil || ck == nil || ck.Value == "" {
				return next(c)
			}
			sid := ck.Value
			ctx := c.Request().Context()
			now := time.Now()

			sess, err := st.LoadSession(ctx, sid)
			if err != nil || !sess.Valid(now) {
				// 会话不存在 / 已过期 / 已登出：浏览器侧也一并清掉
				ClearSessionCookie(c, o)
				return next(c)
			}

			// 滑动续期：数据库 expires_at 与 cookie Max-Age 在同一次请求内一起推。
			newExp := now.Add(o.TTL)
			// ★★ 只把「会话确实无效」当未登录；**续期受阻（并发/锁）不得改变有效性判定**
			//   （2026-10-10 实测：并发请求里撞上写竞争的那个被误判未登录 ⇒ 随机 401）。
			//   且只在「剩余有效期不足一半」时才真续期 ⇒ 首屏并发几乎不写库、不竞争。
			renewBefore := now.Add(o.TTL / 2)
			ok, err := st.TouchSession(ctx, sid, now, newExp, renewBefore)
			if err != nil || !ok {
				ClearSessionCookie(c, o)
				return next(c)
			}
			SetSessionCookie(c, sid, newExp, o)

			p := buildPrincipal(ctx, st, sess.OpenID)
			c.Set(ctxPrincipal, p)
			return next(c)
		}
	}
}

// RequireLogin 要求已登录（未登录 401）。
func RequireLogin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if PrincipalFrom(c) == nil {
				return echo.NewHTTPError(http.StatusUnauthorized, "未登录或会话已失效")
			}
			return next(c)
		}
	}
}

// 授权级别字面量（与 spec/permission-points.json#levels 对齐）。
//
// ★ 为什么放在 access 而不是 permission 包：门禁 scripts/check_perm_registry.py
//
//	会把受保护入口守卫的实参里所有 `permission.XXX` 当成**权限点常量**去核验；
//	级别若也写成 permission.LevelAll，会被误判为「未登记的权限点」⇒ 假红。
//	级别不是权限点，故在这里声明为普通字符串。
const (
	LevelAll     = "ALL"     // 全（增 / 改 / 停）
	LevelRead    = "READ"    // 只读
	LevelInit    = "INIT"    // 仅发起
	LevelApprove = "APPROVE" // 仅审批
)

// RequirePerm 受权限点保护的入口守卫：
// 判定 = 角色 → 权限点 → 级别（多角色并集，无记录 ⇒ 拒绝）。
//
//   - 省略第三参 ⇒ 只要求「已授权」（任一非 NONE 级别）——批 1 的口径；
//   - 传入级别 ⇒ 必须**达到**该级别（ALL 覆盖一切）：
//     写入口用 access.LevelAll、读入口用 access.LevelRead，
//     这样「只读」角色才真的只读（docs/01 §8.0：READ = 只读）。
//
// ★ 参数是 permission.Code **常量**（具名类型），裸写字符串编译不过；
//
//	门禁判据②③ 另行静态复核（scripts/check_perm_registry.py）。
func RequirePerm(st *store.Store, code permission.Code, need ...string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			p := PrincipalFrom(c)
			if p == nil {
				return echo.NewHTTPError(http.StatusUnauthorized, "未登录或会话已失效")
			}
			levels, err := st.LevelsFor(c.Request().Context(), p.OpenID, code)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, "权限判定失败")
			}
			needLevel := ""
			if len(need) > 0 {
				needLevel = need[0]
			}
			allowed := levels.Granted()
			if needLevel != "" {
				allowed = levels.Allows(needLevel)
			}
			if !allowed {
				return echo.NewHTTPError(http.StatusForbidden, map[string]interface{}{
					"error": "无权访问该入口",
					"point": code.String(),
					"need":  needLevel,
					"hint":  "未映射角色、该角色在本权限点上为 NONE，或级别不足（deny by default）",
				})
			}
			return next(c)
		}
	}
}

func buildPrincipal(ctx context.Context, st *store.Store, openID string) *Principal {
	p := &Principal{OpenID: openID, Name: st.UserName(ctx, openID)}
	roles, err := st.RolesOf(ctx, openID)
	if err != nil {
		return p
	}
	for _, r := range roles {
		if r.Kind == "system" {
			p.SysRoles = append(p.SysRoles, r.Code)
		} else {
			p.Roles = append(p.Roles, r.Code)
		}
	}
	return p
}
