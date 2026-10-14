<script setup>
// 收货页（M3 / N-023 D3）：三列表结构 ——
//   ① 未关闭批次 L1（批次预填 · 进度 · 关闭 · 继续预报）
//   ② 未到货车次 L2（车次预报 · 到货确认 · 修改 · 取消/空号）
//   ③ 查询 L3（批次＋车次统一多条件查询；旧版预报只展示不可编辑）
//   ④ 过磅与袋码 / ⑤ 标签打印 / ⑥ 作废与退车（原样保留）
// ★ 主数据下拉只取**启用**项（复用批 2 的 is_current / status 口径）。
// ★ 标签版式页由服务端生成，这里只负责打开浏览器打印窗口（一期不写打印驱动）。
import { ref, computed, onMounted } from 'vue'
import { api } from '../api.js'

defineProps({ me: { type: Object, default: null } })

const TABS = [
  { k: 'batch', n: '未关闭批次' },
  { k: 'notice', n: '未到货车次' },
  { k: 'search', n: '查询' },
  { k: 'truck', n: '过磅与袋码' },
  { k: 'label', n: '标签打印' },
  { k: 'void', n: '作废与退车' },
]
const tab = ref('batch')
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
    err.value = '' // ★ 成功即清错误（否则"未登录时"挂载留下的报错会一直挂着）
  } catch (e) {
    err.value = '加载主数据下拉失败：' + e.message
  }
}

function label(row) {
  return `${row.code || ''} ${row.name || row.plate_no || ''}`.trim()
}

// 查询串拼接：空值不入参（后端空串/0 = 不限）。
function qs(params) {
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== '' && v !== null && v !== undefined) p.set(k, v)
  }
  const s = p.toString()
  return s ? '?' + s : ''
}

// 全量刷新列表（三列表共享；已跑过 L3 查询则顺带重跑）。
async function refreshLists() {
  await Promise.all([loadBatches(), loadOpenBatches(), loadNotices()])
  if (searchResult.value) await runSearch()
}

// ===== L1 · 未关闭批次 =====
const batches = ref([])
const batchStatus = ref('回货中') // 回货中 / 已关闭 / 全部
const batchForm = ref({ customer_id: '', material_id: '', customer_batch_no: '' })
const batchMsg = ref('')
const batchErr = ref('')
const batchExisting = ref([]) // 400 三元组重复时的已有批次（展示/跳转）

async function loadBatches() {
  try {
    const data = await api.get('/api/recv/batches' + qs({ status: batchStatus.value }))
    batches.value = data.rows || []
  } catch (e) {
    batches.value = []
    if (!err.value) err.value = e.message
  }
}

async function createBatch() {
  batchErr.value = ''
  batchMsg.value = ''
  batchExisting.value = []
  const f = batchForm.value
  if (!f.customer_id || !f.material_id || !String(f.customer_batch_no).trim()) {
    batchErr.value = '客户 / 产品 / 客户批次号均为必填'
    return
  }
  try {
    const data = await api.post('/api/recv/batches', {
      customer_id: Number(f.customer_id),
      material_id: Number(f.material_id),
      customer_batch_no: String(f.customer_batch_no).trim(),
    })
    batchMsg.value = data.row.our_batch_no
    f.customer_batch_no = ''
    await refreshLists()
  } catch (e) {
    // 400 三元组重复：e.data.existing 附已有批次供跳转
    batchErr.value = e.message
    batchExisting.value = (e.data && e.data.existing) || []
  }
}

async function closeBatch(b) {
  if (!window.confirm(`确认关闭批次 ${b.our_batch_no}（客户批次 ${b.customer_batch_no}）？关闭后该批次不可再预报新车次。`)) return
  try {
    await api.post(`/api/recv/batches/${b.id}/close`)
    msg.value = `批次 ${b.our_batch_no} 已关闭（接口幂等）`
    await refreshLists()
  } catch (e) {
    err.value = e.message
  }
}

