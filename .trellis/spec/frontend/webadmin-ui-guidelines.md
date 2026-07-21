# Webadmin UI Guidelines

> Vue3 + Vite + ECharts admin UI contracts for the embedded NativeS3-Bridge management interface.

---

## Scenario: Embedded Admin UI

### 1. Scope / Trigger

- Trigger: any change under `pkg/webadmin/ui`, admin API payload types, Vite config, router/auth state, dashboard charts, or credential management UI.
- Goal: keep the frontend contract aligned with `pkg/webadmin` JSON APIs while preserving the frozen Vue3 + Vite + ECharts + go:embed stack.

### 2. Signatures

- App entry: `src/main.ts` mounts `App.vue` with `vue-router`.
- Routes: `/login`, `/dashboard`, `/credentials`, `/buckets`; `/` redirects to `/dashboard`.
- API client: `apiFetch<T>(path: string, options?: RequestOptions): Promise<T>` with `credentials: 'include'`.
- Admin API methods: `login`, `logout`, `listCredentials`, `createCredential`, `updateCredential`, `deleteCredential`, `listBuckets`, `createBucket`, `deleteBucket`, `setBucketACL`, `dashboardSummary`, `usageRanking`, `requestTrend`.
- Logs client: `adminApi.logs({ limit, level?, q?, file? })`; response includes `files: LogFileInfo[]` and optional `selected_file`.
- Vite proxy: `server.proxy['/api'] = 'http://localhost:9001'`.
- Build command: `npm run build` runs `vue-tsc --noEmit && vite build` into `dist/`.
- Build version input: optional `APP_VERSION`; Vite exposes it as the compile-time string `__APP_VERSION__`.

### 3. Contracts

- The UI must use Vue3 Composition API and ECharts from `echarts/core`; do not replace with another framework or chart library.
- API requests must include cookies via `credentials: 'include'` so the signed session cookie is sent.
- A `401` from the API marks local auth state logged out and redirects to `/login`.
- Login redirects must be normalized to safe internal paths only; never redirect to protocol-relative or external URLs.
- Credential list/update/delete UI must not display or persist `secret_key`; only the create modal displays `secret_key` once from the create response.
- Credential bucket scope uses the admin bucket list as a select (`全部桶` plus existing names); dangling historical bindings are visibly marked and must be changed before saving.
- Bucket management UI must call `/api/admin/buckets*` through the shared API client with `credentials: 'include'`; it must not implement or call S3 `PutBucketAcl`.
- Bucket ACL values in the UI are exactly `private` and `public-read`; display copy is Chinese (`私有`, `公开下载`) and `public-read` rows must warn that objects can be anonymously downloaded.
- Bucket delete actions require `window.confirm` or an equivalent explicit second confirmation; non-empty bucket `409` must show a visible friendly error and must not be treated as success.
- Credential quota input is a numeric value plus a `KB`, `MB`, `GB`, or `TB` selector; the frontend converts it to the API's raw `quota_bytes` field with binary 1024-based multipliers. Empty or zero input means `0` (unlimited); invalid, negative, non-finite, unsafe, or non-integral byte results must block submit with a visible error instead of becoming unlimited silently.
- Editing a credential must choose the largest unit that represents its existing `quota_bytes` exactly; legacy values smaller than 1 KB fall back to a precise fractional KB value so saving without changes preserves the byte count.
- Dashboard must render three real ECharts charts from API data: capacity usage donut, usage ranking bar chart, request trend line chart.
- UI styling should stay functional and restrained: normal sidebar, simple cards/tables/forms, no gradients, no glass shells, no decorative hero copy, no oversized radii.
- Application release metadata has one frontend source: `APP_VERSION` is trimmed and falls back to `dev`; Release workflow and Docker Buildx must pass the same release tag, while local builds omit it.
- GitHub URL and compiled version must be defined in shared project config and rendered through a shared component on both login and protected application shells. External repository links use `target="_blank"` and `rel="noopener noreferrer"`.
- Reuse shared visual state classes instead of one-off styles: wrap wide tables in `.table-scroll`, use `.state-row` for loading/empty table rows, use `.status-badge` for credential status, and use `.chart-state` overlays for dashboard loading/empty chart states.
- The logs toolbar starts with a normal select populated only from `files`; labels distinguish current history and show history time, size, and gzip state. Changing files preserves level/query/limit and requests the selected ID through the shared API client.
- A selected-history request failure must remain visible, clear stale entries, keep the selection available so the user can switch back, and never relabel another source as the selected file. File-disabled responses keep the ring view and show the `log.dir` setup notice.

