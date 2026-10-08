// Command jxlabtrace 是「实验检测数据追踪系统」的服务端入口。
//
// ★ 本文件当前是**最小骨架**：只保证 `go build ./...` 可过、门禁可从第一天起跑绿。
//
//	真实的配置装配 / 迁移 / HTTP 服务由 **批 1（M0 地基）** 落地 —— 见 MIMO-NEXT-BATCH-01.md。
package main

import (
	"flag"
	"fmt"
	"os"
)

// version 由构建脚本注入（-ldflags "-X main.version=..."）。
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	fmt.Fprintln(os.Stderr, "jxlabtrace: 骨架尚未装配业务（批 1 将落地 M0 地基，见 MIMO-NEXT-BATCH-01.md）")
	fmt.Fprintf(os.Stderr, "version=%s\n", version)
}
