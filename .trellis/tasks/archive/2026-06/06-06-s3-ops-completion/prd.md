# S3 协议补全：DeleteObjects、CopyObject 与桶探测子资源

## Goal

补全 aws-cli/SDK 高频依赖的 S3 操作，使 NativeS3-Bridge 在现有 Bucket 模型、原生文件映射、SigV4、配额、sidecar 元数据和事件钩子约束下，正确支持：

- `POST /{bucket}?delete` DeleteObjects 批量删除。
- `PUT /{bucket}/{key}` 携带 `x-amz-copy-source` 的 CopyObject 服务端拷贝。
- `GET /{bucket}?location` 与 `GET /{bucket}?versioning` 的 SDK 初始化/探测最小响应。

## User Value

- 让 `aws s3api delete-objects`、`aws s3api copy-object` 以及常见 SDK 启动探测不再失败或误判成功。
- 避免 CopyObject 当前返回 200 但写出 0 字节对象的静默数据错误。
- 保持本项目核心红线：Bucket 是一级目录，Object Key 是相对原生文件路径，最终对象仍是原始字节文件。

## Confirmed Facts

- Current route dispatch has no bucket-level `POST ?delete` branch. aws-cli实测 `delete-objects` 返回 `MethodNotAllowed`，退出码 `254`。
- Current route dispatch has no `x-amz-copy-source` branch. aws-cli实测 `copy-object` 退出码为 `0`，但目标 `copy.txt` 被普通 PutObject 写成 0 字节，ETag 为 `d41d8cd98f00b204e9800998ecf8427e`，不是源对象内容。
- Current bucket-level `GET ?location` and `GET ?versioning` fall through to `ListObjectsV2`. aws-cli实测退出码为 `0`，但 debug 响应体是 `<ListBucketResult>`，不是对应子资源 XML；这是 botocore 容错解析导致的表面成功。
- Existing `storage.FileBackend` already writes objects through temporary file + rename, stores sidecar metadata, removes sidecar on delete, returns `NoSuchBucket` for missing buckets, and treats missing object delete as success.
- Existing quota accounting commits `OpPut`, `OpGet`, and `OpDelete`; anonymous public-read applies only to object `GET`/`HEAD`, so these write/delete operations remain signed-only.
- Existing hook events support `ObjectCreated` and `ObjectDeleted`.

## Requirements

- DeleteObjects:
  - Accept signed `POST /{bucket}?delete` with AWS Delete XML payload.
  - Delete every requested key independently and return a valid `<DeleteResult>` XML.
  - Treat missing keys in an existing bucket as successful deleted entries, matching S3 idempotent delete behavior.
  - Reject malformed XML or missing key entries with `400 InvalidArgument`.
  - Preserve missing bucket semantics: missing bucket returns standard `404 NoSuchBucket` XML for the operation.
  - For existing deleted objects, decrement usage and emit `ObjectDeleted` events consistently with single-object delete.
  - Honor `Quiet=true` by omitting successful `<Deleted>` entries.
  - Ignore `VersionId` because this project does not implement object versioning.

- CopyObject:
  - Detect `x-amz-copy-source` before ordinary PutObject routing.
  - Parse source as `bucket/key` or `/bucket/key`, URL-unescape the path, and ignore unsupported source query parameters such as `versionId`.
  - Return `404 NoSuchBucket` or `404 NoSuchKey` for missing source/target bucket or source object using standard S3 XML mapping.
  - Copy object bytes server-side without loading the whole object into memory.
  - Preserve native file bytes, ETag, content type, user metadata, and existing object tags by default.
  - Return valid `<CopyObjectResult>` XML containing `LastModified` and quoted `ETag`.
  - Count copied bytes against the destination credential quota before writing, and commit `OpPut` only after successful copy.
  - Emit `ObjectCreated` for the destination object after successful copy.
  - Do not change multipart upload behavior or single-part PutObject behavior when `x-amz-copy-source` is absent.

- Bucket subresource probes:
  - `GET /{bucket}?location` must return valid `LocationConstraint` XML for the configured/default region. For `us-east-1`, AWS-compatible parsed result should remain `LocationConstraint: null`.
  - `GET /{bucket}?versioning` must return valid empty `VersioningConfiguration` XML representing versioning disabled/unconfigured.
  - Both probes must verify bucket existence and return `404 NoSuchBucket` for missing buckets.
  - Probe responses must not fall through to object listing.

- Compatibility and safety:
  - Keep all S3 errors through `handlers.WriteS3Error` and all success XML through shared XML response helpers.
  - Keep path traversal defenses centralized in storage path resolution.
  - Do not add new external dependencies.
  - Do not introduce database schema changes.

## Acceptance Criteria

- [ ] `aws s3api delete-objects --bucket <bucket> --delete 'Objects=[{Key=a.txt},{Key=missing.txt}],Quiet=false'` exits `0`, deletes `a.txt`, returns `<Deleted>` entries for both keys, and leaves missing-key deletion successful.
- [ ] Batch deleting existing objects decrements credential `used_bytes` only for objects that existed and emits delete hooks only for those objects.
- [ ] `aws s3api copy-object --bucket <bucket> --key copy.txt --copy-source <bucket>/source.txt` exits `0`; downloaded `copy.txt` byte-for-byte equals `source.txt`; HeadObject reports a non-empty copied size and the expected MD5 ETag.
- [ ] CopyObject preserves content type and user metadata; if source tags exist, `get-object-tagging` on the destination returns the same tags.
- [ ] CopyObject over quota fails with `403 QuotaExceeded`, does not create/overwrite the destination object, and does not commit usage.
- [ ] `aws s3api get-bucket-location --bucket <bucket>` exits `0` and receives a `LocationConstraint` XML response, not a `ListBucketResult` response.
- [ ] `aws s3api get-bucket-versioning --bucket <bucket>` exits `0` and receives a `VersioningConfiguration` XML response, not a `ListBucketResult` response.
- [ ] Missing bucket behavior for DeleteObjects, CopyObject source/target, location, and versioning returns standard S3 XML errors.
- [ ] Existing smoke coverage for PUT/GET/HEAD/ListObjectsV2/DeleteObject/Multipart/Tagging remains green.
- [ ] `go test ./...` passes.

## Out Of Scope

- Enabling or storing object versions.
- Implementing `PUT ?versioning`, `GET ?versions`, delete markers, or version-specific copy/delete behavior.
- Cross-region copy semantics, ACL/grant headers, SSE headers, website/cors/lifecycle/policy subresources.
- Metadata replacement directives beyond preserving source metadata by default, unless needed for basic aws-cli compatibility during implementation.
- Changing the public-read anonymous policy beyond current GET/HEAD object reads.

## Evidence

- `aws-cli/2.34.62` against current code on isolated local server:
  - `delete-objects`: `MethodNotAllowed` when calling `DeleteObjects`.
  - `copy-object`: exit `0`, but target object was 0 bytes with empty MD5 ETag.
  - `get-bucket-location`: request `GET /ops-bucket?location`, HTTP `200`, response body `<ListBucketResult ...>`.
  - `get-bucket-versioning`: request `GET /ops-bucket?versioning`, HTTP `200`, response body `<ListBucketResult ...>`.

## Open Questions

- None blocking. Recommended implementation preserves source metadata/tags on CopyObject and leaves advanced copy directives out of scope.
