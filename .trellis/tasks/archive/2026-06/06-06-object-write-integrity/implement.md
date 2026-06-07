# Implementation Plan: Object Write Integrity

## Pre-Start Review Gate

Do not run `python ./.trellis/scripts/task.py start 06-06-object-write-integrity` until the user reviews and approves `prd.md`, `design.md`, and this plan.

## Ordered Checklist

1. Inspect current dirty state and relevant diffs for `pkg/storage/file_backend.go`, `pkg/handlers/object.go`, `pkg/handlers/common.go`, `pkg/server/router.go`, `pkg/storage/integrity_test.go`, and `pkg/server/ops_test.go`.
2. Confirm existing uncommitted integrity code belongs to this task and does not conflict with unrelated user work.
3. In storage, keep the existing temp-file + `O_EXCL` + stream-copy + MD5 + `f.Sync()` + close + pre-rename digest gate + `os.Rename` flow.
4. Ensure every pre-rename failure branch removes the temp file: copy, sync, close, digest mismatch, and rename failure.
5. Decide directory fsync handling based on design guidance. Prefer minimal best-effort hardening if it can be done safely without changing published-object semantics; otherwise leave existing atomic visibility behavior and record the risk.
6. In handler PUT, parse optional `Content-MD5` as base64 raw 16-byte MD5 and return `InvalidDigest` for format errors before storage write.
7. Pass expected digest to storage only through an optional `PutObjectWithOptions` path; preserve fallbacks for backends without the optional method.
8. Map `storage.ErrBadDigest` to `BadDigest` HTTP 400 and ensure `errorMessage` has entries for `BadDigest` and `InvalidDigest`.
9. Add or complete storage tests for matching digest success and mismatched digest cleanup, including no target object and no `.tmp-` leftovers.
10. Add or complete handler/server tests for matching header success, mismatched header `BadDigest`, malformed header `InvalidDigest`, and no-header success.
11. Check copy/multipart paths are not accidentally forced through `Content-MD5` single PUT semantics.
12. Run formatting if Go files changed: `gofmt` on touched `.go` files.

## Validation Commands

Run before reporting implementation complete:

```bash
go test ./...
go build ./...
go test -race ./pkg/storage ./pkg/handlers ./pkg/server
```

If race tests are too slow or unavailable in the environment, record the exact failure or reason and still run the non-race package tests.

## Risky Files And Rollback Points

- `pkg/storage/file_backend.go`: core object write path. Mistakes can corrupt storage behavior; keep edits minimal and preserve existing tests.
- `pkg/handlers/object.go`: S3 PUT behavior and error mapping. Mistakes can break uploads, metadata, quota accounting, or events.
- `pkg/handlers/common.go`: shared S3 error messages. Avoid changing existing codes unrelated to this task.
- `pkg/server/router.go`: quota and dispatch are adjacent but should not need functional change for this task.
- Existing uncommitted files may include unrelated work. Do not revert or overwrite unrelated hunks.

## Completion Checklist

- PRD acceptance criteria are all satisfied.
- No new dependency is added.
- `storage.Backend` interface remains unchanged.
- Mismatched digest is rejected before final rename.
- Invalid digest header is rejected before any storage write.
- No leftover `.tmp-` files remain in failure tests.
- Build and tests pass or any environment-specific blockers are documented.
- No git commit is created by the agent.