// 继续预报：切到车次预报表单并预选该批次（清筛选，保证预选项在下拉里可见）。
function continueForecast(b) {
  noticeFilter.value = { customer_id: '', material_id: '' }
  noticeForm.value.batch_id = b.id
  noticeErr.value = ''
  tab.value = 'notice'
}

// 400 已有批次 → 跳查询页按我方批次号检索。
function jumpExisting(b) {
  searchForm.value.our_batch_no = b.our_batch_no
  searchResult.value = null
  tab.value = 'search'
  runSearch()
}

// ===== L2 · 未到货车次（车次预报 + 到货确认） =====
const notices = ref([])
// 车次预报的批次下拉：仅回货中（关闭后不可预报）。
const openBatches = ref([])
// 下拉前的客户/产品可选筛选（前端过滤批次选项）。
const noticeFilter = ref({ customer_id: '', material_id: '' })
const noticeForm = ref({ batch_id: '', vehicle_id: '', plate_no: '', driver: '', phone: '' })
const noticeErr = ref('')

const filteredOpenBatches = computed(() =>
  openBatches.value.filter(
    (b) =>
      (!noticeFilter.value.customer_id || Number(b.customer_id) === Number(noticeFilter.value.customer_id)) &&
      (!noticeFilter.value.material_id || Number(b.material_id) === Number(noticeFilter.value.material_id)),
  ),
)

async function loadOpenBatches() {
  try {
    const data = await api.get('/api/recv/batches?status=' + encodeURIComponent('回货中'))
    openBatches.value = data.rows || []
  } catch (e) {
    openBatches.value = []
    if (!err.value) err.value = e.message
  }
}

async function loadNotices() {
  try {
    const data = await api.get('/api/recv/notices?pending=1')
    notices.value = data.rows || []
  } catch (e) {
    notices.value = []
    if (!err.value) err.value = e.message
  }
}

async function submitNotice() {
  noticeErr.value = ''
  const f = noticeForm.value
  if (!f.batch_id) {
    noticeErr.value = '必须选择批次'
    return
  }
  try {
    const data = await api.post('/api/recv/notices', {
      batch_id: Number(f.batch_id),
      vehicle_id: Number(f.vehicle_id || 0),
      plate_no: f.plate_no,
      driver: f.driver,
      phone: f.phone,
    })
    msg.value = `预报已登记，批次内车序 ${String(data.row.seq_no).padStart(2, '0')}（预报单 ${data.row.notice_no}）`
    f.vehicle_id = ''
    f.plate_no = ''
    f.driver = ''
    f.phone = ''
    await refreshLists()
  } catch (e) {
    noticeErr.value = e.message
  }
}

// 修改预报（只允许车牌/司机/电话；必须填原因，车序不变）。
const noticeEdit = ref(null) // { id, notice_no, plate_no, driver, phone, reason }
const noticeEditErr = ref('')

function openNoticeEdit(n) {
  noticeEditErr.value = ''
  noticeEdit.value = {
    id: n.id,
    notice_no: n.notice_no,
    plate_no: n.plate_no || '',
    driver: n.driver || '',
    phone: n.phone || '',
    reason: '',
  }
}

async function saveNoticeEdit() {
  noticeEditErr.value = ''
  const f = noticeEdit.value
  if (!f.reason) {
    noticeEditErr.value = '修改预报必须填原因（车序保持不变）'
    return
  }
  try {
    await api.put(`/api/recv/notices/${f.id}`, {
      reason: f.reason,
      plate_no: f.plate_no,
      driver: f.driver,
      phone: f.phone,
    })
    msg.value = `${f.notice_no} 已更新（车序保持不变）`
    noticeEdit.value = null
    await refreshLists()
  } catch (e) {
    noticeEditErr.value = e.message
  }
}

