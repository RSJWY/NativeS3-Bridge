# AWS CLI Smoke Verification Results

Date: 2026-06-06

Temporary environment:
- Binary: `/tmp/opencode/natives3-multipart-smoke.wXlfY7/natives3bridge`
- Config: `/tmp/opencode/natives3-multipart-smoke.wXlfY7/config.yaml`
- Endpoint: `http://127.0.0.1:19000`
- Data root: `/tmp/opencode/natives3-multipart-smoke.wXlfY7/data`
- Database: `/tmp/opencode/natives3-multipart-smoke.wXlfY7/natives3.db`
- AWS CLI: `aws-cli/2.34.62 Python/3.14.5 Linux/6.6.87.2-microsoft-standard-WSL2 exe/x86_64.ubuntu.24`

## Go verification

- `go build ./...` passed.
- `go vet ./...` passed.
- `go test ./...` passed.

## Core object regression

Using real `aws-cli` with signed requests:
- `aws s3 cp <local> s3://test-bucket/dir/core.bin` passed.
- `aws s3 cp s3://test-bucket/dir/core.bin <local>` passed; `cmp` verified byte equality.
- `aws s3api head-object --bucket test-bucket --key dir/core.bin` returned size `65536`, MD5 ETag, content type `application/octet-stream`.
- `aws s3 ls s3://test-bucket/dir/` listed `core.bin` only.
- `aws s3 rm s3://test-bucket/dir/core.bin` removed the object, and the native disk file was gone.

## Multipart upload

Using real `aws-cli` high-level upload:
- Generated a 120 MiB file with `dd`.
- `aws s3 cp big.bin s3://test-bucket/big.bin` completed successfully and triggered multipart upload.
- Final disk path `data/test-bucket/big.bin` exists as a single native file.
- Downloaded object matched the original via `cmp`.
- `head-object` returned `ContentLength: 125829120` and multipart ETag `"b91e8b6fe1cb6d38020aee966c96e679-15"`.
- `data/.multipart` existed and was empty after completion.

Manual multipart checks:
- `create-multipart-upload` + `upload-part` created `data/.multipart/{uploadID}/part-00001`.
- `list-parts` returned part number `1`.
- `abort-multipart-upload` removed the matching upload directory when run before TTL expiry.

GC check:
- With smoke config `multipart_gc_interval: 1s` and `multipart_ttl: 2s`, created an upload and waited 4 seconds.
- The expired `data/.multipart/{uploadID}` directory was removed by the background GC.

## Metadata and tagging

Metadata:
- `aws s3api put-object --metadata author=jdoe,team=infra --content-type text/plain` passed.
- `head-object` returned metadata keys `author: jdoe` and `team: infra`.
- `aws s3 cp` downloaded the object and `cmp` verified body bytes.

Tagging:
- `put-object-tagging` with `env=prod` and `owner=qa` passed.
- `get-object-tagging` returned both tags.
- `delete-object-tagging` passed and subsequent `get-object-tagging` returned an empty `TagSet`.
- Overwriting the object with `put-object` cleared previous tags; subsequent `get-object-tagging` returned an empty `TagSet`.

Sidecar/listing fallback:
- `aws s3 ls s3://test-bucket/` did not list `.s3meta` files.
- A file manually placed at `data/test-bucket/external.txt` without sidecar downloaded successfully through `aws s3 cp`, with byte equality verified.

## Auth and quota

Credentials:
- Unlimited key: `smoke-access` / `smoke-secret`.
- Quota key: `quota-access` / `quota-secret`, `quota_bytes = 20`.

Auth checks:
- Legal signed key completed object PUT.
- Wrong secret on `aws s3api get-object` returned `SignatureDoesNotMatch` from real `aws-cli`.
- Unsigned direct GET returned HTTP `403` with XML body containing `<Error>` and `<Code>AccessDenied</Code>`.

Quota checks:
- Uploaded a 10-byte object with the 20-byte quota key; DB query showed `quota_bytes=20 used_bytes=10`.
- Uploading an 11-byte second object failed with `QuotaExceeded`.
- The rejected object was not present on disk.
- DB query after the rejected upload still showed `quota_bytes=20 used_bytes=10`.
