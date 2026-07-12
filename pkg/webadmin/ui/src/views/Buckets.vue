<template>
  <section class="page-stack">
    <div class="page-header">
      <div>
        <h1>桶管理</h1>
        <p class="muted">创建 bucket，删除空 bucket，或设置私有 / 公开下载访问。</p>
      </div>
    </div>

    <div v-if="error" class="notice error-notice">{{ error }}</div>

    <section class="panel form-panel">
      <form class="inline-form" @submit.prevent="create">
        <div class="form-field">
          <label for="bucket-name">新建桶</label>
          <input
            id="bucket-name"
            v-model="newBucketName"
            type="text"
            autocomplete="off"
            placeholder="例如：photos-archive"
            :disabled="bucketMutating"
          />
          <p class="muted">名称需为 3-63 位小写字母、数字或连字符，并以字母或数字开头和结尾。</p>
        </div>
        <button class="primary-button" type="submit" :disabled="bucketMutating">{{ creating ? '创建中…' : '新建桶' }}</button>
      </form>
      <p v-if="formError" class="error-text">{{ formError }}</p>
    </section>

    <section class="panel">
      <div class="table-scroll">
        <table class="data-table">
        <thead>
          <tr>
            <th>名称</th>
            <th>ACL</th>
            <th>创建时间</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="loading" class="state-row">
            <td colspan="4">加载中…</td>
          </tr>
          <tr v-else-if="buckets.length === 0" class="state-row">
            <td colspan="4">暂无 bucket。</td>
          </tr>
          <tr v-for="bucket in buckets" :key="bucket.name">
            <td><code>{{ bucket.name }}</code></td>
            <td>
              <span :class="['acl-badge', bucket.acl === 'public-read' ? 'acl-public' : 'acl-private']">
                {{ aclText(bucket.acl) }}
              </span>
              <p v-if="bucket.acl === 'public-read'" class="table-help">该桶对象可被任何人匿名 GET 下载。</p>
            </td>
            <td>{{ formatDate(bucket.created_at) }}</td>
            <td class="actions-cell">
              <label class="acl-select-label" :for="`acl-${bucket.name}`">访问权限</label>
              <select
                :id="`acl-${bucket.name}`"
                :value="bucket.acl"
                :disabled="bucketMutating"
                @change="changeACL(bucket, $event)"
              >
                <option value="private">私有</option>
                <option value="public-read">公开下载</option>
              </select>
              <button class="danger-button" type="button" :disabled="bucketMutating" @click="remove(bucket)">
                {{ deleting === bucket.name ? '删除中…' : '删除' }}
              </button>
              <button class="secondary-button" type="button" :disabled="bucketMutating" @click="reconcile(bucket)">
                {{ reconciling === bucket.name ? '扫描中…' : '存储对账' }}
              </button>
            </td>
          </tr>
        </tbody>
        </table>
      </div>
    </section>

    <section v-if="reconcileReport" class="panel reconcile-panel">
      <div class="reconcile-heading">
        <div>
          <h2>{{ reconcileReport.bucket }} 对账报告</h2>
          <p class="muted">扫描 {{ reconcileReport.object_count }} 个对象，共 {{ formatBytes(reconcileReport.scanned_bytes) }}。</p>
        </div>
        <button class="text-button" type="button" @click="reconcileReport = null">关闭</button>
      </div>
      <div class="reconcile-summary">
        <span>孤儿 sidecar：{{ reconcileReport.orphan_sidecar_count }}</span>
        <span>绑定密钥：{{ reconcileReport.bound_credentials.length }}</span>
      </div>
      <div v-if="reconcileReport.bound_credentials.length" class="table-scroll">
        <table class="data-table reconcile-table">
          <thead><tr><th>密钥</th><th>当前用量</th><th>差异</th></tr></thead>
          <tbody>
            <tr v-for="credential in reconcileReport.bound_credentials" :key="credential.id">
              <td>{{ credential.name || '未命名' }} <code>{{ credential.access_key }}</code></td>
              <td>{{ formatBytes(credential.used_bytes) }}</td>
              <td>{{ formatSignedBytes(credential.diff_bytes) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-if="reconcileReport.orphan_sidecar_samples.length" class="muted">
        孤儿示例：<code>{{ reconcileReport.orphan_sidecar_samples.join('、') }}</code>
      </p>
      <div class="reconcile-actions">
        <p>执行校正会删除孤儿 sidecar，并将绑定密钥的用量设为本次扫描值；不会恢复已从磁盘删除的对象。</p>
        <button v-if="needsApply" class="danger-button" type="button" :disabled="applying" @click="applyReconcile">
          {{ applying ? '校正中…' : '执行校正' }}
        </button>
        <span v-else class="success-text">无需校正</span>
      </div>
    </section>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { adminApi, type Bucket, type BucketACL, type ReconcileReport } from '../api/client'
import { formatBytes } from '../utils/format'

const buckets = ref<Bucket[]>([])
const loading = ref(false)
const creating = ref(false)
const deleting = ref('')
const updatingACL = ref('')
const reconciling = ref('')
const applying = ref(false)
const reconcileReport = ref<ReconcileReport | null>(null)
const error = ref('')
const formError = ref('')
const newBucketName = ref('')
const bucketMutating = computed(() => creating.value || deleting.value !== '' || updatingACL.value !== '' || reconciling.value !== '' || applying.value)
const needsApply = computed(() => reconcileReport.value !== null && (reconcileReport.value.orphan_sidecar_count > 0 || reconcileReport.value.bound_credentials.some((credential) => credential.diff_bytes !== 0)))

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    buckets.value = await adminApi.listBuckets()
  } catch (err) {
    error.value = toBucketError(err, '加载桶列表失败')
  } finally {
    loading.value = false
  }
}

