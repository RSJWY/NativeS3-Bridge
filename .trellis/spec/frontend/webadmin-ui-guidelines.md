# Webadmin UI Guidelines

> Vue3 + Vite + ECharts admin UI contracts for the embedded NativeS3-Bridge management interface.

---

## Scenario: Embedded Admin UI

### 1. Scope / Trigger

- Trigger: any change under `pkg/webadmin/ui`, admin API payload types, Vite config, router/auth state, dashboard charts, or credential management UI.
- Goal: keep the frontend contract aligned with `pkg/webadmin` JSON APIs while preserving the frozen Vue3 + Vite + ECharts + go:embed stack.

### 2. Signatures

- App entry: `src/main.ts` mounts `App.vue` with `vue-router`.
- Routes: `/login`, `/dashboard`, `/credentials`; `/` redirects to `/dashboard`.
- API client: `apiFetch<T>(path: string, options?: RequestOptions): Promise<T>` with `credentials: 'include'`.
- Admin API methods: `login`, `logout`, `listCredentials`, `createCredential`, `updateCredential`, `deleteCredential`, `dashboardSummary`, `usageRanking`, `requestTrend`.
- Vite proxy: `server.proxy['/api'] = 'http://localhost:9001'`.
- Build command: `npm run build` runs `vue-tsc --noEmit && vite build` into `dist/`.

### 3. Contracts

- The UI must use Vue3 Composition API and ECharts from `echarts/core`; do not replace with another framework or chart library.
- API requests must include cookies via `credentials: 'include'` so the signed session cookie is sent.
- A `401` from the API marks local auth state logged out and redirects to `/login`.
- Login redirects must be normalized to safe internal paths only; never redirect to protocol-relative or external URLs.
- Credential list/update/delete UI must not display or persist `secret_key`; only the create modal displays `secret_key` once from the create response.
- Quota input is raw bytes. Empty input means `0` (unlimited); invalid, negative, non-finite, or unsafe numbers must block submit with a visible error instead of becoming unlimited silently.
- Dashboard must render three real ECharts charts from API data: capacity usage donut, usage ranking bar chart, request trend line chart.
- UI styling should stay functional and restrained: normal sidebar, simple cards/tables/forms, no gradients, no glass shells, no decorative hero copy, no oversized radii.

### 4. Validation & Error Matrix

- Login API error -> visible login error text.
- Protected API returns `401` -> local auth cleared and route redirected to `/login`.
- Create/update quota invalid -> block submit and show a form error.
- Create/update/delete/toggle failure -> show a visible page or form error; do not leave unhandled promise rejections.
- Missing dashboard data -> render empty charts/zero values without throwing.
- Mobile viewport under 900px -> sidebar becomes top navigation and charts/tables remain reachable.

### 5. Good/Base/Bad Cases

- Good: browser opens `/`, sees login, submits correct password, reaches `/dashboard`, and sees three chart canvases.
- Good: creating a credential opens a modal with the one-time `Secret Key`; after closing, the table only shows access key/name/status/quota/usage.
- Base: refresh with stale local auth state may hit an API `401`, clear state, and redirect to login.
- Bad: silently converting invalid quota text to `0`, because that grants unlimited capacity.
- Bad: storing the secret key in localStorage or showing it in the credential table.

### 6. Tests Required

- `npm ci && npm run build` for dependency lock, TypeScript checking, and production bundle creation.
- Browser smoke with real Chrome: login page text renders, successful login reaches dashboard, at least three `.chart-box canvas` elements exist, credentials page renders.
- API smoke through the UI session cookie: create credential, list credentials, confirm list response lacks `secret_key`.
- Real `aws-cli` smoke using a UI-created credential, followed by disabling that credential and confirming S3 rejects it.
- Responsive manual/browser check at desktop and mobile widths when changing layout or CSS.

### 7. Wrong vs Correct

Wrong:

```ts
return Number(value) || 0 // invalid values become unlimited quota
```

Correct:

```ts
const parsed = Number(value.trim())
if (!Number.isFinite(parsed) || parsed < 0 || !Number.isSafeInteger(Math.floor(parsed))) {
  return null
}
return Math.floor(parsed)
```

Wrong:

```ts
await router.replace(route.query.redirect as string)
```

Correct:

```ts
const redirect = normalizeRedirect(route.query.redirect)
await router.replace(redirect)
```

---

## Common Mistakes

- Do not add Pinia for this UI unless the scope grows beyond login state and server-fetched page data; the current convention is a lightweight composable.
- Do not import full alternative chart packages. ECharts is frozen, and dashboard charts should use the API payloads rather than placeholder data.
