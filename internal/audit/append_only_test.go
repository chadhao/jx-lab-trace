package audit

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TC-M0-11 门禁：审计**只增不改**。
//
// ★ 判据边界（回执同款说明）：这里防的是**代码里出现改写审计的通路**
//   —— 对 s_audit_log 的 UPDATE / DELETE 在非测试源码与迁移脚本里一律不许出现；
//   自证方式：往临时目录里塞一条 `UPDATE s_audit_log ...` 的变异文件，
//   同一套扫描器必须当场抓出（证明判据有牙齿，不是“看起来在查”）。
//   ★ 不覆盖的形态：绕过应用直连库的人肉篡改 —— schema 已冻结为 38 张表、
//   且 s_audit_log 无哈希列，M0 无法在不改表的前提下检测；如需该能力须开议题。

var rewriteAuditRe = regexp.MustCompile(`(?is)\b(?:UPDATE\s+|DELETE\s+FROM\s+)[^;\n]*\bs_audit_log\b`)

// scanRewrite 返回 root 下所有**非测试** Go 源码与 SQL 脚本里改写审计的命中。
func scanRewrite(root string) ([]string, error) {
	var hits []string
	skipDir := func(name string) bool {
		return name == ".git" || name == "node_modules" || name == "bin" ||
			strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		isGo := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
		isSQL := strings.HasSuffix(name, ".sql")
		if !isGo && !isSQL {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if rewriteAuditRe.Match(b) {
			rel, _ := filepath.Rel(root, path)
			hits = append(hits, rel)
		}
		return nil
	})
	return hits, err
}

func TestTC_M0_11_AuditAppendOnly_NoRewritePath(t *testing.T) {
	hits, err := scanRewrite("../..")
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("★ 审计只增不改被破坏：以下文件出现对 s_audit_log 的 UPDATE/DELETE：%v", hits)
	}
}

func TestTC_M0_11_AuditAppendOnly_GateHasTeeth(t *testing.T) {
	dir := t.TempDir()
	mutant := filepath.Join(dir, "mutant.go")
	body := "package mutant\n\n// 变异：偷偷改审计\nfunc f() { db.Exec(\"UPDATE s_audit_log SET actor_open_id='x' WHERE id=1\") }\n"
	if err := os.WriteFile(mutant, []byte(body), 0o644); err != nil {
		t.Fatalf("写变异文件失败: %v", err)
	}
	hits, err := scanRewrite(dir)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("★ 判据失灵：变异文件里的 UPDATE s_audit_log 没有被抓出")
	}
}

func TestTC_M0_11_AuditAppendOnly_InsertOnlyAPIDefined(t *testing.T) {
	// 审计入口必须存在且是 INSERT 语义（store 层），这里只断言包内 Entry 可校验。
	e := Entry{Entity: "", Action: "x", ActorOpenID: "ou"}
	if err := e.Validate(); err == nil {
		t.Fatal("缺 entity 的审计记录必须被拒绝")
	}
	e = Entry{Entity: "s_permission_point", Action: "status", ActorOpenID: ""}
	if err := e.Validate(); err == nil {
		t.Fatal("缺 actor 的审计记录必须被拒绝（谁做的必须可追溯）")
	}
	e = Entry{Entity: "s_permission_point", Action: "status", ActorOpenID: "ou_x"}
	if err := e.Validate(); err != nil {
		t.Fatalf("合法记录不应被拒: %v", err)
	}
}
