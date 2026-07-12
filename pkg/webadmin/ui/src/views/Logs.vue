<template>
  <section class="page-stack">
    <div class="page-header">
      <div>
        <h1>日志</h1>
        <p class="muted">查看最近的服务运行和 S3 请求日志。</p>
      </div>
      <button class="secondary-button" type="button" :disabled="loading" @click="load">{{ loading ? '刷新中…' : '刷新' }}</button>
    </div>

    <div v-if="error" class="notice error-notice">{{ error }}</div>
    <div v-if="response?.warning" class="notice warning-notice">{{ response.warning }}</div>
    <div v-if="response && !response.file_enabled" class="notice info-notice">当前仅保留内存日志，服务重启后会清空。配置 log.file 可启用轮转落盘。</div>

    <section class="panel form-panel">
      <form class="log-toolbar" @submit.prevent="load">
        <div class="form-field"><label for="log-level">级别</label><select id="log-level" v-model="level"><option value="">全部</option><option>DEBUG</option><option>INFO</option><option>WARN</option><option>ERROR</option></select></div>
        <div class="form-field"><label for="log-query">搜索</label><input id="log-query" v-model="query" type="search" placeholder="消息、桶名或请求 ID" /></div>
        <div class="form-field log-limit"><label for="log-limit">条数</label><select id="log-limit" v-model.number="limit"><option :value="100">100</option><option :value="200">200</option><option :value="500">500</option></select></div>
        <button class="primary-button" type="submit" :disabled="loading">查询</button>
      </form>
    </section>

    <section class="panel log-panel">
      <div v-if="loading" class="log-state">加载中…</div>
      <div v-else-if="!response?.entries.length" class="log-state">暂无匹配日志。</div>
      <div v-else class="log-list">
        <article v-for="(entry, index) in response.entries" :key="`${entry.time}-${index}`" class="log-row">
          <time>{{ formatTime(entry.time) }}</time>
          <strong :class="`log-level level-${entry.level.toLowerCase()}`">{{ entry.level }}</strong>
          <code class="log-message">{{ entry.msg }}</code>
          <code v-if="entry.attrs && Object.keys(entry.attrs).length" class="log-attrs">{{ formatAttrs(entry.attrs) }}</code>
        </article>
      </div>
    </section>
  </section>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { adminApi, type LogsResponse } from '../api/client'

const response = ref<LogsResponse | null>(null)
const loading = ref(false)
const error = ref('')
const level = ref('')
const query = ref('')
const limit = ref(200)

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    response.value = await adminApi.logs({ limit: limit.value, level: level.value, q: query.value.trim() })
  } catch (err) {
    error.value = err instanceof Error ? err.message : '加载日志失败'
  } finally {
    loading.value = false
  }
}

function formatTime(value: string) {
  if (!value) return '—'
  return new Date(value).toLocaleString()
}

function formatAttrs(attrs: Record<string, unknown>) {
  return Object.entries(attrs).map(([key, value]) => `${key}=${String(value)}`).join(' ')
}
</script>