### 4. Validation & Error Matrix

- Login API error -> visible login error text.
- Protected API returns `401` -> local auth cleared and route redirected to `/login`.
- Create/update quota invalid or not exactly representable as whole bytes -> block submit and show a form error.
- Create/update/delete/toggle failure -> show a visible page or form error; do not leave unhandled promise rejections.
- Bucket create invalid name -> visible error explaining lowercase/digit/hyphen 3-63 character naming constraints.
- Bucket ACL update failure -> visible page error and UI selection returns to the previous ACL value.
- Bucket non-empty delete -> visible error that the bucket is not empty and objects must be removed first.
- Missing dashboard data -> render empty charts/zero values without throwing.
- Mobile viewport under 900px -> sidebar becomes top navigation and charts/tables remain reachable.
- Empty or whitespace-only `APP_VERSION` -> compile `dev`; non-empty release tag -> render that exact tag without a runtime request.
- Wide credential or bucket tables on narrow viewports -> horizontal scrolling within `.table-scroll`, not page-level overflow.
- Selected log file removed or unreadable -> visible API error and no stale/other-file entries; no file logging -> ring entries plus an explicit in-memory notice.

### 5. Good/Base/Bad Cases

- Good: browser opens `/`, sees login, submits correct password, reaches `/dashboard`, and sees three chart canvases.
- Good: creating a credential opens a modal with the one-time `Secret Key`; after closing, the table only shows access key/name/status/quota/usage.
- Good: entering `10` with unit `GB` sends `quota_bytes: 10737418240`; editing that credential shows `10 GB`.
- Good: opening `/buckets` after login lists bucket `name`, `acl`, and `created_at`; creating a valid bucket refreshes the table; switching ACL to `public-read` refreshes and shows the `公开下载` badge.
- Base: refresh with stale local auth state may hit an API `401`, clear state, and redirect to login.
- Base: `npm run build` without release environment renders `dev` on login and protected pages.
- Good: Release binary and Docker image built for tag `v1.2.3` both render `v1.2.3` and the canonical GitHub URL.
- Bad: using `package.json` dependency version as the application release, or passing a tag only to the binary build while Docker falls back to `dev`.
- Bad: hiding a failed bucket delete or leaving the ACL select visually changed after the server rejects the update.
- Bad: asking users to enter raw bytes, or silently converting invalid quota text to `0`, because the former is error-prone and the latter grants unlimited capacity.
- Bad: storing the secret key in localStorage or showing it in the credential table.

### 6. Tests Required

- `npm ci && npm run build` for dependency lock, TypeScript checking, and production bundle creation.
- Logs build/browser check: current/history options render from typed API metadata, selection retains level/query/limit, gzip labels are visible, and a failed selected-file request clears stale entries while preserving a route back to current.
- Quota conversion checks: assert KB/MB/GB/TB multipliers, zero/unlimited handling, fractional values that resolve to whole bytes, unsafe values, and exact edit-form round trips.
- Browser smoke with real Chrome: login page text renders, successful login reaches dashboard, at least three `.chart-box canvas` elements exist, credentials page renders.
- Bucket page smoke: login, navigate to `/buckets`, create a bucket, switch ACL to `public-read`, confirm the list shows `公开下载`, switch back to `private`, attempt non-empty delete and verify the friendly error.
- API smoke through the UI session cookie: create credential, list credentials, confirm list response lacks `secret_key`.
- Real `aws-cli` smoke using a UI-created credential, followed by disabling that credential and confirming S3 rejects it.
- Responsive manual/browser check at desktop and mobile widths when changing layout or CSS.
- Build metadata check: inspect default assets for `dev`, rebuild with `APP_VERSION=<tag>`, and assert the tag and canonical GitHub URL are present; verify Release UI env and Docker build arg use the same tag output.

### 7. Wrong vs Correct

Wrong:

```ts
return Number(value) || 0 // invalid values become unlimited quota and ignore the selected unit
```

