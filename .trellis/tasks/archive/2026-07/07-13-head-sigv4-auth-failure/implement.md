# Implement: Preserve HEAD Through S3 Reverse Proxy

## Checklist

1. [x] Reproduce production HeadObject and HeadBucket failures
2. [x] Verify current source succeeds with real AWS CLI locally
3. [x] Probe canonical method variants against production
4. [x] Identify Nginx HEAD-to-GET conversion as root cause
5. [x] Add Nginx cache-conversion warning and safe directives
6. [x] Add HEAD method-preservation SigV4 regression test
7. [x] Run full tests, vet, build, and update specs

## Validation

```bash
go test ./pkg/auth ./pkg/server -count=1
go test ./... -count=1
go vet ./...
go build ./...
```

Production after editing Nginx:

```bash
nginx -t
nginx -s reload
aws s3api head-bucket --endpoint-url https://s3-hk.rsjwy.top --region hk-1 --bucket test
aws s3api head-object --endpoint-url https://s3-hk.rsjwy.top --region hk-1 --bucket test --key '<existing-key>'
```

Expected logs must show `HEAD` in both Nginx access log and NativeS3 `s3 request` log.

## Risk

An inherited or later included Nginx config can override the location cache setting. Inspect generated includes if the upstream still logs GET.
