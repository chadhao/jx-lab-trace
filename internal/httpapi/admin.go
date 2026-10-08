package httpapi

import (
	"net/http"
	"strings"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
)

// ===== M2 权限配置后台接口 =====
//
//	GET    /api/admin/permission-matrix          整张矩阵（51 点 × 6 角色 + 受影响账号数 + locked）
//	PUT    /api/admin/permission-matrix          **原子**保存一批格子改动（含防锁死拒绝与 diff 审计）
//	GET    /api/admin/roles                      角色列表（内置标记 / 账号数 / 授权数）
//	POST   /api/admin/roles                      新增角色（可从某角色复制权限）
//	PATCH  /api/admin/roles/:code                重命名 / 启用停用
//	DELETE /api/admin/roles/:code                删除（★ 内置角色一律拒绝）
//	GET    /api/admin/user-roles                 全部用户绑定
//	GET    /api/admin/user-roles/:open_id        单账号绑定的角色
//	POST   /api/admin/user-roles                 绑定（可叠加；撞车 ⇒ 409 已被占用）
//	DELETE /api/admin/user-roles/:open_id/:role  解绑（★ 最后一个管理员 ⇒ 拒绝）
//
// ★ 权限点字典只读：**没有任何**新增/删除权限点的入口
//	（POST /api/admin/permission-points ⇒ 405，批 1 的 A8 口径本批继续成立）。

func mdActor(c echo.Context) store.MDActor {
	p := access.PrincipalFrom(c)
	if p == nil {
		return store.MDActor{}
	}
	return store.MDActor{
		OpenID: p.OpenID,
		Name:   p.Name,
		Role:   strings.Join(p.AllRoles(), ","),
		IP:     c.RealIP(),
	}
}

func (s *Server) handleGetMatrix(c echo.Context) error {
	m, err := s.Store.GetPermissionMatrix(c.Request().Context())
	if err != nil {
		return mdErr(err)
	}
	return c.JSON(http.StatusOK, m)
}

// handleSaveMatrix 保存矩阵。
// ★★ 原子性：store 层「先整体校验、后落库」⇒ 中途任何一条不合法 ⇒ **一笔都不写**
// （TC-M2-02：取消保存 / 半批非法 ⇒ 无部分生效）。
func (s *Server) handleSaveMatrix(c echo.Context) error {
	var body struct {
		Changes []store.MatrixChange `json:"changes"`
		Reason  string               `json:"reason"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	diffs, err := s.Store.SavePermissionMatrix(c.Request().Context(), body.Changes,
		body.Reason, mdActor(c))
	if err != nil {
		if store.IsDeadlockGuard(err) {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return mdErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"changed": len(diffs),
		"diffs":   orEmptyDiffs(diffs),
		"note":    "保存后立即生效（判定直读 s_role_permission，无缓存）",
	})
}

func (s *Server) handleListRoles(c echo.Context) error {
	roles, err := s.Store.ListRoles(c.Request().Context())
	if err != nil {
		return mdErr(err)
	}
	if roles == nil {
		roles = []store.RoleInfo{}
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(roles), "roles": roles})
}

func (s *Server) handleCreateRole(c echo.Context) error {
	var body struct {
		Code     string `json:"code"`
		Name     string `json:"name"`
		CopyFrom string `json:"copy_from"`
		Reason   string `json:"reason"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	if err := s.Store.CreateRole(c.Request().Context(), body.Code, body.Name,
		body.CopyFrom, mdActor(c)); err != nil {
		return mdErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"created": true, "code": body.Code, "copy_from": body.CopyFrom,
	})
}

func (s *Server) handlePatchRole(c echo.Context) error {
	code := c.Param("code")
	var body struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	if strings.TrimSpace(body.Name) != "" {
		if err := s.Store.RenameRole(c.Request().Context(), code, body.Name, mdActor(c)); err != nil {
			return mdErr(err)
		}
	}
	if strings.TrimSpace(body.Status) != "" {
		if err := s.Store.SetRoleStatus(c.Request().Context(), code, body.Status, mdActor(c)); err != nil {
			return mdErr(err)
		}
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"code": code, "name": body.Name, "status": body.Status, "changed": true,
	})
}

func (s *Server) handleDeleteRole(c echo.Context) error {
	code := c.Param("code")
	if err := s.Store.DeleteRole(c.Request().Context(), code, mdActor(c)); err != nil {
		return mdErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"deleted": true, "code": code})
}

func (s *Server) handleListUserRoles(c echo.Context) error {
	all, err := s.Store.ListUserBindings(c.Request().Context())
	if err != nil {
		return mdErr(err)
	}
	if all == nil {
		all = []store.UserBinding{}
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(all), "bindings": all})
}

func (s *Server) handleGetUserRoles(c echo.Context) error {
	roles, err := s.Store.ListUserRoles(c.Request().Context(), c.Param("open_id"))
	if err != nil {
		return mdErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"open_id": c.Param("open_id"), "roles": roles,
	})
}

func (s *Server) handleBindUserRole(c echo.Context) error {
	var body struct {
		OpenID string `json:"open_id"`
		Role   string `json:"role_code"`
		Reason string `json:"reason"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	if err := s.Store.BindUserRole(c.Request().Context(), body.OpenID, body.Role, mdActor(c)); err != nil {
		return mdErr(err)
	}
	roles, _ := s.Store.ListUserRoles(c.Request().Context(), body.OpenID)
	return c.JSON(http.StatusOK, map[string]interface{}{
		"bound": true, "open_id": body.OpenID, "role_code": body.Role, "roles": roles,
	})
}

func (s *Server) handleUnbindUserRole(c echo.Context) error {
	if err := s.Store.UnbindUserRole(c.Request().Context(),
		c.Param("open_id"), c.Param("role_code"), mdActor(c)); err != nil {
		return mdErr(err)
	}
	roles, _ := s.Store.ListUserRoles(c.Request().Context(), c.Param("open_id"))
	return c.JSON(http.StatusOK, map[string]interface{}{
		"unbound": true, "open_id": c.Param("open_id"), "role_code": c.Param("role_code"), "roles": roles,
	})
}

func orEmptyDiffs(d []store.MatrixDiff) []store.MatrixDiff {
	if d == nil {
		return []store.MatrixDiff{}
	}
	return d
}
