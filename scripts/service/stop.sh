#!/usr/bin/env bash
# stop.sh —— jx-lab-trace 测试服务器停止脚本（SIGTERM → 等待 → SIGKILL 兜底）
#
# ★ 规范沿用 `~/services/jxapproval/`：按 `run.pid` 停，**只动自己的进程**。
# ★★ 两条实测教训：
#   ① **`run.pid` 可能过期**（进程已死而文件还在）⇒ 必须 `kill -0` 复核，
#      并以"进程实际是否还在"为准，不能只信文件（COLLAB `N-005`）；
#   ② ★ **停完必须等端口真正释放**再报成功 —— 否则紧接着的启动会
#      `bind: address already in use` 而**静默失败**（我 2026-10-09 实测踩过：
#      旧进程还在时起新的，新进程 exit、`run.pid` 记账错乱，
#      最后得出"PID 没变 ⇒ 重启没发生"的假结论）。
set -u

APP_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
cd "$APP_DIR" || exit 1

PORT="${JX_PORT:-18080}"
ADDR="127.0.0.1:$PORT"

pid=""
[ -f ./run.pid ] && pid="$(cat ./run.pid 2>/dev/null || true)"

if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
  kill "$pid" 2>/dev/null
  for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
  if kill -0 "$pid" 2>/dev/null; then
    echo "stop.sh: PID $pid 未在 10s 内退出，SIGKILL" >&2
    kill -9 "$pid" 2>/dev/null
    sleep 1
  fi
  echo "stop.sh: 已停止 PID $pid"
else
  echo "stop.sh: run.pid 指向的进程不在（陈旧的 run.pid）—— 按端口兜底清理"
fi

# ★ 端口兜底：只杀**监听该端口的 jxlabtrace/reportd 自身**，绝不碰别的服务
for p in $(ss -lntpH 2>/dev/null | grep " $ADDR " | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | sort -u); do
  if ps -p "$p" -o cmd= 2>/dev/null | grep -qE 'bin/(jxlabtrace|reportd)'; then
    echo "stop.sh: 端口兜底，停止 PID $p"
    kill "$p" 2>/dev/null; sleep 1; kill -9 "$p" 2>/dev/null
  else
    echo "stop.sh: 警告 —— $ADDR 被**非本项目**进程 $p 占用，不处理" >&2
  fi
done

# ★ 等端口真空出来（最多 10s）—— 这是"重启真的发生了"的前提
for _ in $(seq 1 20); do
  ss -lntH 2>/dev/null | grep -q " $ADDR " || { echo "stop.sh: 端口 $ADDR 已释放 ✓"; exit 0; }
  sleep 0.5
done
echo "stop.sh: 警告 —— 端口 $ADDR 仍被占用" >&2
exit 1
