export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) {
    return '0 B'
  }
  if (bytes === 0) {
    return '0 B'
  }
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  const value = bytes / 1024 ** index
  return `${value >= 10 || index === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[index]}`
}

export function formatQuota(bytes: number): string {
  return bytes === 0 ? '不限' : formatBytes(bytes)
}

export function parseQuotaToBytes(value: string): number | null {
  const trimmed = value.trim()
  if (!trimmed) {
    return 0
  }
  const parsed = Number(trimmed)
  if (!Number.isFinite(parsed) || parsed < 0 || !Number.isSafeInteger(Math.floor(parsed))) {
    return null
  }
  return Math.floor(parsed)
}

export function usagePercent(used: number, quota: number): string {
  if (quota <= 0) {
    return '不限'
  }
  return `${Math.min(100, (used / quota) * 100).toFixed(1)}%`
}
