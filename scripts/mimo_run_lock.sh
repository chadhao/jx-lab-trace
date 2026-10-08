#!/usr/bin/env bash
# mimo_run_lock.sh —— **轮次排他锁**：保证「同一工作区同一时刻只有一个驱动在推进」。
#
# ★ 为什么需要它：
#   原先靠「探活」(`tasklist | grep mimo`) 判断有没有轮次在跑 —— 那是**启发式**，不是**排他**。
#   判据一旦误判（进程名匹配不到 / PID 复用 / 编码问题 / 探活命令本身失败），
#   心跳就会**起第二个驱动** ⇒ 两个 mimo 同时改同一工作区 ⇒ 互相覆盖
#   （2026-10-09 已实测 `COLLAB.md` 被整体重写覆盖的事故；亦有"双树并发同一仓库"的前科）。
#   ⇒ ★ **约定（"先探活再派工"）没有执行体，等于没有约定。**
#
# ★ 锁的键（关键）：**要串行化的是「工作区」，不是「驱动进程」**。
#   驱动被杀、但 `mimo.exe` 子进程仍在跑 —— 这工作区**仍然危险**。
#   故 liveness ＝ **驱动存活 或 mimo 存活**，两者都死才算陈锁。
#
# ★★ 首版踩过的坑（2026-10-09 实测，本版已修）：
#   首版用裸 `mkdir "$LOCKDIR"`，而父目录 `.run/` 尚未创建 ⇒ **mkdir 必然失败**；
#   却把「mkdir 失败」一律当成「锁被占用」⇒ 走陈锁分支 ⇒ `rm -rf`（同样没成功）
#   ⇒ `exec "$0"` 无限递归，**刷屏并空转**。
#   ⇒ ★ 判据：**必须把「预期的失败」与「意外的失败」分开** ——
#      `mkdir` 失败后要**再查 `[ -d "$LOCKDIR" ]`** 才能判定"锁真的存在"；
#      且**禁止无界递归**（改为有界重试）。
#
# 用法：
#   bash scripts/mimo_run_lock.sh acquire <议题ID>   # 0=成功，1=已被占，2=环境异常
#   bash scripts/mimo_run_lock.sh release            # 只释放自己持有的
#   bash scripts/mimo_run_lock.sh status             # 0=有活跃轮次，1=无锁/陈锁
#   bash scripts/mimo_run_lock.sh setmimo <pid>      # 驱动起完 mimo 后回填其 PID
#   bash scripts/mimo_run_lock.sh selftest           # ★ 自测（七探针），0=全过
#
# 锁位置：`.run/mimo.lock/`（`mkdir` 原子性；已在 .gitignore 内）
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCKDIR="$ROOT/.run/mimo.lock"
META="$LOCKDIR/meta"

_alive() { [ -n "${1:-}" ] && [ "$1" != "0" ] && kill -0 "$1" 2>/dev/null; }
_read()  { [ -f "$META" ] && sed -n "s/^$1=//p" "$META" 2>/dev/null | head -1 || true; }

_pid_alive_any() {
  _alive "$(_read driver_pid)" || _alive "$(_read mimo_pid)"
}

_write_meta() {
  {
    echo "issue=$1"
    echo "driver_pid=$2"
    echo "mimo_pid=0"
    echo "started=$(date +%s)"
    echo "started_at=$(date '+%F %T')"
  } > "$META"
}

cmd_acquire() {
  local issue="${1:?用法: acquire <议题ID> [owner-pid]}"
  # ★★ owner 必须是**长命进程**的 PID（驱动自己），**不能默认成 $$** ——
  #   因为 `acquire` 是被驱动当**子命令**调用的，它的 `$$` **一返回就退出**
  #   ⇒ liveness 永远为假 ⇒ 锁永远被判陈锁 ⇒ **完全不起排他作用**（实测踩过：第二次 acquire 竟然成功）。
  #   ⇒ 故必须由调用方显式传入（驱动传自己的 `$$`）。默认值仅在人工调试时使用。
  local owner="${2:-$$}"
  # ★ 先确保父目录存在 —— 首版就是漏了这一步，导致 mkdir 必然失败并被误判为"锁被占"
  mkdir -p "$(dirname "$LOCKDIR")" 2>/dev/null
  [ -d "$(dirname "$LOCKDIR")" ] || { echo "✗ 锁父目录不可创建：$(dirname "$LOCKDIR")"; return 2; }

  local took_over=0
  while :; do
    if mkdir "$LOCKDIR" 2>/dev/null; then
      _write_meta "$issue" "$owner"
      echo "✓ 已获得轮次锁（议题 $issue · owner PID $owner）"
      return 0
    fi
    # ★ mkdir 失败 ≠ 锁存在：必须复核目录是否真的在
    if [ ! -d "$LOCKDIR" ]; then
      echo "✗ 建锁失败，但锁目录并不存在 —— 环境异常（权限/路径），并非「被占用」"
      return 2
    fi
    if _pid_alive_any; then
      echo "✗ 轮次锁已被占用：issue=$(_read issue) driver=$(_read driver_pid) mimo=$(_read mimo_pid) since=$(_read started_at)"
      return 1
    fi
    if [ "$took_over" -ge 1 ]; then
      echo "✗ 清除陈锁后仍建锁失败（可能被并发抢走）—— 放弃，避免无界重试"
      return 1
    fi
    took_over=1
    echo "⚠ 检出**陈锁**（驱动与 mimo 均已不在）—— 清除后重试一次：旧 issue=$(_read issue) since=$(_read started_at)"
    rm -rf "$LOCKDIR" 2>/dev/null
    if [ -d "$LOCKDIR" ]; then
      echo "✗ 陈锁目录无法清除（rm 未生效）—— 拒绝继续（不做无界重试）"
      return 2
    fi
  done
}

