# Auth and Quota Guidelines

> SigV4 authentication and per-credential capacity accounting contracts.

---

## Scenario: SigV4 Auth And Per-Key Quota

### 1. Scope / Trigger

- Trigger: any change to `pkg/auth`, `pkg/quota`, S3 auth/quota middleware, usage accounting calls in handlers, credential seed flags, or S3 auth error XML mapping.
- Goal: preserve aws-cli-compatible header SigV4 verification and correct per-credential usage accounting without changing native object bytes.

### 2. Signatures

- `type Identity struct { CredentialID uint; AccessKey string; QuotaBytes int64; UsedBytes int64 }`
- `type Authenticator interface { Verify(r *http.Request) (*Identity, error) }`
- `func NewCredentialStore(gdb *gorm.DB, ttl time.Duration) *CredentialStore`
- `func (s *CredentialStore) Get(accessKey string) (*db.Credential, error)`
- `func (s *CredentialStore) Invalidate(accessKey string)`
- `func NewLocalSigV4Authenticator(store *CredentialStore, region string) *LocalSigV4Authenticator`
- `func Check(id *auth.Identity, incoming int64) error`
- `func Commit(gdb *gorm.DB, credID uint, deltaBytes int64, op Op) error`
- Startup seed flags: `--seed-access-key`, `--seed-secret-key`, `--seed-quota-bytes`.

### 3. Contracts

- Only header-based `Authorization: AWS4-HMAC-SHA256 ...` is accepted in this layer. Query presigned URL validation belongs to the presigned task.
- SigV4 canonical request helpers must remain pure and reusable: canonical URI/query/headers, signed headers, string-to-sign, signing key derivation, and constant-time signature comparison.
- `X-Amz-Date` clock skew is limited to plus or minus 15 minutes and returns `RequestTimeTooSkewed` on violation.
- Credential lookup is by `Credential.AccessKey`; missing keys return `InvalidAccessKeyId`, disabled credentials return `AccessDenied`, and bad signatures return `SignatureDoesNotMatch`.
- Credential cache may cache secret/status/quota for the TTL, but enabled credentials must not serve stale `UsedBytes` for quota checks. Refresh `used_bytes` on cache hit or explicitly invalidate after every usage mutation.
- PUT object quota checks use `Content-Length` or `x-amz-decoded-content-length` when present. Malformed or negative sizes are rejected before writing.
- `QuotaBytes == 0` means unlimited. Otherwise reject when `incoming > QuotaBytes - UsedBytes` to avoid signed integer overflow.
- `Commit` runs in one GORM transaction: update `credentials.used_bytes` with a portable `CASE WHEN` expression and upsert `request_stats` via `clause.OnConflict` on `(credential_id, day)`.
- `OpPut` increments `used_bytes`, `put_count`, and `bytes_in`; `OpGet` increments `get_count` and `bytes_out` only after successful stream copy; `OpDelete` decrements `used_bytes` to a floor of zero and increments `delete_count` after successful delete.
- `Commit` failures after successful object operations are logged and do not change the object response.

### 4. Validation & Error Matrix

- Missing `Authorization` -> HTTP 403 `AccessDenied` XML.
- Malformed authorization header/scope/service/region/date -> HTTP 403 `SignatureDoesNotMatch` XML.
- Unknown access key -> HTTP 403 `InvalidAccessKeyId` XML.
- Disabled credential -> HTTP 403 `AccessDenied` XML.
- Clock skew over 15 minutes -> HTTP 403 `RequestTimeTooSkewed` XML.
- Signature mismatch -> HTTP 403 `SignatureDoesNotMatch` XML.
- Quota exceeded -> HTTP 403 `QuotaExceeded` XML.
- Unknown PUT content length -> HTTP 400 `InvalidArgument` XML.
- Invalid quota operation -> `Commit` returns `ErrInvalidOp` and callers log it.

### 5. Good/Base/Bad Cases

- Good: aws-cli signs a PUT with an enabled DB credential, `Verify` returns identity, `Check` passes, object writes natively, and `Commit` records `used_bytes += actualSize`, `put_count += 1`, `bytes_in += actualSize`.
- Base: aws-cli `--no-sign-request` receives standard 403 `<Error><Code>AccessDenied</Code>...` without leaking internal DB or filesystem details.
- Bad: caching an enabled credential's `UsedBytes` for 60 seconds and allowing sequential uploads to bypass quota until TTL expiry.
- Bad: computing `UsedBytes + incoming > QuotaBytes` directly, which can overflow for large signed integers.
- Bad: incrementing GET `bytes_out` before `io.Copy` succeeds.

### 6. Tests Required

- Unit test canonical request and string-to-sign against AWS S3 documentation vectors.
- Unit test successful `LocalSigV4Authenticator.Verify` plus wrong secret, unknown access key, disabled credential, and clock skew errors.
- Unit test credential cache hit refreshes `UsedBytes` for enabled credentials.
- Unit test `quota.Check` for unlimited, exact limit, exceeded limit, negative incoming, and overflow-safe comparisons.
- Unit test `quota.Commit` for put/get/delete counters, positive and negative delete deltas, lower-bound zero, invalid ops, and concurrent put updates.
- Smoke test with real aws-cli for PUT, HEAD, GET byte compare, LIST, DELETE.
- Smoke test with real aws-cli for wrong secret, missing signature, unknown access key, quota exceeded, DB usage/stat totals, and concurrent uploads.

### 7. Wrong vs Correct

Wrong:

```go
if id.QuotaBytes > 0 && id.UsedBytes+incoming > id.QuotaBytes {
    return ErrQuotaExceeded
}
```

Correct:

```go
if id.QuotaBytes > 0 && incoming > id.QuotaBytes-id.UsedBytes {
    return ErrQuotaExceeded
}
```

Wrong:

```go
if cached && time.Now().Before(entry.expiresAt) {
    return &entry.credential, nil // UsedBytes can be stale.
}
```

Correct:

```go
if cached && time.Now().Before(entry.expiresAt) {
    cred := entry.credential
    if cred.Status == "enabled" {
        cred.UsedBytes = refreshUsedBytes(accessKey)
    }
    return &cred, nil
}
```
