<script setup>
// 检测单页（M5 · D2~D5/D7）：清单（三态）· 逐项录值 · 附件上传（显示上限）·
// 结论/处置 · 让步四字段 + 双签两个动作 · 复检 / 修正入口。
// ★ 动作按 perm-summary 显隐；服务端仍是唯一权威（隐藏 ≠ 放行）。
import { ref, onMounted, computed } from 'vue'
import { api } from '../api.js'

const props = defineProps({
  me: { type: Object, default: null },
  inspectionId: { type: Number, required: true },
})
const emit = defineEmits(['back', 'open', 'opened'])

const msg = ref('')
const err = ref('')
const insp = ref(null)
const items = ref([])
const todo = ref(0)
const files = ref([])
const maxMB = ref(20)

// ---- 权限摘要 ----
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

// ---- 加载 ----
async function loadInsp() {
  err.value = ''
  try {
    const data = await api.get(`/api/insp/${props.inspectionId}`)
    insp.value = data.row
    emit('opened', data.row)
  } catch (e) {
    insp.value = null
    err.value = '加载检测单失败：' + e.message
  }
}
async function loadItems() {
  try {
    const data = await api.get(`/api/insp/${props.inspectionId}/items`)
    items.value = data.rows || []
    todo.value = data.todo || 0
  } catch (e) {
    err.value = '加载清单失败：' + e.message
  }
}
async function loadFiles() {
  try {
    const data = await api.get(`/api/insp/${props.inspectionId}/files`)
    files.value = data.rows || []
    maxMB.value = data.max_mb || 20
  } catch (e) {
    err.value = '加载附件失败：' + e.message
  }
}
async function refresh() {
  await Promise.all([loadInsp(), loadItems(), loadFiles()])
}

// ---- 加项 / 删项（insp.scope.edit） ----
const adding = ref(false)
const candidates = ref([])
const checked = ref({})

async function openAdd() {
  adding.value = true
  err.value = ''
  const inList = new Set(items.value.map((r) => r.item_id))
  try {
    const data = await api.get('/api/md/test-items?status=' + encodeURIComponent('启用'))
    candidates.value = (data.rows || []).filter((r) => !inList.has(Number(r.id)))
    checked.value = {}
  } catch (e) {
    candidates.value = []
    err.value = '加载检测项字典失败：' + e.message
  }
}

async function addItems() {
  err.value = ''
  const ids = Object.keys(checked.value).filter((k) => checked.value[k]).map(Number)
  if (!ids.length) { err.value = '请勾选要加入的检测项'; return }
  try {
    await api.post(`/api/insp/${props.inspectionId}/items`, { item_ids: ids })
    msg.value = `已加 ${ids.length} 项`
    adding.value = false
    await loadItems()
  } catch (e) {
    err.value = '加项失败：' + e.message
  }
}

async function removeItem(row) {
  if (!confirm(`删除未测项「${row.item_name}」？（物理删除，审计留痕）`)) return
  err.value = ''
  try {
    await api.del(`/api/insp/${props.inspectionId}/items/${row.item_id}`)
    msg.value = '已删除该项'
    await loadItems()
  } catch (e) {
    err.value = '删项失败：' + e.message
  }
}

// ---- 录值（三态，insp.result.entry） ----
const entry = ref(null) // { item_id, state, value, unit, remark }

function openEntry(row) {
  entry.value = {
    item_id: row.item_id,
    item_name: row.item_name,
    value_type: row.value_type,
    state: '已测',
    value: row.value_num != null ? String(row.value_num) : (row.value_text || ''),
    unit: row.row_unit || row.unit || '',
    remark: row.remark || '',
  }
}

async function saveEntry() {
  err.value = ''
  const e0 = entry.value
  const body = { state: e0.state, unit: e0.unit, remark: e0.remark }
  if (e0.state === '已测') {
    if (e0.value === '' || e0.value == null) { err.value = '已测必须录入值'; return }
    if (e0.value_type === '数值') {
      const n = Number(e0.value)
      if (Number.isNaN(n)) { err.value = '数值项须填数字'; return }
      body.value_num = n
    } else {
      body.value_text = e0.value
    }
  }
  try {
    await api.patch(`/api/insp/${props.inspectionId}/items/${e0.item_id}`, body)
    msg.value = `「${e0.item_name}」已录为 ${e0.state}`
    entry.value = null
    await loadItems()
  } catch (err2) {
    err.value = '录入失败：' + err2.message
  }
}

