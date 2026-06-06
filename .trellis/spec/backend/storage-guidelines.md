# Storage Guidelines

> Native filesystem object-storage contracts for the S3 core layer.

---

## Scenario: S3 Core Native File Storage

### 1. Scope / Trigger

- Trigger: any change to `pkg/storage`, `pkg/handlers` object/bucket behavior, or S3 route dispatch that can affect native object bytes, object paths, bucket listing, object listing, ETag, Content-Type, Range, or S3 error mapping.
- Goal: preserve the project red line that Bucket = first-level directory and Object Key = relative native file path with original bytes.

### 2. Signatures

- `func ResolveBucketPath(root, bucket string) (string, error)`
- `func ResolveObjectPath(root, bucket, key string) (string, error)`
- `func NewFileBackend(root string) (*FileBackend, error)`
- `func (b *FileBackend) PutObject(bucket, key string, r io.Reader, contentType string) (ObjectInfo, error)`
- `func (b *FileBackend) GetObject(bucket, key string, rng *Range) (io.ReadCloser, ObjectInfo, error)`
- `func (b *FileBackend) HeadObject(bucket, key string) (ObjectInfo, error)`
- `func (b *FileBackend) DeleteObject(bucket, key string) error`
- `func (b *FileBackend) ListObjects(bucket, prefix, delimiter, token string, maxKeys int) (ListResult, error)`
- `func (b *FileBackend) ListBuckets() ([]BucketInfo, error)`

### 3. Contracts

- Bucket names must match `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`.
- Object keys must not contain `..` path segments and must resolve under `data_root/bucket` after `path.Clean` and absolute-path prefix checks.
- `PutObject` must create parent directories, stream the request body once into `<target>.tmp-<random>`, call `fsync`, close, then `os.Rename` to the final native file path.
- The final on-disk file must be the original object bytes; no chunking, container format, encoding, or sidecar dependency is allowed for reading the native file.
- Single-part ETag is the lowercase hex MD5 of the object bytes, returned quoted at HTTP handler boundaries.
- `GetObject` must return an `io.ReadCloser` and handlers must stream with `io.Copy`; do not load whole objects into memory for downloads.
- `Range{Start, End}` uses inclusive byte offsets; open-ended ranges have `End < 0` and normalize to the object tail.
- `ListObjects` returns sorted keys, supports `prefix`, `delimiter=/` as `CommonPrefixes`, `maxKeys`, and continuation token based on the last emitted key or common prefix.
- `ListBuckets` lists first-level valid bucket directories only and excludes hidden directories such as `.multipart` plus non-directory database files.

### 4. Validation & Error Matrix

- Invalid bucket name -> `ErrInvalidBucketName` -> HTTP `400 InvalidBucketName` XML.
- Empty key, `..` segment, or path outside bucket -> `ErrInvalidPath` -> HTTP `400 InvalidArgument` XML.
- Missing bucket on `HeadObject`, `DeleteObject`, or `ListObjects` -> `ErrNoSuchBucket` -> HTTP `404 NoSuchBucket` XML.
- Missing object in an existing bucket -> `ErrNoSuchKey` -> HTTP `404 NoSuchKey` XML.
- Invalid or unsatisfiable Range -> `ErrInvalidRange` -> HTTP `416 InvalidRange` XML.
- Internal filesystem errors -> HTTP `500 InternalError` XML; do not leak internal paths or raw errors in the response body.

### 5. Good/Base/Bad Cases

- Good: `PutObject("test-bucket", "dir/a.txt", r, "text/plain")` creates `data_root/test-bucket/dir/a.txt` with byte-for-byte identical native contents and a quoted MD5 ETag in HTTP responses.
- Base: `ListObjects("test-bucket", "dir/", "/", "", 1000)` returns immediate files under `dir/` as `Contents` and nested folders as `CommonPrefixes`.
- Bad: accepting `dir/../escape.txt`, writing chunk files as the final object, or returning path traversal failures as `InternalError`.

### 6. Tests Required