cmd_setmimo() {
  local pid="${1:?用法: setmimo <pid>}"
  [ -f "$META" ] || { echo "无锁"; return 1; }
  sed -i "s/^mimo_pid=.*/mimo_pid=$pid/" "$META"
}

cmd_release() {
  [ -d "$LOCKDIR" ] || { echo "○ 无锁（无需释放）"; return 0; }
  local d; d="$(_read driver_pid)"
  if [ -n "$d" ] && [ "$d" != "$$" ] && _alive "$d"; then
    echo "✗ 锁属于另一个存活驱动（PID $d）—— 拒绝释放"
    return 1
  fi
  rm -rf "$LOCKDIR" 2>/dev/null
  [ -d "$LOCKDIR" ] && { echo "✗ 释放失败（目录仍在）"; return 1; }
  echo "✓ 已释放轮次锁"
}

cmd_status() {
  [ -d "$LOCKDIR" ] || { echo "○ 无锁"; return 1; }
  local started now dur
  started="$(_read started)"; now="$(date +%s)"; dur=$(( now - ${started:-$now} ))
  if _pid_alive_any; then
    echo "● 有轮次在跑：issue=$(_read issue) driver=$(_read driver_pid) mimo=$(_read mimo_pid) 起于 $(_read started_at) 已 ${dur}s"
    return 0
  fi
  echo "○ 陈锁（无存活进程）：issue=$(_read issue) 起于 $(_read started_at) —— 可抢占"
  return 1
}

# ── ★ 自测：把探针固化进脚本，任何人可复跑 ────────────────────────────
cmd_selftest() {
  local pass=0 fail=0
  _t() { # _t <期望RC> <描述> <命令...>
    local want="$1" desc="$2"; shift 2
    "$@" >/dev/null 2>&1; local got=$?
    if [ "$got" = "$want" ]; then echo "  ok   [$desc] RC=$got"; pass=$((pass+1))
    else echo "  FAIL [$desc] 期望 RC=$want 实际 RC=$got"; fail=$((fail+1)); fi
  }
  rm -rf "$LOCKDIR" 2>/dev/null
  echo "[1] 初始无锁"
  _t 1 "status 无锁"        bash "$0" status
  echo "[2] 抢锁 / 并发拒绝（★ owner 用长命进程模拟真实驱动）"
  sleep 300 & local OWNER=$!
  _t 0 "acquire 首次成功"    bash "$0" acquire N-TEST "$OWNER"
  _t 1 "acquire 二次被拒"    bash "$0" acquire N-OTHER "$OWNER"
  _t 0 "status 有轮次"       bash "$0" status
  # ★ 期望修正（首版写错的是**测试**、不是实现）：owner 仍存活时，`release` **应当拒绝**
  #   —— 它不该释放"别人的锁"。所以要**先让 owner 死掉**，再验 release 成功。
  _t 1 "release 拒绝释放他人锁（owner 存活）" bash "$0" release
  kill "$OWNER" 2>/dev/null; sleep 1
  _t 0 "release 成功（owner 已死）"  bash "$0" release
  echo "[3] 陈锁自愈（有界，不递归）"
  mkdir -p "$LOCKDIR"
  printf 'issue=N-OLD\ndriver_pid=999999\nmimo_pid=999998\nstarted=1\nstarted_at=1970-01-01 00:00\n' > "$META"
  _t 1 "status 报陈锁"       bash "$0" status
  _t 0 "acquire 抢占陈锁"    bash "$0" acquire N-TAKE
  _t 0 "release 清理"        bash "$0" release
  echo "[4] ★ 回归：父目录不存在时 acquire 应成功（首版无限递归的成因）"
  rm -rf "$LOCKDIR" "$(dirname "$LOCKDIR")"
  _t 0 "acquire 自动建父目录" bash "$0" acquire N-NEWDIR
  _t 0 "release"             bash "$0" release
  echo "[5] ★ 回归：owner 已死 ⇒ 应判陈锁并可抢占（模拟驱动被杀）"
  sleep 300 & local OWNER2=$!
  bash "$0" acquire N-DEAD "$OWNER2" >/dev/null 2>&1
  kill -9 "$OWNER2" 2>/dev/null; sleep 1
  _t 1 "status 报陈锁"        bash "$0" status
  _t 0 "acquire 抢占"         bash "$0" acquire N-NEXT
  _t 0 "release"             bash "$0" release
  echo
  echo "  自测结果：通过 $pass / 失败 $fail"
  [ "$fail" -eq 0 ] || return 1
  return 0
}

case "${1:-}" in
  acquire) shift; cmd_acquire "$@" ;;
  release) cmd_release ;;
  status)  cmd_status ;;
  setmimo) shift; cmd_setmimo "$@" ;;
  selftest) cmd_selftest ;;
  *) echo "用法: $0 {acquire <议题ID>|release|status|setmimo <pid>|selftest}" >&2; exit 2 ;;
esac
