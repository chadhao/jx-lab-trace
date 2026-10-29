package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ===== N-033 接口级 TC（MIMO-NEXT-FIX-17 D5）=====
//
// ★ 与 N-032 客户同构：四类主数据（物料／检测项目／原料组成／原料类型）
// 创建时 code 缺省自动分配；显式 code 兼容；版本链沿用原编号。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。

const n033OpenID = "ou_test_m1_editor"

type n033Case struct {
	key, table, label string
	payload           string // 创建 body 的 values 部分（不含 code）
	explicitCode      string
	dupPayload        string // 显式重号用（可与 payload 共用 name）
	hardCap           bool
}

func n033Cases() []n033Case {
	return []n033Case{
		{"materials", "m_material", "物料",
			`{"name":"N033物料","kind":"原料"}`, "9451",
			`{"code":"9451","name":"N033物料重号","kind":"原料"}`, true},
		{"test-items", "m_test_item", "检测项目",
			`{"name":"N033检测项","value_type":"数值"}`, "9461",
			`{"code":"9461","name":"N033检测项重号","value_type":"数值"}`, false},
		{"compositions", "m_composition", "原料组成",
			`{"name":"N033组成"}`, "9471",
			`{"code":"9471","name":"N033组成重号"}`, false},
		{"material-types", "m_material_type", "原料类型",
			`{"name":"N033类型"}`, "9481",
			`{"code":"9481","name":"N033类型重号"}`, false},
	}
}

func newN033Env(t *testing.T) *env {
	t.Helper()
	st := requireDB(t)
	bindRoles(t, st, n033OpenID, "sales", "qc", "receiver")
	return newEnv(t, time.Hour)
}

func wipeN033(t *testing.T, e *env, tables ...string) {
	t.Helper()
	wipeMDHTTP(t, e, tables...)
}

// D5-1/2：四类各连建 2 条无 code ⇒ 编号递增；显式 code 兼容。
func TestN033_HTTP_AutoCodeAndExplicit(t *testing.T) {
	e := newN033Env(t)
	ck := e.login(n033OpenID)

	for _, c := range n033Cases() {
		t.Run(c.key, func(t *testing.T) {
			wipeN033(t, e, c.table)

			// 显式编号兼容（dupPayload 含 explicitCode；若已被种子占用则 409 也算兼容就绪）
			resp, body := e.do("POST", "/api/md/"+c.key, ck,
				`{"values":`+c.dupPayload+`}`)
			if resp.StatusCode == http.StatusOK {
				got := fmt.Sprint(body["row"].(map[string]interface{})["code"])
				if got != c.explicitCode {
					t.Fatalf("显式 code 应保留 %s，实际 %s body=%v", c.explicitCode, got, body)
				}
			} else if resp.StatusCode != http.StatusConflict {
				t.Fatalf("显式 code 首建应 200 或 409，实际 %d body=%v", resp.StatusCode, body)
			}

			// 自动编号：应衔接（高位库 skip）
			resp, body = e.do("POST", "/api/md/"+c.key, ck,
				`{"values":`+c.payload+`}`)
			if resp.StatusCode == http.StatusConflict && strings.Contains(fmt.Sprint(body["error"]), "已用尽") {
				t.Skipf("库内编号已用尽，跳过自动编号段：body=%v", body)
			}
			mustOK(t, resp, body, "自动建甲")
			codeA := fmt.Sprint(body["row"].(map[string]interface{})["code"])

			p2 := strings.Replace(c.payload, "甲", "乙", 1)
			if !strings.Contains(p2, "乙") {
				p2 = strings.Replace(c.payload, "N033", "N033乙", 1)
			}
			resp, body = e.do("POST", "/api/md/"+c.key, ck, `{"values":`+p2+`}`)
			if resp.StatusCode == http.StatusConflict && strings.Contains(fmt.Sprint(body["error"]), "已用尽") {
				t.Skipf("库内编号已用尽，跳过第二个自动编号：body=%v", body)
			}
			mustOK(t, resp, body, "自动建乙")
			codeB := fmt.Sprint(body["row"].(map[string]interface{})["code"])
			if codeA == codeB || codeA == "" || codeB == "" {
				t.Fatalf("自动编号应非空且不同，实际 %q→%q", codeA, codeB)
			}
			var na, nb int
			if _, err := fmt.Sscanf(codeA, "%d", &na); err != nil {
				t.Fatalf("codeA 非数字: %q", codeA)
			}
			if _, err := fmt.Sscanf(codeB, "%d", &nb); err != nil {
				t.Fatalf("codeB 非数字: %q", codeB)
			}
			if nb != na+1 {
				t.Fatalf("自动编号应连续递增，实际 %s→%s", codeA, codeB)
			}
		})
	}
}

