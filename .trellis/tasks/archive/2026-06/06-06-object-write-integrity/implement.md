# Implementation Plan

## Checklist

- [x] Add storage checksum error sentinels, likely `ErrBadDigest` and optionally `ErrInvalidDigest`, in the storage layer so handlers can map them without string matching.
- [x] Add `PutObjectOptions` and `PutObjectWithOptions` to `FileBackend`, preserving existing `PutObject` and `PutObjectWithMetadata` as delegating methods.
- [x] Update `PutObjectWithOptions` to compute MD5 always and SHA-256 when an expected SHA-256 is provided, compare expected digests before `os.Rename`, remove temp files on validation failure, and keep sidecar/ETag behavior unchanged on success.
- [x] Add handler checksum parsing for `Content-MD5` and concrete `x-amz-content-sha256`; ignore known compatibility sentinels such as `UNSIGNED-PAYLOAD` for checksum comparison.
- [x] Extend `writeStorageError` and `errorMessage` for `BadDigest` and, if chosen, `InvalidDigest`.
- [x] Add storage tests for checksum success, mismatch temp cleanup, and overwrite preservation.
- [x] Add handler tests for `Content-MD5` success/mismatch, SHA-256 success/mismatch, `UNSIGNED-PAYLOAD` compatibility, no usage commit/hook on mismatch, and overwrite preservation.
- [x] Run targeted tests: `go test ./pkg/storage ./pkg/handlers`.
- [x] Run broader regression if targeted tests pass: `go test ./...`.

## Validation Commands

- `go test ./pkg/storage ./pkg/handlers`
- `go test ./...`

## Risky Files

- `pkg/storage/file_backend.go`: must not move validation after `os.Rename`; checksum mismatch must never replace an existing object.
- `pkg/handlers/object.go`: must not commit quota or emit hooks when checksum validation fails.
- `pkg/handlers/common.go`: error code mapping must remain S3 XML and avoid leaking internal details.
- `pkg/auth/sigv4.go`: avoid changing SigV4 authentication behavior in this task.

## Review Gate Before Start

- Confirm the malformed digest error-code decision in `prd.md` open questions.
- Confirm the scope remains single-part PUT only, excluding multipart/trailing checksum algorithms.