- Unit tests for invalid bucket names and `..` object keys returning the expected storage errors.
- Unit tests asserting `PutObject` writes exact native bytes and MD5 ETag.
- Unit tests for `HeadObject` size, ETag, LastModified UTC, and Content-Type behavior.
- Unit tests for `GetObject` with Range returning the correct byte slice and full object metadata.
- Unit tests for `DeleteObject` removal plus post-delete `ErrNoSuchKey` in an existing bucket.
- Unit tests for missing bucket semantics on head/delete/list.
- Unit tests for `ListObjects` delimiter, pagination token, `maxKeys=0`, and sidecar filtering.
- Smoke tests with a running server for PUT/GET/HEAD/Range/ListBuckets/ListObjects/Delete and standard S3 XML errors.

### 7. Wrong vs Correct

Wrong:

```go
target := filepath.Join(root, bucket, key)
data, _ := io.ReadAll(r)
_ = os.WriteFile(target, data, 0o644)
```

Correct:

```go
target, err := ResolveObjectPath(root, bucket, key)
if err != nil {
    return ObjectInfo{}, err
}
tmp := target + ".tmp-" + randomHex(8)
f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
if err != nil {
    return ObjectInfo{}, err
}
h := md5.New()
_, copyErr := io.Copy(io.MultiWriter(f, h), r)
syncErr := f.Sync()
closeErr := f.Close()
if copyErr != nil || syncErr != nil || closeErr != nil {
    _ = os.Remove(tmp)
    return ObjectInfo{}, firstErr(copyErr, syncErr, closeErr)
}
if err := os.Rename(tmp, target); err != nil {
    _ = os.Remove(tmp)
    return ObjectInfo{}, err
}
```

### Common Mistakes

- Do not take the address of a Go 1.21 range loop variable when building list result pointers; use `for i := range objects { &objects[i] }`.
- Do not convert `maxKeys == 0` to the default page size. `max-keys=0` should return no entries and indicate truncation when entries exist.
- Do not persist S3 Content-Type by inventing a sidecar in the core objects task. Durable object metadata belongs to the later metadata sidecar task; core storage may only infer by extension or preserve same-process values in memory.
- Do not return `NoSuchKey` for missing buckets on bucket-scoped operations. Preserve `NoSuchBucket` so handlers emit the correct S3 XML.

## Scenario: S3 Compatibility Operations

### 1. Scope / Trigger

- Trigger: any change to S3 route dispatch or handlers for bucket subresources, multi-object operations, or server-side object copy.
- Goal: keep aws-cli/SDK compatibility without weakening native object storage, quota accounting, public-read boundaries, or hook emission.

### 2. Signatures

- Route: `POST /{bucket}?delete` -> DeleteObjects.
- Route: `PUT /{bucket}/{key}` with `x-amz-copy-source` -> CopyObject, before normal PutObject.
- Route: `GET /{bucket}?location` -> GetBucketLocation.
- Route: `GET /{bucket}?versioning` -> GetBucketVersioning.
- Storage capability: `func (b *FileBackend) CopyObject(srcBucket, srcKey, dstBucket, dstKey string) (ObjectInfo, error)`.

### 3. Contracts

- DeleteObjects accepts AWS `<Delete>` XML with one or more `<Object><Key>...</Key></Object>` entries and optional `<Quiet>true</Quiet>`.
- DeleteObjects returns `<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`; when quiet is false, include `<Deleted><Key>...</Key></Deleted>` for existing and missing keys.
- DeleteObjects ignores `VersionId` because object versioning is not implemented.
- CopyObject parses `x-amz-copy-source` as `bucket/key` or `/bucket/key`, strips source query parameters such as `versionId`, and URL path-unescapes bucket/key once.
- CopyObject streams source bytes to a destination temp file, computes the copied object's MD5 ETag, then renames to the native destination path. Do not route copy requests through normal PutObject, because aws-cli sends a zero-length request body.
- CopyObject preserves source sidecar content type, user metadata, and tags by default. Missing source sidecar falls back to `HeadObject` metadata and empty tags.
- CopyObject quota uses source object size, not request Content-Length. Middleware must skip normal PUT Content-Length quota for copy requests; the handler checks quota before storage copy and commits `OpPut` after successful copy.
- `?location` returns empty `LocationConstraint` XML for the default `us-east-1` behavior, which aws-cli parses as `null`.
- `?versioning` returns empty `VersioningConfiguration` XML to represent disabled/unconfigured versioning.
- Anonymous public-read still applies only to object `GET`/`HEAD`; DeleteObjects and CopyObject remain signed-only.

