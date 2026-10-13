package httpapi

// ===== N-021 · 销毁记录列表守卫回归 + 全仓 GET 读入口审计（MIMO-NEXT-FIX-05）=====
//
// ★ 缺陷（COLLAB N-021）：GET /api/sample/destroy 原挂
//	sample.retain.destroy.init + LevelRead；该点 levels = INIT/NONE（动作型）
//	⇒ Allows("READ") 恒 false ⇒ 任何角色（含 sysadmin）都 403，列表对所有人不可用。
// ★ 修复：改挂 sample.retain.in + LevelRead（销毁记录＝留样域读，与 /retention 列表同口径）。
// ★ 通用教训（任务包 §6-2）：**动作型权限点（INIT/APPROVE）不得守 GET** ——
//	TestTC_N021_02 从**路由表**（echo Route 表）逐条驱动核对，不再用源码 grep 拼串。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	n021FullOpenID  = "ou_test_n021_full"
	n021ReadOpenID  = "ou_test_n021_read"
	n021AuditOpenID = "ou_test_n021_audit"
	n021FullRole    = "tr_n021_full"
	n021ReadRole    = "tr_n021_read"
	n021AuditRole   = "tr_n021_audit"
)

// n021Wipe 清掉本用例族的角色授权与账号绑定（只动 tr_n021_% / ou_test_n021_%）。
func n021Wipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("N-021 清理失败（忽略，继续）: %v", err)
		}
	}
	q(`DELETE FROM s_role_permission WHERE role_code LIKE 'tr\_n021\_%'`)
	q(`DELETE FROM s_user_role WHERE role_code LIKE 'tr\_n021\_%'`)
	q(`DELETE FROM s_role WHERE code LIKE 'tr\_n021\_%'`)
	q(`DELETE FROM s_user_role WHERE open_id LIKE 'ou\_test\_n021\_%'`)
}

// n021GrantMax 建一个角色，并把 s_permission_point 里**每个点的最高可授予级别**发给它：
// ALL > READ > INIT > APPROVE（NONE 不发）。这正是「矩阵里能配出的最强角色」模型 ——
// 任何 GET 若连这个角色都 403，说明该 GET 对**全仓任何角色**都不可达。
func n021GrantMax(t *testing.T, st *store.Store, roleCode, roleName string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO s_role (code, name, kind, is_system, status)
VALUES (?, ?, 'business', 0, '启用')`, roleCode, roleName); err != nil {
		t.Fatalf("建 N-021 测试角色失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO s_role_permission (role_code, point_code, level, updated_by, created_by)
SELECT ?, code,
       CASE
         WHEN FIND_IN_SET('ALL', levels) > 0     THEN 'ALL'
         WHEN FIND_IN_SET('READ', levels) > 0    THEN 'READ'
         WHEN FIND_IN_SET('INIT', levels) > 0    THEN 'INIT'
         WHEN FIND_IN_SET('APPROVE', levels) > 0 THEN 'APPROVE'
         ELSE 'NONE'
       END,
       'test', 'test'
  FROM s_permission_point`, roleCode); err != nil {
		t.Fatalf("N-021 角色授权失败: %v", err)
	}
}

// n021GrantOne 建一个角色并只授一个点为指定级别（其余点无记录 = NONE）。
func n021GrantOne(t *testing.T, st *store.Store, roleCode, roleName, pointCode, level string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO s_role (code, name, kind, is_system, status)
VALUES (?, ?, 'business', 0, '启用')`, roleCode, roleName); err != nil {
		t.Fatalf("建 N-021 测试角色失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO s_role_permission (role_code, point_code, level, updated_by, created_by)
VALUES (?, ?, ?, 'test', 'test')`, roleCode, pointCode, level); err != nil {
		t.Fatalf("N-021 角色授权失败: %v", err)
	}
}

