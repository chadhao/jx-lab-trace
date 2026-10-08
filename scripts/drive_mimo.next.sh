#!/usr/bin/env bash
# drive_mimo.next.sh —— drive_mimo.sh 的**下一版**：在原有三条完成判据之上，
#                       增加【内置停滞看门狗】。
#
# ★ 为什么要它（2026-10-08 实测教训）：
#   原版把 `mimo run` **前台阻塞**地跑 ⇒ 一旦 mimo 卡住（进程活着、但日志停止增长、
#   也没有任何外连），驱动会**一直等下去**。实测那次**空转 6 小时**，
#   直到人工发现才终止 —— 而两条完成判据（新提交 / 门禁绿）在"还没改"时**永远是同一个值**，
#   它们**表达不了"卡住"**。
#   ⇒ ★ 修法：把 `mimo run` 放后台，驱动**每分钟轮询日志是否还在增长**；
#     超过 `STALL_SECS` 仍无增长 ⇒ 判定停滞 ⇒ **杀掉并进入下一次尝试**（自动重试）。
#
# ★ 判据设计（避免误杀）：
#   只有 **① 日志静止 > STALL_SECS** 且 **② 进程仍存活** 才判停滞。
#   进程已退出 ⇒ 那是「本轮结束」，正常进入三条判据的评估。
#
# 用法（与原版一致）：
#   bash scripts/drive_mimo.sh <议题ID> <指令文件> [最大尝试次数]
#
# 环境变量：
#   MIMO_SESSION  会话 id（留空 = 新建）
#   MIMO_BIN      mimo 可执行文件路径
#   MIMO_MODEL    模型 id（默认套餐 provider）
#   MIMO_VARIANT  推理档（默认 high）
#   STALL_SECS    日志静止多少秒判「停滞」（默认 900 = 15 分钟）
#   POLL_SECS     轮询间隔（默认 60）
set -o pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
MIMO_BIN="${MIMO_BIN:-$HOME/.mimocode/bin/mimo}"
MIMO_SESSION="${MIMO_SESSION:-}"
MIMO_MODEL="${MIMO_MODEL:-xiaomi-token-plan-cn/mimo-v2.6-flash}"
MIMO_VARIANT="${MIMO_VARIANT:-high}"
STALL_SECS="${STALL_SECS:-900}"
POLL_SECS="${POLL_SECS:-60}"

ISSUE="${1:?用法: drive_mimo.sh <议题ID> <指令文件> [最大尝试次数]}"
PROMPT_FILE="${2:?缺少指令文件}"
MAX="${3:-5}"
LOG="${LOG:-/tmp/drive_mimo_${ISSUE//[^A-Za-z0-9]/_}.log}"

export PY="${PY:-C:/Users/haoduan/.workbuddy/binaries/python/versions/3.13.12/python.exe}"
export PATH="/c/go/bin:$PATH"

die() { echo "✗ $*" >&2; exit 1; }
[ -f "$PROMPT_FILE" ] || die "指令文件不存在：$PROMPT_FILE"
[ -x "$MIMO_BIN" ] || die "找不到 mimo：$MIMO_BIN"
cd "$REPO" || die "无法进入仓库：$REPO"

# ── 完成判据 ①：议题段内出现 MIMO-DONE ─────────────────────────────
# ★ 2026-10-09 修正：原实现「段首规则带 next」⇒ 附录 A 模板标题行会再次命中段首，
#   段尾规则永不执行，段落一直延到文件末尾，附录状态枚举里的标记词被判「段内出现」
#   ⇒ 判据① 恒真（假绿）。修法：段尾认任何 `^#` 标题，且**先于**段首求值；段首只认首次。
section_done() {
  awk -v id="### $ISSUE" '
    inside && /^#/ { inside = 0 }
    index($0, id) == 1 && !seen { seen = 1; inside = 1; next }
    inside && /MIMO-DONE/ { found = 1 }
    END { exit !found }
  ' COLLAB.md
}

# ── 完成判据 ②：**对方**真的提交了（追加作者校验）──
mimo_committed() {
  [ "$(git rev-parse HEAD)" != "$BASE" ] || return 1
  case "$(git log -1 --pretty=%s)" in
    "[WorkBuddy]"*) return 1 ;;
    *) return 0 ;;
  esac
}

