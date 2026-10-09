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
REPORTD_NAME="reportd"                                # 批 8 · M9 公网侧静态服务（零 DB）
LOCAL_BIN="bin/${BIN_NAME}-linux-amd64"
LOCAL_REPORTD="bin/${REPORTD_NAME}-linux-amd64"
ENV_FILE=".env.deploy"
PORT="${JX_DEPLOY_PORT:-18080}"
REPORTD_PORT="${JX_REPORTD_PORT:-18090}"
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
# 批 8 · reportd（公网侧静态服务，零 DB 依赖，只服务 served/）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GO" build -trimpath \
  -ldflags "-s -w" \
  -o "$LOCAL_REPORTD" ./cmd/reportd
ls -l "$LOCAL_REPORTD" | awk '{print "      "$5" bytes  "$9}'

echo "[2/4] 确保服务器目录存在（家目录下，免 sudo）"
ssh "${SSH_OPTS[@]}" "$HOST" "mkdir -p ~/$REMOTE_DIR/{bin,logs,attachments,reports/served,reports/_inactive} && echo ok"

# ★ [2.5/4]（mimo 批 1 修，见 COLLAB N-003）：要重启就必须**先停旧进程再上传** ——
#   旧进程正在执行 bin/jxlabtrace 时，scp 对该文件 O_TRUNC ⇒ Linux 返回
#   ETXTBSY（Text file busy）⇒ 上传失败 ⇒ set -e 退出 ⇒ **重启根本没发生**，
#   「A4 会话跨重启」会因此变成**没有重启的假绿**。实测留档见 N-003。
#
# ★★ [2.5/4] 加固（WorkBuddy，2026-10-09，见 COLLAB N-005）：**只在 run.pid 准确时才有效**。
#   实测反证（N-005 探针 A，2026-10-09 03:1x）：run.pid 陈旧（2383903 已死）而真实监听进程
#   （2384342，「/proc/<pid>/exe」→ 本目录「bin/jxlabtrace」）仍持有该二进制 ⇒ 旧逻辑打印
#   「无旧进程」⇒ [3/4] scp 照旧「dest open ... Failure」（ETXTBSY）⇒ set -e 退出。
#   ★ 触发条件并不罕见：**「A4 会话跨重启」的验收手法本身就是手动 kill + 手动起进程**，
#     手动起的进程不会回写 run.pid ⇒ 下一次「--restart」必然踩中。
#   ⇒ 修法：run.pid 无效时**兜底扫描**——按「/proc/<pid>/exe」**路径精确等于**本目录二进制
#     来判定「该停谁」，**只动自己的进程**，绝不误伤 jxapproval / RustFS / hnyc-erp。
if [ "$DO_RESTART" = 1 ]; then
  echo "[2.5/4] 停远端旧进程（只动本目录二进制持有者，不碰 jxapproval / RustFS / hnyc-erp）"
  ssh "${SSH_OPTS[@]}" "$HOST" "bash -s" <<REMOTE
set -u
cd ~/$REMOTE_DIR
SELF_BIN="\$HOME/$REMOTE_DIR/bin/$BIN_NAME"
pid="\$(cat run.pid 2>/dev/null || true)"
if [ -n "\$pid" ] && kill -0 "\$pid" 2>/dev/null; then
  kill "\$pid"; sleep 1
  kill -0 "\$pid" 2>/dev/null && kill -9 "\$pid" 2>/dev/null || true
  echo "旧进程已停止（PID \$pid，来自 run.pid）"
else
  echo "run.pid 无效（值为 '\${pid:-空}'）⇒ 兜底扫描持有本目录二进制的进程"
  stopped=""
  for p in /proc/[0-9]*; do
    kp="\${p#/proc/}"
    [ "\$kp" = "\$\$" ] && continue
    exe="\$(readlink "\$p/exe" 2>/dev/null || true)"
    [ "\$exe" = "\$SELF_BIN" ] || continue
    kill "\$kp" 2>/dev/null; sleep 1
    kill -0 "\$kp" 2>/dev/null && kill -9 "\$kp" 2>/dev/null || true
    stopped="\$stopped \$kp"
  done
  if [ -n "\$stopped" ]; then
    echo "已兜底停止持有 \$SELF_BIN 的进程：\$stopped"
  else
    echo "确认无旧进程（无需停止）"
  fi
fi
REMOTE

