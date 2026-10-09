#!/usr/bin/env bash
# pulse.sh —— **心跳状态机**（用户 2026-10-09 定的三步处置）。
#
# ★ 用户定的流程：
#   ① 每轮先检查 mimo 状态（在不在跑 / 是否空转）
#   ② 分三种处置：
#      A. **没在跑**     ⇒ 检查产出 → 测试 → 推送 → 继续分配任务
#      B. **在跑、不空转** ⇒ **结束本轮**，等下一轮（15 分钟后）
#      C. **在跑但空转**  ⇒ **结束 mimo 进程** → **检查中间产物** → 再做后续处理
#   ③ 轮次间隔 15 分钟
#
# ★ 为什么做成脚本而不是"写进 prompt 的自然语言"：
#   15 分钟一轮 ⇒ 这套判断要**高频重复执行**。自然语言在重复执行下会漂移、且**无法测试**。
#   本脚本把**确定性的部分**（探活 / 分类 / 杀空转 / 盘点中间产物）固化；
#   **需要判断的部分**（验收实现、决定派哪一批）留给 Agent。
#
# 用法：bash scripts/pulse.sh
# 退出码（= 状态，供上游脚本消费）：
#   0  = IDLE           无轮次在跑 ⇒ **交给 Agent：验收 → 推送 → 派下一批**
#   10 = RUNNING_HEALTHY 有轮次在跑且活跃 ⇒ **Agent 应结束本轮**
#   20 = STALLED_KILLED  有轮次但空转，**已杀进程并盘点中间产物** ⇒ Agent 决定续派/归档
#   30 = NO_REPO/异常
#   40 = ★ BUSY_WRITER   无 mimo，但**写者锁活跃**（另一个写者在写）⇒ Agent 结束本轮、只读
#
# ★★ 关于频率（2026-10-09 结论，**已定案**）：
#   ① 平台自动化的**最小调度粒度是 HOURLY** —— 试过 `FREQ=HOURLY;INTERVAL=1;BYMINUTE=0,15,30,45`，
#      **BYMINUTE 被忽略**（实测 `nextRunAt` 是整整 1 小时后，分钟位 ≠ 0/15/30/45）。
#   ② **用户定案**：「如果平台不支持 15 分钟任务，就按最小任务周期颗粒度来设置就好，其他逻辑不变。」
#      ⇒ 调度 ＝ **1 小时**；**三步状态机逻辑不变**。
#   ③ ★ 「卡住能被及时发现」这件事**不由调度器交付** —— 由**驱动内置看门狗**负责
#      （`STALL_SECS=900`，即 **15 分钟**自愈），**不依赖调度器、比心跳更可靠**。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 30

PY="${PY:-C:/Users/haoduan/.workbuddy/binaries/python/versions/3.13.12/python.exe}"
LOG="${LOG:-/tmp/drive_mimo_N_001.log}"
MAX_IDLE="${MAX_IDLE:-900}"     # 日志静止多少秒判「空转」
export PY

say() { echo "$*"; }
rule() { echo "──────────────────────────────────────────────"; }

rule
say "═══ jx-lab-trace 心跳状态机 · $(date '+%F %T') ═══"
rule

# ── ① 探活：锁 + 进程 ────────────────────────────────────────────
LOCK_OUT="$(bash scripts/mimo_run_lock.sh status 2>&1)"; LOCK_RC=$?
say "[锁] $LOCK_OUT"

LOCK_MIMO="$(sed -n 's/^mimo_pid=//p' .run/mimo.lock/meta 2>/dev/null | head -1)"
LOCK_DRIVER="$(sed -n 's/^driver_pid=//p' .run/mimo.lock/meta 2>/dev/null | head -1)"
# ★ 锁的种类（COLLAB `N-008`）：driver=驱动轮次；writer=心跳/主会话的写者锁（TTL 型）
LOCK_KIND="$(sed -n 's/^owner_kind=//p' .run/mimo.lock/meta 2>/dev/null | head -1)"

# 进程探活：**不依赖锁**（在跑的可能是没加锁的旧驱动）
MIMO_PIDS="$(tasklist 2>/dev/null | grep -i '^mimo\.exe' | awk '{print $2}' | tr '\n' ' ')"
MIMO_PIDS="$(echo "$MIMO_PIDS" | sed 's/ *$//')"
# ★ 驱动 PID 解析必须**精确**（曾误抓到 `scripts/check_mimo_stall.py` 等无关进程 —— 一旦走到
#   杀进程分支就会**杀错人**）。判据：命令行里 $6 是 bash、$7 就是 `scripts/drive_mimo*.sh`。
#   （父级 wrapper 的命令行里虽然也含 "drive_mimo" 字样，但它的 $7 是 `-c`，会被排除。）
DRIVE_PIDS="$(ps -ef 2>/dev/null | awk '$6 ~ /bash$/ && $7 ~ /^scripts\/drive_mimo/ {print $2}' | tr '\n' ' ')"
DRIVE_PIDS="$(echo "$DRIVE_PIDS" | sed 's/ *$//')"

say "[进程] mimo.exe=[${MIMO_PIDS:-无}]  drive_mimo=[${DRIVE_PIDS:-无}]"

