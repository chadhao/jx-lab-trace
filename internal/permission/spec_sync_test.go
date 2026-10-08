package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// 权限点常量必须与 spec/permission-points.json **一一对应**（51 条，不重不漏）。
// 门禁判据② 的库外孪生：这里保证「代码常量 ⇄ 机读规格」同步。

func loadSpecCodes(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "permission-points.json"))
	if err != nil {
		t.Fatalf("读取 spec/permission-points.json 失败: %v", err)
	}
	var spec struct {
		PermissionPoints []struct {
			Code string `json:"code"`
		} `json:"permission_points"`
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatalf("解析 spec 失败: %v", err)
	}
	var out []string
	for _, p := range spec.PermissionPoints {
		out = append(out, p.Code)
	}
	return out
}

func TestSpecSync_ConstantsMatchSpecExactly(t *testing.T) {
	spec := loadSpecCodes(t)
	if len(spec) != 51 {
		t.Fatalf("spec 应有 51 个权限点，实际 %d", len(spec))
	}

	src, err := os.ReadFile("code.go")
	if err != nil {
		t.Fatalf("读取 code.go 失败: %v", err)
	}
	found := regexp.MustCompile(`Code\s*=\s*"([^"]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(found) != len(spec) {
		t.Fatalf("code.go 声明 %d 个常量，spec 有 %d 个", len(found), len(spec))
	}
	got := map[string]bool{}
	for _, m := range found {
		if got[m[1]] {
			t.Fatalf("常量重复声明：%s", m[1])
		}
		got[m[1]] = true
	}
	for _, c := range spec {
		if !got[c] {
			t.Fatalf("spec 中的 %s 在 code.go 里没有对应常量", c)
		}
	}
}

func TestSpecSync_AllCoversEveryConstant(t *testing.T) {
	src, err := os.ReadFile("code.go")
	if err != nil {
		t.Fatalf("读取 code.go 失败: %v", err)
	}
	found := regexp.MustCompile(`Code\s*=\s*"([^"]+)"`).FindAllStringSubmatch(string(src), -1)

	spec := loadSpecCodes(t)
	if len(All) != len(spec) {
		t.Fatalf("permission.All 有 %d 项，spec 有 %d 项 —— 权限点字典由代码注册，两者必须一致", len(All), len(spec))
	}
	inAll := map[string]bool{}
	for _, c := range All {
		inAll[c.String()] = true
	}
	for _, m := range found {
		if !inAll[m[1]] {
			t.Fatalf("常量 %s 已声明但未进 All（= 未注册进字典，配了没人用）", m[1])
		}
	}
	for _, c := range spec {
		if !inAll[c] {
			t.Fatalf("spec 的 %s 未被 All 消费", c)
		}
	}
}

// TestSpecSync_AllIsConsumedOutsideDeclarationFile 是门禁判据① 的孪生断言：
// 每个 code 必须在**声明文件之外**被引用（All 列表 / 受保护入口）。
func TestSpecSync_AllIsConsumedOutsideDeclarationFile(t *testing.T) {
	allSrc, err := os.ReadFile("all.go")
	if err != nil {
		t.Fatalf("读取 all.go 失败: %v", err)
	}
	spec := loadSpecCodes(t)
	codeByName := map[string]string{}
	src, _ := os.ReadFile("code.go")
	for _, m := range regexp.MustCompile(`(\w+)\s+Code\s*=\s*"([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		codeByName[m[2]] = m[1]
	}
	for _, c := range spec {
		name := codeByName[c]
		if name == "" {
			t.Fatalf("spec 的 %s 没有常量名", c)
		}
		if !regexp.MustCompile(`\b` + name + `\b`).Match(allSrc) {
			t.Fatalf("%s（常量 %s）未在 all.go 被引用 —— 配了没人用", c, name)
		}
	}
}

func TestEngine_UnionAndDeny(t *testing.T) {
	// 多角色取并集
	joined := Union(Levels{LevelRead}, Levels{LevelNone}, Levels{LevelRead})
	if len(joined) != 1 || joined[0] != LevelRead {
		t.Fatalf("并集应去重为 [READ]，实际 %v", joined)
	}
	if !joined.Granted() {
		t.Fatal("READ 应视为可用")
	}
	if joined.Allows(LevelAll) {
		t.Fatal("READ 不得满足 ALL")
	}
	if !joined.Allows(LevelRead) {
		t.Fatal("READ 应满足 READ")
	}

	// 无记录 ⇒ 空 ⇒ 一律拒绝
	var empty Levels
	if empty.Granted() || empty.Allows(LevelRead) || empty.Allows(LevelAll) {
		t.Fatalf("空集合（无记录）必须全部拒绝，实际 granted=%v allowsRead=%v", empty.Granted(), empty.Allows(LevelRead))
	}

	// NONE 显式记录同样拒绝
	none := Levels{LevelNone}
	if none.Granted() {
		t.Fatal("NONE 不可用")
	}

	// ALL 覆盖一切级别
	all := Levels{LevelAll}
	for _, req := range []string{LevelAll, LevelRead, LevelInit, LevelApprove} {
		if !all.Allows(req) {
			t.Fatalf("ALL 应满足 %s", req)
		}
	}
}
