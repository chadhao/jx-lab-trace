package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/config"
	"github.com/chadhao/jx-lab-trace/internal/permission"
)

// openTestStore 打开测试库连接。
//
// ★★ 环境约定（docs/05，用户 2026-10-09 硬约束）：本机 go test 是**纯单测，
// 不得依赖外部服务** ⇒ DB 集成测试显式 opt-in（JX_TEST_DB=1，在测试服务器上跑）；
// 未设则 Skip 并说明，**不是静默跳过**；库内一致性另由门禁判据④ 做只读计数校验。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("JX_TEST_DB") != "1" {
		t.Skip("JX_TEST_DB 未设为 1：按 docs/05 本机只跑纯单测；DB 集成测试在测试服务器上以 JX_TEST_DB=1 执行")
	}
	if err := config.LoadDotEnv(filepathDotEnv()); err != nil {
		t.Fatalf("读取 .env 失败: %v", err)
	}
	dsn := os.Getenv("JX_DB_DSN")
	if dsn == "" {
		t.Fatal("JX_TEST_DB=1 但缺 JX_DB_DSN：无法执行 DB 集成测试")
	}
	st, err := Open(dsn)
	if err != nil {
		t.Fatalf("数据库不可用: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func filepathDotEnv() string { return "../../" + config.DotEnvPath() }

// migrateForTest 跑一遍幂等迁移（测试自足，不依赖人工先跑 -migrate）。
func migrateForTest(t *testing.T) *Store {
	t.Helper()
	st := openTestStore(t)
	if _, err := st.Migrate(context.Background(), ""); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return st
}

// TestTC_M0_Migrate_IdempotentAndSeeded 覆盖 A2（幂等）与 A7（51/6/306）。
// 对应 TC-M0-08（权限点注册：字典由代码注册且不悬空）的库侧事实。
func TestTC_M0_Migrate_IdempotentAndSeeded(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()

	first, err := st.TableCount(ctx)
	if err != nil {
		t.Fatalf("统计表数失败: %v", err)
	}
	if first != 38 {
		t.Fatalf("表数应为 38，实际 %d", first)
	}

	// 第二遍：必须无错、无新表（A2）
	res, err := st.Migrate(ctx, "")
	if err != nil {
		t.Fatalf("第二次迁移失败（必须幂等）: %v", err)
	}
	if res.TablesCreated != 0 {
		t.Fatalf("第二次迁移不应新建表，实际新建 %d", res.TablesCreated)
	}
	second, err := st.TableCount(ctx)
	if err != nil {
		t.Fatalf("统计表数失败: %v", err)
	}
	if second != first {
		t.Fatalf("表数漂移: %d → %d", first, second)
	}

	// A7：51 权限点 / 6 角色 / 306 授权
	checkCount(t, st, "s_permission_point", 51)
	checkCount(t, st, "s_role", 6)
	checkCount(t, st, "s_role_permission", 306)

	// 权限点必须与代码注册表逐条对齐
	pts, err := st.ListPermissionPoints(ctx)
	if err != nil {
		t.Fatalf("查询权限点失败: %v", err)
	}
	if len(pts) != len(permission.All) {
		t.Fatalf("库内权限点 %d 个，代码注册 %d 个", len(pts), len(permission.All))
	}
	inDB := map[string]bool{}
	for _, p := range pts {
		inDB[p.Code] = true
	}
	for _, c := range permission.All {
		if !inDB[c.String()] {
			t.Fatalf("代码注册的 %s 未落库", c)
		}
	}
}

func checkCount(t *testing.T, st *Store, table string, want int) {
	t.Helper()
	got, err := st.CountRows(context.Background(), table)
	if err != nil {
		t.Fatalf("统计 %s 失败: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s 行数应为 %d，实际 %d", table, want, got)
	}
}

// TestTC_M0_Session_StoreAndRevoke 覆盖 A4/A5 的库侧事实：
// 会话**落库**（不是内存 map）、登出写 revoked_at 后长期失效。
func TestTC_M0_Session_StoreAndRevoke(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()
	sid, err := NewSessionID()
	if err != nil {
		t.Fatalf("生成会话 ID 失败: %v", err)
	}
	openID := "ou_test_store_session"
	exp := time.Now().Add(time.Hour)
	if err := st.CreateSession(ctx, sid, openID, "127.0.0.1", exp); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(ctx, `DELETE FROM s_session WHERE open_id = ?`, openID)
	})

	sess, err := st.LoadSession(ctx, sid)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if sess.OpenID != openID {
		t.Fatalf("会话归属错乱: %s", sess.OpenID)
	}
	if !sess.Valid(time.Now()) {
		t.Fatal("新建会话应当有效")
	}

	// 滑动续期
	newExp := time.Now().Add(2 * time.Hour)
	ok, err := st.TouchSession(ctx, sid, time.Now(), newExp)
	if err != nil || !ok {
		t.Fatalf("续期失败: ok=%v err=%v", ok, err)
	}
	sess, _ = st.LoadSession(ctx, sid)
	if !sess.ExpiresAt.After(exp.Add(time.Minute)) {
		t.Fatalf("expires_at 未被推后: %v", sess.ExpiresAt)
	}

	// 登出 ⇒ 永久失效（即使 expires_at 还在将来）
	if err := st.RevokeSession(ctx, sid); err != nil {
		t.Fatalf("登出失败: %v", err)
	}
	sess, _ = st.LoadSession(ctx, sid)
	if sess.Valid(time.Now()) {
		t.Fatal("已登出的会话必须无效")
	}
	ok, err = st.TouchSession(ctx, sid, time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("续期调用失败: %v", err)
	}
	if ok {
		t.Fatal("已登出的会话不得被续期复活")
	}
}

