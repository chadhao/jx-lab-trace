#!/usr/bin/env bash
# scripts/seed_demo.sh —— 演示种子脚本（N-031 / MIMO-NEXT-FIX-15 D2）。
#
# ★ 定位：**清库重置后一键生成各板块演示数据** 的仓库资产。
#   主流程走 **真实 Web API**（完整质控链：取样→组→检测单→项→结果→结论→投料），
#   **禁止 SQL 直插绕过业务红线**（取样对象=吨袋 B 码；投料前置=车次结论合格）。
#
# ★★ 幂等：用唯一业务键判存在，重复跑不重复建。
#   · 主数据：客户/物料/检测项按 code；判定限按 (test_item_id, customer=0, material=0)
#   · 收货：来料批次按 (customer_id, material_id, customer_batch_no=DEMO-N031-*)
#   · 取样：按车次查已存在份样数 ≥ 袋数则跳过 take（take 无已取样守卫，必须先查）
#   · 检测单：按车次现行任务态「已检」则跳过
#   · 投料：按袋码查 feed / genealogy has_flow，已投则跳过
#   · 成品/出货：按生产批已挂 fg-lot / 出货单含袋判定
#
# ★★ 存量吸收：若库内已有 1BCG 原料吨袋车次（WorkBuddy 收货链 / 本脚本先前轮次），
#   则**吸收**这些车次继续补检测链与投料，**不重建**收货数据。
#   仅在完全无带袋车次时，才按 --scale 新建 DEMO-N031-* 收货单元。
#
# ★ 用法（**在测试服务器上跑**，服务须已在 127.0.0.1:18080）：
#   bash scripts/seed_demo.sh                 # dry-run：只报告将做什么
#   bash scripts/seed_demo.sh --apply         # 真正执行
#   bash scripts/seed_demo.sh --apply --scale=2
#
# ★ 连接：dev-login 无 body ⇒ ou_dev_admin（sysadmin，测试服务器业务写全通）。
#   SQL 仅用于末尾行数报告（bin/jxdbtool，服务器无 mysql 客户端）。
# ★★ 不在生产数据上误跑（清库后的测试库用）。
# 退出码：0 = 成功（含 dry-run）；1 = 失败。
set -u

# ROOT：仓库根。经 `ssh host 'bash -s' < script` 执行时 BASH_SOURCE 为空 ⇒ 回退 $HOME/jx-lab-trace
_src="${BASH_SOURCE[0]:-}"
if [ -n "$_src" ] && [ -f "$_src" ]; then
  ROOT="$(cd "$(dirname "$_src")/.." && pwd)"
else
  ROOT="${JX_ROOT:-$HOME/jx-lab-trace}"
fi
[ -d "$ROOT" ] || ROOT="$HOME/jx-lab-trace"
A="${JX_BASE_URL:-http://127.0.0.1:18080}"
CK=/tmp/seed_demo.ck
SCALE=1
APPLY=0
PASS=0
FAIL=0
SKIP=0

# 支持 --scale=N 与 --scale N（任务包写法）
_args=()
while [ $# -gt 0 ]; do
  case "$1" in
    --apply) APPLY=1; shift ;;
    --scale)
      shift
      [ $# -ge 1 ] || { echo "✗ --scale 缺少取值" >&2; exit 1; }
      SCALE="$1"; shift ;;
    --scale=*) SCALE="${1#--scale=}"; shift ;;
    -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) _args+=("$1"); shift ;;
  esac
