<template>
  <section class="page-stack">
    <div class="page-header">
      <div>
        <h1>节点管理</h1>
        <p class="muted">管理逻辑节点、注册状态和配置同步进度。</p>
      </div>
      <button class="secondary-button" type="button" :disabled="loading" @click="load">
        {{ loading ? '刷新中…' : '刷新' }}
      </button>
    </div>

    <div v-if="error" class="notice error-notice">{{ error }}</div>

    <section class="panel form-panel">
      <form class="inline-form" @submit.prevent="createNode">
        <div class="form-field">
          <label for="panel-node-name">新建逻辑节点</label>
          <input
            id="panel-node-name"
            v-model="displayName"
            type="text"
            maxlength="128"
            autocomplete="off"
            placeholder="例如：上海-01"
            :disabled="creating"
          />
          <p class="table-help">创建后签发一次性注册令牌，再写入对应 node 配置。</p>
        </div>
        <button class="primary-button" type="submit" :disabled="creating || !displayName.trim()">
          {{ creating ? '创建中…' : '创建节点' }}
        </button>
      </form>
    </section>

    <section class="panel">
      <div class="table-scroll">
        <table class="data-table panel-nodes-table">
          <thead>
            <tr>
              <th>节点</th>
              <th>连接</th>
              <th>状态</th>
              <th>同步</th>
              <th>版本</th>
              <th>最近心跳</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="loading" class="state-row">
              <td colspan="7">加载中…</td>
            </tr>
            <tr v-else-if="nodes.length === 0" class="state-row">
              <td colspan="7">暂无节点，请先创建逻辑节点。</td>
            </tr>
            <tr v-for="node in nodes" :key="node.id">
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
                <span :class="['status-badge', nodeStatusClass(node.status)]">{{ nodeStatusLabel(node.status) }}</span>
              </td>
              <td>{{ syncStateLabel(node.sync_state) }}</td>
              <td>{{ node.applied_version }} / {{ node.desired_version }}</td>
              <td>{{ formatDate(node.last_heartbeat) }}</td>
              <td><RouterLink class="secondary-button panel-link-button" :to="`/nodes/${node.id}`">管理</RouterLink></td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
  </section>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { adminApi, type PanelNode } from '../api/client'

const nodes = ref<PanelNode[]>([])
const displayName = ref('')
const loading = ref(false)
const creating = ref(false)
const error = ref('')

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    nodes.value = await adminApi.listNodes()
  } catch (err) {
    error.value = messageFromError(err, '加载节点失败')
  } finally {
    loading.value = false
  }
}

async function createNode() {
  const name = displayName.value.trim()
  if (!name || creating.value) return
  creating.value = true
  error.value = ''
  try {
    await adminApi.createNode(name)
    displayName.value = ''
    await load()
  } catch (err) {
    error.value = messageFromError(err, '创建节点失败')
  } finally {
    creating.value = false
  }
}

function nodeStatusLabel(status: PanelNode['status']) {
  if (status === 'active') return '启用'
  if (status === 'disabled') return '停用'
  return '已退役'
}

function nodeStatusClass(status: PanelNode['status']) {
  if (status === 'active') return 'status-enabled'
  if (status === 'disabled') return 'status-disabled'
  return 'status-retired'
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
