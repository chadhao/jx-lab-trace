package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/permission"
)

// ===== M2 权限配置 · 接口级 TC（A5–A10）=====
//
// ★ 两个账号：系统管理员（写矩阵）与 收货员（被改权限的一方）。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。

const (
	m2AdminID = "ou_test_m2_admin"
	m2RecvID  = "ou_test_m2_recv"
	m2UserID  = "ou_test_m2_user"
)

func newM2Env(t *testing.T) *env {
	t.Helper()
	st := requireDB(t)
	bindRoles(t, st, m2AdminID, "sysadmin")
	bindRoles(t, st, m2RecvID, "receiver")
	e := newEnv(t, time.Hour)
	// ★ 自愈：本文件会动到这几个格子；**一旦某个断言失败**（例如防锁死被改坏），
	//	留下脏值会让后续用例连锁失败、污染「哪条红」的判断 ⇒ 每例收尾按种子归位。
	t.Cleanup(func() { restoreSeedCells(e) })
	return e
}

// restoreSeedCells 按 spec/permission-points.json 的种子值归位本文件会改的格子。
// ★ 走库层而不是接口：万一 sysadmin 自己失权（正是本文件要防的事故），
//
//	接口就用不了了，兜底必须不依赖权限。
func restoreSeedCells(e *env) {
	seeds := []struct{ role, point, level string }{
		{"sysadmin", "sys.perm.edit", "ALL"},
		{"receiver", "md.vehicle", "ALL"},
		{"receiver", "md.team", "READ"},
		{"receiver", "recv.label.print", "ALL"},
	}
	for _, s := range seeds {
		_, _ = e.st.DB().ExecContext(context.Background(),
			`UPDATE s_role_permission SET level=? WHERE role_code=? AND point_code=? AND level<>?`,
			s.level, s.role, s.point, s.level)
	}
}

// saveMatrix 走 PUT /api/admin/permission-matrix。
func saveMatrix(t *testing.T, e *env, ck, payload string) (*http.Response, map[string]interface{}) {
	t.Helper()
	return e.do("PUT", "/api/admin/permission-matrix", ck, payload)
}

// levelOf 直读引擎口径的级别（**无缓存** ⇒ 正是「立即生效」要验的东西）。
func levelOf(t *testing.T, e *env, openID string, code permission.Code) string {
	t.Helper()
	lv, err := e.st.LevelsFor(context.Background(), openID, code)
	if err != nil {
		t.Fatalf("查询级别失败: %v", err)
	}
	if !lv.Granted() {
		return "NONE"
	}
	return strings.Join([]string(lv), "+")
}

// TC-M2-01 正常：把「标签打印」从收货员 ALL 改为 NONE 并保存 ⇒ 该角色账号立刻不可用。
//
// ★ 标签打印属 M3（批 3 未建），故引擎口径用 permission.RecvLabelPrint 直断言；
//
//	另用**真实入口**（receiver 的 md.vehicle）走一遍 HTTP 200→403，证明「行为立即变化」
//	不是只改了一行数据。
func TestTC_M2_01_MatrixChangeTakesEffectImmediately(t *testing.T) {
	e := newM2Env(t)
	adminCK := e.login(m2AdminID)
	recvCK := e.login(m2RecvID)

	// 还原种子值（测试可重复跑）
	t.Cleanup(func() {
		_, _ = saveMatrix(t, e, adminCK,
			`{"changes":[`+
				`{"role_code":"receiver","point_code":"recv.label.print","level":"ALL"},`+
				`{"role_code":"receiver","point_code":"md.vehicle","level":"ALL"}],`+
				`"reason":"测试后还原种子值"}`)
	})

	// ---- ① 引擎口径：标签打印 ALL → NONE ----
	if got := levelOf(t, e, m2RecvID, permission.RecvLabelPrint); got == "NONE" {
		t.Fatalf("前置失败：收货员在标签打印上应为 ALL，实际 %s", got)
	}
	resp, body := saveMatrix(t, e, adminCK,
		`{"changes":[{"role_code":"receiver","point_code":"recv.label.print","level":"NONE"}],`+
			`"reason":"TC-M2-01 收货员暂不打印标签"}`)
	mustOK(t, resp, body, "保存矩阵")
	if numOf(body["changed"]) != 1 {
		t.Fatalf("应记录 1 条 diff，实际 %v", body)
	}
	if got := levelOf(t, e, m2RecvID, permission.RecvLabelPrint); got != "NONE" {
		t.Fatalf("保存后应**立即**变为 NONE（判定无缓存），实际 %s", got)
	}

	// ---- ② 真实入口口径：md.vehicle ALL → NONE ⇒ HTTP 由 200 变 403 ----
	resp, body = e.do("GET", "/api/md/vehicles", recvCK, "")
	mustOK(t, resp, body, "改动前收货员应能查车辆")
	resp, body = saveMatrix(t, e, adminCK,
		`{"changes":[{"role_code":"receiver","point_code":"md.vehicle","level":"NONE"}],`+
			`"reason":"TC-M2-01 真实入口对照"}`)
	mustOK(t, resp, body, "保存矩阵")
	resp, body = e.do("GET", "/api/md/vehicles", recvCK, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("改一格后该账号行为应**立即**变化（200→403），实际 %d body=%v", resp.StatusCode, body)
	}
}

