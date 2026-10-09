<script setup>
import { ref, onMounted, computed } from 'vue'
import { api } from './api.js'
import MasterData from './components/MasterData.vue'
import PermissionAdmin from './components/PermissionAdmin.vue'
import Receiving from './components/Receiving.vue'
import Sampling from './components/Sampling.vue'
import Retention from './components/Retention.vue'

const page = ref('recv')
const me = ref(null)
const err = ref('')

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
    <nav>
      <button :class="{ on: page === 'recv' }" @click="page = 'recv'">收货</button>
      <button :class="{ on: page === 'sample' }" @click="page = 'sample'">取样</button>
      <button :class="{ on: page === 'retain' }" @click="page = 'retain'">留样</button>
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

  <p v-if="err" class="err">{{ err }}</p>

  <main>
    <Receiving v-if="page === 'recv'" :me="me" />
    <Sampling v-else-if="page === 'sample'" :me="me" />
    <Retention v-else-if="page === 'retain'" :me="me" />
    <MasterData v-else-if="page === 'md'" :me="me" />
    <PermissionAdmin v-else :me="me" />
  </main>
</template>
