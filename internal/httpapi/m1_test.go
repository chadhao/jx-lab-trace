package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/store"
)

// ===== M1 主数据 · 接口级 TC（A2/A3/A4）=====
//
// ★ 账号：一个「多角色叠加」的编辑账号（sales + qc + receiver 的并集恰好覆盖
//	md.* 五个点的 ALL），既练了并集，也让各实体都能写。
// ★ 按 docs/05：这些是 DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。

const (
	m1OpenID = "ou_test_m1_editor"
)

// numOf 把 JSON 反序列化后的数字转成 int64（可能是 float64）。
func numOf(v interface{}) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case json.Number:
		i, _ := t.Int64()
		return i
	case int64:
		return t
	default:
		return 0
	}
}

func bindRoles(t *testing.T, st *store.Store, openID string, roles ...string) {
	t.Helper()
	for _, r := range roles {
		if _, err := st.DB().ExecContext(context.Background(),
			`INSERT IGNORE INTO s_user_role (open_id, role_code, granted_by) VALUES (?, ?, 'test')`,
			openID, r); err != nil {
			t.Fatalf("绑定角色 %s 失败: %v", r, err)
		}
	}
}

// wipeMDHTTP 清掉本批测试写入的主数据（跑前跑后各一次，保证可重复执行）。
func wipeMDHTTP(t *testing.T, e *env, tables ...string) {
	t.Helper()
	ctx := context.Background()
	del := func() {
		for _, tb := range tables {
			_, _ = e.st.DB().ExecContext(ctx,
				"DELETE FROM `"+tb+"` WHERE created_by LIKE 'ou\\_test\\_m1%'")
		}
	}
	del()
	t.Cleanup(del)
}

func newM1Env(t *testing.T) *env {
	t.Helper()
	st := requireDB(t)
	bindRoles(t, st, m1OpenID, "sales", "qc", "receiver")
	e := newEnv(t, time.Hour)
	return e
}

func mustOK(t *testing.T, resp *http.Response, body map[string]interface{}, what string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s 应 200，实际 %d body=%v", what, resp.StatusCode, body)
	}
}

// TC-M1-01 正常：新增客户 code=9001（4 位数字）⇒ 成功，可被检索。
// ★ 用 9xxx 作为**测试保留编号段**，避免与真实客户数据撞号（语义仍是「4 位数字、全局唯一」）。
func TestTC_M1_01_HTTP_CreateCustomer(t *testing.T) {
	e := newM1Env(t)
	wipeMDHTTP(t, e, "m_customer")
	ck := e.login(m1OpenID)

	resp, body := e.do("POST", "/api/md/customers", ck,
		`{"values":{"code":"9001","name":"测试客户甲"}}`)
	mustOK(t, resp, body, "新增客户")
	row, _ := body["row"].(map[string]interface{})
	if fmt.Sprint(row["code"]) != "9001" {
		t.Fatalf("code 应为 9001，实际 %v", row["code"])
	}
	if numOf(row["version"]) != 1 {
		t.Fatalf("新建行应为 v1，实际 %v", row["version"])
	}

	resp, body = e.do("GET", "/api/md/customers?q=9001", ck, "")
	mustOK(t, resp, body, "检索客户")
	if numOf(body["count"]) < 1 {
		t.Fatalf("新增客户应可检索到，实际 body=%v", body)
	}
}

