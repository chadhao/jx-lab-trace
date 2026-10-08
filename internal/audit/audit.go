// Package audit 定义审计留痕的统一结构与「只增不改」不变量。
//
// ★ docs/01 P1：审计**只增不改** —— 本包只提供写入（Append）语义的数据结构，
//
//	任何对 s_audit_log 的 UPDATE / DELETE 都会被
//	internal/audit/append_only_test.go（TC-M0-11）当场判红。
package audit

import (
	"fmt"
	"strings"
)

// Entry 是一笔审计记录（与 s_audit_log 列一一对应）。
type Entry struct {
	Entity      string // 对象类型，如 s_permission_point
	EntityID    int64  // 对象主键（无则 0）
	Action      string // create / status / perm_change / login / logout ...
	Field       string // 被改动的字段（可空）
	OldValue    string // 旧值（可空）
	NewValue    string // 新值（可空）
	ActorOpenID string // 操作者
	ActorName   string
	ActorRole   string // 操作者角色（逗号分隔）
	IP          string
	Reason      string // 变更原因（可空）
}

// Validate 校验必填项（写库前调用）。
func (e Entry) Validate() error {
	if strings.TrimSpace(e.Entity) == "" {
		return fmt.Errorf("审计记录缺 entity")
	}
	if strings.TrimSpace(e.Action) == "" {
		return fmt.Errorf("审计记录缺 action")
	}
	if strings.TrimSpace(e.ActorOpenID) == "" {
		return fmt.Errorf("审计记录缺 actor_open_id（谁做的必须可追溯）")
	}
	return nil
}

// Describe 生成一行可读摘要（日志用）。
func (e Entry) Describe() string {
	return fmt.Sprintf("%s %s#%d %s(%s) %s -> %s by %s",
		e.Action, e.Entity, e.EntityID, e.Field, e.IP, e.OldValue, e.NewValue, e.ActorOpenID)
}
