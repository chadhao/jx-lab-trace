#!/usr/bin/env bash
# scripts/reset_db.sh —— 清库重置（N-025 D2）：功能实验从干净库开始。
#
# ★ 安全设计（任务包 D2）：
#   · **默认 dry-run**：只列出将清空的表与行数，不写任何东西；
#   · **显式 --force 才执行**；执行**第一步 = mysqldump 全库备份**
#     （文件名带时间戳，存 ~/jx-lab-trace/backups/，保留不删）；
#   · 执行后**断言校验**：清空表行数＝0；三张保留表行数与执行前一致；输出重置报告。
#
# ★ 保留（绝不触碰）：s_permission_point / s_role / s_role_permission ——
#   ★★ 三表红线（N-025 §6-1）：这三张表在本脚本任何路径下都不会被写
#   （权限矩阵已配好，新 sysadmin 登录后直接可用）。
#
# 用法：
#   bash scripts/reset_db.sh            # dry-run：只报告，不改库
#   bash scripts/reset_db.sh --force    # 真正执行：备份 → 清空 → 校验 → 报告
#
# 连接：取 JX_DB_DSN（环境变量优先，其次仓库根 .env —— 只读取，不 source）。
# SQL 通道（按序探测，命中即用）：
#   1) PATH 里的 mysql 客户端
#   2) docker exec 进 MySQL 容器（JX_MYSQL_CONTAINER，缺省 jx-lab-trace-mysql）
#   3) 仓库自带 Go 后端 bin/jxdbtool（本机/测试服务器都没有 mysql 客户端时的兜底；
#      有 go 就地构建）—— 只能跑 SQL，**备份仍需 mysqldump 或 docker**
# 退出码：0 = dry-run 完成 / --force 校验全过；1 = 校验失败或前置条件缺失。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FORCE=0
BACKUP_DIR="${JX_BACKUP_DIR:-$HOME/jx-lab-trace/backups}"
CONTAINER="${JX_MYSQL_CONTAINER:-jx-lab-trace-mysql}"

die() { echo "✗ $*" >&2; exit 1; }

for arg in "$@"; do
  case "$arg" in
    --force) FORCE=1 ;;
    -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "未知参数：$arg（只接受 --force）" ;;
  esac
done

# ---------- 连接信息 ----------

read_env_value() { # $1=键 $2=文件 ⇒ stdout 值（去引号、去首尾空白）；读不到 ⇒ 返回 1
  local line v
  [ -f "$2" ] || return 1
  line="$(grep -E "^[[:space:]]*$1[[:space:]]*=" "$2" | head -n 1)" || return 1
  [ -n "$line" ] || return 1
  v="${line#*=}"
  v="$(printf '%s' "$v" | tr -d '\042\047')"   # 去双/单引号（N-004 口径）
  v="${v#"${v%%[![:space:]]*}"}"               # 去前导空白
  v="${v%"${v##*[![:space:]]}"}"               # 去尾随空白
  [ -n "$v" ] || return 1
  printf '%s' "$v"
}

DSN="${JX_DB_DSN:-}"
if [ -z "$DSN" ]; then
  DSN="$(read_env_value JX_DB_DSN "$ROOT/.env" || true)"
fi
[ -n "$DSN" ] || die "缺 JX_DB_DSN（环境变量或 $ROOT/.env 里都没有）"

# 形如 user:pass@tcp(host:port)/db?params
# ★ 正则先放进单引号变量再比对：[[ ]] 右侧直接写 \" 会被 bash 的引号规则吃掉（实测踩坑）。
DSN_RE='^([^:@/]+):([^@]*)@tcp\(([^)]+)\)/([^?"]+)'
if [[ "$DSN" =~ $DSN_RE ]]; then
  DB_USER="${BASH_REMATCH[1]}"
  DB_PASS="${BASH_REMATCH[2]}"
  HOSTPORT="${BASH_REMATCH[3]}"
  DB_NAME="${BASH_REMATCH[4]}"
else
  die "JX_DB_DSN 无法解析（应形如 user:pass@tcp(host:port)/db?...）"
fi
if [[ "$HOSTPORT" == *:* ]]; then
  DB_HOST="${HOSTPORT%%:*}"
  DB_PORT="${HOSTPORT##*:}"
else
  DB_HOST="$HOSTPORT"
  DB_PORT="3306"
fi

# ---------- 清空清单与保留清单（任务包 D2 逐条列名） ----------
TRUNCATE_TABLES=(
  # 业务
  b_arrival_notice b_truck_lot b_bag b_label_print b_obj_void
  b_sample_group b_sample b_sample_retention b_sample_lend b_sample_destroy
  b_inspection b_inspection_result b_inspection_file
  b_production_batch b_batch_operation b_feed_record
  b_fg_lot b_fg_bag b_rework b_shipment b_shipment_item
  b_share_report b_share_access
  # 批次
  m_incoming_batch
  # 主数据（★ 全为试验/联调数据，用户 2026-10-18 定案清）
  m_customer m_composition m_material_type m_material
  m_test_item m_test_item_limit m_vehicle m_team m_user
  # 系统
  s_session s_user_role s_audit_log
)
KEEP_TABLES=(s_permission_point s_role s_role_permission)

# ---------- SQL / 备份 通道 ----------

SQL_MODE=""   # client | docker | helper
HELPER=""
DUMP_MODE=""  # client | docker（找不到 ⇒ 空 ⇒ --force 拒绝执行）

docker_usable() { command -v docker >/dev/null 2>&1 && docker ps >/dev/null 2>&1; }

resolve_helper() {
  local cand
  for cand in "$ROOT/bin/jxdbtool" "$ROOT/bin/jxdbtool-linux-amd64"; do
    if [ -x "$cand" ]; then HELPER="$cand"; return 0; fi
  done
  if command -v go >/dev/null 2>&1; then
    echo "  （未找到现成 bin/jxdbtool ⇒ go build 就地产出）"
    (cd "$ROOT" && go build -o bin/jxdbtool ./cmd/jxdbtool) || return 1
    [ -x "$ROOT/bin/jxdbtool" ] || return 1
    HELPER="$ROOT/bin/jxdbtool"
    return 0
  fi
  return 1
}

resolve_modes() {
  if command -v mysql >/dev/null 2>&1; then
    SQL_MODE=client
  elif docker_usable; then
    SQL_MODE=docker
  elif resolve_helper; then
    SQL_MODE=helper
  else
    die "没有可用的 SQL 通道：装 mysql 客户端、或给 docker 权限、或放一个 bin/jxdbtool（go build ./cmd/jxdbtool）"
  fi
  if command -v mysqldump >/dev/null 2>&1; then
    DUMP_MODE=client
  elif docker_usable; then
    DUMP_MODE=docker
  fi
}

# sql_scalar 'SELECT ...' ⇒ stdout 打单值（行数）
sql_scalar() {
  case "$SQL_MODE" in
    client)
      MYSQL_PWD="$DB_PASS" mysql --batch --skip-column-names \
        -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" "$DB_NAME" -e "$1" ;;
    docker)
      # 容器内连自己（127.0.0.1）：避开宿主端口 hairpin；库名/账号取自 DSN
      docker exec -e MYSQL_PWD="$DB_PASS" "$CONTAINER" mysql --batch --skip-column-names \
        -h127.0.0.1 -P"$DB_PORT" -u"$DB_USER" "$DB_NAME" -e "$1" ;;
    helper)
      "$HELPER" -dsn "$DSN" -q "$1" ;;
    *)
      return 1 ;;
  esac
}

