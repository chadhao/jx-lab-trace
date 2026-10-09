<script setup>
// 出货页（M7 · D7）：装车归集（扫码枪逐袋回车 → 列表收集 → 提交；或向既有单追加）
// · 出货单列表与详情（明细袋码 / 状态 / 出场信息）· 出场登记表单
// · 撤销（发起 / 审批两个动作，按权限显隐，各带原因）。
// ★ 动作按 /api/ship/perm-summary 显隐；★ 服务端仍是唯一权威（前端隐藏 ≠ 服务端放行）。
// ★ 拒绝原因直接回显服务端 message（跨单归集 409 / 跨客户拆单 400 / 自审 403 …）。
import { ref, onMounted, computed } from 'vue'
import { api } from '../api.js'

defineProps({ me: { type: Object, default: null } })

const msg = ref('')
const err = ref('')

// ---- 权限摘要（动作显隐） ----
const perms = ref({})
// 只读判定：级别串含 READ 或 ALL（读入口挂 LevelRead，仅「非 NONE」不够）
const canRead = (code) => {
  const v = perms.value[code]
  return !!v && (v.includes('READ') || v.includes('ALL'))
}
const canWrite = (code) => {
  const v = perms.value[code]
  return !!v && v.includes('ALL')
}
const canInit = (code) => {
  const v = perms.value[code]
  return !!v && (v.includes('INIT') || v.includes('ALL'))
}
const canApprove = (code) => {
  const v = perms.value[code]
  return !!v && (v.includes('APPROVE') || v.includes('ALL'))
}

async function loadPerms() {
  try {
    const data = await api.get('/api/ship/perm-summary')
    perms.value = data.points || {}
  } catch (e) {
    err.value = '加载权限摘要失败：' + e.message
  }
}

// ================= 列表视图 =================
const rows = ref([])
const statusFilter = ref('')
const shipId = ref(null) // 非空 ⇒ 详情视图
const ship = ref(null)
const items = ref([])
const voidRecords = ref([])

async function loadList() {
  err.value = ''
  try {
    let url = '/api/ship/shipments'
    if (statusFilter.value) url += '?status=' + encodeURIComponent(statusFilter.value)
    const data = await api.get(url)
    rows.value = data.rows || []
  } catch (e) {
    rows.value = []
    err.value = e.message
  }
}

async function openShip(id) {
  err.value = ''
  msg.value = ''
  try {
    shipId.value = id
    const data = await api.get(`/api/ship/shipments/${id}`)
    ship.value = data.row
    items.value = data.items || []
    await loadVoidRecords()
  } catch (e) {
    err.value = '打开出货单失败：' + e.message
    shipId.value = null
  }
}

function backToList() {
  shipId.value = null
  ship.value = null
  items.value = []
  voidRecords.value = []
  loadList()
}

async function loadVoidRecords() {
  voidRecords.value = []
  if (!canRead('ship.void.init')) return
  try {
    const data = await api.get(`/api/ship/shipments/${shipId.value}/void-records`)
    voidRecords.value = data.rows || []
  } catch (e) {
    // 读不到不阻塞详情（服务端是唯一权威）
  }
}

// ================= 装车归集（建单） =================
const showForm = ref(false)
const scanCodes = ref([]) // 待提交的成品袋码列表（扫码枪逐袋回车收集）
const scanInput = ref('')
const scanErr = ref('')
const custFilter = ref('')
const remark = ref('')

function openForm() {
  scanErr.value = ''
  scanCodes.value = []
  scanInput.value = ''
  remark.value = ''
  showForm.value = true
}

function addScan() {
  scanErr.value = ''
  const code = (scanInput.value || '').trim()
  if (!code) return
  if (scanCodes.value.includes(code)) {
    scanErr.value = '该袋码已在待提交列表中：' + code
    scanInput.value = ''
    return
  }
  scanCodes.value = [...scanCodes.value, code]
  scanInput.value = ''
}

