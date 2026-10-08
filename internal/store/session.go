package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrNoSession 表示会话不存在（含：从未创建 / 已被清理）。
var ErrNoSession = errors.New("会话不存在")

// Session 是 s_session 的一行（服务端真相源）。
type Session struct {
	SessionID string
	OpenID    string
	IssuedAt  time.Time
	ExpiresAt time.Time
	RevokedAt sql.NullTime
	IP        string
}

// Valid 判定会话当前是否有效：未撤销 且 未过期。
// ★ 判定只看**数据库**（重启后原 cookie 仍有效 ⇒ A4；登出写 revoked_at ⇒ 重启后仍失效 ⇒ A5）。
func (s Session) Valid(now time.Time) bool {
	if s.RevokedAt.Valid {
		return false
	}
	return now.Before(s.ExpiresAt)
}

// NewSessionID 生成 32 字节随机会话 ID（hex 64 字符，适配 VARCHAR(64)）。
func NewSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成会话 ID 失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// CreateSession 落库一条新会话（★ 不用内存 map）。
func (s *Store) CreateSession(ctx context.Context, sessionID, openID, ip string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO s_session (session_id, open_id, issued_at, expires_at, last_seen_at, ip, created_by)
		VALUES (?, ?, NOW(3), ?, NOW(3), ?, 'app')`,
		sessionID, openID, expiresAt, ip)
	if err != nil {
		return fmt.Errorf("写入会话失败: %w", err)
	}
	return nil
}

// LoadSession 按 session_id 读会话；不存在 ⇒ ErrNoSession。
func (s *Store) LoadSession(ctx context.Context, sessionID string) (Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx, `
		SELECT session_id, open_id, issued_at, expires_at, last_seen_at, revoked_at, IFNULL(ip, '')
		  FROM s_session WHERE session_id = ?`, sessionID).
		Scan(&sess.SessionID, &sess.OpenID, &sess.IssuedAt, &sess.ExpiresAt, new(sql.NullTime), &sess.RevokedAt, &sess.IP)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("读取会话失败: %w", err)
	}
	return sess, nil
}

// TouchSession 滑动续期：把 expires_at 推到 expiresAt 并记 last_seen_at。
// ★ 只对「未撤销且当前仍有效」的会话续期（过期/已登出的会话不会被续活）。
// 返回是否续期成功。
func (s *Store) TouchSession(ctx context.Context, sessionID string, now, expiresAt time.Time) (bool, error) {
	r, err := s.db.ExecContext(ctx, `
		UPDATE s_session SET expires_at = ?, last_seen_at = ?
		 WHERE session_id = ? AND revoked_at IS NULL AND expires_at > ?`,
		expiresAt, now, sessionID, now)
	if err != nil {
		return false, fmt.Errorf("会话续期失败: %w", err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("会话续期结果读取失败: %w", err)
	}
	return n == 1, nil
}

// RevokeSession 登出：写 revoked_at（**立即失效，且重启后仍失效**）。
func (s *Store) RevokeSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE s_session SET revoked_at = NOW(3) WHERE session_id = ? AND revoked_at IS NULL`, sessionID)
	if err != nil {
		return fmt.Errorf("登出失效会话失败: %w", err)
	}
	return nil
}
