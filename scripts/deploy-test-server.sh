#!/usr/bin/env bash
# deploy-test-server.sh —— 把服务部署到**测试服务器**并重启。
#
# ★★ 硬约束（用户 2026-10-09）：**系统调试一律在测试服务器上进行，不在本机进行。**
#    本机只做：编辑 / 编译 / 纯单元测试 / 静态检查 / 门禁。**要 listen 或要改远端状态的，都在这边做。**
#    详见 docs/05-环境与调试约定.md。
#
# 用法：
#   bash scripts/deploy-test-server.sh                  # 只编译 + 上传
#   bash scripts/deploy-test-server.sh --restart        # 编译 + 上传 + 重启服务
#   bash scripts/deploy-test-server.sh --restart --smoke # 再加健康检查
#
# 前置：本机存在 `.env.deploy`（已被 .gitignore 覆盖），内容形如
#   JX_DB_DSN=jx_lab:***@tcp(192.168.10.50:3306)/jx_lab_trace?charset=utf8mb4&parseTime=true&loc=Local
#   JX_DEV_MODE=true
#   ...
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

HOST="${JX_DEPLOY_HOST:-chadhao@192.168.10.50}"
REMOTE_DIR="${JX_DEPLOY_DIR:-jx-lab-trace}"          # 相对家目录，免 sudo
BIN_NAME="jxlabtrace"
LOCAL_BIN="bin/${BIN_NAME}-linux-amd64"
ENV_FILE=".env.deploy"
PORT="${JX_DEPLOY_PORT:-18080}"
SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15)

DO_RESTART=0; DO_SMOKE=0
for a in "$@"; do
  case "$a" in
    --restart) DO_RESTART=1 ;;
    --smoke)   DO_SMOKE=1 ;;
    *) echo "未知参数：$a" >&2; exit 2 ;;
  esac
done

[ -f "$ENV_FILE" ] || { echo "✗ 缺少 $ENV_FILE（可从 .env.deploy.example 复制后填写）；★ 该文件不进版本库" >&2; exit 1; }

GO="${GO:-go}"
command -v "$GO" >/dev/null 2>&1 || GO=/c/go/bin/go

echo "[1/4] 交叉编译（linux/amd64，纯 Go 驱动 ⇒ 无 cgo）"
mkdir -p bin
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GO" build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
  -o "$LOCAL_BIN" ./cmd/jxlabtrace
ls -l "$LOCAL_BIN" | awk '{print "      "$5" bytes  "$9}'

echo "[2/4] 确保服务器目录存在（家目录下，免 sudo）"
ssh "${SSH_OPTS[@]}" "$HOST" "mkdir -p ~/$REMOTE_DIR/{bin,logs,attachments} && echo ok"

echo "[3/4] 上传二进制与运行配置"
scp "${SSH_OPTS[@]}" "$LOCAL_BIN" "$HOST:~/$REMOTE_DIR/bin/$BIN_NAME" >/dev/null
scp "${SSH_OPTS[@]}" "$ENV_FILE"  "$HOST:~/$REMOTE_DIR/.env" >/dev/null
ssh "${SSH_OPTS[@]}" "$HOST" "chmod 700 ~/$REMOTE_DIR/bin/$BIN_NAME && chmod 600 ~/$REMOTE_DIR/.env && echo '权限已收紧（二进制 700 / 配置 600）'"

if [ "$DO_RESTART" = 1 ]; then
  echo "[4/4] 重启服务（在服务器上）"
  ssh "${SSH_OPTS[@]}" "$HOST" "bash -s" <<REMOTE
set -u
cd ~/$REMOTE_DIR
# 停掉旧进程（按 PID 文件；★ 只动我们自己的进程，不碰 jxapproval / RustFS / hnyc-erp 等）
if [ -f run.pid ] && kill -0 "\$(cat run.pid)" 2>/dev/null; then
  kill "\$(cat run.pid)"; sleep 1
  kill -0 "\$(cat run.pid)" 2>/dev/null && kill -9 "\$(cat run.pid)" 2>/dev/null || true
  echo "旧进程已停止（PID \$(cat run.pid)）"
else
  echo "无旧进程"
fi
# 起新进程：★ 只绑回环，通过反代对外；不往公网多开端口
set -a; . ./.env; set +a
export JX_HTTP_ADDR="\${JX_HTTP_ADDR:-127.0.0.1:$PORT}"
export JX_ATTACH_DIR="\${JX_ATTACH_DIR:-$PWD/attachments}"
nohup ./bin/$BIN_NAME >> logs/app.log 2>&1 &
echo \$! > run.pid
sleep 2
if kill -0 "\$(cat run.pid)" 2>/dev/null; then
  echo "已启动（PID \$(cat run.pid)，监听 \${JX_HTTP_ADDR}）"
else
  echo "!!! 启动失败，日志尾部：" >&2; tail -20 logs/app.log >&2; exit 1
fi
REMOTE
fi

if [ "$DO_SMOKE" = 1 ]; then
  echo "[smoke] 健康检查（在服务器上）"
  ssh "${SSH_OPTS[@]}" "$HOST" "bash -s" <<REMOTE
set -u
cd ~/$REMOTE_DIR
ADDR="\$(grep -E '^JX_HTTP_ADDR=' .env 2>/dev/null | cut -d= -f2-)"; ADDR="\${ADDR:-127.0.0.1:$PORT}"
echo "--- /healthz ---"
curl -s -m 5 -o /dev/null -w 'http=%{http_code}\n' "http://\$ADDR/healthz" || echo "curl 失败"
curl -s -m 5 "http://\$ADDR/healthz" | head -c 400; echo
echo "--- /api/me（dev 模式应返回身份） ---"
curl -s -m 5 "http://\$ADDR/api/me" | head -c 400; echo
echo "--- 端口归属（确认只绑回环） ---"
ss -lntp 2>/dev/null | grep "\$ADDR" || echo "(未检出监听)"
REMOTE
fi

echo "完成。★ 后续所有『跑起来』的操作都在服务器上；本机只做编译与静态检查。"
