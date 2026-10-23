package httpapi

// ===== N-030 · /api/me 增加 permissions 聚合字段（MIMO-NEXT-FIX-14）=====
//
// ★ 任务包 D3 五条 TC：
//	① sysadmin 会话 ⇒ permissions 聚合其全部非 NONE 授权（矩阵后台可配 ⇒
//	  按库内实况推导集合相等性；种子下 sysadmin 仅 5 个管理域点，
//	  全授权矩阵下为 51 —— 不钉死格子值，钉聚合口径本身）
//	② 仅 production 角色 ⇒ 生产域子集（sample.take/prod.feed.scan ∈；
//	  ship.load.scan ∉ —— 按矩阵现值断言；rpt.view 等随库内实况）
//	③ 停用角色口径：停用角色的授权不计入（与 RolesOf 一致），
//	  启用后立即生效（无缓存）
//	④ 无角色 ⇒ "permissions":[]（非 null）
//	⑤ 形状回归：open_id/name/roles/sys_roles 原样 + permissions 为
//	  string 数组且按 code 字典序稳定排序（Web 端既有用例全绿另证零影响）
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	n030SysOpenID  = "ou_test_n030_sys"
	n030ProdOpenID = "ou_test_n030_prod"
	n030NoneOpenID = "ou_test_n030_none"
	n030MixOpenID  = "ou_test_n030_mix"
	n030OffRole    = "tr_n030_off"
)

// n030Wipe 清本用例族数据（只动 tr_n030_% / ou_test_n030_%）。
func n030Wipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	q := func(sql string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sql, args...); err != nil {
			t.Logf("n030 清理失败（忽略，继续）: %v", err)
		}
	}
	q(`DELETE FROM s_role_permission WHERE role_code LIKE 'tr\_n030\_%'`)
	q(`DELETE FROM s_user_role WHERE role_code LIKE 'tr\_n030\_%'`)
	q(`DELETE FROM s_role WHERE code LIKE 'tr\_n030\_%'`)
	q(`DELETE FROM s_user_role WHERE open_id LIKE 'ou\_test\_n030\_%'`)
}

// n030Me 取 /api/me 响应体（要求 200）。
func n030Me(t *testing.T, e *env, ck string) map[string]interface{} {
	t.Helper()
	resp, body := e.do("GET", "/api/me", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me 应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	return body
}

// n030Perms 从响应体取出 permissions（断言键存在且为 []interface{}，非 null）。
func n030Perms(t *testing.T, body map[string]interface{}) []string {
	t.Helper()
	raw, ok := body["permissions"]
	if !ok {
		t.Fatalf("★ /api/me 缺 permissions 字段（body=%v）", body)
	}
	list, ok := raw.([]interface{})
	if !ok || list == nil {
		t.Fatalf("★ permissions 必须是 JSON 数组且非 null（%T），实际 %v", raw, raw)
	}
	out := make([]string, 0, len(list))
	for _, it := range list {
		s, ok := it.(string)
		if !ok {
			t.Fatalf("★ permissions 元素必须是字符串，实际 %v", it)
		}
		out = append(out, s)
	}
	return out
}

// n030Expected 按库内实况独立复算期望集合（写法刻意不同于
// store.PermissionCodesFor 的 JOIN 形式，用 IN 子查询 —— 避免同形 bug
// 两边一起错的假绿）。
func n030Expected(t *testing.T, st *store.Store, openID string) []string {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(), `
		SELECT point_code FROM s_role_permission
		 WHERE level <> 'NONE'
		   AND role_code IN (
		         SELECT role_code FROM s_user_role WHERE open_id = ?
		           AND role_code IN (SELECT code FROM s_role WHERE status = '启用')
		       )
		 GROUP BY point_code
		 ORDER BY point_code`, openID)
	if err != nil {
		t.Fatalf("复算期望权限集合失败: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("读期望集合失败: %v", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历期望集合失败: %v", err)
	}
	return out
}

// n030AssertSetEqual 断言实际与期望集合相等且按字典序排序（契约确定性）。
func n030AssertSetEqual(t *testing.T, when string, got, want []string) {
	t.Helper()
	if !sort.StringsAreSorted(got) {
		t.Fatalf("★ %s：permissions 未按 code 字典序排序：%v", when, got)
	}
	if len(got) != len(want) {
		t.Fatalf("★ %s：permissions 数量不符：实际 %d %v，期望 %d %v",
			when, len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("★ %s：permissions[%d]=%q，期望 %q（实际=%v 期望=%v）",
				when, i, got[i], want[i], got, want)
		}
	}
}

