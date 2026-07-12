<template>
  <router-view v-if="isLoginRoute" />
  <div v-else class="app-shell">
    <aside class="sidebar">
      <div class="brand-block">
        <div class="brand-mark" aria-hidden="true">NS</div>
        <div>
          <div class="brand">NativeS3 Bridge</div>
          <div class="brand-description">对象存储管理</div>
        </div>
      </div>
      <nav class="nav-list" aria-label="管理导航">
        <RouterLink to="/dashboard">仪表盘</RouterLink>
        <RouterLink to="/credentials">密钥管理</RouterLink>
        <RouterLink to="/buckets">桶管理</RouterLink>
        <RouterLink to="/logs">日志</RouterLink>
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
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { adminApi } from './api/client'
import ProjectMeta from './components/ProjectMeta.vue'
import { markLoggedOut } from './state/auth'

const route = useRoute()
const router = useRouter()
const isLoginRoute = computed(() => route.path === '/login')

async function logout() {
  try {
    await adminApi.logout()
  } finally {
    markLoggedOut()
    await router.replace('/login')
  }
}
</script>
