<script setup>
// 检测任务列表页（M5 · D1/D2/D6）：三来源并集 + 检测态过滤 + 尚未取样提示 +
// 建单挂大样 + 紧急放行发起/审批（按 perm-summary 显隐，服务端仍是唯一权威）。
import { ref, onMounted, computed } from 'vue'
import { api } from '../api.js'

const props = defineProps({ me: { type: Object, default: null } })
const emit = defineEmits(['open'])

const msg = ref('')
const err = ref('')
const rows = ref([])
const stateFilter = ref('')

// ---- 权限摘要（动作显隐） ----
const perms = ref({})
const can = (code) => !!perms.value[code] && perms.value[code] !== 'NONE'

async function loadPerms() {
  try {
    const data = await api.get('/api/insp/perm-summary')
    perms.value = data.points || {}
  } catch (e) {
    err.value = '加载权限摘要失败：' + e.message
  }
}

async function loadTasks() {
  err.value = ''
  try {
    let url = '/api/insp/tasks'
    if (stateFilter.value) url += '?state=' + encodeURIComponent(stateFilter.value)
    const data = await api.get(url)
    rows.value = data.rows || []
  } catch (e) {
    rows.value = []
    err.value = '加载检测任务失败：' + e.message
  }
}

const ENTITIES = { '车次': 'b_truck_lot', '生产批': 'b_production_batch', '成品批': 'b_fg_lot' }

async function createInsp(r) {
  err.value = ''
  msg.value = ''
  try {
    const data = await api.post('/api/insp', { target_type: r.target_type, target_id: r.target_id })
    msg.value = `检测单已建：${data.row.inspection_no}`
    await loadTasks()
    emit('open', data.row.id)
  } catch (e) {
    err.value = '建单失败：' + e.message
  }
}

// ---- 紧急放行（行内面板） ----
const urgentOf = ref(null) // { target, state }
const urgentReason = ref('')

async function openUrgent(r) {
  err.value = ''
  urgentReason.value = ''
  const entity = ENTITIES[r.target_type]
  try {
    const data = await api.get(
      `/api/insp/urgent-release?entity=${entity}&entity_id=${r.target_id}`)
    urgentOf.value = { target: r, entity, state: data.row }
  } catch (e) {
    err.value = '查询放行态失败：' + e.message
  }
}

async function urgentInit() {
  err.value = ''
  try {
    await api.post('/api/insp/urgent-release/init', {
      entity: urgentOf.value.entity,
      entity_id: urgentOf.value.target.target_id,
      reason: urgentReason.value,
    })
    msg.value = '紧急放行已发起（等待审批）'
    await openUrgent(urgentOf.value.target)
  } catch (e) {
    err.value = '发起失败：' + e.message
  }
}

async function urgentApprove() {
  err.value = ''
  try {
    await api.post('/api/insp/urgent-release/approve', {
      entity: urgentOf.value.entity,
      entity_id: urgentOf.value.target.target_id,
      reason: urgentReason.value,
    })
    msg.value = '紧急放行已审批'
    await openUrgent(urgentOf.value.target)
  } catch (e) {
    err.value = '审批失败：' + e.message
  }
}

const stateCounts = computed(() => {
  const c = {}
  for (const r of rows.value) c[r.insp_state] = (c[r.insp_state] || 0) + 1
  return c
})

onMounted(() => {
  loadPerms()
  loadTasks()
})
</script>

<template>
  <section>
    <h2>检测任务（M5）</h2>
    <p class="hint">
      车次 ∪ 生产批 ∪ 成品批三来源并集；<b>尚未取样的对象照常列出</b>（标灰提示）。
      检测挂<b>大样</b>不挂袋 —— 无取样组时建单会被拒绝（先去取样建组）。
    </p>

    <div class="toolbar">
      <label>
        检测态
        <select v-model="stateFilter" @change="loadTasks">
          <option value="">全部</option>
          <option value="未取样">未取样</option>
          <option value="待检">待检</option>
          <option value="已检">已检</option>
        </select>
      </label>
      <button class="ghost" @click="loadTasks">刷新</button>
      <span class="hint">
        共 {{ rows.length }} 条
        <template v-if="!stateFilter">
          （未取样 {{ stateCounts['未取样'] || 0 }} · 待检 {{ stateCounts['待检'] || 0 }} · 已检 {{ stateCounts['已检'] || 0 }}）
        </template>
      </span>
    </div>

    <p v-if="msg" class="ok">{{ msg }}</p>
    <p v-if="err" class="err">{{ err }}</p>

    <table v-if="rows.length">
      <thead>
        <tr>
          <th>来源</th><th>对象编码</th><th>对象状态</th><th>检测态</th>
          <th>检测单号</th><th>结论</th><th>处置</th><th>待办</th><th></th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="r in rows" :key="r.target_type + '#' + r.target_id">
          <td>{{ r.target_type }}</td>
          <td>{{ r.target_code }}</td>
          <td>{{ r.target_status }}</td>
          <td>
            <span class="tag">{{ r.insp_state }}</span>
            <span v-if="r.hint" class="hint">{{ r.hint }}</span>
          </td>
          <td>{{ r.inspection_no || '—' }}</td>
          <td>{{ r.conclusion === 'CONCESSION' ? '让步接收' : (r.conclusion || '—') }}</td>
          <td>{{ r.disposition || '—' }}</td>
          <td>{{ r.todo_count || '—' }}</td>
          <td class="ops">
            <button v-if="r.inspection_id" @click="emit('open', r.inspection_id)">打开</button>
            <button v-if="can('insp.scope.edit') && !r.inspection_id"
              :disabled="r.insp_state === '未取样'" @click="createInsp(r)">建单</button>
            <button v-if="can('insp.urgent.release.init') || can('insp.urgent.release.approve')"
              class="ghost" @click="openUrgent(r)">紧急放行</button>
          </td>
        </tr>
      </tbody>
    </table>
    <p v-else class="hint">暂无检测任务。</p>

    <!-- 紧急放行面板：发起 ≠ 审批，两个动作按权限显隐 -->
    <div v-if="urgentOf" class="panel">
      <div class="panel-head">
        <b>紧急放行 · {{ urgentOf.target.target_type }} {{ urgentOf.target.target_code }}</b>
        <button class="ghost" @click="urgentOf = null">关闭</button>
      </div>
      <p class="hint">
        当前状态：
        <template v-if="urgentOf.state.initialized">
          已发起（{{ urgentOf.state.init_by }} {{ urgentOf.state.init_at }}
          <template v-if="urgentOf.state.approved">→ 已审批 {{ urgentOf.state.approved_by }} {{ urgentOf.state.approved_at }}</template>
          ）
        </template>
        <template v-else>未发起</template>
        <template v-if="urgentOf.state.reason"> · 理由：{{ urgentOf.state.reason }}</template>
      </p>
      <div class="toolbar">
        <input v-model="urgentReason" placeholder="放行理由（发起必填）" style="min-width: 20rem">
        <button v-if="can('insp.urgent.release.init') && !urgentOf.state.initialized"
          @click="urgentInit">发起</button>
        <button v-if="can('insp.urgent.release.approve') && urgentOf.state.initialized && !urgentOf.state.approved"
          @click="urgentApprove">审批</button>
        <span class="hint">发起 ≠ 审批：发起人不得自批（服务端强校验 403）</span>
      </div>
    </div>
  </section>
</template>
