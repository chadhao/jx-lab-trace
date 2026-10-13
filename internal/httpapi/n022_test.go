package httpapi

// ===== N-022 · GET /api/admin/users 账号列表（MIMO-NEXT-FIX-06 D2/D3）=====
//
// ★ 灵魂：**无角色账号也必须出现在列表里** —— 既有绑定视图 GET /api/admin/user-roles
//	以 s_user_role 为驱动表，没绑过角色的账号根本查不到；而管理员要绑的恰恰是他们
//	（N-022 起因：同步进来的新人没有 open_id 手填线索 ⇒ 无法绑角色）。
// ★ 形状：每行含 open_id / name / status / roles 数组；未绑角色的账号排前面。
// ★ 权限：复用 sys.user.manage（sales 种子 = NONE ⇒ 403）；未登录 ⇒ 401。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	n022AdminOpenID = "ou_test_n022_admin" // 绑 sysadmin ⇒ sys.user.manage=ALL ⇒ 可读
	n022BoundOpenID = "ou_test_n022_bound" // 绑 sales（已绑角色的账号）
	n022FreeOpenID  = "ou_test_n022_free"  // 无任何角色 —— 灵魂
	n022SalesOpenID = "ou_test_n022_sales" // 绑 sales ⇒ sys.user.manage=NONE ⇒ 403
)

// n022Wipe 清掉本用例族的账号与绑定（只动 ou_test_n022_%）。
func n022Wipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM s_user_role WHERE open_id LIKE 'ou\_test\_n022\_%'`,
		`DELETE FROM s_session WHERE open_id LIKE 'ou\_test\_n022\_%'`,
		`DELETE FROM m_user WHERE open_id LIKE 'ou\_test\_n022\_%'`,
	} {
		if _, err := st.DB().ExecContext(ctx, q); err != nil {
			t.Logf("N-022 清理失败（忽略，继续）: %v", err)
		}
	}
}

// n022Seed 造 4 个账号：管理员（sysadmin）/ 已绑（sales）/ **无角色** / 仅 sales（无管理权）。
func n022Seed(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	ins := func(openID, name string) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_user (open_id, name, status, created_by)
VALUES (?, ?, '启用', 'n022_test')
ON DUPLICATE KEY UPDATE name = VALUES(name), status = '启用'`, openID, name); err != nil {
			t.Fatalf("N-022 造账号失败: %v", err)
		}
	}
	ins(n022AdminOpenID, "N-022管理员")
	ins(n022BoundOpenID, "N-022已绑账号")
	ins(n022FreeOpenID, "N-022无角色账号")
	ins(n022SalesOpenID, "N-022销售账号")
	bindRole(t, st, n022AdminOpenID, "sysadmin")
	bindRole(t, st, n022BoundOpenID, "sales")
	bindRole(t, st, n022SalesOpenID, "sales")
}

// rowOf 把一行账号 JSON 转成可断言的形态（缺字段/类型错 ⇒ 报错返回 nil）。
func rowOf(t *testing.T, raw interface{}) map[string]interface{} {
	t.Helper()
	row, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("users 数组元素应为对象，实际 %T: %v", raw, raw)
	}
	return row
}

func strOf(t *testing.T, row map[string]interface{}, key string) string {
	t.Helper()
	v, ok := row[key].(string)
	if !ok {
		t.Fatalf("账号行缺字符串字段 %q（行=%v）", key, row)
	}
	return v
}

// rolesOf 取 roles 数组；**缺失或非数组即判红**（roles 恒在场，空数组 = 无角色）。
func rolesOf(t *testing.T, row map[string]interface{}) []interface{} {
	t.Helper()
	rs, ok := row["roles"].([]interface{})
	if !ok {
		t.Fatalf("账号行缺 roles 数组（无角色账号也必须带空数组 —— N-022 灵魂）行=%v", row)
	}
	return rs
}

