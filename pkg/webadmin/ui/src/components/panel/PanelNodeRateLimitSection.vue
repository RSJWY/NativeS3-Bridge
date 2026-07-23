<template>
  <section class="panel panel-detail-section">
    <div class="panel-section-heading">
      <div>
        <h2>匿名下载限流</h2>
        <p class="muted">仅限制未签名的公开对象 GET/HEAD；签名请求不受此策略影响。</p>
      </div>
      <button class="secondary-button" type="button" :disabled="saving || disabled || !rateLimit?.configured" @click="resetRateLimit">恢复默认</button>
    </div>

    <dl v-if="rateLimit" class="panel-effective-state">
      <div><dt>状态</dt><dd>{{ rateLimit.configured ? '已配置' : '使用默认值' }}</dd></div>
      <div><dt>当前 RPS</dt><dd>{{ rateLimit.effective.anonymous_rps }}</dd></div>
      <div><dt>当前 Burst</dt><dd>{{ rateLimit.effective.anonymous_burst }}</dd></div>
      <div><dt>转发头</dt><dd>{{ rateLimit.effective.trust_forwarded ? '信任' : '不信任' }}</dd></div>
    </dl>

    <form class="panel-resource-form panel-rate-limit-form" @submit.prevent="saveRateLimit">
      <div class="form-field">
        <label for="panel-rate-rps">每秒请求数</label>
        <input id="panel-rate-rps" v-model.number="form.anonymous_rps" type="number" min="0.01" step="0.01" :disabled="saving || disabled" />
      </div>
      <div class="form-field">
        <label for="panel-rate-burst">突发容量</label>
        <input id="panel-rate-burst" v-model.number="form.anonymous_burst" type="number" min="1" step="1" :disabled="saving || disabled" />
      </div>
      <label class="panel-checkbox-field"><input v-model="form.trust_forwarded" type="checkbox" :disabled="saving || disabled" /> 信任 X-Forwarded-For / X-Real-IP</label>
      <button class="primary-button" type="submit" :disabled="saving || disabled">{{ saving ? '保存中…' : '保存策略' }}</button>
    </form>
    <div v-if="form.trust_forwarded" class="notice warning-notice panel-inline-notice">
      仅在节点前方是可信代理且代理会覆盖转发头时启用；否则客户端可伪造来源 IP 绕过限流。
    </div>
    <p v-if="loading" class="muted panel-form-error">加载中…</p>
    <p v-if="error" class="error-text panel-form-error">{{ error }}</p>
  </section>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref, watch } from 'vue'
import { adminApi, type PanelRateLimit } from '../../api/client'

const props = defineProps<{ nodeId: number; disabled: boolean; refreshKey: number }>()
const emit = defineEmits<{ changed: [] }>()

const rateLimit = ref<PanelRateLimit | null>(null)
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const form = reactive({ anonymous_rps: 10, anonymous_burst: 20, trust_forwarded: false })

onMounted(() => void load())
watch(() => props.refreshKey, () => void load())

async function load() {
  loading.value = true
  error.value = ''
  try {
    rateLimit.value = await adminApi.getNodeRateLimit(props.nodeId)
    Object.assign(form, rateLimit.value.values ?? rateLimit.value.effective)
  } catch (err) {
    error.value = messageFromError(err, '加载限流策略失败')
  } finally {
    loading.value = false
  }
}

async function saveRateLimit() {
  if (!Number.isFinite(form.anonymous_rps) || form.anonymous_rps <= 0 || !Number.isSafeInteger(form.anonymous_burst) || form.anonymous_burst <= 0) {
    error.value = 'RPS 和突发容量必须是正数，突发容量必须为整数。'
    return
  }
  saving.value = true
  error.value = ''
  try {
    rateLimit.value = await adminApi.updateNodeRateLimit(props.nodeId, { ...form })
    emit('changed')
  } catch (err) {
    error.value = messageFromError(err, '保存限流策略失败')
  } finally {
    saving.value = false
  }
}

async function resetRateLimit() {
  if (!window.confirm('确认恢复匿名下载限流默认值？此变更仍需发布草稿后才会应用到节点。')) return
  saving.value = true
  error.value = ''
  try {
    rateLimit.value = await adminApi.resetNodeRateLimit(props.nodeId)
    Object.assign(form, rateLimit.value.effective)
    emit('changed')
  } catch (err) {
    error.value = messageFromError(err, '恢复默认策略失败')
  } finally {
    saving.value = false
  }
}

function messageFromError(err: unknown, fallback: string) {
  return err instanceof Error ? err.message : fallback
}
</script>
