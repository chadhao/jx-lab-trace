<script setup>
// 生产与谱系页（M6 · D7）：生产批列表与建批 · 详情（投料扫码 / 投料更正删除 /
// 作业段 / 成品批成品袋与打印 / 返工）· 谱系查询（按袋码反查去向，多对多如实显示）。
// ★ 动作按 perm-summary 显隐；★ 服务端仍是唯一权威（前端隐藏 ≠ 服务端放行）。
// ★ 拒绝原因直接回显服务端 message（投料前置 / 超限 / 一袋只投一次 等）。
import { ref, onMounted, computed } from 'vue'
import { api } from '../api.js'

const props = defineProps({ me: { type: Object, default: null } })

const msg = ref('')
const err = ref('')

// ---- 权限摘要（动作显隐） ----
const perms = ref({})
const can = (code) => !!perms.value[code] && perms.value[code] !== 'NONE'

async function loadPerms() {
  try {
    const data = await api.get('/api/prod/perm-summary')
    perms.value = data.points || {}
  } catch (e) {
    err.value = '加载权限摘要失败：' + e.message
  }
}

// ---- 主数据下拉（客户 / 原料 / 成品，仅启用项） ----
const customers = ref([])
const inMaterials = ref([])
const outMaterials = ref([])

async function loadMaster() {
  try {
    const [c, m] = await Promise.all([
      api.get('/api/md/customers?status=' + encodeURIComponent('启用') + '&limit=500'),
      api.get('/api/md/materials?status=' + encodeURIComponent('启用') + '&limit=500'),
    ])
    customers.value = (c.rows || []).filter((r) => Number(r.is_current) === 1)
    const all = (m.rows || []).filter((r) => Number(r.is_current) === 1)
    inMaterials.value = all.filter((r) => r.kind === '原料')
    outMaterials.value = all.filter((r) => r.kind === '成品')
    err.value = '' // ★ 成功即清错误（否则"未登录时"挂载留下的报错会一直挂着）
  } catch (e) {
    err.value = '加载主数据下拉失败：' + e.message
  }
}

