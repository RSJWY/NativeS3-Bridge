# Secret-safe database logging

## Goal

Prevent GORM SQL logging from emitting credential secrets while preserving useful query
diagnostics for development and operations.

## User Value

Operators can run the service, seed local credentials, and create credentials through the
admin API without leaking S3 secret keys into stdout, container logs, systemd journals, or
centralized logging systems.

## Confirmed Facts

- `pkg/db/db.go` currently calls `fc()` and logs the returned `sql` attribute at info,
  warn, and error levels.
- The default config log level is `info`.
- `cmd/natives3bridge/main.go` seeds credentials by writing `secret_key`.
- `pkg/webadmin/api.go` creates credentials by writing generated `SecretKey`.
- GORM's logger interface receives already-expanded SQL from `fc()`, so the safest local
  boundary is to avoid logging complete SQL text or to replace sensitive values before
  writing the log record.

## Requirements

- Database query logs must not contain credential secret values for insert, update, slow
  query, or error query paths.
- Logs should still include enough diagnostics to identify query timing, affected row
  count, and whether a query failed or was slow.
- The implementation must be generic enough to avoid leaking common secret-bearing columns
  such as `secret_key`, `session_secret`, `admin_bootstrap_password`, `metrics_token`,
  `password_hash`, `token`, and `password`.
- The fix must not change database behavior, migrations, credential generation, auth
  behavior, quota behavior, or public API response shapes.
- Add a focused test that would fail if a known secret value appears in GORM log output.

## Acceptance Criteria

- [x] A credential insert or upsert does not print the inserted secret value in logs.
- [x] Slow/error query logging paths also do not print raw sensitive values.
- [x] Query logs still include elapsed duration and rows.
- [x] Existing tests continue to pass.
- [x] `go test ./pkg/db ./pkg/webadmin ./cmd/natives3bridge` passes, followed by `go test ./...`.

## Verification

- `go test ./pkg/db`
- `go test ./pkg/db ./pkg/webadmin ./cmd/natives3bridge`
- `go test ./...`
- `go vet ./...`
- `go build ./...`
- Temporary local startup log check: seed credential logs did not contain `TESTSECRET` and did contain `[redacted]`.

## Out of Scope

- Encrypting credential secrets at rest.
- Changing credential API semantics.
- Removing all SQL/query observability.
