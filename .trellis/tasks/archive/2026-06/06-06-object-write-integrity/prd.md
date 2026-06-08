# 对象写入原子性与完整性校验

## Goal

单段 `PutObject` 必须以本地临时文件写入、`fsync`、`rename` 的方式原子落盘，并在客户端提供可校验 digest 时拒绝损坏内容，避免半写对象和静默损坏。

## User Value

- 上传被中断、写盘失败或校验不一致时，不留下被当作成功对象读取的半截文件。
- 客户端发送 `Content-MD5` 或具体 `x-amz-content-sha256` payload hash 时，服务端会验证真实写入字节并按 S3 语义返回 `BadDigest`。
- 成功 PUT 返回的单段 ETag 继续是对象字节的 lowercase hex MD5，保持现有 S3 客户端行为。

## Confirmed Facts

- `pkg/storage/file_backend.go` 的 `PutObjectWithMetadata` 当前已经解析目标路径、创建父目录、写 `<target>.tmp-<random>`、对临时文件 `fsync`、关闭后 `os.Rename` 到最终路径。
- `PutObjectWithMetadata` 当前在 `io.Copy(io.MultiWriter(f, h), r)` 过程中计算 MD5，并把 lowercase hex MD5 写入 sidecar 与 `ObjectInfo.ETag`。
- `CopyObject` 当前同样使用临时文件、`fsync`、`rename`，并重新计算目标对象 MD5 ETag。
- `pkg/handlers/object.go` 的 `Put` 当前只向 backend 传递 request body、`Content-Type` 和 user metadata；没有解析或透传 `Content-MD5`、`x-amz-content-sha256`。
- `pkg/handlers/common.go` 当前没有 `BadDigest` 错误消息，`writeStorageError` 当前没有 checksum/integrity error 映射。
- `pkg/auth/sigv4.go` 当前把 `x-amz-content-sha256` 用作 SigV4 canonical payload hash 输入；历史决策明确支持具体 hash 和 `UNSIGNED-PAYLOAD` 以兼容 aws-cli。
- 现有 storage spec 要求单段 `PutObject` 写原生字节、临时文件落盘、MD5 ETag；尚未记录请求 checksum 校验契约。

## Requirements

- Preserve the existing atomic write contract for single-part `PutObject`: write the request stream once to a random temp file under the target path, compute digests while streaming, `fsync`, close, and rename only after all validations pass.
- `Content-MD5` validation: when the PUT request includes `Content-MD5`, validate it as base64-encoded MD5 bytes for the uploaded object body. If the decoded value does not match the computed MD5, return S3 XML error code `BadDigest` and do not replace/create the final object.
- `x-amz-content-sha256` validation: when the PUT request includes a concrete 64-character hex SHA-256 payload hash, validate it against the uploaded object body. If it does not match, return `BadDigest` and do not replace/create the final object.
- `x-amz-content-sha256` compatibility sentinels such as `UNSIGNED-PAYLOAD` must remain accepted for SigV4 compatibility but must not be treated as checksum values to compare against object bytes.
- Malformed checksum headers should be rejected before or during PUT without committing usage or emitting ObjectCreated hooks. Prefer standard S3 checksum/digest error codes instead of `InternalError`.
- On checksum mismatch, temporary files must be removed and any pre-existing final object at the destination key must remain unchanged.
- Successful PUT behavior must remain unchanged for clients that do not send checksum headers: native bytes are stored, sidecar metadata is written, ETag is lowercase MD5 hex, HTTP ETag is quoted, usage is committed, and ObjectCreated hook is emitted.
- `CopyObject`, multipart upload part writes, and multipart complete behavior are out of this task unless a direct regression is discovered while touching shared code.

## Acceptance Criteria

- [ ] Unit coverage proves `PutObject`/`PutObjectWithMetadata` keeps temp-file + `fsync` + rename behavior and returns lowercase MD5 ETag for successful writes.
- [ ] Handler-level PUT with matching `Content-MD5` succeeds and returns the expected quoted MD5 ETag.
- [ ] Handler-level PUT with mismatched `Content-MD5` returns HTTP 400 S3 XML code `BadDigest`, leaves no new object for a new key, and does not commit usage or emit ObjectCreated.
- [ ] Handler-level overwrite with mismatched `Content-MD5` returns `BadDigest` and preserves the previous object bytes and ETag.
- [ ] Handler-level PUT with matching concrete `x-amz-content-sha256` succeeds; mismatched concrete SHA-256 returns `BadDigest` without final object replacement.
- [ ] PUT with `x-amz-content-sha256: UNSIGNED-PAYLOAD` continues to work as today and is not checksum-compared.
- [ ] Existing CopyObject, metadata/tagging, quota, and storage tests continue to pass.

## Out Of Scope

- Multipart checksum request headers and AWS checksum algorithms such as `x-amz-checksum-sha256`, `x-amz-checksum-crc32`, or trailing checksums.
- SigV4 streaming payload modes such as `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` unless the current server already accepts them elsewhere.
- Changing the persisted ETag format or adding S3 multipart-style ETags to single-part PUT.
- Hardening crash consistency of parent directories after rename beyond the existing file `fsync` + rename contract.

## Open Questions

- Confirm whether malformed `Content-MD5` should return `InvalidDigest` while mismatches return `BadDigest`, or whether all malformed/mismatched checksum failures should return `BadDigest` for the first implementation.
