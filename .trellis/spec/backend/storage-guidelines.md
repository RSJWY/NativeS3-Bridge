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
- Optional extended write path: `func (b *FileBackend) PutObjectWithOptions(bucket, key string, r io.Reader, opts PutOptions) (ObjectInfo, error)` where `PutOptions.ExpectedMD5` is the lowercase hex MD5 expected for the streamed object bytes.
- `func (b *FileBackend) GetObject(bucket, key string, rng *Range) (io.ReadCloser, ObjectInfo, error)`
- `func (b *FileBackend) HeadObject(bucket, key string) (ObjectInfo, error)`
- `func (b *FileBackend) DeleteObject(bucket, key string) error`
- `func (b *FileBackend) ListObjects(bucket, prefix, delimiter, token string, maxKeys int) (ListResult, error)`
- `func (b *FileBackend) ListBuckets() ([]BucketInfo, error)`

### 3. Contracts

- Bucket names must match `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`.
- Object keys must not contain `..` path segments and must resolve under `data_root/bucket` after `path.Clean` and absolute-path prefix checks.
- `PutObject` must create parent directories, stream the request body once into `<target>.tmp-<random>`, call `fsync`, close, then `os.Rename` to the final native file path.
- Ordinary HTTP PUT Object may include `Content-MD5`, encoded as base64 of the raw 16-byte MD5 digest. Handlers must validate the header format before storage writes and pass the decoded lowercase hex digest through the optional `PutObjectWithOptions` path.
- When `PutOptions.ExpectedMD5` is non-empty, `FileBackend` must compare it with the computed MD5 after `io.Copy`/`f.Sync`/`Close` and before `os.Rename`. A mismatch must remove the temp file and return `ErrBadDigest`; it must not publish the object or write a sidecar.
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
- Malformed `Content-MD5` header (not base64 or decoded length is not 16 bytes) -> HTTP `400 InvalidDigest` XML before storage writes.
- Valid `Content-MD5` header that does not match the streamed object bytes -> `ErrBadDigest` -> HTTP `400 BadDigest` XML, with no target object, sidecar, or `.tmp-*` residue from that write.
- Internal filesystem errors -> HTTP `500 InternalError` XML; do not leak internal paths or raw errors in the response body.

### 5. Good/Base/Bad Cases

- Good: `PutObject("test-bucket", "dir/a.txt", r, "text/plain")` creates `data_root/test-bucket/dir/a.txt` with byte-for-byte identical native contents and a quoted MD5 ETag in HTTP responses.
- Good: HTTP `PUT /test-bucket/a.txt` with `Content-MD5` matching the request body succeeds and returns the same MD5 as the quoted ETag.
- Base: `ListObjects("test-bucket", "dir/", "/", "", 1000)` returns immediate files under `dir/` as `Contents` and nested folders as `CommonPrefixes`.
- Bad: accepting a malformed `Content-MD5` as an unchecked upload, or checking a mismatched digest after `os.Rename` has already made the object visible.
- Bad: accepting `dir/../escape.txt`, writing chunk files as the final object, or returning path traversal failures as `InternalError`.

### 6. Tests Required

- Unit tests for invalid bucket names and `..` object keys returning the expected storage errors.
- Unit tests asserting `PutObject` writes exact native bytes and MD5 ETag.
- Unit tests for `PutObjectWithOptions` with matching `ExpectedMD5` succeeding and mismatched `ExpectedMD5` returning `ErrBadDigest` while leaving no object, sidecar, or `.tmp-*` file.
- Handler/router tests for `Content-MD5`: matching digest succeeds, mismatched digest returns `BadDigest`, malformed header returns `InvalidDigest`, and missing header preserves normal PUT behavior.
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
etag := hex.EncodeToString(h.Sum(nil))
if opts.ExpectedMD5 != "" && !strings.EqualFold(opts.ExpectedMD5, etag) {
    _ = os.Remove(tmp)
    return ObjectInfo{}, ErrBadDigest
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

## Scenario: Bucket Reconciliation Scan

### 1. Scope / Trigger
- Trigger: changes to manual disk reconciliation or object-list exclusion rules.

### 2. Signatures
- `storage.ReconcileBucket(root, bucket, metadataSuffix) (ReconcileReport, error)`.
- `ReconcileReport.DeleteOrphanSidecars() (int, error)`.

### 3. Contracts
- Scan one valid existing bucket; count only regular object files and bytes.
- Skip `.multipart` directories plus metadata suffix, `.s3meta`, `.db`, `.sqlite`, and `.sqlite3` files exactly as object listing does.
- A sidecar is orphaned only when its corresponding regular object is absent. Slash-relative samples are capped at 50.
- Delete only discovered orphan sidecars; never delete objects or create missing sidecars.

### 4. Validation & Error Matrix
- Invalid bucket -> `ErrInvalidBucketName`; missing directory -> `ErrNoSuchBucket`; filesystem failures propagate to the admin layer for sanitized handling.

### 5. Good/Base/Bad Cases
- Good: native bytes contribute to size while a missing object's `.s3meta` is reported.
- Base: empty bucket reports zeros.
- Bad: counting sidecars/database files, following symlinks as objects, or deleting an object during apply.

### 6. Tests Required
- Assert object bytes, sidecar exclusion, orphan detection/deletion, missing/invalid bucket, and object preservation.

### 7. Wrong vs Correct
- Wrong: infer objects from a database table or sidecars.
- Correct: walk the native bucket directory with `ListObjects` exclusions because disk is the object truth source.
