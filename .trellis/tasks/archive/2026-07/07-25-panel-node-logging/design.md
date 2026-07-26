# Design: Panel/Node 日志拉取、查看与滚动完善

## 1. Scope and decisions

The shipped runtime is the Panel/Node pair. `cmd/natives3bridge` remains a
legacy compile/rollback compatibility target only; it does not receive a new
Panel-style UI or remote-log feature.

Panel local logs may read the active file and safe lumberjack history. Node
remote logs are ring-only in this phase. Node still writes and rotates its own
local file, but raw historical files never travel over the control channel.

The implementation keeps the existing mTLS WebSocket task channel and the
existing Vue3 embedded SPA. No new database table or arbitrary command channel
is introduced.

## 2. Data flow

```text
Panel/Node process
  slog
    ├─ stdout (always)
    ├─ lumberjack active file + rotated history (optional)
    └─ in-memory Ring (always)

Panel local view:
  Panel Ring/effective file
    → shared Logs handler
    → authenticated GET /api/admin/logs
    → shared /logs Vue view

Node remote view:
  Node Ring
    → LocalTaskRunner(log_query)
    → mTLS task_result (bounded structured entries)
    → Panel task persistence/typed task API
    → Node detail log component (polling)
```

The control channel is a bounded diagnostic path, not a file-transfer or live
streaming path. The node query has both entry and serialized-byte ceilings.

## 3. Logging and rotation boundary

### 3.1 Shared initialization

Move the duplicated setup behavior into one logging helper that accepts the
normalized log level and `LogConfig`/effective-file settings. It returns the
ring and the effective active file path needed by an admin viewer. The three
command packages keep thin wrappers only where existing tests or command
specific wiring require them.

The helper must:

- always include `os.Stdout`;
- derive `<dir>/natives3bridge.log` only through `LogConfig.EffectiveFile`;
- create the parent directory with the existing restrictive mode;
- use lumberjack `MaxSize`, `MaxBackups`, `MaxAge`, `Compress`, and local time;
- wrap the text handler with `logging.RingHandler`;
- fail before serving when an explicitly configured file cannot be created;
- preserve explicit `max_backups: 0` and the current no-startup-rotation behavior.

The ring remains the single in-process source for Node queries. File parsing and
history discovery remain in the shared webadmin log viewer.

### 3.2 Panel viewer reuse

Expose a small `webadmin` log-handler constructor (or equivalent viewer type)
that owns the existing `logs.go` behavior. `webadmin.Server` and
`panel.AdminServer` both mount that handler behind their existing session
middleware. The handler receives only a ring and an effective active file path;
it never accepts a directory or arbitrary client path.

Panel startup retains the ring returned by logging setup and passes it, plus
`cfg.Log.EffectiveFile()`, through `AdminServerDeps`.

The response stays backward-compatible with the current standalone contract:

```json
{
  "source": "ring|file",
  "file_enabled": true,
  "limit": 200,
  "entries": [],
  "warning": "...",
  "files": [],
  "selected_file": {}
}
```

Explicit history selection remains fail-closed (400/404/500 as appropriate),
while an unselected active-file read may use the existing ring fallback.

## 4. Node task contract

### 4.1 Wire additions

Extend the optional task fields without changing the protocol version:

- `TaskParams.Level` plus existing `Since`, `Until`, `Keyword`, `Limit`;
- `TaskResult.LogEntries` containing `time`, `level`, `msg`, and sanitized
  string attributes;
- keep `TaskResult.LogLines` for older nodes/panels;
- keep `LogTruncated`, and add a source marker such as `ring` if useful to the
  UI.

Unknown fields remain ignorable for older peers. A Panel receiving only
`log_lines` renders a compatibility text fallback; a new Node sends structured
entries.

### 4.2 Node query semantics

`LocalTaskRunner` delegates to a ring query that applies all filters before
limiting:

- RFC3339 `since`/`until`, inclusive at the requested boundaries;
- case-insensitive level;
- case-insensitive message/attribute substring keyword;
- default 200 entries, hard maximum 500;
- hard serialized-result ceiling (planned 256 KiB, below the WebSocket message
  ceiling), with deterministic newest-first truncation.

Invalid time syntax or impossible limits produce a failed task with a safe
operator-facing error. Sensitive attribute keys are filtered again at the wire
boundary, even though the ring handler already filters them on normal writes.

### 4.3 Task lifecycle

Keep the existing online-only dispatch, idempotent task IDs, in-flight window,
and disconnect-to-unknown behavior. Harden the generic lifecycle while exposing
logs:

- enforce the orchestrator timeout with a conditional terminal update;
- release the connection slot on timeout;
- ignore or preserve terminal state when a late result arrives;
- scope `GET /nodes/{id}/tasks/{taskID}` to the requested node;
- return a typed JSON task response instead of making the browser parse
  `ResultJSON`.

For log tasks, persist only a redacted parameter projection (for example,
omit the raw keyword) and the bounded sanitized result. Audit rows retain the
task type/state and node identity, never secrets or raw credentials.

## 5. Frontend boundary

### 5.1 Panel local logs

Allow `/logs` in the Panel runtime route matcher and add a Panel navigation
entry. Reuse `Logs.vue`, preserving the standalone route and file-selection
semantics. Copy should distinguish Panel runtime/control-plane logs from the
standalone S3 wording without branching API contracts.

### 5.2 Node logs

Add typed task/result interfaces and `adminApi` methods for dispatch and
node-scoped polling. Add a focused `PanelNodeLogsSection.vue` to
`PanelNodeDetail.vue`.

The component owns query controls and transient task state:

- level, keyword, optional since/until, and limit;
- dispatch → poll at a bounded interval until terminal/deadline;
- loading, empty, offline, timeout, failed, unknown, and truncated notices;
- structured rows with a text fallback for old nodes;
- cleanup of timers on unmount and no persistence in localStorage/global state.

Panel-only styles live in `src/panel.css`; shared log row styles remain shared.

## 6. Security and compatibility

- All human-facing log endpoints remain behind the existing admin session.
- No client-provided path is ever joined to a server filesystem path.
- Ring/file parsing continues to remove secret/password/authorization/cookie/
  signature/token attributes; GORM SQL literal redaction remains in force.
- Task payload/result sizes, keyword length, and log count are bounded.
- No registration token, S3 secret, private key, session cookie, full signed URL,
  or object body is added to logs, audit rows, task params, or test artifacts.
- Existing `log.file` configurations, old peers, standalone UI behavior, and
  legacy rollback behavior remain compatible.

## 7. Delivery order and rollback

1. Child `panel-log-viewer-rotation`: shared logging/viewer contract, Panel
   route/UI, and rotation tests.
2. Child `node-log-query`: protocol/runner/task API and Node detail UI. It may
   be developed in parallel, but integration uses the finalized shared logging
   contract.
3. Parent integration: real mTLS query, Panel local logs, redaction, build,
   release-contract, and E2E checks.

Rollback is code-only: revert the Panel route/handler and Node optional wire
fields; no schema downgrade is required. Existing log files remain readable by
the previous binaries.

## 8. Verification strategy

- Unit tests for shared setup, ring time filtering, rotation/pruning races,
  safe history selection, structured task results, redaction, timeout races,
  and node scoping.
- Panel admin-server tests for authenticated local logs and mode routing.
- Frontend type/build and browser checks for `/logs`, node log polling, error
  states, and no standalone API calls in Panel mode.
- Extend the existing Panel/Node E2E adapter with one deterministic safe Node
  log query and a Panel local-log assertion. Keep reports redacted.
