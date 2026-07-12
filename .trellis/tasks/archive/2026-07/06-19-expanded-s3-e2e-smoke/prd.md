# Expanded S3 end-to-end smoke coverage

## Goal

Extend runnable smoke coverage for release-critical S3 behavior beyond basic object CRUD.

## User Value

Maintainers can validate the core promise of NativeS3-Bridge before release: native file
mapping remains intact while multipart, metadata/tagging, presigned URLs, and webhook
delivery work through standard clients.

## Confirmed Facts

- `scripts/smoke-test.sh` currently covers bucket create, put/get/head/list/range/delete,
  and private anonymous GET rejection.
- Unit tests cover multipart, metadata sidecars, tagging, presigned verification, webhook
  retry, quota, and router behavior, but there is no single runnable e2e smoke for these
  release-critical flows.
- `aws` CLI is available in the current environment.
- Local service startup in the sandbox requires escalation for listening on loopback ports.

## Requirements

- Add a runnable smoke path that can be executed against an already-running local service.
- Keep the existing basic smoke behavior working.
- Cover:
  - multipart upload with final native single-file output and temporary part cleanup signal where practical;
  - `x-amz-meta-*` upload and `head-object` retrieval;
  - object tagging put/get/delete;
  - presigned URL GET and, where feasible with AWS CLI, PUT or clear documentation of any client limitation;
  - webhook delivery for object create/delete and a non-blocking failure/retry case where practical.
- The smoke script must use environment variables for endpoint, credentials, data root, and optional webhook/admin settings.
- The script must avoid printing full secret values, auth headers, cookies, or full presigned URLs in normal output.

## Acceptance Criteria

- [x] Existing basic smoke still passes.
- [x] Expanded smoke passes against a temporary local service with seeded credentials.
- [x] Smoke validates metadata and tagging via AWS CLI responses.
- [x] Smoke validates at least one presigned URL with a generic HTTP client.
- [x] Smoke validates webhook delivery for at least one create or delete event.
- [x] Failure output is actionable and does not dump secrets.
- [x] `shellcheck` is used if available; otherwise this limitation is reported.

## Verification

- `bash -n scripts/smoke-test-expanded.sh`
- `go test ./scripts/internal/smoke/...`
- Temporary local service run with seeded credential and preloaded hook configs:
  - `./scripts/smoke-test.sh`
  - `ENABLE_WEBHOOK_CHECK=1 ./scripts/smoke-test-expanded.sh`
- Rerun after final review: temporary loopback service on `127.0.0.1:19200/19201`
  passed both `./scripts/smoke-test.sh` and
  `ENABLE_WEBHOOK_CHECK=1 ./scripts/smoke-test-expanded.sh`; service log scan found
  no `TESTSECRET`, `X-Amz-Signature`, or `AWS_SECRET_ACCESS_KEY`.
- `go test ./...`
- `go vet ./...`
- `go build ./...`
- Checked service log from the e2e run for `TESTSECRET`, `X-Amz-Signature`, and `AWS_SECRET_ACCESS_KEY`; no matches.

Environment note: `shellcheck` is not installed in this environment, so shell validation used `bash -n`.

## Out of Scope

- Full AWS S3 conformance suite.
- External MySQL/PostgreSQL e2e coverage.
- Large production-scale performance testing.
