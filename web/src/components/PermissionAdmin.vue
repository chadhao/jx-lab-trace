<script setup>
// 权限配置页（M2）：矩阵编辑 + 角色管理 + 用户绑定 + 变更记录。
//
// ★ 防锁死① 在这里落地：sysadmin × 管理域的格子按后端返回的 locked 置灰不可编辑。
//	防锁死②（后端同步拒绝）由接口保证 —— 绕过本页直接调接口会被 403。
import { ref, computed, onMounted } from 'vue'
import { api } from '../api.js'

const tab = ref('matrix')
const matrix = ref({ points: [], roles: [], cells: [] })
const original = ref({})
const draft = ref({})
const msg = ref('')
const err = ref('')
const collapsed = ref({})
const roles = ref([])
const bindings = ref([])
const auditRows = ref([])

const LEVELS = ['ALL', 'READ', 'INIT', 'APPROVE', 'NONE']

// 按模块分组（保持后端 sort 顺序）
const groups = computed(() => {
  const out = []
  const byModule = new Map()
  for (const p of matrix.value.points) {
    if (!byModule.has(p.module)) {
      byModule.set(p.module, [])
      out.push(p.module)
    }
    byModule.get(p.module).push(p)
  }
  return out.map((m) => ({ module: m, points: byModule.get(m) }))
})

const cellKey = (r, p) => `${r}|${p}`

function levelOf(role, point) {
  const k = cellKey(role, point)
  return draft.value[k] !== undefined ? draft.value[k] : (original.value[k] || 'NONE')
}

function lockedOf(role, point) {
  const c = matrix.value.cells.find((x) => x.role_code === role && x.point_code === point)
  return !!(c && c.locked)
}

function setLevel(role, point, lv) {
  draft.value[cellKey(role, point)] = lv
}

const changes = computed(() => {
  const out = []
  for (const [k, v] of Object.entries(draft.value)) {
    if (original.value[k] !== undefined && original.value[k] !== v) {
      const [role, point] = k.split('|')
      out.push({ role_code: role, point_code: point, level: v, old: original.value[k] })
    }
  }
  return out
})

// 受影响账号数（保存前提示）
const affected = computed(() => {
  const set = new Set(changes.value.map((c) => c.role_code))
  let n = 0
  for (const r of matrix.value.roles) {
    if (set.has(r.code)) n += r.accounts || 0
  }
  return n
})

async function loadMatrix() {
  err.value = ''
  try {
    const m = await api.get('/api/admin/permission-matrix')
    matrix.value = m
    original.value = {}
    for (const c of m.cells) original.value[cellKey(c.role_code, c.point_code)] = c.level
    draft.value = {}
    msg.value = ''
  } catch (e) {
    err.value = e.message
  }
}

async function save() {
  if (!changes.value.length) {
    msg.value = '没有改动'
    return
  }
  const lines = changes.value
    .map((c) => `${c.role_code} × ${c.point_code}: ${c.old} → ${c.level}`)
    .join('\n')
  const ok = window.confirm(
    `即将保存 ${changes.value.length} 处改动，影响账号数 ≈ ${affected}：\n\n${lines}\n\n确认保存？（保存后立即生效；点「取消」则完全不生效）`)
  if (!ok) return
  try {
    const res = await api.put('/api/admin/permission-matrix', {
      changes: changes.value.map(({ role_code, point_code, level }) => ({ role_code, point_code, level })),
      reason: '权限矩阵后台调整',
    })
    msg.value = `已保存 ${res.changed} 处改动，立即生效`
    await loadMatrix()
  } catch (e) {
    err.value = e.message
  }
}

function cancel() {
  draft.value = {}
  msg.value = '已放弃改动（未保存 ⇒ 权限不变）'
}

async function loadRoles() {
  try {
    roles.value = (await api.get('/api/admin/roles')).roles || []
  } catch (e) {
    err.value = e.message
  }
}
async function loadBindings() {
  try {
    bindings.value = (await api.get('/api/admin/user-roles')).bindings || []
  } catch (e) {
    err.value = e.message
  }
}
async function loadAudit() {
  try {
    const d = await api.get('/api/admin/audit-log')
    auditRows.value = (d.rows || []).filter((r) => r.entity === 's_role_permission')
  } catch (e) {
    err.value = e.message
  }
}

