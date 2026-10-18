package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/chadhao/jx-lab-trace/internal/audit"
)

// BootstrapFirstSysAdmin 「首个飞书登录自动成为系统管理员」（N-025 D1）。
//
// 触发条件三条（本函数与调用方 httpapi.openSession 合起来保证）：
//
//  1. 登录来源 = feishu_login —— 由调用方判定，dev 桩根本不进本函数（防 ou_dev_admin 抢占）；
//  2. s_user_role 中不存在任何 sysadmin 绑定 —— **本函数在事务内二次确认**；
//  3. JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID 未配置 —— 由调用方判定（⇒ 与启动引导互斥，
//     两机制谁都没触发时系统处于「无 sysadmin」态，等首个飞书登录）。
//
// 幂等：条件②不满足 ⇒ 返回 (false, nil)（重复登录不重复绑）；并发下撞
// UNIQUE(open_id, role_code) 也按「未引导」处理。
// 成功 ⇒ 同事务写审计 action=bootstrap_sysadmin（actor ＝ 该用户本人）。
//
// ★ 只写 s_user_role / m_user / s_audit_log；★ 绝不触碰 s_role / s_role_permission /
// s_permission_point（N-025 §6-1：三张权限表除既有权限管理 API 外任何路径不得被写）。
func (s *Store) BootstrapFirstSysAdmin(ctx context.Context, openID, name string, actor MDActor) (bool, error) {
	if openID == "" {
		return false, fmt.Errorf("%w：open_id 不能为空", ErrMDBadInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// 条件②（事务内二次确认）：已存在 sysadmin 绑定 ⇒ 不引导。
	var bindings int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM s_user_role WHERE role_code = ?`, SysAdminRole).Scan(&bindings); err != nil {
		return false, fmt.Errorf("检查系统管理员绑定失败: %w", err)
	}
	if bindings > 0 {
		return false, nil
	}

	// 角色定义缺席/停用 ⇒ 无可引导（登录随后按 deny by default 拒绝，不硬塞脏绑定）。
	var roleStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM s_role WHERE code = ?`, SysAdminRole).Scan(&roleStatus)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && roleStatus != "启用") {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询系统管理员角色失败: %w", err)
	}

	// 任务包 D1 口径「EnsureUser 之后再绑定」：先落账号行，再绑角色，同一事务。
	if err := ensureUserTx(ctx, tx, openID, name, "bootstrap_login"); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO s_user_role (open_id, role_code, granted_by)
		VALUES (?, ?, 'bootstrap_login')`, openID, SysAdminRole); err != nil {
		if isDuplicateErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("引导系统管理员失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "s_user_role", Action: "bootstrap_sysadmin",
		Field: openID + " × " + SysAdminRole, NewValue: "bound",
		ActorOpenID: openID, ActorName: name, ActorRole: SysAdminRole, IP: actor.IP,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("提交引导事务失败: %w", err)
	}
	return true, nil
}
