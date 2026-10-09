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
#   bash scripts/mimo_run_lock.sh acquire <议题ID> [owner-pid]   # 驱动锁：0=成功，1=被占，2=异常
#   bash scripts/mimo_run_lock.sh acquire-writer <名字> [ttl秒]   # ★ 写者锁（心跳/主会话）
#   bash scripts/mimo_run_lock.sh renew-writer <名字>             # ★ 续期（写前刷 at）
#   bash scripts/mimo_run_lock.sh release                        # 只释放自己持有的
#   bash scripts/mimo_run_lock.sh status                         # 0=有活跃持有者，1=无锁/陈锁
#   bash scripts/mimo_run_lock.sh setmimo <pid>                  # 驱动起完 mimo 后回填其 PID
#   bash scripts/mimo_run_lock.sh selftest                       # ★ 自测，0=全过
#
# ★★★ 两种锁（2026-10-09 · COLLAB `N-008` 实施）：
#   本脚本原只有「**驱动锁**」，串行化的是「mimo 轮次」—— 但**心跳轮次**与**主会话**
#   同样会写同一工作区，**不经过该锁**（已两次实测并发写）。⇒ 补上「**写者锁**」：
#     · **驱动锁**（`owner_kind=driver`）：liveness ＝ **PID 存活**（驱动/或 mimo 子进程）；
#     · **写者锁**（`owner_kind=writer`）：liveness ＝ **now − at < ttl**（★ TTL 型）。
#   ★ 为什么写者锁必须是 TTL 型：**心跳没有长命进程** —— 它是一次 Agent turn，
#     构成它的 bash 调用**每次都是短命进程**；若照搬 PID 语义，取锁时写下的 owner PID
#     **一返回就死** ⇒ 锁被判陈锁并自愈清除 ⇒ **等于没锁**
#     （这与 `cmd_acquire` 里已记录的「owner 必须是长命进程」是**同一个坑的第三次现身**）。
#   ★ **只读者不取锁**（`git status` / 门禁 / 只读探针）；只有**会写仓库**的动作才取。
#   ★ 心跳的取锁**必须在 `pulse.sh` 判定状态之后**（否则心跳自己的锁会被 `pulse.sh`
#     读成"有活跃轮次"而**自我锁死**），且**派工前释放**（否则驱动 `acquire` 被自己挡住）。
#
# 锁位置：`.run/mimo.lock/`（`mkdir` 原子性；已在 .gitignore 内）
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCKDIR="$ROOT/.run/mimo.lock"
META="$LOCKDIR/meta"
DEFAULT_TTL=1800   # 写者锁缺省 TTL（秒）—— 心跳 wheel 间隔 1h，取 30min 足够覆盖一次写

_alive() { [ -n "${1:-}" ] && [ "$1" != "0" ] && kill -0 "$1" 2>/dev/null; }
_read()  { [ -f "$META" ] && sed -n "s/^$1=//p" "$META" 2>/dev/null | head -1 || true; }

_pid_alive_any() {
  _alive "$(_read driver_pid)" || _alive "$(_read mimo_pid)"
}

# ★ 锁是否**活跃持有**（两种 owner_kind 语义不同）：
#   · driver ⇒ PID 存活（驱动 / 或 mimo 子进程）；
#   · writer ⇒ ★ **TTL 型**：now − at < ttl（心跳没有长命进程，只能靠租约）。
_lock_is_live() {
  [ -d "$LOCKDIR" ] || return 1
  local kind; kind="$(_read owner_kind)"
  if [ "$kind" = "writer" ]; then
    local at ttl now
    at="$(_read at)"; ttl="$(_read ttl)"; now="$(date +%s)"
    [ -n "$at" ] || return 1
    [ $(( now - at )) -lt "${ttl:-$DEFAULT_TTL}" ]
  else
    _pid_alive_any
  fi
}

_lock_kind() { local k; k="$(_read owner_kind)"; echo "${k:-driver}"; }

