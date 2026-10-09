<script setup>
// 追溯页（M8 · D7）：正向查询（车次 / 吨袋码）· 反向查询（成品批码 / 成品批 id）
// · 批次档案页（一页汇总全链，让步接收醒目标注）。
// ★ 动作按 /api/ship/perm-summary 显隐（含 3 个 trace.* 读入口）；
// ★ 未命中 ≠ 报错：后端返回 200 + has_flow:false ⇒ 页面明确显示「无流向」（不是 0 条静默）。
import { ref, onMounted } from 'vue'
import { api } from '../api.js'

defineProps({ me: { type: Object, default: null } })

const err = ref('')
const perms = ref({})
const canRead = (code) => {
  const v = perms.value[code]
  return !!v && (v.includes('READ') || v.includes('ALL'))
}

async function loadPerms() {
  try {
    const data = await api.get('/api/ship/perm-summary')
    perms.value = data.points || {}
  } catch (e) {
    err.value = '加载权限摘要失败：' + e.message
  }
}

// ================= 正向追溯 =================
const fwd = ref({ truck_lot_id: '', bag_code: '' })
const fwdLoading = ref(false)
const fwdResult = ref(null) // null=未查 / { has_flow, ... }

async function doForward() {
  err.value = ''
  fwdResult.value = null
  const q = []
  if (fwd.value.truck_lot_id) q.push('truck_lot_id=' + encodeURIComponent(fwd.value.truck_lot_id))
  if (fwd.value.bag_code) q.push('bag_code=' + encodeURIComponent(fwd.value.bag_code))
  if (!q.length) {
    err.value = '请填车次 id 或吨袋码（至少一个）'
    return
  }
  fwdLoading.value = true
  try {
    fwdResult.value = await api.get('/api/trace/forward?' + q.join('&'))
  } catch (e) {
    err.value = e.message
  } finally {
    fwdLoading.value = false
  }
}

// ================= 反向追溯 =================
const bwd = ref({ fg_lot_id: '', fg_code: '' })
const bwdLoading = ref(false)
const bwdResult = ref(null)

async function doBackward() {
  err.value = ''
  bwdResult.value = null
  const q = []
  if (bwd.value.fg_lot_id) q.push('fg_lot_id=' + encodeURIComponent(bwd.value.fg_lot_id))
  if (bwd.value.fg_code) q.push('fg_code=' + encodeURIComponent(bwd.value.fg_code))
  if (!q.length) {
    err.value = '请填成品批 id 或成品批码（至少一个）'
    return
  }
  bwdLoading.value = true
  try {
    bwdResult.value = await api.get('/api/trace/backward?' + q.join('&'))
  } catch (e) {
    err.value = e.message
  } finally {
    bwdLoading.value = false
  }
}

// ================= 批次档案 =================
const archQuery = ref('')
const archLoading = ref(false)
const arch = ref(null)

async function doArchive() {
  err.value = ''
  arch.value = null
  const id = (archQuery.value || '').trim()
  if (!id) {
    err.value = '请填生产批 id'
    return
  }
  archLoading.value = true
  try {
    arch.value = await api.get('/api/trace/batch/' + encodeURIComponent(id))
  } catch (e) {
    err.value = e.message
  } finally {
    archLoading.value = false
  }
}

function openArchive(batchId) {
  archQuery.value = String(batchId)
  doArchive()
}

onMounted(loadPerms)
</script>

