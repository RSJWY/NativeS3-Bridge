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
- Multipart `UploadPart` does not count against `used_bytes`. `CompleteMultipartUpload` must compute the total submitted part size, run `quota.Check(id, totalSize)` before native merge, then call `Commit(OpPut, totalSize)` only after successful merge and sidecar write.
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
- Multipart complete quota exceeded -> HTTP 403 `QuotaExceeded` XML; temporary multipart upload data is aborted/removed and `used_bytes` is unchanged.
- Invalid quota operation -> `Commit` returns `ErrInvalidOp` and callers log it.

### 5. Good/Base/Bad Cases

- Good: aws-cli signs a PUT with an enabled DB credential, `Verify` returns identity, `Check` passes, object writes natively, and `Commit` records `used_bytes += actualSize`, `put_count += 1`, `bytes_in += actualSize`.
- Good: aws-cli multipart upload stores parts without quota mutation; Complete computes total part size, checks quota once, merges to one native file, then commits `used_bytes += totalSize`.
- Base: aws-cli `--no-sign-request` receives standard 403 `<Error><Code>AccessDenied</Code>...` without leaking internal DB or filesystem details.
- Bad: caching an enabled credential's `UsedBytes` for 60 seconds and allowing sequential uploads to bypass quota until TTL expiry.
- Bad: computing `UsedBytes + incoming > QuotaBytes` directly, which can overflow for large signed integers.
- Bad: incrementing GET `bytes_out` before `io.Copy` succeeds.
- Bad: applying quota to every `UploadPart`, because failed or aborted multipart uploads would consume permanent credential capacity.

### 6. Tests Required

- Unit test canonical request and string-to-sign against AWS S3 documentation vectors.
- Unit test successful `LocalSigV4Authenticator.Verify` plus wrong secret, unknown access key, disabled credential, and clock skew errors.
- Unit test credential cache hit refreshes `UsedBytes` for enabled credentials.
- Unit test `quota.Check` for unlimited, exact limit, exceeded limit, negative incoming, and overflow-safe comparisons.
- Unit test `quota.Commit` for put/get/delete counters, positive and negative delete deltas, lower-bound zero, invalid ops, and concurrent put updates.
- Smoke test with real aws-cli for PUT, HEAD, GET byte compare, LIST, DELETE.
- Smoke test with real aws-cli for wrong secret, missing signature, unknown access key, quota exceeded, DB usage/stat totals, and concurrent uploads.
- Smoke test with real aws-cli multipart upload where `used_bytes` increases by the final merged object size after Complete, and an over-quota Complete rejects without leaving native object bytes or increasing usage.

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

---

## Scenario: Anonymous Public-Read Object Downloads

### 1. Scope / Trigger

- Trigger: any change to S3 auth middleware, bucket ACL lookup, anonymous identity handling, object GET/HEAD dispatch, or GET usage accounting.
- Goal: support managed `public-read` buckets for anonymous single-object downloads without weakening the private-bucket or signed-request security model.

### 2. Signatures

- `func HasPresignQuery(r *http.Request) bool`
- `func Auth(authenticator auth.Authenticator, aclLookup server.ACLLookup) server.Middleware`
- `type ACLLookup func(bucket string) (acl string, exists bool, err error)`
- `func AnonymousIdentity() *auth.Identity`
- `func IsAnonymous(id *auth.Identity) bool`
- `func (s *storage.BucketStore) GetACL(name string) (acl string, exists bool, err error)`
- `func (s *storage.BucketStore) SetACL(name, acl string) error`

### 3. Contracts

- A request has credentials when it includes an `Authorization` header or a complete query presign set detected by `auth.HasPresignQuery`; credentialed requests must continue through `authenticator.Verify` and must not use the anonymous ACL branch.
- A request is eligible for anonymous public-read only when method is `GET` or `HEAD`, path parses to `bucket != ""` and `key != ""`, and query does not contain management/write subresources such as `tagging`, `uploads`, `uploadId`, `acl`, or `tags`.
- Anonymous eligible requests call `BucketStore.GetACL(bucket)`. `exists=false` means historical/unregistered bucket and must be treated as private.
- Anonymous access is allowed only for `exists=true && acl == storage.ACLPublicRead`; allowed requests receive `auth.AnonymousIdentity()` in context before reaching quota and object handlers.
- Anonymous object reads must not call `quota.Commit` or write `request_stats` for `credential_id=0`. Signed GETs continue to count as normal.
- ACL cache invalidation is in-process. `BucketStore.SetACL` invalidates the running store immediately; direct DB updates do not invalidate another already-running `BucketStore` and are only suitable for smoke checks after restart or TTL expiry.