function removeScan(i) {
  scanCodes.value = scanCodes.value.filter((_, idx) => idx !== i)
}

async function saveShipment() {
  scanErr.value = ''
  if (!scanCodes.value.length) {
    scanErr.value = '请先扫入至少 1 个成品袋码（E 类码）'
    return
  }
  try {
    const payload = { bag_codes: scanCodes.value, remark: remark.value }
    if (custFilter.value) payload.customer_id = Number(custFilter.value)
    const data = await api.post('/api/ship/shipments', payload)
    msg.value = `出货单已建：${data.row.shipment_no}（${scanCodes.value.length} 袋）`
    showForm.value = false
    await openShip(data.row.id)
  } catch (e) {
    // ★ 跨单归集 409 / 跨客户拆单 400 / 码校验 …… 原样回显
    scanErr.value = e.message
  }
}

// ================= 向既有单追加扫码 =================
const addInput = ref('')
const addErr = ref('')

async function addItem() {
  addErr.value = ''
  const code = (addInput.value || '').trim()
  if (!code) return
  try {
    await api.post(`/api/ship/shipments/${shipId.value}/items`, { bag_code: code })
    msg.value = '已归集：' + code
    addInput.value = ''
    await openShip(shipId.value)
  } catch (e) {
    addErr.value = e.message // 已归集 409 / 跨客户 400 / 已撤销 409 ……
  }
}

// ================= 出场登记 =================
const departForm = ref(null)
const departErr = ref('')

function openDepart() {
  departErr.value = ''
  departForm.value = { plate_no: '', driver: '', ship_at: '', operator: '' }
}

async function saveDepart() {
  departErr.value = ''
  const f = departForm.value
  if (!f.plate_no || !f.plate_no.trim()) { departErr.value = '车牌必填'; return }
  if (!f.driver || !f.driver.trim()) { departErr.value = '司机必填'; return }
  try {
    const payload = { plate_no: f.plate_no, driver: f.driver, operator: f.operator }
    if (f.ship_at) payload.ship_at = f.ship_at.replace('T', ' ') + ':00'
    await api.post(`/api/ship/shipments/${shipId.value}/depart`, payload)
    msg.value = '出场登记完成（该单全部成品袋转「已出厂」）'
    departForm.value = null
    await openShip(shipId.value)
  } catch (e) {
    departErr.value = e.message // 已登记 409 / 空单 409 ……
  }
}

// ================= 撤销（发起 / 审批） =================
const voidForm = ref(null) // { mode: 'init' | 'approve', reason: '' }
const voidErr = ref('')

function openVoid(mode) {
  voidErr.value = ''
  voidForm.value = { mode, reason: '' }
}

async function saveVoid() {
  voidErr.value = ''
  const f = voidForm.value
  try {
    if (f.mode === 'init') {
      if (!f.reason || !f.reason.trim()) { voidErr.value = '撤销原因必填'; return }
      await api.post(`/api/ship/shipments/${shipId.value}/void`, { reason: f.reason })
      msg.value = '撤销已发起（★ 发起不生效，待管理层审批）'
    } else {
      await api.post(`/api/ship/shipments/${shipId.value}/void/approve`, {})
      msg.value = '撤销已审批生效（单置「已撤销」，袋回退「在库」）'
    }
    voidForm.value = null
    await openShip(shipId.value)
    await loadList()
  } catch (e) {
    // ★ 自审 403 / 未发起 409 / 重复发起 400 …… 原样回显
    voidErr.value = e.message
  }
}

const departed = computed(() => !!(ship.value && ship.value.ship_at))

onMounted(() => {
  loadPerms()
  loadList()
})
</script>