// TC-M1-02 异常：再用同一 code 新增 ⇒ 唯一约束拒绝（409）。
func TestTC_M1_02_HTTP_DuplicateCustomerRejected(t *testing.T) {
	e := newM1Env(t)
	wipeMDHTTP(t, e, "m_customer")
	ck := e.login(m1OpenID)

	resp, body := e.do("POST", "/api/md/customers", ck,
		`{"values":{"code":"9002","name":"测试客户乙"}}`)
	mustOK(t, resp, body, "第一次新增")

	resp, body = e.do("POST", "/api/md/customers", ck,
		`{"values":{"code":"9002","name":"重号客户"}}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("同 code 再次新增应 409，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TC-M1-03 正常：改客户名 ⇒ 产生新版本、旧版本仍可查、is_current 唯一切换。
//
// ★★ 当前预期**失败**，根因见 COLLAB N-009（spec 的 uk_customer_code 未含 version）。
//
//	测试按 docs/04 原样断言，**不改期望、不跳过**。
func TestTC_M1_03_HTTP_RenameCreatesNewVersion(t *testing.T) {
	e := newM1Env(t)
	wipeMDHTTP(t, e, "m_customer")
	ck := e.login(m1OpenID)

	resp, body := e.do("POST", "/api/md/customers", ck,
		`{"values":{"code":"9003","name":"改名前"}}`)
	mustOK(t, resp, body, "准备数据")
	row, _ := body["row"].(map[string]interface{})
	id := numOf(row["id"])

	resp, body = e.do("PUT", fmt.Sprintf("/api/md/customers/%d", id), ck,
		`{"values":{"name":"改名后"},"reason":"TC-M1-03 改客户名"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改客户名应产生新版本（失败根因见 COLLAB N-009），实际 %d body=%v",
			resp.StatusCode, body)
	}
	fresh, _ := body["row"].(map[string]interface{})
	if numOf(fresh["id"]) == id {
		t.Fatal("新版本必须是新行（id 变化）")
	}
	if numOf(fresh["version"]) != 2 {
		t.Fatalf("新版本应为 v2，实际 %v", fresh["version"])
	}

	// 直连库断言：同 code 下 is_current=1 唯一
	var n int
	if err := e.st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM m_customer WHERE code = '9003' AND is_current = 1`).
		Scan(&n); err != nil {
		t.Fatalf("直连库断言失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("同 code 下 is_current=1 应唯一，实际 %d 行", n)
	}
}

// TC-M1-04 边界：查历史版本 ⇒ 能查到改名前的值（接口 + 直连库双口径）。
func TestTC_M1_04_HTTP_HistoryKeepsOldValue(t *testing.T) {
	e := newM1Env(t)
	wipeMDHTTP(t, e, "m_customer")
	ck := e.login(m1OpenID)

	resp, body := e.do("POST", "/api/md/customers", ck,
		`{"values":{"code":"9004","name":"历史版本甲"}}`)
	mustOK(t, resp, body, "准备数据")
	id := numOf(body["row"].(map[string]interface{})["id"])

	resp, body = e.do("PUT", fmt.Sprintf("/api/md/customers/%d", id), ck,
		`{"values":{"name":"历史版本乙"},"reason":"TC-M1-04 查历史"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改名失败（根因见 COLLAB N-009），实际 %d body=%v", resp.StatusCode, body)
	}
	newID := numOf(body["row"].(map[string]interface{})["id"])

	resp, body = e.do("GET", fmt.Sprintf("/api/md/customers/%d/history", newID), ck, "")
	mustOK(t, resp, body, "查版本链")
	chain, _ := body["chain"].([]interface{})
	if len(chain) != 2 {
		t.Fatalf("版本链应有 2 版，实际 %d（body=%v）", len(chain), body)
	}
	first, _ := chain[0].(map[string]interface{})
	if fmt.Sprint(first["name"]) != "历史版本甲" {
		t.Fatalf("链上应保留改名前的值，实际 %v", first["name"])
	}
}

// TC-M1-05 ★ 同一检测项对 A/B 客户各取各的限值（接口侧，含 SQL 查询链路）。
func TestTC_M1_05_HTTP_LimitPerCustomer(t *testing.T) {
	e := newM1Env(t)
	wipeMDHTTP(t, e, "m_test_item_limit", "m_test_item")
	ck := e.login(m1OpenID)

	resp, body := e.do("POST", "/api/md/test-items", ck,
		`{"values":{"code":"T9005","name":"水分","value_type":"数值","unit":"%"}}`)
	mustOK(t, resp, body, "新增检测项")
	ti := numOf(body["row"].(map[string]interface{})["id"])

	mk := func(customer int64, upper string) {
		t.Helper()
		b, err := json.Marshal(map[string]interface{}{
			"values": map[string]interface{}{
				"test_item_id": ti, "customer_id": customer, "material_id": 0,
				"upper_limit": upper,
			},
		})
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		resp, body := e.do("POST", "/api/md/test-item-limits", ck, string(b))
		mustOK(t, resp, body, "新增判定限")
	}
	mk(7101, "1")
	mk(7102, "1.5")

	for _, c := range []struct {
		customer int64
		want     string
	}{{7101, "1"}, {7102, "1.5"}} {
		resp, body := e.do("GET",
			fmt.Sprintf("/api/limits/resolve?test_item_id=%d&customer_id=%d&material_id=9001", ti, c.customer), ck, "")
		mustOK(t, resp, body, "解析判定限")
		if body["resolved"] != true {
			t.Fatalf("客户 %d 应命中判定限，实际 %v", c.customer, body)
		}
		limit, _ := body["limit"].(map[string]interface{})
		if got := fmt.Sprint(limit["upper_limit"]); got != c.want {
			t.Fatalf("客户 %d 应取到 %s，实际 %s（body=%v）", c.customer, c.want, got, body)
		}
	}
}