// TC-M2-02 边界：一批改动里混入非法项 ⇒ **整批拒绝、无部分生效**（等价于「取消保存」）。
func TestTC_M2_02_NoPartialApply(t *testing.T) {
	e := newM2Env(t)
	adminCK := e.login(m2AdminID)

	// 前置：先把两条都拉回种子值（不靠“假设别人没动过”，也不 Skip）
	resp0, body0 := saveMatrix(t, e, adminCK,
		`{"changes":[`+
			`{"role_code":"receiver","point_code":"md.vehicle","level":"ALL"},`+
			`{"role_code":"sysadmin","point_code":"sys.perm.edit","level":"ALL"}],`+
			`"reason":"TC-M2-02 前置归位"}`)
	mustOK(t, resp0, body0, "前置归位")
	if got := levelOf(t, e, m2RecvID, permission.MdVehicle); got == "NONE" {
		t.Fatalf("前置失败：receiver 的 md.vehicle 应为 ALL，实际 %s", got)
	}
	if got := levelOf(t, e, m2AdminID, permission.SysPermEdit); got == "NONE" {
		t.Fatal("前置失败：sysadmin 的 sys.perm.edit 应为 ALL")
	}

	resp, body := saveMatrix(t, e, adminCK,
		`{"changes":[`+
			`{"role_code":"receiver","point_code":"md.vehicle","level":"NONE"},`+
			`{"role_code":"sysadmin","point_code":"sys.perm.edit","level":"NONE"}],`+
			`"reason":"TC-M2-02 半批非法"}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("含防锁死违规的批次必须被拒，实际 200 body=%v", body)
	}

	// ★ 关键断言：合法的那一条**没有**被部分应用
	if got := levelOf(t, e, m2RecvID, permission.MdVehicle); got == "NONE" {
		t.Fatal("整批被拒时不得有任何一格生效（无部分生效）")
	}
	if got := levelOf(t, e, m2AdminID, permission.SysPermEdit); got == "NONE" {
		t.Fatal("sysadmin 的管理域权限不得被改动")
	}
}

// TC-M2-03 异常：尝试删除内置角色 ⇒ 被拒。
func TestTC_M2_03_BuiltinRoleCannotBeDeleted(t *testing.T) {
	e := newM2Env(t)
	adminCK := e.login(m2AdminID)

	resp, body := e.do("DELETE", "/api/admin/roles/qc", adminCK, "")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("内置角色不得被删除，实际 200 body=%v", body)
	}
	msg := fmt.Sprint(body)
	if !strings.Contains(msg, "内置角色") {
		t.Fatalf("拒绝理由应点明「内置角色不可删除」，实际 %v", body)
	}
	// 角色仍在
	resp, body = e.do("GET", "/api/admin/roles", adminCK, "")
	mustOK(t, resp, body, "查角色")
	roles, _ := body["roles"].([]interface{})
	found := false
	for _, r := range roles {
		m, _ := r.(map[string]interface{})
		if fmt.Sprint(m["code"]) == "qc" {
			found = true
		}
	}
	if !found {
		t.Fatal("内置角色 qc 必须仍在")
	}
}

// TC-M2-04 正常：从「质检员」复制权限新建角色 ⇒ 权限一致，且此后互不影响。
func TestTC_M2_04_CopyPermissionsThenIndependent(t *testing.T) {
	e := newM2Env(t)
	adminCK := e.login(m2AdminID)
	const newRole = "qc_assistant"
	t.Cleanup(func() { _, _ = e.do("DELETE", "/api/admin/roles/"+newRole, adminCK, "") })

	resp, body := e.do("POST", "/api/admin/roles", adminCK,
		`{"code":"`+newRole+`","name":"质检助理","copy_from":"qc","reason":"TC-M2-04"}`)
	mustOK(t, resp, body, "新建角色")

	grantsOf := func(code string) map[string]string {
		t.Helper()
		m, err := e.st.GetPermissionMatrix(context.Background())
		if err != nil {
			t.Fatalf("取矩阵失败: %v", err)
		}
		out := map[string]string{}
		for _, c := range m.Cells {
			if c.RoleCode == code {
				out[c.PointCode] = c.Level
			}
		}
		return out
	}
	src, dst := grantsOf("qc"), grantsOf(newRole)
	if len(src) != len(dst) {
		t.Fatalf("复制后授权数应一致：%d vs %d", len(src), len(dst))
	}
	for k, v := range src {
		if dst[k] != v {
			t.Fatalf("复制后 %s 应与 qc 一致：%q vs %q", k, v, dst[k])
		}
	}

	// 改新角色的一个点 ⇒ qc 不受影响
	resp, body = saveMatrix(t, e, adminCK,
		`{"changes":[{"role_code":"`+newRole+`","point_code":"md.team","level":"NONE"}],`+
			`"reason":"TC-M2-04 互不影响"}`)
	mustOK(t, resp, body, "改新角色权限")
	if got := grantsOf("qc")["md.team"]; got == "NONE" {
		t.Fatal("改了新角色不应影响源角色（复制后各自独立）")
	}
	if got := grantsOf(newRole)["md.team"]; got != "NONE" {
		t.Fatalf("新角色应已改为 NONE，实际 %s", got)
	}
}

// TC-M2-05 正常：同一 open_id 绑两个角色 ⇒ 并存（并集）；重复绑定 ⇒ 提示已被占用。
func TestTC_M2_05_MultiRoleBinding(t *testing.T) {
	e := newM2Env(t)
	adminCK := e.login(m2AdminID)
	t.Cleanup(func() {
		_, _ = e.do("DELETE", "/api/admin/user-roles/"+m2UserID+"/management", adminCK, "")
		_, _ = e.do("DELETE", "/api/admin/user-roles/"+m2UserID+"/sysadmin", adminCK, "")
	})

	for _, role := range []string{"management", "sysadmin"} {
		resp, body := e.do("POST", "/api/admin/user-roles", adminCK,
			`{"open_id":"`+m2UserID+`","role_code":"`+role+`","reason":"TC-M2-05"}`)
		mustOK(t, resp, body, "绑定角色 "+role)
	}

	resp, body := e.do("GET", "/api/admin/user-roles/"+m2UserID, adminCK, "")
	mustOK(t, resp, body, "查绑定")
	raw, _ := body["roles"].([]interface{})
	got := map[string]bool{}
	for _, r := range raw {
		got[fmt.Sprint(r)] = true
	}
	if !got["management"] || !got["sysadmin"] {
		t.Fatalf("两个角色应并存，实际 %v", body)
	}

	// 重复绑定 ⇒ 409 已被占用
	resp, body = e.do("POST", "/api/admin/user-roles", adminCK,
		`{"open_id":"`+m2UserID+`","role_code":"management"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复绑定应 409，实际 %d body=%v", resp.StatusCode, body)
	}
	if !strings.Contains(fmt.Sprint(body), "占用") {
		t.Fatalf("提示应说明「已被占用」，实际 %v", body)
	}
}

// TC-M2-06 ★★ 异常：绕过前端直接调接口改 sysadmin 的管理域权限 ⇒ **后端拒绝**。
func TestTC_M2_06_DeadlockGuardRejectsDirectAPICall(t *testing.T) {
	e := newM2Env(t)
	adminCK := e.login(m2AdminID)
	ctx := context.Background()

	before, err := e.st.LevelsFor(ctx, m2AdminID, permission.SysPermEdit)
	if err != nil || !before.Granted() {
		t.Fatalf("前置失败：sysadmin 在 sys.perm.edit 上应为 ALL（%v / %v）", before, err)
	}

	// 直接打接口（没有任何前端置灰参与）
	resp, body := saveMatrix(t, e, adminCK,
		`{"changes":[{"role_code":"sysadmin","point_code":"sys.perm.edit","level":"NONE"}],`+
			`"reason":"TC-M2-06 试图锁死"}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("后端必须拒绝（不能只靠前端置灰），实际 200 body=%v", body)
	}
	if !strings.Contains(fmt.Sprint(body), "防锁死") {
		t.Fatalf("拒绝理由应点明防锁死，实际 %d %v", resp.StatusCode, body)
	}

	after, err := e.st.LevelsFor(ctx, m2AdminID, permission.SysPermEdit)
	if err != nil || !after.Granted() {
		t.Fatalf("拒绝后系统仍须可管理：sys.perm.edit=%v err=%v", after, err)
	}
	// 直连库再确认
	var lvl string
	if err := e.st.DB().QueryRowContext(ctx,
		`SELECT level FROM s_role_permission WHERE role_code='sysadmin' AND point_code='sys.perm.edit'`).
		Scan(&lvl); err != nil {
		t.Fatalf("读库失败: %v", err)
	}
	if lvl != "ALL" {
		t.Fatalf("库内 sysadmin×sys.perm.edit 应保持 ALL，实际 %s", lvl)
	}
}

// TC-M2-07 正常：改权限后查审计日志 ⇒ 有 diff（谁/何时/角色/权限点/旧值→新值）。
func TestTC_M2_07_AuditHasDiff(t *testing.T) {
	e := newM2Env(t)
	adminCK := e.login(m2AdminID)

	resp, body := saveMatrix(t, e, adminCK,
		`{"changes":[{"role_code":"receiver","point_code":"md.team","level":"NONE"}],`+
			`"reason":"TC-M2-07 审计验证"}`)
	mustOK(t, resp, body, "保存矩阵")
	t.Cleanup(func() {
		_, _ = saveMatrix(t, e, adminCK,
			`{"changes":[{"role_code":"receiver","point_code":"md.team","level":"READ"}],`+
				`"reason":"测试后还原种子值"}`)
	})

	resp, body = e.do("GET", "/api/admin/audit-log", adminCK, "")
	mustOK(t, resp, body, "查审计日志")
	rows, _ := body["rows"].([]interface{})
	var hit map[string]interface{}
	for _, r := range rows {
		m, _ := r.(map[string]interface{})
		if fmt.Sprint(m["entity"]) == "s_role_permission" &&
			fmt.Sprint(m["action"]) == "perm_change" &&
			strings.Contains(fmt.Sprint(m["field"]), "receiver") &&
			strings.Contains(fmt.Sprint(m["field"]), "md.team") &&
			fmt.Sprint(m["new_value"]) == "NONE" {
			hit = m
			break
		}
	}
	if hit == nil {
		t.Fatalf("审计里应有该笔权限改动的 diff 记录（最近 %v 行）", body["count"])
	}
	if fmt.Sprint(hit["old_value"]) != "READ" {
		t.Fatalf("diff 旧值应为 READ，实际 %v", hit["old_value"])
	}
	if fmt.Sprint(hit["actor_open_id"]) != m2AdminID {
		t.Fatalf("审计应记录操作者 %s，实际 %v", m2AdminID, hit["actor_open_id"])
	}
	if fmt.Sprint(hit["reason"]) != "TC-M2-07 审计验证" {
		t.Fatalf("审计应记录原因，实际 %v", hit["reason"])
	}
}

// 防锁死③：系统保持至少一个管理员账号（解绑最后一个 / 停用管理员角色都必须被拒）。
//
// ★ 制造「最后一个管理员」的办法：把**其余**管理员账号的 m_user 置为「停用」
//
//	（判定口径 = 启用用户 ∩ 启用 sysadmin 角色），测试结束立即还原 ——
//	不改任何绑定关系，因此即使中断也不丢数据。
func TestM2_LastSysAdminCannotBeUnbound(t *testing.T) {
	e := newM2Env(t)
	adminCK := e.login(m2AdminID)
	ctx := context.Background()

	// ① 停用「其余」管理员，制造最后一个管理员场景
	rows, err := e.st.DB().QueryContext(ctx,
		`SELECT DISTINCT open_id FROM s_user_role WHERE role_code='sysadmin' AND open_id <> ?`,
		m2AdminID)
	if err != nil {
		t.Fatalf("查询其它管理员失败: %v", err)
	}
	var disabled []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("读取失败: %v", err)
		}
		if _, err := e.st.DB().ExecContext(ctx,
			`UPDATE m_user SET status='停用' WHERE open_id = ? AND status='启用'`, id); err == nil {
			disabled = append(disabled, id)
		}
	}
	rows.Close()
	t.Cleanup(func() {
		for _, id := range disabled {
			_, _ = e.st.DB().ExecContext(ctx, `UPDATE m_user SET status='启用' WHERE open_id = ?`, id)
		}
	})

	// ② 解绑最后一个管理员 ⇒ 必须被拒
	resp, body := e.do("DELETE", "/api/admin/user-roles/"+m2AdminID+"/sysadmin", adminCK, "")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("解绑最后一个系统管理员必须被拒，实际 200 body=%v", body)
	}
	if !strings.Contains(fmt.Sprint(body), "管理员") {
		t.Fatalf("拒绝理由应点明「至少保留一个管理员」，实际 %v", body)
	}

	// ③ 停用 sysadmin 角色 ⇒ 同样必须被拒（否则全体管理员立刻失权）
	resp, body = e.do("PATCH", "/api/admin/roles/sysadmin", adminCK,
		`{"status":"停用","reason":"TC 防锁死"}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("停用 sysadmin 角色必须被拒，实际 200 body=%v", body)
	}
	var st string
	if err := e.st.DB().QueryRowContext(ctx,
		`SELECT status FROM s_role WHERE code='sysadmin'`).Scan(&st); err != nil || st != "启用" {
		t.Fatalf("sysadmin 角色必须保持启用（status=%s err=%v）", st, err)
	}
}

// 权限点字典只读：**没有**新增权限点的入口（A8 口径本批继续成立）。
func TestM2_PermissionPointsStillNotCreatable(t *testing.T) {
	e := newM2Env(t)
	adminCK := e.login(m2AdminID)

	resp, body := e.do("POST", "/api/admin/permission-points", adminCK,
		`{"code":"md.fake","name":"假权限点"}`)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("新增权限点应 405，实际 %d body=%v", resp.StatusCode, body)
	}
	// 字典外 code 仍 404（PATCH 路径）
	resp, body = e.do("PATCH", "/api/admin/permission-points/md.fake", adminCK,
		`{"status":"启用"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("字典外 code 应 404，实际 %d body=%v", resp.StatusCode, body)
	}
}
