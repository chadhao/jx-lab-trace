// Command jxq 是 scripts/check_perm_registry.py 判据④ 的**查库辅助**。
//
// ★ 为什么需要它：Python 侧没有（也不想新增）MySQL 驱动依赖 ——
//
//	门禁复用 Go 侧已有的 go-sql-driver，输出三个计数行给脚本比对。
//
// ★ 计数口径与 spec 对齐：权限点 / 角色 / 授权行数。
//
// 用法：JX_DB_DSN=... go run ./scripts/jxq
// 输出：points=51\nroles=6\nrole_perm=306
// 退出码：0 = 成功；2 = 缺 DSN；1 = 查询失败。
package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	dsn := strings.TrimSpace(os.Getenv("JX_DB_DSN"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "JX_DB_DSN 未设置")
		os.Exit(2)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开连接失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "PING 失败: %v\n", err)
		os.Exit(1)
	}

	counts := make(map[string]int, 3)
	for _, t := range []string{"s_permission_point", "s_role", "s_role_permission"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM `" + t + "`").Scan(&n); err != nil {
			fmt.Fprintf(os.Stderr, "统计 %s 失败: %v\n", t, err)
			os.Exit(1)
		}
		counts[t] = n
	}
	fmt.Printf("points=%d\nroles=%d\nrole_perm=%d\n",
		counts["s_permission_point"], counts["s_role"], counts["s_role_permission"])
}
