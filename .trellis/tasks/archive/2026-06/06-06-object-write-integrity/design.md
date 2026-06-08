# Design: Object Write Integrity

## Architecture And Boundaries

The integrity gate belongs at two layers:

1. HTTP handler layer parses and validates the optional `Content-MD5` header.
2. File storage layer verifies the expected digest against bytes actually streamed to disk before publishing the temporary file with `rename`.

The existing `storage.Backend` interface remains unchanged. Any new write option is exposed through an optional interface, for example:

```go
PutObjectWithOptions(bucket, key string, r io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error)
```

`ObjectHandler.Put` should prefer that optional interface when available, then fall back to `PutObjectWithMetadata` or `PutObject` for older backend implementations. This keeps external or test backend compatibility.

## Data Flow

1. `ObjectHandler.Put` reads `Content-MD5` without consuming `r.Body`.
2. Empty header means no expected digest.
3. Non-empty header is trimmed, decoded with standard base64, and must decode to exactly `md5.Size` bytes.
4. Invalid header returns `InvalidDigest` with HTTP 400 before any storage write.
5. Valid header is converted to lowercase hex and passed to the file backend as `ExpectedMD5`.
6. `FileBackend` writes to `<target>.tmp-<random>` with `O_CREATE|O_EXCL|O_WRONLY`, streams `r` through `io.MultiWriter(file, md5Hash)`, calls `f.Sync()`, then closes.
7. If copy, sync, or close fails, remove the temp file and return the first error.
8. Compute lowercase hex digest from the hash.
9. If `ExpectedMD5` is non-empty and does not match the computed digest, remove the temp file and return `storage.ErrBadDigest` before `rename`.
10. Only after the digest gate passes, `os.Rename(tmp, target)` publishes the object.
11. Sidecar metadata is written only after the final object exists; digest mismatch must not write sidecar metadata.

## Error Contracts

- Malformed `Content-MD5`: `InvalidDigest`, HTTP 400.
- Well-formed but mismatched `Content-MD5`: `BadDigest`, HTTP 400.
- Storage-level digest mismatch is represented by `storage.ErrBadDigest` so `writeStorageError` can map it to `BadDigest`.
- Other filesystem errors continue to map to `InternalError` unless an existing storage sentinel applies.
- Error response messages must be present in `pkg/handlers/common.go:errorMessage` for both `BadDigest` and `InvalidDigest`.

## Atomicity And Cleanup

The existing temp-file + rename shape is the correct base and should not be replaced.

Required cleanup points:

- `io.Copy` error: remove temp file.
- `f.Sync` error: remove temp file after attempting close.
- `f.Close` error: remove temp file.
- digest mismatch: remove temp file before returning `ErrBadDigest`.
- `os.Rename` error: remove temp file.

Directory fsync trade-off:

- `f.Sync()` protects file contents before rename, but POSIX durability of the rename directory entry can require opening and syncing the parent directory.
- Adding best-effort parent directory fsync after successful rename improves crash durability, but a failure after rename cannot safely be handled by deleting the object because the object has already been published.
- Recommended implementation: if code is touched in this area, add a small best-effort or explicit `syncDir(filepath.Dir(target))` after successful rename and before returning success. If it returns an error, surface the error as an internal write failure only if the object publication semantics are still acceptable and tests are adjusted. Otherwise document that the project currently guarantees atomic visibility, not full crash-durable directory-entry persistence.

## Compatibility Notes

- Do not require every backend implementation to understand `ExpectedMD5`.
- Do not change multipart part upload behavior in this task. Multipart ETags are not raw object MD5 and are outside the requested `Content-MD5` single PUT scope.
- Do not apply request `Content-MD5` validation to `PUT Object - Copy` unless product scope explicitly expands; copy requests have no client body bytes to compare against.
- Do not read the full request body into memory. Integrity must remain streaming.

## Existing Worktree Findings To Reuse

During planning, related uncommitted code was observed:

- `pkg/storage/file_backend.go` already has `PutOptions.ExpectedMD5`, `ErrBadDigest`, and pre-rename digest comparison.
- `pkg/handlers/object.go` already has `parseContentMD5`, optional `PutObjectWithOptions` dispatch, `InvalidDigest` handling, and `BadDigest` storage error mapping.
- `pkg/handlers/common.go` already has `BadDigest` and `InvalidDigest` messages.
- `pkg/storage/integrity_test.go` and `pkg/server/ops_test.go` already contain partial coverage.

Implementation should verify and complete these changes, not duplicate them.