// ---- 角色管理 ----
const newRole = ref({ code: '', name: '', copy_from: '' })
async function createRole() {
  try {
    await api.post('/api/admin/roles', {
      code: newRole.value.code,
      name: newRole.value.name,
      copy_from: newRole.value.copy_from || undefined,
      reason: '后台新增角色',
    })
    msg.value = '角色已创建' + (newRole.value.copy_from ? `（已从 ${newRole.value.copy_from} 复制权限）` : '')
    newRole.value = { code: '', name: '', copy_from: '' }
    await Promise.all([loadRoles(), loadMatrix(), loadBindings()])
  } catch (e) {
    err.value = e.message
  }
}
async function renameRole(r) {
  const name = window.prompt('新名称', r.name)
  if (!name || name === r.name) return
  try {
    await api.patch(`/api/admin/roles/${r.code}`, { name, reason: '重命名' })
    await loadRoles()
  } catch (e) {
    err.value = e.message
  }
}
async function toggleRole(r) {
  const next = r.status === '启用' ? '停用' : '启用'
  if (!window.confirm(`确定 ${next} 角色「${r.name}」吗？`)) return
  try {
    await api.patch(`/api/admin/roles/${r.code}`, { status: next, reason: `${next}角色` })
    await Promise.all([loadRoles(), loadBindings()])
  } catch (e) {
    err.value = e.message
  }
}
async function deleteRole(r) {
  if (!window.confirm(`确定删除角色「${r.name}」吗？（内置角色不可删除）`)) return
  try {
    await api.del(`/api/admin/roles/${r.code}`)
    msg.value = '角色已删除'
    await Promise.all([loadRoles(), loadMatrix()])
  } catch (e) {
    err.value = e.message
  }
}

// ---- 用户绑定 ----
const bindForm = ref({ open_id: '', role_code: '' })
async function bind() {
  try {
    await api.post('/api/admin/user-roles', {
      open_id: bindForm.value.open_id,
      role_code: bindForm.value.role_code,
      reason: '后台绑定角色',
    })
    msg.value = '已绑定（一个账号可叠加多个角色）'
    await Promise.all([loadBindings(), loadRoles()])
  } catch (e) {
    err.value = e.message
  }
}
async function unbind(openID, role) {
  try {
    await api.del(`/api/admin/user-roles/${encodeURIComponent(openID)}/${role}`)
    await Promise.all([loadBindings(), loadRoles()])
  } catch (e) {
    err.value = e.message
  }
}

onMounted(async () => {
  await loadMatrix()
  await Promise.all([loadRoles(), loadBindings(), loadAudit()])
})
</script>