# sql_exec_batch 'stmt1' 'stmt2' ... ⇒ 同一会话顺序执行，首错即停（语句来自本脚本固定清单）
sql_exec_batch() {
  local stmt joined="" args=()
  case "$SQL_MODE" in
    client)
      for stmt in "$@"; do joined="$joined$stmt;"; done
      MYSQL_PWD="$DB_PASS" mysql --batch \
        -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" "$DB_NAME" \
        -e "SET FOREIGN_KEY_CHECKS=0; $joined SET FOREIGN_KEY_CHECKS=1;" ;;
    docker)
      for stmt in "$@"; do joined="$joined$stmt;"; done
      docker exec -e MYSQL_PWD="$DB_PASS" "$CONTAINER" mysql --batch \
        -h127.0.0.1 -P"$DB_PORT" -u"$DB_USER" "$DB_NAME" \
        -e "SET FOREIGN_KEY_CHECKS=0; $joined SET FOREIGN_KEY_CHECKS=1;" ;;
    helper)
      for stmt in "$@"; do args+=(-x "$stmt"); done
      "$HELPER" -dsn "$DSN" "${args[@]}" ;;
    *)
      return 1 ;;
  esac
}

# dump_backup <目标文件> ⇒ mysqldump 全库；缺工具 ⇒ 返回 127
dump_backup() {
  case "$DUMP_MODE" in
    client)
      # MYSQL_PWD：口令不进命令行（ps 可见）；--no-tablespaces：MySQL 8 免 PROCESS 权限
      MYSQL_PWD="$DB_PASS" mysqldump --single-transaction --routines --triggers \
        --no-tablespaces -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" "$DB_NAME" > "$1" ;;
    docker)
      docker exec -e MYSQL_PWD="$DB_PASS" "$CONTAINER" mysqldump --single-transaction \
        --routines --triggers --no-tablespaces \
        -h127.0.0.1 -P"$DB_PORT" -u"$DB_USER" "$DB_NAME" > "$1" ;;
    *)
      return 127 ;;
  esac
}

count_rows() { sql_scalar "SELECT COUNT(*) FROM $1"; }

print_reminder() {
  echo
  echo "★ 提醒（N-025 D2）：重置后如重启服务，须先确认 .env 中 JX_BOOTSTRAP_ADMIN 已移除，"
  echo "  否则启动引导将抢占首个 sysadmin。"
  echo "  （该提示行的变量名对应实际环境变量 JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID；运维动作由 WorkBuddy 执行。）"
}

# ---------- 主流程 ----------

