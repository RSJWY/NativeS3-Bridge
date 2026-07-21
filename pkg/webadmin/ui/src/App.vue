<template>
  <router-view v-if="isLoginRoute" />
  <main v-else-if="!runtimeState.ready" class="bootstrap-page">
    <section class="panel bootstrap-panel">
      <h1>正在连接管理服务</h1>
      <p v-if="!bootstrapError" class="muted">正在读取服务模式…</p>
      <template v-else>
        <p class="notice error-notice">{{ bootstrapError }}</p>
        <div class="bootstrap-actions">
          <button class="secondary-button" type="button" @click="logout">重新登录</button>
          <button class="primary-button" type="button" :disabled="bootstrapLoading" @click="loadServiceMode">
            {{ bootstrapLoading ? '重试中…' : '重试' }}
          </button>
        </div>
      </template>
    </section>
  </main>
  <div v-else class="app-shell">
    <aside class="sidebar">
      <div class="brand-block">
        <div class="brand-mark" aria-hidden="true">NS</div>
        <div>
          <div class="brand">NativeS3 Bridge</div>
          <div class="brand-description">{{ runtimeState.serviceMode === 'panel' ? '集中节点管理' : '对象存储管理' }}</div>
        </div>
      </div>
      <nav class="nav-list" aria-label="管理导航">
        <template v-if="runtimeState.serviceMode === 'panel'">
          <RouterLink to="/nodes">节点管理</RouterLink>
        </template>
        <template v-else>
          <RouterLink to="/dashboard">仪表盘</RouterLink>
          <RouterLink to="/credentials">密钥管理</RouterLink>
          <RouterLink to="/buckets">桶管理</RouterLink>
          <RouterLink to="/logs">日志</RouterLink>
        </template>
      </nav>
      <div class="sidebar-footer">
        <ProjectMeta />
        <button class="text-button sidebar-logout" type="button" @click="logout">退出登录</button>
      </div>
    </aside>
    <main class="main-content">
      <router-view />
    </main>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { adminApi } from './api/client'
import ProjectMeta from './components/ProjectMeta.vue'
import { markLoggedOut } from './state/auth'
import { routeMatchesService, runtimeState, serviceHomePath, setServiceMode } from './state/runtime'

const route = useRoute()
const router = useRouter()
const bootstrapState = reactive({ loading: false, error: '' })
const isLoginRoute = computed(() => route.path === '/login')
const bootstrapLoading = computed(() => bootstrapState.loading)
const bootstrapError = computed(() => bootstrapState.error)

onMounted(() => {
  if (!isLoginRoute.value) {
    void loadServiceMode()
  }
})

watch(
  () => route.path,
  (path) => {
    if (path !== '/login' && !runtimeState.ready) {
      void loadServiceMode()
    }
  }
)

async function loadServiceMode() {
  if (bootstrapState.loading) return
  bootstrapState.loading = true
  bootstrapState.error = ''
  try {
    const settings = await adminApi.authSettings()
    setServiceMode(settings.service_mode)
    if (!routeMatchesService(route.path)) {
      await router.replace(serviceHomePath())
    }
  } catch (err) {
    bootstrapState.error = err instanceof Error ? err.message : '读取服务模式失败'
  } finally {
    bootstrapState.loading = false
  }
}

async function logout() {
  try {
    await adminApi.logout()
  } finally {
    markLoggedOut()
    await router.replace('/login')
  }
}
</script>
