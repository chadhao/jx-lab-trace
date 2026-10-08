#!/usr/bin/env bash
# scripts/run_tc_server.sh —— 把全套 TC 交叉编译后**在测试服务器上**跑一遍。
#
# ★ 为什么必须在服务器上跑（docs/05 · 用户 2026-10-09 硬约束）：
#     这些是 **DB 集成测试**（JX_TEST_DB=1）—— 要连库、要写测试数据 ⇒ 属「改远端状态」，
#     按判据一律去服务器；本机只做编译 / 纯单测 / 静态检查。
# ★ 本机只做两件事：交叉编译（GOOS=linux）与 scp 上传；**不 listen、不连库跑测试**。
#
# 用法：
#   bash scripts/run_tc_server.sh                 # 全套
#   bash scripts/run_tc_server.sh store httpapi   # 只跑指定包（名字取自 ./internal/<名>）
# 退出码：0 = 全绿；非 0 = 至少一个包失败。
# ★ 注意：本脚本会把仓库内的 .env 一并同步到服务器的测试树（DSN 只在服务器侧使用）。

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

HOST="${JX_DEPLOY_HOST:-chadhao@192.168.10.50}"
REMOTE_DIR="${JX_TC_REMOTE:-jx-lab-trace-tc}"
SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15)

ALL_PKGS=(permission config audit store httpapi)
if [ "$#" -gt 0 ]; then
  ALL_PKGS=("$@")
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "===== [1/3] 交叉编译测试二进制（GOOS=linux）====="
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
