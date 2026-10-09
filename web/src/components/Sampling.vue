<script setup>
// 取样页（M4 · D7）：扫码取样（原料/中间/成品）· 新建取样组并合并份样成大样。
// ★ 样品由扫码行为自然产生，绝不预生成 —— 页面只呈现真实扫出来的记录。
// ★ 对象选择只取有效项（服务端已排除作废/已退货）。
import { ref, onMounted } from 'vue'
import { api } from '../api.js'

defineProps({ me: { type: Object, default: null } })

const TABS = [
  { k: 'scan', n: '扫码取样' },
  { k: 'group', n: '取样组（大样）' },
]
const tab = ref('scan')
const msg = ref('')
const err = ref('')

// ---- 扫码取样 ----
const scanCode = ref('')
const lastTake = ref(null)
const samples = ref([])

async function loadSamples() {
  try {
    const data = await api.get('/api/sample/samples')
    samples.value = data.rows || []
  } catch (e) {
    err.value = '加载样品失败：' + e.message
  }
}

async function takeSample() {
  err.value = ''
  msg.value = ''
  if (!scanCode.value.trim()) { err.value = '请扫码或输入 27 位追踪码'; return }
  try {
    const data = await api.post('/api/sample/take', { code: scanCode.value.trim() })
    lastTake.value = data
    const nos = (data.samples || []).map((s) => s.sample_no).join('、')
    msg.value = `取样成功（父码 ${data.parent}）：${nos}`
    scanCode.value = ''
    await loadSamples()
  } catch (e) {
    err.value = e.message
  }
}

// ---- 取样组 ----
const TARGETS = ['车次', '生产批', '成品批']
const targetType = ref('车次')
const targets = ref([])
const targetId = ref('')
const candidates = ref([]) // 可并入的份样
const checked = ref({}) // id -> true
const groupRemark = ref('')
const groups = ref([])
const membersOf = ref(null) // { group, members }

async function loadTargets() {
  err.value = ''
  try {
    const data = await api.get('/api/sample/targets?target_type=' + encodeURIComponent(targetType.value))
    targets.value = data.rows || []
    targetId.value = ''
    candidates.value = []
    checked.value = {}
  } catch (e) {
    targets.value = []
    err.value = e.message
  }
}

async function pickTarget() {
  candidates.value = []
  checked.value = {}
  membersOf.value = null
  if (!targetId.value) return
  err.value = ''
  try {
    const data = await api.get(
      '/api/sample/samples?role=' + encodeURIComponent('份样') +
      '&target_type=' + encodeURIComponent(targetType.value) +
      '&target_id=' + targetId.value + '&group=none')
    candidates.value = data.rows || []
  } catch (e) {
    err.value = e.message
  }
}

function targetLabel(t) {
  const extra = t.extra ? `（${t.extra}）` : ''
  return `${t.human || t.code} ${extra} · ${t.status}`
}

async function createGroup() {
  err.value = ''
  msg.value = ''
  if (!targetId.value) { err.value = '请先选择目标对象'; return }
  const ids = Object.keys(checked.value).filter((k) => checked.value[k]).map(Number)
  try {
    const data = await api.post('/api/sample/groups', {
      target_type: targetType.value,
      target_id: Number(targetId.value),
      sample_ids: ids,
      remark: groupRemark.value,
    })
    const g = data.row
    msg.value = `取样组已建：组号 ${g.group_no}，并入份样 ${g.sample_count} 个（大样 ${g.composite ? g.composite.sample_no : ''}）`
    groupRemark.value = ''
    await loadGroups()
    await pickTarget()
    await loadSamples()
  } catch (e) {
    err.value = e.message
  }
}

async function loadGroups() {
  try {
    const data = await api.get('/api/sample/groups')
    groups.value = data.rows || []
  } catch (e) {
    err.value = '加载取样组失败：' + e.message
  }
}

async function showMembers(g) {
  err.value = ''
  try {
    const data = await api.get(`/api/sample/groups/${g.id}/members`)
    membersOf.value = data
  } catch (e) {
    err.value = e.message
  }
}

onMounted(() => {
  loadSamples()
  loadTargets()
  loadGroups()
})
</script>