# 批 8：reportd 同样要停（独立 reportd.pid；★ 兜底扫描按 /proc/<pid>/exe 精确匹配
#   本目录 bin/reportd，只停自己的进程）
ssh "${SSH_OPTS[@]}" "$HOST" "bash -s" <<REMOTE_REPORTD
set -u
cd ~/$REMOTE_DIR
SELF_REPORTD="\$HOME/$REMOTE_DIR/bin/$REPORTD_NAME"
pid="\$(cat reportd.pid 2>/dev/null || true)"
if [ -n "\$pid" ] && kill -0 "\$pid" 2>/dev/null; then
  kill "\$pid"; sleep 1
  kill -0 "\$pid" 2>/dev/null && kill -9 "\$pid" 2>/dev/null || true
  echo "旧 reportd 已停止（PID \$pid）"
else
  stopped=""
  for p in /proc/[0-9]*; do
    kp="\${p#/proc/}"
    [ "\$kp" = "\$\$" ] && continue
    exe="\$(readlink "\$p/exe" 2>/dev/null || true)"
    [ "\$exe" = "\$SELF_REPORTD" ] || continue
    kill "\$kp" 2>/dev/null; sleep 1
    kill -0 "\$kp" 2>/dev/null && kill -9 "\$kp" 2>/dev/null || true
    stopped="\$stopped \$kp"
  done
  if [ -n "\$stopped" ]; then
    echo "已兜底停止 reportd 进程：\$stopped"
  else
    echo "确认无旧 reportd"
  fi
fi
REMOTE_REPORTD
fi

echo "[3/4] 上传二进制与运行配置"
scp "${SSH_OPTS[@]}" "$LOCAL_BIN" "$HOST:~/$REMOTE_DIR/bin/$BIN_NAME" >/dev/null
scp "${SSH_OPTS[@]}" "$LOCAL_REPORTD" "$HOST:~/$REMOTE_DIR/bin/$REPORTD_NAME" >/dev/null
scp "${SSH_OPTS[@]}" "$ENV_FILE"  "$HOST:~/$REMOTE_DIR/.env" >/dev/null
ssh "${SSH_OPTS[@]}" "$HOST" "chmod 700 ~/$REMOTE_DIR/bin/$BIN_NAME ~/$REMOTE_DIR/bin/$REPORTD_NAME && chmod 600 ~/$REMOTE_DIR/.env && echo '权限已收紧（二进制 700 / 配置 600）'"

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
# ★ 载入前先做**语法自检**（WorkBuddy，2026-10-09，见 COLLAB N-005）：
#   「.env」是被当 shell 片段 source 的，一行写错（实测：DSN 被写成双重引号 ""...""）
#   会让「. ./.env」报「syntax error near unexpected token」而**失败**；
#   但旧逻辑只有 set -u、**没有 set -e** ⇒ 其后所有赋值全部落空、服务照旧起来、
#   「/healthz」照旧 200 ⇒ **配置错误被静默吞掉（假绿）**。
#   ⇒ 现在：先「bash -n」语法自检，不过就**拒绝启动**（宁可红，不要假绿）。
# ★★ 注意：本文件的两处 heredoc 均为**不加引号**的 <<REMOTE ⇒ heredoc 体内
#   **反引号与未转义的美元符会在【本机】被执行/展开**（实测踩坑：注释里的反引号
#   被本机当命令替换跑掉，连带把远端脚本搞花）。⇒ 注释里一律用「」，不要用反引号。
if ! bash -n ./.env 2>/tmp/.jx_env_syntax.err; then
  echo "!!! ./.env 语法自检未通过 —— 拒绝以错误配置启动：" >&2
  cat /tmp/.jx_env_syntax.err >&2
  exit 1
fi
set -a; . ./.env; set +a
export JX_HTTP_ADDR="\${JX_HTTP_ADDR:-127.0.0.1:$PORT}"
export JX_ATTACH_DIR="\${JX_ATTACH_DIR:-$PWD/attachments}"
# 批 8 · reportd 运行参数（取值一律加双引号，N-004 判例）
# ★ \$PWD 必须转义 ⇒ 在【远端】展开；不转义会被本机 Git-BASH 展开成 /c/... 而落到无权限路径
#   （2026-10-09 实测：JX_REPORT_DIR 缺省时 mkdir /c: permission denied ⇒ reportd 起不来）。
export JX_REPORT_DIR="\${JX_REPORT_DIR:-\$PWD/reports}"
export JX_REPORTD_ADDR="\${JX_REPORTD_ADDR:-127.0.0.1:$REPORTD_PORT}"
export JX_REPORT_PUBLIC_BASE="\${JX_REPORT_PUBLIC_BASE:-http://127.0.0.1:$REPORTD_PORT/r}"
nohup ./bin/$BIN_NAME >> logs/app.log 2>&1 &
echo \$! > run.pid
sleep 2
if kill -0 "\$(cat run.pid)" 2>/dev/null; then
  echo "已启动（PID \$(cat run.pid)，监听 \${JX_HTTP_ADDR}）"
