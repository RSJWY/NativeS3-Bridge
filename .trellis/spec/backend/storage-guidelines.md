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
