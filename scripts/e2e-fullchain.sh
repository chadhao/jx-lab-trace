#!/usr/bin/env bash
# e2e-fullchain.sh —— **端到端业务链回归**（收货 → 取样 → 检测 → 生产谱系 → 出货 → 追溯 → 报告 → 报表）
#
# ★ 定位：**联调回归工具**，不是门禁的一部分（它需要**测试服务器上服务在跑**，
#   故不接入 scripts/check_all.sh —— 门禁必须能在无外部依赖时跑）。
#
# ★★ 运行位置（`docs/05` 硬约束）：**在测试服务器上跑**，不在本机。
#   bash scripts/deploy-test-server.sh --restart --smoke      # 先部署并起服务
#   ssh chadhao@192.168.10.50 'bash ~/jx-lab-trace/scripts/e2e-fullchain.sh'
#
# ★ 它做什么：用**真实 HTTP 接口**走一遍完整业务流，逐步断言 http 码，末尾给 通过/失败 计数。
#   ★ 首次运行会自动补一个联调身份 OU_DJ_E2E，绑 receiver+qc+production+**management**+sales
#     （management 是审批链必需，此前缺账号 —— 本脚本顺带把它补上）。
#
# ★ 与单测的分工：`go test` 验"代码级正确"；本脚本验"**装起来能按业务跑通**"（含权限、双进程、
#   报告对外分享）。★ 两者不可互相替代 —— 单测全绿也可能服务起不来；本脚本全绿也不覆盖边界。
#
# ★★ 已知踩坑（首版都撞过，故此处所有细节都是"读实现/读测试夹具"得出的，不是猜的）：
#   ① 物料必须按 **kind**（原料/成品）过滤，**不能按位置取** —— 否则 404「对象不存在：原料物料 #N」；
#   ② 报表名是**连字符**（quality-trend / customer-recon / output-yield / sample-expiry / nonconform-stat），不是蛇形；
#   ③ 加检测项字段是 **item_ids**（不是 item_code）；
#   ④ 反向追溯参数是 **fg_code**（不是 fg_bag_code）；
#   ⑤ 主数据/业务接口**要用业务身份**（sysadmin 连业务读都是 403 —— 系统角色不进权限矩阵）。

set -u
A="http://127.0.0.1:18080"
AD=/tmp/e2e_admin.ck      # sysadmin（只管管理域）
CK=/tmp/e2e_biz.ck        # 业务身份
OU="ou_dj_e2e"
PASS=0; FAIL=0