// TestTC_M0_Level_UnionAndDeny 覆盖 UC-M0-03 的并集 / 无记录即 NONE。
func TestTC_M0_Level_UnionAndDeny(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()
	admin := "ou_test_union_admin"
	reader := "ou_test_union_reader"
	none := "ou_test_union_none"
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(ctx, `DELETE FROM s_user_role WHERE open_id IN (?,?,?)`, admin, reader, none)
	})

	mustExec := func(query string, args ...interface{}) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("准备测试数据失败: %v", err)
		}
	}
	mustExec(`INSERT IGNORE INTO s_user_role (open_id, role_code, granted_by) VALUES (?, 'sysadmin', 'test')`, admin)
	mustExec(`INSERT IGNORE INTO s_user_role (open_id, role_code, granted_by) VALUES (?, 'management', 'test')`, reader)
	mustExec(`INSERT IGNORE INTO s_user_role (open_id, role_code, granted_by) VALUES (?, 'receiver', 'test')`, none)

	// sysadmin: sys.perm.edit = ALL ⇒ Granted
	lv, err := st.LevelsFor(ctx, admin, permission.SysPermEdit)
	if err != nil {
		t.Fatalf("查询级别失败: %v", err)
	}
	if !lv.Granted() || !lv.Allows(permission.LevelAll) {
		t.Fatalf("sysadmin 应在 sys.perm.edit 上为 ALL，实际 %v", lv)
	}

	// management: sys.perm.edit = NONE ⇒ 拒绝
	lv, err = st.LevelsFor(ctx, reader, permission.SysPermEdit)
	if err != nil {
		t.Fatalf("查询级别失败: %v", err)
	}
	if lv.Granted() {
		t.Fatalf("management 在 sys.perm.edit 上应为 NONE，实际 %v", lv)
	}

	// 未映射任何角色 ⇒ 空集合 ⇒ 拒绝（deny by default）
	lv, err = st.LevelsFor(ctx, "ou_never_mapped_anywhere", permission.SysPermEdit)
	if err != nil {
		t.Fatalf("查询级别失败: %v", err)
	}
	if lv.Granted() {
		t.Fatalf("未映射账号必须被拒，实际 %v", lv)
	}
}
