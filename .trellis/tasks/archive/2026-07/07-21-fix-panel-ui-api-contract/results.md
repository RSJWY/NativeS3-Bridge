# Results

## Outcome

- Added explicit `service_mode` discovery to `/api/admin/auth-settings` with standalone as the backward-compatible default and Panel as an explicit constructor choice.
- Gated protected SPA rendering until mode discovery finishes, then selected mode-specific routes/navigation.
- Added Panel node fleet and detail pages for node creation/status/retirement, registration tokens, node credentials and secret rotation, desired-state publishing/pushing, and certificate inspection/revocation.
- Added backend regression coverage for Panel mode and route registration.
- Recorded the shared-SPA service-mode contract in backend and frontend Trellis specs.

## Verification

- `npm --prefix pkg/webadmin/ui run build` — passed (`vue-tsc --noEmit` + Vite production build).
- `go test ./...` — passed in the non-sandbox environment required by existing Panel WebSocket tests.
- `go vet ./...` — passed.
- `go build ./cmd/panel` — passed.
- `git diff --check` — passed.

### Panel browser smoke

- Logged in and reached `/nodes`.
- Created `smoke-node`, opened `/nodes/1`, and signed a one-time registration token.
- Captured only `auth-settings`, login, and `/api/admin/nodes*` responses; all were 200/201.
- No standalone dashboard/bucket/top-level credential/log API was requested.
- Repeated at a 390x844 viewport and confirmed no page-level horizontal overflow.

### Standalone browser smoke

- Logged in and reached `/dashboard`.
- Dashboard summary, usage ranking, and request trend returned 200.
- No `/api/admin/nodes` request was made.

## Environment Note

Sandboxed full tests cannot open the temporary localhost sockets used by existing Panel transport tests. The same full suite passed outside the socket-restricted sandbox; this is an environment constraint rather than a test failure.
