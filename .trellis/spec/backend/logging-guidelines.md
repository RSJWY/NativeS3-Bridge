# Logging Guidelines

> Structured logging and request-correlation contracts for this project.

---

## Scenario: S3 Request Logging And Correlation

### 1. Scope / Trigger


- Trigger: any change to `pkg/server/router.go` middleware ordering, S3 access logging, S3 error response helpers, or `x-amz-request-id` behavior.
- Goal: every S3 response has a service-generated request ID that appears in the response header, XML error body, and access log entry without logging secrets or request payloads.

### 2. Signatures


- `func Logging(next http.Handler) http.Handler`
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
- The access log must use `slog.Info("s3 request", ...)` and include `request_id`, `method`, `path`, and `elapsed`.
- The `request_id` log field must exactly match the `x-amz-request-id` response header for that request.
- S3 access logs may include method and path, but must not include Authorization headers, secret keys, session cookies, request bodies, object payloads, or presigned query signatures.

### 4. Validation & Error Matrix


- Downstream handler writes success -> response keeps generated `x-amz-request-id` and log includes matching `request_id`.
- Downstream handler writes S3 XML error -> response keeps generated `x-amz-request-id`, XML `<RequestId>` matches it, and log includes matching `request_id`.
- S3 XML error is written outside `Logging` in a unit test or helper context -> `WriteS3Error` may generate its own fallback request ID, but router-served requests must use the middleware-generated ID.
- Panic recovery writes S3 XML error -> recovered response should still use the already-set `x-amz-request-id` when `Logging` is outside the recovery path.

### 5. Good/Base/Bad Cases


- Good: a failed anonymous GET logs `request_id=req-...`, returns `x-amz-request-id: req-...`, and includes `<RequestId>req-...</RequestId>` in the XML body.
- Good: a successful PUT logs the same request ID that appears in the response header.
- Base: request IDs are generated locally and do not require database, storage, or external tracing infrastructure.
- Bad: generating a second request ID inside `WriteS3Error` when a header already exists, because logs and XML errors stop correlating.
- Bad: logging `Authorization`, `X-Amz-Signature`, request bodies, object contents, or admin session cookies.

### 6. Tests Required


- Middleware unit test proving `Logging` adds `x-amz-request-id` and logs `request_id`, `method`, `path`, and `elapsed`.
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
slog.Info("s3 request", "request_id", requestID, "method", r.Method, "path", r.URL.Path, "elapsed", time.Since(started))
```

---

## Common Mistakes

- Do not generate separate IDs for the log, response header, and XML error body; generate once in `Logging` and let S3 error helpers preserve the header.
- Do not log signed URL query strings with `r.URL.String()` on S3 requests; prefer `r.URL.Path` to avoid persisting signatures or other query parameters.
