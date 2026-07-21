<template>
  <section class="page-stack">
    <div class="page-header">
      <div>
        <RouterLink class="panel-back-link" to="/nodes">← 返回节点列表</RouterLink>
        <h1>{{ node?.display_name || '节点详情' }}</h1>
        <p class="muted">节点 ID {{ nodeID }}</p>
      </div>
      <button class="secondary-button" type="button" :disabled="loading" @click="load">
        {{ loading ? '刷新中…' : '刷新' }}
      </button>
    </div>

    <div v-if="error" class="notice error-notice">{{ error }}</div>
    <div v-if="actionMessage" class="notice info-notice">{{ actionMessage }}</div>

    <template v-if="node">
      <section class="panel panel-detail-section">
        <div class="panel-section-heading">
          <div>
            <h2>运行状态</h2>
            <p class="muted">连接状态与期望配置版本。</p>
          </div>
          <div class="actions-cell">
            <button
              v-if="node.status !== 'retired'"
              class="secondary-button"
              type="button"
              :disabled="actionLoading"
              @click="setNodeStatus(node.status === 'active' ? 'disabled' : 'active')"
            >
              {{ node.status === 'active' ? '停用节点' : '启用节点' }}
            </button>
            <button
              v-if="node.status !== 'retired'"
              class="danger-button"
              type="button"
              :disabled="actionLoading"
              @click="retireNode"
            >
              永久退役
            </button>
          </div>
        </div>
        <dl class="node-facts">
          <div>
            <dt>连接</dt>
            <dd>{{ node.online ? '在线' : '离线' }}</dd>
          </div>
          <div>
            <dt>生命周期</dt>
            <dd>{{ nodeStatusLabel(node.status) }}</dd>
          </div>
          <div>
            <dt>同步状态</dt>
            <dd>{{ syncStateLabel(node.sync_state) }}</dd>
          </div>
          <div>
            <dt>应用 / 期望版本</dt>
            <dd>{{ node.applied_version }} / {{ node.desired_version }}</dd>
          </div>
          <div>
            <dt>最近心跳</dt>
            <dd>{{ formatDate(node.last_heartbeat) }}</dd>
          </div>
          <div>
            <dt>创建时间</dt>
            <dd>{{ formatDate(node.created_at) }}</dd>
          </div>
        </dl>
      </section>

      <section class="panel panel-detail-section">
        <div class="panel-section-heading">
          <div>
            <h2>节点注册</h2>
            <p class="muted">令牌默认十分钟有效，明文只在签发结果中显示一次。</p>
          </div>
          <button
            class="primary-button"
            type="button"
            :disabled="actionLoading || node.status === 'retired'"
            @click="issueToken"
          >
            签发注册令牌
          </button>
        </div>
      </section>

      <section class="panel panel-detail-section">
        <div class="panel-section-heading">
          <div>
            <h2>期望状态</h2>
            <p class="muted">凭证变更后发布新版本；在线节点可立即重推。</p>
          </div>
          <div class="actions-cell">
            <button
              class="secondary-button"
              type="button"
              :disabled="actionLoading || node.status === 'retired'"
              @click="publishDesiredState"
            >
              发布新版本
            </button>
            <button
              class="primary-button"
              type="button"
              :disabled="actionLoading || !node.online || node.status !== 'active' || node.desired_version === 0"
              @click="pushDesiredState"
            >
              推送当前版本
            </button>
          </div>
        </div>
        <div v-if="publishResult" class="operation-result">
          <strong>版本 {{ publishResult.version }}</strong>
          <code>{{ publishResult.content_hash }}</code>
          <span>{{ publishResult.pushed ? '已推送到在线节点' : publishResult.push_error || '已保存，节点上线后自动同步' }}</span>
        </div>
      </section>

      <section class="panel panel-detail-section">
        <div class="panel-section-heading">
          <div>
            <h2>S3 密钥</h2>
            <p class="muted">Secret 仅在创建或轮换时显示一次。</p>
          </div>
        </div>
        <form class="panel-credential-form" @submit.prevent="createCredential">
          <div class="form-field">
            <label for="panel-credential-name">名称</label>
            <input id="panel-credential-name" v-model="credentialForm.name" type="text" maxlength="128" :disabled="credentialSaving" />
          </div>
          <div class="form-field">
            <label for="panel-credential-bucket">绑定桶</label>
            <input
              id="panel-credential-bucket"
              v-model="credentialForm.bucket"
              type="text"
              maxlength="63"
              placeholder="留空表示全部桶"
              :disabled="credentialSaving"
            />
          </div>
          <div class="form-field">
            <label for="panel-quota-value">容量配额（0 表示不限）</label>
            <div class="quota-input">
              <input id="panel-quota-value" v-model="credentialForm.quotaValue" type="number" min="0" step="any" :disabled="credentialSaving" />
              <select v-model="credentialForm.quotaUnit" aria-label="配额单位" :disabled="credentialSaving">
                <option v-for="unit in quotaUnits" :key="unit" :value="unit">{{ unit }}</option>
              </select>
            </div>
          </div>
          <button class="primary-button" type="submit" :disabled="credentialSaving || node.status === 'retired'">
            {{ credentialSaving ? '创建中…' : '创建密钥' }}
          </button>
        </form>
        <p v-if="credentialError" class="error-text panel-form-error">{{ credentialError }}</p>

        <div class="table-scroll panel-section-table">
          <table class="data-table">
            <thead>
              <tr>
                <th>Access Key</th>
                <th>名称</th>
                <th>绑定桶</th>
                <th>状态</th>
                <th>配额</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="credentialsLoading" class="state-row"><td colspan="6">加载中…</td></tr>
              <tr v-else-if="credentials.length === 0" class="state-row"><td colspan="6">暂无节点密钥。</td></tr>
              <tr v-for="credential in credentials" :key="credential.id">
                <td><code>{{ credential.access_key }}</code></td>
                <td>{{ credential.name || '未命名' }}</td>
                <td>{{ credential.bucket || '全部桶' }}</td>
                <td>{{ credential.status === 'enabled' ? '启用' : '禁用' }}</td>
                <td>{{ formatQuota(credential.quota_bytes) }}</td>
                <td>
                  <button
                    class="secondary-button"
                    type="button"
                    :disabled="credentialSaving || node.status === 'retired'"
                    @click="rotateCredential(credential)"
                  >
                    轮换 Secret
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <section class="panel panel-detail-section">
        <div class="panel-section-heading">
          <div>
            <h2>客户端证书</h2>
            <p class="muted">撤销证书会立即断开当前控制面连接。</p>
          </div>
          <button
            class="danger-button"
            type="button"
            :disabled="certificateLoading || activeCertificateCount === 0 || node.status === 'retired'"
            @click="revokeCertificates"
          >
            撤销全部有效证书
          </button>
        </div>
        <p v-if="certificateError" class="error-text panel-form-error">{{ certificateError }}</p>
        <div class="table-scroll panel-section-table">
          <table class="data-table">
            <thead>
              <tr><th>序列号</th><th>指纹</th><th>有效期至</th><th>状态</th></tr>
            </thead>
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

    <div v-if="tokenResult" class="modal-backdrop" @click.self="tokenResult = null">
      <section class="modal-card secret-modal">
        <h2>请立即保存注册令牌</h2>
        <p class="muted">令牌只显示这一次，有效期至 {{ formatDate(tokenResult.expires_at) }}。</p>
        <pre>{{ tokenResult.token }}</pre>
        <div class="modal-actions"><button class="primary-button" type="button" @click="tokenResult = null">我已保存</button></div>
      </section>
    </div>

    <div v-if="secretResult" class="modal-backdrop" @click.self="secretResult = null">
      <section class="modal-card secret-modal">
        <h2>{{ secretResult.title }}</h2>
        <p class="muted">Secret Key 只显示这一次。保存后请发布期望状态。</p>
        <label>Access Key</label>
        <pre>{{ secretResult.accessKey }}</pre>
        <label>Secret Key</label>
        <pre>{{ secretResult.secretKey }}</pre>
        <div class="modal-actions"><button class="primary-button" type="button" @click="secretResult = null">我已保存</button></div>
      </section>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  adminApi,
  type PanelCertificate,
  type PanelCredential,
  type PanelNode,
  type PanelPublishResult,
  type PanelRegistrationToken
} from '../api/client'
import { formatQuota, parseQuotaToBytes, quotaUnits, type QuotaUnit } from '../utils/format'