<template>
  <section>
    <h2>出货（装车归集 · 出场登记 · 撤销）</h2>
    <p class="hint">
      ★ 逐袋扫<b>成品袋码（E 类）</b>归集成出货单；★ 一个成品袋只能进一个未撤销的出货单；
      ★ 建单即「已出厂」，出场登记用 <code>ship_at</code> 表达；★ 撤销 = 发起（不生效）→ 审批（生效），发起人 ≠ 审批人。
    </p>

    <div class="toolbar">
      <label>
        状态
        <select v-model="statusFilter" @change="loadList">
          <option value="">全部</option>
          <option value="已出厂">已出厂</option>
          <option value="已撤销">已撤销</option>
        </select>
      </label>
      <button class="ghost" @click="loadList">刷新</button>
      <button v-if="canWrite('ship.load.scan')" @click="openForm">新建出货单（装车归集）</button>
      <span class="hint">共 {{ rows.length }} 条</span>
    </div>

    <p v-if="msg" class="ok">{{ msg }}</p>
    <p v-if="err" class="err">{{ err }}</p>

    <!-- ============ 建单归集表单 ============ -->
    <div v-if="showForm" class="panel">
      <div class="panel-head">
        <b>装车归集（扫码枪逐袋回车 → 列表 → 提交）</b>
        <button class="ghost" @click="showForm = false">取消</button>
      </div>
      <p class="hint">
        ★ 客户缺省时由<b>首个袋</b>所属成品批推定；★ 跨客户必须拆单（会拒绝并提示）；
        ★ 袋须在库、未被其他未撤销出货单归集。
      </p>
      <div class="toolbar">
        <input v-model="scanInput" placeholder="成品袋码（裸串或人读行）回车/点添加"
          style="min-width: 26rem" @keydown.enter.prevent="addScan">
        <button @click="addScan">添加</button>
        <label>客户（缺省按首袋推定）
          <select v-model="custFilter">
            <option value="">自动推定</option>
          </select>
        </label>
        <label>备注 <input v-model="remark" style="min-width: 12rem"></label>
        <button @click="saveShipment" :disabled="!scanCodes.length">提交建单</button>
      </div>
      <p v-if="scanErr" class="err">{{ scanErr }}</p>
      <table v-if="scanCodes.length">
        <thead>
          <tr><th>#</th><th>成品袋码</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-for="(c, i) in scanCodes" :key="c + i">
            <td>{{ i + 1 }}</td>
            <td class="mono">{{ c }}</td>
            <td class="ops"><button class="ghost" @click="removeScan(i)">移除</button></td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">尚未扫码。</p>
    </div>

    <!-- ============ 列表视图 ============ -->
    <template v-if="!shipId">
      <table v-if="rows.length">
        <thead>
          <tr>
            <th>出货单号</th><th>客户</th><th>车牌</th><th>司机</th>
            <th>出场时间</th><th>状态</th><th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in rows" :key="r.id">
            <td class="mono">{{ r.shipment_no }}</td>
            <td>{{ r.customer_name || '#' + r.customer_id }}</td>
            <td>{{ r.plate_no || '—' }}</td>
            <td>{{ r.driver || '—' }}</td>
            <td>{{ r.ship_at || '（未登记出场）' }}</td>
            <td><span class="tag">{{ r.status }}</span></td>
            <td class="ops">
              <button v-if="canRead('ship.load.scan')" @click="openShip(r.id)">详情</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">暂无出货单。</p>
    </template>

    <!-- ============ 详情视图 ============ -->
    <template v-else-if="ship">
      <div class="toolbar">
        <button class="ghost" @click="backToList">← 返回列表</button>
        <b class="mono">{{ ship.shipment_no }}</b>
        <span class="tag">{{ ship.status }}</span>
        <span class="hint">
          客户 {{ ship.customer_name || '#' + ship.customer_id }}
          · {{ departed ? '已出场 ' + ship.ship_at : '★ 未登记出场（ship_at 为空）' }}
        </span>
      </div>

      <!-- 明细 + 追加扫码 -->
      <div class="panel">
        <div class="panel-head"><b>明细袋码（{{ items.length }} 袋）</b></div>
        <div class="toolbar" v-if="canWrite('ship.load.scan') && ship.status === '已出厂'">
          <input v-model="addInput" placeholder="追加成品袋码（回车归集）" style="min-width: 24rem"
            @keydown.enter.prevent="addItem">
          <button @click="addItem">归集到本单</button>
        </div>
        <p v-if="addErr" class="err">{{ addErr }}</p>
        <table v-if="items.length">
          <thead>
            <tr><th>#</th><th>成品袋码</th><th>人读行</th><th>成品批</th><th>袋状态</th></tr>
          </thead>
          <tbody>
            <tr v-for="(it, i) in items" :key="it.id">
              <td>{{ i + 1 }}</td>
              <td class="mono">{{ it.bag_code }}</td>
              <td class="mono">{{ it.bag_human }}</td>
              <td class="mono">{{ it.fg_lot_code }}</td>
              <td>{{ it.bag_status }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">本单无明细。</p>
      </div>

      <!-- 出场登记 -->
      <div class="panel">
        <div class="panel-head">
          <b>出场登记（记客户 / 车牌 / 司机 / 时间）</b>
          <button v-if="canWrite('ship.out.register') && ship.status === '已出厂' && !departed"
            @click="openDepart">登记出场</button>
        </div>
        <p class="hint">★ 前置：单有明细、未登记过；★ 登记后该单全部成品袋转「已出厂」。</p>
        <div v-if="departForm" class="toolbar">
          <input v-model="departForm.plate_no" placeholder="车牌 *（必填）" style="width: 10rem">
          <input v-model="departForm.driver" placeholder="司机 *（必填）" style="width: 8rem">
          <label>出场时间 <input type="datetime-local" v-model="departForm.ship_at"></label>
          <input v-model="departForm.operator" placeholder="经办人" style="width: 8rem">
          <button @click="saveDepart">登记</button>
          <button class="ghost" @click="departForm = null">取消</button>
        </div>
        <p v-if="departErr" class="err">{{ departErr }}</p>
        <p v-if="ship.plate_no" class="hint">
          已登记：{{ ship.plate_no }} · 司机 {{ ship.driver }} · {{ ship.ship_at }}
          <template v-if="ship.operator"> · 经办 {{ ship.operator }}</template>
        </p>
      </div>

      <!-- 撤销 -->
      <div class="panel">
        <div class="panel-head">
          <b>出货单撤销（发起 ≠ 审批）</b>
          <button v-if="canInit('ship.void.init') && ship.status === '已出厂'"
            @click="openVoid('init')">发起撤销</button>
          <button v-if="canApprove('ship.void.approve') && ship.status === '已出厂'"
            @click="openVoid('approve')">审批撤销</button>
        </div>
        <p class="hint">
          ★ 发起只留痕<b>不改状态</b>（填原因）；★ 审批才生效（单置「已撤销」、袋回退「在库」、明细保留）；
          ★★ 发起人不得自行审批（403）。
        </p>
        <div v-if="voidForm" class="toolbar">
          <template v-if="voidForm.mode === 'init'">
            <input v-model="voidForm.reason" placeholder="撤销原因 *（必填）" style="min-width: 20rem">
            <button @click="saveVoid">发起</button>
          </template>
          <template v-else>
            <span class="hint">确认审批通过该撤销？（您不能是发起人）</span>
            <button @click="saveVoid">审批通过</button>
          </template>
          <button class="ghost" @click="voidForm = null">取消</button>
        </div>
        <p v-if="voidErr" class="err">{{ voidErr }}</p>

        <table v-if="voidRecords.length">
          <thead>
            <tr><th>动作</th><th>操作人</th><th>原因</th><th>时间</th></tr>
          </thead>
          <tbody>
            <tr v-for="r in voidRecords" :key="r.id">
              <td>{{ r.action === 'ship_void_init' ? '发起' : '审批生效' }}</td>
              <td>{{ r.actor_name || r.actor_open_id }}</td>
              <td>{{ r.reason || '—' }}</td>
              <td>{{ r.at || '—' }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else-if="canRead('ship.void.init')" class="hint">暂无撤销留痕。</p>
      </div>
    </template>
  </section>
</template>