<template>
  <section>
    <h2>取样（M4）</h2>
    <p class="hint">
      按国标路径 <b>份样 → 大样 → 检测</b>：扫吨袋码/生产批码/成品批码 ⇒ 生成
      <b>1 份样 + 1 保留样</b>（编号 = 父码前 7 组 + 角色码 + 2 位序号）。
      扫了才有记录，没扫就是没有 —— 不预生成、不写「未测」。
    </p>

    <div class="tabs">
      <button v-for="t in TABS" :key="t.k" :class="{ on: tab === t.k }" @click="tab = t.k">{{ t.n }}</button>
    </div>

    <p v-if="msg" class="ok">{{ msg }}</p>
    <p v-if="err" class="err">{{ err }}</p>

    <!-- ===== 扫码取样 ===== -->
    <template v-if="tab === 'scan'">
      <div class="toolbar">
        <input v-model="scanCode" placeholder="扫码或输入 27 位追踪码（吨袋 B / 生产批 C / 成品批 D）"
          style="min-width: 420px" @keyup.enter="takeSample">
        <button @click="takeSample">取样</button>
        <button class="ghost" @click="loadSamples">刷新</button>
        <span class="hint">共 {{ samples.length }} 条样品</span>
      </div>

      <div v-if="lastTake" class="panel">
        <div class="panel-head"><b>本次取样（{{ lastTake.human }}）</b></div>
        <table>
          <thead><tr><th>样品编号</th><th>角色</th><th>状态</th></tr></thead>
          <tbody>
            <tr v-for="s in lastTake.samples" :key="s.id">
              <td>{{ s.sample_no }}</td>
              <td>{{ s.role }}</td>
              <td>{{ s.status }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <table v-if="samples.length">
        <thead>
          <tr><th>样品编号</th><th>角色</th><th>状态</th><th>组</th><th>取样人</th><th>时间</th></tr>
        </thead>
        <tbody>
          <tr v-for="s in samples" :key="s.id">
            <td>{{ s.sample_no }}</td>
            <td>{{ s.role }}</td>
            <td>{{ s.status }}</td>
            <td>{{ s.group_id || '—' }}</td>
            <td>{{ s.sampled_by || '—' }}</td>
            <td>{{ s.sampled_at || '—' }}</td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">尚无样品 —— 扫码后才会产生记录（不预生成）。</p>
    </template>

    <!-- ===== 取样组 ===== -->
    <template v-else>
      <div class="panel">
        <div class="panel-head"><b>新建取样组（并入份样成大样）</b></div>
        <div class="form">
          <label>
            <span>目标类型</span>
            <select v-model="targetType" @change="loadTargets">
              <option v-for="t in TARGETS" :key="t" :value="t">{{ t }}</option>
            </select>
          </label>
          <label>
            <span>目标对象</span>
            <select v-model="targetId" @change="pickTarget">
              <option value="">请选择</option>
              <option v-for="t in targets" :key="t.id" :value="t.id">{{ targetLabel(t) }}</option>
            </select>
          </label>
          <label>
            <span>备注</span>
            <input v-model="groupRemark" placeholder="可空">
          </label>
        </div>

        <template v-if="candidates.length">
          <p class="hint">勾选要并入的<b>份样</b>（已并入的不在此列）：</p>
          <table>
            <thead><tr><th></th><th>样品编号</th><th>状态</th><th>取样人</th></tr></thead>
            <tbody>
              <tr v-for="s in candidates" :key="s.id">
                <td><input type="checkbox" v-model="checked[s.id]"></td>
                <td>{{ s.sample_no }}</td>
                <td>{{ s.status }}</td>
                <td>{{ s.sampled_by || '—' }}</td>
              </tr>
            </tbody>
          </table>
        </template>
        <p v-else-if="targetId" class="hint">该目标下没有未并入的份样 —— 先去「扫码取样」。</p>

        <div class="toolbar">
          <button @click="createGroup">建组并合并为大样</button>
          <span class="hint">检测挂「大样」、不挂袋（批 5 接检测）</span>
        </div>
      </div>

      <div class="toolbar">
        <b>已有取样组（{{ groups.length }}）</b>
        <button class="ghost" @click="loadGroups">刷新</button>
      </div>
      <table v-if="groups.length">
        <thead>
          <tr><th>组号（=大样编号）</th><th>目标</th><th>份样数</th><th>建组人</th><th>时间</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-for="g in groups" :key="g.id">
            <td>{{ g.group_no }}</td>
            <td>{{ g.target_type }} #{{ g.target_id }}</td>
            <td>{{ g.sample_count }}</td>
            <td>{{ g.sampled_by || '—' }}</td>
            <td>{{ g.sampled_at || '—' }}</td>
            <td><button class="ghost" @click="showMembers(g)">成员</button></td>
          </tr>
        </tbody>
      </table>

      <div v-if="membersOf" class="panel">
        <div class="panel-head">
          <b>组 {{ membersOf.group.group_no }} 的成员（{{ membersOf.count }} 个份样）</b>
          <button class="ghost" @click="membersOf = null">关闭</button>
        </div>
        <table>
          <thead><tr><th>样品编号</th><th>角色</th><th>状态</th></tr></thead>
          <tbody>
            <tr v-for="m in membersOf.members" :key="m.id">
              <td>{{ m.sample_no }}</td>
              <td>{{ m.role }}</td>
              <td>{{ m.status }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </section>
</template>
