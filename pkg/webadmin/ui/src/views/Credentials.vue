<template>
  <section class="page-stack">
    <div class="page-header">
      <div>
        <h1>密钥管理</h1>
        <p class="muted">创建访问密钥，调整容量配额，启用或禁用 S3 访问。</p>
      </div>
      <button class="primary-button" type="button" @click="openCreate">新建密钥</button>
    </div>

    <div v-if="error" class="notice error-notice">{{ error }}</div>

    <section class="panel">
      <table class="data-table">
        <thead>
          <tr>
            <th>Access Key</th>
            <th>名称</th>
            <th>状态</th>
            <th>已用 / 配额</th>
            <th>创建时间</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="loading">
            <td colspan="6">加载中…</td>
          </tr>
          <tr v-else-if="credentials.length === 0">
            <td colspan="6">暂无密钥。</td>
          </tr>
          <tr v-for="credential in credentials" :key="credential.id">
            <td><code>{{ credential.access_key }}</code></td>
            <td>{{ credential.name || '未命名' }}</td>
            <td>{{ credential.status === 'enabled' ? '启用' : '禁用' }}</td>
            <td>{{ formatBytes(credential.used_bytes) }} / {{ formatQuota(credential.quota_bytes) }}</td>
            <td>{{ new Date(credential.created_at).toLocaleString() }}</td>
            <td class="actions-cell">
              <button class="secondary-button" type="button" @click="openEdit(credential)">编辑</button>
              <button class="secondary-button" type="button" @click="toggleStatus(credential)">
                {{ credential.status === 'enabled' ? '禁用' : '启用' }}
              </button>
              <button class="danger-button" type="button" @click="remove(credential)">删除</button>
            </td>
          </tr>
        </tbody>
      </table>
    </section>

    <div v-if="showForm" class="modal-backdrop" @click.self="closeForm">
      <form class="modal-card" @submit.prevent="saveForm">
        <h2>{{ editing ? '编辑密钥' : '新建密钥' }}</h2>
        <label for="credential-name">名称</label>
        <input id="credential-name" v-model="form.name" type="text" maxlength="128" />
        <label for="quota-bytes">配额字节数（0 表示不限）</label>
        <input id="quota-bytes" v-model="form.quotaBytes" type="number" min="0" step="1" />
        <p v-if="formError" class="error-text">{{ formError }}</p>
        <div class="modal-actions">
          <button class="secondary-button" type="button" @click="closeForm">取消</button>
          <button class="primary-button" type="submit" :disabled="saving">保存</button>
        </div>
      </form>
    </div>

    <div v-if="created" class="modal-backdrop" @click.self="created = null">
      <section class="modal-card secret-modal">
        <h2>请立即保存 Secret Key</h2>
        <p class="muted">Secret Key 只会显示这一次，关闭后无法从后台再次查看。</p>
        <label>Access Key</label>
        <pre>{{ created.access_key }}</pre>
        <label>Secret Key</label>
        <pre>{{ created.secret_key }}</pre>
        <div class="modal-actions">
          <button class="primary-button" type="button" @click="created = null">我已保存</button>
        </div>
      </section>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { adminApi, type CreatedCredential, type Credential } from '../api/client'
import { formatBytes, formatQuota, parseQuotaToBytes } from '../utils/format'

const credentials = ref<Credential[]>([])
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const formError = ref('')
const showForm = ref(false)
const editing = ref<Credential | null>(null)
const created = ref<CreatedCredential | null>(null)
const form = reactive({ name: '', quotaBytes: '0' })

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    credentials.value = await adminApi.listCredentials()
  } catch (err) {
    error.value = err instanceof Error ? err.message : '加载密钥失败'
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = null
  form.name = ''
  form.quotaBytes = '0'
  formError.value = ''
  showForm.value = true
}

function openEdit(credential: Credential) {
  editing.value = credential
  form.name = credential.name
  form.quotaBytes = String(credential.quota_bytes)
  formError.value = ''
  showForm.value = true
}

function closeForm() {
  showForm.value = false
}

async function saveForm() {
  const quota = parseQuotaToBytes(form.quotaBytes)
  if (quota === null) {
    formError.value = '配额必须是非负整数且不能超过安全范围'
    return
  }
  saving.value = true
  formError.value = ''
  try {
    if (editing.value) {
      await adminApi.updateCredential(editing.value.id, { name: form.name, quota_bytes: quota })
    } else {
      created.value = await adminApi.createCredential({ name: form.name, quota_bytes: quota })
    }
    showForm.value = false
    await load()
  } catch (err) {
    formError.value = err instanceof Error ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}

async function toggleStatus(credential: Credential) {
  const nextStatus = credential.status === 'enabled' ? 'disabled' : 'enabled'
  error.value = ''
  try {
    await adminApi.updateCredential(credential.id, { status: nextStatus })
    await load()
  } catch (err) {
    error.value = err instanceof Error ? err.message : '更新状态失败'
  }
}

async function remove(credential: Credential) {
  if (!window.confirm(`确认删除密钥 ${credential.access_key}？`)) {
    return
  }
  error.value = ''
  try {
    await adminApi.deleteCredential(credential.id)
    await load()
  } catch (err) {
    error.value = err instanceof Error ? err.message : '删除密钥失败'
  }
}
</script>
