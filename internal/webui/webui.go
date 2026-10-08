// Package webui 内嵌前端构建产物（Vue3 + Vite → web/dist → 本目录）。
// ★ go:embed 不能跨目录向上引用，故由 scripts/build.sh 把 web/dist 拷进 dist/。
package webui

import "embed"

// Files 是内嵌的前端文件系统，路径前缀为 "dist/"。
//
//go:embed all:dist
var Files embed.FS
