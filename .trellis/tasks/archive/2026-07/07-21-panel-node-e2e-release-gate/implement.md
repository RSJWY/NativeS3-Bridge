# Implementation Plan

## Phase 0 — Planning gate

- [x] User approves the final planning summary.
- [x] Run `task.py start 07-21-panel-node-e2e-release-gate` only after approval.
- [x] Load `trellis-before-dev` and the listed backend/frontend specs in the active implementation context.

## Phase 1 — Reusable test harness

- [x] Add prerequisite checks, random port allocation, redacted logging, and trap cleanup to `scripts/test-panel-node-e2e.sh`.
- [x] Reuse the PKI/config conventions from `scripts/test-release-integrity.sh`; keep panel and node data roots separate.
- [x] Implement local process adapter and readiness/termination diagnostics.
- [x] Implement Docker adapter with unique network/container/image names and no default-port collisions.

## Phase 2 — Control/data-plane assertions

- [x] Add authenticated Admin API helpers for node/token/bucket/credential/publish operations.
- [x] Add polling assertions for online, heartbeat, desired/applied version, and `synced` state.
- [x] Add curl SigV4 bucket/object CRUD and native-file byte comparisons.
- [x] Add panel outage/restart and node restart scenarios.
- [x] Add wrong-CA negative scenario and explicit S3 safety-net assertion.

## Phase 3 — Browser assertion

- [x] Add `scripts/internal/e2e-browser.py` using ChromeDriver HTTP endpoints and stdlib JSON/HTTP only.
- [x] Assert Panel mode redirects `/dashboard` to `/nodes`, renders the created node, and emits no forbidden standalone API requests.
- [x] Redact browser diagnostics and clean the temporary Chrome profile.

## Phase 4 — GitHub Actions integration

- [x] Add `e2e` job to `.github/workflows/release.yml` after `quality`.
- [x] Make `artifacts`, `images`, and `release` depend on `e2e`.
- [x] Upload only redacted reports/logs on failure; do not upload data roots, databases, certificates, cookies, or configs containing secrets.
- [x] Preserve existing workflow permissions and tag/source SHA pinning.

## Phase 5 — Verification and handoff

- [x] Run `bash -n scripts/test-panel-node-e2e.sh scripts/internal/e2e-browser.py` (or the appropriate Python compile check).
- [x] Run local mode twice with Go 1.21 and the existing `go vet ./...`, `go test ./...`, `go test -race ./...` gates.
- [x] Run Docker mode twice when Docker is available; validate both repository Compose templates with `docker compose config`.
- [x] Run `npm ci && npm run build` and `bash scripts/test-distribution-contract.sh`.
- [x] Commit task-owned files in Phase 3.4 (`008fc14`, `a5fdf28`). A real
  tag-triggered publication is deferred to the next user-authorized release.

## Risky Files / Rollback Points

- `.github/workflows/release.yml`: rollback by removing only the `e2e` job dependency and failure artifact step.
- `scripts/test-panel-node-e2e.sh`: keep runtime adapters isolated from product code; if Docker behavior is flaky, local mode remains the required diagnostic path.
- `scripts/internal/e2e-browser.py`: no package lock changes; delete the helper to roll back browser coverage.
- Any discovered runtime defect must be split into a separate fix commit/task rather than hidden with a test skip.
