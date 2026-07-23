<template>
  <section class="panel panel-detail-section">
    <div class="panel-section-heading">
      <div>
        <h2>S3 密钥</h2>
        <p class="muted">Secret 仅在创建或轮换结果中显示一次；Bucket 绑定只能选择本节点声明。</p>
      </div>
    </div>

    <form class="panel-resource-form" @submit.prevent="createCredential">
      <div class="form-field">
        <label for="panel-credential-name">名称</label>
        <input id="panel-credential-name" v-model="createForm.name" type="text" maxlength="128" :disabled="saving || disabled" />
      </div>
      <div class="form-field">
        <label for="panel-credential-bucket">绑定 Bucket</label>
        <select id="panel-credential-bucket" v-model="createForm.bucket" :disabled="saving || disabled">
          <option value="">全部 Bucket</option>
          <option v-for="bucket in buckets" :key="bucket.name" :value="bucket.name">{{ bucket.name }}</option>
        </select>
      </div>
      <div class="form-field">
        <label for="panel-credential-quota">容量配额（0 表示不限）</label>
        <div class="quota-input">
          <input id="panel-credential-quota" v-model="createForm.quotaValue" type="number" min="0" step="any" :disabled="saving || disabled" />
          <select v-model="createForm.quotaUnit" aria-label="配额单位" :disabled="saving || disabled">
            <option v-for="unit in quotaUnits" :key="unit" :value="unit">{{ unit }}</option>
          </select>
        </div>
      </div>
      <button class="primary-button" type="submit" :disabled="saving || disabled">{{ saving ? '创建中…' : '创建密钥' }}</button>
    </form>
    <p v-if="error" class="error-text panel-form-error">{{ error }}</p>

    <div class="table-scroll panel-section-table">
      <table class="data-table panel-resource-table panel-credentials-table">
        <thead>
          <tr><th>Access Key</th><th>名称</th><th>绑定 Bucket</th><th>状态</th><th>配额</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-if="loading" class="state-row"><td colspan="6">加载中…</td></tr>
          <tr v-else-if="credentials.length === 0" class="state-row"><td colspan="6">暂无节点密钥。</td></tr>
          <tr v-for="credential in credentials" :key="credential.id">
            <td><code>{{ credential.access_key }}</code></td>
            <td>{{ credential.name || '未命名' }}</td>
            <td>{{ credential.bucket || '全部 Bucket' }}</td>
            <td><span :class="['status-badge', credential.status === 'enabled' ? 'status-enabled' : 'status-disabled']">{{ credential.status === 'enabled' ? '启用' : '禁用' }}</span></td>
            <td>{{ formatQuota(credential.quota_bytes) }}</td>
            <td>
              <div class="actions-cell">
                <button class="secondary-button" type="button" :disabled="saving || disabled" @click="openEdit(credential)">编辑</button>
                <button class="secondary-button" type="button" :disabled="saving || disabled" @click="toggleCredential(credential)">{{ credential.status === 'enabled' ? '禁用' : '启用' }}</button>
                <button class="secondary-button" type="button" :disabled="saving || disabled" @click="rotateCredential(credential)">轮换 Secret</button>
                <button class="danger-button" type="button" :disabled="saving || disabled" @click="deleteCredential(credential)">删除</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="editing" class="modal-backdrop" @click.self="closeEdit">
      <form class="modal-card" @submit.prevent="saveEdit">
        <h2>编辑密钥</h2>
        <p class="muted"><code>{{ editing.access_key }}</code></p>
        <label for="panel-edit-credential-name">名称</label>
        <input id="panel-edit-credential-name" v-model="editForm.name" type="text" maxlength="128" :disabled="saving" />
        <label for="panel-edit-credential-bucket">绑定 Bucket</label>
        <select id="panel-edit-credential-bucket" v-model="editForm.bucket" :disabled="saving">
          <option value="">全部 Bucket</option>
          <option v-if="editForm.bucket && !bucketNames.has(editForm.bucket)" :value="editForm.bucket" disabled>{{ editForm.bucket }}（声明已不存在）</option>
          <option v-for="bucket in buckets" :key="bucket.name" :value="bucket.name">{{ bucket.name }}</option>
        </select>
        <label for="panel-edit-credential-status">状态</label>
        <select id="panel-edit-credential-status" v-model="editForm.status" :disabled="saving">
          <option value="enabled">启用</option>
          <option value="disabled">禁用</option>
        </select>
        <label for="panel-edit-credential-quota">容量配额（0 表示不限）</label>
        <div class="quota-input">
          <input id="panel-edit-credential-quota" v-model="editForm.quotaValue" type="number" min="0" step="any" :disabled="saving" />
          <select v-model="editForm.quotaUnit" aria-label="配额单位" :disabled="saving">
            <option v-for="unit in quotaUnits" :key="unit" :value="unit">{{ unit }}</option>
          </select>
        </div>
        <p v-if="editError" class="error-text panel-form-error">{{ editError }}</p>
        <div class="modal-actions">
          <button class="secondary-button" type="button" :disabled="saving" @click="closeEdit">取消</button>
          <button class="primary-button" type="submit" :disabled="saving">{{ saving ? '保存中…' : '保存' }}</button>
        </div>
      </form>
    </div>

    <div v-if="secretResult" class="modal-backdrop" @click.self="closeSecret">
      <section class="modal-card secret-modal">
        <h2>{{ secretResult.title }}</h2>
        <p class="muted">Secret Key 只显示这一次。关闭后将从组件状态清除。</p>
        <label>Access Key</label>
        <pre>{{ secretResult.accessKey }}</pre>
        <label>Secret Key</label>
        <pre>{{ secretResult.secretKey }}</pre>
        <div class="modal-actions"><button class="primary-button" type="button" @click="closeSecret">我已保存</button></div>
      </section>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { adminApi, type PanelBucket, type PanelCredential } from '../../api/client'