async function cancelNotice(n, status) {
  const reason = window.prompt(status === '空号' ? '登记空号（车未到，序号作废不回收）原因：' : '取消预报原因：')
  if (!reason) return
  try {
    await api.patch(`/api/recv/notices/${n.id}/status`, { status, reason })
    msg.value = `${n.notice_no} 已置为「${status}」（车序 ${n.seq_no} 不回收）`
    await refreshLists()
  } catch (e) {
    err.value = e.message
  }
}

async function confirmArrival(n) {
  try {
    const data = await api.post('/api/recv/arrivals', { notice_id: n.id })
    msg.value = `到货确认成功：车码 ${data.row.human || data.row.code}`
    await refreshLists()
    await loadTrucks()
  } catch (e) {
    err.value = e.message
  }
}

// ===== L3 · 统一查询 =====
const searchForm = ref({
  customer_id: '',
  material_id: '',
  customer_batch_no: '',
  our_batch_no: '',
  batch_status: '',
  notice_status: '',
  plate: '',
  legacy: '', // '' = 全部 · '0' = 只看批次预报 · '1' = 只看旧版
})
const searchResult = ref(null) // { batch_count, batches, notice_count, notices }

async function runSearch() {
  err.value = ''
  const f = searchForm.value
  try {
    const data = await api.get(
      '/api/recv/search' +
        qs({
          customer_id: f.customer_id,
          material_id: f.material_id,
          customer_batch_no: f.customer_batch_no,
          our_batch_no: f.our_batch_no,
          batch_status: f.batch_status,
          notice_status: f.notice_status,
          plate: f.plate,
          legacy: f.legacy,
        }),
    )
    searchResult.value = data
  } catch (e) {
    err.value = e.message
  }
}

// ===== 车次：过磅与袋码 =====
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

// ===== 标签打印 =====
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
  if (!pickedCodes.value.length) {
    truckErr.value = '请先勾选要打印的标签'
    return
  }
  const body = { codes: pickedCodes.value }
  if (isReprint) {
    if (!reprintReason.value) {
      truckErr.value = '补打必须填原因（否则会出现「一物两码」）'
      return
    }
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

// ===== 作废与退车 =====
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
  loadBatches()
  loadOpenBatches()
  loadNotices()
  loadTrucks()
})
</script>

