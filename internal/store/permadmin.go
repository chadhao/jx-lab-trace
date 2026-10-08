package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/chadhao/jx-lab-trace/internal/permission"
)

// ===== 权限配置后台（M2 / D4·D5·D6·D7）=====
//
// ★ 防锁死三条一起做（任务包 D6）：
//	① 界面上 sysadmin 的管理域格子置灰（前端按 Locked 字段渲染）；
//	② ★★ **后端同步拒绝**（applyMatrixChanges 里 ErrDeadlockGuard）——
//	   绕过前端直接调接口改 sysadmin 的管理域权限一律 409（TC-M2-06）；
//	③ 保持至少一个管理员账号（解绑最后一个 / 停用 sysadmin 角色一律拒绝），
//	   另有 JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID 环境变量引导兜底（迁移 ⑤）。
// ★ 系统管理员**不自动拥有任何业务权限**（最小权限）—— 本层不做任何“顺手开业务点”的事。

// 防锁死与角色管理的哨兵错误。
var (
	ErrDeadlockGuard = errors.New("防锁死：系统管理员角色的管理域权限不可修改或剥夺")
	ErrBuiltinRole   = errors.New("内置角色不可删除")
	ErrRoleNotFound  = errors.New("角色不存在")
	ErrBadLevel      = errors.New("授权级别不被该权限点支持")
	ErrOccupied      = errors.New("该绑定已被占用")
	ErrLastAdmin     = errors.New("必须至少保留一个启用状态的系统管理员账号")
	ErrRoleCodeBad   = errors.New("角色 code 只能由小写字母/数字/下划线组成，且以字母开头")
	ErrRoleNameEmpty = errors.New("角色名称不能为空")
	ErrMatrixEmpty   = errors.New("没有需要保存的改动")
)

var roleCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// IsDeadlockGuard 判定「因防锁死被拒」（httpapi 映射 403 用）。
func IsDeadlockGuard(err error) bool { return errors.Is(err, ErrDeadlockGuard) }

// SysAdminRole 是内置系统管理员角色 code（spec/permission-points.json#roles）。
const SysAdminRole = "sysadmin"

// AdminDomainModule 是权限点「管理域」的模块名（spec#permission-points.module）。
const AdminDomainModule = "系统管理"

// MatrixPoint 是矩阵的一行（一个权限点）。
type MatrixPoint struct {
	Code   string `json:"code"`
	Module string `json:"module"`
	Name   string `json:"name"`
	Levels string `json:"levels"` // 该点支持的级别集合，如 ALL,READ,NONE
	Status string `json:"status"`
	Sort   int    `json:"sort"`
}

// MatrixRole 是矩阵的一列（一个角色）。
type MatrixRole struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	IsSystem bool   `json:"is_system"`
	Status   string `json:"status"`
	Accounts int    `json:"accounts"` // 受影响账号数（保存前提示用）
}

// MatrixCell 是矩阵的一个格子。
type MatrixCell struct {
	RoleCode  string `json:"role_code"`
	PointCode string `json:"point_code"`
	Level     string `json:"level"`
	Locked    bool   `json:"locked"` // ★ sysadmin × 管理域 ⇒ 前端置灰、后端拒绝
}

// Matrix 是整张权限矩阵。
type Matrix struct {
	Points []MatrixPoint `json:"points"`
	Roles  []MatrixRole  `json:"roles"`
	Cells  []MatrixCell  `json:"cells"`
}

// MatrixChange 是一次格子改动。
type MatrixChange struct {
	RoleCode  string `json:"role_code"`
	PointCode string `json:"point_code"`
	Level     string `json:"level"`
}

// MatrixDiff 是一条已生效的 diff（审计与响应用）。
type MatrixDiff struct {
	RoleCode  string `json:"role_code"`
	PointCode string `json:"point_code"`
	OldLevel  string `json:"old_level"`
	NewLevel  string `json:"new_level"`
}

