# 子任务 4：分段上传与元数据 sidecar

> 父任务：`06-05-natives3-bridge`。实现 S3 Multipart Upload（在不违反 1:1 红线前提下临时分片+合并落地），以及自定义元数据/标签的 sidecar 存储。

## ⛔ 执行者硬约束
需求、临时分片策略、sidecar 格式为**冻结规格**。不得修改/删减/替换。**绝对红线**：合并产物必须是单个原生文件，临时分片必须隔离在隐藏目录并在完成/中止后清理。问题写 `research/change-request.md` 上报。详见父任务 `prd.md`。

---

## Goal

实现 S3 分段上传全流程（Create/UploadPart/Complete/Abort/ListParts），临时分片存于隐藏目录，CompleteMultipartUpload 时按序合并为单个原生文件落地，符合 1:1 映射红线。同时实现 `x-amz-meta-*` 自定义元数据与 Object Tagging 的 sidecar 文件存储（区别于 Rclone 丢弃元数据）。

## 依赖
- 子任务 2（backend、handler、路由）。
- 子任务 3（鉴权、配额——Complete 时按合并后总大小做配额提交）。

## Requirements

### A. 分段上传
1. `pkg/storage/multipart.go`：
   - **CreateMultipartUpload**：生成 `uploadID`（UUID），建临时目录 `multipart_tmp/{uploadID}/`，写 `manifest.json`（记录 bucket、key、content-type、x-amz-meta-*、tags、创建时间）。
   - **UploadPart**：把分片存为 `multipart_tmp/{uploadID}/part-{NNNNN}`（零填充编号保证排序），计算每片 MD5 作为 part ETag。校验 partNumber ∈ [1,10000]。
   - **CompleteMultipartUpload**：按客户端提交的 part 列表顺序，流式 `io.Copy` 合并到目标对象的临时文件→`fsync`→`rename` 落地为单个原生文件；写 sidecar 元数据；计算 multipart ETag（各 part MD5 拼接后再 MD5，追加 `-<partCount>`，符合 S3 约定）；删除临时目录。
   - **AbortMultipartUpload**：删除 `multipart_tmp/{uploadID}/`。
   - **ListParts / ListMultipartUploads**：基于临时目录与 manifest 列举。
2. 路由接入（S2 router）：
   - `POST /{bucket}/{key}?uploads` → Create
   - `PUT /{bucket}/{key}?partNumber=N&uploadId=ID` → UploadPart
   - `POST /{bucket}/{key}?uploadId=ID` (body=CompleteMultipartUpload XML) → Complete
   - `DELETE /{bucket}/{key}?uploadId=ID` → Abort
   - `GET /{bucket}/{key}?uploadId=ID` → ListParts
3. **GC**：启动一个后台 goroutine 定期（默认每 1h）清理超过 TTL（默认 24h）未完成的 `multipart_tmp/{uploadID}/`。TTL/间隔可配置（加到 config 的 storage 段；同步告知不破坏父 design——以 research 记录新增配置项并请规划者确认）。
4. 配额：Complete 成功落地后，用合并后文件总大小调用 `quota.Commit(OpPut)`；UploadPart 阶段不计 used_bytes（或计入临时占用但 Complete/Abort 时结算——默认 Complete 时一次性结算，记入 research）。

### B. 元数据 / 标签 sidecar
5. `pkg/storage/metadata.go`：
   - sidecar 路径 = `<对象路径> + metadata_suffix`（默认 `.s3meta`），与对象同目录。
   - JSON 格式：`{etag, content_type, metadata{x-amz-meta-*}, tags{}, size, uploaded_at}`。
   - **原子写**：临时文件 + rename。
   - PutObject（S2 的单段上传）接入：写对象后写 sidecar（S2 预留的 hook 点本期实现）。
   - GetObject/HeadObject：读取 sidecar，回填 `Content-Type`、`x-amz-meta-*` 响应头。
   - DeleteObject：同时删除 sidecar。
   - sidecar 缺失要容错（老文件/外部拷入的文件没有 sidecar 时，用扩展名推断 content-type，元数据为空，不报错）。
6. Object Tagging API：
   - `PUT /{bucket}/{key}?tagging`（body=Tagging XML）→ 写 tags 到 sidecar。
   - `GET /{bucket}/{key}?tagging` → 返回 Tagging XML。
   - `DELETE /{bucket}/{key}?tagging` → 清空 tags。
7. ListObjectsV2（S2）需继续把 `*.s3meta` 从对象列举中排除。

## 非目标
- 不做预签名、钩子（S5）。
- 不做对象版本控制。

## Acceptance Criteria

- [ ] `go build`/`go vet`/`go test ./pkg/storage/...` 通过。
- [ ] aws-cli 上传 120MB 文件（自动触发 multipart），完成后 `data_root/{bucket}/{key}` 为**单个原生文件**，可用文件管理器打开，内容与源 `diff` 一致。
- [ ] 上传过程中 `multipart_tmp/{uploadID}/` 存在分片；完成后该目录被删除（`.multipart` 下无残留）。
- [ ] Abort 后对应临时目录被删除。
- [ ] multipart 对象的 ETag 形如 `<hash>-<partCount>`。
- [ ] `aws s3api put-object --metadata author=jdoe,team=infra` 上传后，`head-object` 返回 `x-amz-meta-author`/`x-amz-meta-team`。
- [ ] `put-object-tagging` 后 `get-object-tagging` 原样返回标签。
- [ ] 外部直接拷入磁盘、无 sidecar 的文件，GET 仍可下载（content-type 用扩展名推断，不报错）。
- [ ] sidecar 文件不出现在 ListObjectsV2 结果中。
- [ ] GC：构造一个过期 uploadID 目录，触发清理后被删除。
- [ ] Complete 后 `credentials.used_bytes` 按合并后大小正确增加。

## Notes
- 分片合并必须流式，禁止把整文件读入内存。
- 临时目录命名与隐藏要确保 ListBuckets/ListObjects 不暴露。
- 新增的 GC 相关配置项需在 research 记录并请规划者确认后再固化到 config。