// ---- 附件（insp.file.upload） ----
const fileInput = ref(null)
const uploading = ref(false)

async function uploadFile() {
  const f = fileInput.value && fileInput.value.files && fileInput.value.files[0]
  if (!f) { err.value = '请先选择文件'; return }
  err.value = ''
  msg.value = ''
  uploading.value = true
  try {
    const fd = new FormData()
    fd.append('file', f)
    const data = await api.upload(`/api/insp/${props.inspectionId}/files`, fd)
    msg.value = `附件已上传（${(data.size / 1024 / 1024).toFixed(2)} MB，上限 ${maxMB.value}MB）`
    fileInput.value.value = ''
    await loadFiles()
  } catch (e) {
    err.value = '上传失败：' + e.message
  } finally {
    uploading.value = false
  }
}

// ---- 结论 / 处置 ----
const conc = ref({ conclusion: '', defect_desc: '', remark: '' })
// 让步四字段（CONCESSION 必填）
const cons = ref({ authorized_by: '', cust_notified_at: '', cust_contact: '', cust_channel: '' })
const CHANNELS = ['电话', '微信', '邮件', '书面']

async function saveConclusion() {
  err.value = ''
  const body = {
    conclusion: conc.value.conclusion,
    defect_desc: conc.value.defect_desc,
    remark: conc.value.remark,
  }
  if (conc.value.conclusion === 'CONCESSION') {
    Object.assign(body, cons.value)
  }
  try {
    await api.post(`/api/insp/${props.inspectionId}/conclusion`, body)
    msg.value = '结论已保存'
    conc.value = { conclusion: '', defect_desc: '', remark: '' }
    await refresh()
  } catch (e) {
    err.value = '出结论失败：' + e.message
  }
}

const disp = ref('')

async function saveDisposition() {
  err.value = ''
  if (!disp.value) { err.value = '请选择处置'; return }
  try {
    await api.post(`/api/insp/${props.inspectionId}/disposition`, { disposition: disp.value })
    msg.value = '处置已保存'
    disp.value = ''
    await refresh()
  } catch (e) {
    err.value = '填处置失败：' + e.message
  }
}

// ---- 双签（两个权限点两个入口） ----
async function sign(who) {
  err.value = ''
  try {
    await api.post(`/api/insp/${props.inspectionId}/concession/${who}-sign`, {})
    msg.value = who === 'qc' ? '质检方已签署' : '使用部门已签署'
    await refresh()
  } catch (e) {
    err.value = '签署失败：' + e.message
  }
}

// ---- 复检 / 修正 ----
const correcting = ref(false)
const correctReason = ref('')

async function doRecheck() {
  if (!confirm('发起复检？将新开一张检测单，原单结果一字不动。')) return
  err.value = ''
  try {
    const data = await api.post(`/api/insp/${props.inspectionId}/recheck`, {})
    msg.value = `复检单已开：${data.row.inspection_no}`
    emit('open', data.row.id)
  } catch (e) {
    err.value = '复检失败：' + e.message
  }
}

async function doCorrect() {
  err.value = ''
  if (!correctReason.value.trim()) { err.value = '修正必须填写原因'; return }
  try {
    const data = await api.post(`/api/insp/${props.inspectionId}/correct`, { reason: correctReason.value })
    msg.value = `修正单已开：${data.row.inspection_no}（原单已作废）`
    correcting.value = false
    correctReason.value = ''
    emit('open', data.row.id)
  } catch (e) {
    err.value = '修正失败：' + e.message
  }
}

const isCons = computed(() => insp.value && insp.value.conclusion === 'CONCESSION')

onMounted(refresh)
</script>

