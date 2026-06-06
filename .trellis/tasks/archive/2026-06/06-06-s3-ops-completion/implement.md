# Implementation Plan

## Ordered Checklist

- [x] Add storage-level CopyObject support in `pkg/storage` and unit tests covering byte equality, ETag, metadata, tags, missing bucket/key, and overwrite safety.
- [x] Add DeleteObjects XML request/response types and handler logic in `pkg/handlers/object.go`.
- [x] Add CopyObject XML response, copy-source parsing, quota check, usage commit, and hook emission in `pkg/handlers/object.go`.
- [x] Add bucket probe handlers for `?location` and `?versioning` in `pkg/handlers/bucket.go`.
- [x] Update `pkg/server/router.go` dispatch order for `?delete`, `?location`, `?versioning`, and `x-amz-copy-source` without changing existing multipart/tagging priority.
- [x] Add handler/router tests for route selection and XML behavior where practical.
- [x] Run unit test suite.
- [x] Run isolated aws-cli smoke checks for `delete-objects`, `copy-object`, `get-bucket-location`, and `get-bucket-versioning`.

## Validation Commands

- `go test ./...`
- Isolated aws-cli smoke after starting a temporary server with seeded credentials:

```bash
export AWS_ACCESS_KEY_ID=TESTKEY
export AWS_SECRET_ACCESS_KEY=TESTSECRET
export AWS_DEFAULT_REGION=us-east-1
EP='--endpoint-url http://127.0.0.1:9100'
aws $EP s3api create-bucket --bucket ops-bucket
aws $EP s3api put-object --bucket ops-bucket --key source.txt --body /tmp/source.txt --metadata author=alice
aws $EP s3api put-object-tagging --bucket ops-bucket --key source.txt --tagging 'TagSet=[{Key=env,Value=test}]'
aws $EP s3api copy-object --bucket ops-bucket --key copy.txt --copy-source ops-bucket/source.txt
aws $EP s3api get-object --bucket ops-bucket --key copy.txt /tmp/copy.txt
aws $EP s3api get-object-tagging --bucket ops-bucket --key copy.txt
aws $EP s3api delete-objects --bucket ops-bucket --delete 'Objects=[{Key=source.txt},{Key=missing.txt}],Quiet=false'
aws $EP s3api get-bucket-location --bucket ops-bucket
aws $EP s3api get-bucket-versioning --bucket ops-bucket
```

## Risky Files And Rollback Points

- `pkg/server/router.go`: route ordering can accidentally shadow multipart/tagging/list behavior. Roll back route changes if existing smoke tests fail.
- `pkg/handlers/object.go`: usage accounting and hook emission must match existing single-object operations. Roll back new handlers if quota tests fail.
- `pkg/storage/file_backend.go`: CopyObject must not corrupt destination on source read failure. Roll back storage method if native byte equality or sidecar preservation tests fail.

## Pre-Start Review Gate

- Confirm `prd.md`, `design.md`, and this `implement.md` match desired scope.
- Confirm CopyObject should preserve source metadata/tags by default and advanced copy directives remain out of scope.
- After approval, run `python ./.trellis/scripts/task.py start .trellis/tasks/06-06-s3-ops-completion` and proceed to implementation.
