# Object Write Integrity Design

## Scope

This task covers single-part object PUT through `pkg/handlers/object.go` and native filesystem writes in `pkg/storage/file_backend.go`. It preserves existing native object layout, sidecar metadata, quota accounting, hooks, and CopyObject behavior.

## Current State

- `FileBackend.PutObjectWithMetadata` already writes to `<target>.tmp-<random>`, computes MD5 while streaming, calls `fsync`, closes, and renames to the final object path.
- `FileBackend.CopyObject` already uses the same temp-file/rename shape and recomputes MD5 ETag for the copied bytes.
- `ObjectHandler.Put` does not parse checksum headers and can only call `PutObjectWithMetadata(bucket, key, body, contentType, metadata)`.
- `writeStorageError` has no storage error path for checksum mismatch, and `errorMessage` has no `BadDigest` text.

## Proposed Contract

Add a storage-level PUT options path for checksum validation while leaving the existing `Backend` interface stable for simple implementations.

Suggested shape:

```go
type PutObjectOptions struct {
    ContentType    string
    Metadata       map[string]string
    ContentMD5     []byte
    ContentSHA256  string
}

func (b *FileBackend) PutObjectWithOptions(bucket, key string, r io.Reader, opts PutObjectOptions) (ObjectInfo, error)
```

Existing methods can delegate:

```go
PutObject(...) -> PutObjectWithOptions(...)
PutObjectWithMetadata(...) -> PutObjectWithOptions(...)
```

`ObjectHandler.Put` should prefer a backend capability interface with `PutObjectWithOptions`, falling back to the existing methods only if unavailable.

## Data Flow

1. Handler extracts user metadata as today.
2. Handler parses checksum headers:
   - `Content-MD5`: base64 decode to exactly 16 bytes.
   - `x-amz-content-sha256`: if it is a concrete lowercase/uppercase 64-character hex string, normalize to lowercase and pass it as expected SHA-256.
   - Compatibility sentinels such as `UNSIGNED-PAYLOAD` are ignored for checksum comparison.
3. Handler calls `PutObjectWithOptions` when supported.
4. Storage streams request body once into the temp file while updating MD5 and SHA-256 hashers as needed.
5. After copy, before rename, storage compares computed digests against expected values.
6. On mismatch, storage closes/removes the temp file and returns a checksum error. The final target path is never renamed over.
7. On success, storage renames the temp file, writes sidecar metadata, updates content type cache, and returns `ObjectInfo` with the MD5 ETag.
8. Handler maps checksum mismatch to `BadDigest`, writes no success headers, and skips usage commit/hook emission because `Put` already returns on error.

## Error Mapping

- Checksum mismatch should map to HTTP 400 S3 XML code `BadDigest`.
- Malformed `Content-MD5` is the only remaining product ambiguity. AWS distinguishes malformed digest (`InvalidDigest`) from mismatch (`BadDigest`), but the task goal explicitly names `BadDigest` for mismatches. The recommended implementation is `InvalidDigest` for malformed base64/length and `BadDigest` for decoded digest mismatch.
- Malformed concrete `x-amz-content-sha256` that is neither a known compatibility sentinel nor a 64-character hex digest should be treated as a bad digest input before writing.

## Compatibility Notes

- Do not require checksum headers; existing clients with no checksum header must behave exactly as today.
- Do not compare `UNSIGNED-PAYLOAD` to object bytes. It is a SigV4 compatibility marker, not a body checksum.
- Keep single-part ETag as lowercase hex MD5. The SHA-256 validation path must not change ETag semantics.
- Keep `Backend` interface source-compatible by adding an optional capability interface rather than changing the core `Backend` interface.

## Rollback Shape

The change should be localized to checksum parsing, optional PUT options, storage digest comparison, error mapping, and tests. If regressions appear, fallback is to route `ObjectHandler.Put` back to `PutObjectWithMetadata` and remove the new capability path while leaving existing atomic write behavior intact.