### 4. Validation & Error Matrix

- Anonymous private bucket object GET/HEAD -> HTTP 403 `AccessDenied` XML.
- Anonymous public-read object GET/HEAD -> object handler response, including `206` for valid Range requests.
- Anonymous unregistered/historical bucket object GET/HEAD -> HTTP 403 `AccessDenied` XML.
- Anonymous bucket-level GET/ListObjectsV2 -> HTTP 403 `AccessDenied` XML, even when bucket is public-read.
- Anonymous PUT/DELETE/POST/multipart/tagging/ACL subresource -> HTTP 403 `AccessDenied` XML, even when bucket is public-read.
- ACL lookup DB error -> HTTP 500 `InternalError` XML and a structured server log entry; do not leak DB details to the client.
- Credentialed header SigV4 or presigned requests -> unchanged `authenticator.Verify` behavior and existing S3 auth error codes.

### 5. Good/Base/Bad Cases

- Good: `curl http://host/public-bucket/known/key.txt` returns `200` only after the same process observes `SetACL(public-bucket, public-read)`.
- Good: `curl -I http://host/public-bucket/known/key.txt` returns `Content-Length`, `ETag`, and any `x-amz-meta-*` headers without requiring credentials.
- Base: a filesystem bucket with no `buckets` table row remains anonymous-private and returns `403`, while signed access proceeds normally.
- Bad: allowing anonymous `GET /public-bucket` to list objects because the bucket ACL is public-read.
- Bad: treating a partial or malformed presign query as anonymous; only `HasPresignQuery` identifies credentialed presign requests, otherwise anonymous rules apply and will normally deny unsafe paths.

### 6. Tests Required

- Unit test anonymous matrix for method, path shape, ACL result, credential presence, and blocked subresources.
- Unit test credentialed requests bypass ACL lookup and call `authenticator.Verify` exactly once.
- Unit test anonymous identities pass through quota for GET/HEAD and do not call usage commit.
- Integration smoke with real `aws-cli` for signed create bucket, PUT, HEAD, GET, LIST, DELETE, and presigned GET to prove signed behavior is unchanged.
- Integration smoke with real `curl` for anonymous private GET 403 XML, public-read GET 200 byte match, HEAD metadata headers, Range 206 byte match, List/PUT/DELETE 403, and SetACL back to private causing the next anonymous GET to return 403 through the same `BucketStore` instance.

### 7. Wrong vs Correct

Wrong:

```go
if bucketACL == storage.ACLPublicRead && r.Method == http.MethodGet {
    next.ServeHTTP(w, r) // also allows ListObjectsV2 and ?tagging reads.
}
```

Correct:

```go
bucket, key := parseS3Path(r.URL.Path)
if !hasCredentials(r) && isAnonymousObjectRead(r, bucket, key) {
    acl, exists, err := aclLookup(bucket)
    if err == nil && exists && acl == storage.ACLPublicRead {
        next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), auth.AnonymousIdentity())))
        return
    }
}
handlers.WriteS3Error(w, auth.CodeAccessDenied, http.StatusForbidden, r.URL.Path)
```

## Scenario: Reconcile Quota Rewrite

### 1. Scope / Trigger
- Trigger: admin reconciliation writes `credentials.used_bytes` outside S3 PUT/DELETE accounting.

### 2. Signatures
- `POST /api/admin/buckets/{name}/reconcile` with `{"apply": boolean}`.
- `CredentialStore.Invalidate(accessKey string)` after each committed rewrite.

### 3. Contracts
- Dry-run changes nothing. Apply rescans, deletes orphan sidecars, then sets every non-empty `credentials.bucket = name` row to scanned bytes.
- Global and other-bucket credentials are unchanged. Multiple keys bound to one bucket each receive its full scanned bytes.
- Quota limits and request statistics are unchanged; reconciled use may exceed quota.

### 4. Validation & Error Matrix
- Sidecar deletion failure -> 500 before DB rewrite; DB transaction failure -> 500; commit -> invalidate every updated access key.

### 5. Good/Base/Bad Cases
- Good: a cached bound credential reads the reconciled value after invalidation.
- Base: a bucket without bound keys can still scan and remove orphans.
- Bad: rewriting global credentials from a single-bucket scan or omitting invalidation.

### 6. Tests Required
- Assert dry-run immutability, apply value, global/other-bucket preservation, invalidation, and unchanged objects.

### 7. Wrong vs Correct
- Wrong: update every credential that can access the bucket.
- Correct: update only explicitly bound credentials and invalidate after commit.
