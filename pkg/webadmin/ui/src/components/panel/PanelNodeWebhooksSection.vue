<template>
  <section class="panel panel-detail-section">
    <div class="panel-section-heading">
      <div>
        <h2>Webhook</h2>
        <p class="muted">仅支持对象创建与删除事件；投递在 S3 请求完成后异步执行。</p>
      </div>
      <button v-if="editingID !== null" class="secondary-button" type="button" :disabled="saving" @click="resetForm">取消编辑</button>
    </div>

    <form class="panel-resource-form panel-webhook-form" @submit.prevent="saveWebhook">
      <div class="form-field panel-webhook-url">
        <label for="panel-webhook-url">URL</label>
        <input id="panel-webhook-url" v-model="form.url" type="url" maxlength="512" placeholder="https://example.com/hooks/s3" :disabled="saving || disabled" />
      </div>
      <fieldset class="panel-event-field" :disabled="saving || disabled">
        <legend>事件</legend>
        <label><input v-model="form.events" type="checkbox" value="ObjectCreated" /> 对象创建</label>
        <label><input v-model="form.events" type="checkbox" value="ObjectDeleted" /> 对象删除</label>
      </fieldset>
      <label class="panel-checkbox-field"><input v-model="form.enabled" type="checkbox" :disabled="saving || disabled" /> 启用</label>
      <button class="primary-button" type="submit" :disabled="saving || disabled || !form.url.trim()">
        {{ saving ? '保存中…' : editingID === null ? '创建 Webhook' : '保存 Webhook' }}
      </button>
    </form>
    <p v-if="error" class="error-text panel-form-error">{{ error }}</p>

    <div class="table-scroll panel-section-table">
      <table class="data-table panel-resource-table panel-webhooks-table">
        <thead><tr><th>URL</th><th>事件</th><th>状态</th><th></th></tr></thead>
        <tbody>
          <tr v-if="loading" class="state-row"><td colspan="4">加载中…</td></tr>
          <tr v-else-if="webhooks.length === 0" class="state-row"><td colspan="4">暂无 Webhook。</td></tr>
          <tr v-for="webhook in webhooks" :key="webhook.id">
            <td class="panel-url-cell"><code>{{ webhook.url }}</code></td>
            <td>{{ webhook.events.map(eventLabel).join('、') }}</td>
            <td><span :class="['status-badge', webhook.enabled ? 'status-enabled' : 'status-disabled']">{{ webhook.enabled ? '启用' : '禁用' }}</span></td>
            <td>
              <div class="actions-cell">
                <button class="secondary-button" type="button" :disabled="saving || disabled" @click="editWebhook(webhook)">编辑</button>
                <button class="secondary-button" type="button" :disabled="saving || disabled" @click="toggleWebhook(webhook)">{{ webhook.enabled ? '禁用' : '启用' }}</button>
                <button class="danger-button" type="button" :disabled="saving || disabled" @click="deleteWebhook(webhook)">删除</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref, watch } from 'vue'
import { adminApi, type PanelWebhook, type PanelWebhookEvent } from '../../api/client'

const props = defineProps<{ nodeId: number; disabled: boolean; refreshKey: number }>()
const emit = defineEmits<{ changed: [] }>()

const webhooks = ref<PanelWebhook[]>([])
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const editingID = ref<number | null>(null)
const form = reactive<{ url: string; events: PanelWebhookEvent[]; enabled: boolean }>({
  url: '', events: ['ObjectCreated'], enabled: true
})

onMounted(() => void load())
watch(() => props.refreshKey, () => void load())

async function load() {
  loading.value = true
  error.value = ''
  try {
    webhooks.value = await adminApi.listNodeWebhooks(props.nodeId)
  } catch (err) {
    error.value = messageFromError(err, '加载 Webhook 失败')
  } finally {
    loading.value = false
  }
}

async function saveWebhook() {
  if (form.events.length === 0) {
    error.value = '请至少选择一个事件。'
    return
  }
  saving.value = true
  error.value = ''
  try {
    const input = { url: form.url.trim(), events: [...form.events], enabled: form.enabled }
    if (editingID.value === null) {
      await adminApi.createNodeWebhook(props.nodeId, input)
    } else {
      await adminApi.updateNodeWebhook(props.nodeId, editingID.value, input)
    }
    resetForm()
    await load()
    emit('changed')
  } catch (err) {
    error.value = messageFromError(err, '保存 Webhook 失败')
  } finally {
    saving.value = false
  }
}

function editWebhook(webhook: PanelWebhook) {
  editingID.value = webhook.id
  form.url = webhook.url
  form.events = [...webhook.events]
  form.enabled = webhook.enabled
  error.value = ''
}

function resetForm() {
  editingID.value = null
  form.url = ''
  form.events = ['ObjectCreated']
  form.enabled = true
}

async function toggleWebhook(webhook: PanelWebhook) {
  saving.value = true
  error.value = ''
  try {
    await adminApi.updateNodeWebhook(props.nodeId, webhook.id, { enabled: !webhook.enabled })
    await load()
    emit('changed')
  } catch (err) {
    error.value = messageFromError(err, '更新 Webhook 状态失败')
  } finally {
    saving.value = false
  }
}

async function deleteWebhook(webhook: PanelWebhook) {
  if (!window.confirm(`确认删除 Webhook「${webhook.url}」？`)) return
  saving.value = true
  error.value = ''
  try {
    await adminApi.deleteNodeWebhook(props.nodeId, webhook.id)
    if (editingID.value === webhook.id) resetForm()
    await load()
    emit('changed')
  } catch (err) {
    error.value = messageFromError(err, '删除 Webhook 失败')
  } finally {
    saving.value = false
  }
}

function eventLabel(event: PanelWebhookEvent) {
  return event === 'ObjectCreated' ? '对象创建' : '对象删除'
}

function messageFromError(err: unknown, fallback: string) {
  return err instanceof Error ? err.message : fallback
}
</script>