func n030Contains(list []string, code string) bool {
	for _, c := range list {
		if c == code {
			return true
		}
	}
	return false
}

// TC-N030-01 sysadmin 聚合 —— 集合与库内实况相等；种子锁定的 5 个系统域点恒 ∈。
// （「全部 51 点」以全授权矩阵为前提 —— 矩阵后台可配，见 N-029 验收教训；
// 本条钉聚合口径：全部启用角色 × 非 NONE 授权的并集去重。）
func TestTC_N030_01_SysadminAggregation(t *testing.T) {
	st := requireDB(t)
	n030Wipe(t, st)
	t.Cleanup(func() { n030Wipe(t, st) })
	bindRole(t, st, n030SysOpenID, "sysadmin")
	e := newEnv(t, time.Hour)

	body := n030Me(t, e, e.login(n030SysOpenID))
	got := n030Perms(t, body)
	want := n030Expected(t, st, n030SysOpenID)
	n030AssertSetEqual(t, "sysadmin", got, want)

	// 种子锁定：sysadmin 管理域 5 点恒为 ALL（permadmin 禁改），必 ∈
	for _, code := range []string{
		"sys.user.manage", "sys.perm.edit", "sys.audit.view",
		"sys.code.rule", "sys.param.edit",
	} {
		if !n030Contains(got, code) {
			t.Fatalf("★ sysadmin permissions 应含管理域点 %q，实际 %v", code, got)
		}
	}
	// 全授权矩阵下即为 51（任务包 D3-1 语义）；种子矩阵下为 5 —— 两者都合法
	if len(got) == 51 {
		t.Logf("矩阵为全授权态：sysadmin 聚合 %d 点（含业务域）", len(got))
	}
}

// TC-N030-02 production 子集 —— 生产域 ∈ / 发货域 ∉（按矩阵现值）；
// 集合仍与库内实况相等（钉映射不钉格子值）。
func TestTC_N030_02_ProductionSubset(t *testing.T) {
	st := requireDB(t)
	n030Wipe(t, st)
	t.Cleanup(func() { n030Wipe(t, st) })
	bindRole(t, st, n030ProdOpenID, "production")
	e := newEnv(t, time.Hour)

	got := n030Perms(t, n030Me(t, e, e.login(n030ProdOpenID)))
	n030AssertSetEqual(t, "production", got, n030Expected(t, st, n030ProdOpenID))

	// 种子矩阵现值断言（production 的生产域 ALL、发货域 NONE —— 业务域不太可能被重配）
	for _, code := range []string{"sample.take", "prod.feed.scan"} {
		if !n030Contains(got, code) {
			t.Fatalf("★ production permissions 应含生产域点 %q，实际 %v", code, got)
		}
	}
	if n030Contains(got, "ship.load.scan") {
		t.Fatalf("★ production 对 ship.load.scan 应为 NONE（发货域不属生产班组），实际却在 %v", got)
	}
	// production 不得越权拿到管理域锁定点（种子 sys.* 对业务角色恒 NONE）
	if n030Contains(got, "sys.perm.edit") {
		t.Fatalf("★ production 不应含 sys.perm.edit，实际 %v", got)
	}
}