// D5-2：显式 code 重号 ⇒ 409。
func TestN033_HTTP_ExplicitDuplicate409(t *testing.T) {
	e := newN033Env(t)
	ck := e.login(n033OpenID)

	for _, c := range n033Cases() {
		t.Run(c.key, func(t *testing.T) {
			wipeN033(t, e, c.table)
			resp, body := e.do("POST", "/api/md/"+c.key, ck,
				`{"values":`+c.dupPayload+`}`)
			// 可能已存在（未被 wipe 的种子）—— 重复也算 409 路径就绪
			if resp.StatusCode == http.StatusOK {
				// ok
			} else if resp.StatusCode != http.StatusConflict {
				t.Fatalf("首次显式建应 200 或 409，实际 %d body=%v", resp.StatusCode, body)
			}
			resp, body = e.do("POST", "/api/md/"+c.key, ck,
				`{"values":`+c.dupPayload+`}`)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("重号应 409，实际 %d body=%v", resp.StatusCode, body)
			}
		})
	}
}

// D5-3：版本链 —— 改名产生新版本后，编号沿用原值。
func TestN033_HTTP_VersionChainKeepsCode(t *testing.T) {
	e := newN033Env(t)
	ck := e.login(n033OpenID)

	for _, c := range n033Cases() {
		t.Run(c.key, func(t *testing.T) {
			wipeN033(t, e, c.table)
			resp, body := e.do("POST", "/api/md/"+c.key, ck,
				`{"values":`+c.payload+`}`)
			if resp.StatusCode == http.StatusConflict && strings.Contains(fmt.Sprint(body["error"]), "已用尽") {
				t.Skipf("库内编号已用尽：body=%v", body)
			}
			mustOK(t, resp, body, "创建")
			row, _ := body["row"].(map[string]interface{})
			code := fmt.Sprint(row["code"])
			id := numOf(row["id"])

			// 改名（必填 reason）
			put := fmt.Sprintf(`{"values":{"name":"%s改名"},"reason":"N033 版本链沿用原编号"}`,
				fmt.Sprint(row["name"]))
			resp, body = e.do("PUT", fmt.Sprintf("/api/md/%s/%d", c.key, id), ck, put)
			mustOK(t, resp, body, "改名产生新版本")
			fresh, _ := body["row"].(map[string]interface{})
			if got := fmt.Sprint(fresh["code"]); got != code {
				t.Fatalf("版本链应沿用原编号 %s，实际 %s body=%v", code, got, body)
			}
		})
	}
}

// D5-4：物料 9999 用尽 ⇒ 409；检测项目/组成/类型 9999 后 ⇒ 10000（5 位）。
func TestN033_HTTP_CapAndCarry(t *testing.T) {
	e := newN033Env(t)
	ck := e.login(n033OpenID)

	for _, c := range n033Cases() {
		t.Run(c.key, func(t *testing.T) {
			wipeN033(t, e, c.table)

			// 占 9999（补齐各实体必填列）
			extra := ""
			switch c.key {
			case "materials":
				extra = `,"kind":"原料"`
			case "test-items":
				extra = `,"value_type":"数值"`
			}
			resp, body := e.do("POST", "/api/md/"+c.key, ck,
				fmt.Sprintf(`{"values":{"code":"9999","name":"N033占满"%s}}`, extra))
			if resp.StatusCode == http.StatusConflict {
				msg0 := fmt.Sprint(body["error"])
				if !strings.Contains(msg0, "已存在") && !strings.Contains(msg0, "已用尽") {
					t.Fatalf("占 9999 应成功或唯一冲突，实际 %d body=%v", resp.StatusCode, body)
				}
			} else {
				mustOK(t, resp, body, "占 9999")
			}

			resp, body = e.do("POST", "/api/md/"+c.key, ck,
				`{"values":`+c.payload+`}`)
			if c.hardCap {
				if resp.StatusCode != http.StatusConflict {
					t.Fatalf("物料耗尽应 409，实际 %d body=%v", resp.StatusCode, body)
				}
				msg := fmt.Sprint(body["error"])
				if !strings.Contains(msg, "已用尽") {
					t.Fatalf("应提示编号已用尽，实际 %v", body)
				}
				return
			}
			// 无硬上限：进位为 >=10000 的纯数字（高位库可能已更高）
			mustOK(t, resp, body, "9999 后自动分配")
			got := fmt.Sprint(body["row"].(map[string]interface{})["code"])
			var n int
			if _, err := fmt.Sscanf(got, "%d", &n); err != nil || n < 10000 {
				t.Fatalf("9999 后应进位为 >=10000，实际 %s body=%v", got, body)
			}
		})
	}
}