const route = useRoute()
const router = useRouter()
const nodeID = Number(route.params.id)
const node = ref<PanelNode | null>(null)
const credentials = ref<PanelCredential[]>([])
const certificates = ref<PanelCertificate[]>([])
const tokenResult = ref<PanelRegistrationToken | null>(null)
const secretResult = ref<{ title: string; accessKey: string; secretKey: string } | null>(null)
const publishResult = ref<PanelPublishResult | null>(null)
const loading = ref(false)
const actionLoading = ref(false)
const credentialsLoading = ref(false)
const credentialSaving = ref(false)
const certificateLoading = ref(false)
const error = ref('')
const credentialError = ref('')
const certificateError = ref('')
const actionMessage = ref('')
const credentialForm = reactive<{ name: string; bucket: string; quotaValue: string | number; quotaUnit: QuotaUnit }>({
  name: '',
  bucket: '',
  quotaValue: '0',
  quotaUnit: 'GB'
})
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
  actionMessage.value = ''
  try {
    node.value = await adminApi.getNode(nodeID)
    await Promise.all([loadCredentials(), loadCertificates()])
  } catch (err) {
    error.value = messageFromError(err, '加载节点失败')
  } finally {
    loading.value = false
  }
}

async function loadCredentials() {
  credentialsLoading.value = true
  credentialError.value = ''
  try {
    credentials.value = await adminApi.listNodeCredentials(nodeID)
  } catch (err) {
    credentialError.value = messageFromError(err, '加载节点密钥失败')
  } finally {
    credentialsLoading.value = false
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

async function createCredential() {
  if (credentialSaving.value) return
  const quota = parseQuotaToBytes(credentialForm.quotaValue, credentialForm.quotaUnit)
  if (quota === null) {
    credentialError.value = '配额必须是非负数，换算后需为整数字节且不能超过安全范围'
    return
  }
  credentialSaving.value = true
  credentialError.value = ''
  try {
    const created = await adminApi.createNodeCredential(nodeID, {
      name: credentialForm.name.trim(),
      bucket: credentialForm.bucket.trim(),
      quota_bytes: quota
    })
    if (!created.secret_key) throw new Error('创建响应未返回 Secret Key')
    secretResult.value = { title: '请立即保存 Secret Key', accessKey: created.access_key, secretKey: created.secret_key }
    credentialForm.name = ''
    credentialForm.bucket = ''
    credentialForm.quotaValue = '0'
    credentialForm.quotaUnit = 'GB'
    await loadCredentials()
  } catch (err) {
    credentialError.value = messageFromError(err, '创建节点密钥失败')
  } finally {
    credentialSaving.value = false
  }
}

async function rotateCredential(credential: PanelCredential) {
  if (!window.confirm(`确认轮换 ${credential.access_key} 的 Secret？旧 Secret 会在新期望状态应用后失效。`)) return
  credentialSaving.value = true
  credentialError.value = ''
  try {
    const rotated = await adminApi.rotateNodeCredential(nodeID, credential.access_key)
    if (!rotated.secret_key) throw new Error('轮换响应未返回 Secret Key')
    secretResult.value = { title: '请立即保存新的 Secret Key', accessKey: rotated.access_key, secretKey: rotated.secret_key }
    await loadCredentials()
  } catch (err) {
    credentialError.value = messageFromError(err, '轮换 Secret 失败')
  } finally {
    credentialSaving.value = false
  }
}

async function publishDesiredState() {
  actionLoading.value = true
  error.value = ''
  actionMessage.value = ''
  try {
    publishResult.value = await adminApi.publishNodeDesiredState(nodeID)
    node.value = await adminApi.getNode(nodeID)
  } catch (err) {
    error.value = messageFromError(err, '发布期望状态失败')
  } finally {
    actionLoading.value = false
  }
}

async function pushDesiredState() {
  actionLoading.value = true
  error.value = ''
  try {
    await adminApi.pushNodeDesiredState(nodeID)
    actionMessage.value = '当前期望状态已推送。'
    node.value = await adminApi.getNode(nodeID)
  } catch (err) {
    error.value = messageFromError(err, '推送期望状态失败')
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
    await Promise.all([loadCertificates(), adminApi.getNode(nodeID).then((value) => (node.value = value))])
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
    synced: '已同步',
    waiting: '等待同步',
    failed: '同步失败',
    drift: '配置漂移',
    '': '尚未上报'
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
