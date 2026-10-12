package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/labstack/echo/v4"
)

// handleOrgSync D3.2：手动触发飞书组织同步。
//
// ★ 权限点选择（51 点冻结，不新增）：**sys.user.manage**。
//
//	理由：本接口的写目标是 m_user 账号行（建/更新账号），与现有
//	「用户与角色管理」（角色绑定入口 /api/admin/user-roles*）同一管理域；
//	sys.audit.view 只读、sys.perm.edit 管权限字典、sys.param.edit 管参数 —— 均不贴题。
//
// ★ 行为（docs/21 §4 / 任务包 D3.2）：递归拉部门 → 逐部门拉用户 →
//
//	upsert m_user（open_id 唯一键）。**只建/更新，不删除，不动 s_user_role，不自动绑角色**。
//	★ 表名说明：任务包行文写 s_user；机读口径 spec/schema.sql 的账号表是 **m_user**，
//	以机读件为准（N-002 判例）。不改 schema。
func (s *Server) handleOrgSync(c echo.Context) error {
	if s.Feishu == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"未配置飞书应用凭据（JX_FEISHU_APP_ID / JX_FEISHU_APP_SECRET），无法同步组织")
	}
	stats, err := s.syncOrgFromFeishu(c.Request().Context())
	if err != nil {
		c.Logger().Error("飞书组织同步失败: ", err)
		return echo.NewHTTPError(http.StatusBadGateway, "飞书组织同步失败："+err.Error())
	}

	// 审计：一次同步一笔汇总（谁点的、各多少）。
	p := access.PrincipalFrom(c)
	if p != nil {
		if aerr := s.appendAudit(c, p, audit.Entry{
			Entity: "m_user", Action: "org_sync", Field: "open_id",
			NewValue: "created=" + strconv.Itoa(stats.Created) +
				" updated=" + strconv.Itoa(stats.Updated) +
				" skipped=" + strconv.Itoa(stats.Skipped) +
				" users=" + strconv.Itoa(stats.TotalUsers) +
				" depts=" + strconv.Itoa(stats.TotalDepts),
			Reason: "手动飞书组织同步（只 upsert，不删除、不绑角色）",
		}); aerr != nil {
			return aerr
		}
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"result":        "ok",
		"created":       stats.Created,
		"updated":       stats.Updated,
		"skipped":       stats.Skipped,
		"total_users":   stats.TotalUsers,
		"total_depts":   stats.TotalDepts,
		"roles_touched": false,
		"deleted":       0,
	})
}

// orgSyncStats 是一次同步的统计。
type orgSyncStats struct {
	Created    int
	Updated    int
	Skipped    int
	TotalUsers int // 去重后的通讯录用户数（一人多部门只算一次）
	TotalDepts int
}

// syncOrgFromFeishu 拉全树并 upsert。
func (s *Server) syncOrgFromFeishu(ctx context.Context) (orgSyncStats, error) {
	var stats orgSyncStats

	token, err := s.Feishu.TenantAccessToken(ctx)
	if err != nil {
		return stats, err
	}

	// ① 递归收集全部部门 ID（根部门 0 → 子部门 → …）。
	deptIDs, err := s.collectDepartments(ctx, token, "0")
	if err != nil {
		return stats, err
	}
	stats.TotalDepts = len(deptIDs)

	// ② 逐部门拉「直接」用户；按 open_id 去重（一人多部门）。
	seen := map[string]struct{}{}
	type person struct{ openID, name string }
	var people []person
	for _, d := range deptIDs {
		users, err := s.Feishu.ListUsersByDepartment(ctx, token, d)
		if err != nil {
			return stats, err
		}
		for _, u := range users {
			if _, dup := seen[u.OpenID]; dup {
				continue
			}
			seen[u.OpenID] = struct{}{}
			people = append(people, person{openID: u.OpenID, name: u.Name})
		}
	}
	stats.TotalUsers = len(people)

	// ③ upsert（★ 只建/更新；停用状态不被同步复活 —— 见 store.UpsertUser）。
	for _, p := range people {
		r, err := s.Store.UpsertUser(ctx, p.openID, p.name, "org_sync")
		if err != nil {
			return stats, err
		}
		stats.Created += r.Created
		stats.Updated += r.Updated
		stats.Skipped += r.Skipped
	}
	return stats, nil
}

// collectDepartments 深度优先收集部门 ID（含根 0）。
func (s *Server) collectDepartments(ctx context.Context, token, root string) ([]string, error) {
	var out []string
	var walk func(id string) error
	walk = func(id string) error {
		out = append(out, id)
		kids, err := s.Feishu.ListChildDepartments(ctx, token, id)
		if err != nil {
			return err
		}
		for _, k := range kids {
			if err := walk(k.OpenDepartmentID); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return out, nil
}
