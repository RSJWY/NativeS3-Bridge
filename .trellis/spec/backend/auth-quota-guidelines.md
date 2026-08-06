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
- `func NewLocalSigV2Authenticator(store *CredentialStore) *LocalSigV2Authenticator`
- `func NewMultiSchemeAuthenticator(v4, v2 Authenticator) *MultiSchemeAuthenticator`(v2 为 nil 表示禁用)
- `func ParseV2Authorization(header string) (ParsedV2Authorization, error)`
- `func StringToSignV2(r *http.Request, expires string) string`
- `func CanonicalizedAmzHeadersV2(h http.Header) string`
- `func CanonicalizedResourceV2(r *http.Request) string`(RawPath 优先,EscapedPath 回落)
- `func SignStringV2(secret, stringToSign string) string`
- `func Check(id *auth.Identity, incoming int64) error`
- `func Commit(gdb *gorm.DB, credID uint, deltaBytes int64, op Op) error`
- Startup seed flags: `--seed-access-key`, `--seed-secret-key`, `--seed-quota-bytes`.
- Nginx S3 location contract: `proxy_cache off;`, `proxy_cache_convert_head off;`, and `proxy_set_header Host $http_host;`.

### 3. Contracts

- 请求按签名形态由 `MultiSchemeAuthenticator` 分派,v4 严格优先于 v2:
  1. `Authorization` 以 `AWS4-HMAC-SHA256 ` 开头 → v4;
  2. `HasPresignQuery(r)` 为真 → v4 预签名;
  3. `Authorization` 以 `AWS `(带空格)开头(且非 `AWS4-`)→ v2;
  4. 查询串同时含 `AWSAccessKeyId`、`Expires`、`Signature` → v2 预签名;
  5. 其余 → 交回 v4 的既有路径。
  **注意:`AWS4-HMAC-SHA256` 本身也以 `AWS` 开头,判 v2 必须用带空格的 `HasPrefix(h, "AWS ")` 且已被第 1 条拦截在前。** v4 与 v2 预签名的查询参数名不重叠,判定互斥。
