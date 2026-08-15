# 节点遥测基础执行计划

## Ordered Checklist

1. Add protocol contract tests for optional telemetry, explicit zero values, omitted fields, malformed/invalid observation timestamps, and old/new peer decoding.
2. Add node-agent telemetry model/migration and a focused recorder API; cover singleton recovery, invalid state, overflow/negative guards, and atomic mutation deltas.
3. Implement the synchronous startup baseline using the existing storage exclusion rules; test pre-existing objects, sidecars, `.multipart`, database files, zero-byte objects, and failure behavior.
4. Wire the recorder into managed Node object, copy, delete, batch delete, and multipart-complete success paths without changing standalone constructors or quota semantics.
5. Extend Node heartbeat loading/sending and add tests proving no `WalkDir` occurs on the heartbeat path and invalid counters omit telemetry fields.
6. Add nullable Panel `NodeState` telemetry columns and migration tests from the current schema.
7. Persist complete valid telemetry observations in `TransportServer.handleHeartbeat`; define and test legacy-heartbeat behavior, timestamp normalization, and stale snapshot handling.
8. Inject the Panel heartbeat/offline configuration into `AdminAPI`; extend the existing dashboard summary aggregation with valid/missing/stale node telemetry and totals.
9. Add Panel API tests for custom expiry threshold, retired nodes, missing fields, zero values, stale values, auth, method rejection, and sensitive-field exclusion.
10. Extend the typed frontend client and `PanelDashboard.vue` with telemetry cards/status rows, explicit unavailable/expired rendering, loading/empty/error retention, and no standalone regressions.
11. Run browser acceptance for Panel and standalone routing plus telemetry states; keep the existing ChromeDriver workflow.

## Validation Commands

```bash
gofmt -w <changed-go-files>
go test ./pkg/controlproto ./pkg/nodeagent ./pkg/storage ./pkg/handlers ./pkg/panel ./pkg/server
go test ./...
npm --prefix pkg/webadmin/ui run build
bash scripts/test-panel-node-e2e.sh
```

For concurrency-sensitive recorder work, also run the focused package tests with `-race` if supported by the repository environment.

## Final Verification

- `go test -count=1 ./...` passed.
- `go test -race -count=1 ./pkg/controlproto ./pkg/handlers ./pkg/nodeagent ./pkg/panel` passed.
- `go vet ./...` and `go build ./...` passed.
- `npm --prefix pkg/webadmin/ui run build` passed.
- `bash scripts/test-panel-node-e2e.sh --mode local` passed, including exact bytes/object totals, stale API aggregation, stale ChromeDriver row, restart recovery, and final valid status.

## Review Gates

- Before code: confirm baseline choice and nullable/missing-field contract.
- After Node work: verify exact mutation matrix and no per-heartbeat filesystem walk.
- After Panel work: compare summary totals against per-node fixtures and existing health semantics.
- After UI work: verify Panel/standalone route isolation, old API compatibility, explicit missing/stale states, and responsive table/card layout.
- Before task activation/commit: run `trellis-check` quality gate and inspect `git diff` for unrelated changes.

## Risk / Rollback Points

- Baseline startup behavior is the principal operational risk; the implementation must be isolated behind a recorder/collector so the strategy can be changed without changing the wire/API contract.
- Recorder failures must invalidate telemetry, not fail S3 requests or publish misleading zeros.
- Panel migration is additive; if summary aggregation regresses, remove only telemetry fields and preserve existing health response fields.
