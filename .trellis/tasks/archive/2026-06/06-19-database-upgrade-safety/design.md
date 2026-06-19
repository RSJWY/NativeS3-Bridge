# Design

## Boundary

The change is constrained to database opening/migration safety, migration tests, and
operator documentation. GORM remains the ORM. Existing model names, table names, and
portable field tags remain the compatibility boundary.

`db.Migrate(*gorm.DB) error` remains available for existing unit tests and helper
programs. Startup code can move to a richer helper that knows the configured driver and
DSN, because SQLite backup decisions require file-path information that is not available
from `*gorm.DB` alone.

## Approach

Add a migration wrapper in `pkg/db` with a shape such as:

```go
func MigrateConfigured(driver, dsn string, gdb *gorm.DB) error
```

The wrapper will:

- Validate the open database before migration.
- Create a SQLite backup when `driver == "sqlite"` and the DSN resolves to an existing
  local database file with non-zero size.
- Run the existing `Migrate(gdb)` implementation.
- Validate expected schema after migration.
- Return clear wrapped errors so startup logs identify preflight, backup, migration, or
  postflight failure.

`cmd/natives3bridge/main.go` should call the wrapper after `db.Open` and before any
server goroutine starts. `scripts/internal/smoke/seed-hook-config/main.go` should either
use the same wrapper or intentionally stay with `Migrate` only if tests prove that is the
right helper behavior for smoke setup.

## SQLite Backup

SQLite backups are file backups of the configured database file. The implementation
should keep this conservative:

- Only back up DSNs that are plain local file paths or `file:` URLs that map to local
  files.
- Skip backup for in-memory SQLite DSNs such as `:memory:` or `file::memory:`.
- If the database file does not exist or is empty, create no backup and continue.
- Write the backup beside the database with a timestamped suffix, for example
  `natives3.db.pre-upgrade-20260619T103000Z.bak`.
- Use SQLite's native `VACUUM INTO` through the already-open connection so the backup is
  a consistent database image even when SQLite has WAL sidecar files.
- Preserve source mode when possible and fsync the destination backup file after SQLite
  finishes writing it.
- Do not rename, truncate, replace, or delete the live database.

This is not a full transactional restore system. It is a durable pre-upgrade recovery
point before `AutoMigrate` touches the database. `VACUUM INTO` is preferred over raw
byte copying because raw copying can miss committed data still present in WAL files.

## SQLite Integrity

For SQLite, run a native integrity check through GORM before and after migration:

```sql
PRAGMA integrity_check
```

The expected result is exactly `ok`. Any other result fails migration. The preflight
check prevents touching a database that is already corrupt. The postflight check catches
obvious damage before the service starts.

## Schema Validation

After migration, validate the expected table set:

- `credentials`
- `request_stats`
- `hook_configs`
- `buckets`

Also validate important indexes where GORM exposes them portably enough:

- `credentials.access_key` unique index
- `request_stats` composite unique index `idx_cred_day`
- `buckets.name` unique index

If portable index validation proves flaky across drivers, the minimum acceptable
fallback is table validation plus SQLite-specific index validation in tests, with the
tradeoff recorded before implementation starts.

## Compatibility

The service will continue to support `sqlite`, `mysql`, and `postgres`. SQLite gets the
automatic file backup because the process owns a local file path. MySQL and PostgreSQL
keep fail-fast migration semantics and schema validation; operators remain responsible
for database-native backup/snapshot processes before upgrading.

The application will not copy MySQL/PostgreSQL tables during startup. Table copying can
be slow, lock-prone, storage-expensive, and not necessarily transactionally consistent.
Reliable remote database backups belong to native tools such as consistent dumps,
managed snapshots, physical backups, or PITR.

No public API, config key, model field, object storage, or admin UI change is expected.

## Rollback

Rollback is limited to `pkg/db/migrate.go`, new migration helper tests, startup call
sites, and documentation. If a regression appears, startup can temporarily revert to
calling `db.Migrate(gdb)` while preserving the tests and design notes for a follow-up
fix.
