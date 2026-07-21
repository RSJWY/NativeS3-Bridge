import { createRouter, createWebHistory } from 'vue-router'
import Login from './views/Login.vue'
import Dashboard from './views/Dashboard.vue'
import Credentials from './views/Credentials.vue'
import Buckets from './views/Buckets.vue'
import Logs from './views/Logs.vue'
import PanelNodes from './views/PanelNodes.vue'
import PanelNodeDetail from './views/PanelNodeDetail.vue'
import { authState } from './state/auth'
import { routeMatchesService, runtimeState, serviceHomePath } from './state/runtime'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', name: 'login', component: Login, meta: { public: true } },
    { path: '/', redirect: '/dashboard' },
    { path: '/dashboard', name: 'dashboard', component: Dashboard },
    { path: '/credentials', name: 'credentials', component: Credentials },
    { path: '/buckets', name: 'buckets', component: Buckets, meta: { requiresAuth: true } },
    { path: '/logs', name: 'logs', component: Logs, meta: { requiresAuth: true } },
    { path: '/nodes', name: 'panel-nodes', component: PanelNodes },
    { path: '/nodes/:id', name: 'panel-node-detail', component: PanelNodeDetail }
  ]
})

router.beforeEach((to) => {
  if (!to.meta.public && !authState.loggedIn) {
    return { path: '/login', query: { redirect: to.fullPath } }
  }
  if (to.path === '/login' && authState.loggedIn) {
    return serviceHomePath()
  }
  if (!to.meta.public && runtimeState.ready && !routeMatchesService(to.path)) {
    return serviceHomePath()
  }
  return true
})

export default router
