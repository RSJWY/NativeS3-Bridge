# Implementation Plan

## Steps

- [x] Read backend database guidelines before editing.
- [x] Add a configured migration wrapper in `pkg/db` while preserving
  `Migrate(*gorm.DB) error`.
- [x] Add SQLite DSN classification helpers for plain files, `file:` URLs, and in-memory
  DSNs.
- [x] Add durable SQLite backup creation with `VACUUM INTO` before migration for
  existing non-empty DB files.
- [x] Add SQLite `PRAGMA integrity_check` preflight and postflight checks.
- [x] Add post-migration schema validation for expected tables and important indexes.
- [x] Update `cmd/natives3bridge/main.go` to use the safer configured migration path.
- [x] Keep `scripts/internal/smoke/seed-hook-config/main.go` on the low-level migrator
  unless implementation reveals it is part of startup upgrade behavior; the helper is
  not the service upgrade path.
- [x] Add focused tests in `pkg/db`:
  - existing SQLite data survives migration and backup is created
  - new SQLite DB bootstraps without backup
  - in-memory SQLite skips file backup
  - schema validation catches missing expected tables or indexes where feasible
  - corrupt SQLite preflight fails before migration where feasible
- [x] Update README/config docs with SQLite backup behavior and MySQL/PostgreSQL operator
  backup responsibility.
- [x] Run validation: `go test ./pkg/db`, `go test ./...`, `go vet ./...`,
  `go build ./...`.

## Risky Files

- `pkg/db/migrate.go`
- `pkg/db/migrate_test.go`
- `cmd/natives3bridge/main.go`
- `scripts/internal/smoke/seed-hook-config/main.go`
- `README.md`

## Rollback Points

- If DSN parsing becomes ambiguous, limit automatic backup to plain filesystem paths and
  document that advanced SQLite URI DSNs require operator-managed backup.
- Do not add MySQL/PostgreSQL startup table copies; use documentation and fail-fast
  validation for remote databases.
- If portable index validation is unreliable across drivers, keep strict SQLite index
  tests and table validation for all drivers, then defer cross-driver index validation.
- If corruption test setup depends on SQLite driver internals, keep preflight behavior
  covered through a test seam and validate real corrupt-file behavior manually.