import { formatQuota, parseQuotaToBytes, quotaInputFromBytes, quotaUnits, type QuotaUnit } from '../../utils/format'

const props = defineProps<{ nodeId: number; disabled: boolean; refreshKey: number }>()
const emit = defineEmits<{ changed: [] }>()

const credentials = ref<PanelCredential[]>([])
const buckets = ref<PanelBucket[]>([])
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const editing = ref<PanelCredential | null>(null)
const editError = ref('')
const secretResult = ref<{ title: string; accessKey: string; secretKey: string } | null>(null)
const bucketNames = computed(() => new Set(buckets.value.map((bucket) => bucket.name)))
const createForm = reactive<{ name: string; bucket: string; quotaValue: string | number; quotaUnit: QuotaUnit }>({
  name: '', bucket: '', quotaValue: '0', quotaUnit: 'GB'
})
const editForm = reactive<{ name: string; bucket: string; status: 'enabled' | 'disabled'; quotaValue: string | number; quotaUnit: QuotaUnit }>({
  name: '', bucket: '', status: 'enabled', quotaValue: '0', quotaUnit: 'GB'
})

onMounted(() => void load())
watch(() => props.refreshKey, () => void load())

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [nextCredentials, nextBuckets] = await Promise.all([
      adminApi.listNodeCredentials(props.nodeId),
      adminApi.listNodeBuckets(props.nodeId)
    ])
    credentials.value = nextCredentials
    buckets.value = nextBuckets
  } catch (err) {
    error.value = messageFromError(err, '加载节点密钥失败')
  } finally {
    loading.value = false
  }
}

async function createCredential() {
  const quota = parseQuotaToBytes(createForm.quotaValue, createForm.quotaUnit)
  if (quota === null) {
    error.value = '配额必须是非负数，换算后需为整数字节且不能超过安全范围。'
    return
  }
  saving.value = true
  error.value = ''
  try {
    const created = await adminApi.createNodeCredential(props.nodeId, {
      name: createForm.name.trim(), bucket: createForm.bucket, quota_bytes: quota
    })
    secretResult.value = { title: '请立即保存 Secret Key', accessKey: created.access_key, secretKey: created.secret_key }
    createForm.name = ''
    createForm.bucket = ''
    createForm.quotaValue = '0'
    createForm.quotaUnit = 'GB'
    await load()
    emit('changed')
  } catch (err) {
    error.value = messageFromError(err, '创建节点密钥失败')
  } finally {
    saving.value = false
  }
}

function openEdit(credential: PanelCredential) {
  editing.value = credential
  editError.value = ''
  editForm.name = credential.name
  editForm.bucket = credential.bucket
  editForm.status = credential.status
  const quota = quotaInputFromBytes(credential.quota_bytes)
  editForm.quotaValue = quota.value
  editForm.quotaUnit = quota.unit
}

function closeEdit() {
  editing.value = null
  editError.value = ''
}

async function saveEdit() {
  if (!editing.value) return
  const quota = parseQuotaToBytes(editForm.quotaValue, editForm.quotaUnit)
  if (quota === null) {
    editError.value = '配额必须是非负数，换算后需为整数字节且不能超过安全范围。'
    return
  }
  if (editForm.bucket && !bucketNames.value.has(editForm.bucket)) {
    editError.value = '当前 Bucket 声明已不存在，请选择现有 Bucket 或改为全部 Bucket。'
    return
  }
  saving.value = true
  editError.value = ''
  try {
    await adminApi.updateNodeCredential(props.nodeId, editing.value.access_key, {
      name: editForm.name.trim(), bucket: editForm.bucket, status: editForm.status, quota_bytes: quota
    })
    closeEdit()
    await load()
    emit('changed')
  } catch (err) {
    editError.value = messageFromError(err, '保存密钥失败')
  } finally {
    saving.value = false
  }
}

async function toggleCredential(credential: PanelCredential) {
  saving.value = true
  error.value = ''
  try {
    await adminApi.updateNodeCredential(props.nodeId, credential.access_key, {
      status: credential.status === 'enabled' ? 'disabled' : 'enabled'
    })
    await load()
    emit('changed')
  } catch (err) {
    error.value = messageFromError(err, '更新密钥状态失败')
  } finally {
    saving.value = false
  }
}

async function rotateCredential(credential: PanelCredential) {
  if (!window.confirm(`确认轮换 ${credential.access_key} 的 Secret？旧 Secret 会在草稿发布并应用后失效。`)) return
  saving.value = true
  error.value = ''
  try {
    const rotated = await adminApi.rotateNodeCredential(props.nodeId, credential.access_key)
    secretResult.value = { title: '请立即保存新的 Secret Key', accessKey: rotated.access_key, secretKey: rotated.secret_key }
    await load()
    emit('changed')
  } catch (err) {
    error.value = messageFromError(err, '轮换 Secret 失败')
  } finally {
    saving.value = false
  }
}

async function deleteCredential(credential: PanelCredential) {
  if (!window.confirm(`确认删除密钥 ${credential.access_key}？发布草稿后，该密钥将无法继续访问 S3。`)) return
  saving.value = true
  error.value = ''
  try {
    await adminApi.deleteNodeCredential(props.nodeId, credential.access_key)
    await load()
    emit('changed')
  } catch (err) {
    error.value = messageFromError(err, '删除密钥失败')
  } finally {
    saving.value = false
  }
}

function closeSecret() {
  secretResult.value = null
}

function messageFromError(err: unknown, fallback: string) {
  return err instanceof Error ? err.message : fallback
}
</script>
