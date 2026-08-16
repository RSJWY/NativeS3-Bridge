<template>
  <section class="page-stack">
    <div class="page-header">
      <div>
        <h1>仪表盘</h1>
        <p class="muted">节点在线情况、配置同步健康和需要处理的节点。数据生成于 {{ formatDate(summary?.generated_at) }}。</p>
      </div>
      <button class="secondary-button" type="button" :disabled="loading" @click="load">
        {{ loading ? '刷新中…' : '刷新' }}
      </button>
    </div>

    <div v-if="error" class="notice error-notice">{{ error }}</div>

    <section class="summary-grid">
      <div class="summary-card">
        <span>节点总数</span>
        <strong>{{ loading && !summary ? '…' : (summary?.totals.nodes ?? 0) }}</strong>
        <p class="summary-note">含已退役 {{ summary?.totals.retired ?? 0 }} 个</p>
      </div>
      <div class="summary-card">
        <span>在线节点</span>
        <strong>{{ loading && !summary ? '…' : (summary?.totals.online ?? 0) }}</strong>
        <p class="summary-note">不含已退役节点</p>
      </div>
      <div class="summary-card">
        <span>离线节点</span>
        <strong>{{ loading && !summary ? '…' : (summary?.totals.offline ?? 0) }}</strong>
        <p class="summary-note">不含已退役节点</p>
      </div>
      <div class="summary-card">
        <span>需要处理</span>
        <strong>{{ loading && !summary ? '…' : (summary?.totals.attention ?? 0) }}</strong>
        <p class="summary-note">同步失败 / 漂移 / 待发布 / 证书过期</p>
      </div>
      <div class="summary-card">
        <span>总使用容量</span>
        <strong>{{ loading && !summary ? '…' : formatBytes(summary?.telemetry.used_bytes_total ?? 0) }}</strong>
        <p class="summary-note">仅含遥测有效且未过期的节点</p>
      </div>
      <div class="summary-card">
        <span>对象总数</span>
        <strong>{{ loading && !summary ? '…' : formatNumber(summary?.telemetry.object_count ?? 0) }}</strong>
        <p class="summary-note">仅含遥测有效且未过期的节点</p>
      </div>
      <div class="summary-card">
        <span>遥测有效节点</span>
        <strong>{{ loading && !summary ? '…' : (summary?.telemetry.valid_nodes ?? 0) }}</strong>
        <p class="summary-note">未上报 {{ summary?.telemetry.missing_nodes ?? 0 }} · 已过期 {{ summary?.telemetry.stale_nodes ?? 0 }}</p>
      </div>
    </section>

    <section class="panel panel-content-section">
      <h2>节点健康分布</h2>
      <div v-if="loading && !summary" class="state-row-only">加载中…</div>
      <div v-else-if="(summary?.totals.nodes ?? 0) === 0" class="state-row-only">暂无节点，请先在节点管理中创建。</div>
      <dl v-else class="health-groups">
        <div v-for="group in healthGroups" :key="group.label">
          <dt>{{ group.label }}</dt>
          <dd>
            <span v-for="item in group.items" :key="item.label" class="health-item">
              <span :class="['status-badge', item.badgeClass]">{{ item.label }}</span>
              <strong>{{ item.count }}</strong>
            </span>
          </dd>
        </div>
      </dl>
      <p class="table-help">仅统计非退役节点。</p>
    </section>

    <section class="panel panel-content-section">
      <h2>节点存储遥测</h2>
      <div class="table-scroll">
        <table class="data-table panel-telemetry-table">
          <thead>
            <tr>
              <th>节点</th>
              <th>遥测状态</th>
              <th>使用容量</th>
              <th>对象数</th>
              <th>观测时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="loading && !summary" class="state-row">
              <td colspan="5">加载中…</td>
            </tr>
            <tr v-else-if="(summary?.totals.nodes ?? 0) === 0" class="state-row">
              <td colspan="5">暂无节点。</td>
            </tr>
            <tr v-for="node in telemetryNodes" :key="node.node_id">
              <td>
                <strong>{{ node.display_name }}</strong>
                <p class="table-help">ID {{ node.node_id }}</p>
              </td>
              <td>
                <span :class="['status-badge', telemetryBadgeClass(node.status)]">{{ telemetryStatusLabel(node.status) }}</span>
              </td>
              <td>
                <template v-if="node.status === 'valid'">{{ formatBytes(node.used_bytes ?? 0) }}</template>
                <span v-else-if="node.status === 'stale'" class="muted">{{ node.used_bytes === null ? '未上报 / 不可用' : formatBytes(node.used_bytes) }}</span>
                <span v-else class="muted">未上报 / 不可用</span>
              </td>
              <td>
                <template v-if="node.status === 'valid'">{{ formatNumber(node.object_count ?? 0) }}</template>
                <span v-else-if="node.status === 'stale'" class="muted">{{ node.object_count === null ? '未上报 / 不可用' : formatNumber(node.object_count) }}</span>
                <span v-else class="muted">未上报 / 不可用</span>
              </td>
              <td>{{ formatDate(node.observed_at ?? undefined) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p class="table-help">容量与对象数为节点自报观测值，非实时统计。</p>
    </section>

    <section class="panel panel-content-section">
      <h2>需要关注的节点</h2>
      <div class="table-scroll">
        <table class="data-table panel-attention-table">
          <thead>
            <tr>
              <th>级别</th>
              <th>节点</th>
              <th>同步</th>
              <th>版本（应用 / 期望）</th>
              <th>最近心跳</th>
              <th>最近错误</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="loading && !summary" class="state-row">
              <td colspan="7">加载中…</td>
            </tr>
            <tr v-else-if="(summary?.totals.nodes ?? 0) === 0" class="state-row">
              <td colspan="7">暂无节点。</td>
            </tr>
            <tr v-else-if="attentionNodes.length === 0" class="state-row">
              <td colspan="7">所有非退役节点状态正常，无需处理。</td>
            </tr>
            <tr v-for="node in attentionNodes" :key="node.id">
              <td>
                <span :class="['status-badge', severityBadgeClass(node.severity)]">{{ severityLabel(node.severity) }}</span>
              </td>
              <td>
                <strong>{{ node.display_name }}</strong>
                <p class="table-help">ID {{ node.id }}</p>
              </td>
              <td>
                {{ syncStateLabel(node.sync_state) }}
                <p v-if="node.publish_required" class="table-help">需重新发布快照</p>
                <p v-else-if="node.draft_dirty" class="table-help">有未发布草稿</p>
              </td>
              <td>{{ node.applied_version }} / {{ node.desired_version }}</td>
              <td>{{ formatDate(node.last_heartbeat) }}</td>
              <td class="attention-error" :title="node.last_error || undefined">{{ node.last_error || '—' }}</td>
              <td><RouterLink class="secondary-button panel-link-button" :to="`/nodes/${node.id}`">管理</RouterLink></td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { formatBytes } from '../utils/format'
import {
  adminApi,
  type PanelDashboardAttentionNode,
  type PanelDashboardSeverity,
  type PanelDashboardSummary,
  type PanelNodeTelemetry,
  type PanelTelemetryStatus
} from '../api/client'

const summary = ref<PanelDashboardSummary | null>(null)
const loading = ref(false)
const error = ref('')

const attentionNodes = computed<PanelDashboardAttentionNode[]>(() => summary.value?.attention_nodes ?? [])
const telemetryNodes = computed<PanelNodeTelemetry[]>(() => summary.value?.telemetry.nodes ?? [])

function telemetryStatusLabel(status: PanelTelemetryStatus) {
  const labels: Record<PanelTelemetryStatus, string> = {
    valid: '有效',
    missing: '未上报',
    stale: '已过期'
  }
  return labels[status]
}

function telemetryBadgeClass(status: PanelTelemetryStatus) {
  if (status === 'valid') return 'status-enabled'
  if (status === 'stale') return 'status-disabled'
  return 'status-neutral'
}

function formatNumber(value: number) {
  return new Intl.NumberFormat().format(value)
}

const healthGroups = computed(() => [
  {
    label: '连接',
    items: [
      { label: '在线', count: summary.value?.totals.online ?? 0, badgeClass: 'status-enabled' },
      { label: '离线', count: summary.value?.totals.offline ?? 0, badgeClass: 'status-neutral' }
    ]
  },
  {
    label: '同步',
    items: [
      { label: '正常', count: summary.value?.health.synced ?? 0, badgeClass: 'status-enabled' },
      { label: '待同步', count: summary.value?.health.waiting ?? 0, badgeClass: 'status-neutral' },
      { label: '失败', count: summary.value?.health.failed ?? 0, badgeClass: 'status-disabled' },
      { label: '漂移', count: summary.value?.health.drift ?? 0, badgeClass: 'status-disabled' },
      { label: '未上报', count: summary.value?.health.unknown ?? 0, badgeClass: 'status-neutral' }
    ]
  },
  {
    label: '证书',
    items: [
      { label: '已过期', count: summary.value?.certs.expired_nodes ?? 0, badgeClass: 'status-disabled' },
      { label: '临期', count: summary.value?.certs.expiring_nodes ?? 0, badgeClass: 'status-warning' }
    ]
  }
])

onMounted(load)

async function load() {
  // 手动刷新期间禁止重复请求;失败时保留上一份已成功加载的数据。
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    summary.value = await adminApi.panelDashboardSummary()
  } catch (err) {
    error.value = messageFromError(err, '加载仪表盘失败')
  } finally {
    loading.value = false
  }
}

function severityLabel(severity: PanelDashboardSeverity) {
  const labels: Record<PanelDashboardSeverity, string> = {
    cert_expired: '证书已过期',
    sync_failed: '同步失败',
    drift: '配置漂移',
    offline: '离线',
    cert_expiring: '证书临期',
    pending: '待处理'
  }
  return labels[severity]
}

function severityBadgeClass(severity: PanelDashboardSeverity) {
  if (severity === 'cert_expired' || severity === 'sync_failed' || severity === 'drift') return 'status-disabled'
  if (severity === 'cert_expiring') return 'status-warning'
  return 'status-neutral'
}

function syncStateLabel(state: PanelDashboardAttentionNode['sync_state']) {
  const labels: Record<PanelDashboardAttentionNode['sync_state'], string> = {
    synced: '已同步',
    waiting: '等待同步',
    failed: '同步失败',
    drift: '配置漂移',
    '': '尚未上报'
  }
  return labels[state]
}

function formatDate(value?: string) {
  return value ? new Date(value).toLocaleString() : '-'
}

function messageFromError(err: unknown, fallback: string) {
  return err instanceof Error ? err.message : fallback
}
</script>