<template>
  <section>
    <h2>权限配置</h2>
    <p class="hint">
      行 = 权限点（51 项，按模块分组可折叠）· 列 = 角色（6 个）· 格 = 授权级别。<br />
      ★ 系统管理员的<b>管理域</b>格子置灰不可编辑 —— 绕过本页直接调接口也会被后端拒绝（防锁死）。
    </p>

    <div class="tabs">
      <button :class="{ on: tab === 'matrix' }" @click="tab = 'matrix'">权限矩阵</button>
      <button :class="{ on: tab === 'roles' }" @click="tab = 'roles'">角色管理</button>
      <button :class="{ on: tab === 'users' }" @click="tab = 'users'">用户绑定</button>
      <button :class="{ on: tab === 'log' }" @click="tab = 'log'; loadAudit()">变更记录</button>
    </div>

    <p v-if="msg" class="ok">{{ msg }}</p>
    <p v-if="err" class="err">{{ err }}</p>

    <!-- ===== 矩阵 ===== -->
    <template v-if="tab === 'matrix'">
      <div class="toolbar">
        <b>{{ changes.length }} 处未保存</b>
        <span class="hint">影响账号数 ≈ {{ affected }}</span>
        <button :disabled="!changes.length" @click="save">保存（立即生效）</button>
        <button class="ghost" :disabled="!changes.length" @click="cancel">取消改动</button>
        <button class="ghost" @click="loadMatrix">重新加载</button>
      </div>

      <div v-for="g in groups" :key="g.module" class="mod">
        <div class="mod-head" @click="collapsed[g.module] = !collapsed[g.module]">
          <b>{{ collapsed[g.module] ? '▸' : '▾' }} {{ g.module }}</b>
          <span class="hint">{{ g.points.length }} 项</span>
        </div>
        <table v-if="!collapsed[g.module]">
          <thead>
            <tr>
              <th>权限点</th>
              <th v-for="r in matrix.roles" :key="r.code">
                {{ r.name }}<br /><span class="hint">{{ r.accounts }} 人</span>
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="p in g.points" :key="p.code">
              <td>
                {{ p.name }}<br /><span class="hint">{{ p.code }} · {{ p.status }}</span>
              </td>
              <td v-for="r in matrix.roles" :key="r.code"
                  :class="{ grey: lockedOf(r.code, p.code) }">
                <select :value="levelOf(r.code, p.code)"
                        :disabled="lockedOf(r.code, p.code)"
                        :title="lockedOf(r.code, p.code) ? '防锁死：系统管理员的管理域权限不可改' : p.levels"
                        @change="setLevel(r.code, p.code, $event.target.value)">
                  <option v-for="lv in (p.levels || '').split(',')" :key="lv" :value="lv">{{ lv }}</option>
                </select>
                <span v-if="lockedOf(r.code, p.code)" class="lock">🔒</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>

    <!-- ===== 角色 ===== -->
    <template v-else-if="tab === 'roles'">
      <table>
        <thead>
          <tr><th>code</th><th>名称</th><th>类型</th><th>状态</th><th>账号数</th><th>授权数</th><th>操作</th></tr>
        </thead>
        <tbody>
          <tr v-for="r in roles" :key="r.code">
            <td>{{ r.code }}</td>
            <td>{{ r.name }} <span v-if="r.is_system" class="hint">（内置）</span></td>
            <td>{{ r.kind }}</td>
            <td>{{ r.status }}</td>
            <td>{{ r.accounts }}</td>
            <td>{{ r.grants }}</td>
            <td class="ops">
              <button class="ghost" @click="renameRole(r)">重命名</button>
              <button class="ghost" @click="toggleRole(r)">
                {{ r.status === '启用' ? '停用' : '启用' }}
              </button>
              <button class="ghost" :disabled="r.is_system" :title="r.is_system ? '内置角色不可删除' : ''"
                      @click="deleteRole(r)">删除</button>
            </td>
          </tr>
        </tbody>
      </table>

      <div class="panel">
        <div class="panel-head"><b>新增角色</b></div>
        <div class="form">
          <label><span>code*</span><input v-model="newRole.code" placeholder="如 qc_assistant" /></label>
          <label><span>名称*</span><input v-model="newRole.name" placeholder="如 质检助理" /></label>
          <label>
            <span>从某角色复制权限</span>
            <select v-model="newRole.copy_from">
              <option value="">（不复制）</option>
              <option v-for="r in roles" :key="r.code" :value="r.code">{{ r.name }}</option>
            </select>
          </label>
          <div class="full"><button @click="createRole">创建</button></div>
        </div>
      </div>
    </template>

    <!-- ===== 用户绑定 ===== -->
    <template v-else-if="tab === 'users'">
      <div class="panel">
        <div class="panel-head"><b>绑定角色</b></div>
        <div class="form">
          <label><span>open_id*</span><input v-model="bindForm.open_id" placeholder="飞书 open_id" /></label>
          <label>
            <span>角色*</span>
            <select v-model="bindForm.role_code">
              <option value="" disabled>请选择</option>
              <option v-for="r in roles" :key="r.code" :value="r.code">{{ r.name }}</option>
            </select>
          </label>
          <div class="full">
            <button :disabled="!bindForm.open_id || !bindForm.role_code" @click="bind">绑定</button>
            <span class="hint">同一账号可叠加多个角色，权限取并集</span>
          </div>
        </div>
      </div>

      <table>
        <thead><tr><th>open_id</th><th>姓名</th><th>状态</th><th>角色</th><th>操作</th></tr></thead>
        <tbody>
          <tr v-for="b in bindings" :key="b.open_id">
            <td>{{ b.open_id }}</td>
            <td>{{ b.name }}</td>
            <td>{{ b.status }}</td>
            <td>
              <span v-for="r in b.roles" :key="r" class="tag">
                {{ r }}
                <a href="javascript:;" @click="unbind(b.open_id, r)">×</a>
              </span>
            </td>
            <td></td>
          </tr>
          <tr v-if="!bindings.length"><td colspan="5" class="empty">（无绑定）</td></tr>
        </tbody>
      </table>
    </template>

    <!-- ===== 变更记录 ===== -->
    <template v-else>
      <table>
        <thead>
          <tr><th>时间</th><th>操作者</th><th>角色 × 权限点</th><th>旧值</th><th>新值</th><th>原因</th></tr>
        </thead>
        <tbody>
          <tr v-for="r in auditRows" :key="r.id">
            <td>{{ r.at }}</td>
            <td>{{ r.actor_open_id }}</td>
            <td>{{ r.field }}</td>
            <td>{{ r.old_value }}</td>
            <td>{{ r.new_value }}</td>
            <td>{{ r.reason }}</td>
          </tr>
          <tr v-if="!auditRows.length"><td colspan="6" class="empty">（暂无权限变更记录）</td></tr>
        </tbody>
      </table>
    </template>
  </section>
</template>
