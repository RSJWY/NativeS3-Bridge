import { reactive } from 'vue'

export type ServiceMode = 'standalone' | 'panel'

export const runtimeState = reactive<{ serviceMode: ServiceMode; ready: boolean }>({
  serviceMode: 'standalone',
  ready: false
})

export function setServiceMode(mode: ServiceMode) {
  runtimeState.serviceMode = mode === 'panel' ? 'panel' : 'standalone'
  runtimeState.ready = true
}

export function serviceHomePath(mode: ServiceMode = runtimeState.serviceMode) {
  return mode === 'panel' ? '/panel-dashboard' : '/dashboard'
}

export function routeMatchesService(path: string, mode: ServiceMode = runtimeState.serviceMode) {
  const nodeRoute = path === '/nodes' || path.startsWith('/nodes/')
  if (mode === 'panel') {
    return nodeRoute || path === '/panel-dashboard' || path === '/logs'
  }
  // standalone 保留共享的 /logs,只拒绝 Panel 专属路由。
  return !nodeRoute && path !== '/panel-dashboard'
}
