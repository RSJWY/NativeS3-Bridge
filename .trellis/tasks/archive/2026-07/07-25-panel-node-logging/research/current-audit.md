# Current audit: Panel/Node logging

Date: 2026-07-25

## Repository evidence

- `Dockerfile` and `.github/workflows/release.yml` build only `cmd/panel` and `cmd/node` as supported release targets. `scripts/test-distribution-contract.sh` rejects `cmd/natives3bridge` in the release workflow. `scripts/test-upgrade-rollback.sh` builds a legacy standalone binary only from a legacy ref for rollback verification.
- `cmd/panel/main.go`, `cmd/node/main.go`, and `cmd/natives3bridge/main.go` each configure `slog` with stdout, optional lumberjack, and `logging.Ring`. Panel currently discards the returned ring when constructing `panel.AdminServer`.
- `pkg/webadmin/server.go` mounts the authenticated standalone `/api/admin/logs`; `pkg/panel/adminserver.go` mounts only auth and `/api/admin/nodes*`, so Panel mode has no own log API. The shared SPA hides `/logs` in Panel mode through `src/App.vue` and `src/state/runtime.ts`.
- `pkg/webadmin/logs.go` already provides safe current/history file enumeration, gzip reading, level/query/limit filtering, ring fallback, and traversal/symlink protections. It should be reused rather than duplicated.
- `controlproto.TaskLogQuery`, `panel.TaskOrchestrator`, Panel task REST routes, and `nodeagent.LocalTaskRunner` already exist. The runner currently reads only the ring, ignores `TaskParams.Since`/`Until` and level, and emits only `LogLines` strings. The Panel UI has no typed task client or node log section.
- Existing task protocol constraints are bounded one-shot tasks over mTLS, no arbitrary shell, idempotent task IDs, in-flight backpressure, and no queued execution for offline nodes.
- Existing logging tests cover generic lumberjack and standalone initialization, but not Panel/Node initialization or real Panel↔Node log-query UI flow.

## Baseline verification

The following passed before implementation:

```text
go test ./pkg/config ./pkg/logging ./pkg/webadmin ./pkg/panel ./pkg/nodeagent
go test ./cmd/panel ./cmd/node
npm run build --prefix pkg/webadmin/ui
```

## Decisions recorded for this task

1. Production scope is Panel/Node. Standalone receives no new product feature; keep compile, existing tests, and rollback compatibility.
2. Panel own logs can read current and rotated local files through the existing safe viewer.
3. Node remote logs are ring-only in the first phase. Node local file rotation is still fully tested; raw historical files are not sent over the control channel.

## Relevant prior work

- Archived tasks `07-12-admin-logging` and `07-12-log-directory-history` define the existing ring/file/history contracts and security tests.
- Archived Panel UI contract work intentionally hid standalone-only `/logs` in Panel mode; this task reintroduces the route only after Panel owns a compatible `/api/admin/logs` handler.
