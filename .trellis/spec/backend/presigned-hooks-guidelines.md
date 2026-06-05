# Presigned Hooks Guidelines

> Query SigV4 presigned URL verification and asynchronous webhook event contracts.

---

## Scenario: Presigned URLs And Webhook Hooks

### 1. Scope / Trigger

- Trigger: any change to `pkg/auth` query SigV4 verification, `pkg/handlers/presigned.go`, `pkg/hooks`, hook config loading, S3 object/multipart event emission, or hook-related config keys.
- Goal: allow ordinary HTTP clients to use valid query-presigned GET/PUT URLs while keeping PUT quota enforcement and emitting non-blocking webhook events after successful object creation/deletion.

### 2. Signatures

- `func HasPresignQuery(r *http.Request) bool`
- `func (a *LocalSigV4Authenticator) Verify(r *http.Request) (*Identity, error)` branches to query presign when all required `X-Amz-*` query fields are present.
- `func CanonicalRequestWithQuery(r *http.Request, signedHeaders []string, payloadHash string, query url.Values) (string, error)`
- `func GeneratePresignedURL(cred db.Credential, method, endpoint, bucket, key, region string, expires time.Duration) (string, error)`
- `type Event struct { Type EventType; Bucket string; Key string; Size int64; ETag string; Metadata map[string]string; CredentialID uint; Timestamp string }`
- `func NewManager(gdb *gorm.DB, cfg hooks.Config) *Manager`
- `func (m *Manager) Start()` / `Emit(Event)` / `Reload() error` / `Stop()`
- Config keys: `hooks.queue_size`, `hooks.workers`, `hooks.max_retry`, `hooks.timeout`.

### 3. Contracts

- Presigned URLs must include `X-Amz-Algorithm=AWS4-HMAC-SHA256`, `X-Amz-Credential`, `X-Amz-Date`, `X-Amz-Expires`, `X-Amz-SignedHeaders`, and `X-Amz-Signature`.
- Presigned canonical query must exclude `X-Amz-Signature` and include all other query parameters in AWS canonical order.
- Presigned payload hash is always `UNSIGNED-PAYLOAD`; do not read or hash the request body while authenticating query-presigned PUT.
- A valid presigned request returns the same `auth.Identity` shape as header SigV4, so existing object handlers and quota middleware must be reused.
- Hook events are JSON POSTs. `Timestamp` is RFC3339 UTC and is set before enqueueing when missing.
- `Emit` must not block S3 request handling. Queue full means drop the event and log a warning, not delay the client response.
- `Reload` reads only enabled `hook_configs` rows and rebuilds the in-memory hook list.
- Webhook delivery treats non-2xx status codes as failures, logs response status, and retries with exponential backoff.

### 4. Validation & Error Matrix

- Missing presign query fields with no header auth -> header auth path -> HTTP `403 AccessDenied` XML.
- Malformed presign credential scope, service, region, date, expires, or signed headers -> HTTP `403 SignatureDoesNotMatch` XML.
- Expired presigned URL -> HTTP `403 AccessDenied` XML.
- Tampered path, query, or signature -> HTTP `403 SignatureDoesNotMatch` XML.
- Presigned PUT over quota -> HTTP `403 QuotaExceeded` XML before writing native bytes.
- Disabled hook config -> no webhook delivery.
- Webhook timeout, connection failure, or non-2xx response -> retry up to `max_retry`; after exhaustion log warning and keep original S3 operation successful.
- Hook queue full -> log warning and drop event.

### 5. Good/Base/Bad Cases

- Good: `aws s3 presign s3://bucket/key` produces a GET URL; `curl` downloads within expiry and receives standard S3 `403` XML after expiry.
- Good: query-presigned PUT uploads native bytes and then emits `ObjectCreated` with bucket, key, size, ETag, metadata, credential ID, and timestamp.
- Good: completing multipart upload emits one `ObjectCreated` event for the final native object, not per part.
- Base: deleting an existing object emits `ObjectDeleted`; deleting a non-existing key remains S3-idempotent and should not invent object metadata.
- Bad: duplicating SigV4 canonicalization for presigned URLs instead of reusing shared canonical request and signing key helpers.
- Bad: synchronously POSTing webhook callbacks from the S3 handler goroutine.

### 6. Tests Required

- Unit test presigned verification for success, expired URL, tampered path/signature, and an AWS CLI generated fixture.
- Unit test hook `Reload` filters `enabled=false` rows.
- Unit test hook retry performs initial delivery plus configured retries and reports failure after exhaustion.
- Integration smoke with real `aws-cli`: PUT/GET/HEAD/LIST/DELETE still pass under hook manager wiring.
- Integration smoke with real `curl`: presigned GET downloads, presigned PUT uploads native bytes, expired/tampered URLs return standard `403` XML.
- Integration smoke with local webhook receiver: PUT, DELETE, and multipart complete deliver the expected event JSON; receiver outage does not block upload and logs retry failure; disabled hooks do not trigger.
- Quota smoke: presigned PUT observes the same quota rejection and used-bytes accounting as header-auth PUT.

### 7. Wrong vs Correct

Wrong:

```go
func (h *ObjectHandler) Put(w http.ResponseWriter, r *http.Request, bucket, key string) {
    info, _ := h.backend.PutObject(bucket, key, r.Body, r.Header.Get("Content-Type"))
    http.Post(hookURL, "application/json", eventBody(info)) // blocks request path
}
```

Correct:

```go
func (h *ObjectHandler) Put(w http.ResponseWriter, r *http.Request, bucket, key string) {
    info, err := h.backend.PutObject(bucket, key, r.Body, r.Header.Get("Content-Type"))
    if err != nil {
        writeStorageError(w, err, r.URL.Path)
        return
    }
    h.hooks.Emit(hooks.Event{Type: hooks.ObjectCreated, Bucket: bucket, Key: key, Size: info.Size})
}
```

Wrong:

```go
canonical, err := CanonicalRequest(r, signedHeaders, bodySHA256(r.Body))
```

Correct:

```go
query := r.URL.Query()
query.Del("X-Amz-Signature")
canonical, err := CanonicalRequestWithQuery(r, signedHeaders, UnsignedPayload, query)
```

---

## Common Mistakes

- GORM `default:true` on `HookConfig.Enabled` can make zero-value `Create` write `true`. Tests that need disabled rows should create then explicitly update `enabled=false`, or use an update path that writes the zero value.
- `max_retry` means retries after the first delivery attempt. A default of 3 therefore allows up to 4 delivery attempts total.