function today() {
  const d = new Date()
  const p = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

// ================= 列表视图 =================
const rows = ref([])
const statusFilter = ref('')
const batchId = ref(null) // 非空 ⇒ 详情视图
const batch = ref(null)

async function loadBatches() {
  err.value = ''
  try {
    let url = '/api/prod/batches'
    if (statusFilter.value) url += '?status=' + encodeURIComponent(statusFilter.value)
    const data = await api.get(url)
    rows.value = data.rows || []
  } catch (e) {
    rows.value = []
    err.value = e.message
  }
}

// ---- 建批 ----
const showForm = ref(false)
const form = ref(null)
const formErr = ref('')

function openForm() {
  formErr.value = ''
  form.value = {
    customer_id: '',
    input_material_id: '',
    planned_output_material_id: '',
    batch_date: today(),
    remark: '',
  }
}

async function saveBatch() {
  formErr.value = ''
  const f = form.value
  if (!f.customer_id) { formErr.value = '必须选择客户（受托加工，BT 恒为 CG）'; return }
  if (!f.input_material_id) { formErr.value = '必须选择投入原料'; return }
  if (!f.planned_output_material_id) { formErr.value = '必须选择计划产出成品物料'; return }
  try {
    const data = await api.post('/api/prod/batches', {
      customer_id: Number(f.customer_id),
      input_material_id: Number(f.input_material_id),
      planned_output_material_id: Number(f.planned_output_material_id),
      batch_date: f.batch_date,
      remark: f.remark,
    })
    msg.value = `生产批已建：${data.row.human || data.row.code}`
    showForm.value = false
    await loadBatches()
    await openBatch(data.row.id)
  } catch (e) {
    formErr.value = e.message
  }
}

// ================= 详情视图 =================
const feeds = ref([])
const ops = ref([])
const fgLots = ref([])
const reworks = ref([])

// 投料扫码
const scan = ref({ bag_code: '', feed_weight: '', operator: '' })
const scanErr = ref('')

async function loadDetail(id) {
  const data = await api.get(`/api/prod/batches/${id}`)
  batch.value = data.row
  await Promise.all([loadFeeds(), loadOps(), loadFgLots(), loadReworks()])
}

async function openBatch(id) {
  err.value = ''
  msg.value = ''
  try {
    batchId.value = id
    await loadDetail(id)
  } catch (e) {
    err.value = '打开生产批失败：' + e.message
    batchId.value = null
  }
}

function backToList() {
  batchId.value = null
  batch.value = null
  loadBatches()
}

async function loadFeeds() {
  try {
    const data = await api.get(`/api/prod/batches/${batchId.value}/feeds`)
    feeds.value = data.rows || []
  } catch (e) {
    feeds.value = []
    err.value = e.message
  }
}

async function doScan() {
  scanErr.value = ''
  if (!scan.value.bag_code) { scanErr.value = '请扫 / 输入吨袋码'; return }
  try {
    await api.post(`/api/prod/batches/${batchId.value}/feeds`, {
      bag_code: scan.value.bag_code,
      feed_weight: scan.value.feed_weight === '' ? undefined : Number(scan.value.feed_weight),
      operator: scan.value.operator,
    })
    msg.value = `投料成功：${scan.value.bag_code}`
    scan.value.bag_code = ''
    scan.value.feed_weight = ''
    await loadFeeds()
  } catch (e) {
    // ★ 拒绝原因原样回显（未出结论 / 已投料 / 校验位不过 …）
    scanErr.value = e.message
  }
}

async function correctFeed(f) {
  const reason = window.prompt(`更正投料记录 #${f.id} 的原因（只能改投料量 / 备注，谱系关系不可改）：`)
  if (!reason) return
  const w = window.prompt('新的投料量（吨，留空保持不变）：', f.feed_weight ?? '')
  const remark = window.prompt('新的备注（留空保持不变）：', f.remark || '')
  const payload = { reason }
  if (w !== null && w !== '') payload.feed_weight = Number(w)
  if (remark !== null) payload.remark = remark
  try {
    await api.patch(`/api/prod/feeds/${f.id}`, payload)
    msg.value = `投料记录 #${f.id} 已更正（原因：${reason}）`
    await loadFeeds()
  } catch (e) {
    err.value = e.message
  }
}

async function deleteFeed(f) {
  const reason = window.prompt(`删除投料记录 #${f.id}（袋 ${f.bag_code}）的原因：`)
  if (!reason) return
  try {
    await api.del(`/api/prod/feeds/${f.id}`, { reason })
    msg.value = `投料记录 #${f.id} 已删除，袋状态回置「在库」`
    await loadFeeds()
  } catch (e) {
    err.value = e.message
  }
}

// ---- 作业段 ----
const opForm = ref(null)
const opErr = ref('')

function openOp() {
  opErr.value = ''
  opForm.value = { operator: '', start_at: '', end_at: '', output_weight: '', remark: '' }
}

async function saveOp() {
  opErr.value = ''
  const f = opForm.value
  try {
    const payload = { operator: f.operator, remark: f.remark }
    if (f.start_at) payload.start_at = f.start_at.replace('T', ' ') + ':00'
    if (f.end_at) payload.end_at = f.end_at.replace('T', ' ') + ':00'
    if (f.output_weight !== '') payload.output_weight = Number(f.output_weight)
    await api.post(`/api/prod/batches/${batchId.value}/operations`, payload)
    msg.value = '作业段已记录（跨班组即多段，本段产出不等于整批产出）'
    opForm.value = null
    await loadOps()
  } catch (e) {
    opErr.value = e.message
  }
}

async function loadOps() {
  try {
    const data = await api.get(`/api/prod/batches/${batchId.value}/operations`)
    ops.value = data.rows || []
  } catch (e) {
    ops.value = []
    err.value = e.message
  }
}

// ---- 成品批 / 成品袋 ----
const fgForm = ref(null)
const fgErr = ref('')
const fgBags = ref({}) // fg_lot_id → rows

function openFgForm() {
  fgErr.value = ''
  fgForm.value = { pack_spec: '', net_weight: '', produced_at: '', remark: '' }
}

async function saveFgLot() {
  fgErr.value = ''
  const f = fgForm.value
  try {
    const payload = { pack_spec: f.pack_spec, remark: f.remark }
    if (f.net_weight !== '') payload.net_weight = Number(f.net_weight)
    if (f.produced_at) payload.produced_at = f.produced_at.replace('T', ' ') + ':00'
    await api.post(`/api/prod/batches/${batchId.value}/fg-lots`, payload)
    msg.value = '成品批已生成（实际产出只记在成品批，不回写生产批）'
    fgForm.value = null
    await loadFgLots()
  } catch (e) {
    fgErr.value = e.message
  }
}

async function loadFgLots() {
  try {
    const data = await api.get(`/api/prod/batches/${batchId.value}/fg-lots`)
    fgLots.value = data.rows || []
  } catch (e) {
    fgLots.value = []
    err.value = e.message
  }
}

async function genBags(lot) {
  const n = window.prompt(`成品批 ${lot.human || lot.code}：本次生成袋数（1~999，袋序连续不回收）：`, '10')
  if (!n) return
  try {
    const data = await api.post(`/api/prod/fg-lots/${lot.id}/bags`, { count: Number(n) })
    msg.value = `已生成 ${data.count} 个成品袋（qty_bag=${lot.qty_bag}→实际）`
    await loadFgLots()
    await showBags(lot)
  } catch (e) {
    err.value = e.message
  }
}

async function showBags(lot) {
  try {
    const data = await api.get(`/api/prod/fg-lots/${lot.id}/bags`)
    fgBags.value = Object.assign({}, fgBags.value, { [lot.id]: data.rows || [] })
  } catch (e) {
    err.value = e.message
  }
}

async function printBags(lot) {
  try {
    const data = await api.post(`/api/prod/fg-lots/${lot.id}/print`, {})
    msg.value = `已打印 ${data.count} 张标签`
  } catch (e) {
    err.value = e.message
  }
}

async function reprintBags(lot) {
  const reason = window.prompt('补打原因（必填，否则会出现「一物两码」）：')
  if (!reason) return
  try {
    const data = await api.post(`/api/prod/fg-lots/${lot.id}/print`, { is_reprint: true, reason })
    msg.value = `已补打 ${data.count} 张（is_reprint=1，原因：${reason}）`
  } catch (e) {
    err.value = e.message
  }
}

// ---- 返工 ----
const reworkErr = ref('')
async function doRework() {
  reworkErr.value = ''
  const reason = window.prompt('返工原因（必填；将新建一个**新批号**的生产批并关联原批）：')
  if (!reason) return
  try {
    const data = await api.post('/api/prod/rework', {
      src_batch_id: batchId.value,
      reason,
    })
    msg.value = `返工批已建：${data.row.human || data.row.code}（独立取号，非原批加后缀）`
    await loadReworks()
  } catch (e) {
    reworkErr.value = e.message
  }
}

async function loadReworks() {
  try {
    const data = await api.get(`/api/prod/rework?src_batch_id=${batchId.value}`)
    reworks.value = data.rows || []
  } catch (e) {
    reworks.value = []
    err.value = e.message
  }
}

// ================= 谱系查询 =================
const gene = ref({ code: '', row: null })
const geneErr = ref('')

async function doGene() {
  geneErr.value = ''
  gene.value.row = null
  if (!gene.value.code) { geneErr.value = '请输入 / 扫描吨袋码'; return }
  try {
    const data = await api.get('/api/prod/genealogy/bags/' + encodeURIComponent(gene.value.code.trim()))
    gene.value.row = data.row
  } catch (e) {
    geneErr.value = e.message
  }
}

const geneTargets = computed(() => {
  const r = gene.value.row
  if (!r) return []
  return r.feeds || []
})

onMounted(() => {
  loadPerms()
  loadMaster()
  loadBatches()
})
</script>

<template>
  <section>
    <h2>生产与谱系（M6）</h2>
    <p class="hint">
      建批 → <b>投料扫码写投料记录（谱系承重墙，多对多）</b> → 作业段（跨班组多段）→
      成品批 / 成品袋生成与打码 → 返工（新批号 + 关联原批）。
      ★ 未出结论的料不得投料（除非<b>生效</b>的紧急放行：init+approve 两人）；
      ★ 一袋只投一次；★ 实际产出只记在成品批。
    </p>

    <div class="toolbar">
      <label>
        状态
        <select v-model="statusFilter" @change="loadBatches">
          <option value="">全部</option>
          <option value="进行中">进行中</option>
          <option value="已完成">已完成</option>
          <option value="已作废">已作废</option>
        </select>
      </label>
      <button class="ghost" @click="loadBatches">刷新</button>
      <button v-if="can('prod.batch.create')" @click="openForm">建生产批</button>
      <span class="hint">共 {{ rows.length }} 条</span>
    </div>

    <p v-if="msg" class="ok">{{ msg }}</p>
    <p v-if="err" class="err">{{ err }}</p>

    <!-- ============ 谱系查询 ============ -->
    <details>
      <summary>谱系查询（按袋码反查去向）</summary>
      <div class="toolbar">
        <input v-model="gene.code" placeholder="吨袋码（裸串或人读行）" style="min-width: 24rem">
        <button @click="doGene">反查</button>
      </div>
      <p v-if="geneErr" class="err">{{ geneErr }}</p>
      <div v-if="gene.row" class="panel">
        <div class="panel-head"><b>袋 {{ gene.row.bag_human || gene.row.bag_code }}</b></div>
        <p class="hint">
          状态：{{ gene.row.status }} · 车次：{{ gene.row.truck_code || '—' }}
          · 去向批数：<b>{{ geneTargets.length }}</b>（多对多如实显示）
        </p>
        <table v-if="geneTargets.length">
          <thead>
            <tr><th>投料记录</th><th>生产批</th><th>投料量(吨)</th><th>投料时间</th><th>操作人</th></tr>
          </thead>
          <tbody>
            <tr v-for="f in geneTargets" :key="f.id">
              <td>#{{ f.id }}</td>
              <td>{{ f.batch_code || f.batch_id }}</td>
              <td>{{ f.feed_weight ?? '—' }}</td>
              <td>{{ f.fed_at || '—' }}</td>
              <td>{{ f.operator || '—' }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">该袋尚未投料（仍在库）。</p>
      </div>
    </details>

    <!-- ============ 建批表单 ============ -->
    <div v-if="showForm" class="panel">
      <div class="panel-head">
        <b>建生产批</b>
        <button class="ghost" @click="showForm = false">取消</button>
      </div>
      <p v-if="formErr" class="err">{{ formErr }}</p>
      <div class="toolbar" v-if="form">
        <label>客户
          <select v-model="form.customer_id">
            <option value="">请选择</option>
            <option v-for="c in customers" :key="c.id" :value="c.id">{{ c.code }} {{ c.name }}</option>
          </select>
        </label>
        <label>投入原料
          <select v-model="form.input_material_id">
            <option value="">请选择</option>
            <option v-for="m in inMaterials" :key="m.id" :value="m.id">{{ m.code }} {{ m.name }}</option>
          </select>
        </label>
        <label>计划产出成品
          <select v-model="form.planned_output_material_id">
            <option value="">请选择</option>
            <option v-for="m in outMaterials" :key="m.id" :value="m.id">{{ m.code }} {{ m.name }}</option>
          </select>
        </label>
        <label>链根日期 <input type="date" v-model="form.batch_date"></label>
        <label>备注 <input v-model="form.remark" style="min-width: 14rem"></label>
        <button @click="saveBatch">建批</button>
      </div>
      <p class="hint">批码 = 生产批码 C，物料段取<b>计划产出成品</b>，日期段 = 链根日期，批序按「客户 + 成品物料 + 创建日」。</p>
    </div>

    <!-- ============ 列表视图 ============ -->
    <template v-if="!batchId">
      <table v-if="rows.length">
        <thead>
          <tr>
            <th>批码</th><th>人读行</th><th>客户</th><th>投入物料</th><th>计划产出</th>
            <th>链根日期</th><th>状态</th><th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in rows" :key="r.id">
            <td class="mono">{{ r.code }}</td>
            <td class="mono">{{ r.human }}</td>
            <td>#{{ r.customer_id }}</td>
            <td>#{{ r.input_material_id }}</td>
            <td>#{{ r.planned_output_material_id }}</td>
            <td>{{ r.batch_date }}</td>
            <td><span class="tag">{{ r.status }}</span></td>
            <td class="ops">
              <button @click="openBatch(r.id)">详情</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">暂无生产批。</p>
    </template>

    <!-- ============ 详情视图 ============ -->
    <template v-else-if="batch">
      <div class="toolbar">
        <button class="ghost" @click="backToList">← 返回列表</button>
        <b class="mono">{{ batch.human || batch.code }}</b>
        <span class="tag">{{ batch.status }}</span>
        <span class="hint">链根日期 {{ batch.batch_date }} · 计划产出物料 #{{ batch.planned_output_material_id }}</span>
      </div>

      <!-- 投料扫码 -->
      <div class="panel">
        <div class="panel-head"><b>投料扫码（谱系承重墙）</b></div>
        <p class="hint">扫吨袋码（B）；★ 前置：袋在库且未投过 + 车次现行单合格/让步 或 生效紧急放行。</p>
        <div class="toolbar">
          <input v-model="scan.bag_code" placeholder="吨袋码（裸串或人读行）" style="min-width: 24rem">
          <input v-model="scan.feed_weight" placeholder="投料量(吨)" style="width: 8rem">
          <input v-model="scan.operator" placeholder="操作人" style="width: 8rem">
          <button v-if="can('prod.feed.scan')" @click="doScan">投料</button>
        </div>
        <p v-if="scanErr" class="err">{{ scanErr }}</p>

        <table v-if="feeds.length">
          <thead>
            <tr>
              <th>#</th><th>袋码</th><th>车次</th><th>投料量</th><th>投料时间</th><th>操作人</th><th>备注</th><th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="f in feeds" :key="f.id">
              <td>{{ f.id }}</td>
              <td class="mono">{{ f.bag_human || f.bag_code }}</td>
              <td class="mono">{{ f.truck_code || '—' }}</td>
              <td>{{ f.feed_weight ?? '—' }}</td>
              <td>{{ f.fed_at || '—' }}</td>
              <td>{{ f.operator || '—' }}</td>
              <td>{{ f.remark || '—' }}</td>
              <td class="ops">
                <button v-if="can('prod.feed.correct')" @click="correctFeed(f)">更正</button>
                <button v-if="can('prod.feed.correct')" @click="deleteFeed(f)">删除</button>
              </td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">尚未投料。</p>
      </div>

      <!-- 作业段 -->
      <div class="panel">
        <div class="panel-head">
          <b>作业段（跨班组记多段）</b>
          <button v-if="can('prod.op.log')" @click="openOp">记一段</button>
        </div>
        <p class="hint">★ 本段产出 ≠ 整批产出；★ 不校验时间段重叠、不做自动结算。</p>
        <div v-if="opForm" class="toolbar">
          <input v-model="opForm.operator" placeholder="操作人 / 班组" style="width: 10rem">
          <label>开始 <input type="datetime-local" v-model="opForm.start_at"></label>
          <label>结束 <input type="datetime-local" v-model="opForm.end_at"></label>
          <input v-model="opForm.output_weight" placeholder="本段产出(吨)" style="width: 9rem">
          <input v-model="opForm.remark" placeholder="备注" style="min-width: 12rem">
          <button @click="saveOp">保存</button>
          <button class="ghost" @click="opForm = null">取消</button>
        </div>
        <p v-if="opErr" class="err">{{ opErr }}</p>
        <table v-if="ops.length">
          <thead>
            <tr><th>段序</th><th>班组</th><th>操作人</th><th>开始</th><th>结束</th><th>本段产出(吨)</th><th>备注</th></tr>
          </thead>
          <tbody>
            <tr v-for="o in ops" :key="o.id">
              <td>{{ o.seq }}</td>
              <td>{{ o.team_id ?? '—' }}</td>
              <td>{{ o.operator || '—' }}</td>
              <td>{{ o.start_at || '—' }}</td>
              <td>{{ o.end_at || '—' }}</td>
              <td>{{ o.output_weight ?? '—' }}</td>
              <td>{{ o.remark || '—' }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">尚无作业段。</p>
      </div>

      <!-- 成品批 / 成品袋 -->
      <div class="panel">
        <div class="panel-head">
          <b>成品批 / 成品袋（实际产出唯一记录点）</b>
          <button v-if="can('prod.fg.gen')" @click="openFgForm">生成成品批</button>
        </div>
        <div v-if="fgForm" class="toolbar">
          <input v-model="fgForm.pack_spec" placeholder="包装规格" style="width: 10rem">
          <input v-model="fgForm.net_weight" placeholder="净重(吨)" style="width: 9rem">
          <label>产出时间 <input type="datetime-local" v-model="fgForm.produced_at"></label>
          <input v-model="fgForm.remark" placeholder="备注" style="min-width: 12rem">
          <button @click="saveFgLot">生成</button>
          <button class="ghost" @click="fgForm = null">取消</button>
        </div>
        <p v-if="fgErr" class="err">{{ fgErr }}</p>
        <table v-if="fgLots.length">
          <thead>
            <tr>
              <th>成品批码</th><th>人读行</th><th>袋数</th><th>净重</th><th>状态</th><th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="l in fgLots" :key="l.id">
              <td class="mono">{{ l.code }}</td>
              <td class="mono">{{ l.human }}</td>
              <td>{{ l.qty_bag }}</td>
              <td>{{ l.net_weight ?? '—' }}</td>
              <td><span class="tag">{{ l.status }}</span></td>
              <td class="ops">
                <button v-if="can('prod.fg.gen')" @click="genBags(l)">生成袋</button>
                <button class="ghost" @click="showBags(l)">看袋</button>
                <button v-if="can('prod.fg.gen')" @click="printBags(l)">打印</button>
                <button v-if="can('prod.fg.gen')" @click="reprintBags(l)">补打</button>
              </td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">尚无成品批。</p>

        <div v-for="l in fgLots" :key="'bags-' + l.id">
          <template v-if="fgBags[l.id] && fgBags[l.id].length">
            <p class="hint">成品批 {{ l.human }} 的成品袋：</p>
            <table>
              <thead>
                <tr><th>袋序</th><th>成品袋码</th><th>人读行</th><th>摊算重量</th><th>状态</th></tr>
              </thead>
              <tbody>
                <tr v-for="b in fgBags[l.id]" :key="b.id">
                  <td>{{ b.bag_seq }}</td>
                  <td class="mono">{{ b.code }}</td>
                  <td class="mono">{{ b.human }}</td>
                  <td>{{ b.weight_allocated ?? '—' }}</td>
                  <td>{{ b.status }}</td>
                </tr>
              </tbody>
            </table>
          </template>
        </div>
      </div>

      <!-- 返工 -->
      <div class="panel">
        <div class="panel-head">
          <b>返工（新批号 + 关联原批）</b>
          <button v-if="can('prod.rework')" @click="doRework">发起返工</button>
        </div>
        <p class="hint">★ 新批**独立取号**（非原批加后缀），链根日期 = 新建当日；★ 需生产 / 质检角色的发起级权限。</p>
        <p v-if="reworkErr" class="err">{{ reworkErr }}</p>
        <table v-if="reworks.length">
          <thead>
            <tr><th>新批</th><th>原批</th><th>原因</th><th>时间</th></tr>
          </thead>
          <tbody>
            <tr v-for="r in reworks" :key="r.id">
              <td class="mono">{{ r.new_batch_code }}</td>
              <td class="mono">{{ r.src_batch_code }}</td>
              <td>{{ r.reason }}</td>
              <td>{{ r.created_at || '—' }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">本批暂无返工关联。</p>
      </div>
    </template>
  </section>
</template>
