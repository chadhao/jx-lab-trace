#!/usr/bin/env bash
# start.sh —— jx-lab-trace 测试服务器启动脚本
#
# ★ 规范沿用 `~/services/jxapproval/`（并在其上补两条本项目特有的教训）：
#   ① 目录 = 脚本所在目录；
#   ② **先注入 `.env`，缺它拒绝启动**（失败可见，绝不静默退回默认配置）；
#   ③ `echo $$ > run.pid`（`exec` 后 $$ 不变 ⇒ run.pid 即最终服务进程）；
#   ④ ★ **日志固定落 `logs/app.log`**，由本脚本负责重定向 ——
#      不许依赖"启动时记得加 `> log`"，那是一条"记不住就静默丢日志"的坑；
#   ⑤ 用 `exec` 让 PID 稳定（便于 `kill $(cat run.pid)`）。
#
# ★★ 本项目特有（`docs/05` 环境与调试约定）：
#   · **只绑回环** —— JX_HTTP_ADDR 默认 `127.0.0.1:18080`，对外由 Caddy 反代；
#   · **绝不 source 后忘了引号** —— `.env` 的值一律带双引号（含 `&` 的 DSN 尤其，
#     见 COLLAB `N-004`：未加引号时 `set -a; . ./.env` 会把赋值丢进后台子 shell）。
set -u

APP_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
cd "$APP_DIR" || exit 1

if [ ! -f ./.env ]; then
  echo "start.sh: ./.env 不存在，拒绝启动（缺它即丢全部配置）" >&2
  exit 1
fi

# ★ .env 语法自检：含无引号的 `&`/`#`/空格 的值会在 source 时被吞掉，这里先拦住。
if grep -nE '^[A-Za-z_][A-Za-z0-9_]*=([^"'"'"']*[&#][^"'"'"']*)$' ./.env >/dev/null 2>&1; then
  echo "start.sh: ./.env 有未加引号且含 & 或 # 的值 —— 请在值两侧加双引号（见 COLLAB N-004）：" >&2
  grep -nE '^[A-Za-z_][A-Za-z0-9_]*=([^"'"'"']*[&#][^"'"'"']*)$' ./.env >&2
  exit 1
fi

set -a
. ./.env
set +a

if [ ! -x ./bin/jxlabtrace ]; then
  echo "start.sh: ./bin/jxlabtrace 不存在或不可执行" >&2
  exit 1
fi

echo $$ > ./run.pid

LOG_DEST="${JX_LOG_DEST:-$APP_DIR/logs/app.log}"
mkdir -p "$(dirname "$LOG_DEST")"

exec ./bin/jxlabtrace >>"$LOG_DEST" 2>&1
