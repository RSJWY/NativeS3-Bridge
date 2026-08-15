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
        <p class="summary-note">同步失败 / 漂移 / 待发布</p>
      </div>
    </section>

    <section class="panel">
      <h2>节点健康分布</h2>
      <div v-if="loading && !summary" class="state-row-only">加载中…</div>
      <div v-else-if="(summary?.totals.nodes ?? 0) === 0" class="state-row-only">暂无节点，请先在节点管理中创建。</div>
      <ul v-else class="health-list">
        <li v-for="item in healthRows" :key="item.label">
          <span :class="['status-badge', item.badgeClass]">{{ item.label }}</span>
          <strong>{{ item.count }}</strong>
        </li>
      </ul>
      <p class="table-help">分布只统计非退役节点；区域仅作参考，不参与健康判定。</p>
    </section>

    <section class="panel">
      <h2>需要关注的节点</h2>
      <div class="table-scroll">
        <table class="data-table panel-attention-table">
          <thead>
            <tr>
              <th>级别</th>
              <th>节点</th>
              <th>连接</th>
              <th>同步</th>
              <th>区域</th>
              <th>版本</th>
              <th>最近心跳</th>
              <th>最近错误</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="loading && !summary" class="state-row">
              <td colspan="9">加载中…</td>
            </tr>
            <tr v-else-if="(summary?.totals.nodes ?? 0) === 0" class="state-row">
              <td colspan="9">暂无节点。</td>
            </tr>
            <tr v-else-if="attentionNodes.length === 0" class="state-row">
              <td colspan="9">所有非退役节点状态正常，无需处理。</td>
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
                <span :class="['status-badge', node.online ? 'status-enabled' : 'status-neutral']">
                  {{ node.online ? '在线' : '离线' }}
                </span>
              </td>
              <td>
                {{ syncStateLabel(node.sync_state) }}
                <p v-if="node.publish_required" class="table-help">需重新发布快照</p>
                <p v-else-if="node.draft_dirty" class="table-help">有未发布草稿</p>
              </td>
              <td>{{ node.region || '未上报' }}</td>
              <td>{{ node.applied_version }} / {{ node.desired_version }}</td>
              <td>{{ formatDate(node.last_heartbeat) }}</td>
              <td class="attention-error">{{ node.last_error || '—' }}</td>
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
import {
  adminApi,
  type PanelDashboardAttentionNode,
  type PanelDashboardSeverity,
  type PanelDashboardSummary
} from '../api/client'

const summary = ref<PanelDashboardSummary | null>(null)
const loading = ref(false)
const error = ref('')

const attentionNodes = computed<PanelDashboardAttentionNode[]>(() => summary.value?.attention_nodes ?? [])

const healthRows = computed(() => [
  { label: '在线', count: summary.value?.totals.online ?? 0, badgeClass: 'status-enabled' },
  { label: '离线', count: summary.value?.totals.offline ?? 0, badgeClass: 'status-neutral' },
  { label: '同步正常', count: summary.value?.health.synced ?? 0, badgeClass: 'status-enabled' },
  { label: '待同步', count: summary.value?.health.waiting ?? 0, badgeClass: 'status-neutral' },
  { label: '同步失败', count: summary.value?.health.failed ?? 0, badgeClass: 'status-disabled' },
  { label: '配置漂移', count: summary.value?.health.drift ?? 0, badgeClass: 'status-disabled' },
  { label: '未上报', count: summary.value?.health.unknown ?? 0, badgeClass: 'status-neutral' }
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
    sync_failed: '同步失败',
    drift: '配置漂移',
    offline: '离线',
    pending: '待处理'
  }
  return labels[severity]
}

function severityBadgeClass(severity: PanelDashboardSeverity) {
  if (severity === 'sync_failed' || severity === 'drift') return 'status-disabled'
  if (severity === 'offline') return 'status-neutral'
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