// IsLockedCell 判定「不可编辑的格子」：★ 系统管理员 × 管理域（D6 / TC-M2-06）。
func IsLockedCell(roleCode, pointModule string) bool {
	return roleCode == SysAdminRole && pointModule == AdminDomainModule
}

// GetPermissionMatrix 返回整张矩阵（行=51 权限点，列=6 角色，格=级别）。
func (s *Store) GetPermissionMatrix(ctx context.Context) (*Matrix, error) {
	m := &Matrix{Points: []MatrixPoint{}, Roles: []MatrixRole{}, Cells: []MatrixCell{}}

	pts, err := s.ListPermissionPoints(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range pts {
		m.Points = append(m.Points, MatrixPoint{
			Code: p.Code, Module: p.Module, Name: p.Name,
			Levels: p.Levels, Status: p.Status, Sort: p.Sort,
		})
	}

	roles, err := s.listRolesWithAccounts(ctx)
	if err != nil {
		return nil, err
	}
	m.Roles = roles

	// 格子：库里没有的组合 ⇒ 默认 NONE（deny by default 的可见化）
	type cellKey struct{ role, point string }
	current := map[cellKey]string{}
	rows, err := s.db.QueryContext(ctx, `SELECT role_code, point_code, level FROM s_role_permission`)
	if err != nil {
		return nil, fmt.Errorf("查询授权矩阵失败: %w", err)
	}
	for rows.Next() {
		var c cellKey
		var lv string
		if err := rows.Scan(&c.role, &c.point, &lv); err != nil {
			rows.Close()
			return nil, err
		}
		current[c] = lv
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	pointModule := map[string]string{}
	for _, p := range m.Points {
		pointModule[p.Code] = p.Module
	}
	for _, r := range m.Roles {
		for _, p := range m.Points {
			lv, ok := current[cellKey{r.Code, p.Code}]
			if !ok {
				lv = permission.LevelNone
			}
			m.Cells = append(m.Cells, MatrixCell{
				RoleCode: r.Code, PointCode: p.Code, Level: lv,
				Locked: IsLockedCell(r.Code, p.Module),
			})
		}
	}
	return m, nil
}

// SavePermissionMatrix **原子**保存一批格子改动：
//   - 先**整体校验**（含防锁死）再落库 ⇒ 校验不过 ⇒ 一笔都不写（TC-M2-02 无部分生效）；
//   - 每条实际变化写一条审计 diff：哪个角色 × 哪个权限点 × 从哪级 → 哪级（TC-M2-07）；
//   - 保存即生效：判定读的是 s_role_permission，**无缓存**（TC-M2-01 立刻生效）。
func (s *Store) SavePermissionMatrix(ctx context.Context, changes []MatrixChange, reason string, actor MDActor) ([]MatrixDiff, error) {
	if len(changes) == 0 {
		return nil, ErrMatrixEmpty
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// ---- ① 取字典（角色 / 权限点）----
	roles := map[string]MatrixRole{}
	{
		rows, err := tx.QueryContext(ctx,
			`SELECT code, name, kind, is_system, status FROM s_role`)
		if err != nil {
			return nil, fmt.Errorf("查询角色失败: %w", err)
		}
		for rows.Next() {
			var r MatrixRole
			var sys int
			if err := rows.Scan(&r.Code, &r.Name, &r.Kind, &sys, &r.Status); err != nil {
				rows.Close()
				return nil, err
			}
			r.IsSystem = sys == 1
			roles[r.Code] = r
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	points := map[string]MatrixPoint{}
	{
		rows, err := tx.QueryContext(ctx,
			`SELECT code, module, name, levels, status, sort FROM s_permission_point`)
		if err != nil {
			return nil, fmt.Errorf("查询权限点失败: %w", err)
		}
		for rows.Next() {
			var p MatrixPoint
			if err := rows.Scan(&p.Code, &p.Module, &p.Name, &p.Levels, &p.Status, &p.Sort); err != nil {
				rows.Close()
				return nil, err
			}
			points[p.Code] = p
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	// ---- ② 先读旧值（同一事务内），再整体校验 ----
	type key struct{ role, point string }
	oldLevels := map[key]string{}
	for _, c := range changes {
		k := key{c.RoleCode, c.PointCode}
		if _, dup := oldLevels[k]; dup {
			continue // 同格多次提交：以最后一次为准（下面统一处理）
		}
		var lv string
		err := tx.QueryRowContext(ctx,
			`SELECT level FROM s_role_permission WHERE role_code = ? AND point_code = ?`,
			c.RoleCode, c.PointCode).Scan(&lv)
		if errors.Is(err, sql.ErrNoRows) {
			lv = permission.LevelNone
		} else if err != nil {
			return nil, fmt.Errorf("读取现状失败: %w", err)
		}
		oldLevels[k] = lv
	}
	// 同格只保留最后一次
	final := map[key]MatrixChange{}
	order := []key{}
	for _, c := range changes {
		k := key{c.RoleCode, c.PointCode}
		if _, seen := final[k]; !seen {
			order = append(order, k)
		}
		final[k] = c
	}

	var diffs []MatrixDiff
	for _, k := range order {
		c := final[k]
		role, ok := roles[c.RoleCode]
		if !ok {
			return nil, fmt.Errorf("%w：%s", ErrRoleNotFound, c.RoleCode)
		}
		if role.Status != "启用" {
			return nil, fmt.Errorf("%w：%s 已停用", ErrRoleNotFound, c.RoleCode)
		}
		pt, ok := points[c.PointCode]
		if !ok {
			return nil, fmt.Errorf("%w：%s", ErrPointNotFound, c.PointCode)
		}
		lv := strings.TrimSpace(c.Level)
		if !levelAllowed(pt.Levels, lv) {
			return nil, fmt.Errorf("%w：%s 支持 %s，收到 %q", ErrBadLevel, c.PointCode, pt.Levels, lv)
		}
		old := oldLevels[k]
		if lv == old {
			continue
		}
		// ★★ 防锁死②：后端同步拒绝（绕过前端也过不去，TC-M2-06）
		if IsLockedCell(c.RoleCode, pt.Module) {
			return nil, fmt.Errorf("%w：%s × %s", ErrDeadlockGuard, c.RoleCode, c.PointCode)
		}
		diffs = append(diffs, MatrixDiff{
			RoleCode: c.RoleCode, PointCode: c.PointCode,
			OldLevel: old, NewLevel: lv,
		})
	}
	if len(diffs) == 0 {
		return []MatrixDiff{}, nil
	}

	// ---- ③ 校验全部通过 ⇒ 落库 + 审计（同事务）----
	for _, d := range diffs {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO s_role_permission (role_code, point_code, level, updated_by, created_by)
			VALUES (?, ?, ?, ?, 'matrix')
			ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id), level = VALUES(level), updated_by = VALUES(updated_by)`,
			d.RoleCode, d.PointCode, d.NewLevel, actor.OpenID)
		if err != nil {
			return nil, fmt.Errorf("保存 %s×%s 失败: %w", d.RoleCode, d.PointCode, err)
		}
		rpID, _ := res.LastInsertId()
		if err := appendAuditTx(ctx, tx, audit.Entry{
			Entity: "s_role_permission", EntityID: rpID, Action: "perm_change",
			Field:       d.RoleCode + " × " + d.PointCode,
			OldValue:    d.OldLevel,
			NewValue:    d.NewLevel,
			ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
			Reason: strings.TrimSpace(reason),
		}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	return diffs, nil
}

func levelAllowed(levelsCSV, level string) bool {
	if level == "" {
		return false
	}
	for _, l := range strings.Split(levelsCSV, ",") {
		if strings.TrimSpace(l) == level {
			return true
		}
	}
	return false
}

// ---- 角色管理（D5）----

// RoleInfo 是角色管理页的一行。
type RoleInfo struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	IsSystem bool   `json:"is_system"`
	Status   string `json:"status"`
	Accounts int    `json:"accounts"`
	Grants   int    `json:"grants"`
}

func (s *Store) listRolesWithAccounts(ctx context.Context) ([]MatrixRole, error) {
	accounts, err := s.roleAccountCounts(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT code, name, kind, is_system, status FROM s_role ORDER BY sort, code`)
	if err != nil {
		return nil, fmt.Errorf("查询角色失败: %w", err)
	}
	defer rows.Close()
	out := []MatrixRole{}
	for rows.Next() {
		var r MatrixRole
		var sys int
		if err := rows.Scan(&r.Code, &r.Name, &r.Kind, &sys, &r.Status); err != nil {
			return nil, err
		}
		r.IsSystem = sys == 1
		r.Accounts = accounts[r.Code]
		out = append(out, r)
	}
	return out, rows.Err()
}

// roleAccountCounts 统计每个角色下**启用状态**的账号数（保存前提示「受影响账号数」）。
func (s *Store) roleAccountCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ur.role_code, COUNT(DISTINCT ur.open_id)
		  FROM s_user_role ur
		  JOIN m_user u ON u.open_id = ur.open_id AND u.status = '启用'
		 GROUP BY ur.role_code`)
	if err != nil {
		return nil, fmt.Errorf("统计角色账号数失败: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var code string
		var n int
		if err := rows.Scan(&code, &n); err != nil {
			return nil, err
		}
		out[code] = n
	}
	return out, rows.Err()
}

// ListRoles 列出角色（含账号数与授权数）。
func (s *Store) ListRoles(ctx context.Context) ([]RoleInfo, error) {
	accounts, err := s.roleAccountCounts(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.code, r.name, r.kind, r.is_system, r.status,
		       (SELECT COUNT(*) FROM s_role_permission rp WHERE rp.role_code = r.code)
		  FROM s_role r ORDER BY r.sort, r.code`)
	if err != nil {
		return nil, fmt.Errorf("查询角色失败: %w", err)
	}
	defer rows.Close()
	out := []RoleInfo{}
	for rows.Next() {
		var r RoleInfo
		var sys int
		if err := rows.Scan(&r.Code, &r.Name, &r.Kind, &sys, &r.Status, &r.Grants); err != nil {
			return nil, err
		}
		r.IsSystem = sys == 1
		r.Accounts = accounts[r.Code]
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateRole 新增角色；copyFrom 非空 ⇒ **从该角色复制权限**（复制后各自独立，TC-M2-04）。
func (s *Store) CreateRole(ctx context.Context, code, name, copyFrom string, actor MDActor) error {
	code = strings.TrimSpace(code)
	name = strings.TrimSpace(name)
	if !roleCodeRe.MatchString(code) {
		return ErrRoleCodeBad
	}
	if name == "" {
		return ErrRoleNameEmpty
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO s_role (code, name, kind, is_system, sort, status, created_by)
		VALUES (?, ?, 'business', 0, 900, '启用', ?)`, code, name, actor.OpenID)
	if err != nil {
		if isDuplicateErr(err) {
			return fmt.Errorf("%w：%s", ErrMDDuplicate, code)
		}
		return fmt.Errorf("新增角色失败: %w", err)
	}

	copied := 0
	if src := strings.TrimSpace(copyFrom); src != "" {
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM s_role WHERE code = ?`, src).Scan(&exists); err != nil {
			return fmt.Errorf("查询来源角色失败: %w", err)
		}
		if exists == 0 {
			return fmt.Errorf("%w：%s", ErrRoleNotFound, src)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO s_role_permission (role_code, point_code, level, created_by)
			SELECT ?, point_code, level, ? FROM s_role_permission WHERE role_code = ?`,
			code, actor.OpenID, src)
		if err != nil {
			return fmt.Errorf("复制权限失败: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			copied = int(n)
		}
	}

	field := "code,name"
	newVal := code + "," + name
	if copied > 0 {
		field += ",copy_from"
		newVal += "," + fmt.Sprintf("%s(%d)", copyFrom, copied)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "s_role", Action: "create", Field: field, NewValue: newVal,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RenameRole 重命名角色（内置角色只改显示名，code 不变）。
func (s *Store) RenameRole(ctx context.Context, code, newName string, actor MDActor) error {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return ErrRoleNameEmpty
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var old string
	err = tx.QueryRowContext(ctx, `SELECT name FROM s_role WHERE code = ? FOR UPDATE`, code).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRoleNotFound
	}
	if err != nil {
		return fmt.Errorf("查询角色失败: %w", err)
	}
	if old == newName {
		return ErrMDNoChange
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE s_role SET name = ? WHERE code = ?`, newName, code); err != nil {
		return fmt.Errorf("重命名失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "s_role", Action: "rename", Field: "name",
		OldValue: old, NewValue: newName,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetRoleStatus 启用 / 停用角色。
// ★★ 防锁死③：停用 sysadmin ⇒ 全体管理员立刻失权 ⇒ 拒绝（ErrLastAdmin）。
func (s *Store) SetRoleStatus(ctx context.Context, code, status string, actor MDActor) error {
	status = strings.TrimSpace(status)
	if status != "启用" && status != "停用" {
		return fmt.Errorf("%w：status 只允许 启用 / 停用", ErrMDBadInput)
	}
	if code == SysAdminRole && status == "停用" {
		return ErrLastAdmin
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var old string
	err = tx.QueryRowContext(ctx, `SELECT status FROM s_role WHERE code = ? FOR UPDATE`, code).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRoleNotFound
	}
	if err != nil {
		return fmt.Errorf("查询角色失败: %w", err)
	}
	if old == status {
		return ErrMDNoChange
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE s_role SET status = ? WHERE code = ?`, status, code); err != nil {
		return fmt.Errorf("更新角色状态失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "s_role", Action: "status", Field: "status",
		OldValue: old, NewValue: status,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteRole 删除**非内置**角色（连同其授权与绑定，同事务留痕）。
// ★ 内置角色一律拒绝（TC-M2-03：尝试删除内置角色「质检员」⇒ 被拒）。
func (s *Store) DeleteRole(ctx context.Context, code string, actor MDActor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var isSystem int
	err = tx.QueryRowContext(ctx, `SELECT is_system FROM s_role WHERE code = ? FOR UPDATE`, code).Scan(&isSystem)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRoleNotFound
	}
	if err != nil {
		return fmt.Errorf("查询角色失败: %w", err)
	}
	if isSystem == 1 {
		return fmt.Errorf("%w：%s", ErrBuiltinRole, code)
	}

	var grants, bindings int
	_ = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM s_role_permission WHERE role_code = ?`, code).Scan(&grants)
	_ = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM s_user_role WHERE role_code = ?`, code).Scan(&bindings)

	if _, err := tx.ExecContext(ctx, `DELETE FROM s_role_permission WHERE role_code = ?`, code); err != nil {
		return fmt.Errorf("删除授权失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM s_user_role WHERE role_code = ?`, code); err != nil {
		return fmt.Errorf("删除绑定失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM s_role WHERE code = ?`, code); err != nil {
		return fmt.Errorf("删除角色失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "s_role", Action: "delete", Field: "code",
		OldValue:    fmt.Sprintf("%s(grants=%d,bindings=%d)", code, grants, bindings),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- 用户角色绑定（D5）----

// UserBinding 是用户绑定视图的一行。
type UserBinding struct {
	OpenID string   `json:"open_id"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Roles  []string `json:"roles"`
}

// ListUserBindings 列出全部绑定（按 open_id 聚合，角色可叠加）。
func (s *Store) ListUserBindings(ctx context.Context) ([]UserBinding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ur.open_id, IFNULL(u.name, ''), IFNULL(u.status, ''), ur.role_code
		  FROM s_user_role ur
		  LEFT JOIN m_user u ON u.open_id = ur.open_id
		 ORDER BY ur.open_id, ur.role_code`)
	if err != nil {
		return nil, fmt.Errorf("查询用户绑定失败: %w", err)
	}
	defer rows.Close()
	idx := map[string]int{}
	out := []UserBinding{}
	for rows.Next() {
		var openID, name, status, role string
		if err := rows.Scan(&openID, &name, &status, &role); err != nil {
			return nil, err
		}
		i, ok := idx[openID]
		if !ok {
			out = append(out, UserBinding{OpenID: openID, Name: name, Status: status, Roles: []string{}})
			i = len(out) - 1
			idx[openID] = i
		}
		out[i].Roles = append(out[i].Roles, role)
	}
	return out, rows.Err()
}

// ListUserRoles 返回某账号绑定的角色。
func (s *Store) ListUserRoles(ctx context.Context, openID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role_code FROM s_user_role WHERE open_id = ? ORDER BY role_code`, openID)
	if err != nil {
		return nil, fmt.Errorf("查询账号角色失败: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BindUserRole 绑定角色（UNIQUE(open_id, role_code)：一账号可叠加多角色）。
// ★ 撞车 ⇒ ErrOccupied（TC-M2-05 的「提示已被占用」）。
func (s *Store) BindUserRole(ctx context.Context, openID, roleCode string, actor MDActor) error {
	openID = strings.TrimSpace(openID)
	roleCode = strings.TrimSpace(roleCode)
	if openID == "" {
		return fmt.Errorf("%w：open_id 不能为空", ErrMDBadInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM s_role WHERE code = ?`, roleCode).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRoleNotFound
	}
	if err != nil {
		return fmt.Errorf("查询角色失败: %w", err)
	}
	if status != "启用" {
		return fmt.Errorf("%w：%s 已停用", ErrRoleNotFound, roleCode)
	}
	// 用户行可能还不存在（按 open_id 直接绑定）⇒ 补齐，名字暂用 open_id
	if err := ensureUserTx(ctx, tx, openID, openID, actor.OpenID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO s_user_role (open_id, role_code, granted_by)
		VALUES (?, ?, ?)`, openID, roleCode, actor.OpenID); err != nil {
		if isDuplicateErr(err) {
			return fmt.Errorf("%w：%s × %s", ErrOccupied, openID, roleCode)
		}
		return fmt.Errorf("绑定失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "s_user_role", Action: "bind",
		Field: openID + " × " + roleCode, NewValue: "bound",
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// UnbindUserRole 解除绑定。
// ★★ 防锁死③：解绑**最后一个**启用中的系统管理员 ⇒ 拒绝（ErrLastAdmin）。
func (s *Store) UnbindUserRole(ctx context.Context, openID, roleCode string, actor MDActor) error {
	openID = strings.TrimSpace(openID)
	roleCode = strings.TrimSpace(roleCode)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	if roleCode == SysAdminRole {
		n, err := countEnabledSysAdmins(ctx, tx)
		if err != nil {
			return err
		}
		if n <= 1 {
			return ErrLastAdmin
		}
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM s_user_role WHERE open_id = ? AND role_code = ?`, openID, roleCode)
	if err != nil {
		return fmt.Errorf("解除绑定失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w：%s × %s", ErrMDDuplicate, openID, roleCode)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "s_user_role", Action: "unbind",
		Field: openID + " × " + roleCode, OldValue: "bound",
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// countEnabledSysAdmins 统计「启用用户 ∩ 启用 sysadmin 角色」的账号数。
func countEnabledSysAdmins(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT ur.open_id)
		  FROM s_user_role ur
		  JOIN m_user u ON u.open_id = ur.open_id AND u.status = '启用'
		  JOIN s_role r ON r.code = ur.role_code AND r.status = '启用'
		 WHERE ur.role_code = ?`, SysAdminRole).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("统计系统管理员账号失败: %w", err)
	}
	return n, nil
}
