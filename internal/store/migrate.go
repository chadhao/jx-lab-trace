package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/chadhao/jx-lab-trace/internal/permission"
	"github.com/chadhao/jx-lab-trace/migrations"
)

// MigrateResult 是一次迁移的结果回执（启动日志回显用）。
type MigrateResult struct {
	TablesCreated  int    // 本次真正 CREATE 的表数
	TablesExisted  int    // 已存在、按幂等规则跳过的表数
	PointsUpserted int    // 权限点（= len(permission.All)，51）
	PointsRemoved  int    // 库中存在但代码未注册的权限点（字典以代码为准，删除）
	RolesUpserted  int    // 角色（6）
	GrantsInserted int    // 本次新补的授权行（空库首跑 = 306）
	GrantsTotal    int    // 库内授权总行数
	BootstrapAdmin string // 引导的系统管理员 open_id（空 = 未配置）
}

// permSpec 对应 spec/permission-points.json 的结构（只取迁移需要的字段）。
type permSpec struct {
	PermissionPoints []struct {
		Code   string   `json:"code"`
		Module string   `json:"module"`
		Name   string   `json:"name"`
		Levels []string `json:"levels"`
	} `json:"permission_points"`
	Roles []struct {
		Code     string `json:"code"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		IsSystem bool   `json:"is_system"`
	} `json:"roles"`
	Seed map[string]map[string]string `json:"seed"`
}

var createTableRe = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+([A-Za-z0-9_]+)`)

// Migrate 执行**可重复执行（幂等）**的迁移：
//
//	① spec/schema.sql 的 38 张表：逐条判断 information_schema，已存在则跳过；
//	② 权限点字典按**代码**（permission.All）upsert —— 后台不可增删，
//	   库中多出的 code 属于「字典外假权限」，删除；
//	③ 角色（spec 6 个）upsert；
//	④ 授权种子只**补齐缺失行**（INSERT IGNORE）—— 后台已改过的 level 不被覆盖；
//	⑤ JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID 存在时引导首个系统管理员（防锁死兜底）。
//
// 连跑两次：第二次 TablesCreated=0、无报错、表数稳定 38（A2）。
func (s *Store) Migrate(ctx context.Context, bootstrapOpenID string) (MigrateResult, error) {
	var res MigrateResult

	stmts := splitSQL(string(migrations.SchemaSQL))
	if len(stmts) == 0 {
		return res, fmt.Errorf("迁移文件为空：migrations/0001_init.sql")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer tx.Rollback()

	for _, stmt := range stmts {
		m := createTableRe.FindStringSubmatch(stmt)
		if m != nil {
			exists, err := tableExists(ctx, tx, m[1])
			if err != nil {
				return res, fmt.Errorf("检查表 %s 是否存在失败: %w", m[1], err)
			}
			if exists {
				res.TablesExisted++
				continue
			}
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return res, fmt.Errorf("执行 DDL 失败（语句前 120 字符）%q: %w", trim(stmt, 120), err)
		}
		if m != nil {
			res.TablesCreated++
		}
	}

	var spec permSpec
	if err := json.Unmarshal(migrations.PermissionSpecJSON, &spec); err != nil {
		return res, fmt.Errorf("解析权限点规格失败: %w", err)
	}

	// ② 权限点：代码是唯一注册源。
	specByCode := map[string]int{}
	for i, p := range spec.PermissionPoints {
		specByCode[p.Code] = i
	}
	for i, code := range permission.All {
		idx, ok := specByCode[code.String()]
		if !ok {
			return res, fmt.Errorf("代码注册了规格中不存在的权限点 %q（spec/permission-points.json#permission_points）", code)
		}
		p := spec.PermissionPoints[idx]
		levels := strings.Join(p.Levels, ",")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO s_permission_point (code, module, name, levels, sort, is_system, status, created_by)
			VALUES (?, ?, ?, ?, ?, 1, '启用', 'code')
			ON DUPLICATE KEY UPDATE module = VALUES(module), name = VALUES(name),
				levels = VALUES(levels), sort = VALUES(sort)`,
			p.Code, p.Module, p.Name, levels, i*10,
		); err != nil {
			return res, fmt.Errorf("注册权限点 %s 失败: %w", p.Code, err)
		}
		res.PointsUpserted++
	}
	del, err := tx.ExecContext(ctx, `DELETE FROM s_permission_point WHERE code NOT IN (`+placeholders(len(permission.All))+`)`, codesArgs()...)
	if err != nil {
		return res, fmt.Errorf("清理字典外权限点失败: %w", err)
	}
	if n, _ := del.RowsAffected(); n > 0 {
		res.PointsRemoved = int(n)
	}

	// ③ 角色。
	for i, r := range spec.Roles {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO s_role (code, name, kind, is_system, sort, status, created_by)
			VALUES (?, ?, ?, ?, ?, '启用', 'spec')
			ON DUPLICATE KEY UPDATE name = VALUES(name), kind = VALUES(kind)`,
			r.Code, r.Name, r.Kind, boolToInt(r.IsSystem), i*10,
		); err != nil {
			return res, fmt.Errorf("注册角色 %s 失败: %w", r.Code, err)
		}
		res.RolesUpserted++
	}

	// ④ 授权种子：只补缺失行。
	for code, byRole := range spec.Seed {
		for role, level := range byRole {
			r, err := tx.ExecContext(ctx, `
				INSERT IGNORE INTO s_role_permission (role_code, point_code, level, created_by)
				VALUES (?, ?, ?, 'spec')`, role, code, level)
			if err != nil {
				return res, fmt.Errorf("种入授权 %s×%s 失败: %w", role, code, err)
			}
			if n, _ := r.RowsAffected(); n > 0 {
				res.GrantsInserted++
			}
		}
	}

	// ⑤ 引导首个系统管理员（防锁死兜底，docs/01 §8.0.1 硬约束 3）。
	if bootstrapOpenID != "" {
		if err := ensureUserTx(ctx, tx, bootstrapOpenID, bootstrapOpenID, "bootstrap"); err != nil {
			return res, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT IGNORE INTO s_user_role (open_id, role_code, granted_by)
			VALUES (?, 'sysadmin', 'bootstrap')`, bootstrapOpenID); err != nil {
			return res, fmt.Errorf("引导系统管理员 %s 失败: %w", bootstrapOpenID, err)
		}
		res.BootstrapAdmin = bootstrapOpenID
	}

	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM s_role_permission`).Scan(&res.GrantsTotal); err != nil {
		return res, fmt.Errorf("统计授权行数失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("提交迁移事务失败: %w", err)
	}
	return res, nil
}

// TableCount 返回当前库的表数（A2：幂等后应稳定 = 38）。
func (s *Store) TableCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()`).Scan(&n)
	return n, err
}

// CountRows 统计指定表行数（权限点 / 角色 / 授权的验收计数）。
func (s *Store) CountRows(ctx context.Context, table string) (int, error) {
	if !regexp.MustCompile(`^[a-z_]+$`).MatchString(table) {
		return 0, fmt.Errorf("非法表名 %q", table)
	}
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&n)
	return n, err
}

func tableExists(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = DATABASE() AND table_name = ?`, name).Scan(&n)
	return n > 0, err
}

// splitSQL 去掉 `--` 整行注释后按 `;` 切分语句。
// （spec/schema.sql 无行内 `--` 注释、分号只出现在语句末尾 —— 已核对；
// 若将来规格引入行内注释，切分会先失真并由迁移测试当场报红。）
func splitSQL(script string) []string {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	var out []string
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// codesArgs 返回 permission.All 的 code 列表（与 placeholders 配对）。
func codesArgs() []interface{} {
	args := make([]interface{}, 0, len(permission.All))
	for _, c := range permission.All {
		args = append(args, c.String())
	}
	return args
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
