<script setup>
// 报告页（M9 · D6）：生成 · 列表 · 复制链接 · 刷新 · 撤销 · 设有效期 · 过期清扫 ·
// 访问日志查看与同步。
// ★ 动作按 /api/report/perm-summary 显隐（服务端仍是唯一权威）；
//   生成/刷新 = report.generate，生命周期与同步 = report.share.manage，读 = rpt.view。
// ★★ 页面不得出现对外禁用字样（D20：对外不披露；内部档案页照旧标注，见追溯页）。
import { ref, onMounted } from 'vue'
import { api } from '../api.js'

defineProps({ me: { type: Object, default: null } })

const err = ref('')
const ok = ref('')
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

// ================= 生成 =================
const batches = ref([])
const form = ref({ batch_id: '', title: '' })

async function loadBatches() {
  try {
    const data = await api.get('/api/prod/batches')
    batches.value = data.rows || []
  } catch (e) {
    batches.value = [] // 无生产批读权限时退化为手填 id
  }
}

async function doGenerate() {
  err.value = ''
  ok.value = ''
  if (!form.value.batch_id) {
    err.value = '请选择 / 填写生产批'
    return
  }
  try {
    const body = {
      scope_type: '按批次',
      scope: { batch_id: Number(form.value.batch_id) },
      title: form.value.title || '',
    }
    const data = await api.post('/api/report/generate', body)
    ok.value = '已生成 ' + (data.row && data.row.report_no)
    await loadList()
  } catch (e) {
    err.value = e.message
  }
}

// ================= 列表 =================
const rows = ref([])
const statusFilter = ref('')

async function loadList() {
  err.value = ''
  try {
    const q = statusFilter.value ? '?status=' + encodeURIComponent(statusFilter.value) : ''
    const data = await api.get('/api/report/list' + q)
    rows.value = data.rows || []
  } catch (e) {
    err.value = e.message
  }
}

async function copyLink(url) {
  try {
    await navigator.clipboard.writeText(url)
    ok.value = '链接已复制：' + url
  } catch (e) {
    ok.value = '复制失败（请手动复制）：' + url
  }
}

// ================= 刷新（新 token 新链接）=================
async function doRefresh(row) {
  err.value = ''
  ok.value = ''
  if (!window.confirm('刷新将生成「新链接」，旧链接立即失效（旧快照移出可服务目录）。确定刷新？')) {
    return
  }
  try {
    const data = await api.post('/api/report/' + row.id + '/refresh', {})
    ok.value = '已刷新：新编号 ' + (data.row && data.row.report_no) +
      '（旧 ' + data.superseded_report_no + ' 已失效）'
    await loadList()
  } catch (e) {
    err.value = e.message
  }
}

// ================= 撤销（单步生效，reason 必填）=================
async function doRevoke(row) {
  err.value = ''
  ok.value = ''
  const reason = window.prompt('撤销原因（必填）：撤销后旧链接立即不可访问。')
  if (reason === null) return
  if (!reason.trim()) {
    err.value = '撤销必须填写原因'
    return
  }
  try {
    await api.post('/api/report/' + row.id + '/revoke', { reason: reason.trim() })
    ok.value = '已撤销 ' + row.report_no
    await loadList()
  } catch (e) {
    err.value = e.message
  }
}

// ================= 设有效期 =================
async function doExpires(row) {
  err.value = ''
  ok.value = ''
  const cur = row.expires_at ? String(row.expires_at).slice(0, 10) : ''
  const v = window.prompt('有效期至（到期日，含当日，格式 YYYY-MM-DD）：', cur)
  if (v === null) return
  try {
    await api.post('/api/report/' + row.id + '/expires', { expires_at: v.trim() })
    ok.value = '有效期已更新'
    await loadList()
  } catch (e) {
    err.value = e.message
  }
}

// ================= 过期清扫 / 访问日志同步 =================
async function doSweep() {
  err.value = ''
  ok.value = ''
  try {
    const data = await api.post('/api/report/expire/sweep', {})
    ok.value = '清扫完成：' + data.swept + ' 条' +
      ((data.report_nos || []).length ? '（' + data.report_nos.join('、') + '）' : '')
    await loadList()
  } catch (e) {
    err.value = e.message
  }
}

async function doSync() {
  err.value = ''
  ok.value = ''
  try {
    const data = await api.post('/api/report/access/sync', {})
    ok.value = '访问日志同步：解析 ' + data.parsed + ' · 入库 ' + data.inserted +
      ' · 跳过 ' + data.skipped + ' · 偏移 ' + data.offset
    if (accessOf.value) await loadAccess(accessOf.value)
  } catch (e) {
    err.value = e.message
  }
}

// ================= 访问记录 =================
const accessOf = ref(null) // 当前查看的报告 id
const accessRows = ref([])

