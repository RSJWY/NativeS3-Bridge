<template>
  <section class="panel panel-detail-section">
    <div class="panel-section-heading">
      <div>
        <h2>Bucket 与 ACL</h2>
        <p class="muted">删除只移除受管声明；发布并应用后，节点磁盘对象仍会保留，但不再可列出或访问。</p>
      </div>
    </div>

    <form class="panel-resource-form panel-resource-form-compact" @submit.prevent="createBucket">
      <div class="form-field">
        <label for="panel-bucket-name">Bucket 名称</label>
        <input id="panel-bucket-name" v-model="form.name" type="text" maxlength="63" placeholder="例如 archive-data" :disabled="saving || disabled" />
      </div>
      <div class="form-field">
        <label for="panel-bucket-acl">访问权限</label>
        <select id="panel-bucket-acl" v-model="form.acl" :disabled="saving || disabled">
          <option value="private">私有</option>
          <option value="public-read">公开下载</option>
        </select>
      </div>
      <button class="primary-button" type="submit" :disabled="saving || disabled || !form.name.trim()">
        {{ saving ? '创建中…' : '创建 Bucket' }}
      </button>
    </form>
    <p v-if="error" class="error-text panel-form-error">{{ error }}</p>

    <div class="table-scroll panel-section-table">
      <table class="data-table panel-resource-table">
        <thead>
          <tr><th>名称</th><th>ACL</th><th>创建时间</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-if="loading" class="state-row"><td colspan="4">加载中…</td></tr>
          <tr v-else-if="buckets.length === 0" class="state-row"><td colspan="4">暂无受管 Bucket。</td></tr>
          <tr v-for="bucket in buckets" :key="bucket.name">
            <td><code>{{ bucket.name }}</code></td>
            <td>
              <select :value="bucket.acl" :disabled="saving || disabled" aria-label="Bucket ACL" @change="changeACL(bucket, $event)">
                <option value="private">私有</option>
                <option value="public-read">公开下载</option>
              </select>
              <p v-if="bucket.acl === 'public-read'" class="table-help">对象可被匿名下载。</p>
            </td>
            <td>{{ formatDate(bucket.created_at) }}</td>
            <td>
              <button class="danger-button" type="button" :disabled="saving || disabled" @click="deleteBucket(bucket)">删除声明</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref, watch } from 'vue'
import { ApiError, adminApi, type BucketACL, type PanelBucket } from '../../api/client'

const props = defineProps<{ nodeId: number; disabled: boolean; refreshKey: number }>()
const emit = defineEmits<{ changed: [] }>()

const buckets = ref<PanelBucket[]>([])
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const form = reactive<{ name: string; acl: BucketACL }>({ name: '', acl: 'private' })

onMounted(() => void load())
watch(() => props.refreshKey, () => void load())

async function load() {
  loading.value = true
  error.value = ''
  try {
    buckets.value = await adminApi.listNodeBuckets(props.nodeId)
  } catch (err) {
    error.value = messageFromError(err, '加载 Bucket 失败')
  } finally {
    loading.value = false
  }
}

async function createBucket() {
  if (saving.value) return
  saving.value = true
  error.value = ''
  try {
    await adminApi.createNodeBucket(props.nodeId, { name: form.name.trim(), acl: form.acl })
    form.name = ''
    form.acl = 'private'
    await load()
    emit('changed')
  } catch (err) {
    error.value = messageFromError(err, '创建 Bucket 失败')
  } finally {
    saving.value = false
  }
}

async function changeACL(bucket: PanelBucket, event: Event) {
  const target = event.target as HTMLSelectElement
  const acl = target.value as BucketACL
  saving.value = true
  error.value = ''
  try {
    await adminApi.updateNodeBucketACL(props.nodeId, bucket.name, acl)
    await load()
    emit('changed')
  } catch (err) {
    target.value = bucket.acl
    error.value = messageFromError(err, '更新 ACL 失败')
  } finally {
    saving.value = false
  }
}

async function deleteBucket(bucket: PanelBucket) {
  const confirmed = window.confirm(`确认删除受管 Bucket「${bucket.name}」？\n\n此操作不会删除节点磁盘中的对象；发布草稿并由节点应用后，Bucket 才会从 S3 视图隐藏且无法访问。`)
  if (!confirmed) return
  saving.value = true
  error.value = ''
  try {
    await adminApi.deleteNodeBucket(props.nodeId, bucket.name)
    await load()
    emit('changed')
  } catch (err) {
    if (err instanceof ApiError && err.status === 409) {
      error.value = '该 Bucket 仍有绑定密钥，请先解除绑定。'
    } else {
      error.value = messageFromError(err, '删除 Bucket 失败')
    }
  } finally {
    saving.value = false
  }
}

function formatDate(value: string) {
  return new Date(value).toLocaleString()
}

function messageFromError(err: unknown, fallback: string) {
  return err instanceof Error ? err.message : fallback
}
</script>
