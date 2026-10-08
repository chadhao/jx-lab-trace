package httpapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TC-M0-07 / TC-M0-08：门禁报红测试。
//
// 判据本体在 scripts/check_perm_registry.py；这两个用例跑它的 `--selftest`
// （在临时目录里造**违规样本**，断言门禁对样本 exit 1、对干净样本 exit 0）。
// ★ 这样 TC 的「期望＝门禁报红」才是被持续验证的，而不是嘴上说说。

func runGateSelftest(t *testing.T, only string) {
	t.Helper()
	py := findPython(t)
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}
	script := filepath.Join(repoRoot, "scripts", "check_perm_registry.py")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("找不到门禁脚本: %v", err)
	}
	cmd := exec.Command(py, script, "--selftest", "--only", only)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("门禁自检未通过（= 变异没被抓住，或干净样本被误判）: %v\n输出:\n%s", err, out)
	}
}

func findPython(t *testing.T) string {
	t.Helper()
	for _, cand := range []string{"python", "python3"} {
		if p, err := exec.LookPath(cand); err == nil {
			return p
		}
	}
	t.Skip("PATH 里没有 python：无法执行 check_perm_registry.py（本仓库门禁全部由 python 驱动，正常验收环境必有）")
	return ""
}

// TC-M0-07 门禁：代码里出现字典外的权限点字符串 ⇒ 门禁报红。
func TestTC_M0_07_GateFlagsUndeclaredPermissionString(t *testing.T) {
	runGateSelftest(t, "undeclared_string")
}

// TC-M0-08 门禁：某权限点没有任何代码消费 ⇒ 门禁报红（防“配了没用”）。
func TestTC_M0_08_GateFlagsUnconsumedPermissionPoint(t *testing.T) {
	runGateSelftest(t, "unused_point")
}

// 附带：干净样本必须放行，否则上面两条「报红」毫无意义（假红 = 假绿的镜像）。
func TestTC_M0_07_GateAcceptsCleanSample(t *testing.T) {
	runGateSelftest(t, "clean")
}
