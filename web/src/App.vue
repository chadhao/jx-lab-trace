<script setup>
import { ref, onMounted, computed } from 'vue'
import { api } from './api.js'
import MasterData from './components/MasterData.vue'
import PermissionAdmin from './components/PermissionAdmin.vue'
import Receiving from './components/Receiving.vue'
import Sampling from './components/Sampling.vue'
import Retention from './components/Retention.vue'
import InspTasks from './components/InspTasks.vue'
import InspDetail from './components/InspDetail.vue'
import Production from './components/Production.vue'
import Shipment from './components/Shipment.vue'
import Trace from './components/Trace.vue'
import Report from './components/Report.vue'
import Rpt from './components/Rpt.vue'

const page = ref('recv')
const me = ref(null)
const err = ref('')
const inspId = ref(null) // 非空 ⇒ 检测 tab 内显示检测单详情

async function loadMe() {
  try {
    me.value = await api.get('/api/me')
    err.value = ''
  } catch (e) {
    me.value = null
    err.value = e.status === 401 ? '未登录（或会话已失效）' : e.message
  }
}
onMounted(loadMe)

const roleText = computed(() => {
  if (!me.value) return ''
  const all = [...(me.value.roles || []), ...(me.value.sys_roles || [])]
  return all.length ? all.join(' / ') : '（无角色）'
})

async function devLogin() {
  try {
    await api.post('/api/auth/dev-login', {})
    await loadMe()
  } catch (e) {
    err.value = 'dev 登录失败：' + e.message
  }
}
async function logout() {
  try {
    await api.post('/api/auth/logout', {})
  } catch (e) { /* 忽略 */ }
  await loadMe()
}
</script>

<template>
  <header class="topbar">
    <div class="brand">江熙新材 · 实验检测数据追踪</div>
    <nav v-if="me">
      <button :class="{ on: page === 'recv' }" @click="page = 'recv'">收货</button>
      <button :class="{ on: page === 'sample' }" @click="page = 'sample'">取样</button>
      <button :class="{ on: page === 'retain' }" @click="page = 'retain'">留样</button>
      <button :class="{ on: page === 'insp' }" @click="page = 'insp'">检测</button>
      <button :class="{ on: page === 'prod' }" @click="page = 'prod'">生产</button>
      <button :class="{ on: page === 'ship' }" @click="page = 'ship'">出货</button>
      <button :class="{ on: page === 'trace' }" @click="page = 'trace'">追溯</button>
      <button :class="{ on: page === 'report' }" @click="page = 'report'">报告</button>
      <button :class="{ on: page === 'rpt' }" @click="page = 'rpt'">报表</button>
      <button :class="{ on: page === 'md' }" @click="page = 'md'">主数据</button>
      <button :class="{ on: page === 'perm' }" @click="page = 'perm'">权限配置</button>
    </nav>
    <div class="who">
      <template v-if="me">
        <span>{{ me.name }}</span>
        <span class="roles">{{ roleText }}</span>
        <button class="ghost" @click="logout">退出</button>
      </template>
      <template v-else>
        <button @click="devLogin">dev 登录</button>
      </template>
    </div>
  </header>

  <!-- ★★ 未登录 ⇒ **只渲染登录引导，绝不渲染业务页**。
       动因（2026-10-10 实测）：原实现 `/api/me` 401 时**照样渲染业务页**，
       各子组件各自去拉数据、各自报「未登录或会话已失效」，用户看到一屏报错却不知怎么办。
       ⇒ ★ 判据：**鉴权失败的页面必须"引导去登录"，不能"装作已登录"**。 -->
  <section v-if="!me" class="login-card">
    <h2>请先登录</h2>
    <p v-if="err" class="err">{{ err }}</p>
    <p class="hint">身份由飞书提供（open_id 为身份主键，**未在系统内映射角色的账号会被拒绝**）。</p>
    <div class="login-actions">
      <a class="btn-primary" href="/api/auth/feishu/start">用飞书登录</a>
      <button class="ghost" @click="devLogin">开发模式免登</button>
    </div>
    <p class="hint">★ 从飞书工作台点开本应用时，会自动走免登，通常看不到本页。</p>
  </section>

  <template v-else>
    <p v-if="err" class="err">{{ err }}</p>

    <main>
      <Receiving v-if="page === 'recv'" :me="me" />
      <Sampling v-else-if="page === 'sample'" :me="me" />
      <Retention v-else-if="page === 'retain'" :me="me" />
      <InspDetail v-else-if="page === 'insp' && inspId" :me="me" :inspection-id="inspId"
        @open="(id) => (inspId = id)" @back="inspId = null" />
      <InspTasks v-else-if="page === 'insp'" :me="me" @open="(id) => (inspId = id)" />
      <MasterData v-else-if="page === 'md'" :me="me" />
      <Production v-else-if="page === 'prod'" :me="me" />
      <Shipment v-else-if="page === 'ship'" :me="me" />
      <Trace v-else-if="page === 'trace'" :me="me" />
      <Report v-else-if="page === 'report'" :me="me" />
      <Rpt v-else-if="page === 'rpt'" :me="me" />
      <PermissionAdmin v-else :me="me" />
    </main>
  </template>
</template>
