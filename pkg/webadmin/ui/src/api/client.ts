import router from '../router'
import { markLoggedOut } from '../state/auth'

export interface Credential {
  id: number
  access_key: string
  name: string
  status: 'enabled' | 'disabled'
  quota_bytes: number
  used_bytes: number
  created_at: string
}

export interface CreatedCredential extends Credential {
  secret_key: string
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
  login(password: string) {
    return apiFetch<{ ok: boolean }>('/api/admin/login', {
      method: 'POST',
      body: JSON.stringify({ password }),
      skipAuthRedirect: true
    })
  },
  logout() {
    return apiFetch<{ ok: boolean }>('/api/admin/logout', { method: 'POST' })
  },
  listCredentials() {
    return apiFetch<Credential[]>('/api/admin/credentials')
  },
  createCredential(input: { name: string; quota_bytes: number }) {
    return apiFetch<CreatedCredential>('/api/admin/credentials', {
      method: 'POST',
      body: JSON.stringify(input)
    })
  },
  updateCredential(id: number, input: { name?: string; status?: 'enabled' | 'disabled'; quota_bytes?: number }) {
    return apiFetch<Credential>(`/api/admin/credentials/${id}`, {
      method: 'PATCH',
      body: JSON.stringify(input)
    })
  },
  deleteCredential(id: number) {
    return apiFetch<{ ok: boolean }>(`/api/admin/credentials/${id}`, { method: 'DELETE' })
  },
  dashboardSummary() {
    return apiFetch<DashboardSummary>('/api/admin/dashboard/summary')
  },
  usageRanking() {
    return apiFetch<UsageRankingItem[]>('/api/admin/dashboard/usage-ranking')
  },
  requestTrend(days = 30) {
    return apiFetch<RequestTrendItem[]>(`/api/admin/dashboard/request-trend?days=${days}`)
  }
}