<template>
  <section>
    <div class="toolbar">
      <button class="ghost" @click="emit('back')">← 返回任务列表</button>
      <template v-if="insp">
        <b>{{ insp.inspection_no }}</b>
        <span class="tag">{{ insp.target_type }} #{{ insp.target_id }}</span>
        <span v-if="insp.group_no" class="tag">大样 {{ insp.group_no }}</span>
        <span v-if="insp.is_recheck" class="tag">复检（源 #{{ insp.recheck_of }}）</span>
        <span v-if="insp.voided" class="tag">已作废</span>
      </template>
    </div>

    <p v-if="msg" class="ok">{{ msg }}</p>
    <p v-if="err" class="err">{{ err }}</p>
    <p v-if="!insp" class="hint">加载中…</p>

    <template v-if="insp">
      <!-- ===== 清单（三态） ===== -->
      <div class="panel">
        <div class="panel-head">
          <b>批次检测项清单（{{ items.length }} 项 · 待办 {{ todo }}）</b>
          <span class="hint">三态：未测（待办）/ 已测 / 不适用 —— 不可合并</span>
          <button v-if="can('insp.scope.edit') && !insp.voided" @click="openAdd">加项</button>
        </div>

        <div v-if="adding" class="panel">
          <div class="panel-head"><b>加入检测项</b>
            <button class="ghost" @click="adding = false">取消</button>
          </div>
          <p v-if="!candidates.length" class="hint">没有可加入的启用检测项。</p>
          <table v-else>
            <thead><tr><th></th><th>编码</th><th>名称</th><th>类型</th><th>单位</th></tr></thead>
            <tbody>
              <tr v-for="c in candidates" :key="c.id">
                <td><input type="checkbox" v-model="checked[c.id]"></td>
                <td>{{ c.code }}</td><td>{{ c.name }}</td>
                <td>{{ c.value_type }}</td><td>{{ c.unit || '—' }}</td>
              </tr>
            </tbody>
          </table>
          <button @click="addItems">确认加项</button>
        </div>

        <table v-if="items.length">
          <thead>
            <tr>
              <th>编码</th><th>名称</th><th>状态</th><th>值</th><th>判定</th>
              <th>判定限</th><th>备注</th><th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in items" :key="r.id">
              <td>{{ r.item_code }}</td>
              <td>{{ r.item_name }}</td>
              <td><span class="tag">{{ r.state }}</span></td>
              <td>
                <template v-if="r.state === '已测'">
                  {{ r.value_num != null ? r.value_num : r.value_text }} {{ r.row_unit || r.unit }}
                </template>
                <template v-else>—</template>
              </td>
              <td>{{ r.judge || '—' }}</td>
              <td>
                <template v-if="r.lower_limit != null || r.upper_limit != null">
                  {{ r.lower_limit != null ? r.lower_limit : '' }} ~ {{ r.upper_limit != null ? r.upper_limit : '' }}
                </template>
                <template v-else>—</template>
              </td>
              <td>{{ r.remark || '—' }}</td>
              <td class="ops">
                <template v-if="!insp.voided">
                  <button v-if="can('insp.result.entry') && r.state === '未测'" @click="openEntry(r)">录值</button>
                  <button v-if="can('insp.scope.edit') && r.state === '未测'" class="ghost"
                    @click="removeItem(r)">删项</button>
                </template>
              </td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">清单为空 —— 先「加项」。</p>

        <!-- 录值表单 -->
        <div v-if="entry" class="panel">
          <div class="panel-head">
            <b>录值 · {{ entry.item_name }}</b>
            <button class="ghost" @click="entry = null">取消</button>
          </div>
          <div class="form">
            <label>
              <span>状态</span>
              <select v-model="entry.state">
                <option value="已测">已测</option>
                <option value="不适用">不适用</option>
              </select>
            </label>
            <label v-if="entry.state === '已测'">
              <span>值<em v-if="entry.value_type === '数值'">*</em></span>
              <input v-model="entry.value"
                :placeholder="entry.value_type === '数值' ? '数值' : '文本'">
            </label>
            <label>
              <span>单位</span>
              <input v-model="entry.unit" placeholder="可空">
            </label>
            <label class="full">
              <span>备注</span>
              <input v-model="entry.remark" placeholder="可空">
            </label>
          </div>
          <p class="hint">★ 只允许「未测 → 已测 / 不适用」单向一次；已测改值须走修正。</p>
          <button @click="saveEntry">保存</button>
        </div>
      </div>

      <!-- ===== 附件 ===== -->
      <div class="panel">
        <div class="panel-head">
          <b>附件（{{ files.length }}）</b>
          <span class="hint">单文件上限 {{ maxMB }}MB · 超限先拒后写 · 库中只存路径</span>
        </div>
        <div class="toolbar" v-if="can('insp.file.upload') && !insp.voided">
          <input ref="fileInput" type="file">
          <button :disabled="uploading" @click="uploadFile">
            {{ uploading ? '上传中…' : '上传' }}
          </button>
        </div>
        <table v-if="files.length">
          <thead><tr><th>文件名</th><th>类型</th><th>大小</th><th>上传人</th><th>时间</th></tr></thead>
          <tbody>
            <tr v-for="f in files" :key="f.id">
              <td>{{ f.name }}</td>
              <td>{{ f.file_type || '—' }}</td>
              <td>{{ (f.size / 1024 / 1024).toFixed(2) }} MB</td>
              <td>{{ f.created_by || '—' }}</td>
              <td>{{ f.created_at || '—' }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">暂无附件。</p>
      </div>

      <!-- ===== 结论 ===== -->
      <div class="panel">
        <div class="panel-head">
          <b>结论 / 处置</b>
          <span class="hint">
            当前结论：{{ insp.conclusion ? (isCons ? '让步接收（CONCESSION）' : insp.conclusion) : '未出' }}
            <template v-if="insp.disposition"> · 处置：{{ insp.disposition }}</template>
          </span>
        </div>

        <template v-if="!insp.conclusion && can('insp.conclusion') && !insp.voided">
          <div class="form">
            <label>
              <span>结论<em>*</em></span>
              <select v-model="conc.conclusion">
                <option value="">请选择</option>
                <option value="合格">合格</option>
                <option value="不合格">不合格</option>
                <option value="CONCESSION">让步接收（CONCESSION）</option>
              </select>
            </label>
            <label v-if="conc.conclusion === '不合格'">
              <span>缺陷描述</span>
              <input v-model="conc.defect_desc">
            </label>
            <label class="full">
              <span>备注</span>
              <input v-model="conc.remark">
            </label>
          </div>

          <!-- 让步四字段（必填） -->
          <template v-if="conc.conclusion === 'CONCESSION'">
            <p class="hint">★ 让步接收四字段必填 + 之后双签（质检方 / 使用部门）。</p>
            <div class="form">
              <label><span>授权人<em>*</em></span>
                <input v-model="cons.authorized_by"></label>
              <label><span>何时告知客户<em>*</em></span>
                <input v-model="cons.cust_notified_at" placeholder="YYYY-MM-DD HH:MM:SS"></label>
              <label><span>告知谁<em>*</em></span>
                <input v-model="cons.cust_contact"></label>
              <label><span>渠道<em>*</em></span>
                <select v-model="cons.cust_channel">
                  <option value="">请选择</option>
                  <option v-for="c in CHANNELS" :key="c" :value="c">{{ c }}</option>
                </select></label>
            </div>
          </template>
          <button @click="saveConclusion">保存结论</button>
        </template>

        <template v-if="insp.conclusion && !insp.disposition && can('insp.disposition') && !insp.voided">
          <div class="form">
            <label>
              <span>处置<em>*</em></span>
              <select v-model="disp">
                <option value="">请选择</option>
                <template v-if="insp.conclusion === '不合格'">
                  <option value="退货">退货</option>
                  <option value="换货">换货</option>
                  <option value="返工">返工</option>
                </template>
                <template v-else-if="isCons">
                  <option value="让步接收">让步接收</option>
                </template>
              </select>
            </label>
          </div>
          <p class="hint">合格不得填处置；不合格必填；CONCESSION ⇒ 必须「让步接收」。</p>
          <button @click="saveDisposition">保存处置</button>
        </template>

        <!-- 双签：两个权限点 = 两个入口 -->
        <template v-if="isCons">
          <div class="toolbar">
            <span class="hint">双签：</span>
            <button v-if="can('insp.concession.qc_sign') && !insp.qc_signed_by"
              @click="sign('qc')">质检方签署</button>
            <span v-else-if="insp.qc_signed_by" class="tag">质检 {{ insp.qc_signed_by }} 已签</span>
            <button v-if="can('insp.concession.dept_sign') && !insp.dept_signed_by"
              @click="sign('dept')">使用部门签署</button>
            <span v-else-if="insp.dept_signed_by" class="tag">部门 {{ insp.dept_signed_by }} 已签</span>
          </div>
        </template>
      </div>

      <!-- ===== 复检 / 修正 ===== -->
      <div class="panel" v-if="!insp.voided">
        <div class="panel-head">
          <b>复检 / 修正</b>
          <span class="hint">复检新开单（原单不动）；修正作废原单 + 新开单（原因必填）</span>
        </div>
        <div class="toolbar">
          <button v-if="can('insp.scope.edit')" @click="doRecheck">复检</button>
          <button v-if="can('insp.result.correct')" class="ghost"
            @click="correcting = !correcting">修正</button>
        </div>
        <div v-if="correcting" class="toolbar">
          <input v-model="correctReason" placeholder="修正原因（必填）" style="min-width: 20rem">
          <button @click="doCorrect">确认修正</button>
        </div>
      </div>
    </template>
  </section>
</template>
