# Implementation Plan

## Steps

- [x] Read backend spec guidance before code edits.
- [x] Inspect existing smoke script, hook configuration model, and admin APIs.
- [x] Decide whether to extend `scripts/smoke-test.sh` or add `scripts/smoke-test-expanded.sh`.
- [x] Add metadata/tagging checks.
- [x] Add multipart upload and native-file assertions.
- [x] Add presigned URL curl check.
- [x] Add webhook delivery check if feasible without introducing brittle external dependencies.
- [x] Run the script against a temporary local service.
- [x] Run `go test ./...`, `go vet ./...`, and `go build ./...`.

## Risky Files

- `scripts/smoke-test.sh`
- Optional new script under `scripts/`
- Product code only if smoke finds a real bug.

## Rollback Point

If webhook e2e seeding is too brittle for shell, leave webhook at unit-test level and
document the limitation in the final report. Do not fake success.
