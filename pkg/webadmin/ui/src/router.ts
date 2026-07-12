import { createRouter, createWebHistory } from 'vue-router'
import Login from './views/Login.vue'
import Dashboard from './views/Dashboard.vue'
import Credentials from './views/Credentials.vue'
import Buckets from './views/Buckets.vue'
import Logs from './views/Logs.vue'
import { authState } from './state/auth'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', name: 'login', component: Login, meta: { public: true } },
    { path: '/', redirect: '/dashboard' },
    { path: '/dashboard', name: 'dashboard', component: Dashboard },
    { path: '/credentials', name: 'credentials', component: Credentials },
    { path: '/buckets', name: 'buckets', component: Buckets, meta: { requiresAuth: true } },
    { path: '/logs', name: 'logs', component: Logs, meta: { requiresAuth: true } }
  ]
})

router.beforeEach((to) => {
  if (!to.meta.public && !authState.loggedIn) {
    return { path: '/login', query: { redirect: to.fullPath } }
  }
  if (to.path === '/login' && authState.loggedIn) {
    return '/dashboard'
  }
  return true
})

export default router
