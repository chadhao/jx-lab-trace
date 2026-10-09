<script setup>
// 收货页（M3）：预报登记与车序 · 到货确认 · 过磅与袋码批量生成 · 标签打印/补打 · 袋作废与退车。
// ★ 主数据下拉只取**启用**项（复用批 2 的 is_current / status 口径）。
// ★ 标签版式页由服务端生成，这里只负责打开浏览器打印窗口（一期不写打印驱动）。
import { ref, onMounted } from 'vue'
import { api } from '../api.js'

defineProps({ me: { type: Object, default: null } })

const TABS = [
  { k: 'notice', n: '预报与到货' },
  { k: 'truck', n: '过磅与袋码' },
  { k: 'label', n: '标签打印' },
  { k: 'void', n: '作废与退车' },
]
const tab = ref('notice')
const msg = ref('')
const err = ref('')

// ---- 主数据下拉（仅启用项） ----
const customers = ref([])
const materials = ref([])
const vehicles = ref([])

async function loadMaster() {
  try {
    const [c, m, v] = await Promise.all([
      api.get('/api/md/customers?status=' + encodeURIComponent('启用') + '&limit=500'),
      api.get('/api/md/materials?status=' + encodeURIComponent('启用') + '&limit=500'),
      api.get('/api/md/vehicles?status=' + encodeURIComponent('启用') + '&limit=500'),
    ])
    customers.value = (c.rows || []).filter((r) => Number(r.is_current) === 1)
    materials.value = (m.rows || []).filter((r) => r.kind === '原料' && Number(r.is_current) === 1)
    vehicles.value = (v.rows || []).filter((r) => Number(r.is_current) === 1)
  } catch (e) {
    err.value = '加载主数据下拉失败：' + e.message
  }
}

