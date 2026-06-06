# S3 Ops Completion Design

## Architecture And Boundaries

- `pkg/server/router.go` owns method/subresource dispatch only. It should distinguish `?delete`, `?location`, `?versioning`, and `x-amz-copy-source` before falling back to existing ListObjectsV2 or PutObject behavior.
- `pkg/handlers/bucket.go` owns bucket-level XML responses and bucket existence checks for probe subresources.
- `pkg/handlers/object.go` owns DeleteObjects and CopyObject HTTP contracts, XML request/response shapes, quota checks for copy size, usage commits, and hook emission.
- `pkg/storage` owns native byte movement and sidecar preservation. Path validation must continue through `ResolveBucketPath`/`ResolveObjectPath` rather than handler-level filesystem joins.

## Route Dispatch

Bucket-level requests where `key == ""`:

- `POST` + `hasQuery(req, "delete")` -> `ObjectHandler.DeleteObjects`.
- `GET` + `hasQuery(req, "location")` -> `BucketHandler.GetBucketLocation`.
- `GET` + `hasQuery(req, "versioning")` -> `BucketHandler.GetBucketVersioning`.
- Existing `GET ?uploads` and ListObjectsV2 behavior remain after these specific probe checks.

Object-level requests where `key != ""`:

- `PUT` + non-empty `x-amz-copy-source` -> `ObjectHandler.Copy`.
- `PUT` without `x-amz-copy-source` -> existing `ObjectHandler.Put`.
- Existing tagging and multipart branches keep priority before normal object operations.

## DeleteObjects Contract

Request XML:

```xml
<Delete>
  <Object><Key>a.txt</Key><VersionId>ignored</VersionId></Object>
  <Quiet>false</Quiet>
</Delete>
```

Response XML:

```xml
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Deleted><Key>a.txt</Key></Deleted>
</DeleteResult>
```

Processing flow:

- Decode XML from `r.Body` with `encoding/xml`.
- Reject malformed XML, empty object list, or empty keys with `InvalidArgument`.
- For each key, call `HeadObject` first to determine whether the object exists and to capture size/metadata for usage and hooks.
- If `HeadObject` returns `ErrNoSuchKey`, still call/delete idempotently or record success without usage/hook emission.
- If `HeadObject` returns `ErrNoSuchBucket`, fail the whole operation with `NoSuchBucket`.
- Call `DeleteObject` for each key. Storage continues to own path validation and sidecar removal.
- Commit `OpDelete` only for objects that existed, using negative size. Emit `ObjectDeleted` only for objects that existed.
- If `Quiet` is false, include successful `<Deleted>` entries for all requested keys, including missing keys.

## CopyObject Contract

Request:

- `PUT /{destBucket}/{destKey}`.
- Header `x-amz-copy-source: source-bucket/path/to/source.txt` or `/source-bucket/path/to/source.txt`.

Response XML:

```xml
<CopyObjectResult>
  <LastModified>2026-06-06T12:00:00.000Z</LastModified>
  <ETag>"md5hex"</ETag>
</CopyObjectResult>
```

Processing flow:

- Parse copy source with URL path unescape, trim leading slash, split on first slash, and strip any `?versionId=...` suffix from the source key.
- Validate that source bucket and key are non-empty; otherwise return `InvalidArgument`.
- Ask storage to copy source to destination using native streaming and atomic final rename.
- Before the destination write, check quota against the source object size because the request body length is normally 0 for CopyObject.
- After successful copy, commit `OpPut` for destination bytes and emit `ObjectCreated`.
- Return `CopyObjectResult` with destination LastModified and quoted ETag.

Storage design:

- Add a narrow optional storage capability, implemented by `FileBackend`, for copying an object while preserving sidecar data:

```go
CopyObject(srcBucket, srcKey, dstBucket, dstKey string) (ObjectInfo, error)
```

- The implementation should:
  - `HeadObject` source first for size, ETag, content type, and user metadata fallback.
  - Resolve source and destination paths through existing storage path helpers.
  - Ensure destination parent directory exists.
  - Stream from source file to a destination temp file while computing MD5.
  - `fsync`, close, and rename to the final destination path.
  - Read source sidecar when present and copy metadata/tags/content type. Missing sidecar falls back to `HeadObject` metadata and empty tags.
  - Write destination sidecar after native file rename.
  - Return destination `ObjectInfo` from the written file.

This mirrors existing PutObject/Multipart atomic write patterns and avoids loading whole objects into memory.

## Bucket Probe Contracts

`GetBucketLocation`:

- Verify bucket exists by calling `ListObjects(bucket, "", "", "", 0)` or an equivalent backend existence check.
- Return `LocationConstraint` XML with the configured region except `us-east-1` represented as an empty element so aws-cli parses `null`.
- The current handler stack does not pass config region into `BucketHandler`; the minimal implementation can return empty `LocationConstraint` because project defaults and examples use `us-east-1`. If region-aware output is added, thread region into `NewBucketHandler` without changing storage.

`GetBucketVersioning`:

- Verify bucket exists.
- Return empty `VersioningConfiguration` XML to represent versioning disabled/unconfigured.
- Do not create bucket metadata or database rows for versioning.

## Quota And Hooks

- DeleteObjects uses the same signed identity context and `commitUsage` behavior as single DeleteObject.
- CopyObject must not rely on middleware content length because copy requests have no object body. The handler performs a quota check using source size before invoking storage copy.
- CopyObject commits `OpPut` only after successful native copy and sidecar write.
- CopyObject emits `ObjectCreated` for destination; DeleteObjects emits `ObjectDeleted` only for objects that existed.

## Compatibility Notes

- `VersionId` fields in DeleteObjects and `versionId` in copy source are accepted but ignored.
- Advanced CopyObject headers such as ACL grants, SSE, storage class, website redirect, and metadata replacement are out of scope.
- Existing PutObject, ListObjectsV2, multipart, tagging, presigned URL, and anonymous public-read behavior should be unchanged.

## Operational And Rollback Considerations

- No database migrations or config changes are required.
- Rollback is code-only: removing the new route branches and storage copy method restores previous behavior.
- The riskiest path is CopyObject overwriting an existing destination; implementation must write to a temp file and rename only after the source stream succeeds.
