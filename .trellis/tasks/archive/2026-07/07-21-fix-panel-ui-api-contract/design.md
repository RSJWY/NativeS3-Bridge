# Design: Panel UI / API Contract Alignment

## Boundary and Runtime Mode

Keep one embedded Vue SPA so the existing standalone server and the Panel can continue sharing authentication, static assets, release metadata, and the frontend toolchain. Make the backend mode explicit through the existing public login-settings response:

```json
{
  "totp_required": false,
  "captcha_enabled": false,
  "captcha_provider": "",
  "captcha_site_key": "",
  "service_mode": "panel"
}
```

`webadmin.Auth` owns this response and receives a mode at construction. The default remains `standalone`; `pkg/panel.AdminServer` constructs it with `panel`. This avoids 404-based feature detection and keeps one source of truth at the HTTP boundary.

## Frontend Bootstrap and Routing

The frontend stores only the non-sensitive service mode in reactive runtime state. `Login.vue` records the mode returned by `authSettings()` before completing login. On refresh with a stale local logged-in flag, `App.vue` performs an auth-settings bootstrap before rendering protected routes, so a Panel route never mounts the standalone Dashboard while mode is unknown.

Routes remain in one router:

- standalone: `/dashboard`, `/credentials`, `/buckets`, `/logs`
- panel: `/nodes` and `/nodes/:id`

Navigation and redirects are mode-aware. A mode-incompatible route is redirected to that mode's home route. Login remains common to both modes.

## Panel API Client Contract

Extend the shared typed API client with Panel-owned types and methods for:

- `GET/POST /api/admin/nodes`
- `GET/PATCH/DELETE /api/admin/nodes/{id}`
- `POST /api/admin/nodes/{id}/tokens`
- `GET/POST /api/admin/nodes/{id}/credentials`
- `POST /api/admin/nodes/{id}/credentials/{accessKey}/rotate`
- `POST /api/admin/nodes/{id}/desired-state`
- `POST /api/admin/nodes/{id}/desired-state/push`
- `GET /api/admin/nodes/{id}/certs`
- `POST /api/admin/nodes/{id}/certs/revoke`

All methods reuse `apiFetch`, cookie credentials, JSON error extraction, and the shared 401 redirect behavior. Path identifiers are encoded at the client boundary.

## Panel Views

Use two focused pages rather than adapting the standalone dashboard:

1. `PanelNodes.vue`: node fleet overview and node creation.
2. `PanelNodeDetail.vue`: status/version summary and grouped operational sections for lifecycle, registration, credentials, desired-state delivery, and certificates.

Secrets are held only in component-local refs. They are never copied into global state, localStorage, route query, logs, or list models. Refresh/close clears the current one-time result.

Risky operations use explicit confirmation:

- retire node: irreversible warning
- revoke certificates: connection-impact warning
- rotate credential: warn that desired state must be published/pushed afterward

## Compatibility

- The auth-settings field is additive; older clients ignore it.
- Standalone mode is the constructor default, so existing `webadmin.NewAuth(...)` call sites and tests retain behavior unless explicitly selecting Panel mode.
- Existing standalone Vue pages remain intact and are not mounted in Panel mode.
- No DB migration or deployment configuration change is required. Rebuilding/publishing the Panel image is sufficient.

## Validation Strategy

- Go unit tests assert the additive auth-settings mode field for both modes.
- Panel admin-server tests authenticate and verify `/api/admin/nodes` is routed while a standalone-only route returns the expected Panel 404.
- Frontend build/type-check verifies typed contracts and route imports.
- Browser acceptance starts a real Panel server, logs in, confirms the nodes page renders, and records network responses to prove no standalone-only API is requested.
- Build the Panel binary/image input path to confirm the generated SPA is embedded.

## Rollback

The change is isolated to auth settings, shared SPA routing/client code, new Panel views, tests, and documentation/spec updates. Rollback restores the prior SPA routing and the auth constructor option; there is no data rollback.
