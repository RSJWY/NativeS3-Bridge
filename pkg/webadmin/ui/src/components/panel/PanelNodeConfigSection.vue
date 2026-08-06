<template>
  <section class="panel panel-detail-section">
    <div class="panel-section-heading">
      <div>
        <h2>已发布配置</h2>
        <p class="muted">
          面板最后一次显式发布的权威快照，也是节点重连时会收到的内容。
          <strong>不代表节点当前实际生效的配置</strong>——面板只能通过版本号与内容哈希判断是否一致。
        </p>
      </div>
    </div>

    <p v-if="loading" class="muted">加载中…</p>
    <p v-if="error" class="error-text panel-form-error">{{ error }}</p>

    <template v-else-if="snapshot">
      <!-- 无已发布快照 -->
      <p v-if="!snapshot.published" class="muted">
        尚未发布任何配置。资源编辑只保存草稿，需点击「发布草稿」后才会下发到节点。
      </p>

      <template v-else>
        <!-- 旧格式：需重新发布 -->
        <div v-if="snapshot.republish_needed" class="notice warning-notice panel-inline-notice">
          已发布快照为旧格式，无法安全解析。请重新发布当前草稿后才能查看与推送。
        </div>

        <template v-else>
          <dl class="node-facts">
            <div><dt>版本</dt><dd>{{ snapshot.version }}</dd></div>
            <div><dt>内容哈希</dt><dd><code class="fingerprint-code">{{ snapshot.content_hash }}</code></dd></div>
            <div><dt>发布者</dt><dd>{{ snapshot.updated_by || '-' }}</dd></div>
            <div><dt>发布时间</dt><dd>{{ formatDate(snapshot.updated_at) }}</dd></div>
          </dl>

          <div class="table-scroll panel-section-table">
            <table class="data-table panel-resource-table">
              <thead>
                <tr><th>名称</th><th>ACL</th></tr>
              </thead>
              <tbody>
                <tr v-if="snapshot.buckets.length === 0" class="state-row"><td colspan="2">本次发布未声明任何桶。</td></tr>
                <tr v-for="bucket in snapshot.buckets" :key="bucket.name">
                  <td><code>{{ bucket.name }}</code></td>
                  <td>{{ bucket.acl === 'public-read' ? '公开下载' : '私有' }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <div class="table-scroll panel-section-table">
            <table class="data-table panel-resource-table">
              <thead>
                <tr><th>Access Key</th><th>名称</th><th>绑定桶</th><th>状态</th><th>配额</th></tr>
              </thead>
              <tbody>
                <tr v-if="snapshot.credentials.length === 0" class="state-row"><td colspan="5">本次发布未声明任何密钥。</td></tr>
                <tr v-for="credential in snapshot.credentials" :key="credential.access_key">
                  <td><code>{{ credential.access_key }}</code></td>
                  <td>{{ credential.name || '未命名' }}</td>
                  <td>{{ credential.bucket || '全部桶' }}</td>
                  <td><span :class="['status-badge', credential.status === 'enabled' ? 'status-enabled' : 'status-disabled']">{{ credential.status === 'enabled' ? '启用' : '禁用' }}</span></td>
                  <td>{{ formatQuota(credential.quota_bytes) }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <div class="table-scroll panel-section-table">
            <table class="data-table panel-resource-table">
              <thead>
                <tr><th>URL</th><th>事件</th><th>启用</th></tr>
              </thead>
              <tbody>
                <tr v-if="snapshot.webhooks.length === 0" class="state-row"><td colspan="3">本次发布未声明任何 Webhook。</td></tr>
                <tr v-for="(webhook, index) in snapshot.webhooks" :key="`${webhook.url}-${index}`">
                  <td><code>{{ webhook.url }}</code></td>
                  <td>{{ webhook.events.length > 0 ? webhook.events.join('，') : '无' }}</td>
                  <td>{{ webhook.enabled ? '是' : '否' }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <dl v-if="snapshot.rate_limit" class="node-facts">
            <div><dt>匿名 RPS</dt><dd>{{ snapshot.rate_limit.anonymous_rps }}</dd></div>
            <div><dt>突发容量</dt><dd>{{ snapshot.rate_limit.anonymous_burst }}</dd></div>
            <div><dt>转发头信任</dt><dd>{{ snapshot.rate_limit.trust_forwarded ? '信任' : '不信任' }}</dd></div>
          </dl>
          <p v-else class="muted">未配置匿名限流，节点使用内置默认值。</p>
        </template>
      </template>
    </template>
  </section>
</template>

<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { adminApi, type PanelPublishedSnapshot } from '../../api/client'
import { formatQuota } from '../../utils/format'

const props = defineProps<{ nodeId: number; disabled: boolean; refreshKey: number }>()

const snapshot = ref<PanelPublishedSnapshot | null>(null)
const loading = ref(false)
const error = ref('')

onMounted(() => void load())
watch(() => props.refreshKey, () => void load())

async function load() {
  loading.value = true
  error.value = ''
  try {
    snapshot.value = await adminApi.getNodeDesiredState(props.nodeId)
  } catch (err) {
    error.value = messageFromError(err, '加载已发布配置失败')
  } finally {
    loading.value = false
  }
}

function formatDate(value?: string) {
  return value ? new Date(value).toLocaleString() : '-'
}

function messageFromError(err: unknown, fallback: string) {
  return err instanceof Error ? err.message : fallback
}
</script>
