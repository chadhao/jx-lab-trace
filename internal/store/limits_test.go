package store

import (
	"encoding/json"
	"fmt"
	"testing"
)

// jsonNum / numStr 是纯单测里的小工具（判定限数值以 json.Number 表达）。
func jsonNum(s string) interface{} { return json.Number(s) }

func numStr(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case json.Number:
		return t.String()
	case string:
		return t
	default:
		return fmt.Sprint(v)
	}
}

// ===== 判定限解析（D3 / TC-M1-05 / TC-M1-06）=====
//
// ★ 这两组是**纯单测**：不连库、不起服务即可断言优先级顺序
//（任务包 D3：解析函数必须可单测）。

// TC-M1-05 ★ 同一检测项对不同客户取到不同限值。
func TestTC_M1_05_Limit_DifferentCustomersGetTheirOwn(t *testing.T) {
	cands := []LimitRow{
		{ID: 1, TestItemID: 7, CustomerID: 101, MaterialID: 0, Upper: jsonNum("1"), Status: "启用"},
		{ID: 2, TestItemID: 7, CustomerID: 202, MaterialID: 0, Upper: jsonNum("1.5"), Status: "启用"},
	}
	got, ok := ResolveLimit(cands, 101, 5)
	if !ok {
		t.Fatal("A 客户应命中自己的判定限")
	}
	if numStr(got.Upper) != "1" {
		t.Fatalf("A 客户应取到 <1，实际 %v", got.Upper)
	}
	got, ok = ResolveLimit(cands, 202, 5)
	if !ok {
		t.Fatal("B 客户应命中自己的判定限")
	}
	if numStr(got.Upper) != "1.5" {
		t.Fatalf("B 客户应取到 <1.5，实际 %v", got.Upper)
	}
}

// TC-M1-06 ★ 既配通用默认、又配 A 客户专属 ⇒ 取专属（最具体优先）。
func TestTC_M1_06_Limit_MostSpecificWins(t *testing.T) {
	general := LimitRow{ID: 1, TestItemID: 7, CustomerID: 0, MaterialID: 0, Upper: jsonNum("9"), Status: "启用"}
	specific := LimitRow{ID: 2, TestItemID: 7, CustomerID: 101, MaterialID: 0, Upper: jsonNum("1"), Status: "启用"}

	got, ok := ResolveLimit([]LimitRow{general, specific}, 101, 5)
	if !ok || numStr(got.Upper) != "1" {
		t.Fatalf("应取 A 客户专属限值 1，实际 ok=%v upper=%v", ok, got.Upper)
	}
	// 换个顺序也必须一样（顺序无关）
	got, ok = ResolveLimit([]LimitRow{specific, general}, 101, 5)
	if !ok || numStr(got.Upper) != "1" {
		t.Fatalf("候选顺序不应影响结果，实际 ok=%v upper=%v", ok, got.Upper)
	}
	// 别的客户没有专属 ⇒ 落回通用
	got, ok = ResolveLimit([]LimitRow{general, specific}, 202, 5)
	if !ok || numStr(got.Upper) != "9" {
		t.Fatalf("无专属时应取通用默认 9，实际 ok=%v upper=%v", ok, got.Upper)
	}
}

// ★ 优先级阶梯：精确 > (客户,0) > (0,物料) > 通用；无关 scope 不参与。
func TestLimitRank_Order(t *testing.T) {
	cases := []struct {
		c, m         int64
		wantC, wantM int64
		want         int
	}{
		{1, 5, 1, 5, 3},  // 精确
		{1, 0, 1, 5, 2},  // 客户专属
		{0, 5, 1, 5, 1},  // 物料专属
		{0, 0, 1, 5, 0},  // 通用默认
		{9, 5, 1, 5, -1}, // 别的客户 ⇒ 不参与
		{1, 9, 1, 5, -1}, // 别的物料 ⇒ 不参与
		{9, 9, 1, 5, -1}, // 都不匹配
		{0, 0, 0, 0, 3},  // 全零查询下的通用即精确
	}
	for _, c := range cases {
		if got := LimitRank(c.c, c.m, c.wantC, c.wantM); got != c.want {
			t.Fatalf("LimitRank(c=%d,m=%d want=%d,%d) = %d，期望 %d",
				c.c, c.m, c.wantC, c.wantM, got, c.want)
		}
	}
}

// ★ 停用的判定限不参与解析；同 rank 时取 version 更大者。
func TestResolveLimit_SkipsDisabledAndPrefersNewerVersion(t *testing.T) {
	disabled := LimitRow{ID: 1, CustomerID: 101, MaterialID: 0, Upper: jsonNum("1"), Status: "停用"}
	old := LimitRow{ID: 2, CustomerID: 101, MaterialID: 0, Upper: jsonNum("2"), Status: "启用", Version: 1}
	fresh := LimitRow{ID: 3, CustomerID: 101, MaterialID: 0, Upper: jsonNum("3"), Status: "启用", Version: 2}

	got, ok := ResolveLimit([]LimitRow{disabled, old, fresh}, 101, 0)
	if !ok {
		t.Fatal("应有命中")
	}
	if numStr(got.Upper) != "3" {
		t.Fatalf("应取最新启用版本 3，实际 %v（停用行不得参与）", got.Upper)
	}
}
