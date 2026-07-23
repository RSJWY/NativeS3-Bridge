<template>
  <section class="page-stack">
    <div class="page-header">
      <div>
        <RouterLink class="panel-back-link" to="/nodes">← 返回节点列表</RouterLink>
        <h1>{{ node?.display_name || '节点详情' }}</h1>
        <p class="muted">节点 ID {{ nodeID }}</p>
      </div>
      <button class="secondary-button" type="button" :disabled="loading" @click="refreshAll">{{ loading ? '刷新中…' : '刷新' }}</button>
    </div>

    <div v-if="error" class="notice error-notice">{{ error }}</div>
    <div v-if="actionMessage" class="notice info-notice">{{ actionMessage }}</div>

    <template v-if="node">
      <section class="panel panel-detail-section">
        <div class="panel-section-heading">
          <div>
            <h2>运行状态</h2>
            <p class="muted">连接、生命周期和配置版本状态。</p>
          </div>
          <div class="actions-cell">
            <button v-if="node.status !== 'retired'" class="secondary-button" type="button" :disabled="actionLoading" @click="setNodeStatus(node.status === 'active' ? 'disabled' : 'active')">
              {{ node.status === 'active' ? '停用节点' : '启用节点' }}
            </button>
            <button v-if="node.status !== 'retired'" class="danger-button" type="button" :disabled="actionLoading" @click="retireNode">永久退役</button>
          </div>
        </div>
        <dl class="node-facts">
          <div><dt>连接</dt><dd>{{ node.online ? '在线' : '离线' }}</dd></div>
          <div><dt>生命周期</dt><dd>{{ nodeStatusLabel(node.status) }}</dd></div>
          <div><dt>同步状态</dt><dd>{{ syncStateLabel(node.sync_state) }}</dd></div>
          <div><dt>草稿</dt><dd>{{ node.publish_required ? '需要重新发布' : node.draft_dirty ? '有未发布变更' : '无未发布变更' }}</dd></div>
          <div><dt>已发布版本</dt><dd>{{ node.desired_version || '—' }}</dd></div>
          <div><dt>已应用版本</dt><dd>{{ node.applied_version || '—' }}</dd></div>
          <div><dt>最近心跳</dt><dd>{{ formatDate(node.last_heartbeat) }}</dd></div>
          <div><dt>创建时间</dt><dd>{{ formatDate(node.created_at) }}</dd></div>
          <div><dt>最近同步错误</dt><dd>{{ node.last_error || '—' }}</dd></div>
        </dl>
      </section>

      <section class="panel panel-detail-section">
        <div class="panel-section-heading">
          <div>
            <h2>节点注册</h2>
            <p class="muted">令牌默认十分钟有效，明文只在签发结果中显示一次。</p>
          </div>
          <button class="primary-button" type="button" :disabled="actionLoading || node.status === 'retired'" @click="issueToken">签发注册令牌</button>
        </div>
      </section>

      <section class="panel panel-detail-section">
        <div class="panel-section-heading">
          <div>
            <h2>发布与同步</h2>
            <p class="muted">资源编辑只保存草稿；发布创建新版本，重推只发送最后已发布版本。</p>
          </div>
          <div class="actions-cell">
            <button class="secondary-button" type="button" :disabled="actionLoading || node.status === 'retired' || (!node.draft_dirty && !node.publish_required)" @click="publishDesiredState">发布草稿</button>
            <button class="primary-button" type="button" :disabled="actionLoading || !node.online || node.status !== 'active' || node.desired_version === 0 || node.publish_required" @click="pushDesiredState">重推已发布版本</button>
          </div>
        </div>
        <div v-if="node.publish_required" class="notice warning-notice panel-inline-notice">当前发布快照来自旧格式，无法安全恢复精确 Secret。请显式发布当前草稿后再推送。</div>
        <div v-else-if="node.draft_dirty" class="notice warning-notice panel-inline-notice">草稿包含未发布变更。节点重连或点击重推时仍只会收到版本 {{ node.desired_version || 0 }}。</div>
        <div v-else-if="node.desired_version > 0" class="notice info-notice panel-inline-notice">草稿与已发布版本 {{ node.desired_version }} 一致。</div>
        <div v-if="node.last_error" class="notice error-notice panel-inline-notice">{{ node.last_error }}</div>
        <div v-if="publishResult" class="operation-result">
          <strong>版本 {{ publishResult.version }}</strong>
          <code>{{ publishResult.content_hash }}</code>
          <span>{{ publishResult.pushed ? '已发送到在线节点' : publishResult.push_error || '已保存，节点上线后自动同步' }}</span>
        </div>
      </section>

      <PanelNodeImportSection :node-id="nodeID" :online="node.online" :disabled="node.status === 'retired'" :refresh-key="resourceRevision" @changed="refreshNode" @confirmed="handleImportConfirmed" />
      <PanelNodeBucketsSection :node-id="nodeID" :disabled="node.status === 'retired'" :refresh-key="resourceRevision" @changed="handleDraftChanged" />
      <PanelNodeCredentialsSection :node-id="nodeID" :disabled="node.status === 'retired'" :refresh-key="resourceRevision" @changed="handleDraftChanged" />
      <PanelNodeWebhooksSection :node-id="nodeID" :disabled="node.status === 'retired'" :refresh-key="resourceRevision" @changed="handleDraftChanged" />
      <PanelNodeRateLimitSection :node-id="nodeID" :disabled="node.status === 'retired'" :refresh-key="resourceRevision" @changed="handleDraftChanged" />

      <section class="panel panel-detail-section">
        <div class="panel-section-heading">
          <div>
            <h2>客户端证书</h2>
            <p class="muted">撤销证书会立即断开当前控制面连接。</p>
          </div>
          <button class="danger-button" type="button" :disabled="certificateLoading || activeCertificateCount === 0 || node.status === 'retired'" @click="revokeCertificates">撤销全部有效证书</button>
        </div>
        <p v-if="certificateError" class="error-text panel-form-error">{{ certificateError }}</p>
        <div class="table-scroll panel-section-table">
          <table class="data-table">
            <thead><tr><th>序列号</th><th>指纹</th><th>有效期至</th><th>状态</th></tr></thead>
            <tbody>
              <tr v-if="certificateLoading" class="state-row"><td colspan="4">加载中…</td></tr>
              <tr v-else-if="certificates.length === 0" class="state-row"><td colspan="4">暂无已签发证书。</td></tr>
              <tr v-for="certificate in certificates" :key="certificate.ID">
                <td><code>{{ certificate.Serial }}</code></td>
                <td><code class="fingerprint-code">{{ certificate.Fingerprint }}</code></td>
                <td>{{ formatDate(certificate.NotAfter) }}</td>
                <td>{{ certificate.Revoked ? '已撤销' : '有效' }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
    </template>

    <div v-if="tokenResult" class="modal-backdrop" @click.self="closeToken">
      <section class="modal-card secret-modal">
        <h2>请立即保存注册令牌</h2>
        <p class="muted">令牌只显示这一次，有效期至 {{ formatDate(tokenResult.expires_at) }}。</p>
        <pre>{{ tokenResult.token }}</pre>
        <div class="modal-actions"><button class="primary-button" type="button" @click="closeToken">我已保存</button></div>
      </section>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { adminApi, type PanelCertificate, type PanelNode, type PanelPublishResult, type PanelRegistrationToken } from '../api/client'
import PanelNodeBucketsSection from '../components/panel/PanelNodeBucketsSection.vue'
import PanelNodeCredentialsSection from '../components/panel/PanelNodeCredentialsSection.vue'
import PanelNodeImportSection from '../components/panel/PanelNodeImportSection.vue'
import PanelNodeRateLimitSection from '../components/panel/PanelNodeRateLimitSection.vue'
import PanelNodeWebhooksSection from '../components/panel/PanelNodeWebhooksSection.vue'

const route = useRoute()
const router = useRouter()
const nodeID = Number(route.params.id)
const node = ref<PanelNode | null>(null)
const certificates = ref<PanelCertificate[]>([])
const tokenResult = ref<PanelRegistrationToken | null>(null)
const publishResult = ref<PanelPublishResult | null>(null)
const resourceRevision = ref(0)
const loading = ref(false)
const actionLoading = ref(false)
const certificateLoading = ref(false)
const error = ref('')
const certificateError = ref('')
const actionMessage = ref('')
const activeCertificateCount = computed(() => certificates.value.filter((certificate) => !certificate.Revoked).length)

onMounted(() => {
  if (!Number.isSafeInteger(nodeID) || nodeID <= 0) {
    void router.replace('/nodes')
    return
  }
  void load()
})

async function load() {
  loading.value = true
  error.value = ''
  try {
    await Promise.all([refreshNode(), loadCertificates()])
  } finally {
    loading.value = false
  }
}

async function refreshAll() {
  resourceRevision.value += 1
  actionMessage.value = ''
  await load()
}

async function refreshNode() {
  try {
    node.value = await adminApi.getNode(nodeID)
  } catch (err) {
    error.value = messageFromError(err, '加载节点失败')
  }
}

async function loadCertificates() {
  certificateLoading.value = true
  certificateError.value = ''
  try {
    certificates.value = await adminApi.listNodeCertificates(nodeID)
  } catch (err) {
    certificateError.value = messageFromError(err, '加载证书失败')
  } finally {
    certificateLoading.value = false
  }
}

async function handleDraftChanged() {
  publishResult.value = null
  resourceRevision.value += 1
  await refreshNode()
}

async function handleImportConfirmed() {
  resourceRevision.value += 1
  actionMessage.value = '导入已确认，Panel 已成为该节点配置权威。'
  await refreshNode()
}

async function setNodeStatus(status: 'active' | 'disabled') {
  actionLoading.value = true
  error.value = ''
  try {
    node.value = await adminApi.updateNode(nodeID, { status })
    actionMessage.value = status === 'active' ? '节点已启用。' : '节点已停用，当前控制面连接已断开。'
  } catch (err) {
    error.value = messageFromError(err, '更新节点状态失败')
  } finally {
    actionLoading.value = false
  }
}

async function retireNode() {
  if (!window.confirm('确认永久退役该节点？此操作不可逆，并会撤销证书与未使用的注册令牌。')) return
  actionLoading.value = true
  error.value = ''
  try {
    node.value = await adminApi.retireNode(nodeID)
    actionMessage.value = '节点已永久退役。'
    await loadCertificates()
  } catch (err) {
    error.value = messageFromError(err, '退役节点失败')
  } finally {
    actionLoading.value = false
  }
}

async function issueToken() {
  actionLoading.value = true
  error.value = ''
  try {
    tokenResult.value = await adminApi.issueNodeToken(nodeID)
  } catch (err) {
    error.value = messageFromError(err, '签发注册令牌失败')
  } finally {
    actionLoading.value = false
  }
}

function closeToken() {
  tokenResult.value = null
}

async function publishDesiredState() {
  actionLoading.value = true
  error.value = ''
  actionMessage.value = ''
  try {
    publishResult.value = await adminApi.publishNodeDesiredState(nodeID)
    await refreshNode()
  } catch (err) {
    error.value = messageFromError(err, '发布草稿失败')
  } finally {
    actionLoading.value = false
  }
}

async function pushDesiredState() {
  actionLoading.value = true
  error.value = ''
  try {
    await adminApi.pushNodeDesiredState(nodeID)
    actionMessage.value = '已重推最后发布版本。'
    await refreshNode()
  } catch (err) {
    error.value = messageFromError(err, '重推已发布版本失败')
  } finally {
    actionLoading.value = false
  }
}

async function revokeCertificates() {
  if (!window.confirm('确认撤销该节点的全部有效证书？当前控制面连接会立即断开，需要重新注册。')) return
  certificateLoading.value = true
  certificateError.value = ''
  try {
    const result = await adminApi.revokeNodeCertificates(nodeID)
    actionMessage.value = `已撤销 ${result.revoked} 张证书。`
    await Promise.all([loadCertificates(), refreshNode()])
  } catch (err) {
    certificateError.value = messageFromError(err, '撤销证书失败')
  } finally {
    certificateLoading.value = false
  }
}

function nodeStatusLabel(status: PanelNode['status']) {
  if (status === 'active') return '启用'
  if (status === 'disabled') return '停用'
  return '已退役'
}

function syncStateLabel(state: PanelNode['sync_state']) {
  const labels: Record<PanelNode['sync_state'], string> = {
    synced: '已同步', waiting: '等待同步', failed: '同步失败', drift: '配置漂移', '': '尚未上报'
  }
  return labels[state]
}

function formatDate(value?: string) {
  return value ? new Date(value).toLocaleString() : '—'
}

function messageFromError(err: unknown, fallback: string) {
  return err instanceof Error ? err.message : fallback
}
</script>
