<script setup>
// 留样页（M4 · D7）：留样入库（位置 + 期限）· 借出/归还 · 销毁（发起/审批，按权限显隐）· 到期检索。
// ★ 只对留样（保留样 / 仲裁样）开放；销毁「发起 ≠ 审批」是两个权限点两个动作。
import { ref, onMounted, computed } from 'vue'
import { api } from '../api.js'

defineProps({ me: { type: Object, default: null } })

const TABS = [
  { k: 'in', n: '留样入库' },
  { k: 'lend', n: '借出 / 归还' },
  { k: 'destroy', n: '销毁' },
  { k: 'due', n: '到期检索' },
]
const tab = ref('in')
const msg = ref('')
const err = ref('')

// ---- 权限摘要（按权限显隐发起/审批） ----
const perms = ref({})
const can = (code) => perms.value[code] && perms.value[code] !== 'NONE'

async function loadPerms() {
  try {
    const data = await api.get('/api/sample/perm-summary')
    perms.value = data.points || {}
  } catch (e) {
    err.value = '加载权限摘要失败：' + e.message
  }
}

// ---- 数据 ----
const retentionSamples = ref([]) // 保留样 / 仲裁样
const retentionRows = ref([])
const defaults = ref(null)
const lendRows = ref([])
const destroyRows = ref([])
const pendingOnly = ref(true)

async function loadRetentionSamples() {
  try {
    const [a, b] = await Promise.all([
      api.get('/api/sample/samples?role=' + encodeURIComponent('保留样')),
      api.get('/api/sample/samples?role=' + encodeURIComponent('仲裁样')),
    ])
    retentionSamples.value = [...(a.rows || []), ...(b.rows || [])]
  } catch (e) {
    err.value = '加载留样样品失败：' + e.message
  }
}

async function loadRetention(dueBefore) {
  try {
    let url = '/api/sample/retention'
    if (dueBefore) url += '?due_before=' + encodeURIComponent(dueBefore)
    const data = await api.get(url)
    retentionRows.value = data.rows || []
    defaults.value = data.defaults || null
  } catch (e) {
    err.value = '加载留样记录失败：' + e.message
  }
}

async function loadLends() {
  try {
    const data = await api.get('/api/sample/lend')
    lendRows.value = data.rows || []
  } catch (e) {
    err.value = '加载借还记录失败：' + e.message
  }
}

async function loadDestroys() {
  try {
    const url = '/api/sample/destroy' + (pendingOnly.value ? '?pending=1' : '')
    const data = await api.get(url)
    destroyRows.value = data.rows || []
  } catch (e) {
    err.value = '加载销毁记录失败：' + e.message
  }
}

// ---- 入库 ----
const inForm = ref({ sample_id: '', location: '', retention_until: '' })

async function retainIn() {
  err.value = ''
  msg.value = ''
  const f = inForm.value
  if (!f.sample_id) { err.value = '请选择样品'; return }
  if (!f.location) { err.value = '位置必填（三层文本，如 化验室-留样柜A-第3层）'; return }
  try {
    const payload = { sample_id: Number(f.sample_id), location: f.location }
    if (f.retention_until) payload.retention_until = f.retention_until
    const data = await api.post('/api/sample/retention', payload)
    msg.value = `已入库：${data.row.sample_no} → ${data.row.location}，保留至 ${data.row.retention_until}（期限按样品类型取默认）`
    inForm.value = { sample_id: '', location: '', retention_until: '' }
    await loadRetention()
    await loadRetentionSamples()
  } catch (e) {
    err.value = e.message
  }
}

// ---- 借还 ----
const lendForm = ref({ sample_id: '', lent_to: '', purpose: '' })

