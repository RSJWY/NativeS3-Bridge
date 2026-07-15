# Database Guidelines

> Database patterns and conventions for this project.

---

## Overview

NativeS3-Bridge uses GORM with one shared model set across SQLite, MySQL, and PostgreSQL.

The database package contract is:

```go
func Open(driver, dsn string) (*gorm.DB, error)
func Migrate(db *gorm.DB) error
func MigrateConfigured(driver, dsn string, db *gorm.DB) error
```

Supported `driver` values are exactly `sqlite`, `mysql`, and `postgres`. The SQLite implementation uses the pure Go `github.com/glebarez/sqlite` driver to avoid CGO and preserve single-file cross-platform builds.

All schema definitions must remain portable across the three drivers. Do not use driver-specific column types such as PostgreSQL `jsonb` or MySQL-specific datetime precision tags.

---

## Query Patterns

Keep DB access behind `pkg/db` models and future service packages. Do not duplicate driver dispatch outside `pkg/db.Open`.

For per-credential usage and request statistics, use transactions when updating multiple counters. Capacity usage and `RequestStat` increments are expected to be updated atomically in later quota work.

---

## Migrations

Low-level migrations are managed by GORM `AutoMigrate` through `pkg/db.Migrate`.
`Migrate` must also validate the expected tables and key indexes after `AutoMigrate`:

```go
func Migrate(db *gorm.DB) error {
    if err := db.AutoMigrate(&Credential{}, &RequestStat{}, &HookConfig{}, &Bucket{}); err != nil {
        return err
    }
    return validateSchema(db)
}
```

Service startup must use `MigrateConfigured(driver, dsn, db)` instead of calling
`Migrate` directly. The configured wrapper owns driver-aware upgrade safety checks:

- SQLite: preflight `PRAGMA integrity_check`, pre-upgrade backup for existing local DB
  files with user tables, `Migrate`, postflight `PRAGMA integrity_check`.
- MySQL/PostgreSQL: no application-side table copying; use `Migrate` plus fail-fast
  schema validation. Operators are responsible for database-native consistent backups.

### Scenario: DB Foundation Schema

#### 1. Scope / Trigger

- Trigger: any change to `Credential`, `RequestStat`, `HookConfig`, `Bucket`, `Open`, `Migrate`, `MigrateConfigured`, or database dependencies.

#### 2. Signatures

- `Open(driver, dsn string) (*gorm.DB, error)`
- `Migrate(db *gorm.DB) error`
- `MigrateConfigured(driver, dsn string, db *gorm.DB) error`
- Models: `Credential`, `RequestStat`, `HookConfig`, `Bucket`

#### 3. Contracts

- `Credential.AccessKey` is unique, size-limited to 128, and required.
- `Credential.Status` defaults to `enabled` and is required.
- `Credential.QuotaBytes` and `Credential.UsedBytes` default to `0` and are required.
- `RequestStat` has a composite unique index on `(CredentialID, Day)` named `idx_cred_day`.
- `Bucket.Name` is unique, size-limited to 63, and required.
- Expected tables after migration: `credentials`, `request_stats`, `hook_configs`, `buckets`.
- Expected key indexes after migration: `idx_credentials_access_key`, `idx_cred_day`, `idx_buckets_name`.
- JSON-like data must be stored as plain `string` / TEXT columns unless the frozen task spec changes.

#### 4. Validation & Error Matrix

- Unsupported `driver` -> `Open` returns `unsupported db driver: "<driver>"`.
- Invalid DSN / unavailable server -> `gorm.Open` returns an error from the selected driver.
- Migration failure -> `Migrate` returns the GORM migration error to the caller.
- Missing expected table or index after migration -> `Migrate` returns a schema validation error.

#### 5. Good/Base/Bad Cases

- Good: `Open("sqlite", "./natives3.db")` with `github.com/glebarez/sqlite` and `Migrate` creates all expected tables and indexes.
- Base: MySQL/PostgreSQL paths use the same model definitions and differ only by GORM dialector.
- Bad: adding `gorm:"type:jsonb"`, `datetime(6)`, or any single-driver-only column tag.

#### 6. Tests Required

- Unit/integration test with SQLite temp DB that calls `Open` and `Migrate`.
- Assert `credentials`, `request_stats`, `hook_configs`, and `buckets` exist via `gdb.Migrator().HasTable`.
- Assert key indexes exist via `gdb.Migrator().HasIndex`.
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

### Scenario: Startup Upgrade Safety

#### 1. Scope / Trigger

- Trigger: service startup migration, SQLite file backup behavior, schema validation,
  or documentation for database upgrade procedures.

#### 2. Signatures

- `MigrateConfigured(driver, dsn string, db *gorm.DB) error`
- `Migrate(db *gorm.DB) error`

#### 3. Contracts

