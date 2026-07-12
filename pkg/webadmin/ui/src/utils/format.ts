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

export const quotaUnits = ['KB', 'MB', 'GB', 'TB'] as const

export type QuotaUnit = (typeof quotaUnits)[number]

const quotaUnitBytes: Record<QuotaUnit, number> = {
  KB: 1024,
  MB: 1024 ** 2,
  GB: 1024 ** 3,
  TB: 1024 ** 4
}

export function parseQuotaToBytes(value: string | number, unit: QuotaUnit): number | null {
  const trimmed = String(value).trim()
  if (!trimmed) {
    return 0
  }
  const parsed = Number(trimmed)
  const bytes = parsed * quotaUnitBytes[unit]
  if (!Number.isFinite(parsed) || parsed < 0 || !Number.isSafeInteger(bytes)) {
    return null
  }
  return bytes
}

export function quotaInputFromBytes(bytes: number): { value: string; unit: QuotaUnit } {
  if (bytes === 0) {
    return { value: '0', unit: 'GB' }
  }
  for (const unit of [...quotaUnits].reverse()) {
    const multiplier = quotaUnitBytes[unit]
    if (bytes % multiplier === 0) {
      return { value: String(bytes / multiplier), unit }
    }
  }
  return { value: String(bytes / quotaUnitBytes.KB), unit: 'KB' }
}

export function usagePercent(used: number, quota: number): string {
  if (quota <= 0) {
    return '不限'
  }
  return `${Math.min(100, (used / quota) * 100).toFixed(1)}%`
}
