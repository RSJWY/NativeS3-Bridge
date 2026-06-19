# Harden database upgrades

## Goal

Make automatic startup migration safer so an application upgrade does not corrupt or
silently destroy existing database state.

## User Value

Operators can upgrade NativeS3-Bridge with confidence that the service either starts
with a valid migrated database or fails before serving traffic while preserving a
recoverable pre-upgrade database copy where the project can do that locally.

## Confirmed Facts

- NativeS3-Bridge supports `sqlite`, `mysql`, and `postgres` through GORM.
- `cmd/natives3bridge/main.go` opens the configured database and immediately calls
  `db.Migrate(gdb)` before wiring storage, auth, hooks, S3, or admin servers.
- `pkg/db.Migrate` currently calls `AutoMigrate(&Credential{}, &RequestStat{},
  &HookConfig{}, &Bucket{})` directly.
- The SQLite implementation uses `github.com/glebarez/sqlite` to keep CGO disabled.
- The Docker example stores SQLite state at `/state/natives3.db`; local examples use
  `./natives3.db`.
- Object bytes and object sidecar metadata are intentionally stored in the filesystem,
  not in the relational database.
- Existing tests only verify that migration creates expected tables in a new SQLite DB.

## Requirements

- Startup migration must not serve traffic if migration fails or if a post-migration
  health check detects a broken schema.
- Existing SQLite databases must be backed up before migration when the DSN points at
  a local file that already exists and contains data.
- SQLite backup creation must be durable enough for upgrade safety: write to a sibling
  backup file, preserve original file permissions when possible, fsync the backup, and
  avoid replacing or deleting the live database.
- SQLite integrity must be checked before migration and after migration using SQLite
  native checks exposed through the active DB connection.
- Migration must remain portable across SQLite, MySQL, and PostgreSQL. Model tags must
  not introduce driver-specific column types.
- The existing `db.Migrate(*gorm.DB) error` contract should remain usable by tests and
  helper binaries.
- MySQL and PostgreSQL upgrades must keep the current fail-fast behavior and add schema
  validation coverage, but automatic physical backups are out of scope because the
  process does not own those remote database files.
- MySQL and PostgreSQL must not copy tables on every startup. Operators should use
  database-native consistent backup/snapshot tooling before upgrading.
- Tests must cover existing-data migration, backup creation, failed SQLite migration
  behavior where feasible, and post-migration schema expectations.
- Documentation must tell operators where SQLite upgrade backups are written and what
  remote DB operators are expected to do before upgrading.

## Acceptance Criteria

- [x] Upgrading a non-empty SQLite database creates a timestamped sibling backup before
  any migration changes are attempted.
- [x] If SQLite preflight integrity fails, migration returns an error and does not run
  schema migration.
- [x] If schema migration or postflight validation fails, startup returns an error before
  S3 or admin servers start.
- [x] Existing rows in `credentials`, `request_stats`, `hook_configs`, and `buckets`
  remain readable after migration.
- [x] Migration verifies the expected tables and key indexes after `AutoMigrate`.
- [x] New SQLite databases still bootstrap without requiring a pre-existing file.
- [x] MySQL/PostgreSQL code paths still use the same model definitions and fail startup
  on migration or schema validation error.
- [x] README/config documentation describes SQLite automatic backup behavior and remote
  database backup responsibility.
- [x] `go test ./pkg/db`, `go test ./...`, `go vet ./...`, and `go build ./...` pass.

## Out of Scope

- Building logical backup/restore for MySQL or PostgreSQL.
- Copying MySQL/PostgreSQL tables during application startup.
- Encrypting the database or changing credential-at-rest behavior.
- Replacing GORM migrations with a full versioned migration framework.
- Changing S3 object storage layout or sidecar metadata format.
- Starting servers in degraded mode after migration failure.

## Decisions

- Implement automatic physical backups only for SQLite local database files.
- For MySQL/PostgreSQL, keep migration fail-fast and schema validation in application
  code, and document that consistent backups belong to database-native operator tooling.