resolve_modes
probe="$(sql_scalar 'SELECT 1')" || die "数据库连不上（通道=$SQL_MODE，库=$DB_NAME @ $DB_HOST:$DB_PORT）"
[ "$probe" = "1" ] || die "连接探测异常（SELECT 1 返回 '$probe'）"

if [ "$FORCE" != 1 ]; then
  echo "===== reset_db.sh · DRY-RUN（默认：未执行任何写操作）====="
  echo "库        : $DB_NAME @ $DB_HOST:$DB_PORT"
  echo "SQL 通道  : $SQL_MODE${HELPER:+（$HELPER）}"
  if [ -n "$DUMP_MODE" ]; then
    echo "备份通道  : $DUMP_MODE（--force 第一步可用）"
  else
    echo "备份通道  : ✗ 未找到 mysqldump（--force 将被拒绝执行；装 mysql-client 或给 docker 权限）"
  fi
  echo
  echo "[将清空 · TRUNCATE，共 ${#TRUNCATE_TABLES[@]} 张]"
  total=0
  for t in "${TRUNCATE_TABLES[@]}"; do
    n="$(count_rows "$t")" || die "读 $t 行数失败（表缺失或连接不可用）"
    printf '  %-24s %s\n' "$t" "$n"
    total=$((total + n))
  done
  echo "  —— 合计 $total 行"
  echo
  echo "[将保留 · 绝不触碰，共 ${#KEEP_TABLES[@]} 张]"
  for t in "${KEEP_TABLES[@]}"; do
    n="$(count_rows "$t")" || die "读 $t 行数失败（表缺失或连接不可用）"
    printf '  %-24s %s\n' "$t" "$n"
  done
  echo
  echo "真正执行请用：bash scripts/reset_db.sh --force"
  echo "  --force 第一步 = mysqldump 全库备份到 $BACKUP_DIR/jx_lab_trace_<时间戳>.sql（保留不删）"
  print_reminder
  exit 0
fi

# ---------- --force ----------
echo "===== reset_db.sh · FORCE（真正执行）====="
echo "库        : $DB_NAME @ $DB_HOST:$DB_PORT"
echo "SQL 通道  : $SQL_MODE${HELPER:+（$HELPER）}"
[ -n "$DUMP_MODE" ] || die "找不到 mysqldump（PATH / docker 均无）—— 无备份不做清空，已中止"
mkdir -p "$BACKUP_DIR" || die "无法创建备份目录 $BACKUP_DIR"
BACKUP_FILE="$BACKUP_DIR/jx_lab_trace_$(date +%Y%m%d_%H%M%S).sql"

echo "[1/4] mysqldump 全库备份 → $BACKUP_FILE"
if ! dump_backup "$BACKUP_FILE"; then
  rm -f "$BACKUP_FILE"
  die "备份失败 —— 已中止（未做任何清空）"
fi
if [ ! -s "$BACKUP_FILE" ]; then
  die "备份文件为空 —— 已中止（未做任何清空）：$BACKUP_FILE"
fi
echo "  备份完成：$(wc -c < "$BACKUP_FILE") 字节（保留不删）"

echo "[2/4] 记录保留表行数（执行前基线）"
declare -A KEEP_PRE=()
for t in "${KEEP_TABLES[@]}"; do
  n="$(count_rows "$t")" || die "读 $t 行数失败"
  KEEP_PRE[$t]="$n"
done

echo "[3/4] TRUNCATE ${#TRUNCATE_TABLES[@]} 张表"
STMTS=()
for t in "${TRUNCATE_TABLES[@]}"; do
  STMTS+=("TRUNCATE TABLE $t")
done
sql_exec_batch "${STMTS[@]}" || die "TRUNCATE 执行失败 —— 库可能处于部分清空状态，请立即用备份恢复"

echo "[4/4] 校验 + 重置报告"
RC=0
echo "  [清空表（应为 0）]"
for t in "${TRUNCATE_TABLES[@]}"; do
  n="$(count_rows "$t")" || { echo "  ✗ $t 行数读取失败"; RC=1; continue; }
  if [ "$n" = "0" ]; then
    printf '  ✓ %-24s %s\n' "$t" "$n"
  else
    printf '  ✗ %-24s %s（应为 0）\n' "$t" "$n"
    RC=1
  fi
done
echo "  [保留表（应与执行前一致）]"
for t in "${KEEP_TABLES[@]}"; do
  n="$(count_rows "$t")" || { echo "  ✗ $t 行数读取失败"; RC=1; continue; }
  if [ "$n" = "${KEEP_PRE[$t]}" ]; then
    printf '  ✓ %-24s %s（执行前 %s）\n' "$t" "$n" "${KEEP_PRE[$t]}"
  else
    printf '  ✗ %-24s %s（执行前 %s，★ 变了）\n' "$t" "$n" "${KEEP_PRE[$t]}"
    RC=1
  fi
done
echo
echo "  备份文件：$BACKUP_FILE"
print_reminder

if [ "$RC" -ne 0 ]; then
  die "★ 校验失败（见上方 ✗ 行）—— 请用备份文件恢复后重试"
fi
echo "===== 重置完成：${#TRUNCATE_TABLES[@]} 张表已清空，${#KEEP_TABLES[@]} 张保留表行数未变 ====="
