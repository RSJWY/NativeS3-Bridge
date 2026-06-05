import { reactive } from 'vue'

const SESSION_KEY = 'natives3.admin.loggedIn'

export const authState = reactive({
  loggedIn: localStorage.getItem(SESSION_KEY) === '1'
})

export function markLoggedIn() {
  authState.loggedIn = true
  localStorage.setItem(SESSION_KEY, '1')
}

export function markLoggedOut() {
  authState.loggedIn = false
  localStorage.removeItem(SESSION_KEY)
}
