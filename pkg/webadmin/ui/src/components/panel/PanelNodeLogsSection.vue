<template>
  <section class="panel panel-detail-section">
    <div class="panel-section-heading">
      <div>
        <h2>节点日志</h2>
        <p class="muted">通过 mTLS 拉取该节点当前内存 ring；不读取轮转文件，也不提供实时流。</p>
      </div>
      <button class="primary-button" type="button" :disabled="querying || disabled || !online" @click="runQuery">
        {{ querying ? '拉取中…' : '拉取日志' }}
      </button>
    </div>

    <div v-if="!online" class="notice warning-notice panel-inline-notice">节点当前离线，无法发起日志查询。节点数据面不受影响。</div>
    <div v-if="error" class="notice error-notice panel-inline-notice">{{ error }}</div>
    <div v-if="taskStateMessage" class="notice info-notice panel-inline-notice">{{ taskStateMessage }}</div>
    <div v-if="task?.result.log_truncated" class="notice warning-notice panel-inline-notice">结果已达到条数、字段或 256 KiB 响应上限，仅显示最新的可用部分。</div>

    <form class="panel-log-query-form" @submit.prevent="runQuery">
      <div class="form-field">
        <label for="node-log-level">级别</label>
        <select id="node-log-level" v-model="level" :disabled="querying || disabled">
          <option value="">全部</option>
          <option value="DEBUG">DEBUG</option>
          <option value="INFO">INFO</option>
          <option value="WARN">WARN</option>
          <option value="ERROR">ERROR</option>
        </select>
      </div>
      <div class="form-field panel-log-keyword">
        <label for="node-log-keyword">关键字</label>
        <input id="node-log-keyword" v-model="keyword" type="search" maxlength="256" placeholder="消息或属性" :disabled="querying || disabled" />
      </div>
      <div class="form-field">
        <label for="node-log-since">开始时间</label>
        <input id="node-log-since" v-model="since" type="datetime-local" :disabled="querying || disabled" />
      </div>
      <div class="form-field">
        <label for="node-log-until">结束时间</label>
        <input id="node-log-until" v-model="until" type="datetime-local" :disabled="querying || disabled" />
      </div>
      <div class="form-field panel-log-limit">
        <label for="node-log-limit">条数</label>
        <select id="node-log-limit" v-model.number="limit" :disabled="querying || disabled">
          <option :value="100">100</option>
          <option :value="200">200</option>
          <option :value="500">500</option>
        </select>
      </div>
    </form>

    <div class="panel-log-results">
      <div v-if="querying && !task" class="log-state">正在派发查询…</div>
      <div v-else-if="task?.state === 'success' && structuredEntries.length === 0 && legacyLines.length === 0" class="log-state">没有匹配的节点日志。</div>
      <div v-else-if="!task" class="log-state">设置过滤条件后拉取一次节点日志。</div>
      <div v-if="structuredEntries.length" class="log-list">
        <article v-for="(entry, index) in structuredEntries" :key="`${entry.time || 'entry'}-${index}`" class="log-row">
          <time>{{ formatTime(entry.time) }}</time>
          <strong :class="`log-level level-${(entry.level || 'info').toLowerCase()}`">{{ entry.level || 'INFO' }}</strong>
          <code class="log-message">{{ entry.msg }}</code>
          <code v-if="entry.attrs && Object.keys(entry.attrs).length" class="log-attrs">{{ formatAttrs(entry.attrs) }}</code>
        </article>
      </div>
      <div v-else-if="legacyLines.length" class="panel-log-legacy">
        <p class="muted">该节点返回旧版文本结果。</p>
        <pre v-for="(line, index) in legacyLines" :key="index">{{ line }}</pre>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { adminApi, ApiError, type PanelTask, type PanelTaskLogEntry } from '../../api/client'

const props = defineProps<{ nodeId: number; online: boolean; disabled: boolean }>()

const level = ref('')
const keyword = ref('')
const since = ref('')
const until = ref('')
const limit = ref(200)
const querying = ref(false)
const task = ref<PanelTask | null>(null)
const error = ref('')
let pollTimer: number | undefined
let queryGeneration = 0
let disposed = false

