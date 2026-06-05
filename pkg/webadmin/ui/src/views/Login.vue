<template>
  <main class="login-page">
    <form class="login-card" @submit.prevent="submit">
      <h1>管理后台登录</h1>
      <p class="muted">输入配置中的单管理员密码。</p>
      <label for="password">密码</label>
      <input id="password" v-model="password" type="password" autocomplete="current-password" required autofocus />
      <p v-if="error" class="error-text">{{ error }}</p>
      <button class="primary-button" type="submit" :disabled="loading">{{ loading ? '登录中…' : '登录' }}</button>
    </form>
  </main>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { adminApi } from '../api/client'
import { markLoggedIn } from '../state/auth'

const password = ref('')
const error = ref('')
const loading = ref(false)
const router = useRouter()
const route = useRoute()

async function submit() {
  error.value = ''
  loading.value = true
  try {
    await adminApi.login(password.value)
    markLoggedIn()
    const redirect = normalizeRedirect(route.query.redirect)
    await router.replace(redirect)
  } catch (err) {
    error.value = err instanceof Error ? err.message : '登录失败'
  } finally {
    loading.value = false
  }
}

function normalizeRedirect(value: unknown): string {
  if (typeof value !== 'string' || !value.startsWith('/') || value.startsWith('//') || value === '/login') {
    return '/dashboard'
  }
  return value
}
</script>