async function loadAccess(id) {
  err.value = ''
  accessOf.value = id
  try {
    const data = await api.get('/api/report/' + id + '/access')
    accessRows.value = data.rows || []
  } catch (e) {
    err.value = e.message
  }
}

function fmtTime(v) {
  if (!v) return '—'
  return String(v).replace('T', ' ').slice(0, 19)
}

onMounted(() => {
  loadPerms()
  loadList()
  loadBatches()
})
</script>

<template>
  <section>
    <h2>报告（对外静态快照 · 唯一链接 · 可撤销）</h2>
    <p class="hint">
      ★ 按批次生成自包含静态快照，链接可转发（长随机 token、非零有效期、访问留痕）；
      ★ 刷新 = 新链接，旧链接立即失效；★ 撤销 / 过期后链接不可访问（快照移出可服务目录）。
    </p>
    <p v-if="err" class="err">{{ err }}</p>
    <p v-if="ok" class="ok">{{ ok }}</p>

    <!-- ============ 生成 ============ -->
    <div class="panel" v-if="can('report.generate') || can('rpt.view')">
      <div class="panel-head"><b>生成报告</b></div>
      <div class="toolbar">
        <label v-if="batches.length">生产批
          <select v-model="form.batch_id" style="min-width: 22rem">
            <option disabled value="">请选择…</option>
            <option v-for="b in batches" :key="b.id" :value="b.id">
              {{ b.human || b.code }}（#{{ b.id }}）
            </option>
          </select>
        </label>
        <label v-else>生产批 id
          <input v-model="form.batch_id" placeholder="如 3" style="width: 8rem">
        </label>
        <label>标题（可选）
          <input v-model="form.title" placeholder="如 检验报告" style="width: 16rem">
        </label>
        <button v-if="can('report.generate')" @click="doGenerate">生成</button>
        <span v-else class="hint">（无 report.generate 权限，仅可查看）</span>
      </div>
    </div>

    <!-- ============ 列表 ============ -->
    <div class="panel">
      <div class="panel-head">
        <b>报告列表</b>
        <span class="hint">
          <select v-model="statusFilter" @change="loadList">
            <option value="">全部状态</option>
            <option value="有效">有效</option>
            <option value="已撤销">已撤销</option>
            <option value="已过期">已过期</option>
          </select>
        </span>
        <span class="ops">
          <button v-if="can('report.share.manage')" class="ghost" @click="doSweep">过期清扫</button>
          <button v-if="can('report.share.manage')" class="ghost" @click="doSync">同步访问日志</button>
          <button class="ghost" @click="loadList">刷新列表</button>
        </span>
      </div>

      <table v-if="rows.length">
        <thead>
          <tr>
            <th>报告编号</th><th>批次</th><th>状态</th><th>有效期至</th><th>链接</th><th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in rows" :key="r.id">
            <td class="mono">{{ r.report_no }}</td>
            <td class="mono">{{ r.batch_human || ('#' + (r.scope && r.scope.batch_id)) }}</td>
            <td>
              <span class="tag" :style="r.status === '有效' ? '' : 'color:#d93025;font-weight:700'">
                {{ r.status }}
              </span>
            </td>
            <td>{{ fmtTime(r.expires_at) }}</td>
            <td>
              <a :href="r.url" target="_blank" rel="noopener" class="mono">{{ r.url }}</a>
              <button class="ghost" @click="copyLink(r.url)">复制链接</button>
            </td>
            <td class="ops">
              <button v-if="can('report.generate')" class="ghost" @click="doRefresh(r)"
                title="生成新链接，旧链接失效">刷新</button>
              <button v-if="can('report.share.manage')" class="ghost" @click="doExpires(r)">设有效期</button>
              <button v-if="can('report.share.manage') && r.status === '有效'" class="ghost"
                @click="doRevoke(r)">撤销</button>
              <button v-if="canRead('rpt.view')" class="ghost" @click="loadAccess(r.id)">访问记录</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">暂无报告。</p>
    </div>

    <!-- ============ 访问记录 ============ -->
    <div class="panel" v-if="accessOf">
      <div class="panel-head">
        <b>访问记录 · 报告 #{{ accessOf }}</b>
        <span class="ops"><button class="ghost" @click="accessOf = null">关闭</button></span>
      </div>
      <table v-if="accessRows.length">
        <thead><tr><th>时间</th><th>IP</th><th>User-Agent</th></tr></thead>
        <tbody>
          <tr v-for="a in accessRows" :key="a.id">
            <td>{{ fmtTime(a.accessed_at) }}</td>
            <td class="mono">{{ a.ip || '—' }}</td>
            <td>{{ a.ua || '—' }}</td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">暂无访问记录（可先点「同步访问日志」）。</p>
    </div>
  </section>
</template>