- `cmd/natives3bridge/main.go` must call `MigrateConfigured` after `Open` and before
  starting any S3 or admin server goroutine.
- `MigrateConfigured("sqlite", dsn, db)` must run `PRAGMA integrity_check` before and
  after `Migrate`.
- SQLite backup applies only to local filesystem DSNs that point to an existing DB file
  containing user tables. Skip `:memory:`, `file::memory:`, named `mode=memory` DSNs,
  missing files, empty files, and file URIs with non-local hosts.
- SQLite backup files are sibling files named like
  `<db>.pre-upgrade-YYYYMMDDTHHMMSSZ.bak`.
- SQLite backup must use `VACUUM INTO` through the active SQLite connection so WAL-backed
  databases produce a consistent standalone backup.
- MySQL/PostgreSQL startup must not copy tables. Application responsibility is
  fail-fast migration plus schema validation; consistent backups are operator-owned.

#### 4. Validation & Error Matrix

- Nil DB handle -> `database handle is nil`.
- Unsupported driver passed to `MigrateConfigured` -> `unsupported db driver: "<driver>"`.
- SQLite preflight integrity failure -> `sqlite preflight integrity check: ...`; no
  backup or schema migration should run.
- SQLite backup failure -> `sqlite pre-upgrade backup: ...`; schema migration should not run.
- GORM migration or schema validation failure -> `auto migrate: ...`.
- SQLite postflight integrity failure -> `sqlite postflight integrity check: ...`; startup
  must fail before serving traffic.

#### 5. Good/Base/Bad Cases

- Good: existing SQLite DB with credentials, stats, hooks, and buckets creates one
  sibling pre-upgrade backup and preserves all rows after migration.
- Base: new SQLite DB migrates without creating a backup.
- Base: MySQL/PostgreSQL use the same models and fail startup on migration/schema errors.
- Bad: raw-copying only the main SQLite file; WAL mode can leave committed data outside
  that file.
- Bad: copying MySQL/PostgreSQL tables during app startup; it is slow, lock-prone,
  storage-heavy, and may not be a consistent backup.

#### 6. Tests Required

- SQLite configured migration preserves existing rows and creates a readable backup.
- New SQLite DB and in-memory SQLite DSNs skip file backup.
- Corrupt SQLite DB fails preflight before migration.
- Schema validation detects missing expected indexes.
- Tests that serve requests from a temp-file SQLite database must close client
  connections and wait for background server goroutines to finish their final DB
  writes before `t.TempDir()` cleanup. In control-plane tests, wait until the Hub
  has unregistered the node after closing its WebSocket.
- Full regression: `go test ./pkg/db`, `go test ./...`, `go vet ./...`, `go build ./...`.

#### 7. Wrong vs Correct

Wrong:

```go
gdb, _ := db.Open(cfg.Database.Driver, cfg.Database.DSN)
_ = db.Migrate(gdb)
startServers()
```

Correct:

```go
gdb, _ := db.Open(cfg.Database.Driver, cfg.Database.DSN)
if err := db.MigrateConfigured(cfg.Database.Driver, cfg.Database.DSN, gdb); err != nil {
    return err
}
startServers()
```

### Go Module Compatibility

The module directive is pinned to `go 1.21` for this task tree. When tidying dependencies with a newer installed Go toolchain, use `go mod tidy -go=1.21` or verify the directive remains `go 1.21` afterward.

If `go mod tidy -go=1.21` tries to resolve newer transitive packages that require Go 1.22+, pin the compatible indirect version in `go.mod` and record the reason in the active task `research/decisions.md`. Current example: `github.com/rogpeppe/go-internal v1.12.0` is pinned to keep the dependency graph compatible with `go 1.21`.

---

## Naming Conventions

- Let GORM derive snake_case table and column names from model names and fields.
- Preserve the expected table names: `credentials`, `request_stats`, `hook_configs`,
  `buckets`.
- Preserve the `RequestStat` composite unique index name: `idx_cred_day`.

---

## Common Mistakes

- Do not use CGO SQLite unless the frozen task spec is changed; it conflicts with the single-file cross-platform deployment goal.
- Do not raw-copy SQLite database files for upgrade backup. Use `VACUUM INTO`; raw copy
  can miss data in WAL sidecar files.
- Do not add MySQL/PostgreSQL table-copy backups to application startup. Use native
  database backup/snapshot tooling outside the app.
- Do not trust the installed Go toolchain default when running `go mod tidy`; newer Go versions can rewrite the `go` directive.
- Do not rely only on the `sqlite3` CLI for migration verification. Some environments do not have it installed; keep a Go/GORM migrator test for table existence.
- Do not let `t.TempDir()` remove a SQLite directory while a background goroutine
  can still update the database. This can surface as `attempt to write a readonly
  database` or `TempDir RemoveAll cleanup: directory not empty`, especially on
  Go 1.21. Close the connection and wait on an observable shutdown barrier first.