# ── ② 分类与处置 ────────────────────────────────────────────────
if [ -z "$MIMO_PIDS" ]; then
  # ★★ 先判「是否已有**活跃写者**」（COLLAB `N-008`）：写者锁活跃 ⇒ **本轮不写**，
  #    否则「心跳轮次」与「主会话 / 另一个心跳」会并发写同一工作区（已两次实测）。
  if [ "$LOCK_RC" = 0 ] && [ "$LOCK_KIND" = "writer" ]; then
    say "[判定] **D · 无 mimo，但写者锁活跃**（$LOCK_OUT）"
    rule
    say "STATE=BUSY_WRITER —— **Agent 应结束本轮**：另一个写者正在写工作区，本轮只读不写"
    exit 40
  fi
  # ── A. 没在跑 ──
  say "[判定] **A · 无轮次在跑**"
  if [ -d .run/mimo.lock ]; then
    say "[处置] 清理陈锁（无 mimo 进程却仍有锁）"
    bash scripts/mimo_run_lock.sh release 2>&1 | sed 's/^/        /'
  fi
  say ""
  say "[产出盘点]"
  say "    HEAD        = $(git log --oneline -1 2>/dev/null)"
  say "    未提交变更   = $(git status --porcelain 2>/dev/null | wc -l) 个文件"
  say "    最近 3 次提交:"
  git log --oneline -3 2>/dev/null | sed 's/^/        /'
  rule
  # ★★ 注意：本行**不得使用反引号**包命令 —— bash 会把它当**命令替换**真的执行
  #   （实测踩过：写示例命令时用了反引号 ⇒ `pulse.sh` 在报 IDLE 的同时**自己抢了一把写者锁**
  #    ⇒ 下一次跑判成 BUSY_WRITER ⇒ **自我锁死**。这正是 COLLAB N-008 预警的形态之一。）
  #   同族第三次 ⇒ 见「附加铁律 13（引号纪律）」。用单引号或「」写字面文本。
  say 'STATE=IDLE —— 交给 Agent：① 先取写锁（bash scripts/mimo_run_lock.sh acquire-writer heartbeat 1800；★ 取不到即说明有别的写者，本轮改只读）② 复跑门禁 ③ 读实现验收 ④ 通过则提交并推送 ⑤ 派下一批；⑥ 收尾释放写锁（release heartbeat）'
  exit 0
fi

# 有 mimo 在跑 —— 判是否空转（取第一个 PID）
MPID="$(echo "$MIMO_PIDS" | awk '{print $1}')"
say ""
say "[停滞判据] 对 PID $MPID 运行（阈值 ${MAX_IDLE}s）"
STALL_OUT="$("$PY" scripts/check_mimo_stall.py --log "$LOG" --max-idle "$MAX_IDLE" --pid "$MPID" 2>&1)"
STALL_RC=$?
echo "$STALL_OUT" | sed 's/^/    /'

if [ "$STALL_RC" -ne 0 ]; then
  # ── C. 在跑但空转 ──
  say ""
  say "[判定] **C · 在跑但空转** ⇒ 结束 mimo 进程"
  for p in $MIMO_PIDS; do kill -9 "$p" 2>/dev/null && say "        已终止 mimo.exe PID $p"; done
  for p in $DRIVE_PIDS; do kill -9 "$p" 2>/dev/null && say "        已终止驱动 PID $p"; done
  sleep 2
  bash scripts/mimo_run_lock.sh release >/dev/null 2>&1 || true

  say ""
  say "[清理残留子进程]（隧道等）"
  netstat -ano 2>/dev/null | grep LISTENING | grep ':13306' | awk '{print $NF}' | sort -u | while read -r p; do
    [ -n "$p" ] && kill -9 "$p" 2>/dev/null && say "        已终止 13306 监听 PID $p"
  done
  say "        （如无输出即无残留）"

  say ""
  say "★★ [中间产物盘点]（**不许丢**：先看清、再决定归档或续用）"
  say "    HEAD        = $(git log --oneline -1 2>/dev/null)"
  N_CHANGED="$(git status --porcelain 2>/dev/null | wc -l)"
  say "    未提交变更   = $N_CHANGED 个文件"
  if [ "$N_CHANGED" -gt 0 ]; then
    say "    变更清单（前 20）："
    git status --porcelain 2>/dev/null | head -20 | sed 's/^/        /'
    say "    规模：$(git diff --shortstat 2>/dev/null)"
    # ★★ 同样禁止反引号（见上）：这行若用反引号包 `git stash push ...`，走进 STALLED 分支时
    #   脚本会**真的执行 stash**、未经确认地动工作区。用单引号写字面文本。
    say '    ★ 处置建议：**先归档再续派** —— 用 git stash push -u -m wip-<时间>，或打一个 wip 分支；'
    say '      **不要直接丢弃**（半成品里可能已有真东西），**也不要直接留着**（会污染下一轮）。'
    say '      ★ 注意：本仓库 core.autocrlf=true，**禁用 git stash 归档 .go 文件**（会落成 CRLF 致 gofmt 判红）——用「复制归档 + 移除」替代（见附加铁律 12）。'
  else
    say "    ★ 工作区干净 ⇒ 无中间产物需归档。"
  fi
  rule
  say "STATE=STALLED_KILLED —— 交给 Agent：决定「归档中间产物 → 续派」还是「直接续派」"
  exit 20
fi

# ── B. 在跑且不空转 ──
say ""
say "[判定] **B · 在跑且活跃（非空转）**"
say "    日志：$(stat -c '%y' "$LOG" 2>/dev/null | cut -d. -f1)  大小 $(stat -c%s "$LOG" 2>/dev/null) 字节"
say "    HEAD：$(git log --oneline -1 2>/dev/null)"
say "    未提交变更：$(git status --porcelain 2>/dev/null | wc -l) 个文件"
rule
say "STATE=RUNNING_HEALTHY —— **Agent 应结束本轮**（不派工、不改动），等下一轮复查（调度 ＝ 平台最小粒度 **1 小时**）"
exit 10
