#!/usr/bin/env bash
# scripts/check_all.sh —— **一条命令跑完全部门禁**。
#
# ★ 动因：门禁散落多处 ⇒ 没人会全跑 ⇒ 等于不存在。本脚本把「跑什么、每项耗时、总判定」收拢到一处。
# ★ 分两档（**不许把「会报」并进「必绿」**）：并进去 ⇒ 基线常年见红 ⇒ 红成为常态 ⇒ 被忽略。
#   · 【必绿】当前语料**必定通过**的检查 —— 任一失败 ⇒ 总判定失败（exit 1）。
#   · 【会报】**会报出既存问题**的检查 —— 不阻塞总判定，只列命中清单。
#
# ★ 用 `set -o pipefail`（否则「管道吃掉退出码 → 静默假绿」）；
#   ★ 刻意**不用 `set -e`**：要**跑完所有项**再给总判定。
#
# 用法：bash scripts/check_all.sh
# 退出码：0 = 全部「必绿」通过；1 = 至少一项「必绿」失败。
set -o pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

PY="${PY:-python}"
GO="${GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1 && [ -x /c/go/bin/go ]; then
  GO=/c/go/bin/go
fi
# ★ 用 `gofmt` 而非 `go fmt` —— 后者不接受 `-l`（实测报 `flag provided but not defined: -l`）。
GOFMT="${GOFMT:-gofmt}"
if ! command -v "$GOFMT" >/dev/null 2>&1 && [ -x /c/go/bin/gofmt ]; then
  GOFMT=/c/go/bin/gofmt
fi

GREEN_FAIL=0
GREEN_TOTAL=0
declare -a SUMMARY

_ms() { date +%s%N; }
_elapsed() { echo $(( ($(_ms) - $1) / 1000000 )); }

green() {
  local label="$1"; shift
  local t0 out rc ms
  t0=$(_ms); out=$("$@" 2>&1); rc=$?; ms=$(_elapsed "$t0")
  GREEN_TOTAL=$((GREEN_TOTAL + 1))
  if [ "$rc" -eq 0 ]; then
    echo "  ✓ [必绿] ${label}  (${ms} ms)"
    SUMMARY+=("✓ 必绿  ${label}  ${ms}ms")
  else
    echo "  ✗ [必绿] ${label}  (${ms} ms) —— **失败**"
    printf '%s\n' "$out" | tail -n 25 | sed 's/^/        | /'
    SUMMARY+=("✗ 必绿  ${label}  ${ms}ms  ← 失败")
    GREEN_FAIL=$((GREEN_FAIL + 1))
  fi
}

# 空输出判据：如 `gofmt -l .` 退出码恒为 0，须「输出为空」才算过。
green_empty() {
  local label="$1"; shift
  local t0 out rc ms
  t0=$(_ms); out=$("$@" 2>&1); rc=$?; ms=$(_elapsed "$t0")
  GREEN_TOTAL=$((GREEN_TOTAL + 1))
  if [ "$rc" -eq 0 ] && [ -z "$out" ]; then
    echo "  ✓ [必绿] ${label}  (${ms} ms)"
    SUMMARY+=("✓ 必绿  ${label}  ${ms}ms")
  else
    echo "  ✗ [必绿] ${label}  (${ms} ms) —— **失败**（要求输出为空）"
    printf '%s\n' "$out" | head -n 25 | sed 's/^/        | /'
    SUMMARY+=("✗ 必绿  ${label}  ${ms}ms  ← 失败")
    GREEN_FAIL=$((GREEN_FAIL + 1))
  fi
}

report() {
  local label="$1"; shift
  local t0 out rc ms
  t0=$(_ms); out=$("$@" 2>&1); rc=$?; ms=$(_elapsed "$t0")
  if [ "$rc" -eq 0 ]; then
    echo "  ○ [会报] ${label}  (${ms} ms) —— 无命中"
    SUMMARY+=("○ 会报  ${label}  ${ms}ms  无命中")
  else
    echo "  ● [会报] ${label}  (${ms} ms) —— **有命中，需人工处置（不阻塞总判定）**"
    printf '%s\n' "$out" | sed 's/^/        | /'
    SUMMARY+=("● 会报  ${label}  ${ms}ms  ← 需人处置")
  fi
}

T_ALL=$(_ms)
echo "===== 全门禁 run-all  (repo: ${ROOT}) ====="
echo
echo "[必绿基线]"
# gofmt：排除 `_` 前缀目录（Go 工具链约定 `_` 前缀不参与构建；`gofmt -l .` 是纯文件遍历会走进去
#   ⇒ 会去检查工具链根本不构建的文件，那是门禁自身的不一致）。
#   `xargs -r`：无文件时不调用 gofmt（否则读空 stdin 会挂起）。
green_empty "gofmt -l .（排除 _ 前缀，对齐 go build/vet）" \
  bash -c "find . -type f -name '*.go' -not -path '*/_*' -not -path '*/.git/*' -not -path '*/node_modules/*' -print0 | xargs -0 -r '${GOFMT}' -l"
green       "go build ./..."          "$GO" build ./...
green       "go vet ./..."            "$GO" vet ./...
green       "go test ./... -count=1"  "$GO" test ./... -count=1
green       "md 表格列数门禁"          "$PY" scripts/check_md_tables.py
green       "COLLAB 协商台账门禁"      "$PY" scripts/check_collab.py
green       "用例↔测试用例覆盖门禁"    "$PY" scripts/check_uc_tc.py
green       "追踪码规格自校验"         "$PY" spec/verify_code_rules.py
green       "权限点规格自校验"         "$PY" spec/verify_permission_points.py
echo
echo "[会报既存问题]（不阻塞）"
report      "md 结构与一致性门禁"      "$PY" scripts/check_md_structure.py

echo
echo "===== 汇总（本次跑了 $((${#SUMMARY[@]})) 项，总耗时 $(_elapsed "$T_ALL") ms）====="
for l in "${SUMMARY[@]}"; do
  echo "  ${l}"
done
echo
if [ "$GREEN_FAIL" -ne 0 ]; then
  echo "===== 总判定：**失败**（必绿基线 ${GREEN_FAIL}/${GREEN_TOTAL} 项失败，exit 1）====="
  exit 1
fi
echo "===== 总判定：**通过**（必绿基线 ${GREEN_TOTAL}/${GREEN_TOTAL} 全绿；会报项如需处置见上）====="
exit 0
