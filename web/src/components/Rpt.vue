<script setup>
// 报表页（M10 · D6）：5 张基础报表 + 通用筛选 + 导出 CSV。
// ★ 只读展示；查询挂 rpt.view（LevelRead），导出挂 rpt.export（LevelAll）。
// ★★ 空数据两态文案必须不同（后端已给 note，这里**原样显示**）：
//   「数据未接入」（数据源整体无数据）≠「本条件下无记录」（筛选窗口无命中）。
// ★ 不合格用红字（不以绿表示不合格）；一期只用表格 + CSS，不引图表库。
import { ref, onMounted, computed } from 'vue'
import { api } from '../api.js'

defineProps({ me: { type: Object, default: null } })

const err = ref('')
const perms = ref({})
const can = (code) => {
  const v = perms.value[code]
  return !!v && v !== 'NONE'
}
const canRead = (code) => {
  const v = perms.value[code]
  return !!v && (v.includes('READ') || v.includes('ALL'))
}

async function loadPerms() {
  try {
    const data = await api.get('/api/report/perm-summary')
    perms.value = data.points || {}
  } catch (e) {
    err.value = '加载权限摘要失败：' + e.message
  }
}

const TABS = [
  { key: 'quality-trend', name: '质量趋势' },
  { key: 'customer-recon', name: '客户对账' },
  { key: 'output-yield', name: '产量与合格率' },
  { key: 'sample-expiry', name: '留样到期' },
  { key: 'nonconform-stat', name: '不合格统计' },
]
const tab = ref('quality-trend')
const filters = ref({ from: '', to: '', customer_id: '', bucket: 'month' })
const result = ref(null)
const loading = ref(false)

const customers = ref([])
async function loadCustomers() {
  try {
    const data = await api.get('/api/md/customers')
    customers.value = data.rows || []
  } catch (e) {
    customers.value = [] // 无主数据读权限 ⇒ 退化为手填客户 id
  }
}

function query() {
  const q = []
  if (filters.value.from) q.push('from=' + encodeURIComponent(filters.value.from))
  if (filters.value.to) q.push('to=' + encodeURIComponent(filters.value.to))
  if (filters.value.customer_id) q.push('customer_id=' + encodeURIComponent(filters.value.customer_id))
  if (tab.value === 'quality-trend' && filters.value.bucket) {
    q.push('bucket=' + encodeURIComponent(filters.value.bucket))
  }
  return q.length ? '?' + q.join('&') : ''
}

async function run() {
  err.value = ''
  result.value = null
  if (!canRead('rpt.view')) {
    err.value = '无查看报表权限（rpt.view）'
    return
  }
  loading.value = true
  try {
    result.value = await api.get('/api/rpt/' + tab.value + query())
  } catch (e) {
    err.value = e.message
  } finally {
    loading.value = false
  }
}

function switchTab(key) {
  tab.value = key
  result.value = null
  run()
}

// 单元格渲染：不合格红字，null 显示 —
const isBad = (v) => v === '不合格'

function cellText(v) {
  if (v === null || v === undefined) return '—'
  if (typeof v === 'boolean') return v ? '是' : '否'
  return String(v)
}

// CSS 条形（占比列，纯 CSS 不引图表库）
const maxCnt = computed(() => {
  if (!result.value || !result.value.rows) return 0
  let m = 0
  for (const r of result.value.rows) {
    const n = Number(r.cnt)
    if (!Number.isNaN(n) && n > m) m = n
  }
  return m
})

