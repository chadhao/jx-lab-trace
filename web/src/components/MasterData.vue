<script setup>
// 主数据管理页（8 类）：列表 / 新增 / 修改（走版本链，必填原因）/ 停用启用 / 版本链历史。
import { ref, watch, computed, onMounted } from 'vue'
import { api } from '../api.js'

const props = defineProps({ me: { type: Object, default: null } })

// ★ 8 类主数据的字段定义（列名 = spec/schema.sql 的列；后端有白名单校验兜底）
const ENTITIES = [
  {
    key: 'customers', name: '客户', perm: 'md.customer',
    fields: [
      { k: 'code', l: '客户编号(4位)', req: true },
      { k: 'name', l: '名称', req: true },
      { k: 'short_name', l: '简称' },
      { k: 'contact', l: '联系人' },
      { k: 'phone', l: '电话' },
      { k: 'is_internal', l: '集团内(0/1)', t: 'int' },
    ],
    cols: ['code', 'name', 'short_name', 'contact', 'phone', 'status', 'version'],
  },
  {
    key: 'compositions', name: '原料组成', perm: 'md.material',
    fields: [{ k: 'code', l: '编码', req: true }, { k: 'name', l: '名称', req: true }],
    cols: ['code', 'name', 'status', 'version'],
  },
  {
    key: 'material-types', name: '原料类型', perm: 'md.material',
    fields: [{ k: 'code', l: '编码', req: true }, { k: 'name', l: '名称', req: true }],
    cols: ['code', 'name', 'status', 'version'],
  },
  {
    key: 'materials', name: '物料', perm: 'md.material',
    fields: [
      { k: 'code', l: '物料编号(4位)', req: true },
      { k: 'name', l: '名称', req: true },
      { k: 'kind', l: '原料/成品', req: true, opts: ['原料', '成品'] },
      { k: 'composition_id', l: '组成ID', t: 'int' },
      { k: 'material_type_id', l: '类型ID', t: 'int' },
      { k: 'spec', l: '规格' },
      { k: 'unit', l: '单位' },
    ],
    cols: ['code', 'name', 'kind', 'spec', 'unit', 'status', 'version'],
  },
  {
    key: 'test-items', name: '检测项目', perm: 'md.test_item',
    fields: [
      { k: 'code', l: '编码', req: true },
      { k: 'name', l: '名称', req: true },
      { k: 'unit', l: '单位' },
      { k: 'method', l: '检测方法/标准号' },
      { k: 'value_type', l: '值类型', req: true, opts: ['数值', '文本', '枚举'] },
      { k: 'decimals', l: '小数位', t: 'int' },
      { k: 'enum_values', l: '枚举候选(逗号分隔)' },
      { k: 'sort', l: '排序', t: 'int' },
    ],
    cols: ['code', 'name', 'unit', 'method', 'value_type', 'decimals', 'sort', 'status', 'version'],
  },
  {
    key: 'test-item-limits', name: '判定限', perm: 'md.test_item',
    fields: [
      { k: 'test_item_id', l: '检测项ID', req: true, t: 'int' },
      { k: 'customer_id', l: '客户ID(0=通用)', t: 'int' },
      { k: 'material_id', l: '物料ID(0=通用)', t: 'int' },
      { k: 'lower_limit', l: '下限', t: 'num' },
      { k: 'upper_limit', l: '上限', t: 'num' },
      { k: 'is_required', l: '必检(0/1)', t: 'int' },
      { k: 'note', l: '备注' },
    ],
    cols: ['test_item_id', 'customer_id', 'material_id', 'lower_limit', 'upper_limit', 'status', 'version'],
    hint: 'customer_id / material_id = 0 表示通用默认；查限按「客户 × 物料」最具体者优先。',
  },
  {
    key: 'vehicles', name: '车辆', perm: 'md.vehicle',
    fields: [
      { k: 'plate_no', l: '车牌号', req: true },
      { k: 'default_driver', l: '默认司机' },
      { k: 'default_phone', l: '司机电话' },
      { k: 'carrier', l: '承运商' },
      { k: 'note', l: '备注' },
    ],
    cols: ['plate_no', 'default_driver', 'default_phone', 'carrier', 'status', 'version'],
  },
  {
    key: 'teams', name: '班组', perm: 'md.team',
    fields: [{ k: 'code', l: '编码', req: true }, { k: 'name', l: '名称', req: true }],
    cols: ['code', 'name', 'status', 'version'],
    hint: '班组不带版本链（来去用状态表达）。',
  },
]

