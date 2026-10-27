package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ===== N-032 接口级 TC（MIMO-NEXT-FIX-16 D4）=====
//
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。

const n032OpenID = "ou_test_m1_editor"

func newN032Env(t *testing.T) *env {
	t.Helper()
	st := requireDB(t)
	bindRoles(t, st, n032OpenID, "sales", "qc", "receiver")
	return newEnv(t, time.Hour)
}

func wipeN032(t *testing.T, e *env) {
	t.Helper()
	wipeMDHTTP(t, e, "m_customer")
}

// D4-1/2：连建 2 个无 code 客户 ⇒ 409 以外均 200 且编号递增；显式 code 兼容。
func TestN032_HTTP_AutoCodeAndExplicit(t *testing.T) {
	e := newN032Env(t)
	wipeN032(t, e)
	ck := e.login(n032OpenID)

	// 昔式编号兼容
	resp, body := e.do("POST", "/api/md/customers", ck,
		`{"values":{"code":"9701","name":"N032 显式"}}`)
	mustOK(t, resp, body, "显式 code 新增")
	if fmt.Sprint(body["row"].(map[string]interface{})["code"]) != "9701" {
		t.Fatalf("显式 code 应保留，body=%v", body)
	}

	// 自动编号：应衔接
	resp, body = e.do("POST", "/api/md/customers", ck,
		`{"values":{"name":"N032 自动甲"}}`)
	mustOK(t, resp, body, "自动建甲")
	codeA := fmt.Sprint(body["row"].(map[string]interface{})["code"])
	resp, body = e.do("POST", "/api/md/customers", ck,
		`{"values":{"name":"N032 自动乙"}}`)
	mustOK(t, resp, body, "自动建乙")
	codeB := fmt.Sprint(body["row"].(map[string]interface{})["code"])
	if codeA == codeB || codeA == "" || codeB == "" {
		t.Fatalf("自动编号应非空且不同，实际 %q→%q", codeA, codeB)
	}
	// 与 9701 衔接 ⇒ 9702→9703（若 9702/9703 已被其它测试占用则至少验证递增）
	if codeA != "9702" || codeB != "9703" {
		// 允许库内有更高编号：只断言数值递增且 4 位
		var na, nb int
		if _, err := fmt.Sscanf(codeA, "%04d", &na); err != nil {
			t.Fatalf("codeA 非 4 位数字: %q", codeA)
		}
		if _, err := fmt.Sscanf(codeB, "%04d", &nb); err != nil {
			t.Fatalf("codeB 非 4 位数字: %q", codeB)
		}
		if nb != na+1 {
			t.Fatalf("自动编号应连续递增，实际 %s→%s", codeA, codeB)
		}
	}
}

// D4-2：显式 code 重号 ⇒ 409。
func TestN032_HTTP_ExplicitDuplicate409(t *testing.T) {
	e := newN032Env(t)
	wipeN032(t, e)
	ck := e.login(n032OpenID)

	resp, body := e.do("POST", "/api/md/customers", ck,
		`{"values":{"code":"9711","name":"N032 重号甲"}}`)
	mustOK(t, resp, body, "第一次")
	resp, body = e.do("POST", "/api/md/customers", ck,
		`{"values":{"code":"9711","name":"N032 重号乙"}}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重号应 409，实际 %d body=%v", resp.StatusCode, body)
	}
}

// D4-3：9999 已存在 ⇒ 自动新建 409 且文案含「已用尽」。
func TestN032_HTTP_CodeExhausted409(t *testing.T) {
	e := newN032Env(t)
	wipeN032(t, e)
	ck := e.login(n032OpenID)

	resp, body := e.do("POST", "/api/md/customers", ck,
		`{"values":{"code":"9999","name":"N032 占满"}}`)
	mustOK(t, resp, body, "占 9999")
	resp, body = e.do("POST", "/api/md/customers", ck,
		`{"values":{"name":"N032 应耗尽"}}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("耗尽应 409，实际 %d body=%v", resp.StatusCode, body)
	}
	msg := fmt.Sprint(body["error"])
	if !strings.Contains(msg, "已用尽") {
		t.Fatalf("应提示编号已用尽，实际 %v", body)
	}
}

// D4-4：is_internal / is_outsourced 传 2/"是" ⇒ 400。
func TestN032_HTTP_Bool01Rejected400(t *testing.T) {
	e := newN032Env(t)
	wipeN032(t, e)
	ck := e.login(n032OpenID)

	for _, payload := range []string{
		`{"values":{"code":"9721","name":"脏集团内","is_internal":2}}`,
		`{"values":{"code":"9722","name":"脏集团内串","is_internal":"是"}}`,
		`{"values":{"code":"9723","name":"脏委外","is_outsourced":2}}`,
		`{"values":{"code":"9724","name":"脏委外串","is_outsourced":"是"}}`,
	} {
		resp, body := e.do("POST", "/api/md/customers", ck, payload)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("应 400，实际 %d body=%v payload=%s", resp.StatusCode, body, payload)
		}
	}
}

// D4-5：is_outsourced=1 往返（详情 + 列表形状含 is_outsourced）。
func TestN032_HTTP_OutsourcedRoundtrip(t *testing.T) {
	e := newN032Env(t)
	wipeN032(t, e)
	ck := e.login(n032OpenID)

	resp, body := e.do("POST", "/api/md/customers", ck,
		`{"values":{"code":"9731","name":"N032 委外","is_outsourced":1,"is_internal":0}}`)
	mustOK(t, resp, body, "建委外客户")
	row, _ := body["row"].(map[string]interface{})
	id := numOf(row["id"])
	if numOf(row["is_outsourced"]) != 1 {
		t.Fatalf("创建响应 is_outsourced 应 1，实际 %v", row["is_outsourced"])
	}

	resp, body = e.do("GET", fmt.Sprintf("/api/md/customers/%d", id), ck, "")
	mustOK(t, resp, body, "详情")
	got, _ := body["row"].(map[string]interface{})
	if numOf(got["is_outsourced"]) != 1 {
		t.Fatalf("详情 is_outsourced 应 1，实际 %v", got["is_outsourced"])
	}

	resp, body = e.do("GET", "/api/md/customers?q=9731", ck, "")
	mustOK(t, resp, body, "列表")
	rows, _ := body["rows"].([]interface{})
	if len(rows) == 0 {
		t.Fatalf("列表应命中，body=%v", body)
	}
	first, _ := rows[0].(map[string]interface{})
	if _, ok := first["is_outsourced"]; !ok {
		t.Fatalf("列表行应含 is_outsourced，实际 %v", first)
	}
	if numOf(first["is_outsourced"]) != 1 {
		t.Fatalf("列表 is_outsourced 应 1，实际 %v", first["is_outsourced"])
	}
}