Correct:

```ts
const parsed = Number(String(value).trim()) // type=number v-model yields a number after editing
const bytes = parsed * quotaUnitBytes[unit]
if (!Number.isFinite(parsed) || parsed < 0 || !Number.isSafeInteger(bytes)) {
  return null
}
return bytes
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

Wrong:

```ts
await adminApi.setBucketACL(bucket.name, nextACL)
bucket.acl = nextACL // optimistic state remains if the request fails
```

Correct:

```ts
try {
  await adminApi.setBucketACL(bucket.name, nextACL)
  await load()
} catch (err) {
  target.value = bucket.acl
  error.value = toBucketError(err, '更新访问权限失败')
}
```

Wrong:

```ts
export const appVersion = '0.1.0' // package metadata is not the application release
```

Correct:

```ts
const appVersion = process.env.APP_VERSION?.trim() || 'dev'
define: { __APP_VERSION__: JSON.stringify(appVersion) }
```

Wrong:

```ts
adminApi.logs({ file: '/state/logs/app.log', limit }) // client invents a path
```

Correct:

```ts
adminApi.logs({ file: selectedFileID, limit, level, q }) // ID came from response.files
```

---

## Common Mistakes

- Do not add Pinia for this UI unless the scope grows beyond login state and server-fetched page data; the current convention is a lightweight composable.
- Do not import full alternative chart packages. ECharts is frozen, and dashboard charts should use the API payloads rather than placeholder data.
- Do not create page-specific loading/empty table styles when `.state-row` and `.table-scroll` already cover the pattern; divergent one-offs make the admin pages feel inconsistent.

## Scenario: Runtime-Selected Standalone And Panel UI

### 1. Scope / Trigger

- Trigger: adding a second backend that embeds the same Vue bundle but exposes a different admin API surface.
- Goal: select routes and navigation before protected pages mount, so an incompatible page cannot fire a burst of expected 404 requests.

### 2. Signatures

- `AuthSettings.service_mode: 'standalone' | 'panel'`.
- Runtime owner: `runtimeState`, `setServiceMode`, `serviceHomePath`, and `routeMatchesService` in `src/state/runtime.ts`.
- Panel routes: `/nodes` and `/nodes/:id`; standalone routes remain `/dashboard`, `/credentials`, `/buckets`, and `/logs`.

### 3. Contracts

- Login records `service_mode` before redirecting.
- On a protected-page refresh, `App.vue` fetches auth settings and gates `<router-view>` until runtime mode is ready.
- Route redirects and sidebar navigation use the shared runtime helpers; components do not implement private mode checks.
- Panel pages call only typed `/api/admin/nodes*` methods through the shared `apiFetch` client.
- One-time registration tokens and credential secrets remain component-local and are cleared when their result modal closes.

### 4. Validation & Error Matrix

- Auth settings unavailable -> protected page remains gated and shows a retry/login choice; neither mode's data page mounts.
- Panel user opens a standalone route -> redirect to `/nodes` before the standalone component mounts.
- Standalone user opens a Panel route -> redirect to `/dashboard` before the Panel component mounts.
- Protected API `401` -> clear login state and redirect to `/login`, unchanged across modes.

### 5. Good/Base/Bad Cases

- Good: Panel login reaches `/nodes`, creates a node, and signs a token with no standalone API request in the network log.
- Good: standalone login reaches `/dashboard` and loads all three dashboard endpoints with no `/api/admin/nodes` request.
- Base: an older backend omitting `service_mode` is normalized by runtime code to standalone behavior.
- Bad: probe mode by calling `/api/admin/nodes` and interpreting a 404 or transient error as standalone.

### 6. Tests Required

- `npm run build` for typed mode values, routes, and API methods.
- Panel browser smoke: login -> `/nodes` -> create node -> node detail -> issue token; reject any standalone API request or API status >= 400.
- Standalone browser smoke: login -> `/dashboard`; reject any `/api/admin/nodes` request or dashboard API status >= 400.
- Narrow viewport check for node tables, detail actions, and one-time secret modals.

### 7. Wrong vs Correct

Wrong:

```vue
<router-view /> <!-- Dashboard mounts before the backend mode is known -->
```

Correct:

```vue
<router-view v-if="runtimeState.ready" />
```
