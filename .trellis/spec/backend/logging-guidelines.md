# Logging Guidelines

> Structured logging and request-correlation contracts for this project.

---

## Scenario: S3 Request Logging And Correlation

### 1. Scope / Trigger


- Trigger: any change to `pkg/server/router.go` middleware ordering, S3 access logging, S3 error response helpers, or `x-amz-request-id` behavior.
- Goal: every S3 response has a service-generated request ID that appears in the response header, XML error body, and access log entry without logging secrets or request payloads.

### 2. Signatures


- `func Logging(next http.Handler) http.Handler`
- `type statusResponseWriter` records the first response status, defaults to 200, and exposes `Unwrap() http.ResponseWriter`.
- `func logAuthDenied(w http.ResponseWriter, r *http.Request, reason, code string, attrs ...any)`.
- `func newS3RequestID(t time.Time) string`
- `func WriteS3Error(w http.ResponseWriter, code string, status int, resource string)` from `pkg/handlers/common.go`
- Response header: `x-amz-request-id`.
- XML error body field: `<RequestId>`.
- Access log message: `s3 request`.

### 3. Contracts


- The S3 router `Logging` middleware generates one request ID per request before calling downstream handlers.
- The request ID format is `req-<16 lowercase hex UTC unix-nanoseconds>-<8 lowercase hex sequence>`, for example `req-184f0f5d8d4a0000-00000001`.
- `Logging` sets `x-amz-request-id` on the response before downstream handlers run; downstream S3 error writers must preserve the existing header and copy the same value into `<RequestId>`.
- Successful S3 responses and XML error responses must both include `x-amz-request-id`.
- The access log must use `slog.Info("s3 request", ...)` and include `request_id`, `method`, `path`, `status`, and `elapsed`. The status is the first explicit `WriteHeader` or implicit 200 from `Write`/normal completion.
- The `request_id` log field must exactly match the `x-amz-request-id` response header for that request.
- Auth rejection logs use `slog.Warn("s3 auth denied", ...)` with the same `request_id`, `method`, and path plus stable `reason` and S3 `code`: `verify_failed`, `bucket_mismatch`, `credentials_required`, `acl_lookup_unavailable`, `acl_lookup_failed`, or `anonymous_not_allowed`.
- Bucket mismatches add `bound_bucket`/`request_bucket`; anonymous ACL failures add non-secret bucket/existence/ACL diagnostics. Never log access keys, Authorization, secret material, raw query strings, or signature values.
- S3 access logs may include method and path, but must not include Authorization headers, secret keys, session cookies, request bodies, object payloads, or presigned query signatures.

### 4. Validation & Error Matrix


- Downstream handler writes success -> response keeps generated `x-amz-request-id` and log includes matching `request_id`.
- Downstream handler writes S3 XML error -> response keeps generated `x-amz-request-id`, XML `<RequestId>` matches it, and log includes matching `request_id`.
- Signed missing-object HEAD -> 404 `NoSuchKey` and access-log `status=404`; anonymous/auth failures keep their existing 403/500 XML status and add the matching diagnostic reason/code.
- S3 XML error is written outside `Logging` in a unit test or helper context -> `WriteS3Error` may generate its own fallback request ID, but router-served requests must use the middleware-generated ID.
- Panic recovery writes S3 XML error -> recovered response should still use the already-set `x-amz-request-id` when `Logging` is outside the recovery path.

### 5. Good/Base/Bad Cases


- Good: a failed anonymous GET logs `request_id=req-...`, returns `x-amz-request-id: req-...`, and includes `<RequestId>req-...</RequestId>` in the XML body.
- Good: a successful PUT logs the same request ID that appears in the response header.
- Base: request IDs are generated locally and do not require database, storage, or external tracing infrastructure.
- Bad: generating a second request ID inside `WriteS3Error` when a header already exists, because logs and XML errors stop correlating.
- Bad: logging `Authorization`, `X-Amz-Signature`, request bodies, object contents, or admin session cookies.

### 6. Tests Required


- Middleware unit test proving `Logging` adds `x-amz-request-id` and logs `request_id`, `method`, `path`, `status`, and `elapsed`; cover explicit status, implicit 200, repeated `WriteHeader`, and `Write` before `WriteHeader`.
- Auth middleware tests for verify failure, missing credentials, private/missing anonymous bucket metadata, ACL lookup failure/unavailability, and bound-bucket mismatch; correlate both log entries with the response request ID and assert credential/signature sentinels are absent.
- Router test proving signed HEAD of a missing key remains 404 `NoSuchKey` and logs status 404.
- Router success test proving a successful S3 response includes a generated request ID.
- Router error test proving S3 XML `<RequestId>` equals the response `x-amz-request-id`.
- Run `gofmt`, `go build ./...`, `go vet ./...`, and `go test ./...` after logging changes.

### 7. Wrong vs Correct

Wrong:

```go
func WriteS3Error(w http.ResponseWriter, code string, status int, resource string) {
    requestID := newRequestID() // ignores header already set by Logging
    w.Header().Set("x-amz-request-id", requestID)
}
```

Correct:

```go
func WriteS3Error(w http.ResponseWriter, code string, status int, resource string) {
    requestID := ensureStandardHeaders(w)
    // XML RequestId uses requestID from the response header.
}
```

Wrong:

```go
slog.Info("s3 request", "authorization", r.Header.Get("Authorization"), "path", r.URL.String())
```

Correct:

```go
slog.Info("s3 request", "request_id", requestID, "method", r.Method, "path", r.URL.Path, "status", statusWriter.status, "elapsed", time.Since(started))
```

Wrong:

```go
slog.Warn("s3 auth denied", "authorization", r.Header.Get("Authorization"), "url", r.URL.String())
```

Correct:

```go
logAuthDenied(w, r, "verify_failed", auth.ErrorCode(err))
```

---

## Common Mistakes

- Do not generate separate IDs for the log, response header, and XML error body; generate once in `Logging` and let S3 error helpers preserve the header.
- Do not log signed URL query strings with `r.URL.String()` on S3 requests; prefer `r.URL.Path` to avoid persisting signatures or other query parameters.

## Scenario: Rotating File And Admin Ring Logging

### 1. Scope / Trigger
- Trigger: changes to `config.LogConfig`, `setupSlog`, `pkg/logging`, or the admin log viewer.

### 2. Signatures
- `setupSlog(level string, cfg config.LogConfig) (*logging.Ring, error)`.
- `config.LogConfig.EffectiveFile() string`; `log.dir` resolves to `<dir>/natives3bridge.log`, while legacy `log.file` remains a full path.
- `GET /api/admin/logs?limit=200&level=&q=&file=<enumerated-id>` under admin session middleware.

### 3. Contracts
- Stdout is always enabled; either non-empty `log.dir` or legacy `log.file` adds lumberjack rotation; the in-memory ring always stores the newest 2000 enabled records.
- `log.dir` and `log.file` are mutually exclusive. New deployments use `log.dir`; only `EffectiveFile` may derive the active path passed to `setupSlog` and webadmin.
- `log.max_size_mb` defaults to 100, `max_backups` to 5, `max_age_days` to 0, and `compress` to false. Explicit `max_backups: 0` is preserved.
- The API lists only the active basename and exact lumberjack timestamp backups (including `.gz`), rejects symlinks/non-regular files, and sorts current first then history newest-first.
- `file` is an enumerated basename, never a path. Explicit selection is revalidated against a fresh allowlist; malformed IDs return 400, missing/cleaned files return 404, and read/decompression failures return 500 without ring fallback.
- With no `file`, the API reads the active file and preserves the legacy ring fallback with `warning`. Responses add `files` and optional `selected_file` while preserving `source`, `file_enabled`, `limit`, `entries`, and `warning`.
- Gzip history is transparently decompressed before the same level/query/limit filters and newest-first projection are applied.
- Attr keys containing secret, password, authorization, cookie, signature, or token are excluded case-insensitively.

### 4. Validation & Error Matrix
- Both `log.dir` and `log.file` set -> config load error; enabled file logging with `max_size_mb < 1`, negative backups, or negative age -> config load error.
- Log directory/file cannot be created -> startup error.
- Missing admin session -> 401; non-GET -> 405; limit is clamped to 1..1000 with 200 default.
- Absolute/traversal/separator file ID -> 400; unmatched or removed ID -> 404; corrupt gzip -> 500.

### 5. Good/Base/Bad Cases
- Good: `/state/logs/natives3bridge.log` writes stdout + rotating file + ring.
- Good: selecting `natives3bridge-2026-07-12T10-00-00.000.log.gz` reads filtered history without exposing the directory.
- Base: both path fields empty keeps stdout + ring; legacy `log.file` keeps its basename-specific history.
- Bad: joining an untrusted `file` query directly to the log directory, following a matching symlink, silently falling back after an explicit historical selection, or returning secret attrs.

### 6. Tests Required
- Assert directory and legacy effective paths, mutual exclusion, explicit zero backups, invalid rotation values, active file writes, ring wrap/filter/concurrency, API 401, default file tail, and ring fallback.
- Assert exact lumberjack discovery/order, plain and gzip history filters, traversal/absolute/separator/unmatched/symlink rejection, removed history 404, and corrupt gzip 500 without fallback.
- `lumberjack.Logger` prunes files beyond `MaxBackups` in its background mill
  goroutine. Rotation-limit tests must poll with a bounded deadline until the
  expected backup count is visible; an immediate glob after `Write` is flaky.

### 7. Wrong vs Correct
- Wrong: `slog.NewTextHandler(file, ...)`, which removes stdout and ring.
- Wrong: `os.Open(filepath.Join(logDir, r.URL.Query().Get("file")))`, which turns the admin endpoint into an arbitrary file reader.
- Wrong: asserting `len(backups) == MaxBackups` immediately after a write that
  triggers rotation, before lumberjack's asynchronous mill has pruned old files.
- Correct: wrap a stdout/effective-file `io.MultiWriter` handler with `logging.NewRingHandler`; enumerate exact regular backup basenames and open only the matched server-side record.
- Correct: poll the backup glob with a short bounded deadline and assert the
  final count after asynchronous pruning completes.
