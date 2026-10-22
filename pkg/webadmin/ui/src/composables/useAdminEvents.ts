import { onScopeDispose, readonly, ref } from 'vue'

/**
 * 管理面板 SSE 事件订阅(panel admin 的 /api/admin/events,见 pkg/panel/events.go)。
 *
 * 设计要点:
 * - 通知式事件:收到 node_online/node_offline/task_updated 后由调用方重拉对应
 *   REST 端点,REST 始终是唯一数据源,这里不缓存任何业务数据。
 * - 全页面共享一条 EventSource 连接(模块级单例 + 引用计数),多组件订阅不会
 *   建立多条流;最后一个订阅者卸载时关闭连接。
 * - 判活与降级:服务端按 ~15s 发 heartbeat 事件;超过 DEGRADE_THRESHOLD_MS 没有
 *   收到任何帧(例如反向代理缓冲了 SSE)即判定不可用,自动按 DEGRADE_POLL_MS
 *   周期调用各订阅者的 reload 兜底,恢复收到事件/心跳后停止轮询。
 * - 重连:EventSource 网络级错误交给浏览器内建重连;致命关闭(如 401/404,浏览器
 *   会把 readyState 置为 CLOSED 且不再自动重连)由这里按 RECONNECT_DELAY_MS 手动
 *   重试。每次(重)连成功时触发一次 onReconnect,让调用方重拉补齐断连期间错过的
 *   状态变化。
 * - 登录态失效:会话过期时 SSE 收到 401 → 致命关闭 → 降级轮询路径的 apiFetch
 *   收到 401 → markLoggedOut + 跳转 /login,与 REST 的既有行为一致。
 */

export type AdminEventType = 'node_online' | 'node_offline' | 'task_updated'

export interface AdminEventPayload {
  node_id?: number
  task_id?: string
  state?: string
}

export interface AdminEventHandlers {
  /** 业务事件回调(heartbeat 不经过这里)。 */
  onEvent?: (type: AdminEventType, payload: AdminEventPayload) => void
  /** SSE (重)连成功后调用一次:重拉 REST 补齐断连期间错过的变化。 */
  onReconnect?: () => void
  /** 页面数据重拉函数:重连补齐与降级轮询都由它执行。 */
  reload: () => void
}

// 服务端心跳 15s;阈值取 3 倍,单次丢帧/网络抖动不会误降级。
const DEGRADE_THRESHOLD_MS = 45_000
const DEGRADE_POLL_MS = 30_000
// EventSource 致命关闭后的手动重连间隔(连接失败如 401 期间,靠降级轮询保数据新鲜)。
const RECONNECT_DELAY_MS = 15_000
// 判活检查的巡检周期。
const WATCHDOG_TICK_MS = 5_000

interface Subscription {
  handlers: AdminEventHandlers
  pollTimer?: number
}

// --- 模块级共享连接状态 ---
let refCount = 0
let source: EventSource | null = null
let everOpened = false
let lastReceivedAt = 0
let degraded = false
let watchdogTimer: number | undefined
let reconnectTimer: number | undefined
const subscriptions = new Set<Subscription>()

const connectedRef = ref(false)
const degradedRef = ref(false)

export function useAdminEvents(handlers: AdminEventHandlers) {
  const subscription: Subscription = { handlers }
  subscriptions.add(subscription)
  refCount += 1
  ensureConnected()
  ensureWatchdog()
  // 加入时已处于降级状态(如路由切换后回到面板页):立即补拉并把新订阅者纳入轮询。
  if (degraded) {
    handlers.reload()
    subscription.pollTimer = window.setInterval(() => handlers.reload(), DEGRADE_POLL_MS)
  }

  onScopeDispose(() => {
    subscriptions.delete(subscription)
    if (subscription.pollTimer !== undefined) {
      window.clearInterval(subscription.pollTimer)
      subscription.pollTimer = undefined
    }
    refCount -= 1
    if (refCount === 0) teardown()
  })

  return { connected: readonly(connectedRef), degraded: readonly(degradedRef) }
}

function ensureConnected() {
  if (source || refCount === 0) return
  // 订阅时刻起算判活时钟:连接一直建不起来(如后端未升级、网关 404)也要能降级。
  lastReceivedAt = Date.now()
  const es = new EventSource('/api/admin/events')
  source = es

  es.onopen = () => {
    connectedRef.value = true
    // 只有「重」连才需要补齐;页面首次加载本来就会拉一次数据。
    if (everOpened) {
      for (const sub of subscriptions) {
        if (sub.handlers.onReconnect) sub.handlers.onReconnect()
      }
    }
    everOpened = true
  }

  es.addEventListener('heartbeat', () => noteReceived())
  for (const type of ['node_online', 'node_offline', 'task_updated'] as const) {
    es.addEventListener(type, (event) => {
      noteReceived()
      let payload: AdminEventPayload = {}
      try {
        payload = JSON.parse((event as MessageEvent).data) as AdminEventPayload
      } catch {
        // data 解析失败按空负载处理:类型本身已足够驱动一次重拉。
      }
      for (const sub of subscriptions) {
        if (sub.handlers.onEvent) sub.handlers.onEvent(type, payload)
      }
    })
  }

  es.onerror = () => {
    connectedRef.value = false
    // CONNECTING 状态是浏览器在自动重连,不要插手;CLOSED 是致命失败(非 200 或
    // 非 event-stream 响应,典型如会话过期 401),浏览器不会再重试。
    if (es.readyState === EventSource.CLOSED) {
      closeSource()
      if (refCount > 0 && reconnectTimer === undefined) {
        reconnectTimer = window.setTimeout(() => {
          reconnectTimer = undefined
          ensureConnected()
        }, RECONNECT_DELAY_MS)
      }
    }
  }
}

function closeSource() {
  if (!source) return
  source.close()
  source = null
  connectedRef.value = false
}

function teardown() {
  closeSource()
  if (watchdogTimer !== undefined) {
    window.clearInterval(watchdogTimer)
    watchdogTimer = undefined
  }
  if (reconnectTimer !== undefined) {
    window.clearTimeout(reconnectTimer)
    reconnectTimer = undefined
  }
  undegrade()
}

function ensureWatchdog() {
  if (watchdogTimer !== undefined) return
  watchdogTimer = window.setInterval(() => {
    if (degraded) return
    if (Date.now() - lastReceivedAt > DEGRADE_THRESHOLD_MS) degrade()
  }, WATCHDOG_TICK_MS)
}

function noteReceived() {
  lastReceivedAt = Date.now()
  if (degraded) undegrade()
}

function degrade() {
  degraded = true
  degradedRef.value = true
  for (const sub of subscriptions) {
    // 降级立即补拉一次,再按低频轮询兜底。
    sub.handlers.reload()
    sub.pollTimer = window.setInterval(() => sub.handlers.reload(), DEGRADE_POLL_MS)
  }
}

function undegrade() {
  degraded = false
  degradedRef.value = false
  for (const sub of subscriptions) {
    if (sub.pollTimer !== undefined) {
      window.clearInterval(sub.pollTimer)
      sub.pollTimer = undefined
    }
  }
}