async function create() {
  if (bucketMutating.value) return
  const name = newBucketName.value.trim()
  if (!name) {
    formError.value = '请输入桶名称'
    return
  }
  if (!isValidBucketName(name)) {
    formError.value = '桶名称不合法：需为 3-63 位小写字母、数字或连字符，并以字母或数字开头和结尾。'
    return
  }
  creating.value = true
  formError.value = ''
  try {
    await adminApi.createBucket({ name })
    newBucketName.value = ''
    await load()
  } catch (err) {
    formError.value = toBucketError(err, '创建桶失败')
  } finally {
    creating.value = false
  }
}

async function changeACL(bucket: Bucket, event: Event) {
  if (updatingACL.value || deleting.value) return
  const target = event.target as HTMLSelectElement
  const nextACL = target.value as BucketACL
  if (nextACL === bucket.acl) {
    return
  }
  updatingACL.value = bucket.name
  error.value = ''
  try {
    await adminApi.setBucketACL(bucket.name, nextACL)
    await load()
  } catch (err) {
    target.value = bucket.acl
    error.value = toBucketError(err, '更新访问权限失败')
  } finally {
    updatingACL.value = ''
  }
}

async function remove(bucket: Bucket) {
  if (deleting.value || updatingACL.value) return
  if (!window.confirm(`确认删除桶 ${bucket.name}？仅空桶可删；若仍有密钥绑定该桶，将无法删除。`)) {
    return
  }
  deleting.value = bucket.name
  error.value = ''
  try {
    await adminApi.deleteBucket(bucket.name)
    await load()
  } catch (err) {
    error.value = toBucketError(err, '删除桶失败')
  } finally {
    deleting.value = ''
  }
}

async function reconcile(bucket: Bucket) {
  if (bucketMutating.value) return
  reconciling.value = bucket.name
  error.value = ''
  try {
    reconcileReport.value = await adminApi.reconcileBucket(bucket.name, false)
  } catch (err) {
    error.value = toBucketError(err, '存储对账失败')
  } finally {
    reconciling.value = ''
  }
}

async function applyReconcile() {
  const report = reconcileReport.value
  if (!report || applying.value) return
  const message = `确认校正 ${report.bucket}？\n将删除 ${report.orphan_sidecar_count} 个孤儿 sidecar，并将 ${report.bound_credentials.length} 把绑定密钥的 used_bytes 设为 ${formatBytes(report.scanned_bytes)}。\n此操作无法恢复已从磁盘删除的文件。`
  if (!window.confirm(message)) return
  applying.value = true
  error.value = ''
  try {
    reconcileReport.value = await adminApi.reconcileBucket(report.bucket, true)
  } catch (err) {
    error.value = toBucketError(err, '执行存储校正失败')
  } finally {
    applying.value = false
  }
}

function formatSignedBytes(value: number) {
  if (value === 0) return '一致'
  return `${value > 0 ? '+' : '-'}${formatBytes(Math.abs(value))}`
}

function aclText(acl: BucketACL) {
  return acl === 'public-read' ? '公开下载' : '私有'
}

function formatDate(value: string) {
  return new Date(value).toLocaleString()
}

function isValidBucketName(name: string) {
  return /^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$/.test(name)
}

function toBucketError(err: unknown, fallback: string) {
  const message = err instanceof Error ? err.message : ''
  switch (message) {
    case 'invalid bucket name':
      return '桶名称不合法：需为 3-63 位小写字母、数字或连字符，并以字母或数字开头和结尾。'
    case 'bucket not empty':
      return '桶非空，无法删除。请先删除桶内对象后再重试。'
    case 'bucket has bound credentials':
      return '该桶仍有密钥绑定，请先在密钥管理中解绑或改绑后再删除。'
    case 'bucket not found':
      return '桶不存在，请刷新列表后重试。'
    case 'acl must be private or public-read':
      return 'ACL 只能选择私有或公开下载。'
    default:
      return message || fallback
  }
}
</script>
