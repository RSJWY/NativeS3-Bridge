<template>
  <section class="panel panel-detail-section">
    <div class="panel-section-heading">
      <div>
        <h2>原地迁移导入</h2>
        <p class="muted">先从在线节点读取只读摘要，确认后才在 Panel 内原子接管并发布版本 1；不会主动改写节点。</p>
      </div>
      <button v-if="!summary" class="secondary-button" type="button" :disabled="loading || disabled || !online" @click="requestImport">
        {{ loading ? '等待节点报告…' : '请求导入' }}
      </button>
    </div>

    <div v-if="!online && !summary" class="notice info-notice panel-inline-notice">节点离线时不能请求导入；已有 pending 摘要仍可继续审阅。</div>
    <p v-if="error" class="error-text panel-form-error">{{ error }}</p>
    <p v-if="message" class="success-text panel-form-error">{{ message }}</p>

    <div v-if="summary" class="panel-import-summary">
      <dl class="panel-effective-state">
        <div><dt>密钥</dt><dd>{{ summary.credential_count }}</dd></div>
        <div><dt>Bucket</dt><dd>{{ summary.bucket_count }}</dd></div>
        <div><dt>Webhook</dt><dd>{{ summary.webhook_count }}</dd></div>
        <div><dt>限流策略</dt><dd>{{ summary.rate_limit_configured ? '包含' : '未配置' }}</dd></div>
      </dl>
      <div class="panel-import-lists">
        <div>
          <strong>Access Key</strong>
          <p v-if="summary.access_keys.length === 0" class="muted">无</p>
          <code v-for="accessKey in summary.access_keys" :key="accessKey">{{ accessKey }}</code>
        </div>
        <div>
          <strong>Bucket 名称</strong>
          <p v-if="summary.bucket_names.length === 0" class="muted">无</p>
          <code v-for="bucket in summary.bucket_names" :key="bucket">{{ bucket }}</code>
        </div>
      </div>
      <div class="operation-result">
        <span>本地配置 Hash</span>
        <code>{{ summary.content_hash }}</code>
      </div>
      <div class="reconcile-actions">
        <span>摘要不包含 Secret。确认失败时不会留下部分接管数据。</span>
        <div class="actions-cell">
          <button class="secondary-button" type="button" :disabled="loading" @click="abortImport">中止</button>
          <button class="primary-button" type="button" :disabled="loading || disabled" @click="confirmImport">确认接管</button>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { ApiError, adminApi, type PanelImportSummary } from '../../api/client'

const props = defineProps<{ nodeId: number; online: boolean; disabled: boolean; refreshKey: number }>()
const emit = defineEmits<{ confirmed: []; changed: [] }>()

const summary = ref<PanelImportSummary | null>(null)
const loading = ref(false)
const error = ref('')
const message = ref('')

onMounted(() => void loadPending())
watch(() => props.refreshKey, () => void loadPending())

async function loadPending() {
  error.value = ''
  try {
    summary.value = await adminApi.getNodeImport(props.nodeId)
  } catch (err) {
    error.value = importError(err, '读取待确认导入失败')
  }
}

async function requestImport() {
  loading.value = true
  error.value = ''
  message.value = ''
  try {
    summary.value = await adminApi.requestNodeImport(props.nodeId)
  } catch (err) {
    error.value = importError(err, '请求导入失败')
  } finally {
    loading.value = false
  }
}

async function confirmImport() {
  if (!window.confirm('确认由 Panel 接管这份配置并发布版本 1？确认前不会写入权威配置，确认成功后后续修改必须从 Panel 发布。')) return
  loading.value = true
  error.value = ''
  message.value = ''
  try {
    const result = await adminApi.confirmNodeImport(props.nodeId)
    summary.value = null
    message.value = `已接管并发布版本 ${result.version}。`
    emit('confirmed')
  } catch (err) {
    error.value = importError(err, '确认接管失败')
  } finally {
    loading.value = false
  }
}

async function abortImport() {
  if (!window.confirm('确认中止本次导入并丢弃待审阅摘要？节点配置不会改变。')) return
  loading.value = true
  error.value = ''
  message.value = ''
  try {
    await adminApi.abortNodeImport(props.nodeId)
    summary.value = null
    message.value = '已中止导入。'
    emit('changed')
  } catch (err) {
    error.value = importError(err, '中止导入失败')
  } finally {
    loading.value = false
  }
}

function importError(err: unknown, fallback: string) {
  if (err instanceof ApiError) {
    if (err.status === 504) return '节点在 30 秒内没有返回导入报告，请检查连接后重试。'
    if (err.status === 409 && err.message.includes('offline')) return '节点当前离线，无法请求只读导入报告。'
    if (err.status === 409 && err.message.includes('managed')) return '该节点已经存在受管配置，不能再次导入。'
    if (err.status === 409 && err.message.includes('pending')) return '已有待确认导入，请先审阅、确认或中止。'
    if (err.status === 404) return '没有待确认的导入摘要。'
    return err.message
  }
  return err instanceof Error ? err.message : fallback
}
</script>
