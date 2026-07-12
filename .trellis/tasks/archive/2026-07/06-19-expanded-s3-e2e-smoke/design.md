# Design

## Boundary

This task primarily changes test/smoke tooling. Product code changes are allowed only if
the smoke uncovers a real defect in already-claimed functionality.

## Script Shape

Extend `scripts/smoke-test.sh` or add a companion script under `scripts/` if keeping the
basic script compact is cleaner.

The script should accept:

- `EP` for AWS CLI endpoint option.
- `EP_HOST` for direct curl URL base.
- `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `AWS_DEFAULT_REGION`.
- `DATA_ROOT` for native file assertions.
- Optional webhook/admin variables only if the script needs to seed hook config through
  SQLite or an admin API.

## Flow

1. Create an isolated bucket/key prefix.
2. Upload object with metadata and verify `head-object` metadata.
3. Put/get/delete object tags.
4. Multipart upload a generated file large enough to force multipart, then verify final
   file bytes and that no user-visible chunked object exists.
5. Generate a presigned URL with AWS CLI and verify GET through `curl`.
6. Exercise webhook delivery. Preferred path: insert a hook config into the test database
   or expose an existing admin/API route if available; then run a local HTTP receiver and
   assert event payloads.

## Secrets

The script should use `set -euo pipefail` but avoid `set -x`.

## Rollback

Rollback is limited to smoke scripts unless a real product bug is found.
