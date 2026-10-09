#!/usr/bin/env bash
# scripts/run_tc_server.sh —— 把全套 TC 交叉编译后**在测试服务器上**跑一遍。
#
# ★ 为什么必须在服务器上跑（docs/05 · 用户 2026-10-09 硬约束）：
#     这些是 **DB 集成测试**（JX_TEST_DB=1）—— 要连库、要写测试数据 ⇒ 属「改远端状态」，
#     按判据一律去服务器；本机只做编译 / 纯单测 / 静态检查。
# ★ 本机只做两件事：交叉编译（GOOS=linux）与 scp 上传；**不 listen、不连库跑测试**。
#
# 用法：
#   bash scripts/run_tc_server.sh                 # 全套（★ 包集自动发现，见下）
#   bash scripts/run_tc_server.sh store httpapi   # 只跑指定包（名字取自 ./internal/<名>）
#   bash scripts/run_tc_server.sh --list-pkgs     # 只打印解析出的包集并退出（供探针自证）
# 退出码：0 = 全绿；非 0 = 至少一个包失败；2 = 前置/编译失败或包集为空。
# ★ 注意：本脚本会把仓库内的 .env 一并同步到服务器的测试树（DSN 只在服务器侧使用）。
#
# ★★ 包集自动发现（2026-10-09 修 —— 原为硬编码 `(permission config audit store httpapi)`）：
#     硬编码列表在**新增测试包**时会**静默漏跑**，而末尾仍打印「总判定：全绿」——
#     即「**部分覆盖**冒充**全部覆盖**」（同族缺陷：check_md_tables.py 文件头已记）。
#     实测事故：批 3 新增 `internal/codec`（6 条 TC）后，跑「全套」只覆盖 5/6 个包。
#     现改为扫描 `internal/*/` 下**含 `*_test.go`** 的目录：
#       · `_` 前缀目录跳过（与 gofmt / go build 的排除口径一致）；
#       · **不含测试文件的目录不得入选** —— 否则 `go test -c` 产不出二进制，
#         后续 scp / 执行必失败（把「不适用」误当「失败」）；
#       · **空包集一律拒绝执行（exit 2）** —— 防「零覆盖」冒充「全绿」。

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

HOST="${JX_DEPLOY_HOST:-chadhao@192.168.10.50}"
REMOTE_DIR="${JX_TC_REMOTE:-jx-lab-trace-tc}"
SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15)

# ---- 包集解析：显式参数优先；无参数 ⇒ 自动发现 ----
resolve_pkgs() {
  if [ "$#" -gt 0 ]; then
    printf '%s\n' "$@"
    return 0
  fi
  local d base tg
  for d in internal/*/; do
    [ -d "$d" ] || continue
    base="${d#internal/}"; base="${base%/}"
    case "$base" in _*) continue ;; esac
    tg=("$d"*_test.go)
    [ -e "${tg[0]}" ] || continue
    printf '%s\n' "$base"
  done
}

if [ "${1:-}" = "--list-pkgs" ]; then
  shift
  resolve_pkgs "$@"
  exit 0
fi

ALL_PKGS=()
while IFS= read -r _pkg; do
  [ -n "$_pkg" ] || continue
  ALL_PKGS+=("$_pkg")
done < <(resolve_pkgs "$@")

if [ "${#ALL_PKGS[@]}" -eq 0 ]; then
  echo "★★ 包集为空 —— 拒绝执行（防「零覆盖」冒充「全绿」）：internal/ 下没有含 *_test.go 的包"
  exit 2
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "===== [1/3] 交叉编译测试二进制（GOOS=linux）====="
echo "  ★ 包集（${#ALL_PKGS[@]} 个）：${ALL_PKGS[*]}"
for p in "${ALL_PKGS[@]}"; do
  out="$WORK/${p}.test"
  if ! GOOS=linux GOARCH=amd64 go test -c -o "$out" "./internal/$p"; then
    echo "编译 internal/$p 失败"
    exit 2
  fi
  echo "  ok internal/$p -> ${p}.test"
done

echo "===== [2/3] 同步源码树到 ${HOST}:~/${REMOTE_DIR} ====="
# ★ 同步的是**源码与规格**（测试二进制里已把 Go 代码编译进去，
#   但仍需 spec/ scripts/ migrations/ .env 供运行期读取与门禁自测使用）。
TAR="$WORK/src.tgz"
tar -czf "$TAR" \
  --exclude='.git' --exclude='node_modules' --exclude='bin' \
  --exclude='_probe' --exclude='_scratch' --exclude='.run' \
  --exclude='*.exe' --exclude='*.test' --exclude='web/dist' \
  internal spec scripts migrations go.mod go.sum .env 2>/dev/null

scp "${SSH_OPTS[@]}" "$TAR" "$HOST:/tmp/jx_tc_src.tgz" >/dev/null || exit 2
scp "${SSH_OPTS[@]}" "$WORK"/*.test "$HOST:/tmp/" >/dev/null || exit 2

# 测试二进制放进测试树根（下面按「包目录为 cwd」执行时用 ../../<pkg>.test 调用）
# ★ scp 过来的文件没有执行位 ⇒ 必须 chmod +x，否则 rc=126 Permission denied
ssh "${SSH_OPTS[@]}" "$HOST" "rm -rf ~/${REMOTE_DIR} && mkdir -p ~/${REMOTE_DIR} && tar -xzf /tmp/jx_tc_src.tgz -C ~/${REMOTE_DIR} && mv /tmp/*.test ~/${REMOTE_DIR}/ && chmod +x ~/${REMOTE_DIR}/*.test && echo synced" || exit 2

echo "===== [3/3] 在服务器上执行 TC ====="
TOTAL_RC=0
SUMMARY=()
for p in "${ALL_PKGS[@]}"; do
  # ★ cwd 必须是**包目录**：多数用例用 ../../spec、../../.env 定位仓库根（批 1 验收踩过 cwd 陷阱）
  set +e
  out=$(ssh "${SSH_OPTS[@]}" "$HOST" \
    "cd ~/${REMOTE_DIR}/internal/$p && PATH=/usr/local/go/bin:\$PATH JX_TEST_DB=1 ../../$p.test -test.count=1 -test.v" 2>&1)
  rc=$?
  set +e
  npass=$(printf '%s\n' "$out" | grep -c '^--- PASS' || true)
  nfail=$(printf '%s\n' "$out" | grep -c '^--- FAIL' || true)
  nskip=$(printf '%s\n' "$out" | grep -c '^--- SKIP' || true)
  echo "----- internal/$p (rc=$rc  PASS=$npass FAIL=$nfail SKIP=$nskip) -----"
  printf '%s\n' "$out" | grep -E '^(--- FAIL|    [a-z_]+\.go:)' | head -n 40
  if [ "$rc" -ne 0 ]; then
    TOTAL_RC=1
    SUMMARY+=("FAIL internal/$p  (PASS=$npass FAIL=$nfail SKIP=$nskip)")
  else
    SUMMARY+=("ok   internal/$p  (PASS=$npass FAIL=$nfail SKIP=$nskip)")
  fi
done

echo
echo "===== TC 汇总 ====="
for l in "${SUMMARY[@]}"; do
  echo "  $l"
done
if [ "$TOTAL_RC" -ne 0 ]; then
  echo "===== 总判定：**失败**（exit 1）====="
  exit 1
fi
echo "===== 总判定：**全绿** ====="
exit 0
