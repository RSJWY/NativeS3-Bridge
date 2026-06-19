# Design

## Boundary

The change is constrained to database logging. GORM remains the ORM, existing models and
migrations remain unchanged, and all credential write paths keep their current behavior.

## Approach

`slogGORMLogger.Trace` will stop writing fully expanded SQL text. Instead it will write a
sanitized query string and fixed attributes:

- `elapsed`
- `rows`
- optionally `sql`, with sensitive values redacted
- `error` for failed query logs

The preferred implementation is defensive redaction before logging. Redaction targets SQL
assignments and insert/update value positions for sensitive column names. If preserving
SQL shape becomes brittle, the fallback is to omit `sql` entirely and log a stable
placeholder such as `"[redacted]"`.

## Sensitive Fields

At minimum, redact values associated with:

- `secret_key`
- `session_secret`
- `admin_bootstrap_password`
- `metrics_token`
- `password_hash`
- `password`
- `token`
- `captcha_token`

## Tests

Add tests in `pkg/db` using a temporary SQLite database and an in-memory slog handler.
The test should insert or upsert a `db.Credential` with a distinctive secret value and
assert the log output does not contain that value.

## Compatibility

No database schema, config, API, CLI, or UI compatibility changes are expected.

## Rollback

Rollback is limited to `pkg/db/db.go` and related tests.