_write_meta() {
  {
    echo "owner_kind=$3"
    echo "owner=$1"        # ★ 驱动锁=议题ID；写者锁=owner 名字（renew/release 都按它匹配）
    echo "issue=$1"
    echo "driver_pid=$2"
    echo "mimo_pid=0"
    echo "started=$(date +%s)"
    echo "started_at=$(date '+%F %T')"
    echo "at=$(date +%s)"
    echo "ttl=$DEFAULT_TTL"
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
      _write_meta "$issue" "$owner" driver
      echo "✓ 已获得驱动锁（议题 $issue · owner PID $owner）"
      return 0
    fi
    # ★ mkdir 失败 ≠ 锁存在：必须复核目录是否真的在
    if [ ! -d "$LOCKDIR" ]; then
      echo "✗ 建锁失败，但锁目录并不存在 —— 环境异常（权限/路径），并非「被占用」"
      return 2
    fi
    if _lock_is_live; then
      echo "✗ 工作区已被占用：kind=$(_lock_kind) holder=$(_read issue)$(_read owner) driver=$(_read driver_pid) mimo=$(_read mimo_pid) since=$(_read started_at)"
      return 1
    fi
    if [ "$took_over" -ge 1 ]; then
      echo "✗ 清除陈锁后仍建锁失败（可能被并发抢走）—— 放弃，避免无界重试"
      return 1
    fi
    took_over=1
    echo "⚠ 检出**陈锁**（kind=$(_lock_kind) 已失效）—— 清除后重试一次：旧 issue=$(_read issue) since=$(_read started_at)"
    rm -rf "$LOCKDIR" 2>/dev/null
    if [ -d "$LOCKDIR" ]; then
      echo "✗ 陈锁目录无法清除（rm 未生效）—— 拒绝继续（不做无界重试）"
      return 2
    fi
  done
}

# ★★ 写者锁（COLLAB `N-008`）：给「心跳轮次 / 主会话」这类**没有长命进程**的写者用。
#   owner ＝ 一个名字（如 `heartbeat`），liveness ＝ TTL 租约，**不靠 PID**。
cmd_acquire_writer() {
  local name="${1:?用法: acquire-writer <名字> [ttl秒]}"
  local ttl="${2:-$DEFAULT_TTL}"
  mkdir -p "$(dirname "$LOCKDIR")" 2>/dev/null
  [ -d "$(dirname "$LOCKDIR")" ] || { echo "✗ 锁父目录不可创建：$(dirname "$LOCKDIR")"; return 2; }

  local took_over=0
  while :; do
    if mkdir "$LOCKDIR" 2>/dev/null; then
      _write_meta "$name" 0 writer
      sed -i "s/^ttl=.*/ttl=$ttl/" "$META"
      echo "✓ 已获得写者锁（owner=$name · TTL=${ttl}s）"
      return 0
    fi
    if [ ! -d "$LOCKDIR" ]; then
      echo "✗ 建锁失败，但锁目录并不存在 —— 环境异常"; return 2
    fi
    if _lock_is_live; then
      echo "✗ 工作区写锁已被占用：kind=$(_lock_kind) holder=$(_read issue)$(_read owner) at=$(_read at) ttl=$(_read ttl)"
      return 1
    fi
    if [ "$took_over" -ge 1 ]; then
      echo "✗ 清除失效锁后仍建锁失败 —— 放弃（不做无界重试）"; return 1
    fi
    took_over=1
    echo "⚠ 检出**失效写者锁**（TTL 过期）—— 清除后重试：旧 owner=$(_read owner) at=$(_read at)"
    rm -rf "$LOCKDIR" 2>/dev/null
    [ -d "$LOCKDIR" ] && { echo "✗ 无法清除（rm 未生效）"; return 2; }
  done
}

# ★ 续期：写者**每次写前**刷 `at`，把租约顶住；turn 结束**不强制释放**（靠 TTL 自然过期）。
cmd_renew_writer() {
  local name="${1:?用法: renew-writer <名字>}"
  [ -f "$META" ] || { echo "✗ 无锁可续期"; return 1; }
  [ "$(_lock_kind)" = "writer" ] || { echo "✗ 当前是驱动锁，不可用 renew-writer"; return 1; }
  [ "$(_read owner)" = "$name" ] || { echo "✗ 锁属于 $(_read owner)，非 $name —— 拒绝续期"; return 1; }
  sed -i "s/^at=.*/at=$(date +%s)/" "$META"
  echo "✓ 已续期（owner=$name · at=$(date '+%F %T')）"
}

cmd_setmimo() {
  local pid="${1:?用法: setmimo <pid>}"
  [ -f "$META" ] || { echo "无锁"; return 1; }
  sed -i "s/^mimo_pid=.*/mimo_pid=$pid/" "$META"
}

cmd_release() {
  [ -d "$LOCKDIR" ] || { echo "○ 无锁（无需释放）"; return 0; }
  local kind; kind="$(_lock_kind)"
  if [ "$kind" = "writer" ]; then
    # 写者锁：给了名字必须匹配；没给名字时**只有租约已失效**才允许释放（防误放他人租约）
    local who="${1:-}"
    if [ -n "$who" ] && [ "$(_read owner)" != "$who" ]; then
      echo "✗ 写者锁属于 $(_read owner)，非 $who —— 拒绝释放"; return 1
    fi
    if [ -z "$who" ] && _lock_is_live; then
      echo "✗ 写者锁由 $(_read owner) 活跃持有（租约未过期）—— 拒绝释放"; return 1
    fi
  else
    local d; d="$(_read driver_pid)"
    if [ -n "$d" ] && [ "$d" != "$$" ] && _alive "$d"; then
      echo "✗ 锁属于另一个存活驱动（PID $d）—— 拒绝释放"
      return 1
    fi
  fi
  rm -rf "$LOCKDIR" 2>/dev/null
  [ -d "$LOCKDIR" ] && { echo "✗ 释放失败（目录仍在）"; return 1; }
  echo "✓ 已释放（$kind 锁）"
}

cmd_status() {
  [ -d "$LOCKDIR" ] || { echo "○ 无锁"; return 1; }
  local kind now
  kind="$(_lock_kind)"; now="$(date +%s)"
  if [ "$kind" = "writer" ]; then
    local at ttl dur
    at="$(_read at)"; ttl="$(_read ttl)"; dur=$(( now - ${at:-$now} ))
    if _lock_is_live; then
      echo "● 有写者在写：owner=$(_read owner) 起于 $(_read started_at) 租约 ${dur}s/${ttl}s（未过期）"
      return 0
    fi
    echo "○ 写者锁已过期（owner=$(_read owner) 租约 ${dur}s/${ttl}s）—— 可抢占"
    return 1
  fi
  local started dur
  started="$(_read started)"; dur=$(( now - ${started:-$now} ))
  if _lock_is_live; then
    echo "● 有驱动轮次在跑：issue=$(_read issue) driver=$(_read driver_pid) mimo=$(_read mimo_pid) 起于 $(_read started_at) 已 ${dur}s"
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
  echo "[6] ★ 写者锁（TTL 型 · COLLAB N-008 三象限）"
  rm -rf "$LOCKDIR" 2>/dev/null
  _t 0 "写者取锁（TTL 60s）"            bash "$0" acquire-writer hb 60
  _t 1 "★ 象限一：第二写者被拒"          bash "$0" acquire-writer other 60
  _t 1 "★ 象限一：驱动 acquire 同样被挡"  bash "$0" acquire N-BLOCK
  _t 0 "续期成功（自己）"                bash "$0" renew-writer hb
  _t 1 "续期被拒（他人）"                bash "$0" renew-writer other
  _t 1 "release 无名字且租约活跃 ⇒ 拒"    bash "$0" release
  _t 0 "release 带对名字 ⇒ 成功"          bash "$0" release hb
  echo "[6b] ★ TTL 过期语义（心跳无长命进程，只能靠租约）"
  bash "$0" acquire-writer hb 1 >/dev/null 2>&1
  sleep 2
  _t 1 "status 报『已过期』"             bash "$0" status
  _t 0 "★ 过期后可被抢占"                bash "$0" acquire-writer other 60
  _t 0 "release(other)"                 bash "$0" release other
  echo "[6c] ★ 象限二：只读不取锁 —— status 不得改动锁"
  bash "$0" acquire-writer hb 300 >/dev/null 2>&1
  local before after
  before="$(grep -E '^(at|owner_kind|owner)=' "$META" 2>/dev/null)"
  bash "$0" status >/dev/null 2>&1
  after="$(grep -E '^(at|owner_kind|owner)=' "$META" 2>/dev/null)"
  if [ "$before" = "$after" ]; then echo "  ok   [status 未改动锁]"; pass=$((pass+1))
  else echo "  FAIL [status 改动了锁]"; fail=$((fail+1)); fi
  _t 0 "release(hb)"                    bash "$0" release hb

  echo
  # ★ 收尾清场：自测中途失败会留下锁，污染后续调用（实测踩过：残留锁让 pulse 误判 BUSY_WRITER）
  rm -rf "$LOCKDIR" 2>/dev/null
  echo "  自测结果：通过 $pass / 失败 $fail（已清场）"
  [ "$fail" -eq 0 ] || return 1
  return 0
}

case "${1:-}" in
  acquire)         shift; cmd_acquire "$@" ;;
  acquire-writer)  shift; cmd_acquire_writer "$@" ;;
  renew-writer)    shift; cmd_renew_writer "$@" ;;
  release)         shift; cmd_release "$@" ;;
  status)          cmd_status ;;
  setmimo)         shift; cmd_setmimo "$@" ;;
  selftest)        cmd_selftest ;;
  *) echo "用法: $0 {acquire <议题ID> [owner-pid]|acquire-writer <名字> [ttl秒]|renew-writer <名字>|release [名字]|status|setmimo <pid>|selftest}" >&2; exit 2 ;;
esac
