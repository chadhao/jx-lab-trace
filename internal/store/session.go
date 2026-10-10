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
// TouchSession 滑动续期。
//
// ★★★ 语义（2026-10-10 修正，实测事故驱动）：
//
//	**返回值表示「会话是否有效」，不是「是否发生了续期」。**
//	★ 根因：原实现把"UPDATE 没命中/写报错"当作"会话无效"，于是**并发请求里撞上写竞争的
//	  那一个被误判为未登录 ⇒ 401**。现场：首屏 `Promise.all` 三并发 ⇒ **随机一个 401**，
//	  且**每次强刷中招的请求都不同**；而同期 `/api/me` 仍 200 ⇒ **会话根本没坏**。
//	⇒ 判据：**"续期没成功" ≠ "会话无效"**。二者混同会让并发请求随机掉线。
//
// ★ 并发优化：只在**临近过期**（`expires_at < renewBefore`）时才真正写库。
//
//	绝大多数请求**一次库都不用写** ⇒ 从根上消除"首屏并发各写同一行"的锁竞争。
func (s *Store) TouchSession(ctx context.Context, sessionID string, now, expiresAt, renewBefore time.Time) (bool, error) {
	// ① 仅当「剩余有效期不足」时才续期 —— 写条件收窄 ⇒ 并发时几乎不产生写。
	r, err := s.db.ExecContext(ctx, `
		UPDATE s_session SET expires_at = ?, last_seen_at = ?
		 WHERE session_id = ? AND revoked_at IS NULL AND expires_at > ? AND expires_at < ?`,
		expiresAt, now, sessionID, now, renewBefore)
	if err == nil {
		if n, e := r.RowsAffected(); e == nil && n == 1 {
			return true, nil // 已续期，会话有效
		}
	}
	// ② 没命中（= 无需续期）**或写报错**（如并发/锁等待）⇒ 一律**读一次判定会话是否仍有效**。
	//    ★ 关键：这一步把"没续成"与"会话无效"彻底分开。
	sess, lerr := s.LoadSession(ctx, sessionID)
	if lerr != nil || !sess.Valid(now) {
		return false, nil // 确实无效/已撤销/已过期
	}
	return true, nil // 会话有效，只是无需续期（或续期被并发挡下，但会话没坏）
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