// TestTC_N021_01_销毁记录列表守卫回归 —— 任务包 D2 五条：
//
//	① 全授权账号 ⇒ GET /api/sample/destroy 200（?pending=1 同）；
//	② 无相关授权账号（sales，sample.* 全 NONE）⇒ 403，point=sample.retain.in；
//	③ 未登录 ⇒ 401；
//	④ destroy/init 与 destroy/approve 两个动作 POST 守卫不变（动作型点回归）；
//	⑤ 既有用例（TestTC_M4_07_HTTP_DestroyInitApproveSplit 等）不破。
func TestTC_N021_01_销毁记录列表守卫回归(t *testing.T) {
	st := requireDB(t)
	n021Wipe(t, st)
	t.Cleanup(func() { n021Wipe(t, st) })

	// ① 全授权账号：每点最高可授予级别（sample.retain.in ⇒ ALL ⇒ Allows(READ)）
	n021GrantMax(t, st, n021FullRole, "N-021全授权角色")
	bindRole(t, st, n021FullOpenID, n021FullRole)
	// ④ 动作守卫回归用：只有 retain.in=ALL，destroy.init/approve 均无记录
	n021GrantOne(t, st, n021ReadRole, "N-021仅留样读角色",
		"sample.retain.in", "ALL")
	bindRole(t, st, n021ReadOpenID, n021ReadRole)
	// ② deny-by-default：sales 对 sample.* 均 NONE（lend=READ 不覆盖 retain.in）
	bindRole(t, st, "ou_test_n021_sales", "sales")

	e := newEnv(t, time.Hour)

	// ① 全授权账号 ⇒ 200（对照修复前：任何账号都 403）
	fullCK := e.login(n021FullOpenID)
	for _, path := range []string{
		"/api/sample/destroy",
		"/api/sample/destroy?pending=1",
	} {
		resp, body := e.do("GET", path, fullCK, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("全授权账号 GET %s 应 200（N-021 修复点），实际 %d body=%v",
				path, resp.StatusCode, body)
		}
		if _, ok := body["count"]; !ok {
			t.Fatalf("GET %s 响应应含 count，实际 %v", path, body)
		}
	}

	// ② 无相关授权 ⇒ 403，且响应体点名**新挂点**（不是旧的 destroy.init）
	salesCK := e.login("ou_test_n021_sales")
	resp, body := e.do("GET", "/api/sample/destroy?pending=1", salesCK, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sales 账号 GET 销毁列表应 403（deny by default），实际 %d body=%v",
			resp.StatusCode, body)
	}
	if got := fmt.Sprint(body["point"]); got != "sample.retain.in" {
		t.Fatalf("403 响应体 point 应为 sample.retain.in（新挂点），实际 %q body=%v", got, body)
	}
	if got := fmt.Sprint(body["need"]); got != "READ" {
		t.Fatalf("403 响应体 need 应为 READ，实际 %q body=%v", got, body)
	}

	// ③ 未登录 ⇒ 401
	resp, body = e.do("GET", "/api/sample/destroy?pending=1", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录 GET 销毁列表应 401，实际 %d body=%v", resp.StatusCode, body)
	}

	// ④ 动作 POST 守卫不变：仅持有 retain.in=ALL 的账号
	//	⇒ 发起入口仍要 INIT 级、审批入口仍要 APPROVE 级（动作型点，不随本修复放宽）
	readCK := e.login(n021ReadOpenID)
	resp, body = e.do("POST", "/api/sample/destroy/init", readCK, `{"sample_id":1}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("无 INIT 级走发起入口应 403（守卫未放宽），实际 %d body=%v", resp.StatusCode, body)
	}
	if got := fmt.Sprint(body["point"]); got != "sample.retain.destroy.init" {
		t.Fatalf("发起入口 403 point 应为 sample.retain.destroy.init，实际 %q body=%v", got, body)
	}
	if got := fmt.Sprint(body["need"]); got != "INIT" {
		t.Fatalf("发起入口 403 need 应为 INIT，实际 %q body=%v", got, body)
	}
	resp, body = e.do("POST", "/api/sample/destroy/approve", readCK, `{"sample_id":1}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("无 APPROVE 级走审批入口应 403（守卫未放宽），实际 %d body=%v", resp.StatusCode, body)
	}
	if got := fmt.Sprint(body["point"]); got != "sample.retain.destroy.approve" {
		t.Fatalf("审批入口 403 point 应为 sample.retain.destroy.approve，实际 %q body=%v", got, body)
	}
	if got := fmt.Sprint(body["need"]); got != "APPROVE" {
		t.Fatalf("审批入口 403 need 应为 APPROVE，实际 %q body=%v", got, body)
	}

	// 同一账号读列表仍然 200 —— 「读入口放宽 ≠ 动作放宽」
	resp, body = e.do("GET", "/api/sample/destroy", readCK, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("仅 retain.in=ALL 账号 GET 销毁列表应 200，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TestTC_N021_02_路由表GET读入口审计 —— 任务包 §6-2：
//
//	从 **echo 路由表** 逐条驱动（不是源码 grep 拼串），断言**没有任何 GET 入口**
//	被挂成「矩阵里配不出任何能读它的角色」（即 INIT/APPROVE 型点守 GET 这类缺陷）。
//
// 模型：造一个「每点取最高可授予级别」的角色（ALL > READ > INIT > APPROVE）。
//
//	GET 若连它都 403（响应体带 point）⇒ 该守卫要求的级别高于该点可授予的最高级别
//	⇒ 全仓任何角色都读不到 ⇒ 判红。
func TestTC_N021_02_路由表GET读入口审计(t *testing.T) {
	st := requireDB(t)
	n021Wipe(t, st)
	t.Cleanup(func() { n021Wipe(t, st) })

	n021GrantMax(t, st, n021AuditRole, "N-021路由表审计角色")
	bindRole(t, st, n021AuditOpenID, n021AuditRole)

	e := newEnv(t, time.Hour)
	ck := e.login(n021AuditOpenID)

	// 路由表：直接从 echo 的 Route 表取（与线上装配的是同一张表）
	handler := New(e.cfg, e.st, "test").Handler()
	paramRe := regexp.MustCompile(`:[A-Za-z0-9_]+`)

	var probed, blocked []string
	for _, r := range handler.Routes() {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.Path, "/api/") {
			continue
		}
		path := paramRe.ReplaceAllString(r.Path, "0")
		probed = append(probed, r.Method+" "+r.Path)
		resp, body := e.do("GET", path, ck, "")
		// 守卫 401 ⇒ 主体没装载上（测试基座问题，单独报）
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("GET %s 对已登录审计账号返回 401（会话/主体装载异常）body=%v", path, body)
			continue
		}
		// 权限守卫拒绝（响应体带 point 字段）⇒ 该读入口不可达
		if resp.StatusCode == http.StatusForbidden {
			if _, isGuard := body["point"]; isGuard {
				blocked = append(blocked, fmt.Sprintf("%s ⇒ point=%v need=%v",
					r.Path, body["point"], body["need"]))
			}
		}
	}

	if len(probed) < 60 {
		t.Fatalf("路由表 GET(/api/*) 只取到 %d 条（预期 ≥60）——Route 表解析失效会静默放过缺陷： %v",
			len(probed), probed)
	}
	sort.Strings(blocked)
	if len(blocked) > 0 {
		t.Fatalf("以下 GET 入口连「每点最高可授予级别」的角色都读不到"+
			"（动作型/无 READ 级别点守 GET ⇒ 全仓 403，N-021 同类错挂）:\n  %s",
			strings.Join(blocked, "\n  "))
	}
	t.Logf("N-021 路由表审计通过：%d 条 GET(/api/*) 全部可达", len(probed))
}
