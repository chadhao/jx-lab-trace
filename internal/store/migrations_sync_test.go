package store

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/chadhao/jx-lab-trace/migrations"
)

// 门禁：migrations/ 下的副本必须与 spec/ 原件**逐字节一致** ——
// go:embed 不能跨目录引用 ../spec，故采用「副本 + 机检」保证「以 spec 为准」不漂移。

func TestMigrationsSchemaCopyMatchesSpec(t *testing.T) {
	assertSameFile(t, filepath.Join("..", "..", "spec", "schema.sql"), migrations.SchemaSQL, "schema.sql")
}

func TestMigrationsPermissionSpecCopyMatchesSpec(t *testing.T) {
	assertSameFile(t, filepath.Join("..", "..", "spec", "permission-points.json"),
		migrations.PermissionSpecJSON, "permission-points.json")
}

func assertSameFile(t *testing.T, specPath string, embedded []byte, label string) {
	t.Helper()
	want, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("读取 spec/%s 失败: %v", label, err)
	}
	if !bytes.Equal(want, embedded) {
		t.Fatalf("migrations/ 副本与 spec/%s 不一致（%d vs %d 字节）—— "+
			"必须以 spec 为准，改了 spec 却没同步副本（或反之）", label, len(want), len(embedded))
	}
}

// TestSpecFilesAreParseable 确保切分器对当前 spec 仍然有效（语句数 = 表数 + SET 语句）。
func TestSpecFilesAreParseable(t *testing.T) {
	stmts := splitSQL(string(migrations.SchemaSQL))
	creates := 0
	for _, s := range stmts {
		if createTableRe.MatchString(s) {
			creates++
		}
	}
	if creates != 38 {
		t.Fatalf("spec/schema.sql 应切出 38 条 CREATE TABLE，实际 %d（切分器或 spec 变了）", creates)
	}
	// 每条语句都必须以可执行文本收尾（不吞掉半截）
	for i, s := range stmts {
		if _, err := io.WriteString(io.Discard, s); err != nil || len(s) == 0 {
			t.Fatalf("第 %d 条语句为空", i)
		}
	}
}
