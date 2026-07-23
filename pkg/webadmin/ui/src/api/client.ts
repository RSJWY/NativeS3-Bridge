import router from '../router'
import { markLoggedOut } from '../state/auth'

export interface Credential {
  id: number
  access_key: string
  name: string
  bucket: string
  status: 'enabled' | 'disabled'
  quota_bytes: number
  used_bytes: number
  created_at: string
}

export interface CreatedCredential extends Credential {
  secret_key: string
}

export type BucketACL = 'private' | 'public-read'

export interface Bucket {
  name: string
  acl: BucketACL
  created_at: string
}

export interface DashboardSummary {
  total_credentials: number
  total_quota_bytes: number
  total_used_bytes: number
}

export interface UsageRankingItem {
  access_key: string
  name: string
  used_bytes: number
  quota_bytes: number
}

export interface RequestTrendItem {
  day: string
  put_count: number
  get_count: number
  delete_count: number
  bytes_in: number
  bytes_out: number
}

export interface LogEntry {
  time: string
  level: string
  msg: string
  attrs?: Record<string, unknown>
}

export interface LogFileInfo {
  id: string
  name: string
  size: number
  modified_at: string
  current: boolean
  compressed: boolean
}

export interface LogsResponse {
  source: 'ring' | 'file'
  file_enabled: boolean
  limit: number
  entries: LogEntry[]
  warning?: string
  files: LogFileInfo[]
  selected_file?: LogFileInfo
}

export interface ReconcileCredential {
  id: number
  access_key: string
  name: string
  used_bytes: number
  diff_bytes: number
  updated: boolean
}

export interface ReconcileReport {
  bucket: string
  apply: boolean
  object_count: number
  scanned_bytes: number
  orphan_sidecar_count: number
  orphan_sidecar_samples: string[]
  bound_credentials: ReconcileCredential[]
  orphans_deleted: number
  credentials_updated: number
}

export interface AuthSettings {
  totp_required: boolean
  captcha_enabled: boolean
  captcha_provider: string
  captcha_site_key: string
  service_mode: 'standalone' | 'panel'
}

export interface PanelNode {
  id: number
  display_name: string
  status: 'active' | 'disabled' | 'retired'
  online: boolean
  applied_version: number
  desired_version: number
  sync_state: 'synced' | 'waiting' | 'failed' | 'drift' | ''
  last_error?: string
  draft_dirty: boolean
  publish_required: boolean
  last_heartbeat?: string
  created_at: string
}

export interface PanelRegistrationToken {
  token: string
  expires_at: string
}

export interface PanelCredential {
  id: number
  node_id: number
  access_key: string
  name: string
  bucket: string
  status: 'enabled' | 'disabled'
  quota_bytes: number
}

export interface PanelCreatedCredential extends PanelCredential {
  secret_key: string
}

export interface PanelBucket {
  name: string
  acl: BucketACL
  created_at: string
}

export type PanelWebhookEvent = 'ObjectCreated' | 'ObjectDeleted'

export interface PanelWebhook {
  id: number
  node_id: number
  url: string
  events: PanelWebhookEvent[]
  enabled: boolean
  created_at: string
}

export interface PanelRateLimitValues {
  anonymous_rps: number
  anonymous_burst: number
  trust_forwarded: boolean
}

export interface PanelRateLimit {
  configured: boolean
  values?: PanelRateLimitValues
  effective: PanelRateLimitValues
}

export interface PanelImportSummary {
  node_id: number
  credential_count: number
  bucket_count: number
  webhook_count: number
  access_keys: string[]
  bucket_names: string[]
  content_hash: string
  rate_limit_configured: boolean
}

export interface PanelPublishResult {
  version: number
  content_hash: string
  pushed: boolean
  push_error?: string
}

export interface PanelCertificate {
  ID: number
  NodeID: number
  Fingerprint: string
  Serial: string
  NotBefore: string
  NotAfter: string
  Revoked: boolean
  RevokedAt?: string
  CreatedAt: string
}

export interface LoginInput {
  password: string
  totp_code?: string
  captcha_token?: string
}

interface RequestOptions extends RequestInit {
  skipAuthRedirect?: boolean
}

export class ApiError extends Error {
  readonly status: number
  readonly payload: unknown

  constructor(status: number, message: string, payload: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.payload = payload
  }
}

export async function apiFetch<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const headers = new Headers(options.headers)
  if (options.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

  const response = await fetch(path, {
    ...options,
    headers,
    credentials: 'include'
  })

  if (response.status === 401 && !options.skipAuthRedirect) {
    markLoggedOut()
    await router.replace({ path: '/login', query: { redirect: router.currentRoute.value.fullPath } })
    throw new ApiError(response.status, 'unauthorized', null)
  }

  const payload = await readPayload(response)
  if (!response.ok) {
    const message = isErrorPayload(payload) ? payload.error : `请求失败：${response.status}`
    throw new ApiError(response.status, message, payload)
  }
  return payload as T
}

