package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/chadhao/jx-lab-trace/internal/audit"
)

// AppendAudit 落一笔审计（**唯一入口，只 INSERT**）。
// ★ 与业务写操作同语义：先校验再写；审计本身失败 ⇒ 调用方应让整笔操作失败
//
//	（本批的权限相关写操作在 httpapi handler 内串行调用，失败即返回 500）。
func (s *Store) AppendAudit(ctx context.Context, e audit.Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, auditInsertSQL, auditArgs(e)...)
	if err != nil {
		return fmt.Errorf("写入审计日志失败: %w", err)
	}
	return nil
}

// appendAuditTx 在**同一事务**内落一笔审计（UC-M0-05「同事务写 s_audit_log」）：
// 业务写入与留痕要么一起提交、要么一起回滚 —— 不允许「写成功但没留痕」。
func appendAuditTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, auditInsertSQL, auditArgs(e)...); err != nil {
		return fmt.Errorf("写入审计日志失败: %w", err)
	}
	return nil
}

const auditInsertSQL = `
		INSERT INTO s_audit_log
			(entity, entity_id, action, field, old_value, new_value,
			 actor_open_id, actor_name, actor_role, ip, reason, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'app')`

func auditArgs(e audit.Entry) []interface{} {
	return []interface{}{
		e.Entity, nullID(e.EntityID), e.Action, nullIfEmpty(truncateField(e.Field)),
		nullIfEmpty(e.OldValue), nullIfEmpty(e.NewValue),
		e.ActorOpenID, e.ActorName, nullIfEmpty(e.ActorRole),
		nullIfEmpty(e.IP), nullIfEmpty(e.Reason),
	}
}

// truncateField 截断 s_audit_log.field —— 该列是 VARCHAR(64)（**64 个字符**），
// 超长会 1406 并让**整笔业务写入**跟着回滚；完整内容在 old/new_value（TEXT）里。
// ★ 按 rune 截，避免把中文字劈开。
func truncateField(s string) string {
	const max = 64
	if len(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

// AuditRow 是 s_audit_log 的一行（审计查看接口用）。
type AuditRow struct {
	ID          int64  `json:"id"`
	Entity      string `json:"entity"`
	EntityID    int64  `json:"entity_id"`
	Action      string `json:"action"`
	Field       string `json:"field"`
	OldValue    string `json:"old_value"`
	NewValue    string `json:"new_value"`
	ActorOpenID string `json:"actor_open_id"`
	ActorName   string `json:"actor_name"`
	ActorRole   string `json:"actor_role"`
	IP          string `json:"ip"`
	Reason      string `json:"reason"`
	At          string `json:"at"`
}

// ListAudit 最近 limit 条审计（受 sys.audit.view 保护的入口用）。
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entity, IFNULL(entity_id, 0), action, IFNULL(field, ''),
		       IFNULL(old_value, ''), IFNULL(new_value, ''),
		       IFNULL(actor_open_id, ''), IFNULL(actor_name, ''), IFNULL(actor_role, ''),
		       IFNULL(ip, ''), IFNULL(reason, ''), at
		  FROM s_audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询审计日志失败: %w", err)
	}
	defer rows.Close()

	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.Entity, &r.EntityID, &r.Action, &r.Field,
			&r.OldValue, &r.NewValue, &r.ActorOpenID, &r.ActorName, &r.ActorRole,
			&r.IP, &r.Reason, &r.At); err != nil {
			return nil, fmt.Errorf("读取审计日志失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FindAuditByEntity 查某对象的审计（测试断言用）。
func (s *Store) FindAuditByEntity(ctx context.Context, entity string, entityID int64) ([]AuditRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entity, IFNULL(entity_id, 0), action, IFNULL(field, ''),
		       IFNULL(old_value, ''), IFNULL(new_value, ''),
		       IFNULL(actor_open_id, ''), IFNULL(actor_name, ''), IFNULL(actor_role, ''),
		       IFNULL(ip, ''), IFNULL(reason, ''), at
		  FROM s_audit_log WHERE entity = ? AND entity_id = ? ORDER BY id DESC`, entity, entityID)
	if err != nil {
		return nil, fmt.Errorf("查询审计日志失败: %w", err)
	}
	defer rows.Close()

	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.Entity, &r.EntityID, &r.Action, &r.Field,
			&r.OldValue, &r.NewValue, &r.ActorOpenID, &r.ActorName, &r.ActorRole,
			&r.IP, &r.Reason, &r.At); err != nil {
			return nil, fmt.Errorf("读取审计日志失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullID(v int64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