async function lendOut() {
  err.value = ''
  msg.value = ''
  const f = lendForm.value
  if (!f.sample_id) { err.value = '请选择留样'; return }
  if (!f.lent_to) { err.value = '借给谁（lent_to）必填'; return }
  try {
    await api.post('/api/sample/lend', {
      sample_id: Number(f.sample_id), lent_to: f.lent_to, purpose: f.purpose,
    })
    msg.value = '借出已登记（状态 → 已借出）'
    lendForm.value = { sample_id: '', lent_to: '', purpose: '' }
    await refreshAll()
  } catch (e) {
    err.value = e.message
  }
}

async function returnLend(row) {
  err.value = ''
  msg.value = ''
  try {
    await api.post(`/api/sample/lend/${row.id}/return`, {})
    msg.value = `${row.sample_no} 已归还（状态 → 在库）`
    await refreshAll()
  } catch (e) {
    err.value = e.message
  }
}

// ---- 销毁 ----
const initForm = ref({ sample_id: '', destroyed_by: '', reason: '' })
const approveForm = ref({ sample_id: '', approved_by: '' })

async function destroyInit() {
  err.value = ''
  msg.value = ''
  const f = initForm.value
  if (!f.sample_id) { err.value = '请选择留样'; return }
  if (!f.destroyed_by) { err.value = '销毁执行人必填'; return }
  if (!f.reason) { err.value = '销毁原因必填'; return }
  try {
    await api.post('/api/sample/destroy/init', {
      sample_id: Number(f.sample_id), destroyed_by: f.destroyed_by, reason: f.reason,
    })
    msg.value = '销毁已发起（待审批 —— 发起不改状态，须审批人通过才销毁）'
    initForm.value = { sample_id: '', destroyed_by: '', reason: '' }
    pendingOnly.value = true
    await refreshAll()
  } catch (e) {
    err.value = e.message
  }
}

async function destroyApprove() {
  err.value = ''
  msg.value = ''
  const f = approveForm.value
  if (!f.sample_id) { err.value = '请选择待审批的销毁'; return }
  if (!f.approved_by) { err.value = '审批人必填（缺失即拒绝）'; return }
  try {
    await api.post('/api/sample/destroy/approve', {
      sample_id: Number(f.sample_id), approved_by: f.approved_by,
    })
    msg.value = '销毁已审批（状态 → 已销毁，发起人与审批人均已留痕）'
    approveForm.value = { sample_id: '', approved_by: '' }
    await refreshAll()
  } catch (e) {
    err.value = e.message
  }
}

// 在库的留样（借出/销毁的下拉只列这些）
const inStockRetentions = computed(() => retentionRows.value.filter((r) => r.status === '在库'))
const pendingDestroys = computed(() => destroyRows.value.filter((d) => d.pending))

// ---- 到期检索 ----
const dueDate = ref('')

async function searchDue() {
  await loadRetention(dueDate.value)
}