function today() {
  const d = new Date()
  const p = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

function label(row) {
  return `${row.code || ''} ${row.name || row.plate_no || ''}`.trim()
}

// ---- 预报 ----
const notices = ref([])
const noticeForm = ref(null) // null = 关闭；{ id?, ... }
const noticeErr = ref('')

function emptyNotice() {
  return {
    biz_type: 'CG',
    customer_id: '',
    material_id: '',
    vehicle_id: '',
    plate_no: '',
    driver: '',
    phone: '',
    eta: '',
    est_bag_count: '',
    arrive_date: today(),
    reason: '',
  }
}

async function loadNotices() {
  err.value = ''
  try {
    const data = await api.get('/api/recv/notices')
    notices.value = data.rows || []
  } catch (e) {
    notices.value = []
    err.value = e.message
  }
}

function openNotice(n) {
  noticeErr.value = ''
  if (n) {
    noticeForm.value = {
      id: n.id,
      biz_type: n.biz_type,
      customer_id: n.customer_id || '',
      material_id: n.material_id || '',
      vehicle_id: n.vehicle_id || '',
      plate_no: n.plate_no || '',
      driver: n.driver || '',
      phone: n.phone || '',
      eta: n.eta ? new Date(n.eta).toISOString().slice(0, 16) : '',
      est_bag_count: n.est_bag_count || '',
      arrive_date: n.arrive_date,
      reason: '',
    }
  } else {
    noticeForm.value = emptyNotice()
  }
}

async function saveNotice() {
  noticeErr.value = ''
  const f = noticeForm.value
  if (!f.material_id) { noticeErr.value = '必须选择原料物料'; return }
  if (f.biz_type === 'CG' && !f.customer_id) { noticeErr.value = '客供必须选择客户'; return }
  const payload = {
    biz_type: f.biz_type,
    customer_id: f.biz_type === 'ZG' ? 0 : Number(f.customer_id || 0),
    material_id: Number(f.material_id),
    vehicle_id: Number(f.vehicle_id || 0),
    plate_no: f.plate_no,
    driver: f.driver,
    phone: f.phone,
    est_bag_count: Number(f.est_bag_count || 0),
    arrive_date: f.arrive_date,
  }
  if (f.eta) payload.eta = new Date(f.eta).toISOString()
  try {
    if (f.id) {
      if (!f.reason) { noticeErr.value = '修改预报必须填原因（且车序不变）'; return }
      await api.put(`/api/recv/notices/${f.id}`, Object.assign({ reason: f.reason }, payload))
      msg.value = '预报已更新（车序保持不变）'
    } else {
      const data = await api.post('/api/recv/notices', payload)
      msg.value = `预报已登记，分配车序 ${String(data.row.seq_no).padStart(2, '0')}`
    }
    noticeForm.value = null
    await loadNotices()
  } catch (e) {
    noticeErr.value = e.message
  }
}

async function cancelNotice(n, status) {
  const reason = window.prompt(status === '空号' ? '登记空号（车未到，序号作废不回收）原因：' : '取消预报原因：')
  if (!reason) return
  try {
    await api.patch(`/api/recv/notices/${n.id}/status`, { status, reason })
    msg.value = `${n.notice_no} 已置为「${status}」（车序 ${n.seq_no} 不回收）`
    await loadNotices()
  } catch (e) {
    err.value = e.message
  }
}

async function confirmArrival(n) {
  try {
    const data = await api.post('/api/recv/arrivals', { notice_id: n.id })
    msg.value = `到货确认成功：车码 ${data.row.human || data.row.code}`
    await loadNotices()
    await loadTrucks()
  } catch (e) {
    err.value = e.message
  }
}

// ---- 车次：过磅与袋码 ----
const trucks = ref([])
const truckId = ref('')
const bags = ref([])
const weighForm = ref({ gross_weight: '', tare_weight: '' })
const bagCount = ref('')
const truckErr = ref('')

async function loadTrucks() {
  try {
    const data = await api.get('/api/recv/trucks')
    trucks.value = data.rows || []
  } catch (e) {
    trucks.value = []
    if (!err.value) err.value = e.message
  }
}

const curTruck = ref(null)

async function pickTruck(id) {
  truckId.value = id
  truckErr.value = ''
  curTruck.value = trucks.value.find((t) => Number(t.id) === Number(id)) || null
  bags.value = []
  bagCount.value = ''
  if (!id) return
  try {
    const data = await api.get(`/api/recv/trucks/${id}/bags`)
    bags.value = data.rows || []
  } catch (e) {
    truckErr.value = e.message
  }
}

async function submitWeigh() {
  truckErr.value = ''
  try {
    const data = await api.post(`/api/recv/trucks/${truckId.value}/weigh`, {
      gross_weight: Number(weighForm.value.gross_weight),
      tare_weight: Number(weighForm.value.tare_weight),
    })
    msg.value = `过磅完成：净重 ${data.row.net_weight} 吨`
    await loadTrucks()
    await pickTruck(truckId.value)
  } catch (e) {
    truckErr.value = e.message
  }
}

async function genBags() {
  truckErr.value = ''
  try {
    const data = await api.post(`/api/recv/trucks/${truckId.value}/bags`, {
      count: Number(bagCount.value),
    })
    msg.value = `已按实际袋数生成 ${data.generated} 个袋码（预报袋数不预打）`
    await loadTrucks()
    await pickTruck(truckId.value)
  } catch (e) {
    truckErr.value = e.message
  }
}

// ---- 标签打印 ----
const pickedCodes = ref([])
const reprintReason = ref('')

function toggleCode(code) {
  const i = pickedCodes.value.indexOf(code)
  if (i >= 0) pickedCodes.value.splice(i, 1)
  else pickedCodes.value.push(code)
}

function pickAll(validBags) {
  pickedCodes.value = validBags.filter((b) => b.status !== '作废').map((b) => b.code)
}

// 打开一个空白窗口再跳转：避免 fetch 之后 window.open 被浏览器当成弹窗拦截。
function openPrintWindow(pageUrl) {
  const win = window.open('about:blank', '_blank')
  if (win) win.location = pageUrl
  else window.open(pageUrl, '_blank')
}

async function printLabels(isReprint) {
  truckErr.value = ''
  if (!pickedCodes.value.length) { truckErr.value = '请先勾选要打印的标签'; return }
  const body = { codes: pickedCodes.value }
  if (isReprint) {
    if (!reprintReason.value) { truckErr.value = '补打必须填原因（否则会出现「一物两码」）'; return }
    body.reason = reprintReason.value
  }
  try {
    const data = isReprint
      ? await api.post('/api/recv/labels/reprint', body)
      : await api.post('/api/recv/labels/print', body)
    msg.value = isReprint ? `补打 ${data.count} 张（已留痕）` : `已生成 ${data.count} 张标签版式页`
    if (data.page_url) openPrintWindow(data.page_url)
  } catch (e) {
    truckErr.value = e.message
  }
}

// ---- 作废与退车 ----
const voidErr = ref('')

async function voidBag(bag) {
  const reason = window.prompt(`作废袋码 ${bag.human || bag.code} 的原因（作废码永久不重用）：`)
  if (!reason) return
  voidErr.value = ''
  try {
    await api.post(`/api/recv/bags/${bag.id}/void`, { reason })
    msg.value = '袋已作废（bag_count 按有效袋重算）'
    await pickTruck(truckId.value)
    await loadTrucks()
  } catch (e) {
    voidErr.value = e.message
  }
}

async function returnTruck() {
  if (!curTruck.value) return
  const reason = window.prompt('退车原因（须已有质检「退货」处置判定）：')
  if (reason === null) return
  voidErr.value = ''
  try {
    const data = await api.post(`/api/recv/trucks/${curTruck.value.id}/return`, { reason })
    msg.value = `车次已转「${data.row.status}」，该车全部袋码已作废`
    await loadTrucks()
    await pickTruck(truckId.value)
  } catch (e) {
    voidErr.value = e.message
  }
}

onMounted(() => {
  loadMaster()
  loadNotices()
  loadTrucks()
})
</script>

<template>
  <section>
    <h2>收货与打码（M3）</h2>
    <p class="hint">
      预报分配<b>车序</b> → 到货确认生成<b>车码 A</b> → 过磅后按<b>实际袋数</b>批量生成<b>袋码 B</b> → 打印标签。
      跳号不回收、超限不进位、补打必填原因、已取样/已投料的袋不许作废。
    </p>

    <div class="tabs">
      <button v-for="t in TABS" :key="t.k" :class="{ on: tab === t.k }" @click="tab = t.k">{{ t.n }}</button>
    </div>

    <p v-if="msg" class="ok">{{ msg }}</p>
    <p v-if="err" class="err">{{ err }}</p>

    <!-- ===== 预报与到货 ===== -->
    <template v-if="tab === 'notice'">
      <div class="toolbar">
        <button @click="openNotice(null)">登记预报</button>
        <button class="ghost" @click="loadNotices">刷新</button>
        <span class="hint">共 {{ notices.length }} 条</span>
      </div>

      <div v-if="noticeForm" class="panel">
        <div class="panel-head">
          <b>{{ noticeForm.id ? '修改预报（车序不变）' : '登记预报（系统分配车序）' }}</b>
          <button class="ghost" @click="noticeForm = null">取消</button>
        </div>
        <div class="form">
          <label>
            <span>业务类型</span>
            <select v-model="noticeForm.biz_type">
              <option value="CG">CG 客供（受托加工）</option>
              <option value="ZG">ZG 自购（客户段填 0000）</option>
            </select>
          </label>
          <label v-if="noticeForm.biz_type === 'CG'">
            <span>客户<em>*</em></span>
            <select v-model="noticeForm.customer_id">
              <option value="">请选择</option>
              <option v-for="c in customers" :key="c.id" :value="c.id">{{ label(c) }}</option>
            </select>
          </label>
          <label>
            <span>原料物料<em>*</em></span>
            <select v-model="noticeForm.material_id">
              <option value="">请选择</option>
              <option v-for="m in materials" :key="m.id" :value="m.id">{{ label(m) }}</option>
            </select>
          </label>
          <label>
            <span>车辆（可空）</span>
            <select v-model="noticeForm.vehicle_id">
              <option value="">未登记车辆</option>
              <option v-for="v in vehicles" :key="v.id" :value="v.id">{{ label(v) }}</option>
            </select>
          </label>
          <label><span>车牌号</span><input v-model="noticeForm.plate_no" placeholder="湘F·xxxxx" /></label>
          <label><span>司机</span><input v-model="noticeForm.driver" /></label>
          <label><span>司机电话</span><input v-model="noticeForm.phone" /></label>
          <label><span>预计到货时间</span><input v-model="noticeForm.eta" type="datetime-local" /></label>
          <label><span>预计袋数（仅预报用，不据此打码）</span><input v-model="noticeForm.est_bag_count" type="number" min="0" /></label>
          <label>
            <span>到货日（车序空间的链根日期，登记后不可改）</span>
            <input v-model="noticeForm.arrive_date" type="date" :disabled="!!noticeForm.id" />
          </label>
          <label v-if="noticeForm.id" class="full">
            <span>修改原因<em>*</em></span>
            <input v-model="noticeForm.reason" placeholder="必填：落审计；车序保持不动" />
          </label>
          <p v-if="noticeErr" class="err full">{{ noticeErr }}</p>
          <div class="full">
            <button @click="saveNotice">保存</button>
            <button class="ghost" @click="noticeForm = null">取消</button>
          </div>
        </div>
      </div>

      <table>
        <thead>
          <tr>
            <th>车序</th><th>预报单号</th><th>客户</th><th>物料</th><th>车牌</th><th>司机</th>
            <th>电话</th><th>预计到货</th><th>预计袋数</th><th>状态</th><th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="n in notices" :key="n.id">
            <td>{{ String(n.seq_no).padStart(2, '0') }}</td>
            <td>{{ n.notice_no }}</td>
            <td>{{ n.customer_code ? n.customer_code + ' ' + n.customer_name : '—' }}</td>
            <td>{{ n.material_code + ' ' + n.material_name }}</td>
            <td>{{ n.plate_no || '—' }}</td>
            <td>{{ n.driver || '—' }}</td>
            <td>{{ n.phone || '—' }}</td>
            <td>{{ n.eta ? new Date(n.eta).toLocaleString() : '—' }}</td>
            <td>{{ n.est_bag_count }}</td>
            <td>{{ n.status }}</td>
            <td class="ops">
              <template v-if="n.status === '预报'">
                <button @click="confirmArrival(n)">到货确认</button>
                <button class="ghost" @click="openNotice(n)">修改</button>
                <button class="ghost" @click="cancelNotice(n, '已取消')">取消</button>
                <button class="ghost" @click="cancelNotice(n, '空号')">空号</button>
              </template>
              <span v-else class="hint">—</span>
            </td>
          </tr>
          <tr v-if="!notices.length">
            <td colspan="11" class="empty">（暂无预报）</td>
          </tr>
        </tbody>
      </table>
    </template>

    <!-- ===== 过磅与袋码 ===== -->
    <template v-else-if="tab === 'truck'">
      <div class="toolbar">
        <span>选择车次：</span>
        <select :value="truckId" @change="pickTruck($event.target.value)">
          <option value="">— 请选择 —</option>
          <option v-for="t in trucks" :key="t.id" :value="t.id">
            {{ String(t.seq_no).padStart(2, '0') }} · {{ t.human || t.code }} · {{ t.plate_no || '' }} · {{ t.status }}
          </option>
        </select>
        <button class="ghost" @click="loadTrucks">刷新</button>
      </div>

      <div v-if="curTruck" class="panel">
        <div class="panel-head">
          <b>车次 {{ curTruck.human || curTruck.code }}</b>
          <span class="hint">
            {{ curTruck.customer_name }} / {{ curTruck.material_name }} ·
            净重 {{ curTruck.net_weight === null ? '未过磅' : curTruck.net_weight + ' t' }} ·
            有效袋数 {{ curTruck.bag_count }} · {{ curTruck.status }}
          </span>
        </div>

        <div class="form">
          <label><span>毛重（吨）</span><input v-model="weighForm.gross_weight" type="number" step="0.001" min="0" :disabled="bags.length > 0" /></label>
          <label><span>皮重（吨）</span><input v-model="weighForm.tare_weight" type="number" step="0.001" min="0" :disabled="bags.length > 0" /></label>
          <div class="full">
            <button :disabled="bags.length > 0" @click="submitWeigh">过磅确认（净重 = 毛 − 皮）</button>
          </div>
          <label><span>实际袋数 N（只在过磅确认后生成）</span><input v-model="bagCount" type="number" min="1" max="999" :disabled="bags.length > 0" /></label>
          <div class="full">
            <button :disabled="bags.length > 0" @click="genBags">批量生成 N 个袋码</button>
            <span class="hint"> 预报袋数不预打；已生成后不可重复生成（录错走作废）</span>
          </div>
          <p v-if="truckErr" class="err full">{{ truckErr }}</p>
        </div>
      </div>

      <table v-if="bags.length">
        <thead>
          <tr><th>袋序</th><th>袋码（人读行）</th><th>裸串</th><th>摊算袋重(t)</th><th>摊算标记</th><th>状态</th></tr>
        </thead>
        <tbody>
          <tr v-for="b in bags" :key="b.id">
            <td>{{ b.bag_seq }}</td>
            <td>{{ b.human }}</td>
            <td><code>{{ b.code }}</code></td>
            <td>{{ b.weight_allocated }}</td>
            <td>{{ b.weight_is_allocated === 1 ? '摊算(恒 1)' : b.weight_is_allocated }}</td>
            <td>{{ b.status }}</td>
          </tr>
        </tbody>
      </table>
    </template>

    <!-- ===== 标签打印 ===== -->
    <template v-else-if="tab === 'label'">
      <div class="toolbar">
        <span>选择车次：</span>
        <select :value="truckId" @change="pickTruck($event.target.value)">
          <option value="">— 请选择 —</option>
          <option v-for="t in trucks" :key="t.id" :value="t.id">
            {{ String(t.seq_no).padStart(2, '0') }} · {{ t.human || t.code }} · {{ t.plate_no || '' }}
          </option>
        </select>
        <button v-if="bags.length" class="ghost" @click="pickAll(bags)">全选有效袋</button>
        <button class="ghost" @click="pickedCodes = []">清空勾选</button>
      </div>

      <div class="toolbar" v-if="pickedCodes.length">
        <button @click="printLabels(false)">打印选中（{{ pickedCodes.length }} 张）</button>
        <input v-model="reprintReason" placeholder="补打原因（必填）" style="min-width: 16rem" />
        <button @click="printLabels(true)">补打选中</button>
        <span class="hint">版式页由服务端生成 → 浏览器打印（二维码 = 27 位裸串）</span>
      </div>
      <p v-if="truckErr" class="err">{{ truckErr }}</p>

      <table v-if="bags.length">
        <thead>
          <tr><th>选</th><th>袋序</th><th>袋码（人读行）</th><th>状态</th></tr>
        </thead>
        <tbody>
          <tr v-for="b in bags" :key="b.id">
            <td>
              <input type="checkbox" :checked="pickedCodes.includes(b.code)" :disabled="b.status === '作废'"
                     @change="toggleCode(b.code)" />
            </td>
            <td>{{ b.bag_seq }}</td>
            <td>{{ b.human }}</td>
            <td>{{ b.status }}</td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">（请先选择车次；袋码在过磅确认后才会生成）</p>
    </template>

    <!-- ===== 作废与退车 ===== -->
    <template v-else>
      <div class="toolbar">
        <span>选择车次：</span>
        <select :value="truckId" @change="pickTruck($event.target.value)">
          <option value="">— 请选择 —</option>
          <option v-for="t in trucks" :key="t.id" :value="t.id">
            {{ String(t.seq_no).padStart(2, '0') }} · {{ t.human || t.code }} · {{ t.status }}
          </option>
        </select>
        <button v-if="curTruck && curTruck.status !== '已退货'" class="ghost" @click="returnTruck">退车登记</button>
      </div>
      <p class="hint">
        袋作废：录错袋数时不改正、不删行，对多余袋执行作废（填原因）；<b>已取样 / 已投料的袋会被拒绝</b>；
        作废码永久不重用，bag_count 按有效袋计。退车须先有质检「退货」判定，通过后整车袋码作废。
      </p>
      <p v-if="voidErr" class="err">{{ voidErr }}</p>

      <table v-if="bags.length">
        <thead>
          <tr><th>袋序</th><th>袋码（人读行）</th><th>状态</th><th>操作</th></tr>
        </thead>
        <tbody>
          <tr v-for="b in bags" :key="b.id">
            <td>{{ b.bag_seq }}</td>
            <td>{{ b.human }}</td>
            <td>{{ b.status }}</td>
            <td class="ops">
              <button v-if="b.status !== '作废'" class="ghost" @click="voidBag(b)">作废</button>
              <span v-else class="hint">已作废</span>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">（请先选择车次）</p>
    </template>
  </section>
</template>