### 4. Validation & Error Matrix

- Malformed DeleteObjects XML, empty object list, or empty key -> HTTP `400 InvalidArgument` XML.
- DeleteObjects missing key in existing bucket -> successful deleted entry, no usage commit, no hook event.
- DeleteObjects missing bucket -> HTTP `404 NoSuchBucket` XML.
- Malformed or empty `x-amz-copy-source` -> HTTP `400 InvalidArgument` XML.
- CopyObject missing source bucket or destination bucket -> HTTP `404 NoSuchBucket` XML.
- CopyObject missing source object -> HTTP `404 NoSuchKey` XML.
- CopyObject over quota -> HTTP `403 QuotaExceeded` XML before writing destination bytes or committing usage.
- `?location` or `?versioning` missing bucket -> HTTP `404 NoSuchBucket` XML, not a ListObjects response.

### 5. Good/Base/Bad Cases

- Good: `aws s3api copy-object --bucket b --key copy.txt --copy-source b/source.txt` creates `data_root/b/copy.txt` with byte-for-byte identical native bytes, copied metadata/tags, and MD5 ETag of copied bytes.
- Good: `aws s3api delete-objects --bucket b --delete 'Objects=[{Key=a.txt},{Key=missing.txt}],Quiet=false'` returns two deleted entries and only decrements usage/emits hooks for `a.txt` if it existed.
- Base: `aws s3api get-bucket-location --bucket b` returns `LocationConstraint: null` for `us-east-1`; `get-bucket-versioning` returns an empty parsed object.
- Bad: letting `PUT` with `x-amz-copy-source` fall through to normal PutObject, which creates a 0-byte destination object and reports the empty MD5 ETag.
- Bad: applying normal PUT request-body quota middleware to CopyObject, because copy requests often have unknown or zero body length while the stored object size comes from the source.

### 6. Tests Required

- Unit test storage `CopyObject` byte equality, MD5 ETag, content type, user metadata, tags, missing source bucket/key, missing destination bucket, and destination overwrite behavior.
- Unit test handler CopyObject success response, quota rejection before copy, usage commit after success, and ObjectCreated hook emission.
- Unit test DeleteObjects existing plus missing key, quiet mode, malformed XML/empty key, missing bucket, usage decrement, and ObjectDeleted hook emission only for existing keys.
- Unit test route/middleware priority: `x-amz-copy-source` bypasses normal PUT quota middleware and dispatches before normal PutObject; tagging and multipart routes keep priority.
- Unit test bucket probes return LocationConstraint/VersioningConfiguration and missing buckets return NoSuchBucket instead of ListBucketResult.
- aws-cli smoke for `copy-object`, `get-object` byte compare, metadata/tags round trip, `delete-objects`, `get-bucket-location`, and `get-bucket-versioning`.

### 7. Wrong vs Correct

Wrong:

```go
if r.Method == http.MethodPut {
    objectHandler.Put(w, r, bucket, key) // copy requests write an empty body
}
```

Correct:

```go
if r.Method == http.MethodPut {
    if r.Header.Get("x-amz-copy-source") != "" {
        objectHandler.Copy(w, r, bucket, key)
        return
    }
    objectHandler.Put(w, r, bucket, key)
}
```

Wrong:

```go
if r.Method == http.MethodPut {
    checkQuota(r.ContentLength)
}
```

Correct:

```go
if r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") == "" {
    checkQuota(contentLengthForQuota(r))
}
```

## Scenario: Multipart Uploads And Metadata Sidecars

### 1. Scope / Trigger

