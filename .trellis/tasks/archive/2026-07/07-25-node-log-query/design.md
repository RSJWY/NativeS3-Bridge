# Design: Node remote log query

## Boundary

This child owns the bounded Node ring query, its backward-compatible control
payload, the typed Panel task API, and the Node-detail UI. It does not transfer
Node log files or rotation history.

## Protocol

Add optional structured fields to the existing task payload/result. Keep the
protocol version and legacy `log_lines`; new peers prefer `log_entries`.

The Node applies RFC3339 time bounds, level, keyword, and limit before result
projection. Results are newest-first and capped at 500 entries and a 256 KiB
serialized budget. Reaching either cap sets `log_truncated`.

## Task lifecycle and persistence

The Panel keeps online-only dispatch and idempotency. Add timeout enforcement
with conditional state changes and connection-slot release. A late result must
not overwrite an already terminal timeout/disconnect result.

Return a typed response decoded from `ResultJSON` and scope task lookup by both
node ID and task ID. Persist a redacted log-query parameter projection (omit
the raw keyword) and only bounded sanitized results.

## UI

`PanelNodeLogsSection.vue` dispatches through the shared API client and polls a
node-scoped task endpoint. It renders structured entries, falls back to legacy
text lines, and exposes offline/timeout/failed/unknown/empty/truncated states.
Polling and timers are component-local and cleaned up on unmount.

## Dependency and compatibility

This child may use the current ring setup while the Panel viewer child is in
progress. Before parent integration, update constructor/wiring if the shared
setup child changes signatures. Additive payload fields keep old peers working.

## Verification

Cover protocol round trips, ring filters, byte/count caps, sensitive attrs,
invalid ranges, offline dispatch, timeout/late-result races, node-scoped task
lookup, frontend build, and a real mTLS log query.