# ── 完成判据 ③：门禁必绿 ──────────────────────────────────────────
gate_ok() { bash scripts/check_all.sh >/dev/null 2>&1; }

if ! gate_ok; then
  die "基线门禁非绿 —— 拒绝交办。请先跑 'bash scripts/check_all.sh' 看清红在哪。"
fi

BASE="$(git rev-parse HEAD)"
echo "═══ 驱动 mimo · 议题 $ISSUE（含停滞看门狗）═══"
echo "仓库     : $REPO"
echo "会话     : ${MIMO_SESSION:-<新建>}"
echo "基线 HEAD: $BASE"
echo "日志     : $LOG"
echo "停滞阈值 : ${STALL_SECS}s（轮询 ${POLL_SECS}s）"
echo

log_mtime() { stat -c %Y "$LOG" 2>/dev/null || echo 0; }
now_ts()    { date +%s; }

attempt=1
while [ "$attempt" -le "$MAX" ]; do
  if [ "$attempt" -eq 1 ]; then
    PROMPT="$(cat "$PROMPT_FILE")"
  else
    PROMPT="继续 $ISSUE 未完成的项。★★ 已完成的项**不要重做**。完成判据（三条全满足）：① COLLAB.md 的 $ISSUE 段内写回执且状态改 MIMO-DONE；② 提交代码（**显式路径，禁止 git add -A**）；③ 提交前跑 bash scripts/check_all.sh 必绿全绿。★ 若上一轮被打断，直接从断点继续。"
  fi

  echo "── 第 $attempt/$MAX 次 ──"
  { echo; echo "════════ attempt $attempt · $(date '+%F %T') ════════"; } >> "$LOG"

  # ★ 后台跑 mimo（不再前台阻塞），以便并行做停滞监控
  if [ -n "$MIMO_SESSION" ]; then
    "$MIMO_BIN" run -s "$MIMO_SESSION" --dir "$REPO" \
      -m "$MIMO_MODEL" --variant "$MIMO_VARIANT" --yolo "$PROMPT" >>"$LOG" 2>&1 </dev/null &
  else
    "$MIMO_BIN" run --dir "$REPO" \
      -m "$MIMO_MODEL" --variant "$MIMO_VARIANT" --yolo "$PROMPT" >>"$LOG" 2>&1 </dev/null &
  fi
  MPID=$!

  STALLED=0
  while kill -0 "$MPID" 2>/dev/null; do
    sleep "$POLL_SECS"
    kill -0 "$MPID" 2>/dev/null || break
    idle=$(( $(now_ts) - $(log_mtime) ))
    if [ "$idle" -gt "$STALL_SECS" ]; then
      echo "   ⚠ 停滞：日志已静止 ${idle}s（> ${STALL_SECS}s）而进程仍存活 ⇒ 终止本轮并重试"
      { echo; echo "[$(date '+%F %T')] ★ 看门狗判定停滞（静止 ${idle}s）—— 终止 PID $MPID"; } >> "$LOG"
      kill "$MPID" 2>/dev/null
      sleep 3
      kill -0 "$MPID" 2>/dev/null && kill -9 "$MPID" 2>/dev/null
      STALLED=1
      break
    fi
  done
  wait "$MPID" 2>/dev/null

  # ── 逐条判据，缺哪条报哪条（可见失败，不许静默续跑）──
  ok_sec=0; ok_git=0; ok_gate=0
  section_done && ok_sec=1
  mimo_committed && ok_git=1
  gate_ok && ok_gate=1
  echo "   判据：台账回执=$ok_sec  新提交=$ok_git  门禁绿=$ok_gate$([ "$STALLED" = 1 ] && echo '  （本轮因停滞被看门狗终止）')"

  if [ "$ok_sec" = 1 ] && [ "$ok_git" = 1 ] && [ "$ok_gate" = 1 ]; then
    echo "✓ 完成（第 $attempt 次）· HEAD=$(git rev-parse --short HEAD)"
    exit 0
  fi
  attempt=$((attempt + 1))
done

echo "✗ 达最大尝试 $MAX 次仍未满足判据 —— **停手报人**（不无限重试）。" >&2
echo "  最后一次状态：台账回执=$ok_sec 新提交=$ok_git 门禁绿=$ok_gate" >&2
echo "  ★ 注意：门禁绿可能只是「还没改」，请勿据此认为无需处理。" >&2
exit 1
