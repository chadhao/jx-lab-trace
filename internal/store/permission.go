package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/chadhao/jx-lab-trace/internal/permission"
)

// Role 是 s_role 的一行。
type Role struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Kind string `json:"kind"` // business / system
}

// RolesOf 返回账号绑定的全部角色（s_user_role × s_role，含已停用角色被过滤）。
// 无绑定 ⇒ 空切片 ⇒ 后续判定一律拒绝（deny by default）。
func (s *Store) RolesOf(ctx context.Context, openID string) ([]Role, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.code, r.name, r.kind
		  FROM s_user_role ur
		  JOIN s_role r ON r.code = ur.role_code
		 WHERE ur.open_id = ? AND r.status = '启用'
		 ORDER BY r.sort, r.code`, openID)
	if err != nil {
		return nil, fmt.Errorf("查询账号角色失败: %w", err)
	}
	defer rows.Close()

	var out []Role
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.Code, &r.Name, &r.Kind); err != nil {
			return nil, fmt.Errorf("读取角色失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LevelsFor 返回账号在**某权限点**上持有的级别并集：
// 多角色取并集；无记录 ⇒ 空集合 ⇒ 判 NONE（docs/01 §8.0/§8.3）。
func (s *Store) LevelsFor(ctx context.Context, openID string, point permission.Code) (permission.Levels, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rp.level
		  FROM s_role_permission rp
		  JOIN s_user_role ur ON ur.role_code = rp.role_code
		  JOIN s_role r ON r.code = ur.role_code AND r.status = '启用'
		 WHERE ur.open_id = ? AND rp.point_code = ?`, openID, point.String())
	if err != nil {
		return nil, fmt.Errorf("查询权限级别失败: %w", err)
	}
	defer rows.Close()

	var out permission.Levels
	for rows.Next() {
		var lv string
		if err := rows.Scan(&lv); err != nil {
			return nil, fmt.Errorf("读取权限级别失败: %w", err)
		}
		out = append(out, lv)
	}
	return permission.Union(out), rows.Err()
}

// PermissionPoint 是 s_permission_point 的一行（后台只读展示）。
type PermissionPoint struct {
	Code      string `json:"code"`
	Module    string `json:"module"`
	Name      string `json:"name"`
	Levels    string `json:"levels"`
	ScopeKind string `json:"scope_kind"`
	Sort      int    `json:"sort"`
	Status    string `json:"status"`
}

// ListPermissionPoints 按模块顺序列出全部权限点。
func (s *Store) ListPermissionPoints(ctx context.Context) ([]PermissionPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT code, module, name, levels, IFNULL(scope_kind, ''), sort, status
		  FROM s_permission_point ORDER BY sort, code`)
	if err != nil {
		return nil, fmt.Errorf("查询权限点失败: %w", err)
	}
	defer rows.Close()

	var out []PermissionPoint
	for rows.Next() {
		var p PermissionPoint
		if err := rows.Scan(&p.Code, &p.Module, &p.Name, &p.Levels, &p.ScopeKind, &p.Sort, &p.Status); err != nil {
			return nil, fmt.Errorf("读取权限点失败: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ErrPointNotFound 表示权限点不在字典中（字典外 code ⇒ 拒绝一切写操作）。
var ErrPointNotFound = fmt.Errorf("权限点不存在")

// PermissionPointByCode 返回单个权限点（id 供审计 entity_id 用）。
func (s *Store) PermissionPointByCode(ctx context.Context, code string) (PermissionPoint, int64, error) {
	var p PermissionPoint
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, code, module, name, levels, IFNULL(scope_kind, ''), sort, status
		  FROM s_permission_point WHERE code = ?`, code).
		Scan(&id, &p.Code, &p.Module, &p.Name, &p.Levels, &p.ScopeKind, &p.Sort, &p.Status)
	if err == sql.ErrNoRows {
		return PermissionPoint{}, 0, ErrPointNotFound
	}
	if err != nil {
		return PermissionPoint{}, 0, fmt.Errorf("查询权限点失败: %w", err)
	}
	return p, id, nil
}

// SetPermissionPointStatus 后台只能**启用 / 停用**权限点（不可增删 —— 无新增接口，
// 见 httpapi 的路由表；判据 A8 / TC-M0-09）。返回变更前状态。
func (s *Store) SetPermissionPointStatus(ctx context.Context, id int64, status string) (string, error) {
	var old string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM s_permission_point WHERE id = ?`, id).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPointNotFound
	}
	if err != nil {
		return "", fmt.Errorf("读取权限点状态失败: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE s_permission_point SET status = ? WHERE id = ?`, status, id); err != nil {
		return "", fmt.Errorf("更新权限点状态失败: %w", err)
	}
	return old, nil
}
