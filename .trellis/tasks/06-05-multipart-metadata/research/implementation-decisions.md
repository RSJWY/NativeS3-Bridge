# Multipart / Metadata Implementation Records

Date: 2026-06-05

## Quota settlement timing

- `UploadPart` does not call quota `Check` or `Commit`; temporary parts are not counted in `credentials.used_bytes`.
- `CompleteMultipartUpload` computes the merged size from submitted part files before merging, runs `quota.Check(identity, totalSize)`, and only then performs the native merge.
- After successful native merge + sidecar write, it calls `quota.Commit(OpPut, totalSize)`.
- If the complete-time quota check fails, the implementation aborts the multipart upload and deletes `multipart_tmp/{uploadID}/`, matching the child design note that over-quota Complete rejects and cleans temporary data.

## GC configuration items

- Added storage config keys:
  - `storage.multipart_gc_interval` (`time.Duration`, default `1h`)
  - `storage.multipart_ttl` (`time.Duration`, default `24h`)
- Existing `storage.multipart_tmp` continues to point at the hidden temporary root; if omitted, it defaults to `<data_root>/.multipart`.
- These additions extend the storage config without changing the parent task's 1:1 native mapping constraints. Planner confirmation is requested for these new keys per the S4 PRD note.

## Multipart ETag algorithm validation

- Each `UploadPart` computes the part ETag as lowercase hex MD5 of that part's raw bytes.
- `CompleteMultipartUpload` recomputes each submitted part's MD5, validates it against the client's ETag (quotes tolerated), concatenates the raw 16-byte MD5 sums in part order, hashes that concatenation with MD5, and returns `<hex>-<partCount>`.
- Storage unit test `TestMultipartCompleteMergesInOrderAndWritesMultipartETag` verifies merge order, final bytes, sidecar persistence, temp directory cleanup, and multipart ETag format/content.