// TestTC_N022_01_账号列表形状与无角色账号 —— 任务包 D3 形状断言：
//
//	① 200 且形状成立（count == len(users)；每行 open_id/name/status/roles 齐全）；
//	② **无角色账号出现在列表里且 roles 为空数组**（本需求的灵魂）；
//	③ 已绑账号标注现有角色（bound 行 roles 含 sales）；
//	④ 未绑角色的账号排在已绑的前面。
func TestTC_N022_01_账号列表形状与无角色账号(t *testing.T) {
	st := requireDB(t)
	n022Wipe(t, st)
	t.Cleanup(func() { n022Wipe(t, st) })
	n022Seed(t, st)

	e := newEnv(t, time.Hour)
	ck := e.login(n022AdminOpenID)

	resp, body := e.do("GET", "/api/admin/users", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/users 应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	raw, ok := body["users"].([]interface{})
	if !ok {
		t.Fatalf("响应缺 users 数组: %v", body)
	}
	count, ok := body["count"].(float64)
	if !ok || int(count) != len(raw) {
		t.Fatalf("count 应等于 users 长度，实际 count=%v len=%d body=%v", body["count"], len(raw), body)
	}

	var freeIdx, boundIdx = -1, -1
	seenBound := false
	for i, item := range raw {
		row := rowOf(t, item)
		if id := strOf(t, row, "open_id"); id == "" {
			t.Fatalf("第 %d 行 open_id 为空: %v", i, row)
		}
		_ = strOf(t, row, "name")
		_ = strOf(t, row, "status")
		roles := rolesOf(t, row)

		// ④ 未绑在前：一旦见到已绑行，其后不得再出现无角色行
		if len(roles) > 0 {
			seenBound = true
		} else if seenBound {
			t.Fatalf("未绑角色的账号排在已绑账号之后（顺序错）：第 %d 行 %v", i, row)
		}

		switch strOf(t, row, "open_id") {
		case n022FreeOpenID:
			freeIdx = i
			if len(roles) != 0 {
				t.Fatalf("无角色账号应 roles=[]（灵魂），实际 %v", roles)
			}
		case n022BoundOpenID:
			boundIdx = i
			found := false
			for _, r := range roles {
				if r == "sales" {
					found = true
				}
			}
			if !found {
				t.Fatalf("已绑账号应标注现有角色 sales，实际 %v", roles)
			}
		}
	}

	// ② 灵魂：无角色账号在列（驱动表是 m_user，不是 s_user_role）
	if freeIdx < 0 {
		t.Fatalf("无角色账号 %s 未出现在列表（灵魂断言失败），users=%v", n022FreeOpenID, raw)
	}
	if boundIdx < 0 {
		t.Fatalf("已绑账号 %s 未出现在列表，users=%v", n022BoundOpenID, raw)
	}
	if freeIdx > boundIdx {
		t.Fatalf("未绑角色账号应排在已绑账号前面：free=%d bound=%d", freeIdx, boundIdx)
	}
}

// TestTC_N022_02_账号列表权限 —— 未登录 401；无 sys.user.manage 授权 403（点名挂点）。
func TestTC_N022_02_账号列表权限(t *testing.T) {
	st := requireDB(t)
	n022Wipe(t, st)
	t.Cleanup(func() { n022Wipe(t, st) })
	n022Seed(t, st)

	e := newEnv(t, time.Hour)

	// 未登录 ⇒ 401
	resp, body := e.do("GET", "/api/admin/users", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录 GET /api/admin/users 应 401，实际 %d body=%v", resp.StatusCode, body)
	}

	// sales（sys.user.manage = NONE）⇒ 403，响应体点名挂点（复用点可审计）
	salesCK := e.login(n022SalesOpenID)
	resp, body = e.do("GET", "/api/admin/users", salesCK, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("无 sys.user.manage 授权应 403，实际 %d body=%v", resp.StatusCode, body)
	}
	if got := strOrAny(t, body, "point"); got != "sys.user.manage" {
		t.Fatalf("403 响应体 point 应为 sys.user.manage，实际 %q body=%v", got, body)
	}
}

// strOrAny 取响应体里的字符串字段（取不到返回空串，交给上层判红）。
func strOrAny(t *testing.T, body map[string]interface{}, key string) string {
	t.Helper()
	if v, ok := body[key].(string); ok {
		return v
	}
	return ""
}
