<template>
  <section class="page-stack">
    <div class="page-header">
      <div>
        <h1>仪表盘</h1>
        <p class="muted">查看容量使用、密钥用量排行和最近 30 天请求趋势。</p>
      </div>
      <button class="secondary-button" type="button" :disabled="loading" @click="load">
        {{ loading ? '刷新中…' : '刷新' }}
      </button>
    </div>

    <div v-if="error" class="notice error-notice">{{ error }}</div>

    <section class="summary-grid">
      <div class="summary-card">
        <span>密钥数</span>
        <strong>{{ summary?.total_credentials ?? 0 }}</strong>
      </div>
      <div class="summary-card">
        <span>总已用</span>
        <strong>{{ formatBytes(summary?.total_used_bytes ?? 0) }}</strong>
      </div>
      <div class="summary-card">
        <span>总配额</span>
        <strong>{{ formatQuota(summary?.total_quota_bytes ?? 0) }}</strong>
      </div>
      <div class="summary-card">
        <span>使用率</span>
        <strong>{{ usagePercent(summary?.total_used_bytes ?? 0, summary?.total_quota_bytes ?? 0) }}</strong>
      </div>
    </section>

    <section class="chart-grid">
      <div class="panel chart-panel">
        <h2>容量使用率</h2>
        <div class="chart-frame">
          <div ref="usageChartEl" class="chart-box"></div>
          <div v-if="loading" class="chart-state">加载中…</div>
          <div v-else-if="!hasCapacityData" class="chart-state">暂无容量使用数据</div>
        </div>
      </div>
      <div class="panel chart-panel">
        <h2>各密钥用量排行</h2>
        <div class="chart-frame">
          <div ref="rankingChartEl" class="chart-box"></div>
          <div v-if="loading" class="chart-state">加载中…</div>
          <div v-else-if="!hasRankingData" class="chart-state">暂无密钥用量数据</div>
        </div>
      </div>
      <div class="panel chart-panel chart-wide">
        <h2>请求次数趋势</h2>
        <div class="chart-frame">
          <div ref="trendChartEl" class="chart-box"></div>
          <div v-if="loading" class="chart-state">加载中…</div>
          <div v-else-if="!hasTrendData" class="chart-state">暂无请求趋势数据</div>
        </div>
      </div>
    </section>
  </section>
</template>

<script setup lang="ts">
import { BarChart, LineChart, PieChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import * as echarts from 'echarts/core'
import { CanvasRenderer } from 'echarts/renderers'
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { adminApi, type DashboardSummary, type RequestTrendItem, type UsageRankingItem } from '../api/client'
import { formatBytes, formatQuota, usagePercent } from '../utils/format'

echarts.use([PieChart, BarChart, LineChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer])

const summary = ref<DashboardSummary | null>(null)
const ranking = ref<UsageRankingItem[]>([])
const trend = ref<RequestTrendItem[]>([])
const error = ref('')
const loading = ref(false)
const usageChartEl = ref<HTMLDivElement | null>(null)
const rankingChartEl = ref<HTMLDivElement | null>(null)
const trendChartEl = ref<HTMLDivElement | null>(null)
let usageChart: echarts.ECharts | null = null
let rankingChart: echarts.ECharts | null = null
let trendChart: echarts.ECharts | null = null

const hasCapacityData = computed(() => {
  const total = summary.value?.total_quota_bytes ?? 0
  const used = summary.value?.total_used_bytes ?? 0
  return total > 0 || used > 0
})
const hasRankingData = computed(() => ranking.value.some((item) => item.used_bytes > 0))
const hasTrendData = computed(() => trend.value.some((item) => item.put_count > 0 || item.get_count > 0 || item.delete_count > 0))

onMounted(async () => {
  window.addEventListener('resize', resizeCharts)
  await load()
})

onBeforeUnmount(() => {
  window.removeEventListener('resize', resizeCharts)
  usageChart?.dispose()
  rankingChart?.dispose()
  trendChart?.dispose()
})

async function load() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    const [summaryResult, rankingResult, trendResult] = await Promise.all([
      adminApi.dashboardSummary(),
      adminApi.usageRanking(),
      adminApi.requestTrend(30)
    ])
    summary.value = summaryResult
    ranking.value = rankingResult
    trend.value = trendResult
  } catch (err) {
    error.value = err instanceof Error ? err.message : '加载仪表盘失败'
  } finally {
    loading.value = false
    await nextTick()
    renderCharts()
  }
}

function renderCharts() {
  renderUsageChart()
  renderRankingChart()
  renderTrendChart()
}

function renderUsageChart() {
  if (!usageChartEl.value) return
  usageChart ||= echarts.init(usageChartEl.value)
  const total = summary.value?.total_quota_bytes ?? 0
  const used = summary.value?.total_used_bytes ?? 0
  const remaining = Math.max(total - used, 0)
  usageChart.setOption({
    tooltip: { formatter: ({ name, value }: { name: string; value: number }) => `${name}: ${formatBytes(value)}` },
    legend: { bottom: 0 },
    series: [
      {
        type: 'pie',
        radius: ['50%', '72%'],
        center: ['50%', '44%'],
        label: { formatter: '{b}' },
        data: getUsageChartData(total, used, remaining)
      }
    ]
  })
}

function getUsageChartData(total: number, used: number, remaining: number) {
  if (!hasCapacityData.value) {
    return []
  }
  if (total > 0) {
    return [{ name: '已用', value: used }, { name: '剩余', value: remaining }]
  }
  return [{ name: '已用', value: used }]
}

function renderRankingChart() {
  if (!rankingChartEl.value) return
  rankingChart ||= echarts.init(rankingChartEl.value)
  rankingChart.setOption({
    tooltip: { formatter: ({ name, value }: { name: string; value: number }) => `${name}: ${formatBytes(value)}` },
    grid: { left: 56, right: 24, top: 20, bottom: 72 },
    xAxis: { type: 'category', data: ranking.value.map((item) => item.name || item.access_key), axisLabel: { rotate: 30 } },
    yAxis: { type: 'value', axisLabel: { formatter: (value: number) => formatBytes(value) } },
    series: [{ type: 'bar', data: ranking.value.map((item) => item.used_bytes), itemStyle: { color: '#57534e' } }]
  })
}

function renderTrendChart() {
  if (!trendChartEl.value) return
  trendChart ||= echarts.init(trendChartEl.value)
  trendChart.setOption({
    tooltip: { trigger: 'axis' },
    legend: { top: 0 },
    grid: { left: 48, right: 24, top: 42, bottom: 36 },
    xAxis: { type: 'category', data: trend.value.map((item) => item.day.slice(5)) },
    yAxis: { type: 'value' },
    series: [
      { name: 'PUT', type: 'line', data: trend.value.map((item) => item.put_count), smooth: false },
      { name: 'GET', type: 'line', data: trend.value.map((item) => item.get_count), smooth: false },
      { name: 'DELETE', type: 'line', data: trend.value.map((item) => item.delete_count), smooth: false }
    ]
  })
}

function resizeCharts() {
  usageChart?.resize()
  rankingChart?.resize()
  trendChart?.resize()
}
</script>
