# Design: Panel local log viewer and rotation

## Boundary

This child owns the process-local logging contract and Panel's own viewer. It
does not change the Node remote task protocol.

## Shared setup

Create one reusable setup implementation for level parsing, stdout, effective
file creation, lumberjack options, and ring wrapping. Panel, Node, and legacy
standalone use that implementation so rotation fixes cannot drift between
entry points. Existing command helpers may remain as thin delegates for tests.

The returned runtime data is the ring plus the effective active file path.
Panel retains both and passes them to its admin server. Node continues passing
the ring to `LocalTaskRunner`.

## Shared viewer

Refactor the existing `webadmin.API.Logs` ownership into an exported handler or
viewer constructor while preserving its response and standalone route. The
viewer owns only ring/file access; it has no credential/bucket/database
dependency.

`panel.AdminServer` mounts the viewer at `/api/admin/logs` behind the reused
auth middleware. The current file/history/gzip allowlist and explicit-selection
error behavior remain unchanged.

## Frontend

The shared `/logs` route is valid in both service modes. Panel navigation labels
it `Panel 日志`; standalone keeps its current label/behavior. `Logs.vue` uses
runtime mode only for copy, not to invent a separate endpoint or payload.

## Compatibility and rollback

There is no schema change. Legacy `log.file`, new `log.dir`, and the standalone
viewer stay compatible. Reverting the handler injection removes the Panel page
without touching log files.

## Verification

Cover shared setup, invalid paths, explicit zero backups, no startup rotation,
asynchronous pruning, gzip/history selection, Panel auth, runtime routes, and
both frontend modes.