function today() {
  const d = new Date()
  const p = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

async function refreshAll() {
  await Promise.all([
    loadRetentionSamples(), loadRetention(), loadLends(), loadDestroys(),
  ])
}

onMounted(() => {
  dueDate.value = today()
  loadPerms()
  refreshAll()
})
</script>

<template>
  <section>
    <h2>留样（M4）</h2>
    <p class="hint">
      保留样与检测样<b>物理分开、各自独立跟踪</b>：保留样不因检测完成而销毁。
      期限默认 <b>原料 6 月 / 中间 3 月 / 成品 1 年 / 仲裁 2 年</b>
      <template v-if="defaults">
        （当前配置：原料 {{ defaults.RawMonths }} / 中间 {{ defaults.IntermediateMonths }}
        / 成品 {{ defaults.FGMonths }} / 仲裁 {{ defaults.ArbitrationMonths }} 月）
      </template>。
      销毁 <b>发起 ≠ 审批</b>：两个动作、两个权限，审批人缺失即拒绝。
    </p>

    <div class="tabs">
      <button v-for="t in TABS" :key="t.k" :class="{ on: tab === t.k }" @click="tab = t.k">{{ t.n }}</button>
    </div>

    <p v-if="msg" class="ok">{{ msg }}</p>
    <p v-if="err" class="err">{{ err }}</p>

    <!-- ===== 留样入库 ===== -->
    <template v-if="tab === 'in'">
      <div v-if="can('sample.retain.in')" class="panel">
        <div class="panel-head"><b>留样入库（位置 + 保留期限）</b></div>
        <div class="form">
          <label>
            <span>样品<em>*</em></span>
            <select v-model="inForm.sample_id">
              <option value="">请选择</option>
              <option v-for="s in retentionSamples" :key="s.id" :value="s.id">
                {{ s.sample_no }}（{{ s.role }} · {{ s.status }}）
              </option>
            </select>
          </label>
          <label>
            <span>位置（三层文本）<em>*</em></span>
            <input v-model="inForm.location" placeholder="化验室-留样柜A-第3层">
          </label>
          <label>
            <span>保留期限</span>
            <input v-model="inForm.retention_until" type="date"
              :placeholder="defaults ? `默认按类型（如原料 ${defaults.RawMonths} 个月）` : '默认按样品类型'">
          </label>
        </div>
        <div class="toolbar">
          <button @click="retainIn">登记入库</button>
          <span class="hint">留空 ⇒ 系统按样品类型取默认期限</span>
        </div>
      </div>
      <p v-else class="hint">当前账号无 sample.retain.in 权限，不能登记入库。</p>

      <div class="toolbar">
        <b>留样记录（{{ retentionRows.length }}）</b>
        <button class="ghost" @click="loadRetention()">刷新</button>
      </div>
      <table v-if="retentionRows.length">
        <thead>
          <tr><th>样品编号</th><th>角色</th><th>位置</th><th>入库时间</th><th>保留至</th><th>状态</th></tr>
        </thead>
        <tbody>
          <tr v-for="r in retentionRows" :key="r.id">
            <td>{{ r.sample_no }}</td>
            <td>{{ r.role }}</td>
            <td>{{ r.location }}</td>
            <td>{{ r.stored_at }}</td>
            <td>{{ r.retention_until }}</td>
            <td>{{ r.status }}</td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">尚无留样记录。</p>
    </template>

    <!-- ===== 借出 / 归还 ===== -->
    <template v-else-if="tab === 'lend'">
      <div v-if="can('sample.retain.lend')" class="panel">
        <div class="panel-head"><b>借出登记（只对在库留样开放）</b></div>
        <div class="form">
          <label>
            <span>留样<em>*</em></span>
            <select v-model="lendForm.sample_id">
              <option value="">请选择</option>
              <option v-for="r in inStockRetentions" :key="r.id" :value="r.sample_id">
                {{ r.sample_no }}（{{ r.role }}）
              </option>
            </select>
          </label>
          <label>
            <span>借给谁<em>*</em></span>
            <input v-model="lendForm.lent_to" placeholder="如：张工">
          </label>
          <label>
            <span>用途</span>
            <input v-model="lendForm.purpose" placeholder="如：复核">
          </label>
        </div>
        <div class="toolbar"><button @click="lendOut">登记借出</button></div>
      </div>
      <p v-else class="hint">当前账号无 sample.retain.lend 权限。</p>

      <div class="toolbar">
        <b>借还记录（{{ lendRows.length }}）</b>
        <button class="ghost" @click="loadLends">刷新</button>
      </div>
      <table v-if="lendRows.length">
        <thead>
          <tr><th>样品编号</th><th>借出时间</th><th>借给</th><th>用途</th><th>归还时间</th><th>当前状态</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-for="l in lendRows" :key="l.id">
            <td>{{ l.sample_no }}</td>
            <td>{{ l.lent_at }}</td>
            <td>{{ l.lent_to }}</td>
            <td>{{ l.purpose || '—' }}</td>
            <td>{{ l.returned_at || '未归还' }}</td>
            <td>{{ l.status }}</td>
            <td>
              <button v-if="!l.returned_at && can('sample.retain.lend')" class="ghost"
                @click="returnLend(l)">归还</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">尚无借还记录。</p>
    </template>

    <!-- ===== 销毁 ===== -->
    <template v-else-if="tab === 'destroy'">
      <div class="toolbar">
        <label class="hint"><input type="checkbox" v-model="pendingOnly" @change="loadDestroys"> 只看待审批</label>
        <button class="ghost" @click="loadDestroys">刷新</button>
      </div>

      <div v-if="can('sample.retain.destroy.init')" class="panel">
        <div class="panel-head"><b>销毁 · 发起（sample.retain.destroy.init）</b></div>
        <div class="form">
          <label>
            <span>留样<em>*</em></span>
            <select v-model="initForm.sample_id">
              <option value="">请选择</option>
              <option v-for="r in inStockRetentions" :key="r.id" :value="r.sample_id">
                {{ r.sample_no }}（{{ r.role }}）
              </option>
            </select>
          </label>
          <label>
            <span>销毁执行人<em>*</em></span>
            <input v-model="initForm.destroyed_by" placeholder="实际执行销毁的人">
          </label>
          <label>
            <span>原因<em>*</em></span>
            <input v-model="initForm.reason" placeholder="如：超过保留期">
          </label>
        </div>
        <div class="toolbar">
          <button @click="destroyInit">发起销毁</button>
          <span class="hint">发起 ≠ 审批：发起只登记，不改状态</span>
        </div>
      </div>

      <div v-if="can('sample.retain.destroy.approve')" class="panel">
        <div class="panel-head"><b>销毁 · 审批（sample.retain.destroy.approve）</b></div>
        <div class="form">
          <label>
            <span>待审批<em>*</em></span>
            <select v-model="approveForm.sample_id">
              <option value="">请选择</option>
              <option v-for="d in pendingDestroys" :key="d.id" :value="d.sample_id">
                {{ d.sample_no }}（发起人 {{ d.destroyed_by }} · {{ d.reason }}）
              </option>
            </select>
          </label>
          <label>
            <span>审批人<em>*</em></span>
            <input v-model="approveForm.approved_by" placeholder="必填，缺失即拒绝">
          </label>
        </div>
        <div class="toolbar">
          <button @click="destroyApprove">审批通过（销毁生效）</button>
        </div>
      </div>

      <p v-if="!can('sample.retain.destroy.init') && !can('sample.retain.destroy.approve')" class="hint">
        当前账号既无发起也无审批权限。
      </p>

      <div class="toolbar"><b>销毁记录（{{ destroyRows.length }}）</b></div>
      <table v-if="destroyRows.length">
        <thead>
          <tr><th>样品编号</th><th>发起人</th><th>审批人</th><th>原因</th><th>销毁时间</th><th>状态</th></tr>
        </thead>
        <tbody>
          <tr v-for="d in destroyRows" :key="d.id">
            <td>{{ d.sample_no }}</td>
            <td>{{ d.destroyed_by }}</td>
            <td>{{ d.approved_by || '（待审批）' }}</td>
            <td>{{ d.reason || '—' }}</td>
            <td>{{ d.destroyed_at }}</td>
            <td>{{ d.pending ? '待审批' : '已销毁' }}</td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">尚无销毁记录。</p>
    </template>

    <!-- ===== 到期检索 ===== -->
    <template v-else>
      <div class="toolbar">
        <label>保留期限 ≤
          <input v-model="dueDate" type="date">
        </label>
        <button @click="searchDue">检索</button>
        <span class="hint">走 idx_retention_until 索引；到期提醒为二期，本批只做到期可检索</span>
      </div>
      <table v-if="retentionRows.length">
        <thead>
          <tr><th>样品编号</th><th>角色</th><th>位置</th><th>保留至</th><th>状态</th></tr>
        </thead>
        <tbody>
          <tr v-for="r in retentionRows" :key="r.id">
            <td>{{ r.sample_no }}</td>
            <td>{{ r.role }}</td>
            <td>{{ r.location }}</td>
            <td>{{ r.retention_until }}</td>
            <td>{{ r.status }}</td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">该日期前没有到期留样。</p>
    </template>
  </section>
</template>
