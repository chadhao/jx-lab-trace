package specload

// ===== N-023 D5-6：spec 判据双引擎一致性 =====
//
// ★ 本测试（Go 引擎）必须全绿；并**真跑** Python 引擎（scripts/check_spec.py），
//	逐条对账两引擎对同一 checks.json 的判定（id 集合 + 每条 pass/fail）——
//	「任一单独过不算」：单侧绿而另一侧红 ⇒ 此处对账失败。
// ★ Python 解释器：优先 python，回退 python3（门禁 PY 与服务器 e2e 均有其一）。

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..")
}

// TestSpecChecks_AllPass Go 引擎：checks.json 全部判据通过，
// 且判据使用的 kind 全在声明全集内（Run 对未知 kind 记失败，此处兜底断言）。
func TestSpecChecks_AllPass(t *testing.T) {
	e, err := Load(repoRoot(t))
	if err != nil {
		t.Fatalf("加载 spec 判据失败: %v", err)
	}
	if len(e.Doc.Checks) == 0 {
		t.Fatal("checks.json 无判据")
	}
	results := e.Run()
	passed := 0
	for _, r := range results {
		if r.Passed {
			passed++
		} else {
			t.Errorf("判据 %s 未通过: %s（%s）", r.ID, r.Desc, r.Detail)
		}
	}
	if passed != len(results) {
		t.Fatalf("Go 引擎 %d/%d 通过", passed, len(results))
	}
	t.Logf("Go 引擎：%d 条判据全部通过（kinds 声明 %d 种）", len(results), len(e.Doc.Kinds))
}

// TestSpecChecks_DualEngineAgree 双引擎对账（D5-6）：
// 真跑 scripts/check_spec.py，解析其逐条判定，与 Go 引擎结果比对 id 集与 pass/fail。
func TestSpecChecks_DualEngineAgree(t *testing.T) {
	e, err := Load(repoRoot(t))
	if err != nil {
		t.Fatalf("加载 spec 判据失败: %v", err)
	}
	goResults := e.Run()

	py := findPython(t)
	cmd := exec.Command(py, filepath.Join("scripts", "check_spec.py"))
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	pyExitOK := err == nil
	text := string(out)

	// 解析 Python 侧逐条判定：行首 "  ok   ID" / "  FAIL ID"
	// ★ ID 形如 N023-01 / N032-01 等（前缀可能随议题变化），用通用 pattern。
	lineRe := regexp.MustCompile(`(?m)^\s+(ok|FAIL)\s+([A-Z0-9]+-\d+)`)
	pyVerdict := map[string]bool{}
	for _, m := range lineRe.FindAllStringSubmatch(text, -1) {
		pyVerdict[m[2]] = m[1] == "ok"
	}
	if len(pyVerdict) == 0 {
		t.Fatalf("无法从 Python 引擎输出解析逐条判定（exit err=%v）:\n%s", err, text)
	}

	// ① id 集合一致
	if len(pyVerdict) != len(goResults) {
		t.Errorf("判据条数不一致: Go %d vs Python %d", len(goResults), len(pyVerdict))
	}
	for _, r := range goResults {
		py, seen := pyVerdict[r.ID]
		if !seen {
			t.Errorf("Python 引擎未覆盖判据 %s", r.ID)
			continue
		}
		if py != r.Passed {
			t.Errorf("判据 %s 判定不一致: Go=%v Python=%v", r.ID, r.Passed, py)
		}
	}
	// ② Python 侧必须全绿（退出码 0）
	if !pyExitOK {
		t.Errorf("Python 引擎（%s scripts/check_spec.py）未通过:\n%s", py, text)
	} else {
		t.Logf("双引擎对账一致：%d 条判据，两侧全绿（Go + %s）", len(goResults), py)
	}
}

// findPython 找可用的 Python 解释器；找不到 ⇒ Fail（双引擎是硬要求，不许跳过）。
func findPython(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python", "python3", "py"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	// 环境变量 PY（check_all.sh 同款约定）
	if v := os.Getenv("PY"); v != "" {
		return v
	}
	t.Fatal("找不到 python 解释器 —— spec 判据双引擎的 Python 侧是硬要求（scripts/check_spec.py），不许跳过")
	return ""
}