// ================= 导出 CSV（★ 只写审计，不写业务表）=================
const exporting = ref(false)
async function doExport() {
  err.value = ''
  if (!can('rpt.export')) {
    err.value = '无导出权限（rpt.export）'
    return
  }
  exporting.value = true
  try {
    const body = {
      name: tab.value,
      filters: {
        from: filters.value.from || '',
        to: filters.value.to || '',
        customer_id: filters.value.customer_id ? Number(filters.value.customer_id) : 0,
        bucket: filters.value.bucket || '',
      },
    }
    const resp = await fetch('/api/rpt/export', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    if (!resp.ok) {
      let msg = 'HTTP ' + resp.status
      try { const j = await resp.json(); if (j && j.error) msg = j.error } catch (e) { /* 忽略 */ }
      throw new Error(msg)
    }
    const blob = await resp.blob()
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = tab.value + '.csv'
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    URL.revokeObjectURL(a.href)
  } catch (e) {
    err.value = '导出失败：' + e.message
  } finally {
    exporting.value = false
  }
}

onMounted(() => {
  loadPerms()
  loadCustomers()
  run()
})
</script>

<template>
  <section>
    <h2>报表（只读 · 5 张基础报表）</h2>
    <p class="hint">
      ★ 通用筛选：时间窗（起 / 止）与客户；★ 质量趋势按月 / 周分桶；
      ★ 空数据两态：<b>数据未接入</b>（数据源整体无数据）与 <b>本条件下无记录</b>（筛选无命中）含义不同，文案不同。
    </p>
    <p v-if="err" class="err">{{ err }}</p>

    <!-- ============ tabs ============ -->
    <div class="toolbar">
      <button v-for="t in TABS" :key="t.key" :class="{ on: tab === t.key }" @click="switchTab(t.key)">
        {{ t.name }}
      </button>
    </div>

    <!-- ============ 筛选 ============ -->
    <div class="panel">
      <div class="panel-head"><b>筛选</b></div>
      <div class="toolbar">
        <label>起 <input v-model="filters.from" placeholder="YYYY-MM-DD" style="width: 9rem"></label>
        <label>止 <input v-model="filters.to" placeholder="YYYY-MM-DD" style="width: 9rem"></label>
        <label v-if="customers.length">客户
          <select v-model="filters.customer_id" style="min-width: 14rem">
            <option value="">全部客户</option>
            <option v-for="c in customers" :key="c.id" :value="String(c.id)">{{ c.name }}</option>
          </select>
        </label>
        <label v-else>客户 id <input v-model="filters.customer_id" placeholder="留空=全部" style="width: 8rem"></label>
        <label v-if="tab === 'quality-trend'">分桶
          <select v-model="filters.bucket">
            <option value="month">按月</option>
            <option value="week">按周</option>
          </select>
        </label>
        <button :disabled="loading" @click="run">{{ loading ? '查询中…' : '查询' }}</button>
        <button v-if="can('rpt.export')" :disabled="exporting" @click="doExport">
          {{ exporting ? '导出中…' : '导出 CSV' }}
        </button>
        <span v-else class="hint">（无 rpt.export 权限）</span>
      </div>
    </div>

    <!-- ============ 结果 ============ -->
    <div class="panel" v-if="result">
      <div class="panel-head">
        <b>{{ result.title || result.name }}</b>
        <span class="hint">数据状态：{{ result.has_data ? '有数据源' : '无数据源' }}</span>
      </div>

      <!-- ★★ 两态文案：各自不同，原样显示后端 note -->
      <p v-if="result.note === '数据未接入'" class="err" style="font-weight:700">
        数据未接入：该报表的数据源整体还没有任何数据（这是「没接数据」，不是 0）。
      </p>
      <p v-else-if="result.note === '本条件下无记录'" class="hint" style="font-weight:700">
        本条件下无记录：数据源有数据，但当前筛选窗口没有匹配行。
      </p>

      <table v-if="result.rows && result.rows.length">
        <thead>
          <tr>
            <th v-for="c in result.columns" :key="c.key">{{ c.label }}</th>
            <th v-if="result.name === 'nonconform-stat'">分布</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(row, i) in result.rows" :key="i">
            <td v-for="c in result.columns" :key="c.key"
              :style="isBad(row[c.key]) ? 'color:#d93025;font-weight:700' : ''">
              {{ cellText(row[c.key]) }}
            </td>
            <td v-if="result.name === 'nonconform-stat'">
              <span class="bar" :style="{
                display: 'inline-block', height: '10px', borderRadius: '3px',
                background: '#d93025', verticalAlign: 'middle',
                width: (maxCnt ? Math.max(4, Math.round(Number(row.cnt) / maxCnt * 120)) : 4) + 'px',
              }"></span>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else-if="result.note !== '数据未接入' && result.note !== '本条件下无记录'" class="hint">无记录。</p>
    </div>
  </section>
</template>