jq() { python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception as e: print('(非 JSON)'); sys.exit(0)
p='$1'.split('.')
for k in p:
    if k=='': continue
    if isinstance(d,list):
        try: d=d[int(k)]
        except Exception: print(''); sys.exit(0)
    else:
        d=d.get(k) if isinstance(d,dict) else None
    if d is None: print(''); sys.exit(0)
print(d if not isinstance(d,(dict,list)) else json.dumps(d,ensure_ascii=False)[:200])
"; }

step() { # step <名称> <期望码> <方法> <路径> <body|-> <存文件>
  local name="$1" want="$2" m="$3" p="$4" body="$5" out="$6"
  local code
  if [ "$body" = "-" ]; then
    code=$(curl -s -b "$CK" -o "$out" -w '%{http_code}' -X "$m" "$A$p")
  else
    code=$(curl -s -b "$CK" -o "$out" -w '%{http_code}' -X "$m" -H 'Content-Type: application/json' -d "$body" "$A$p")
  fi
  if [ "$code" = "$want" ]; then
    printf '  ✓ %-34s http=%s\n' "$name" "$code"; PASS=$((PASS+1))
  else
    printf '  ✗ %-34s http=%s（期望 %s）\n     body=%s\n' "$name" "$code" "$want" "$(head -c 300 "$out")"; FAIL=$((FAIL+1))
  fi
}

echo "════════ 0. 准备：补 management 缺口 + 绑联调身份 ════════"
curl -s -c "$AD" -X POST "$A/api/auth/dev-login" -o /dev/null
for r in receiver qc production sales management; do
  printf '  绑 %-11s ' "$r"
  curl -s -b "$AD" -X POST "$A/api/admin/user-roles" -H 'Content-Type: application/json' \
    -d "{\"open_id\":\"$OU\",\"role_code\":\"$r\"}" -o /tmp/e2e_b.json -w 'http=%{http_code}\n'
done
echo "  联调身份登入："
curl -s -c "$CK" -X POST "$A/api/auth/dev-login" -H 'Content-Type: application/json' -d "{\"open_id\":\"$OU\"}" -o /tmp/e2e_l.json -w '  http=%{http_code}  '
cat /tmp/e2e_l.json; echo

echo "════════ 1. 取主数据 ID ════════"
curl -s -b "$CK" "$A/api/md/customers" > /tmp/e2e_cust.json
curl -s -b "$CK" "$A/api/md/materials" > /tmp/e2e_mat.json
CUST=$(jq rows.0.id < /tmp/e2e_cust.json)
CUST_CODE=$(jq rows.0.code < /tmp/e2e_cust.json)
# ★ 物料必须按 kind 选（原料/成品）—— 按位置取是错的（实测踩过：把成品当原料 ⇒ 404「对象不存在：原料物料」）
INMAT=$(python3 -c "
import json,io
d=json.load(io.open('/tmp/e2e_mat.json',encoding='utf-8'))
print(next((r['id'] for r in d['rows'] if r.get('kind')=='原料'),''))")
OUTMAT=$(python3 -c "
import json,io
d=json.load(io.open('/tmp/e2e_mat.json',encoding='utf-8'))
print(next((r['id'] for r in d['rows'] if r.get('kind')=='成品'),''))")
curl -s -b "$CK" "$A/api/md/test-items" > /tmp/e2e_ti.json
TITEM=$(jq rows.0.id < /tmp/e2e_ti.json)
echo "  客户 id=$CUST(code=$CUST_CODE)  原料 id=$INMAT  成品 id=$OUTMAT  检测项 id=$TITEM"
[ -n "$CUST" ] && [ -n "$INMAT" ] && [ -n "$OUTMAT" ] && [ -n "$TITEM" ] || { echo "  ✗ 主数据不足（客户/原料/成品/检测项），中止"; exit 1; }

echo "════════ 2. 收货与打码（M3）════════"
step "预报登记" 200 POST /api/recv/notices \
  "{\"customer_id\":$CUST,\"material_id\":$INMAT,\"biz_type\":\"CG\",\"arrive_date\":\"2026-10-09\",\"plate_no\":\"湘F·DJ01\",\"driver\":\"联调司机\",\"est_bag_count\":2}" /tmp/e2e_n.json
NID=$(jq row.id < /tmp/e2e_n.json)
step "到货确认" 200 POST /api/recv/arrivals "{\"notice_id\":$NID}" /tmp/e2e_t.json
TID=$(jq row.id < /tmp/e2e_t.json)
echo "      → 车次 id=$TID 车码=$(jq row.code < /tmp/e2e_t.json)"
step "过磅（35t/5t）" 200 POST "/api/recv/trucks/$TID/weigh" '{"gross_weight":35000,"tare_weight":5000}' /tmp/e2e_w.json
step "生成 2 个袋码" 200 POST "/api/recv/trucks/$TID/bags" '{"count":2}' /tmp/e2e_bags.json
BAG1=$(jq rows.0.code < /tmp/e2e_bags.json)
echo "      → 袋码1=$BAG1"

echo "════════ 3. 取样与留样（M4）════════"
step "扫码取样（生成份样+保留样）" 200 POST /api/sample/take "{\"code\":\"$BAG1\"}" /tmp/e2e_s.json
SID=$(jq samples.0.id < /tmp/e2e_s.json)
echo "      → 份样 id=$SID  样品组=$(jq samples.0.role < /tmp/e2e_s.json)"
step "建取样组（并入份样）" 200 POST /api/sample/groups \
  "{\"target_type\":\"车次\",\"target_id\":$TID,\"sample_ids\":[$SID],\"remark\":\"联调\"}" /tmp/e2e_g.json

echo "════════ 4. 检测（M5）════════"
step "建检测单" 200 POST /api/insp "{\"target_type\":\"车次\",\"target_id\":$TID}" /tmp/e2e_i.json
IID=$(jq row.id < /tmp/e2e_i.json)
step "加检测项" 200 POST "/api/insp/$IID/items" "{\"item_ids\":[$TITEM]}" /tmp/e2e_it.json
step "出结论=合格" 200 POST "/api/insp/$IID/conclusion" '{"conclusion":"合格"}' /tmp/e2e_cc.json

echo "════════ 5. 生产与谱系（M6）════════"
step "建生产批" 200 POST /api/prod/batches \
  "{\"customer_id\":$CUST,\"input_material_id\":$INMAT,\"planned_output_material_id\":$OUTMAT,\"batch_date\":\"2026-10-09\",\"remark\":\"联调批\"}" /tmp/e2e_pb.json
PBID=$(jq row.id < /tmp/e2e_pb.json)
echo "      → 生产批 id=$PBID 码=$(jq row.code < /tmp/e2e_pb.json)"
step "★ 投料扫码（谱系承重墙）" 200 POST "/api/prod/batches/$PBID/feeds" \
  "{\"bag_code\":\"$BAG1\",\"feed_weight\":1.25,\"operator\":\"联调投料工\"}" /tmp/e2e_feed.json
step "记作业段" 200 POST "/api/prod/batches/$PBID/operations" \
  '{"operator":"联调班组","start_at":"2026-10-09 08:00:00","end_at":"2026-10-09 12:00:00","output_weight":1.5}' /tmp/e2e_op.json
step "生成成品批" 200 POST "/api/prod/batches/$PBID/fg-lots" '{"pack_spec":"吨袋","net_weight":20}' /tmp/e2e_lot.json
LOTID=$(jq row.id < /tmp/e2e_lot.json)
step "生成 2 个成品袋" 200 POST "/api/prod/fg-lots/$LOTID/bags" '{"count":2}' /tmp/e2e_fgb.json
FGB1=$(jq rows.0.code < /tmp/e2e_fgb.json)
FGLOTCODE=$(jq row.code < /tmp/e2e_lot.json)
echo "      → 成品批码=$(jq row.code < /tmp/e2e_lot.json)  成品袋1=$FGB1"

echo "════════ 6. 出货（M7）════════"
step "逐袋归集建出货单" 200 POST /api/ship/shipments \
  "{\"bag_codes\":[\"$FGB1\"],\"remark\":\"联调出货\"}" /tmp/e2e_sh.json
SHID=$(jq row.id < /tmp/e2e_sh.json)
step "出场登记" 200 POST "/api/ship/shipments/$SHID/depart" '{"plate_no":"湘A·DJ88","driver":"联调司机"}' /tmp/e2e_dp.json

echo "════════ 7. 追溯（M8）════════"
step "★ 正向：车次 → 成品" 200 GET "/api/trace/forward?truck_lot_id=$TID" - /tmp/e2e_fw.json
echo "      has_flow=$(jq has_flow < /tmp/e2e_fw.json)"
step "反向：成品袋 → 来源车次" 200 GET "/api/trace/backward?fg_code=$FGLOTCODE" - /tmp/e2e_bw.json

echo "════════ 8. 报告分享（M9）════════"
step "生成报告快照" 200 POST /api/report/generate \
  "{\"scope_type\":\"按批次\",\"scope\":{\"batch_id\":$PBID},\"title\":\"联调检验报告\"}" /tmp/e2e_rp.json
echo "      → report_no=$(jq row.report_no < /tmp/e2e_rp.json)  token=$(jq row.token < /tmp/e2e_rp.json | cut -c1-12)…  url=$(jq row.url < /tmp/e2e_rp.json)"

echo "════════ 9. 报表（M10）════════"
step "读报表（客户对账）" 200 GET "/api/rpt/customer-recon?customer_id=$CUST" - /tmp/e2e_rpt.json
echo "      has_data=$(jq has_data < /tmp/e2e_rpt.json)"

echo "════════ 10. 权限对照（应被拒）════════"
curl -s -c /tmp/e2e_adm2.ck -X POST "$A/api/auth/dev-login" -o /dev/null
printf '  sysadmin 读业务数据 ⇒ '; curl -s -b /tmp/e2e_adm2.ck -o /dev/null -w 'http=%{http_code}（期望非 200）\n' "$A/api/md/customers"

echo
echo "════════ 结果：通过 $PASS / 失败 $FAIL ════════"