- Trigger: any change to `pkg/storage/metadata.go`, `pkg/storage/multipart.go`, object tagging handlers, multipart handlers, S3 route query dispatch, storage config keys for multipart GC, or sidecar-aware object listing.
- Goal: preserve native object bytes while supporting S3 multipart upload, custom metadata, and object tags through isolated temporary directories and `.s3meta` sidecars.

### 2. Signatures

- `type Sidecar struct { ETag string; ContentType string; Metadata map[string]string; Tags map[string]string; Size int64; UploadedAt string }`
- `func WriteSidecar(objPath, suffix string, s Sidecar) error`
- `func ReadSidecar(objPath, suffix string) (Sidecar, bool, error)`
- `func DeleteSidecar(objPath, suffix string) error`
- `func NewMultipartStore(root, tmpRoot, metadataSuffix string) (*MultipartStore, error)`
- `func (s *MultipartStore) Create(bucket, key, contentType string, meta map[string]string, tags map[string]string) (string, error)`
- `func (s *MultipartStore) ValidateTarget(uploadID, bucket, key string) error`
- `func (s *MultipartStore) UploadPart(uploadID string, partNumber int, r io.Reader) (string, error)`
- `func (s *MultipartStore) CompleteSize(uploadID string, parts []CompletedPart) (int64, error)`
- `func (s *MultipartStore) Complete(uploadID string, parts []CompletedPart) (ObjectInfo, error)`
- `func (s *MultipartStore) Abort(uploadID string) error`
- `func (s *MultipartStore) ListParts(uploadID string) ([]PartInfo, error)`
- `func (s *MultipartStore) ListMultipartUploads(bucket, prefix string) ([]MultipartUploadInfo, error)`
- `func (s *MultipartStore) CleanupExpired(ttl time.Duration) error`
- Config keys: `storage.multipart_tmp`, `storage.metadata_suffix`, `storage.multipart_gc_interval`, `storage.multipart_ttl`.

### 3. Contracts

- Sidecar path is exactly `<native object path><metadata_suffix>`, defaulting to `.s3meta`; sidecars are auxiliary metadata and must never be required to read the native object bytes.
- Sidecar JSON stores `etag`, `content_type`, user `metadata` without the `x-amz-meta-` prefix, `tags`, `size`, and RFC3339 UTC `uploaded_at`.
- `WriteSidecar` must write to a temporary file, `fsync`, close, then rename atomically to the sidecar path.
- Missing sidecar is not an error. `HeadObject`/`GetObject` must infer content type by extension and return empty metadata/tags for externally copied native files.
- Single-part `PutObject` writes native bytes first, then writes a sidecar containing the MD5 ETag, content type, user metadata, empty tags, size, and upload timestamp.
- `GetObject` and `HeadObject` must return sidecar `Content-Type` plus every metadata key as `x-amz-meta-<key>` at the HTTP handler boundary.
- `DeleteObject` must remove the native object and its sidecar; missing sidecar deletion is ignored.
- Object tagging updates only `Sidecar.Tags`. `PUT ?tagging` replaces tags, `GET ?tagging` returns current tags, and `DELETE ?tagging` clears tags. A later `PutObject` overwrite resets tags to an empty map.
- Multipart temporary layout is `{multipart_tmp}/{uploadID}/manifest.json` plus `part-00001` through `part-10000`; `uploadID` must be a canonical UUID before resolving any directory.
- Multipart handlers must call `ValidateTarget(uploadID, bucket, key)` before `UploadPart`, `Complete`, `Abort`, or `ListParts` so a valid upload ID cannot be used against a different URL bucket/key.
- `UploadPart` writes each part natively to a temporary file, `fsync`s, renames to `part-%05d`, and returns the lowercase hex MD5 as the part ETag.
- `Complete` must stream submitted parts in client order into one temporary target object file, `fsync`, close, rename to the final native object path, write the sidecar, compute multipart ETag as `md5(concat(raw part md5s)) + "-<partCount>"`, and remove the upload directory.
- `ListObjects` must exclude files ending in the configured metadata suffix and `.s3meta`, and must skip hidden multipart directories.
- Multipart GC runs from `main` using `StartGC(ctx.Done(), cfg.Storage.MultipartGCInterval, cfg.Storage.MultipartTTL)`; defaults are `1h` interval and `24h` TTL.

