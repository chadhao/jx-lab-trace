package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EnsureUser 保证 m_user 有该 open_id 的行（飞书免登首次进入 / dev 桩共用）。
// 名字已存在则不覆盖（真实姓名以首次同步为准）。
func (s *Store) EnsureUser(ctx context.Context, openID, name string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO m_user (open_id, name, status, created_by)
		VALUES (?, ?, '启用', 'app')
		ON DUPLICATE KEY UPDATE open_id = open_id`, openID, name)
	if err != nil {
		return fmt.Errorf("确保用户行存在失败: %w", err)
	}
	return nil
}

func ensureUserTx(ctx context.Context, tx *sql.Tx, openID, name, by string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO m_user (open_id, name, status, created_by)
		VALUES (?, ?, '启用', ?)
		ON DUPLICATE KEY UPDATE open_id = open_id`, openID, name, by)
	if err != nil {
		return fmt.Errorf("确保用户行存在失败: %w", err)
	}
	return nil
}

// UserName 取 m_user.name；无行时回退 open_id 本身（不阻塞 /api/me）。
func (s *Store) UserName(ctx context.Context, openID string) string {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT name FROM m_user WHERE open_id = ?`, openID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return openID
	}
	return name
}

// UserStatus 取 m_user.status；无行 ⇒ ("", nil)（不是错误——deny 由角色判定兜底）。
func (s *Store) UserStatus(ctx context.Context, openID string) (string, error) {
	var st string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM m_user WHERE open_id = ?`, openID).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取用户状态失败: %w", err)
	}
	return st, nil
}

// UpsertUserResult 是一次账号 upsert 的分类计数（D3 幂等统计）。
type UpsertUserResult struct {
	Created int // 原本无行，新建
	Updated int // 有行且姓名有变，更新
	Skipped int // 有行且姓名未变，跳过
}

// UpsertUser 按 open_id **只建/更新** m_user 行（★ 绝不删除、不动 s_user_role）。
//   - 无行 ⇒ INSERT status=启用；计入 Created
//   - 有行且 name 相同 ⇒ 不写；计入 Skipped
//   - 有行且 name 不同 ⇒ 只更新 name（★ 不动 status：停用状态不得被同步悄悄复活）；计入 Updated
//
// ★ 重复调用同一输入 ⇒ 第二次全 Skipped（幂等）。
func (s *Store) UpsertUser(ctx context.Context, openID, name, by string) (UpsertUserResult, error) {
	var res UpsertUserResult
	if openID == "" {
		return res, errors.New("open_id 不能为空")
	}
	if name == "" {
		name = openID
	}
	var existingName string
	err := s.db.QueryRowContext(ctx,
		`SELECT name FROM m_user WHERE open_id = ?`, openID).Scan(&existingName)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO m_user (open_id, name, status, created_by)
			VALUES (?, ?, '启用', ?)`, openID, name, by); err != nil {
			return res, fmt.Errorf("新建用户失败: %w", err)
		}
		res.Created = 1
		return res, nil
	case err != nil:
		return res, fmt.Errorf("查询用户失败: %w", err)
	}
	if existingName == name {
		res.Skipped = 1
		return res, nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE m_user SET name = ? WHERE open_id = ?`, name, openID); err != nil {
		return res, fmt.Errorf("更新用户失败: %w", err)
	}
	res.Updated = 1
	return res, nil
}

// DisableUser 离职停用：status=停用（有行则改；无行则补一行停用的，防止「没号还能靠角色登」）。
// ★ 返回停用前的旧状态（无行 = 空串），供审计 old_value 用。
func (s *Store) DisableUser(ctx context.Context, openID, by string) (string, error) {
	if openID == "" {
		return "", errors.New("open_id 不能为空")
	}
	old, err := s.UserStatus(ctx, openID)
	if err != nil {
		return "", err
	}
	if old == "" {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO m_user (open_id, name, status, created_by)
			VALUES (?, ?, '停用', ?)`, openID, openID, by); err != nil {
			return "", fmt.Errorf("补建停用用户失败: %w", err)
		}
		return "", nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE m_user SET status = '停用' WHERE open_id = ?`, openID); err != nil {
		return "", fmt.Errorf("停用用户失败: %w", err)
	}
	return old, nil
}

// RevokeUserSessions 立即失效该用户**全部活跃会话**（离职安全要求：在线会话当场作废）。
// 返回受影响（本次真正从有效→撤销）的会话数。
func (s *Store) RevokeUserSessions(ctx context.Context, openID string) (int64, error) {
	r, err := s.db.ExecContext(ctx,
		`UPDATE s_session SET revoked_at = NOW(3) WHERE open_id = ? AND revoked_at IS NULL`, openID)
	if err != nil {
		return 0, fmt.Errorf("批量失效会话失败: %w", err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("批量失效会话影响行数获取失败: %w", err)
	}
	return n, nil
}
