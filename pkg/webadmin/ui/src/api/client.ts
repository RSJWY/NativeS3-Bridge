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
  secret_key?: string
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
    throw new Error('unauthorized')
  }

  const payload = await readPayload(response)
  if (!response.ok) {
    const message = isErrorPayload(payload) ? payload.error : `请求失败：${response.status}`
    throw new Error(message)
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
    return apiFetch<PanelCredential>(`/api/admin/nodes/${id}/credentials`, {
      method: 'POST',
      body: JSON.stringify(input)
    })
  },
  rotateNodeCredential(id: number, accessKey: string) {
    return apiFetch<PanelCredential>(`/api/admin/nodes/${id}/credentials/${encodeURIComponent(accessKey)}/rotate`, { method: 'POST' })
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