### 4. Validation & Error Matrix

- Invalid bucket/key path on multipart create -> storage path error -> S3 `InvalidBucketName` or `InvalidArgument`.
- Non-UUID, missing, expired, or mismatched `uploadId` -> `ErrNoSuchUpload` -> HTTP `404 NoSuchUpload` XML.
- `partNumber < 1` or `partNumber > 10000` -> `ErrInvalidPartNumber` -> HTTP `400 InvalidArgument` XML.
- Missing submitted part file or ETag mismatch -> `ErrInvalidPart` -> HTTP `400 InvalidPart` XML.
- Non-contiguous submitted part numbers -> `ErrInvalidPartOrder` -> HTTP `400 InvalidPartOrder` XML.
- Corrupt sidecar JSON -> storage error -> HTTP `500 InternalError`; missing sidecar remains a non-error fallback.
- Missing object for tagging -> storage `ErrNoSuchKey` / `ErrNoSuchBucket` -> standard S3 XML from `writeStorageError`.

### 5. Good/Base/Bad Cases

- Good: `aws s3 cp` of a 120 MiB file creates multipart temp parts, completes into one native `data_root/bucket/key` file, returns an ETag like `<hex>-15`, and leaves `multipart_tmp` empty.
- Good: `aws s3api put-object --metadata author=jdoe` writes native bytes plus a sidecar; `head-object` returns `Metadata.author == jdoe`.
- Base: direct filesystem copy of `data_root/test-bucket/external.txt` without sidecar still downloads through GET with inferred content type and empty metadata.
- Bad: accepting `uploadId=.` or `uploadId=../x` and resolving it through `filepath.Base`; this can target the multipart root instead of one upload directory.
- Bad: completing or aborting an upload ID from `/other-bucket/other-key?uploadId=<valid>` without checking the manifest bucket/key.
- Bad: listing `object.s3meta` sidecars as S3 objects or requiring sidecars to read native object bytes.

### 6. Tests Required

- Unit test `WriteSidecar`/`ReadSidecar`/`DeleteSidecar` for atomic write shape, missing-sidecar fallback, initialized maps, and delete idempotency.
- Unit test single-part `PutObjectWithMetadata`, `HeadObject`, `GetObject`, and `DeleteObject` preserve native bytes, return metadata headers, and remove sidecar.
- Unit test tagging PUT/GET/DELETE replaces, returns, and clears sidecar tags, including creating sidecar metadata for an existing external file.
- Unit test multipart create/upload/list/complete verifies part sorting, merged bytes, multipart ETag algorithm, sidecar content, and temp directory cleanup.
- Unit test rejects non-UUID upload IDs and verifies the multipart root remains intact.
- Unit test `ValidateTarget` rejects mismatched bucket or key for an otherwise valid upload ID.
- Unit test GC removes expired upload directories and preserves non-expired directories.
- Smoke test with real `aws-cli` for 100 MiB+ multipart upload, metadata round-trip, tagging round-trip/delete/overwrite, sidecar listing exclusion, external no-sidecar GET, and abort cleanup.

### 7. Wrong vs Correct

Wrong:

```go
func (s *MultipartStore) uploadDir(uploadID string) string {
    return filepath.Join(s.tmpRoot, filepath.Base(uploadID))
}

func (s *MultipartStore) Abort(uploadID string) error {
    return os.RemoveAll(s.uploadDir(uploadID))
}
```

Correct:

```go
func (s *MultipartStore) Abort(uploadID string) error {
    if err := validateUploadID(uploadID); err != nil {
        return err
    }
    return os.RemoveAll(s.uploadDir(uploadID))
}
```

Wrong:

```go
etag, err := store.UploadPart(r.URL.Query().Get("uploadId"), partNumber, r.Body)
```

Correct:

```go
uploadID := r.URL.Query().Get("uploadId")
if err := store.ValidateTarget(uploadID, bucket, key); err != nil {
    return err
}
etag, err := store.UploadPart(uploadID, partNumber, r.Body)
```