<template>
  <section>
    <h2>追溯（正向 · 反向 · 批次档案）</h2>
    <p class="hint">
      ★ 正向：车次 / 吨袋 → 生产批 → 成品批 → 出货单；反向：成品批 → 生产批 → 投料吨袋 / 车次（含现行检测结果）；
      ★ 两向同走 <code>b_feed_record</code>，互为逆；★ 未命中显示「<b>无流向</b>」，不是报错、不是 0 条静默。
    </p>
    <p v-if="err" class="err">{{ err }}</p>

    <!-- ============ 正向追溯 ============ -->
    <div class="panel">
      <div class="panel-head"><b>正向追溯（车 → 成品）</b></div>
      <div class="toolbar">
        <label>车次 id <input v-model="fwd.truck_lot_id" placeholder="如 12" style="width: 8rem"></label>
        <label>或 吨袋码 <input v-model="fwd.bag_code" placeholder="B 类码（裸串或人读行）" style="min-width: 22rem"></label>
        <button v-if="canRead('trace.forward')" :disabled="fwdLoading" @click="doForward">
          {{ fwdLoading ? '查询中…' : '正向追溯' }}
        </button>
      </div>
      <p v-if="fwdResult && !fwdResult.has_flow" class="err">
        ★★ 无流向：该车 / 该袋未进入投料谱系（查询成功，结果为空 —— 不是报错）。
      </p>
      <template v-if="fwdResult && fwdResult.has_flow">
        <div class="panel">
          <div class="panel-head">
            <b>来源</b>
            <span class="hint" v-if="fwdResult.source">
              车次 {{ fwdResult.source.truck_code || '#' + fwdResult.source.truck_lot_id }}
              <template v-if="fwdResult.source.customer"> · {{ fwdResult.source.customer }}</template>
              <template v-if="fwdResult.source.material"> · {{ fwdResult.source.material }}</template>
              <template v-if="fwdResult.source.status"> · {{ fwdResult.source.status }}</template>
            </span>
          </div>
        </div>
        <table v-if="fwdResult.batches && fwdResult.batches.length">
          <thead><tr><th>生产批</th><th>人读行</th><th>链根日期</th><th>状态</th><th></th></tr></thead>
          <tbody>
            <tr v-for="b in fwdResult.batches" :key="b.batch_id">
              <td class="mono">{{ b.code }}</td>
              <td class="mono">{{ b.human }}</td>
              <td>{{ b.batch_date }}</td>
              <td><span class="tag">{{ b.status }}</span></td>
              <td class="ops"><button class="ghost" @click="openArchive(b.batch_id)">批次档案</button></td>
            </tr>
          </tbody>
        </table>
        <table v-if="fwdResult.fg_lots && fwdResult.fg_lots.length">
          <thead><tr><th>成品批</th><th>人读行</th><th>产出物料</th><th>净重(吨)</th><th>状态</th></tr></thead>
          <tbody>
            <tr v-for="f in fwdResult.fg_lots" :key="f.fg_lot_id">
              <td class="mono">{{ f.code }}</td>
              <td class="mono">{{ f.human }}</td>
              <td>{{ f.output_material || '—' }}</td>
              <td>{{ f.net_weight ?? '—' }}</td>
              <td><span class="tag">{{ f.status }}</span></td>
            </tr>
          </tbody>
        </table>
        <table v-if="fwdResult.shipments && fwdResult.shipments.length">
          <thead><tr><th>出货单</th><th>状态</th><th>出场时间</th><th>车牌</th><th>客户</th></tr></thead>
          <tbody>
            <tr v-for="s in fwdResult.shipments" :key="s.shipment_id">
              <td class="mono">{{ s.shipment_no }}</td>
              <td><span class="tag">{{ s.status }}</span></td>
              <td>{{ s.ship_at || '（未登记）' }}</td>
              <td>{{ s.plate_no || '—' }}</td>
              <td>{{ s.customer || '—' }}</td>
            </tr>
          </tbody>
        </table>
      </template>
    </div>

    <!-- ============ 反向追溯 ============ -->
    <div class="panel">
      <div class="panel-head"><b>反向追溯（成品 → 车）</b></div>
      <div class="toolbar">
        <label>成品批 id <input v-model="bwd.fg_lot_id" placeholder="如 5" style="width: 8rem"></label>
        <label>或 成品批码 <input v-model="bwd.fg_code" placeholder="D 类码（裸串或人读行）" style="min-width: 22rem"></label>
        <button v-if="canRead('trace.backward')" :disabled="bwdLoading" @click="doBackward">
          {{ bwdLoading ? '查询中…' : '反向追溯' }}
        </button>
      </div>
      <p v-if="bwdResult && !bwdResult.has_flow" class="err">
        ★★ 无流向：该成品批没有投料来源记录（查询成功，结果为空）。
      </p>
      <template v-if="bwdResult && bwdResult.has_flow">
        <table v-if="bwdResult.batches && bwdResult.batches.length">
          <thead><tr><th>来源生产批</th><th>人读行</th><th>链根日期</th><th>状态</th></tr></thead>
          <tbody>
            <tr v-for="b in bwdResult.batches" :key="b.batch_id">
              <td class="mono">{{ b.code }}</td>
              <td class="mono">{{ b.human }}</td>
              <td>{{ b.batch_date }}</td>
              <td><span class="tag">{{ b.status }}</span></td>
            </tr>
          </tbody>
        </table>
        <table v-if="bwdResult.feeds && bwdResult.feeds.length">
          <thead>
            <tr>
              <th>投料吨袋</th><th>车次</th><th>客户</th><th>投料量</th><th>投料时间</th>
              <th>当时检测结果（现行单）</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(f, i) in bwdResult.feeds" :key="i">
              <td class="mono">{{ f.bag_human || f.bag_code }}</td>
              <td class="mono">{{ f.truck_code || '#' + f.truck_lot_id }}</td>
              <td>{{ f.customer || '—' }}</td>
              <td>{{ f.feed_weight ?? '—' }}</td>
              <td>{{ f.fed_at || '—' }}</td>
              <td>
                <template v-if="f.inspection">
                  <span class="tag">{{ f.inspection.conclusion || '（无结论）' }}</span>
                  <span v-if="f.inspection.inspection_no" class="mono">{{ f.inspection.inspection_no }}</span>
                  <span v-if="f.inspection.is_current" class="hint">（现行单，非历史快照）</span>
                  <span v-if="f.inspection.urgent_release" class="err">＋紧急放行生效</span>
                  <ul v-if="f.inspection.results && f.inspection.results.length" style="margin:0.2rem 0 0;padding-left:1.2rem">
                    <li v-for="(r, j) in f.inspection.results" :key="j" class="hint">
                      {{ r.item_name || '—' }}：{{ r.value_num ?? r.value_text ?? '—' }}{{ r.unit || '' }}
                      （{{ r.judge || r.state }}）
                    </li>
                  </ul>
                </template>
                <span v-else class="hint">无检测记录</span>
              </td>
            </tr>
          </tbody>
        </table>
      </template>
    </div>

    <!-- ============ 批次档案 ============ -->
    <div class="panel">
      <div class="panel-head"><b>批次档案（一页汇总全链 · 内部档案）</b></div>
      <div class="toolbar">
        <label>生产批 id <input v-model="archQuery" placeholder="如 3" style="width: 8rem"></label>
        <button v-if="canRead('trace.batch.view')" :disabled="archLoading" @click="doArchive">
          {{ archLoading ? '读取中…' : '读档案' }}
        </button>
      </div>

      <template v-if="arch">
        <!-- ★ 让步接收醒目标注（D20 内部显著标注；对外不披露属 M9） -->
        <p v-if="arch.concession_used" class="err" style="font-weight:700">
          ⚠ 让步接收：该批投料链上使用了让步接收的原料（
          <span v-for="(c, i) in arch.concession_sources" :key="i">
            {{ c.truck_code || '#' + c.truck_lot_id }} {{ c.inspection_no }}{{ i < arch.concession_sources.length - 1 ? '、' : '' }}
          </span>）
        </p>

        <div class="panel">
          <div class="panel-head">
            <b class="mono">{{ arch.batch.human || arch.batch.code }}</b>
            <span class="tag">{{ arch.batch.status }}</span>
            <span class="hint">
              链根日期 {{ arch.batch.batch_date }}
              · 客户 {{ arch.customer_name || '—' }}
              · 投入 {{ arch.input_material || '—' }}
              · 计划产出 {{ arch.planned_output_material || '—' }}
            </span>
          </div>
        </div>

        <table v-if="arch.feeds && arch.feeds.length">
          <thead><tr><th>投料吨袋</th><th>车次</th><th>投料量</th><th>时间</th><th>操作人</th></tr></thead>
          <tbody>
            <tr v-for="f in arch.feeds" :key="f.id">
              <td class="mono">{{ f.bag_human || f.bag_code }}</td>
              <td class="mono">{{ f.truck_code || '—' }}</td>
              <td>{{ f.feed_weight ?? '—' }}</td>
              <td>{{ f.fed_at || '—' }}</td>
              <td>{{ f.operator || '—' }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">无投料明细。</p>

        <table v-if="arch.operations && arch.operations.length">
          <thead><tr><th>段序</th><th>班组</th><th>操作人</th><th>开始</th><th>结束</th><th>本段产出(吨)</th></tr></thead>
          <tbody>
            <tr v-for="o in arch.operations" :key="o.id">
              <td>{{ o.seq }}</td>
              <td>{{ o.team_id ?? '—' }}</td>
              <td>{{ o.operator || '—' }}</td>
              <td>{{ o.start_at || '—' }}</td>
              <td>{{ o.end_at || '—' }}</td>
              <td>{{ o.output_weight ?? '—' }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">无作业段。</p>

        <template v-for="fg in arch.fg_lots" :key="'fg-' + fg.fg_lot.id">
          <p class="hint">成品批 <span class="mono">{{ fg.fg_lot.human || fg.fg_lot.code }}</span>
            · {{ fg.fg_lot.status }} · 袋数 {{ (fg.bags || []).length }}</p>
          <table v-if="fg.bags && fg.bags.length">
            <thead><tr><th>袋序</th><th>成品袋码</th><th>状态</th></tr></thead>
            <tbody>
              <tr v-for="b in fg.bags" :key="b.id">
                <td>{{ b.bag_seq }}</td>
                <td class="mono">{{ b.human || b.code }}</td>
                <td>{{ b.status }}</td>
              </tr>
            </tbody>
          </table>
        </template>

        <table v-if="arch.shipments && arch.shipments.length">
          <thead><tr><th>出货单</th><th>状态</th><th>出场时间</th><th>车牌</th></tr></thead>
          <tbody>
            <tr v-for="s in arch.shipments" :key="s.shipment_id">
              <td class="mono">{{ s.shipment_no }}</td>
              <td><span class="tag">{{ s.status }}</span></td>
              <td>{{ s.ship_at || '（未登记）' }}</td>
              <td>{{ s.plate_no || '—' }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">尚无出货记录。</p>
      </template>
    </div>
  </section>
</template>
