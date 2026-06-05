# Database Guidelines

> Database patterns and conventions for this project.

---

## Overview

NativeS3-Bridge uses GORM with one shared model set across SQLite, MySQL, and PostgreSQL.

The database package contract is:

```go
func Open(driver, dsn string) (*gorm.DB, error)
func Migrate(db *gorm.DB) error
```

Supported `driver` values are exactly `sqlite`, `mysql`, and `postgres`. The SQLite implementation uses the pure Go `github.com/glebarez/sqlite` driver to avoid CGO and preserve single-file cross-platform builds.

All schema definitions must remain portable across the three drivers. Do not use driver-specific column types such as PostgreSQL `jsonb` or MySQL-specific datetime precision tags.

---

## Query Patterns

Keep DB access behind `pkg/db` models and future service packages. Do not duplicate driver dispatch outside `pkg/db.Open`.

For per-credential usage and request statistics, use transactions when updating multiple counters. Capacity usage and `RequestStat` increments are expected to be updated atomically in later quota work.

---

## Migrations

Migrations are managed by GORM `AutoMigrate` through `pkg/db.Migrate`:

```go
func Migrate(db *gorm.DB) error {
    return db.AutoMigrate(&Credential{}, &RequestStat{}, &HookConfig{})
}
```

### Scenario: DB Foundation Schema

#### 1. Scope / Trigger

- Trigger: any change to `Credential`, `RequestStat`, `HookConfig`, `Open`, `Migrate`, or database dependencies.

#### 2. Signatures

- `Open(driver, dsn string) (*gorm.DB, error)`
- `Migrate(db *gorm.DB) error`
- Models: `Credential`, `RequestStat`, `HookConfig`

#### 3. Contracts

- `Credential.AccessKey` is unique, size-limited to 128, and required.
- `Credential.Status` defaults to `enabled` and is required.
- `Credential.QuotaBytes` and `Credential.UsedBytes` default to `0` and are required.
- `RequestStat` has a composite unique index on `(CredentialID, Day)` named `idx_cred_day`.
- JSON-like data must be stored as plain `string` / TEXT columns unless the frozen task spec changes.

#### 4. Validation & Error Matrix

- Unsupported `driver` -> `Open` returns `unsupported db driver: "<driver>"`.
- Invalid DSN / unavailable server -> `gorm.Open` returns an error from the selected driver.
- Migration failure -> `Migrate` returns the GORM migration error to the caller.

#### 5. Good/Base/Bad Cases

- Good: `Open("sqlite", "./natives3.db")` with `github.com/glebarez/sqlite` and `Migrate` creates all three tables.
- Base: MySQL/PostgreSQL paths use the same model definitions and differ only by GORM dialector.
- Bad: adding `gorm:"type:jsonb"`, `datetime(6)`, or any single-driver-only column tag.

#### 6. Tests Required

- Unit/integration test with SQLite temp DB that calls `Open` and `Migrate`.
- Assert `credentials`, `request_stats`, and `hook_configs` exist via `gdb.Migrator().HasTable`.
- For config validation, assert missing `storage.data_root` or invalid `database.driver` returns a clear error.
- Run `go test ./...`, `go build ./...`, and `go vet ./...` after DB changes.

#### 7. Wrong vs Correct

Wrong:

```go
type RequestStat struct {
    Payload string `gorm:"type:jsonb"`
}
```

Correct:

```go
type RequestStat struct {
    Day string `gorm:"size:10;index;uniqueIndex:idx_cred_day;not null"`
}
```

### Go Module Compatibility

The module directive is pinned to `go 1.21` for this task tree. When tidying dependencies with a newer installed Go toolchain, use `go mod tidy -go=1.21` or verify the directive remains `go 1.21` afterward.

If `go mod tidy -go=1.21` tries to resolve newer transitive packages that require Go 1.22+, pin the compatible indirect version in `go.mod` and record the reason in the active task `research/decisions.md`. Current example: `github.com/rogpeppe/go-internal v1.12.0` is pinned to keep the dependency graph compatible with `go 1.21`.

---

## Naming Conventions

- Let GORM derive snake_case table and column names from model names and fields.
- Preserve the expected table names: `credentials`, `request_stats`, `hook_configs`.
- Preserve the `RequestStat` composite unique index name: `idx_cred_day`.

---

## Common Mistakes

- Do not use CGO SQLite unless the frozen task spec is changed; it conflicts with the single-file cross-platform deployment goal.
- Do not trust the installed Go toolchain default when running `go mod tidy`; newer Go versions can rewrite the `go` directive.
- Do not rely only on the `sqlite3` CLI for migration verification. Some environments do not have it installed; keep a Go/GORM migrator test for table existence.
