# 节点遥测基础技术设计

## 1. Architecture and Data Flow

```text
native objects / successful S3 mutations
        -> node-level persistent telemetry counter
        -> heartbeat payload (optional fields + observed_at)
        -> Panel TransportServer heartbeat handler
        -> Panel NodeState latest snapshot
        -> /api/admin/dashboard/summary aggregation
        -> PanelDashboard.vue cards + node telemetry status
```

Panel 不访问 Node 数据盘，也不主动拉取扫描；Node 是遥测数据的唯一采集方。心跳读取计数器必须是一次轻量 DB 查询。

## 2. Wire Contract

Extend `HeartbeatPayload` additively:

```go
type HeartbeatPayload struct {
    AppliedVersion int64  `json:"applied_version"`
    UsedBytesTotal *int64 `json:"used_bytes_total,omitempty"`
    ObjectCount    *int64 `json:"object_count,omitempty"`
    ObservedAt     string  `json:"observed_at,omitempty"`
}
```

Pointers are required because JSON omission and a legitimate numeric zero have different meanings. `ObservedAt` is an RFC3339 UTC string; empty means no valid observation. Unknown fields remain ignored, so old Panel builds continue to decode newer heartbeats. New Panel builds accept old Node heartbeats with nil telemetry fields.

The node should only send all three telemetry fields together when the persisted counter is valid and has a non-zero observation timestamp. Partial telemetry is treated as unavailable at the Panel boundary.

## 3. Node Persistence and Accounting

Add one additive node-agent state model, for example `StorageTelemetry`:

- singleton primary key;
- `UsedBytesTotal int64`;
- `ObjectCount int64`;
- `ObservedAt time.Time`;
- `Valid bool`;
- optional `NeedsRebuild bool` / `UpdatedAt` for operational diagnosis.

Keep it in the node-agent state migration registry, not `pkg/db.Migrate`, preserving standalone compatibility.

The accounting owner is a node-level recorder used by managed S3 handlers. It updates the singleton transactionally after successful storage mutations:

- create PUT / Copy / Multipart Complete: `bytes += newSize`; `objects += 1` only if target did not exist;
- overwrite: `bytes += newSize - oldSize`; object count unchanged;
- delete / batch delete: `bytes -= oldSize`; `objects -= 1` only when target existed;
- zero-byte objects use existence, not size, to decide object count.

The recorder is independent of per-credential quota accounting. If its DB update fails after native storage has committed, mark telemetry invalid and log the failure; never turn a successful S3 request into a false telemetry value. A later baseline/reconcile repairs it.

The existing handlers already know target existence/size for quota reservations and delete paths. The design should add an optional recorder dependency to object/multipart handlers and wire it only from `cmd/node`; standalone constructors retain nil behavior.

The managed Node shares one recorder instance across S3 handlers, the heartbeat client, and the task runner. Object changes hold a shared mutation gate from the pre-write existence check through native commit and counter persistence; same-key operations also use a bounded shard lock. Counter read-modify-write transactions are serialized in-process for portable SQLite/MySQL/PostgreSQL behavior.

## 4. Baseline and Rebuild

Before the Node starts accepting S3 traffic, run one full storage-root scan using the same exclusions as `ReconcileBucket`, then persist a valid singleton snapshot. This gives upgraded nodes a correct starting point for pre-existing native files and avoids a concurrent scan/mutation race. The startup delay is an accepted trade-off for accurate initial telemetry.

The existing storage scan/reconcile task remains the explicit rebuild path. A failed or not-yet-run baseline sets `Valid=false`; the heartbeat omits all telemetry fields. No path may report zero as a fallback for invalid state.

No asynchronous baseline is used in the first version; avoiding a concurrent scan/mutation handoff keeps the initial contract deterministic.

Online rebuild takes the exclusive side of the shared mutation gate across scan and baseline persistence. Epoch-only detection is insufficient because a native file can be visible to the scan before the matching DB increment commits. Rebuild writes `.natives3-telemetry-invalid` before scanning and clears it only after the valid row is durable; the same marker forces repair after process restart if invalidation DB writes fail or the process exits mid-rebuild.

## 5. Panel Persistence and Migration

Extend `panel.NodeState` with nullable telemetry columns so legacy rows remain distinguishable from zero values:

- `UsedBytesTotal *int64` or a nullable DB integer;
- `ObjectCount *int64`;
- `TelemetryObservedAt *time.Time`;
- `TelemetryValid bool` (default false);
- optionally `TelemetryUpdatedAt *time.Time` for receive diagnostics.

`handleHeartbeat` decodes the optional payload, updates liveness/version on every heartbeat, and only updates telemetry columns when the payload contains a complete valid snapshot. A legacy heartbeat clears/marks telemetry unavailable for the current node observation rather than writing zeros or preserving a stale value as current. The latest prior snapshot may remain inspectable, but `TelemetryValid=false` prevents aggregation.

Use additive `AutoMigrate` and migration tests for an existing `node_states` table. No history table is introduced.

## 6. Summary API Contract

Extend the existing response, preserving current fields:

```json
{
  "telemetry": {
    "used_bytes_total": 1234,
    "object_count": 7,
    "valid_nodes": 1,
    "missing_nodes": 1,
    "stale_nodes": 1,
    "nodes": [
      {"node_id": 1, "used_bytes": 1234, "object_count": 7,
       "observed_at": "2026-08-15T12:00:00Z", "status": "valid"}
    ]
  }
}
```

The final field names may follow existing TypeScript naming, but the contract must distinguish `valid`, `missing`, and `stale`. Only valid snapshots whose `observed_at` is within `heartbeat_interval * offline_multiplier` contribute to totals. The threshold is injected into `AdminAPI` from `PanelConfig`; tests use a short custom interval. Retired nodes are excluded from health and telemetry totals consistently with the existing dashboard.

## 7. Frontend

Extend `PanelDashboardSummary` and render telemetry cards/rows from the single summary response. Add explicit formatting helpers:

- bytes use the existing dashboard's restrained units/formatting convention;
- `observed_at` uses a time placeholder when absent;
- `valid` shows the value, `missing` shows “未上报/不可用”, `stale` shows “已过期” and does not show the value as current.

Keep current manual refresh, loading, error retention, empty state, Panel/standalone route gating and typed API client. Do not add polling, charts or alerts.

## 8. Trade-offs / Risks

- One startup scan preserves correctness for pre-existing native files but can delay first service availability; this delay is an accepted operational trade-off.
- Incremental counters do not detect direct filesystem edits after baseline. Existing reconcile remains the explicit repair mechanism; invalidation must be visible rather than silently returning zero.
- Optional handler dependencies touch several constructors and tests; keep the recorder interface narrow and nil-safe.
- Nullable Panel fields are necessary to preserve old-node compatibility; plain `int64 default 0` would violate the “missing is not zero” requirement.

## 9. Rollback

The feature is additive: reverting Node recorder/heartbeat additions, Panel columns/aggregation, and UI fields restores the health dashboard. Existing control protocol version and old data-plane behavior remain compatible. A migration rollback is not required for additive columns, but old binaries must tolerate the extra columns and unknown heartbeat fields.
