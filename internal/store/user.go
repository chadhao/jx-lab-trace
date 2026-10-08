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
