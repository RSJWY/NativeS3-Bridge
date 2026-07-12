# Implementation Plan

## Steps

- [x] Read backend spec guidance before code edits.
- [x] Inspect current `pkg/db/db.go` logger and existing db tests.
- [x] Add a redaction helper or replace logged SQL with a redacted representation.
- [x] Add a focused regression test proving credential secret values do not appear in log output.
- [x] Run targeted tests: `go test ./pkg/db`.
- [x] Run broader checks: `go test ./...`, `go vet ./...`, `go build ./...`.
- [x] Re-run a local seed/start smoke enough to confirm startup logs no longer print `TESTSECRET`.

## Risky Files

- `pkg/db/db.go`
- `pkg/db/*_test.go`

## Rollback Point

If redaction is too brittle, omit SQL text entirely from GORM logs while keeping elapsed,
rows, and error attributes.