const cur = ref(ENTITIES[0])
const rows = ref([])
const q = ref('')
const statusFilter = ref('')
const msg = ref('')
const err = ref('')
const loading = ref(false)

// 表单
const showForm = ref(false)
const editing = ref(null) // null = 新增
const form = ref({})
const reason = ref('')
const formErr = ref('')

// 历史
const history = ref(null)

const canWrite = computed(() => {
  if (!props.me) return false
  // 前端只做提示；真正的写保护在后端 RequirePerm(ALL)
  return true
})

async function load() {
  loading.value = true
  err.value = ''
  try {
    const params = new URLSearchParams()
    if (q.value) params.set('q', q.value)
    if (statusFilter.value) params.set('status', statusFilter.value)
    const data = await api.get(`/api/md/${cur.value.key}?${params.toString()}`)
    rows.value = data.rows || []
  } catch (e) {
    rows.value = []
    err.value = e.message
  } finally {
    loading.value = false
  }
}

function pick(ent) {
  cur.value = ent
  history.value = null
  load()
}

function openCreate() {
  editing.value = null
  form.value = {}
  reason.value = ''
  formErr.value = ''
  showForm.value = true
}

function openEdit(row) {
  editing.value = row
  form.value = {}
  for (const f of cur.value.fields) {
    form.value[f.k] = row[f.k] === undefined || row[f.k] === null ? '' : row[f.k]
  }
  reason.value = ''
  formErr.value = ''
  showForm.value = true
}

function coerce(f, v) {
  if (v === '' || v === null || v === undefined) return null
  if (f.t === 'int') return parseInt(v, 10)
  if (f.t === 'num') return parseFloat(v)
  return v
}

async function submit() {
  formErr.value = ''
  const values = {}
  for (const f of cur.value.fields) {
    if (f.req && (form.value[f.k] === '' || form.value[f.k] === null || form.value[f.k] === undefined)) {
      formErr.value = `「${f.l}」必填`
      return
    }
    if (form.value[f.k] !== '' && form.value[f.k] !== null && form.value[f.k] !== undefined) {
      values[f.k] = coerce(f, form.value[f.k])
    }
  }
  try {
    if (editing.value) {
      await api.put(`/api/md/${cur.value.key}/${editing.value.id}`,
        { values, reason: reason.value })
      msg.value = '已产生新版本（原版本保留可查）'
    } else {
      await api.post(`/api/md/${cur.value.key}`, { values })
      msg.value = '新增成功'
    }
    showForm.value = false
    await load()
  } catch (e) {
    formErr.value = e.message
  }
}

async function toggleStatus(row) {
  const next = row.status === '启用' ? '停用' : '启用'
  if (!window.confirm(`确定把「${row.name || row.code || row.plate_no || row.id}」设为 ${next} 吗？`)) return
  try {
    await api.patch(`/api/md/${cur.value.key}/${row.id}`,
      { status: next, reason: `${next}（后台操作）` })
    msg.value = `已${next}`
    await load()
  } catch (e) {
    err.value = e.message
  }
}

async function showHistory(row) {
  try {
    const data = await api.get(`/api/md/${cur.value.key}/${row.id}/history`)
    history.value = { row, chain: data.chain || [] }
  } catch (e) {
    err.value = e.message
  }
}

function cell(row, col) {
  const v = row[col]
  if (v === null || v === undefined) return ''
  if (typeof v === 'object') return JSON.stringify(v)
  return String(v)
}

watch(q, () => load())
onMounted(load)
</script>

