# 对象写入原子性与完整性校验（临时文件+rename，Content-MD5/ETag 校验）

## Goal

确保单段 PUT 对象写入具备 S3 风格的请求体完整性保护：客户端提供 `Content-MD5` 时，服务端必须在对象发布前校验实际落盘字节的 MD5，不匹配时返回明确的 S3 XML 错误且不留下可见对象或临时残留。

本任务不是重写文件后端。现有 `pkg/storage/file_backend.go` 已采用写 `<target>.tmp-<random>`、`O_EXCL`、流式 `io.Copy` 同时计算 MD5、`f.Sync()`、`Close()`、`os.Rename()` 的原子落盘形态，多段合并的 `WriteCompletedObject` 也走 `rename`。本任务目标是在该基础上补齐并验证 `Content-MD5` 校验、错误映射和清理路径。

## Confirmed Facts

- `storage.Backend` 当前只暴露 `PutObject/GetObject/HeadObject/DeleteObject/ListObjects/ListBuckets`，不能为本任务强制修改既有接口签名。
- `FileBackend.PutObject` 已委托到 `PutObjectWithMetadata`，实际写入集中在 `PutObjectWithOptions` 或等价内部实现时最适合承载校验逻辑。
- 单段对象 ETag 合同是对象字节 MD5 的 lowercase hex，HTTP handler 边界负责加引号。
- `ObjectHandler.Put` 是普通 PUT 对象入口；服务端复制 `PUT Object - Copy` 由 `object_ops.go` 的 `Copy` 路径处理，不应被本任务的 `Content-MD5` 请求体校验误伤。
- `pkg/server/router.go` 的 Quota 中间件只基于请求 `Content-Length` 或 `x-amz-decoded-content-length` 预检查 PUT body 大小，不消费 body，和本任务的流式 MD5 校验不冲突。
- 目前工作树中已出现与本任务相关的未提交实现痕迹：`PutOptions.ExpectedMD5`、`parseContentMD5`、`BadDigest/InvalidDigest` 错误文案、以及部分 storage/server 测试。实施阶段必须先复核这些改动是否完整、是否属于本任务、是否需要补强，而不是重复造轮子或推倒重写。

## Requirements

- 普通 PUT 对象在请求头包含 `Content-MD5` 时必须校验该头。
- `Content-MD5` 的合法格式为 base64 编码的原始 16 字节 MD5 digest；非法 base64 或解码后长度不是 16 字节必须返回 S3 错误码 `InvalidDigest`，HTTP 400。
- 校验必须对实际写入的对象字节计算 MD5，且必须发生在最终 `rename` 发布对象之前。
- MD5 不匹配必须返回 S3 错误码 `BadDigest`，HTTP 400。
- MD5 不匹配时不得留下目标对象、sidecar、或 `<target>.tmp-*` 临时文件。
- 未携带 `Content-MD5` 时普通 PUT 行为必须与现状保持一致：继续写入对象、返回 MD5 ETag，不强制校验。
- 既有 `storage.Backend` 接口不可破坏；新增写入选项必须通过可选接口断言或兼容扩展实现。
- 复核并加固 `FileBackend` 单段写入的错误清理路径：copy/sync/close 错误、digest mismatch、rename 错误均应清理临时文件。
- 复核目录项持久性：如果项目选择加强目录 fsync，必须保持失败处理安全，且不能导致已发布对象被误删；若不实现目录 fsync，必须在设计中记录原因和剩余风险。
- 不引入新依赖，仅使用 Go 标准库与现有项目依赖。
- 不改变 multipart 上传完成语义，除非复核发现其错误清理路径需要与本任务一致的最小加固。
- 本任务不自行创建 git commit。

## Acceptance Criteria

- [ ] `Content-MD5` 匹配的 PUT 返回 200，落盘对象字节正确，ETag 等于对象 MD5 hex。
- [ ] `Content-MD5` 不匹配的 PUT 返回 HTTP 400 XML，`Code` 为 `BadDigest`。
- [ ] `Content-MD5` 不匹配后，目标对象不存在，metadata sidecar 不存在，对象目录下没有本次写入留下的 `.tmp-` 文件。
- [ ] `Content-MD5` 非 base64 或 base64 解码后不是 16 字节时返回 HTTP 400 XML，`Code` 为 `InvalidDigest`，且不创建对象或临时文件。
- [ ] 未携带 `Content-MD5` 的 PUT 仍正常成功，行为与现有测试期望一致。
- [ ] 后端单元测试覆盖匹配成功、不匹配失败且无残留。
- [ ] handler 或 server 路由测试覆盖 `BadDigest`、`InvalidDigest`、无头成功路径；测试应经过真实 `ObjectHandler.Put` 或 `Router`，验证 S3 错误码。
- [ ] `go test ./...` 通过。
- [ ] `go build ./...` 通过。
- [ ] 关键并发/写入路径如有改动，至少运行相关包 `go test -race ./pkg/storage ./pkg/handlers ./pkg/server` 或记录无法运行的原因。

## Notes

- Scope is a single deliverable. No parent/child task split is needed because storage, handler, and route tests are one integrated acceptance target.
- Keep implementation minimal and preserve current native-file storage contract: final object is the original byte stream, not a wrapped/container format.
- Treat existing uncommitted changes in related files as shared work: read and preserve them; do not revert unrelated edits.
