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
  return mode === 'panel' ? '/nodes' : '/dashboard'
}

export function routeMatchesService(path: string, mode: ServiceMode = runtimeState.serviceMode) {
  const panelRoute = path === '/nodes' || path.startsWith('/nodes/')
  return mode === 'panel' ? panelRoute : !panelRoute
}