<template>
  <section>
    <h2>主数据（8 类）</h2>
    <p class="hint">
      修改业务字段一律走「<b>作废 + 新增</b>」的版本链，且<b>必须填原因</b>；停用是状态流转、不产生新版本。
    </p>

    <div class="tabs">
      <button v-for="e in ENTITIES" :key="e.key"
              :class="{ on: cur.key === e.key }" @click="pick(e)">{{ e.name }}</button>
    </div>

    <p v-if="cur.hint" class="hint">{{ cur.hint }}</p>

    <div class="toolbar">
      <input v-model="q" placeholder="按编号 / 名称搜索" />
      <select v-model="statusFilter" @change="load">
        <option value="">全部状态</option>
        <option value="启用">仅启用</option>
        <option value="停用">仅停用</option>
      </select>
      <button @click="openCreate" :disabled="!canWrite">新增{{ cur.name }}</button>
      <button class="ghost" @click="load">刷新</button>
    </div>

    <p v-if="msg" class="ok">{{ msg }}</p>
    <p v-if="err" class="err">{{ err }}</p>

    <table>
      <thead>
        <tr>
          <th v-for="c in cur.cols" :key="c">{{ c }}</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="r in rows" :key="r.id">
          <td v-for="c in cur.cols" :key="c">{{ cell(r, c) }}</td>
          <td class="ops">
            <button class="ghost" @click="openEdit(r)">修改</button>
            <button class="ghost" @click="showHistory(r)">历史</button>
            <button class="ghost" @click="toggleStatus(r)">
              {{ r.status === '启用' ? '停用' : '启用' }}
            </button>
          </td>
        </tr>
        <tr v-if="!rows.length">
          <td :colspan="cur.cols.length + 1" class="empty">（无数据）</td>
        </tr>
      </tbody>
    </table>

    <!-- 历史版本链 -->
    <div v-if="history" class="panel">
      <div class="panel-head">
        <b>版本链</b>
        <span class="hint">共 {{ history.chain.length }} 版（含已失效版本；只存前向 supersedes_id，反向由反查得出）</span>
        <button class="ghost" @click="history = null">关闭</button>
      </div>
      <table>
        <thead>
          <tr><th>version</th><th>is_current</th><th>supersedes_id</th><th>valid_to</th><th>内容</th></tr>
        </thead>
        <tbody>
          <tr v-for="(c, i) in history.chain" :key="c.id">
            <td>{{ c.version }}</td>
            <td>{{ c.is_current }}</td>
            <td>{{ c.supersedes_id === null || c.supersedes_id === undefined ? '—' : c.supersedes_id }}</td>
            <td>{{ c.valid_to || '—' }}</td>
            <td class="hist">
              <template v-for="f in cur.fields" :key="f.k">
                <span class="kv"><i>{{ f.l }}</i> {{ cell(c, f.k) }}</span>
              </template>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- 新增 / 修改 -->
    <div v-if="showForm" class="panel">
      <div class="panel-head">
        <b>{{ editing ? '修改（生成新版本）' : '新增' + cur.name }}</b>
        <button class="ghost" @click="showForm = false">取消</button>
      </div>
      <div class="form">
        <label v-for="f in cur.fields" :key="f.k">
          <span>{{ f.l }}<em v-if="f.req">*</em></span>
          <select v-if="f.opts" v-model="form[f.k]">
            <option v-for="o in f.opts" :key="o" :value="o">{{ o }}</option>
          </select>
          <input v-else v-model="form[f.k]" :placeholder="f.l" />
        </label>
        <label v-if="editing" class="full">
          <span>更正原因<em>*</em></span>
          <input v-model="reason" placeholder="必填：为什么改（落审计 + 版本链留痕）" />
        </label>
        <p v-if="formErr" class="err full">{{ formErr }}</p>
        <div class="full">
          <button @click="submit">保存</button>
          <button class="ghost" @click="showForm = false">取消</button>
        </div>
      </div>
    </div>
  </section>
</template>
