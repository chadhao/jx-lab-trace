#!/usr/bin/env bash
# drive_mimo.sh —— 驱动 mimo code 完成一轮交办：**目标判据 ＋ 中断自动续跑**
#
# ★ 核心教训（沿用自上一个项目的实测）：mimo 执行到一半会 **interrupt** —— 命令行**正常返回、退出码 0**，
#   看起来"跑完了"，而工作其实只做了一半。⇒ **命令返回 ≠ 任务完成**，必须改判据：**看客观产物，不看进程退出**。
#
# 用法：
#   bash scripts/drive_mimo.sh <议题ID> <指令文件> [最大尝试次数]
# 例：
#   bash scripts/drive_mimo.sh N-001 MIMO-NEXT-BATCH-01.md 5
#
# 完成判据（三条**全满足**才算完成，缺一即续跑）：
#   ① 台账：COLLAB.md 的该议题段内出现 `MIMO-DONE`
#   ② 提交：HEAD 已越过本次基线，**且最新提交的 subject 不以 `[WorkBuddy]` 开头**（排除我方提交）
#   ③ 门禁：bash scripts/check_all.sh 退出码 0
#
# 环境变量：
#   MIMO_SESSION  会话 id（留空 = 新建会话；★ 首次跑完把新 id 记进 COLLAB.md §1 供后续复用）
#   MIMO_BIN      mimo 可执行文件路径
#   MIMO_MODEL    模型 id（默认套餐 provider）
#   MIMO_VARIANT  推理档（默认 high）
set -o pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
MIMO_BIN="${MIMO_BIN:-$HOME/.mimocode/bin/mimo}"
MIMO_SESSION="${MIMO_SESSION:-}"
# ★ provider 实测：`xiaomi/`（直连 API）会报 Insufficient account balance ⇒ 用套餐 provider。
MIMO_MODEL="${MIMO_MODEL:-xiaomi-token-plan-cn/mimo-v2.6-flash}"
MIMO_VARIANT="${MIMO_VARIANT:-high}"

ISSUE="${1:?用法: drive_mimo.sh <议题ID> <指令文件> [最大尝试次数]}"
PROMPT_FILE="${2:?缺少指令文件}"
MAX="${3:-5}"
LOG="${LOG:-/tmp/drive_mimo_${ISSUE//[^A-Za-z0-9]/_}.log}"

# 门禁所需环境（python 与 go 的路径在 Git Bash 下未必在 PATH 里）
export PY="${PY:-C:/Users/haoduan/.workbuddy/binaries/python/versions/3.13.12/python.exe}"
export PATH="/c/go/bin:$PATH"

die() { echo "✗ $*" >&2; exit 1; }
[ -f "$PROMPT_FILE" ] || die "指令文件不存在：$PROMPT_FILE"
[ -x "$MIMO_BIN" ] || die "找不到 mimo：$MIMO_BIN"

cd "$REPO" || die "无法进入仓库：$REPO"

# ── 完成判据 ①：议题段内出现 MIMO-DONE ─────────────────────────────
section_done() {
  # ★ 修正（2026-10-09）：原实现「段首规则带 next」⇒ 附录 A 模板标题行
  #   `### N-001 · <一句话标题>` 会再次命中段首并 next，段尾规则永不执行，
  #   段落一直延伸到文件末尾，附录状态枚举里的完成标记词随即被判「段内出现」
  #   ⇒ 判据① 恒真（假绿）。修法：段尾同时认 `## / ###`（任何 `^#` 标题）
  #   且**先于**段首求值；段首加 `seen` 守卫，**只认首次**出现。
  awk -v id="### $ISSUE" '
    inside && /^#/ { inside = 0 }
    index($0, id) == 1 && !seen { seen = 1; inside = 1; next }
    inside && /MIMO-DONE/ { found = 1 }
    END { exit !found }
  ' COLLAB.md
}

# ── 完成判据 ②：**对方**真的提交了（追加作者校验，防把我方提交算成对方完成）──
mimo_committed() {
  [ "$(git rev-parse HEAD)" != "$BASE" ] || return 1
  case "$(git log -1 --pretty=%s)" in
    "[WorkBuddy]"*) return 1 ;;
    *) return 0 ;;
  esac
}

# ── 完成判据 ③：门禁必绿 ──────────────────────────────────────────
gate_ok() { bash scripts/check_all.sh >/dev/null 2>&1; }

# ── 开工前自检：基线必须绿（红着开工 = 在错误前提上盖楼）──
if ! gate_ok; then
  die "基线门禁非绿 —— 拒绝交办。请先跑 'bash scripts/check_all.sh' 看清红在哪。"
fi

BASE="$(git rev-parse HEAD)"
echo "═══ 驱动 mimo · 议题 $ISSUE ═══"
echo "仓库     : $REPO"
echo "会话     : ${MIMO_SESSION:-<新建>}"
echo "基线 HEAD: $BASE"
echo "日志     : $LOG"
echo

attempt=1
while [ "$attempt" -le "$MAX" ]; do
  if [ "$attempt" -eq 1 ]; then
    PROMPT="$(cat "$PROMPT_FILE")"
  else
    PROMPT="继续 $ISSUE 未完成的项。★★ 已完成的项**不要重做**。完成判据（三条全满足）：① COLLAB.md 的 $ISSUE 段内写回执且状态改 MIMO-DONE；② 提交代码（**显式路径，禁止 git add -A**）；③ 提交前跑 bash scripts/check_all.sh 必绿全绿。★ 若中途被打断，直接从断点继续。"
  fi

  echo "── 第 $attempt/$MAX 次 ──"
  {
    echo; echo "════════ attempt $attempt · $(date '+%F %T') ════════"
  } >> "$LOG"

  # ★ 有意**不看退出码** —— 返回 0 不代表完成（见文件头）
  if [ -n "$MIMO_SESSION" ]; then
    "$MIMO_BIN" run -s "$MIMO_SESSION" --dir "$REPO" \
      -m "$MIMO_MODEL" --variant "$MIMO_VARIANT" --yolo "$PROMPT" >>"$LOG" 2>&1 </dev/null
  else
    "$MIMO_BIN" run --dir "$REPO" \
      -m "$MIMO_MODEL" --variant "$MIMO_VARIANT" --yolo "$PROMPT" >>"$LOG" 2>&1 </dev/null
  fi

  # ── 逐条判据，**缺哪条就报哪条**（可见失败，不许静默续跑）──
  ok_sec=0; ok_git=0; ok_gate=0
  section_done && ok_sec=1
  mimo_committed && ok_git=1
  gate_ok && ok_gate=1
  echo "   判据：台账回执=$ok_sec  新提交=$ok_git  门禁绿=$ok_gate"

  if [ "$ok_sec" = 1 ] && [ "$ok_git" = 1 ] && [ "$ok_gate" = 1 ]; then
    echo "✓ 完成（第 $attempt 次）· HEAD=$(git rev-parse --short HEAD)"
    echo "★ 会话 id（下次复用）：$(mimo session list 2>/dev/null | tail -n +2 | head -1 | awk '{print $1}')"
    exit 0
  fi
  attempt=$((attempt + 1))
done

echo "✗ 达最大尝试 $MAX 次仍未满足判据 —— **停手报人**（不无限重试）。" >&2
echo "  最后一次状态：台账回执=$ok_sec 新提交=$ok_git 门禁绿=$ok_gate" >&2
echo "  ★ 注意：门禁绿可能只是「还没改」，请勿据此认为无需处理。" >&2
exit 1
