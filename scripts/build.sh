#!/usr/bin/env bash
# scripts/build.sh —— 构建单二进制（前端内嵌）。
#
# 步骤：① vite build（web/）→ ② 把 dist 拷进 internal/webui/dist → ③ go build（//go:embed 内嵌）
# ★ 前端内嵌型项目的铁律：**改了 web/src/ 必须重建 dist**，否则部署上去仍是旧页面。
#
# 用法：bash scripts/build.sh [版本号]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
GO="${GO:-go}"
command -v "$GO" >/dev/null 2>&1 || GO=/c/go/bin/go

DIST_DST="internal/webui/dist"
mkdir -p "$DIST_DST" bin

if [ -f web/package.json ]; then
  echo "[1/3] vite build (web/)"
  ( cd web && npm install --silent && npm run build )
  echo "[2/3] 拷贝 dist → $DIST_DST"
  rm -rf "$DIST_DST"/*
  cp -r web/dist/. "$DIST_DST"/
else
  echo "[1/3] 跳过：web/ 尚未初始化（批 2 起）"
  echo "[2/3] 跳过"
fi

echo "[3/3] go build → bin/jxlabtrace"
"$GO" build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o bin/jxlabtrace ./cmd/jxlabtrace
echo "完成：bin/jxlabtrace（version=$VERSION）"
