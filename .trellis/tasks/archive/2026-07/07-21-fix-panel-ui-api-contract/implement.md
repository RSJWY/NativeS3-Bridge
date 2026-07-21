# Implementation Plan

1. Add an explicit standalone/panel service-mode contract to `pkg/webadmin.Auth`, update Panel construction, and add focused Go tests.
2. Add frontend runtime-mode state and mode-aware bootstrap/route guards without changing standalone page behavior.
3. Extend the shared API client with typed Panel node, token, credential, desired-state, and certificate methods.
4. Add Panel fleet and node-detail views using existing shared UI patterns; keep all plaintext token/secret values component-local and once-only.
5. Add only scoped CSS needed by the new Panel pages in a separate stylesheet, preserving the user's current dashboard/style work.
6. Add server/browser regression coverage that validates actual Panel API/UI behavior rather than only SPA HTTP 200.
7. Update the backend/frontend executable specs with the explicit service-mode and Panel UI contracts.
8. Run formatting, focused Go tests, full relevant Go tests, frontend production build, Panel binary build, and browser smoke. Review the final diff for accidental changes to pre-existing user work.

## Validation Commands

```bash
gofmt -w <changed Go files>
go test ./pkg/webadmin ./pkg/panel
npm --prefix pkg/webadmin/ui run build
go build ./cmd/panel
go test ./...
```

Browser smoke will run against a temporary SQLite Panel configuration and use a temporary data directory/ports under `/tmp`.

## Risk and Rollback Points

- `pkg/webadmin/ui/src/App.vue` and router changes affect both modes; verify standalone redirect/navigation separately.
- `pkg/webadmin/ui/src/styles.css` already has user changes; keep Panel-only rules in `src/panel.css` so task commits do not absorb unrelated style work.
- `pkg/webadmin/ui/dist` is generated/embedded output; regenerate only through the project build.
- Do not modify or discard the user's existing uncommitted dashboard/config/install-script work.
