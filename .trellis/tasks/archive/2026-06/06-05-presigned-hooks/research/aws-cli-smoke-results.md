# AWS CLI / Curl Smoke Results

Date: 2026-06-06

Environment:
- Temporary config pointed storage and SQLite DB at `/tmp/opencode`.
- Server started with seed credential `testaccess` / `testsecret`.
- Real `aws-cli` v2.34.62 and `curl` 8.5.0 were used.

Results:
- `go build ./...`: passed.
- `go vet ./...`: passed.
- `go test ./...`: passed.
- Core object regression with real aws-cli passed: `put`, `get`, byte compare, `head-object`, `list`, native file existence check, and `delete`.
- Presigned GET passed: `aws s3 presign` generated URL downloaded successfully with `curl`; expired URL returned HTTP 403 XML with `AccessDenied`; tampered signature returned HTTP 403 XML with `SignatureDoesNotMatch`.
- Presigned PUT passed: standard query SigV4 URL uploaded successfully with `curl -T`; native file landed under the bucket directory with matching bytes.
- Hook callbacks passed: enabled `hook_configs` row delivered `ObjectCreated` for PUT, `ObjectDeleted` for DELETE, and `ObjectCreated` for multipart complete to a local HTTP receiver.
- Hook receiver outage passed: upload completed successfully while receiver was down, and server log recorded hook delivery failure after retries.
- Disabled hook filtering passed: `enabled=false` hook config produced no callback.
- Auth and quota smoke passed: legal key succeeded; missing signature returned standard 403 XML; wrong secret returned `SignatureDoesNotMatch`; over-quota upload returned `QuotaExceeded` and the quota credential `used_bytes` stayed at `0`.

Notes:
- AWS CLI supports `aws s3 presign` for GET URLs. PUT presign verification was exercised with a standard query SigV4 URL and real `curl -T` upload because AWS CLI v2 has no generic PUT presign subcommand.
