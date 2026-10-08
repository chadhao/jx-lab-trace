<script setup>
import { onMounted, ref } from 'vue'

const health = ref(null)
const me = ref(null)
const error = ref('')

onMounted(async () => {
  try {
    const h = await fetch('/healthz')
    health.value = h.ok ? await h.json() : { status: 'FAIL ' + h.status }
    const m = await fetch('/api/me')
    me.value = m.ok ? await m.json() : null
  } catch (e) {
    error.value = String(e)
  }
})
</script>

<template>
  <main class="page">
    <h1>实验检测数据追踪系统</h1>
    <p>
      健康检查：
      <strong :class="{ ok: health?.status === 'ok' }">
        {{ health ? health.status : '…' }}
      </strong>
      · 版本 {{ health?.version ?? '-' }}
    </p>
    <p v-if="me">已登录：{{ me.name }}（{{ me.open_id }}）· 角色：{{ [...(me.roles || []), ...(me.sys_roles || [])].join('、') || '无' }}</p>
    <p v-else>未登录（dev 环境可用 <code>POST /api/auth/dev-login</code> 建会话）</p>
    <p v-if="error" class="err">{{ error }}</p>
    <p class="hint">M0 地基批：仅最小可用页面；业务页面自批 2 起。</p>
  </main>
</template>