const structuredEntries = computed<PanelTaskLogEntry[]>(() => task.value?.result.log_entries ?? [])
const legacyLines = computed(() => structuredEntries.value.length ? [] : task.value?.result.log_lines ?? [])
const taskStateMessage = computed(() => {
  if (!task.value) return ''
  if (task.value.state === 'pending') return '查询已提交，等待节点接收。'
  if (task.value.state === 'running') return '节点正在查询当前内存日志。'
  if (task.value.state === 'unknown') return '控制面连接已断开，无法确认查询是否完成；请在节点恢复在线后重新拉取。'
  if (task.value.state === 'success') return `已从远程 ${task.value.result.log_source || 'ring'} 返回 ${structuredEntries.value.length || legacyLines.value.length} 条日志。`
  return ''
})

onBeforeUnmount(() => {
  disposed = true
  queryGeneration += 1
  stopPolling()
})

async function runQuery() {
  if (querying.value) return
  error.value = ''
  task.value = null
  if (props.disabled) {
    error.value = '已退役节点不能发起日志查询。'
    return
  }
  if (!props.online) {
    error.value = '节点当前离线，无法发起日志查询。'
    return
  }
  let sinceValue: string | undefined
  let untilValue: string | undefined
  try {
    sinceValue = toRFC3339(since.value)
    untilValue = toRFC3339(until.value)
    if (sinceValue && untilValue && Date.parse(sinceValue) > Date.parse(untilValue)) {
      error.value = '开始时间不能晚于结束时间。'
      return
    }
  } catch {
    error.value = '时间格式无效，请重新选择。'
    return
  }
  stopPolling()
  const generation = ++queryGeneration
  querying.value = true
  try {
    const dispatched = await adminApi.dispatchNodeLogQuery(props.nodeId, {
      level: level.value || undefined,
      keyword: keyword.value.trim() || undefined,
      since: sinceValue,
      until: untilValue,
      limit: limit.value
    })
    await pollTask(dispatched.task_id, generation, Date.now() + 70_000)
  } catch (err) {
    if (generation !== queryGeneration || disposed) return
    querying.value = false
    error.value = queryErrorMessage(err)
  }
}

async function pollTask(taskID: string, generation: number, deadline: number) {
  if (generation !== queryGeneration || disposed) return
  if (Date.now() >= deadline) {
    querying.value = false
    error.value = '等待节点日志结果超时，请确认控制面连接后重试。'
    return
  }
  try {
    const next = await adminApi.getNodeTask(props.nodeId, taskID)
    if (generation !== queryGeneration || disposed) return
    task.value = next
    if (next.state === 'success') {
      querying.value = false
      return
    }
    if (next.state === 'failed') {
      querying.value = false
      error.value = next.error?.includes('timed out') ? '节点日志查询超时，请重试。' : next.error || '节点日志查询失败。'
      return
    }
    if (next.state === 'unknown') {
      querying.value = false
      return
    }
    pollTimer = window.setTimeout(() => { void pollTask(taskID, generation, deadline) }, 800)
  } catch (err) {
    if (generation !== queryGeneration || disposed) return
    querying.value = false
    error.value = queryErrorMessage(err)
  }
}

function stopPolling() {
  if (pollTimer !== undefined) {
    window.clearTimeout(pollTimer)
    pollTimer = undefined
  }
}

function toRFC3339(value: string) {
  if (!value) return undefined
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) throw new Error('invalid time')
  return date.toISOString()
}

function queryErrorMessage(err: unknown) {
  if (err instanceof ApiError) {
    if (err.status === 409) return '节点当前离线或不可用，请等待控制面恢复后重试。'
    if (err.status === 429) return '节点当前任务较多，请稍后重试。'
    if (err.status === 404) return '节点或日志任务不存在。'
  }
  return err instanceof Error ? err.message : '节点日志查询失败。'
}

function formatTime(value?: string) {
  return value ? new Date(value).toLocaleString() : '—'
}

function formatAttrs(attrs: Record<string, string>) {
  return Object.entries(attrs).map(([key, value]) => `${key}=${value}`).join(' ')
}
</script>