async function readPayload(response: Response): Promise<unknown> {
  const text = await response.text()
  if (!text) {
    return null
  }
  try {
    return JSON.parse(text)
  } catch {
    return text
  }
}

function isErrorPayload(payload: unknown): payload is { error: string } {
  return typeof payload === 'object' && payload !== null && 'error' in payload && typeof (payload as { error: unknown }).error === 'string'
}

export const adminApi = {
  authSettings() {
    return apiFetch<AuthSettings>('/api/admin/auth-settings', { skipAuthRedirect: true })
  },
  login(input: LoginInput) {
    return apiFetch<{ ok: boolean }>('/api/admin/login', {
      method: 'POST',
      body: JSON.stringify(input),
      skipAuthRedirect: true
    })
  },
  logout() {
    return apiFetch<{ ok: boolean }>('/api/admin/logout', { method: 'POST' })
  },
  listCredentials() {
    return apiFetch<Credential[]>('/api/admin/credentials')
  },
  createCredential(input: { name: string; bucket: string; quota_bytes: number }) {
    return apiFetch<CreatedCredential>('/api/admin/credentials', {
      method: 'POST',
      body: JSON.stringify(input)
    })
  },
  updateCredential(id: number, input: { name?: string; bucket?: string; status?: 'enabled' | 'disabled'; quota_bytes?: number }) {
    return apiFetch<Credential>(`/api/admin/credentials/${id}`, {
      method: 'PATCH',
      body: JSON.stringify(input)
    })
  },
  deleteCredential(id: number) {
    return apiFetch<{ ok: boolean }>(`/api/admin/credentials/${id}`, { method: 'DELETE' })
  },
  listBuckets() {
    return apiFetch<Bucket[]>('/api/admin/buckets')
  },
  createBucket(input: { name: string }) {
    return apiFetch<Bucket>('/api/admin/buckets', {
      method: 'POST',
      body: JSON.stringify(input)
    })
  },
  deleteBucket(name: string) {
    return apiFetch<{ ok: boolean }>(`/api/admin/buckets/${encodeURIComponent(name)}`, { method: 'DELETE' })
  },
  setBucketACL(name: string, acl: BucketACL) {
    return apiFetch<Bucket>(`/api/admin/buckets/${encodeURIComponent(name)}/acl`, {
      method: 'PUT',
      body: JSON.stringify({ acl })
    })
  },
  reconcileBucket(name: string, apply: boolean) {
    return apiFetch<ReconcileReport>(`/api/admin/buckets/${encodeURIComponent(name)}/reconcile`, {
      method: 'POST',
      body: JSON.stringify({ apply })
    })
  },
  dashboardSummary() {
    return apiFetch<DashboardSummary>('/api/admin/dashboard/summary')
  },
  usageRanking() {
    return apiFetch<UsageRankingItem[]>('/api/admin/dashboard/usage-ranking')
  },
  requestTrend(days = 30) {
    return apiFetch<RequestTrendItem[]>(`/api/admin/dashboard/request-trend?days=${days}`)
  },
  logs(params: { limit: number; level?: string; q?: string; file?: string }) {
    const query = new URLSearchParams({ limit: String(params.limit) })
    if (params.level) query.set('level', params.level)
    if (params.q) query.set('q', params.q)
    if (params.file) query.set('file', params.file)
    return apiFetch<LogsResponse>(`/api/admin/logs?${query.toString()}`)
  },
  listNodes() {
    return apiFetch<PanelNode[]>('/api/admin/nodes')
  },
  createNode(displayName: string) {
    return apiFetch<PanelNode>('/api/admin/nodes', {
      method: 'POST',
      body: JSON.stringify({ display_name: displayName })
    })
  },
  getNode(id: number) {
    return apiFetch<PanelNode>(`/api/admin/nodes/${id}`)
  },
  updateNode(id: number, input: { display_name?: string; status?: 'active' | 'disabled' }) {
    return apiFetch<PanelNode>(`/api/admin/nodes/${id}`, {
      method: 'PATCH',
      body: JSON.stringify(input)
    })
  },
  retireNode(id: number) {
    return apiFetch<PanelNode>(`/api/admin/nodes/${id}`, { method: 'DELETE' })
  },
  issueNodeToken(id: number) {
    return apiFetch<PanelRegistrationToken>(`/api/admin/nodes/${id}/tokens`, { method: 'POST' })
  },
  listNodeCredentials(id: number) {
    return apiFetch<PanelCredential[]>(`/api/admin/nodes/${id}/credentials`)
  },
  createNodeCredential(id: number, input: { name: string; bucket: string; quota_bytes: number }) {
    return apiFetch<PanelCreatedCredential>(`/api/admin/nodes/${id}/credentials`, {
      method: 'POST',
      body: JSON.stringify(input)
    })
  },
  rotateNodeCredential(id: number, accessKey: string) {
    return apiFetch<PanelCreatedCredential>(`/api/admin/nodes/${id}/credentials/${encodeURIComponent(accessKey)}/rotate`, { method: 'POST' })
  },
  updateNodeCredential(id: number, accessKey: string, input: { name?: string; bucket?: string; status?: 'enabled' | 'disabled'; quota_bytes?: number }) {
    return apiFetch<PanelCredential>(`/api/admin/nodes/${id}/credentials/${encodeURIComponent(accessKey)}`, {
      method: 'PATCH',
      body: JSON.stringify(input)
    })
  },
  deleteNodeCredential(id: number, accessKey: string) {
    return apiFetch<{ deleted: boolean }>(`/api/admin/nodes/${id}/credentials/${encodeURIComponent(accessKey)}`, { method: 'DELETE' })
  },
  listNodeBuckets(id: number) {
    return apiFetch<PanelBucket[]>(`/api/admin/nodes/${id}/buckets`)
  },
  createNodeBucket(id: number, input: { name: string; acl?: BucketACL }) {
    return apiFetch<PanelBucket>(`/api/admin/nodes/${id}/buckets`, {
      method: 'POST',
      body: JSON.stringify(input)
    })
  },
  updateNodeBucketACL(id: number, name: string, acl: BucketACL) {
    return apiFetch<PanelBucket>(`/api/admin/nodes/${id}/buckets/${encodeURIComponent(name)}/acl`, {
      method: 'PUT',
      body: JSON.stringify({ acl })
    })
  },
  deleteNodeBucket(id: number, name: string) {
    return apiFetch<{ deleted: boolean }>(`/api/admin/nodes/${id}/buckets/${encodeURIComponent(name)}`, { method: 'DELETE' })
  },
  listNodeWebhooks(id: number) {
    return apiFetch<PanelWebhook[]>(`/api/admin/nodes/${id}/webhooks`)
  },
  createNodeWebhook(id: number, input: { url: string; events: PanelWebhookEvent[]; enabled: boolean }) {
    return apiFetch<PanelWebhook>(`/api/admin/nodes/${id}/webhooks`, {
      method: 'POST',
      body: JSON.stringify(input)
    })
  },
  updateNodeWebhook(id: number, webhookID: number, input: { url?: string; events?: PanelWebhookEvent[]; enabled?: boolean }) {
    return apiFetch<PanelWebhook>(`/api/admin/nodes/${id}/webhooks/${webhookID}`, {
      method: 'PATCH',
      body: JSON.stringify(input)
    })
  },
  deleteNodeWebhook(id: number, webhookID: number) {
    return apiFetch<{ deleted: boolean }>(`/api/admin/nodes/${id}/webhooks/${webhookID}`, { method: 'DELETE' })
  },
  getNodeRateLimit(id: number) {
    return apiFetch<PanelRateLimit>(`/api/admin/nodes/${id}/rate-limit`)
  },
  updateNodeRateLimit(id: number, input: PanelRateLimitValues) {
    return apiFetch<PanelRateLimit>(`/api/admin/nodes/${id}/rate-limit`, {
      method: 'PUT',
      body: JSON.stringify(input)
    })
  },
  resetNodeRateLimit(id: number) {
    return apiFetch<PanelRateLimit>(`/api/admin/nodes/${id}/rate-limit`, { method: 'DELETE' })
  },
  async getNodeImport(id: number): Promise<PanelImportSummary | null> {
    try {
      return await apiFetch<PanelImportSummary>(`/api/admin/nodes/${id}/import`)
    } catch (error) {
      if (error instanceof ApiError && error.status === 404) return null
      throw error
    }
  },
  requestNodeImport(id: number) {
    return apiFetch<PanelImportSummary>(`/api/admin/nodes/${id}/import`, { method: 'POST' })
  },
  confirmNodeImport(id: number) {
    return apiFetch<{ version: number; content_hash: string }>(`/api/admin/nodes/${id}/import/confirm`, { method: 'POST' })
  },
  abortNodeImport(id: number) {
    return apiFetch<{ aborted: boolean }>(`/api/admin/nodes/${id}/import/abort`, { method: 'POST' })
  },
  publishNodeDesiredState(id: number) {
    return apiFetch<PanelPublishResult>(`/api/admin/nodes/${id}/desired-state`, { method: 'POST' })
  },
  pushNodeDesiredState(id: number) {
    return apiFetch<{ pushed: boolean }>(`/api/admin/nodes/${id}/desired-state/push`, { method: 'POST' })
  },
  listNodeCertificates(id: number) {
    return apiFetch<PanelCertificate[]>(`/api/admin/nodes/${id}/certs`)
  },
  revokeNodeCertificates(id: number) {
    return apiFetch<{ revoked: number }>(`/api/admin/nodes/${id}/certs/revoke`, { method: 'POST' })
  }
}