- SigV2 默认关闭(`auth.allow_sigv2`,bool 零值即 false),由配置显式开启。关闭时 v2 形态请求返回 `InvalidRequest` 而非 `SignatureDoesNotMatch`,便于运维区分"被禁用"与"签名算错"。
- SigV2 `StringToSign` = `HTTP-Verb\n + Content-MD5\n + Content-Type\n + Date\n + CanonicalizedAmzHeaders + CanonicalizedResource`;签名 = `Base64(HMAC-SHA1(SecretKey, StringToSign))`;比较恒定时间。
- SigV2 `CanonicalizedResource` 的路径取值**优先 `r.URL.RawPath`,为空回落 `r.URL.EscapedPath()`,绝不用已解码的 `r.URL.Path`**——含中文/括号/空格的 key 编码往返不一致会导致验签失败(GitHub issue #3 的核心)。子资源白名单(acl/lifecycle/location/logging/notification/partNumber/policy/requestPayment/torrent/uploadId/uploads/versionId/versioning/versions/website/cors/delete/tagging 与 `response-*` 前缀)按名字典序追加;非白名单参数不参与签名。
- SigV2 `CanonicalizedAmzHeaders`:收集所有 `x-amz-*` 头,名称小写、字典序、同名逗号合并、值折叠空白(复用 `normalizeHeaderValue`)、每条以 `\n` 结尾。`x-amz-date` 也纳入(此时 StringToSign 的 Date 行置空);未知 `x-amz-*` 头(如 boto3≥1.36 的 `x-amz-checksum-crc32`、`x-amz-sdk-checksum-algorithm`)按规范纳入,不得拒绝。
- SigV2 时间:`x-amz-date` 优先,否则 `Date` 头;RFC1123 与 ISO8601 都支持;时钟偏移 ±15 分钟同 v4。v2 预签名的 `Expires` 是绝对 Unix 时间戳,过期 → `AccessDenied`。
- SigV2 的安全弱点(已知限制):用 HMAC-SHA1、不签请求体、无 region/service scope;v2 + aws-chunked 的 payload 完整性完全依赖 aws-chunked 解码器校验(v2 不签 `x-amz-content-sha256`)。
- SigV4 canonical request helpers must remain pure and reusable: canonical URI/query/headers, signed headers, string-to-sign, signing key derivation, and constant-time signature comparison.
- Reverse proxies must preserve the exact client HTTP method and Host used by SigV4. For Nginx S3 locations, disable generic proxy caching and set `proxy_cache_convert_head off`; converting signed HEAD requests to upstream GET requests causes `SignatureDoesNotMatch`.
- `X-Amz-Date` clock skew is limited to plus or minus 15 minutes and returns `RequestTimeTooSkewed` on violation.
- Credential lookup is by `Credential.AccessKey`; missing keys return `InvalidAccessKeyId`, disabled credentials return `AccessDenied`, and bad signatures return `SignatureDoesNotMatch`.
- Credential cache may cache secret/status/quota for the TTL, but enabled credentials must not serve stale `UsedBytes` for quota checks. Refresh `used_bytes` on cache hit or explicitly invalidate after every usage mutation.
- PUT object quota checks use `Content-Length` or `x-amz-decoded-content-length` when present. Malformed or negative sizes are rejected before writing.
- Multipart `UploadPart` does not count against `used_bytes`. `CompleteMultipartUpload` must compute the total submitted part size, run `quota.Check(id, totalSize)` before native merge, then call `Commit(OpPut, totalSize)` only after successful merge and sidecar write.
- `QuotaBytes == 0` means unlimited. Otherwise reject when `incoming > QuotaBytes - UsedBytes` to avoid signed integer overflow.
- `Commit` runs in one GORM transaction: update `credentials.used_bytes` with a portable `CASE WHEN` expression and upsert `request_stats` via `clause.OnConflict` on `(credential_id, day)`.
- `OpPut` increments `used_bytes`, `put_count`, and `bytes_in`; `OpGet` increments `get_count` and `bytes_out` only after successful stream copy; `OpDelete` decrements `used_bytes` to a floor of zero and increments `delete_count` after successful delete.
- `Commit` failures after successful object operations are logged and do not change the object response.
- aws-chunked 请求体由 `server.AwsChunked` 中间件在 Auth 之后、quotaMiddleware 之前解码。判定条件:`x-amz-content-sha256` 属于 `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` / `STREAMING-UNSIGNED-PAYLOAD-TRAILER` / `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER` 之一,或 `Content-Encoding` 含 `aws-chunked`(大小写不敏感、逗号分隔取值)。命中后 `r.Body` 被替换为解码器,`r.ContentLength` 覆写为 `x-amz-decoded-content-length`,并从 `Content-Encoding` 移除 `aws-chunked` 这一个值(保留其余如 gzip)。
- 配额预检、`quotaLimitReadCloser`、`used_bytes`、`request_stats.bytes_in` 全部以解码后的真实对象字节数为准。`quotaLimitReadCloser` 只能包裹解码后的流;不得用解码后长度去截断未解码的流(否则会误报 `QuotaExceeded`)。
- `chunk-signature=` 不验证(已知限制)。头部 SigV4 签名保证请求头完整性;分块签名缺失只影响 body 完整性,风险等级低于数据损坏。trailer 中 `x-amz-checksum-crc32` / `crc32c` / `sha1` / `sha256` 在能识别时校验,不匹配返回 `BadDigest`;无法识别的算法名忽略。

### 4. Validation & Error Matrix

- Missing `Authorization` -> HTTP 403 `AccessDenied` XML.
- Malformed authorization header/scope/service/region/date -> HTTP 403 `SignatureDoesNotMatch` XML.
- Unknown access key -> HTTP 403 `InvalidAccessKeyId` XML.
- Disabled credential -> HTTP 403 `AccessDenied` XML.
- Clock skew over 15 minutes -> HTTP 403 `RequestTimeTooSkewed` XML.
- Signature mismatch -> HTTP 403 `SignatureDoesNotMatch` XML.
- SigV2 形态但 `auth.allow_sigv2` 关闭 -> HTTP 403 `InvalidRequest` XML(**不得**返回 `SignatureDoesNotMatch`)。
- SigV2 预签名 `Expires` 过期 -> HTTP 403 `AccessDenied` XML。
- Reverse proxy changes signed HEAD to upstream GET -> HTTP 403 `SignatureDoesNotMatch`; both proxy and application logs must record HEAD after correction.
- Quota exceeded -> HTTP 403 `QuotaExceeded` XML.
- Unknown PUT content length -> HTTP 400 `InvalidArgument` XML.
- Multipart complete quota exceeded -> HTTP 403 `QuotaExceeded` XML; temporary multipart upload data is aborted/removed and `used_bytes` is unchanged.
- Invalid quota operation -> `Commit` returns `ErrInvalidOp` and callers log it.
- aws-chunked 分块框架格式错误(长度行非法、缺少 CRLF、chunk 长度与实际不符) -> HTTP 400 `IncompleteBody` XML,**不得**返回 `QuotaExceeded`。
- aws-chunked 解码后长度与 `x-amz-decoded-content-length` 不一致 -> HTTP 400 `IncompleteBody` XML。
- aws-chunked trailer 校验和不匹配 -> HTTP 400 `BadDigest` XML,且不留下对象、sidecar 或 `.tmp-*` 残留。
- `x-amz-decoded-content-length` 缺失或非法 -> HTTP 400 `InvalidArgument` XML(缺失时传 -1 给解码器,不校验总长)。

### 5. Good/Base/Bad Cases

- Good: aws-cli signs a PUT with an enabled DB credential, `Verify` returns identity, `Check` passes, object writes natively, and `Commit` records `used_bytes += actualSize`, `put_count += 1`, `bytes_in += actualSize`.
- Good: Nginx forwards a signed HeadObject as HEAD with the original Host; an existing key returns 200 and a missing key reaches the handler and returns 404.
- Good: aws-cli multipart upload stores parts without quota mutation; Complete computes total part size, checks quota once, merges to one native file, then commits `used_bytes += totalSize`.
- Base: aws-cli `--no-sign-request` receives standard 403 `<Error><Code>AccessDenied</Code>...` without leaking internal DB or filesystem details.
- Bad: an inherited hosting-panel proxy cache converts client HEAD to upstream GET, so uploads and lists work while folder creation fails during its HeadObject preflight.
- Bad: caching an enabled credential's `UsedBytes` for 60 seconds and allowing sequential uploads to bypass quota until TTL expiry.
- Bad: computing `UsedBytes + incoming > QuotaBytes` directly, which can overflow for large signed integers.
- Bad: incrementing GET `bytes_out` before `io.Copy` succeeds.
- Bad: applying quota to every `UploadPart`, because failed or aborted multipart uploads would consume permanent credential capacity.
- Bad: 用 `x-amz-decoded-content-length`(解码后长度)去截断未解码的 aws-chunked 流,导致读到分块框架部分时误报 `QuotaExceeded`(GitHub issue #2 的真实根因)。
- Bad: 判宽 aws-chunked 条件(例如只看 `Content-Encoding` 存在),把普通 PUT 全部损坏。
- Bad: 把 `aws-chunked` 值留在 `Content-Encoding` 头里,使它被存进 sidecar 当对象元数据。

### 6. Tests Required

- Unit test canonical request and string-to-sign against AWS S3 documentation vectors.
- Unit test successful `LocalSigV4Authenticator.Verify` plus wrong secret, unknown access key, disabled credential, and clock skew errors.
- Unit test a correctly signed HEAD request and prove that changing its method to GET invalidates the signature.
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

```nginx
location / {
    proxy_cache s3_cache;
    proxy_pass http://127.0.0.1:9000;
}
```

Correct:

```nginx
location / {
    proxy_cache off;
    proxy_cache_convert_head off;
    proxy_set_header Host $http_host;
    proxy_pass http://127.0.0.1:9000;
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
