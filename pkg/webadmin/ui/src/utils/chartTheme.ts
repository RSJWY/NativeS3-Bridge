// ECharts 共享主题:与 styles.css 的 teal/stone 色系保持一致。
import type { EChartsOption } from 'echarts'

export const CHART_COLORS = ['#0f766e', '#57534e', '#b45309', '#9f1239']

export const chartText = {
  color: '#57534e',
  fontFamily: 'ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, sans-serif'
} as const

export const chartTooltip: NonNullable<EChartsOption['tooltip']> = {
  backgroundColor: '#ffffff',
  borderColor: '#e7e5e4',
  textStyle: { color: '#1c1917', fontSize: 13 },
  extraCssText: 'box-shadow: 0 4px 12px rgba(28,25,23,0.1); border-radius: 7px; padding: 8px 10px;'
}

export const chartAxis = {
  axisLine: { lineStyle: { color: '#d6d3d1' } },
  axisTick: { show: false },
  axisLabel: { color: '#78716c', fontSize: 12 },
  splitLine: { lineStyle: { color: '#f0efee' } }
} as const
