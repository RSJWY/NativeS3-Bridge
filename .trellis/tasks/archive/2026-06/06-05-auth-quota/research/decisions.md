# Research Decisions

## SigV4 Verification

- Implemented header-based AWS Signature Version 4 verification directly in `pkg/auth`.
- The implementation follows the Amazon S3 canonical request format documented in AWS S3 API docs for single-chunk header authentication.
- Query-string presigned URL verification is intentionally deferred to S5, but canonical request, string-to-sign, and signing-key helpers are pure functions reusable by S5.
- Supported payload hash forms include the concrete `x-amz-content-sha256` value and `UNSIGNED-PAYLOAD` for aws-cli interoperability.

## SecretKey Storage

- First version stores `credentials.secret_key` in plaintext, matching the existing parent model and allowing direct SigV4 recomputation.
- Risk: database disclosure exposes S3 secrets. Future hardening can encrypt at rest if a stable key-management contract is added by planning.

## used_bytes Atomic Update

- `quota.Commit` uses one SQL expression in a GORM transaction:
  `CASE WHEN used_bytes + ? < 0 THEN 0 ELSE used_bytes + ? END`.
- This avoids driver-specific `GREATEST` differences and remains portable across SQLite, MySQL, and PostgreSQL.
- `request_stats` aggregation uses GORM `clause.OnConflict` on `(credential_id, day)` to hide dialect-specific upsert syntax.

## Soft Quota

- Quota enforcement follows the frozen S3 task design default: check before object write and commit usage afterward.
- This is a soft quota with a known Check/Commit TOCTOU window under concurrent uploads; atomic `Commit` prevents lost usage updates, but it does not reject all simultaneous overage races.
- A hard quota would require moving the quota check and used-byte reservation into a transaction before writing, which is outside this frozen task scope.

## Temporary Seed Credential

- Added temporary startup flags for local validation before the webadmin key CRUD task exists:
  `--seed-access-key`, `--seed-secret-key`, `--seed-quota-bytes`.
- The seed path upserts one enabled credential and is intentionally CLI-only testing scaffolding for this subtask.