else
  echo "!!! 启动失败，日志尾部：" >&2; tail -20 logs/app.log >&2; exit 1
fi
# —— reportd：公网侧静态服务（零 DB；只服务 \$JX_REPORT_DIR/served/）——
# ★ env -u JX_DB_DSN：启动前**显式剔除 DSN** —— 进程环境里连 DSN 都不带，
#   把「公网侧不连内网库」从「代码不读」加强到「环境也没有」（A10 反证）。
env -u JX_DB_DSN nohup ./bin/$REPORTD_NAME >> logs/reportd.log 2>&1 &
echo \$! > reportd.pid
sleep 1
if kill -0 "\$(cat reportd.pid)" 2>/dev/null; then
  echo "reportd 已启动（PID \$(cat reportd.pid)，监听 \${JX_REPORTD_ADDR}，目录 \${JX_REPORT_DIR}）"
else
  echo "!!! reportd 启动失败，日志尾部：" >&2; tail -20 logs/reportd.log >&2; exit 1
fi
REMOTE
fi

if [ "$DO_SMOKE" = 1 ]; then
  echo "[smoke] 健康检查（在服务器上，★ 两个进程分列断言）"
  ssh "${SSH_OPTS[@]}" "$HOST" "bash -s" <<REMOTE
set -u
cd ~/$REMOTE_DIR
# ★ N-004 联动（WorkBuddy，2026-10-09）：模板现已要求取值一律加双引号 ⇒ 这里**必须先剥引号**，
#   否则 cut -d= -f2- 会带上字面引号，拼出 http://"127.0.0.1:18080"/healthz 而 curl 解析失败。
#   tr 用八进制转义（\042=" / \047='）书写，避免在 heredoc 里出现字面引号。
ADDR="\$(grep -E '^[[:space:]]*JX_HTTP_ADDR[[:space:]]*=' .env 2>/dev/null | head -n1 | cut -d= -f2- | tr -d '\\042\\047' | tr -d '[:space:]')"
ADDR="\${ADDR:-127.0.0.1:$PORT}"
echo "=== 主服务（$BIN_NAME） ==="
echo "--- /healthz ---"
curl -s -m 5 -o /dev/null -w 'http=%{http_code}\n' "http://\$ADDR/healthz" || echo "curl 失败"
curl -s -m 5 "http://\$ADDR/healthz" | head -c 400; echo
echo "--- /api/me（dev 模式应返回身份） ---"
curl -s -m 5 "http://\$ADDR/api/me" | head -c 400; echo
echo "--- 端口归属（确认只绑回环） ---"
ss -lntp 2>/dev/null | grep "\$ADDR" || echo "(未检出监听)"

echo "=== reportd（$REPORTD_NAME，零 DB 的公网侧静态服务） ==="
RADDR="\$(grep -E '^[[:space:]]*JX_REPORTD_ADDR[[:space:]]*=' .env 2>/dev/null | head -n1 | cut -d= -f2- | tr -d '\\042\\047' | tr -d '[:space:]')"
RADDR="\${RADDR:-127.0.0.1:$REPORTD_PORT}"
echo "--- reportd /healthz（应 200 且 body=ok） ---"
curl -s -m 5 -o /dev/null -w 'http=%{http_code}\n' "http://\$RADDR/healthz" || echo "curl 失败"
curl -s -m 5 "http://\$RADDR/healthz" | head -c 100; echo
echo "--- reportd 未命中的链接应 404（不查库、纯静态） ---"
curl -s -m 5 -o /dev/null -w 'http=%{http_code}\n' "http://\$RADDR/r/nonexistent0000000000000000000000000.html"
echo "--- reportd 端口归属（确认只绑回环） ---"
ss -lntp 2>/dev/null | grep "\$RADDR" || echo "(未检出监听)"
REMOTE
fi

echo "完成。★ 后续所有『跑起来』的操作都在服务器上；本机只做编译与静态检查。"
