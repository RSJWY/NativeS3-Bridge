<template>
  <main class="login-page">
    <div class="login-shell">
      <div class="login-brand">
        <div class="brand-mark" aria-hidden="true">NS</div>
        <div>
          <div class="brand">NativeS3 Bridge</div>
          <div class="brand-description">对象存储管理</div>
        </div>
      </div>
      <form class="login-card" @submit.prevent="submit">
        <h1>管理后台登录</h1>
        <p class="muted">输入配置中的单管理员密码。</p>
        <label for="password">密码</label>
        <input id="password" v-model="password" type="password" autocomplete="current-password" required autofocus />
        <template v-if="settings.totp_required">
          <label for="totp-code">动态验证码</label>
          <input id="totp-code" v-model="totpCode" type="text" inputmode="numeric" autocomplete="one-time-code" maxlength="6" required />
        </template>
        <div v-if="settings.captcha_enabled" class="captcha-slot">
          <div ref="captchaEl"></div>
        </div>
        <p v-if="error" class="error-text">{{ error }}</p>
        <button class="primary-button" type="submit" :disabled="loading || settingsLoading">{{ loading ? '登录中…' : '登录' }}</button>
      </form>
      <ProjectMeta />
    </div>
  </main>
</template>

<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { adminApi, type AuthSettings } from '../api/client'
import ProjectMeta from '../components/ProjectMeta.vue'
import { markLoggedIn } from '../state/auth'
import { routeMatchesService, serviceHomePath, setServiceMode } from '../state/runtime'

const password = ref('')
const totpCode = ref('')
const captchaToken = ref('')
const error = ref('')
const loading = ref(false)
const settingsLoading = ref(true)
const captchaEl = ref<HTMLElement | null>(null)
const captchaWidgetId = ref<string | null>(null)
const settings = ref<AuthSettings>({
  totp_required: false,
  captcha_enabled: false,
  captcha_provider: '',
  captcha_site_key: '',
  service_mode: 'standalone'
})
const router = useRouter()
const route = useRoute()

onMounted(async () => {
  try {
    settings.value = await adminApi.authSettings()
    setServiceMode(settings.value.service_mode)
    await nextTick()
    await renderCaptchaIfNeeded()
  } catch (err) {
    error.value = err instanceof Error ? err.message : '加载登录配置失败'
  } finally {
    settingsLoading.value = false
  }
})

async function submit() {
  error.value = ''
  if (settings.value.captcha_enabled && !captchaToken.value) {
    error.value = '请完成人机验证'
    return
  }
  loading.value = true
  try {
    await adminApi.login({
      password: password.value,
      totp_code: settings.value.totp_required ? totpCode.value : undefined,
      captcha_token: settings.value.captcha_enabled ? captchaToken.value : undefined
    })
    markLoggedIn()
    const redirect = normalizeRedirect(route.query.redirect)
    await router.replace(redirect)
  } catch (err) {
    error.value = err instanceof Error ? err.message : '登录失败'
    resetCaptcha()
  } finally {
    loading.value = false
  }
}

function normalizeRedirect(value: unknown): string {
  if (typeof value !== 'string' || !value.startsWith('/') || value.startsWith('//') || value === '/login') {
    return serviceHomePath()
  }
  return routeMatchesService(value) ? value : serviceHomePath()
}

async function renderCaptchaIfNeeded() {
  if (!settings.value.captcha_enabled || settings.value.captcha_provider !== 'turnstile' || !settings.value.captcha_site_key || !captchaEl.value) {
    return
  }
  await loadTurnstile()
  const turnstile = window.turnstile
  if (!turnstile) {
    throw new Error('加载人机验证失败')
  }
  captchaWidgetId.value = turnstile.render(captchaEl.value, {
    sitekey: settings.value.captcha_site_key,
    callback(token: string) {
      captchaToken.value = token
    },
    'expired-callback'() {
      captchaToken.value = ''
    },
    'error-callback'() {
      captchaToken.value = ''
    }
  })
}

function resetCaptcha() {
  if (captchaWidgetId.value && window.turnstile) {
    window.turnstile.reset(captchaWidgetId.value)
    captchaToken.value = ''
  }
}

function loadTurnstile(): Promise<void> {
  if (window.turnstile) {
    return Promise.resolve()
  }
  const existing = document.querySelector<HTMLScriptElement>('script[data-turnstile="true"]')
  if (existing) {
    return new Promise((resolve, reject) => {
      existing.addEventListener('load', () => resolve(), { once: true })
      existing.addEventListener('error', () => reject(new Error('加载人机验证失败')), { once: true })
    })
  }
  return new Promise((resolve, reject) => {
    const script = document.createElement('script')
    script.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit'
    script.async = true
    script.defer = true
    script.dataset.turnstile = 'true'
    script.addEventListener('load', () => resolve(), { once: true })
    script.addEventListener('error', () => reject(new Error('加载人机验证失败')), { once: true })
    document.head.appendChild(script)
  })
}
</script>