// TC-M1-06 ★ 既配通用又配专属 ⇒ 取专属（最具体优先），并在响应里给出 rank 佐证。
func TestTC_M1_06_HTTP_MostSpecificWins(t *testing.T) {
	e := newM1Env(t)
	wipeMDHTTP(t, e, "m_test_item_limit", "m_test_item")
	ck := e.login(m1OpenID)

	resp, body := e.do("POST", "/api/md/test-items", ck,
		`{"values":{"code":"T9006","name":"灰分","value_type":"数值","unit":"%"}}`)
	mustOK(t, resp, body, "新增检测项")
	ti := numOf(body["row"].(map[string]interface{})["id"])

	for _, c := range []struct {
		customer, material int64
		upper              string
	}{{0, 0, "9"}, {7201, 0, "1"}} {
		b, _ := json.Marshal(map[string]interface{}{
			"values": map[string]interface{}{
				"test_item_id": ti, "customer_id": c.customer, "material_id": c.material,
				"upper_limit": c.upper,
			},
		})
		resp, body := e.do("POST", "/api/md/test-item-limits", ck, string(b))
		mustOK(t, resp, body, "新增判定限")
	}

	resp, body = e.do("GET",
		fmt.Sprintf("/api/limits/resolve?test_item_id=%d&customer_id=7201&material_id=9001", ti), ck, "")
	mustOK(t, resp, body, "解析判定限")
	limit, _ := body["limit"].(map[string]interface{})
	if got := fmt.Sprint(limit["upper_limit"]); got != "1" {
		t.Fatalf("应取 A 客户专属限值 1（而非通用 9），实际 %s（body=%v）", got, body)
	}
	if numOf(body["rank"]) != 2 {
		t.Fatalf("命中 scope 的 rank 应为 2（客户专属），实际 %v", body["rank"])
	}
}

// TC-M1-07 正常：新增同一车牌两次 ⇒ 第二次被拒（409）。
func TestTC_M1_07_HTTP_DuplicatePlate(t *testing.T) {
	e := newM1Env(t)
	wipeMDHTTP(t, e, "m_vehicle")
	ck := e.login(m1OpenID)

	resp, body := e.do("POST", "/api/md/vehicles", ck,
		`{"values":{"plate_no":"湘F12345"}}`)
	mustOK(t, resp, body, "第一次新增车牌")

	resp, body = e.do("POST", "/api/md/vehicles", ck,
		`{"values":{"plate_no":"湘F12345"}}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复车牌应 409，实际 %d body=%v", resp.StatusCode, body)
	}
}

// TC-M1-08 异常：停用物料后不再出现在「启用」列表（下拉口径），但记录仍可查。
func TestTC_M1_08_HTTP_DisabledMaterialHidden(t *testing.T) {
	e := newM1Env(t)
	wipeMDHTTP(t, e, "m_material")
	ck := e.login(m1OpenID)

	resp, body := e.do("POST", "/api/md/materials", ck,
		`{"values":{"code":"9008","name":"测试物料","kind":"原料"}}`)
	mustOK(t, resp, body, "新增物料")
	id := numOf(body["row"].(map[string]interface{})["id"])

	resp, body = e.do("PATCH", fmt.Sprintf("/api/md/materials/%d/status", id), ck,
		`{"status":"停用","reason":"TC-M1-08 停用"}`)
	mustOK(t, resp, body, "停用物料")

	// ① 启用列表 ⇒ 不出现
	resp, body = e.do("GET", "/api/md/materials?status=启用", ck, "")
	mustOK(t, resp, body, "查启用列表")
	rows, _ := body["rows"].([]interface{})
	for _, r := range rows {
		m, _ := r.(map[string]interface{})
		if numOf(m["id"]) == id {
			t.Fatal("已停用物料不得出现在「启用」列表里")
		}
	}
	// ② 记录本身仍可查（历史单据仍能显示它）
	resp, body = e.do("GET", fmt.Sprintf("/api/md/materials/%d", id), ck, "")
	mustOK(t, resp, body, "查已停用记录")
	row, _ := body["row"].(map[string]interface{})
	if fmt.Sprint(row["status"]) != "停用" {
		t.Fatalf("status 应为 停用，实际 %v", row["status"])
	}
	if numOf(row["version"]) != 1 {
		t.Fatalf("停用是状态流转，不应产生新版本，实际 %v", row["version"])
	}
}

// 只读角色不能写（READ ≠ ALL 的边界，呼应 docs/01 §8.0「READ = 只读」）。
func TestMD_ReadOnlyRoleCannotWrite(t *testing.T) {
	st := requireDB(t)
	const reader = "ou_test_m1_reader"
	bindRoles(t, st, reader, "receiver") // receiver: md.customer = READ
	e := newEnv(t, time.Hour)
	ck := e.login(reader)

	resp, body := e.do("GET", "/api/md/customers", ck, "")
	mustOK(t, resp, body, "只读角色应能查")

	resp, body = e.do("POST", "/api/md/customers", ck,
		`{"values":{"code":"9099","name":"越权客户"}}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("只读角色写入应 403，实际 %d body=%v", resp.StatusCode, body)
	}
}