done
case "$SCALE" in ''|*[!0-9]*) echo "✗ --scale 须为正整数" >&2; exit 1 ;; esac
[ "$SCALE" -ge 1 ] || { echo "✗ --scale 须 ≥1" >&2; exit 1; }
if [ ${#_args[@]} -gt 0 ]; then
  echo "✗ 未知参数：${_args[*]}" >&2; exit 1
fi

py() { python3 -c "$1"; }
log()  { printf '%s\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '  ✓ %s\n' "$*"; }
miss() { FAIL=$((FAIL+1)); printf '  ✗ %s\n' "$*"; }
skip() { SKIP=$((SKIP+1)); printf '  ○ %s\n' "$*"; }
plan() { printf '  · [plan] %s\n' "$*"; }
die()  { echo "✗ $*" >&2; exit 1; }

req() { # req <期望> <方法> <路径> [body]
  local want="$1" m="$2" p="$3" body="${4:-}" code
  if [ -z "$body" ] || [ "$body" = "-" ]; then
    code=$(curl -s -b "$CK" -o /tmp/seed_r.json -w '%{http_code}' -X "$m" "$A$p")
  else
    code=$(curl -s -b "$CK" -o /tmp/seed_r.json -w '%{http_code}' -X "$m" \
      -H 'Content-Type: application/json' -d "$body" "$A$p")
  fi
  if [ "$code" = "$want" ]; then
    if [ "$APPLY" -eq 1 ]; then ok "$m $p ⇒ $code"
    else plan "$m $p ⇒ $code"; fi
    return 0
  fi
  miss "$m $p ⇒ $code（期望 $want） body=$(head -c 240 /tmp/seed_r.json)"
  return 1
}

req_code() { # 输出 http 码，body → /tmp/seed_r.json
  local m="$1" p="$2" body="${3:-}"
  if [ -z "$body" ] || [ "$body" = "-" ]; then
    curl -s -b "$CK" -o /tmp/seed_r.json -w '%{http_code}' -X "$m" "$A$p"
  else
    curl -s -b "$CK" -o /tmp/seed_r.json -w '%{http_code}' -X "$m" \
      -H 'Content-Type: application/json' -d "$body" "$A$p"
  fi
}

jget() { # jget <path> [file]
  local f="${2:-/tmp/seed_r.json}"
  python3 - "$1" "$f" <<'PY'
import json,sys
path, f = sys.argv[1], sys.argv[2]
try:
    d=json.load(open(f,encoding='utf-8'))
except Exception:
    print(""); sys.exit(0)
for k in path.split('.'):
    if k=='': continue
    if isinstance(d,list):
        try: d=d[int(k)]
        except Exception: print(""); sys.exit(0)
    else:
        d=d.get(k) if isinstance(d,dict) else None
    if d is None: print(""); sys.exit(0)
print(d if not isinstance(d,(dict,list)) else json.dumps(d,ensure_ascii=False)[:200])
PY
}

echo "════════ seed_demo · scale=$SCALE mode=$([ $APPLY -eq 1 ] && echo APPLY || echo DRY-RUN) ════════"

# ---------- 0. 登录 ----------
log "══ 0. dev-login ══"
code=$(curl -s -c "$CK" -o /tmp/seed_login.json -w '%{http_code}' -X POST "$A/api/auth/dev-login")
if [ "$code" = "200" ]; then
  ok "dev-login http=200（ou_dev_admin）"
else
  die "dev-login 失败 http=$code body=$(head -c 200 /tmp/seed_login.json)"
fi

# ---------- 1. 主数据（幂等） ----------
log "══ 1. 主数据（客户/物料/检测项/判定限） ══"

CID_A=""; CID_B=""; M_IN=""; M_OUT=""
declare -A TI_ID

ensure_customer() { # code name → 全局 CID_<code>
  local code="$1" name="$2" hit
  curl -s -b "$CK" "$A/api/md/customers?q=$code" -o /tmp/seed_q.json
  hit=$(python3 -c "
import json
d=json.load(open('/tmp/seed_q.json',encoding='utf-8'))
print(next((r['id'] for r in d.get('rows') or [] if r.get('code')=='$code'),''))
")
  if [ -n "$hit" ]; then
    skip "客户 $code 已存在 id=$hit"
    eval "CID_$code=$hit"
    return 0
  fi
  if [ "$APPLY" -eq 1 ]; then
    req 200 POST /api/md/customers "{\"values\":{\"code\":\"$code\",\"name\":\"$name\"}}" || die "建客户 $code"
    eval "CID_$code=$(jget row.id)"
    ok "客户 $code 新建 id=$(eval echo \$CID_$code)"
  else
    plan "POST /api/md/customers {code:$code, name:$name}"
  fi
}

ensure_material() { # code name kind
  local code="$1" name="$2" kind="$3" hit
  curl -s -b "$CK" "$A/api/md/materials?q=$code" -o /tmp/seed_q.json
  hit=$(python3 -c "
import json
d=json.load(open('/tmp/seed_q.json',encoding='utf-8'))
print(next((r['id'] for r in d.get('rows') or [] if r.get('code')=='$code'),''))
")
  if [ -n "$hit" ]; then
    skip "物料 $code ($kind) 已存在 id=$hit"
    eval "MID_$code=$hit"
    return 0
  fi
  if [ "$APPLY" -eq 1 ]; then
    body=$(python3 -c "import json;print(json.dumps({'values':{'code':'$code','name':'$name','kind':'$kind'}},ensure_ascii=False))")
    req 200 POST /api/md/materials "$body" || die "建物料 $code"
    eval "MID_$code=$(jget row.id)"
    ok "物料 $code 新建 id=$(eval echo \$MID_$code)"
  else
    plan "POST /api/md/materials {code:$code, name:$name, kind:$kind}"
  fi
}

ensure_customer 0301 "华晨陶瓷原料"
ensure_customer 0302 "岳阳鑫达材料厂"
CID_A="${CID_0301:-}"; CID_B="${CID_0302:-}"

ensure_material 0310 "高铝矾土熟料" "原料"
ensure_material 0311 "活性氧化铝微粉" "成品"
M_IN="${MID_0310:-}"; M_OUT="${MID_0311:-}"

curl -s -b "$CK" "$A/api/md/test-items" -o /tmp/seed_ti.json
ensure_ti() { # code name unit method sort
  local code="$1" name="$2" unit="$3" method="$4" sort="$5" hit
  hit=$(python3 -c "
import json
d=json.load(open('/tmp/seed_ti.json',encoding='utf-8'))
print(next((r['id'] for r in d.get('rows') or [] if r.get('code')=='$code'),''))
")
  if [ -n "$hit" ]; then
    skip "检测项 $code ($name) 已存在 id=$hit"
    TI_ID[$code]=$hit
    return 0
  fi
  if [ "$APPLY" -eq 1 ]; then
    body=$(python3 -c "import json;print(json.dumps({'values':{'code':'$code','name':'$name','value_type':'数值','unit':'$unit','method':'$method','sort':int('$sort')}},ensure_ascii=False))")
    req 200 POST /api/md/test-items "$body" || die "建检测项 $code"
    TI_ID[$code]=$(jget row.id)
    ok "检测项 $code 新建 id=${TI_ID[$code]}"
  else
    plan "POST /api/md/test-items {code:$code, name:$name}"
  fi
}
ensure_ti 9501 "水分" "%" "烘干法" 1
ensure_ti 9502 "粒度D50" "μm" "激光粒度仪" 2
ensure_ti 9503 "Al2O3含量" "%" "X荧光" 3

# 判定限（通用 0/0）
for spec in "9501:0:8" "9502:20:60" "9503:85:92"; do
  IFS=: read -r code lo hi <<<"$spec"
  tid="${TI_ID[$code]:-}"
  has=""
  if [ -n "$tid" ]; then
    curl -s -b "$CK" "$A/api/md/test-item-limits?test_item_id=$tid" -o /tmp/seed_lim.json
    has=$(python3 -c "
import json
d=json.load(open('/tmp/seed_lim.json',encoding='utf-8'))
print('yes' if any(r.get('customer_id')==0 and r.get('material_id')==0 for r in d.get('rows') or []) else '')
")
  fi
  if [ -n "$has" ]; then
    skip "判定限 $code 通用限已存在"
  elif [ "$APPLY" -eq 1 ] && [ -n "$tid" ]; then
    body=$(python3 -c "import json;print(json.dumps({'values':{'test_item_id':int('$tid'),'customer_id':0,'material_id':0,'lower_limit':$lo,'upper_limit':$hi,'is_required':True}}))")
    req 200 POST /api/md/test-item-limits "$body" || die "建判定限 $code"
  else
    plan "POST /api/md/test-item-limits {item:$code, 0/0, $lo–$hi}"
  fi
done

ITEM_IDS=$(python3 -c "
ids=['${TI_ID[9501]:-}','${TI_ID[9502]:-}','${TI_ID[9503]:-}']
print(','.join(i for i in ids if i))
")

# ---------- 2. 收货链：吸收或新建 ----------
log "══ 2. 收货链（吸收已有带袋 B 车次 / 或按 DEMO-N031-* 新建） ══"

scan_bags_to_tsv() { # 重扫演示客户名下带 1BCG 袋的车次（按 id 升序）→ /tmp/seed_demo_trucks.tsv
  : > /tmp/seed_demo_trucks.tsv
  : > /tmp/seed_all_bags.tsv
  curl -s -b "$CK" "$A/api/recv/trucks" -o /tmp/seed_trucks.json
  # 只收演示客户（0301/0302）名下的带袋车次，按 id 升序（保证 P1/P2 投料分配稳定）
  python3 - <<'PY' > /tmp/seed_truck_ids.txt
import json
d=json.load(open('/tmp/seed_trucks.json',encoding='utf-8'))
rows=d.get('rows') or []
demo=set()
if open('/tmp/seed_demo_cids.txt').read().strip():
    demo={int(x) for x in open('/tmp/seed_demo_cids.txt').read().split() if x}
# 无演示客户 id 时退化为全部（新建路径）
pick=[r for r in rows if (not demo or r.get('customer_id') in demo)]
pick.sort(key=lambda r: r.get('id') or 0)
print(' '.join(str(r['id']) for r in pick))
PY
  for t in $(cat /tmp/seed_truck_ids.txt); do
    curl -s -b "$CK" "$A/api/recv/trucks/$t/bags" -o /tmp/seed_bags.json
    python3 - "$t" <<'PY' >> /tmp/seed_demo_trucks.tsv
import json,sys
t=sys.argv[1]
d=json.load(open('/tmp/seed_bags.json',encoding='utf-8'))
codes=[r.get('code') for r in d.get('rows') or [] if r.get('code')]
if codes and codes[0].startswith('1BCG'):
    print(f"{t}\t{','.join(codes)}")
PY
    python3 - "$t" <<'PY' >> /tmp/seed_all_bags.tsv
import json,sys
t=sys.argv[1]
d=json.load(open('/tmp/seed_bags.json',encoding='utf-8'))
for r in d.get('rows') or []:
    if (r.get('code') or '').startswith('1BCG'):
        print(f"{r.get('id')}\t{r.get('code')}\t{t}\t{r.get('status')}")
PY
  done
  wc -l < /tmp/seed_demo_trucks.tsv | tr -d ' '
}

: > /tmp/seed_demo_cids.txt
[ -n "${CID_A:-}" ] && echo "$CID_A" >> /tmp/seed_demo_cids.txt
[ -n "${CID_B:-}" ] && echo "$CID_B" >> /tmp/seed_demo_cids.txt

N_ABSORB=$(scan_bags_to_tsv)
log "  吸收候选车次（演示客户名下带 1BCG 袋，id 升序）：$N_ABSORB"

make_recv_unit() { # cid cbn bags_spec(逗号分隔每车袋数) gross_base
  local cid="$1" cbn="$2" bags_spec="$3" gbase="$4" bid=""
  curl -s -b "$CK" "$A/api/recv/search?customer_id=$cid&customer_batch_no=$cbn" -o /tmp/seed_sb.json
  bid=$(python3 -c "
import json
d=json.load(open('/tmp/seed_sb.json',encoding='utf-8'))
print(next((b.get('id') for b in d.get('batches') or [] if b.get('customer_batch_no')=='$cbn'),''))
")
  if [ -n "$bid" ]; then
    skip "批次 $cbn 已存在 id=$bid"
  elif [ "$APPLY" -eq 1 ]; then
    req 200 POST /api/recv/batches "{\"customer_id\":$cid,\"material_id\":$M_IN,\"customer_batch_no\":\"$cbn\"}" || die "建批次 $cbn"
    bid=$(jget row.id)
    ok "批次 $cbn id=$bid"
  else
    plan "POST /api/recv/batches {cust:$cid, mat:$M_IN, cbn:$cbn}"
  fi

  local idx=0 n_bags
  local IFS_SAVE=$IFS
  IFS=','
  for n_bags in $bags_spec; do
    IFS=$IFS_SAVE
    idx=$((idx+1))
    local plate="湘F·DJ$(printf '%02d' $idx)$(printf '%s' "$cbn" | tail -c 2 | tr -d '-')"
    # 是否已有该序车（按 plate 搜）
    curl -s -b "$CK" "$A/api/recv/search?customer_batch_no=$cbn" -o /tmp/seed_sn.json
    t=$(python3 -c "
import json
d=json.load(open('/tmp/seed_sn.json',encoding='utf-8'))
for n in d.get('notices') or []:
    tid=n.get('truck_id') or n.get('truck_lot_id') or n.get('truck_lot') and n['truck_lot'].get('id')
    if isinstance(tid,dict): tid=tid.get('id')
    if tid: print(tid); break
")
    if [ -n "$t" ]; then
      skip "车 $cbn#$idx 已存在 truck=$t"
    elif [ -n "$bid" ] && [ "$APPLY" -eq 1 ]; then
      req 200 POST /api/recv/notices "{\"batch_id\":$bid,\"plate_no\":\"$plate\",\"driver\":\"种子司机$idx\"}" || return 1
      nid=$(jget row.id)
      req 200 POST /api/recv/arrivals "{\"notice_id\":$nid}" || return 1
      t=$(jget row.id)
      g1=$(python3 -c "print(round($gbase + $idx * 0.7, 2))")
      g2=$(python3 -c "print(round(1.20 + ($idx % 3) * 0.05, 2))")
      req 200 POST "/api/recv/trucks/$t/weigh" "{\"gross_weight\":$g1,\"tare_weight\":$g2}" || return 1
      req 200 POST "/api/recv/trucks/$t/bags" "{\"count\":$n_bags}" || return 1
      ok "车 $plate id=$t 袋×$n_bags"
    else
      plan "车 $cbn#$idx：预报$plate → 到货 → 过磅 → 袋×$n_bags"
    fi
    IFS=','
  done
  IFS=$IFS_SAVE
  return 0
}

if [ "$N_ABSORB" -eq 0 ]; then
  log "  无可吸收车次 → 新建 DEMO-N031 收货单元 ×$SCALE"
  for i in $(seq 1 "$SCALE"); do
    [ -n "$CID_A" ] && make_recv_unit "$CID_A" "DEMO-N031-A-$i" "3,2" 28.0
    [ -n "$CID_B" ] && make_recv_unit "$CID_B" "DEMO-N031-B-$i" "2" 27.0
  done
  N_ABSORB=$(scan_bags_to_tsv)
  log "  扫描后候选车次：$N_ABSORB"
fi

[ "$N_ABSORB" -gt 0 ] || die "无任何带袋车次可继续（收货链为空）"

# ---------- 3. 检测链（每带袋车次幂等补全） ----------
log "══ 3. 检测链（取样→组→检测单→项→结果→结论） ══"
# ★ 红线：取样对象=吨袋 B 码；投料前置=结论合格——全程 API，无 SQL 绕过。
# ★ PATCH items/:itemId 的 itemId = m_test_item.id（非清单行 id）。
# ★ 合格禁填处置 → 不调 disposition。

declare -A RES_VAL RES_UNIT
RES_VAL[9501]=5.0;  RES_UNIT[9501]='%'
RES_VAL[9502]=40;   RES_UNIT[9502]='μm'
RES_VAL[9503]=88.0; RES_UNIT[9503]='%'

insp_state_of() { # truck_id → insp_state 或空
  local t="$1"
  curl -s -b "$CK" "$A/api/insp/tasks" -o /tmp/seed_tasks.json
  python3 -c "
import json
d=json.load(open('/tmp/seed_tasks.json',encoding='utf-8'))
for r in d.get('rows') or []:
    if r.get('target_type')=='车次' and r.get('target_id')==int('$t'):
        print(r.get('insp_state') or ''); break
"
}

insp_id_of() { # truck_id → inspection_id 或空
  local t="$1"
  curl -s -b "$CK" "$A/api/insp/tasks" -o /tmp/seed_tasks.json
  python3 -c "
import json
d=json.load(open('/tmp/seed_tasks.json',encoding='utf-8'))
for r in d.get('rows') or []:
    if r.get('target_type')=='车次' and r.get('target_id')==int('$t'):
        v=r.get('inspection_id') or r.get('id') or ''
        print(v); break
"
}

ensure_detection() { # truck_id bag_codes_csv
  local t="$1" bags_csv="$2"
  local state
  state=$(insp_state_of "$t")
  if [ "$state" = "已检" ]; then
    skip "车 $t 检测已完结（已检）"
    return 0
  fi

  # 3a. 取样
  curl -s -b "$CK" "$A/api/sample/samples?role=%E4%BB%BD%E6%A0%B7&target_type=%E8%BD%A6%E6%AC%A1&target_id=$t" -o /tmp/seed_ss.json
  have_n=$(jget count /tmp/seed_ss.json); have_n=${have_n:-0}
  bag_n=$(python3 -c "print(len('''$bags_csv'''.split(',')))")
  if [ "$have_n" -ge "$bag_n" ] 2>/dev/null; then
    skip "车 $t 份样已齐 count=$have_n（≥袋数 $bag_n）"
  else
    local IFS_SAVE=$IFS bag
    IFS=','
    for bag in $bags_csv; do
      IFS=$IFS_SAVE
      if [ "$APPLY" -eq 1 ]; then
        code=$(req_code POST /api/sample/take "{\"code\":\"$bag\"}")
        if [ "$code" = "200" ]; then
          ok "取样 $bag"
        else
          miss "取样 $bag http=$code $(head -c 160 /tmp/seed_r.json)"
          return 1
        fi
      else
        plan "POST /api/sample/take {code:$bag}（若未取）"
      fi
      IFS=','
    done
    IFS=$IFS_SAVE
    # 重查
    curl -s -b "$CK" "$A/api/sample/samples?role=%E4%BB%BD%E6%A0%B7&target_type=%E8%BD%A6%E6%AC%A1&target_id=$t" -o /tmp/seed_ss.json
  fi

  # 3b. 取样组
  curl -s -b "$CK" "$A/api/sample/groups?target_type=%E8%BD%A6%E6%AC%A1" -o /tmp/seed_gs.json
  has_grp=$(python3 -c "
import json
d=json.load(open('/tmp/seed_gs.json',encoding='utf-8'))
print('yes' if any(g.get('target_id')==int('$t') for g in d.get('rows') or []) else '')
")
  if [ -n "$has_grp" ]; then
    skip "车 $t 取样组已存在"
  else
    sids=$(python3 -c "
import json
d=json.load(open('/tmp/seed_ss.json',encoding='utf-8'))
print(','.join(str(r['id']) for r in d.get('rows') or [] if not r.get('group_id')))
")
    if [ -z "$sids" ]; then
      # dry-run 下可能尚未 take，用将有袋数占位
      if [ "$APPLY" -eq 1 ]; then
        miss "车 $t 无可并入份样"
        return 1
      else
        plan "POST /api/sample/groups 车$t（份样=take 后未并入者）"
        sids=""
      fi
    fi
    if [ -n "$sids" ] || [ "$APPLY" -eq 0 ]; then
      body=$(python3 -c "
import json
ids=[int(x) for x in '''$sids'''.split(',') if x]
print(json.dumps({'target_type':'车次','target_id':int('$t'),'sample_ids':ids,'remark':'N-031种子'},ensure_ascii=False))
")
      if [ "$APPLY" -eq 1 ]; then
        req 200 POST /api/sample/groups "$body" || return 1
      else
        plan "POST /api/sample/groups 车$t 份样[$sids]"
      fi
    fi
  fi

  # 3c. 检测单
  local iid
  iid=$(insp_id_of "$t")
  if [ -n "$iid" ]; then
    skip "车 $t 检测单已存在 id=$iid"
  elif [ "$APPLY" -eq 1 ]; then
    req 200 POST /api/insp "{\"target_type\":\"车次\",\"target_id\":$t,\"inspector\":\"种子质检员\",\"method\":\"抽检\"}" || return 1
    iid=$(jget row.id)
    ok "检测单 车$t id=$iid"
  else
    plan "POST /api/insp 车$t"
    return 0
  fi

  # dry-run 且 iid 空已 return；下方仅 apply 或已有 iid
  if [ "$APPLY" -eq 0 ]; then
    plan "检测单 $iid：补项/结果/结论（按需）"
    return 0
  fi

  # 3d. 加项
  curl -s -b "$CK" "$A/api/insp/$iid/items" -o /tmp/seed_ii.json
  cur=$(jget count /tmp/seed_ii.json); cur=${cur:-0}
  if [ "$cur" = "0" ]; then
    req 200 POST "/api/insp/$iid/items" "{\"item_ids\":[$ITEM_IDS]}" || return 1
    curl -s -b "$CK" "$A/api/insp/$iid/items" -o /tmp/seed_ii.json
  else
    skip "检测单 $iid 项已挂 count=$cur"
  fi

  # 3e. 录结果（itemId = m_test_item.id）
  for code in 9501 9502 9503; do
    tid="${TI_ID[$code]:-}"
    [ -n "$tid" ] || continue
    st=$(python3 -c "
import json
d=json.load(open('/tmp/seed_ii.json',encoding='utf-8'))
for r in d.get('rows') or []:
    if str(r.get('item_code'))=='$code' or str(r.get('item_id'))=='$tid':
        print(r.get('state') or ''); break
")
    if [ "$st" = "已测" ]; then
      skip "结果 $code 已测"
      continue
    fi
    attached=$(python3 -c "
import json
d=json.load(open('/tmp/seed_ii.json',encoding='utf-8'))
print('yes' if any(str(r.get('item_code'))=='$code' or str(r.get('item_id'))=='$tid' for r in d.get('rows') or []) else '')
")
    if [ -z "$attached" ]; then
      skip "结果 $code 未挂项"
      continue
    fi
    body=$(python3 -c "import json;print(json.dumps({'state':'已测','value_num':${RES_VAL[$code]},'unit':'''${RES_UNIT[$code]}'''},ensure_ascii=False))")
    req 200 PATCH "/api/insp/$iid/items/$tid" "$body" || return 1
  done

  # 3f. 结论合格
  curl -s -b "$CK" "$A/api/insp/$iid" -o /tmp/seed_id.json
  concl=$(jget conclusion /tmp/seed_id.json)
  # 单详情可能无顶层 conclusion；查任务态
  if [ -z "$concl" ]; then
    state=$(insp_state_of "$t")
    if [ "$state" = "已检" ] || [ "$state" = "已结论" ]; then
      skip "检测单 $iid 已有结论（任务态=$state）"
      return 0
    fi
  else
    skip "检测单 $iid 已有结论=$concl"
    return 0
  fi
  req 200 POST "/api/insp/$iid/conclusion" '{"conclusion":"合格","remark":"N-031种子"}' || return 1
  # ★ 处置不在合格链上：合格禁填处置
  return 0
}

while IFS=$'\t' read -r t bags; do
  [ -n "${t:-}" ] || continue
  log "  —— 车 $t ——"
  ensure_detection "$t" "$bags" || die "车 $t 检测链失败"
done < /tmp/seed_demo_trucks.tsv

# ---------- 4. 生产批 + 投料 ----------
log "══ 4. 生产批与投料（红线：仅结论合格后） ══"
# 投料分配（D1.4 / 探针口径）：第1车→P1、第2车→P2；第3车起保持在库。

ensure_prod_batch() { # cid remark → stdout id
  local cid="$1" remark="$2" pid
  curl -s -b "$CK" "$A/api/prod/batches" -o /tmp/seed_pb.json
  pid=$(python3 -c "
import json
d=json.load(open('/tmp/seed_pb.json',encoding='utf-8'))
for r in d.get('rows') or []:
    if r.get('customer_id')==int('$cid') and (r.get('remark') or '')=='$remark':
        print(r.get('id')); raise SystemExit
for r in d.get('rows') or []:
    if r.get('customer_id')==int('$cid'):
        print(r.get('id')); raise SystemExit
")
  if [ -n "$pid" ]; then
    skip "生产批 cust=$cid ($remark) 已存在 id=$pid"
    echo "$pid"
    return 0
  fi
  if [ "$APPLY" -eq 1 ]; then
    body=$(python3 -c "
import json,datetime
print(json.dumps({
  'customer_id':int('$cid'),
  'input_material_id':int('$M_IN'),
  'planned_output_material_id':int('$M_OUT'),
  'batch_date':datetime.date.today().isoformat(),
  'remark':'$remark'
},ensure_ascii=False))
")
    req 200 POST /api/prod/batches "$body" || return 1
    jget row.id
  else
    plan "POST /api/prod/batches cust=$cid remark=$remark"
    echo ""
  fi
}

# id 写入文件，日志走原 stdout（避免 stdout 捕获污染）
PID_A_FILE=/tmp/seed_pida.txt
PID_B_FILE=/tmp/seed_pidb.txt
ensure_prod_to() { # cid remark outfile
  local cid="$1" remark="$2" out="$3" pid
  curl -s -b "$CK" "$A/api/prod/batches" -o /tmp/seed_pb.json
  pid=$(python3 -c "
import json
d=json.load(open('/tmp/seed_pb.json',encoding='utf-8'))
for r in d.get('rows') or []:
    if r.get('customer_id')==int('$cid') and (r.get('remark') or '')=='$remark':
        print(r.get('id')); raise SystemExit
for r in d.get('rows') or []:
    if r.get('customer_id')==int('$cid'):
        print(r.get('id')); raise SystemExit
")
  if [ -n "$pid" ]; then
    skip "生产批 cust=$cid ($remark) 已存在 id=$pid"
    echo "$pid" > "$out"
    return 0
  fi
  if [ "$APPLY" -eq 1 ]; then
    body=$(python3 -c "
import json,datetime
print(json.dumps({
  'customer_id':int('$cid'),
  'input_material_id':int('$M_IN'),
  'planned_output_material_id':int('$M_OUT'),
  'batch_date':datetime.date.today().isoformat(),
  'remark':'$remark'
},ensure_ascii=False))
")
    req 200 POST /api/prod/batches "$body" || return 1
    jget row.id > "$out"
  else
    plan "POST /api/prod/batches cust=$cid remark=$remark"
    echo "" > "$out"
  fi
  return 0
}

ensure_prod_to "${CID_A:-0}" "N-031-P1" "$PID_A_FILE" || die "P1"
ensure_prod_to "${CID_B:-0}" "N-031-P2" "$PID_B_FILE" || die "P2"
PID_A=$(cat "$PID_A_FILE" 2>/dev/null || true)
PID_B=$(cat "$PID_B_FILE" 2>/dev/null || true)
[ -n "$PID_A" ] && log "  P1 id=$PID_A"
[ -n "$PID_B" ] && log "  P2 id=$PID_B"

feed_bag() { # pid bag weight
  local pid="$1" bag="$2" w="$3"
  [ -n "$pid" ] || { miss "无生产批不可投 $bag"; return 1; }
  done_fed=""
  curl -s -b "$CK" "$A/api/prod/batches/$pid/feeds" -o /tmp/seed_fr.json
  done_fed=$(python3 -c "
import json
d=json.load(open('/tmp/seed_fr.json',encoding='utf-8'))
print('yes' if any((r.get('bag_code') or '')=='$bag' for r in d.get('rows') or []) else '')
")
  if [ -z "$done_fed" ]; then
    curl -s -b "$CK" "$A/api/prod/genealogy/bags/$bag" -o /tmp/seed_g.json
    gf=$(jget has_flow /tmp/seed_g.json)
    echo "$gf" | grep -qiE '^(true|1)$' && done_fed=yes
  fi
  if [ -n "$done_fed" ]; then
    skip "已投料 $bag → 批$pid"
    return 0
  fi
  if [ "$APPLY" -eq 1 ]; then
    req 200 POST "/api/prod/batches/$pid/feeds" "{\"bag_code\":\"$bag\",\"feed_weight\":$w,\"operator\":\"种子投料工\"}"
  else
    plan "POST /api/prod/batches/$pid/feeds bag=$bag w=$w"
  fi
}

line_no=0
while IFS=$'\t' read -r t bags; do
  [ -n "${t:-}" ] || continue
  line_no=$((line_no+1))
  if [ "$line_no" -eq 1 ]; then
    target_pid="$PID_A"
  elif [ "$line_no" -eq 2 ]; then
    target_pid="$PID_B"
  else
    skip "车 $t 袋保持在库未投（对照样例）"
    continue
  fi
  [ -n "$target_pid" ] || { [ "$APPLY" -eq 1 ] && die "第$line_no车无目标生产批"; }
  _ifs_save=$IFS
  IFS=','
  for bag in $bags; do
    IFS=$_ifs_save
    w=$(python3 -c "import hashlib;print(round(1.20 + (int(hashlib.md5('$bag'.encode()).hexdigest()[:4],16) % 5) * 0.01, 2))")
    feed_bag "$target_pid" "$bag" "$w" || die "投料失败 $bag"
    IFS=','
  done
  IFS=$_ifs_save
done < /tmp/seed_demo_trucks.tsv

# ---------- 5. 成品批/袋 + 出货 ----------
log "══ 5. 成品批与出货 ══"
: > /tmp/seed_e_codes.txt

ensure_fg() { # pid want_n
  local pid="$1" want_n="$2" lot have add
  [ -n "$pid" ] || return 0
  curl -s -b "$CK" "$A/api/prod/batches/$pid/fg-lots" -o /tmp/seed_fl.json
  lot=$(python3 -c "
import json
d=json.load(open('/tmp/seed_fl.json',encoding='utf-8'))
rows=d.get('rows') or []
print(rows[0].get('id') if rows else '')
")
  if [ -z "$lot" ]; then
    if [ "$APPLY" -eq 1 ]; then
      body=$(python3 -c "import json;print(json.dumps({'pack_spec':'吨袋','net_weight':20,'remark':'N-031种子'},ensure_ascii=False))")
      if ! req 200 POST "/api/prod/batches/$pid/fg-lots" "$body"; then
        body=$(python3 -c "import json;print(json.dumps({'pack_spec':'吨袋','net_weight':20},ensure_ascii=False))")
        req 200 POST "/api/prod/batches/$pid/fg-lots" "$body" || return 1
      fi
      lot=$(jget row.id)
      ok "成品批 生产$pid → lot=$lot"
    else
      plan "POST /api/prod/batches/$pid/fg-lots（吨袋，目标袋×$want_n）"
      return 0
    fi
  else
    skip "生产批 $pid 成品批已存在 lot=$lot"
  fi
  curl -s -b "$CK" "$A/api/prod/fg-lots/$lot/bags" -o /tmp/seed_fb.json
  have=$(jget count /tmp/seed_fb.json); have=${have:-0}
  if [ "$have" -ge "$want_n" ] 2>/dev/null; then
    skip "成品袋已齐 lot=$lot count=$have"
  else
    add=$((want_n - have))
    if [ "$APPLY" -eq 1 ]; then
      req 200 POST "/api/prod/fg-lots/$lot/bags" "{\"count\":$add}" || return 1
      ok "成品袋 +$add → lot=$lot"
    else
      plan "POST /api/prod/fg-lots/$lot/bags count=$add"
    fi
  fi
  curl -s -b "$CK" "$A/api/prod/fg-lots/$lot/bags" -o /tmp/seed_fb.json
  python3 -c "
import json
d=json.load(open('/tmp/seed_fb.json',encoding='utf-8'))
rows=d.get('rows') or []
print(rows[0].get('code') if rows else '')
" >> /tmp/seed_e_codes.txt
}

ensure_fg "$PID_A" 4 || die "P1 成品失败"
ensure_fg "$PID_B" 2 || die "P2 成品失败"

# 出货：第一个未出场 E 袋
E1=$(head -n1 /tmp/seed_e_codes.txt 2>/dev/null || true)
if [ -n "${E1:-}" ]; then
  curl -s -b "$CK" "$A/api/ship/shipments" -o /tmp/seed_sh_list.json
  in_ship=""
  for sid in $(python3 -c "
import json
d=json.load(open('/tmp/seed_sh_list.json',encoding='utf-8'))
print(' '.join(str(r.get('id')) for r in d.get('rows') or [] if r.get('id')))
"); do
    curl -s -b "$CK" "$A/api/ship/shipments/$sid" -o /tmp/seed_sh.json
    hit=$(python3 -c "
import json
d=json.load(open('/tmp/seed_sh.json',encoding='utf-8'))
blob=json.dumps(d,ensure_ascii=False)
print('yes' if '''$E1''' in blob else '')
")
    [ -n "$hit" ] && in_ship=yes && break
  done
  if [ -n "$in_ship" ]; then
    skip "E袋 $E1 已在出货单"
  elif [ "$APPLY" -eq 1 ]; then
    code=$(req_code POST /api/ship/shipments "{\"bag_codes\":[\"$E1\"],\"remark\":\"N-031种子出货\"}")
    if [ "$code" = "200" ]; then
      ok "出货单含 $E1"
      shid=$(jget row.id)
      curl -s -b "$CK" "$A/api/ship/shipments/$shid" -o /tmp/seed_sh.json
      st=$(jget status /tmp/seed_sh.json)
      if [ -n "$st" ] && [ "$st" != "已出场" ]; then
        req 200 POST "/api/ship/shipments/$shid/depart" '{"plate_no":"湘A·DJ88","driver":"种子司机"}' || true
      fi
    else
      miss "建出货单失败 http=$code $(head -c 160 /tmp/seed_r.json)"
    fi
  else
    plan "POST /api/ship/shipments bag=$E1 → depart"
  fi
fi

# ---------- 6. 谱系抽测 ----------
log "══ 6. 谱系抽测（E 码 backward，非 genealogy/bags） ══"
# ★ 任务包 D4 写 genealogy/bags/<E码> 与实现不符（该端点只收 B 码）；
#   E 码 backward 全链走 /api/trace/backward?fg_code=
if [ "$APPLY" -eq 1 ]; then
  ec=$(head -n1 /tmp/seed_e_codes.txt 2>/dev/null || true)
  if [ -n "${ec:-}" ]; then
    curl -s -b "$CK" "$A/api/trace/backward?fg_code=$ec" -o /tmp/seed_bw.json
    hf=$(jget has_flow /tmp/seed_bw.json)
    # has_flow 可能是 true/True/1
    if echo "$hf" | grep -qiE '^(true|1)$'; then
      ok "backward $ec has_flow=$hf"
    else
      miss "backward $ec has_flow=$hf $(head -c 160 /tmp/seed_bw.json)"
    fi
  else
    skip "无 E 码可抽测"
  fi
else
  plan "GET /api/trace/backward?fg_code=<E码> has_flow=true"
fi

# ---------- 7. 行数报告 ----------
log "══ 7. 行数报告 ══"
JXDB="$ROOT/bin/jxdbtool"
DSN="${JX_DB_DSN:-}"
if [ -z "$DSN" ] && [ -f "$ROOT/.env" ]; then
  DSN=$(grep -E '^JX_DB_DSN=' "$ROOT/.env" | head -n1 | sed 's/^JX_DB_DSN=//' | tr -d '\042\047')
fi
if [ -x "$JXDB" ]; then
  if [ -n "$DSN" ]; then
    "$JXDB" -dsn "$DSN" \
      -q 'SELECT "m_customer", COUNT(*) FROM m_customer' \
      -q 'SELECT "m_material", COUNT(*) FROM m_material' \
      -q 'SELECT "m_test_item", COUNT(*) FROM m_test_item' \
      -q 'SELECT "m_test_item_limit", COUNT(*) FROM m_test_item_limit' \
      -q 'SELECT "m_incoming_batch", COUNT(*) FROM m_incoming_batch' \
      -q 'SELECT "b_arrival_notice", COUNT(*) FROM b_arrival_notice' \
      -q 'SELECT "b_truck_lot", COUNT(*) FROM b_truck_lot' \
      -q 'SELECT "b_bag", COUNT(*) FROM b_bag' \
      -q 'SELECT "b_sample", COUNT(*) FROM b_sample' \
      -q 'SELECT "b_sample_group", COUNT(*) FROM b_sample_group' \
      -q 'SELECT "b_inspection", COUNT(*) FROM b_inspection' \
      -q 'SELECT "b_inspection_result", COUNT(*) FROM b_inspection_result' \
      -q 'SELECT "b_production_batch", COUNT(*) FROM b_production_batch' \
      -q 'SELECT "b_feed_record", COUNT(*) FROM b_feed_record' \
      -q 'SELECT "b_fg_lot", COUNT(*) FROM b_fg_lot' \
      -q 'SELECT "b_fg_bag", COUNT(*) FROM b_fg_bag' \
      -q 'SELECT "b_shipment", COUNT(*) FROM b_shipment' \
      2>&1 | awk -F'\t' 'NF>=2 {printf "  %-22s %s\n", $1, $2}'
  else
    log "  （无 JX_DB_DSN，跳过行数报告）"
  fi
else
  log "  （bin/jxdbtool 不在，跳过行数报告）"
fi

echo
echo "════════ 结果：通过 $PASS / 跳过 $SKIP / 失败 $FAIL · mode=$([ $APPLY -eq 1 ] && echo APPLY || echo DRY-RUN) ════════"
[ "$FAIL" -eq 0 ] || exit 1
exit 0