// TC-N030-03 停用角色口径 —— 停用角色的授权不计入；启用后立即生效（无缓存）。
func TestTC_N030_03_DisabledRoleExcluded(t *testing.T) {
	st := requireDB(t)
	n030Wipe(t, st)
	t.Cleanup(func() { n030Wipe(t, st) })
	ctx := context.Background()

	// 造一停用角色：仅授 recv.arrive.confirm=ALL（production 对该点为 NONE ⇒ 区分度明确）
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO s_role (code, name, kind, is_system, status)
VALUES (?, 'N-030停用角色', 'business', 0, '停用')`, n030OffRole); err != nil {
		t.Fatalf("建停用角色失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO s_role_permission (role_code, point_code, level, updated_by, created_by)
VALUES (?, 'recv.arrive.confirm', 'ALL', 'test', 'test')`, n030OffRole); err != nil {
		t.Fatalf("停用角色授权失败: %v", err)
	}
	bindRole(t, st, n030MixOpenID, "production")
	bindRole(t, st, n030MixOpenID, n030OffRole)

	e := newEnv(t, time.Hour)
	ck := e.login(n030MixOpenID)

	// ① 停用态：仅聚合启用的 production —— recv.arrive.confirm ∉
	got := n030Perms(t, n030Me(t, e, ck))
	n030AssertSetEqual(t, "停用态", got, n030Expected(t, st, n030MixOpenID))
	if n030Contains(got, "recv.arrive.confirm") {
		t.Fatalf("★ 停用角色的授权不得计入 permissions（实际 %v）", got)
	}

	// ② 启用该角色（模拟后台改矩阵）⇒ 下一次请求立即生效（判定无缓存）
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE s_role SET status = '启用' WHERE code = ?`, n030OffRole); err != nil {
		t.Fatalf("启用角色失败: %v", err)
	}
	got = n030Perms(t, n030Me(t, e, ck))
	n030AssertSetEqual(t, "启用后", got, n030Expected(t, st, n030MixOpenID))
	if !n030Contains(got, "recv.arrive.confirm") {
		t.Fatalf("★ 角色启用后其授权应计入 permissions（实际 %v）", got)
	}

	// ③ 再停用 ⇒ 立即回退（口径对称）
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE s_role SET status = '停用' WHERE code = ?`, n030OffRole); err != nil {
		t.Fatalf("停用角色失败: %v", err)
	}
	got = n030Perms(t, n030Me(t, e, ck))
	if n030Contains(got, "recv.arrive.confirm") {
		t.Fatalf("★ 角色再停用后授权应立即失效（实际 %v）", got)
	}
}

// TC-N030-04 无角色 ⇒ "permissions":[]（非 null）。
func TestTC_N030_04_NoRoleEmptyArray(t *testing.T) {
	st := requireDB(t)
	n030Wipe(t, st)
	t.Cleanup(func() { n030Wipe(t, st) })
	e := newEnv(t, time.Hour)

	ck := e.login(n030NoneOpenID)
	resp, body := e.do("GET", "/api/me", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me 应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	got := n030Perms(t, body)
	if len(got) != 0 {
		t.Fatalf("★ 无角色账号 permissions 应为空数组，实际 %v", got)
	}
	// 原始 JSON 层面钉死 [] 非 null（map 反序列化无法区分 null 与 [] ⇒ 读原文）
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/api/me", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Cookie", ck)
	httpResp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer httpResp.Body.Close()
	orig, err := io.ReadAll(httpResp.Body)
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	if !strings.Contains(string(orig), `"permissions":[]`) {
		t.Fatalf("★ 原始 JSON 应含 \"permissions\":[]（非 null），实际 %s", orig)
	}
}

// TC-N030-05 形状回归 —— 既有四键原样 + permissions 为字典序 string 数组。
func TestTC_N030_05_ShapeRegression(t *testing.T) {
	st := requireDB(t)
	n030Wipe(t, st)
	t.Cleanup(func() { n030Wipe(t, st) })
	bindRole(t, st, n030ProdOpenID, "production")
	e := newEnv(t, time.Hour)

	body := n030Me(t, e, e.login(n030ProdOpenID))

	for _, k := range []string{"open_id", "name", "roles", "sys_roles", "permissions"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("★ 字段 %q 缺失（body=%v）", k, body)
		}
	}
	if body["open_id"] != n030ProdOpenID {
		t.Fatalf("open_id 应为 %q，实际 %v", n030ProdOpenID, body["open_id"])
	}
	sysRoles, _ := body["sys_roles"].([]interface{})
	if len(sysRoles) == 0 {
		t.Fatalf("★ sys_roles 应含 production（登录时聚合），实际 %v", body["sys_roles"])
	}
	// roles 与 sys_roles 的角色集合应一致（同源：登录时 RolesOf）
	roles, _ := body["roles"].([]interface{})
	if len(roles) != len(sysRoles) {
		t.Fatalf("★ roles(%d) 与 sys_roles(%d) 长度不一致（body=%v）",
			len(roles), len(sysRoles), body)
	}

	got := n030Perms(t, body)
	if !sort.StringsAreSorted(got) {
		t.Fatalf("★ permissions 未排序：%v", got)
	}
	// 与 Store 直查一致（handler 挂点没有丢过滤条件）
	n030AssertSetEqual(t, "形状回归", got, n030Expected(t, st, n030ProdOpenID))

	// ⑤ JSON 编码层面：permissions 必须是字符串数组
	if _, err := json.Marshal(map[string]interface{}{"permissions": got}); err != nil {
		t.Fatalf("permissions 应可 JSON 序列化: %v", err)
	}
}