<template>
  <section>
    <h2>收货与打码（M3）</h2>
    <p class="hint">
      预填<b>批次</b>（客户＋产品＋客户批次号）→ 批次内车次预报分配<b>车序</b> → 到货确认生成<b>车码 A</b>
      → 过磅后按<b>实际袋数</b>批量生成<b>袋码 B</b> → 打印标签。
      跳号不回收、超限不进位、补打必填原因、已取样/已投料的袋不许作废。
    </p>

    <div class="tabs">
      <button v-for="t in TABS" :key="t.k" :class="{ on: tab === t.k }" @click="tab = t.k">{{ t.n }}</button>
    </div>

    <p v-if="msg" class="ok">{{ msg }}</p>
    <p v-if="err" class="err">{{ err }}</p>

    <!-- ===== 修改预报（L2/L3 共用行内表单） ===== -->
    <div v-if="noticeEdit && (tab === 'notice' || tab === 'search')" class="panel">
      <div class="panel-head">
        <b>修改预报 {{ noticeEdit.notice_no }}（车序不变）</b>
        <button class="ghost" @click="noticeEdit = null">收起</button>
      </div>
      <div class="form">
        <label><span>车牌号</span><input v-model="noticeEdit.plate_no" placeholder="湘F·xxxxx" /></label>
        <label><span>司机</span><input v-model="noticeEdit.driver" /></label>
        <label><span>司机电话</span><input v-model="noticeEdit.phone" /></label>
        <label class="full">
          <span>修改原因<em>*</em></span>
          <input v-model="noticeEdit.reason" placeholder="必填：落审计；车序保持不动" />
        </label>
        <p v-if="noticeEditErr" class="err full">{{ noticeEditErr }}</p>
        <div class="full">
          <button @click="saveNoticeEdit">保存</button>
          <button class="ghost" @click="noticeEdit = null">取消</button>
        </div>
      </div>
    </div>

    <!-- ===== L1 · 未关闭批次 ===== -->
    <template v-if="tab === 'batch'">
      <div class="panel">
        <div class="panel-head">
          <b>批次预填（客户＋产品＋客户批次号，三元组唯一）</b>
        </div>
        <div class="form">
          <label>
            <span>客户<em>*</em></span>
            <select v-model="batchForm.customer_id">
              <option value="">请选择</option>
              <option v-for="c in customers" :key="c.id" :value="c.id">{{ label(c) }}</option>
            </select>
          </label>
          <label>
            <span>产品（原料）<em>*</em></span>
            <select v-model="batchForm.material_id">
              <option value="">请选择</option>
              <option v-for="m in materials" :key="m.id" :value="m.id">{{ label(m) }}</option>
            </select>
          </label>
          <label>
            <span>客户批次号<em>*</em></span>
            <input v-model="batchForm.customer_batch_no" placeholder="供应商批号" />
          </label>
          <div class="full">
            <button @click="createBatch">预填批次</button>
            <span class="hint"> 提交后生成我方批次号；同一三元组不可重复预填</span>
          </div>
          <p v-if="batchMsg" class="ok full">
            已创建批次，我方批次号 <b class="mono">{{ batchMsg }}</b>
          </p>
          <p v-if="batchErr" class="err full">{{ batchErr }}</p>
          <div v-if="batchExisting.length" class="full">
            <span class="hint">已有批次（点击跳查询）：</span>
            <button
              v-for="b in batchExisting"
              :key="b.id || b.our_batch_no"
              class="ghost"
              @click="jumpExisting(b)"
            >
              <span class="mono">{{ b.our_batch_no }}</span> / {{ b.customer_batch_no }}
            </button>
          </div>
        </div>
      </div>

      <div class="toolbar">
        <span>状态：</span>
        <select v-model="batchStatus" @change="loadBatches">
          <option value="回货中">回货中</option>
          <option value="已关闭">已关闭</option>
          <option value="">全部</option>
        </select>
        <button class="ghost" @click="loadBatches">刷新</button>
        <span class="hint">共 {{ batches.length }} 条</span>
      </div>

      <table>
        <thead>
          <tr>
            <th>我方批次号</th><th>客户批次号</th><th>客户</th><th>产品</th><th>状态</th>
            <th>车次数</th><th>已到货</th><th>已投完</th><th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="b in batches" :key="b.id">
            <td class="mono">{{ b.our_batch_no }}</td>
            <td>{{ b.customer_batch_no }}</td>
            <td>{{ b.customer_code ? b.customer_code + ' ' + b.customer_name : '—' }}</td>
            <td>{{ b.material_code + ' ' + b.material_name }}</td>
            <td>{{ b.status }}</td>
            <td>{{ b.notice_count }}</td>
            <td>{{ b.arrived_count }}</td>
            <td>已投 {{ b.fed_done_count }}/{{ b.arrived_count }} 车</td>
            <td class="ops">
              <template v-if="b.status !== '已关闭'">
                <button @click="closeBatch(b)">关闭</button>
                <button class="ghost" @click="continueForecast(b)">继续预报</button>
              </template>
              <span v-else class="hint">已关闭</span>
            </td>
          </tr>
          <tr v-if="!batches.length">
            <td colspan="9" class="empty">（暂无批次）</td>
          </tr>
        </tbody>
      </table>
    </template>

    <!-- ===== L2 · 未到货车次 ===== -->
    <template v-else-if="tab === 'notice'">
      <div class="panel">
        <div class="panel-head">
          <b>车次预报（系统分配批次内车序）</b>
        </div>
        <div class="form">
          <label>
            <span>客户筛选（可选，前端过滤批次）</span>
            <select v-model="noticeFilter.customer_id">
              <option value="">全部客户</option>
              <option v-for="c in customers" :key="c.id" :value="c.id">{{ label(c) }}</option>
            </select>
          </label>
          <label>
            <span>产品筛选（可选，前端过滤批次）</span>
            <select v-model="noticeFilter.material_id">
              <option value="">全部产品</option>
              <option v-for="m in materials" :key="m.id" :value="m.id">{{ label(m) }}</option>
            </select>
          </label>
          <label>
            <span>批次（仅回货中）<em>*</em></span>
            <select v-model="noticeForm.batch_id">
              <option value="">请选择</option>
              <option v-for="b in filteredOpenBatches" :key="b.id" :value="b.id">
                {{ b.our_batch_no }} {{ b.customer_batch_no }} {{ b.customer_name }} {{ b.material_name }}
              </option>
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
          <p v-if="noticeErr" class="err full">{{ noticeErr }}</p>
          <div class="full">
            <button @click="submitNotice">登记预报</button>
            <span class="hint"> 客户/产品/批次号由批次带出；无预计袋数与预计到货时间</span>
          </div>
        </div>
      </div>

      <div class="toolbar">
        <button class="ghost" @click="refreshLists">刷新</button>
        <span class="hint">共 {{ notices.length }} 条未到货车次</span>
      </div>

      <table>
        <thead>
          <tr>
            <th>车序</th><th>预报单号</th><th>我方批次号</th><th>客户批次号</th><th>车牌</th>
            <th>司机</th><th>电话</th><th>状态</th><th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="n in notices" :key="n.id">
            <td>{{ String(n.seq_no).padStart(2, '0') }}</td>
            <td>{{ n.notice_no }}</td>
            <td class="mono">{{ n.our_batch_no }}</td>
            <td>{{ n.customer_batch_no }}</td>
            <td>{{ n.plate_no || '—' }}</td>
            <td>{{ n.driver || '—' }}</td>
            <td>{{ n.phone || '—' }}</td>
            <td>{{ n.status }}</td>
            <td class="ops">
              <template v-if="n.status === '预报'">
                <button @click="confirmArrival(n)">到货确认</button>
                <button class="ghost" @click="openNoticeEdit(n)">修改</button>
                <button class="ghost" @click="cancelNotice(n, '已取消')">取消</button>
                <button class="ghost" @click="cancelNotice(n, '空号')">空号</button>
              </template>
              <span v-else class="hint">—</span>
            </td>
          </tr>
          <tr v-if="!notices.length">
            <td colspan="9" class="empty">（暂无未到货车次）</td>
          </tr>
        </tbody>
      </table>
    </template>

    <!-- ===== L3 · 查询 ===== -->
    <template v-else-if="tab === 'search'">
      <div class="panel">
        <div class="panel-head">
          <b>统一查询（批次 ＋ 车次预报）</b>
        </div>
        <div class="form">
          <label>
            <span>客户</span>
            <select v-model="searchForm.customer_id">
              <option value="">全部</option>
              <option v-for="c in customers" :key="c.id" :value="c.id">{{ label(c) }}</option>
            </select>
          </label>
          <label>
            <span>产品（原料）</span>
            <select v-model="searchForm.material_id">
              <option value="">全部</option>
              <option v-for="m in materials" :key="m.id" :value="m.id">{{ label(m) }}</option>
            </select>
          </label>
          <label><span>客户批次号</span><input v-model="searchForm.customer_batch_no" placeholder="模糊匹配" /></label>
          <label><span>我方批次号</span><input v-model="searchForm.our_batch_no" placeholder="精确匹配" class="mono" /></label>
          <label>
            <span>批次状态</span>
            <select v-model="searchForm.batch_status">
              <option value="">全部</option>
              <option value="回货中">回货中</option>
              <option value="已关闭">已关闭</option>
            </select>
          </label>
          <label>
            <span>预报状态</span>
            <select v-model="searchForm.notice_status">
              <option value="">全部</option>
              <option value="预报">预报</option>
              <option value="已到货">已到货</option>
              <option value="已取消">已取消</option>
              <option value="空号">空号</option>
            </select>
          </label>
          <label><span>车牌</span><input v-model="searchForm.plate" placeholder="车牌号" /></label>
          <div class="full radios">
            <span>旧版：</span>
            <label><input v-model="searchForm.legacy" type="radio" value="" />全部</label>
            <label><input v-model="searchForm.legacy" type="radio" value="0" />只看批次预报</label>
            <label><input v-model="searchForm.legacy" type="radio" value="1" />只看旧版</label>
          </div>
          <div class="full">
            <button @click="runSearch">查询</button>
          </div>
        </div>
      </div>

      <template v-if="searchResult">
        <p class="hint">
          批次 {{ searchResult.batch_count }} 条 · 车次预报 {{ searchResult.notice_count }} 条
        </p>

        <h3>批次</h3>
        <table>
          <thead>
            <tr>
              <th>我方批次号</th><th>客户批次号</th><th>客户</th><th>产品</th><th>状态</th>
              <th>车次数</th><th>已到货</th><th>已投完</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="b in searchResult.batches" :key="b.id">
              <td class="mono">{{ b.our_batch_no }}</td>
              <td>{{ b.customer_batch_no }}</td>
              <td>{{ b.customer_code ? b.customer_code + ' ' + b.customer_name : '—' }}</td>
              <td>{{ b.material_code + ' ' + b.material_name }}</td>
              <td>{{ b.status }}</td>
              <td>{{ b.notice_count }}</td>
              <td>{{ b.arrived_count }}</td>
              <td>已投 {{ b.fed_done_count }}/{{ b.arrived_count }} 车</td>
            </tr>
            <tr v-if="!searchResult.batches.length">
              <td colspan="8" class="empty">（无匹配批次）</td>
            </tr>
          </tbody>
        </table>

        <h3>车次预报</h3>
        <table>
          <thead>
            <tr>
              <th>车序</th><th>预报单号</th><th>我方批次号</th><th>客户批次号</th><th>车牌</th>
              <th>司机</th><th>电话</th><th>状态</th><th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="n in searchResult.notices" :key="n.id">
              <td>{{ String(n.seq_no).padStart(2, '0') }}</td>
              <td>
                {{ n.notice_no }}
                <span v-if="n.legacy" class="tag">旧版</span>
              </td>
              <td class="mono">{{ n.our_batch_no || '—' }}</td>
              <td>{{ n.customer_batch_no || '—' }}</td>
              <td>{{ n.plate_no || '—' }}</td>
              <td>{{ n.driver || '—' }}</td>
              <td>{{ n.phone || '—' }}</td>
              <td>{{ n.status }}</td>
              <td class="ops">
                <!-- 旧版预报（batch_id=0）：只展示，不可编辑/取消 -->
                <span v-if="n.legacy" class="hint">旧版只读</span>
                <template v-else-if="n.status === '预报'">
                  <button @click="confirmArrival(n)">到货确认</button>
                  <button class="ghost" @click="openNoticeEdit(n)">修改</button>
                  <button class="ghost" @click="cancelNotice(n, '已取消')">取消</button>
                  <button class="ghost" @click="cancelNotice(n, '空号')">空号</button>
                </template>
                <span v-else class="hint">—</span>
              </td>
            </tr>
            <tr v-if="!searchResult.notices.length">
              <td colspan="9" class="empty">（无匹配车次预报）</td>
            </tr>
          </tbody>
        </table>
      </template>
      <p v-else class="hint">（设置筛选条件后点「查询」；条件留空 = 全部，旧版预报只读展示）</p>
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
