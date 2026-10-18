// Command jxdbtool —— scripts/reset_db.sh 的 SQL 执行后端（N-025 D2）。
//
// ★ 动因（如实）：测试服务器与本机都没有 mysql 客户端（服务器账号 chadhao 也无
// docker 权限、无免密 sudo），而重置脚本必须先能 dry-run 列出各表行数、再按固定
// 清单执行 TRUNCATE。本仓库已有纯 Go 的 MySQL 驱动（go-sql-driver/mysql，服务端
// 在用）⇒ 用它补一条最小执行通道，**不引入任何新依赖**。
//
// 用法（语句一律由 reset_db.sh 的固定清单给出，本工具不接受外部输入）：
//
//	jxdbtool -dsn "$DSN" -q 'SELECT COUNT(*) FROM t' [-q ...]   # 查询 ⇒ stdout 打 TSV
//	jxdbtool -dsn "$DSN" -x 'TRUNCATE TABLE t' [-x ...]         # 顺序执行，首错即停
//
// DSN 缺省取环境变量 JX_DB_DSN（与服务端同一口径）。退出码：0 成功；1 失败。
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "github.com/go-sql-driver/mysql"
)

// stringsFlag 支持同一 flag 重复出现（-q / -x 各收一串）。
type stringsFlag []string

func (f *stringsFlag) String() string { return strings.Join(*f, " | ") }
func (f *stringsFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func main() {
	var queries, execs stringsFlag
	dsn := flag.String("dsn", "", "MySQL DSN（缺省取环境变量 JX_DB_DSN）")
	flag.Var(&queries, "q", "只读查询：结果按 TSV 打到 stdout（可重复）")
	flag.Var(&execs, "x", "执行语句：顺序执行、首错即停（可重复）")
	flag.Parse()

	d := strings.TrimSpace(*dsn)
	if d == "" {
		d = strings.TrimSpace(os.Getenv("JX_DB_DSN"))
	}
	if d == "" {
		fatal("缺少 DSN：用 -dsn 或设置 JX_DB_DSN")
	}
	if len(queries) == 0 && len(execs) == 0 {
		fatal("至少要给一个 -q（查询）或 -x（执行）")
	}

	db, err := sql.Open("mysql", d)
	if err != nil {
		fatal("打开数据库失败: " + err.Error())
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // 运维一次性脚本：单连接足够，也让执行顺序确定
	if err := db.Ping(); err != nil {
		fatal("连接数据库失败: " + err.Error())
	}

	for _, q := range queries {
		if err := printTSV(db, q); err != nil {
			fatal("查询失败（" + q + "）: " + err.Error())
		}
	}
	for _, x := range execs {
		if _, err := db.Exec(x); err != nil {
			fatal("执行失败（" + x + "）: " + err.Error())
		}
	}
}

// printTSV 把查询结果按行打到 stdout：列以制表符分隔，NULL 打空串。
func printTSV(db *sql.DB, query string) error {
	rows, err := db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	vals := make([]sql.NullString, len(cols))
	args := make([]interface{}, len(cols))
	for i := range vals {
		args[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(args...); err != nil {
			return err
		}
		out := make([]string, len(cols))
		for i, v := range vals {
			out[i] = v.String
		}
		fmt.Println(strings.Join(out, "\t"))
	}
	return rows.Err()
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "jxdbtool: "+msg)
	os.Exit(1)
}
