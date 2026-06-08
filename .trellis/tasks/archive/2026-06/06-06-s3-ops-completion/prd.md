# S3 协议补全：DeleteObjects 批量删除与 CopyObject 服务端拷贝

## Goal

补全 aws-cli/SDK 高频依赖的 S3 操作：`POST /{bucket}?delete` 批量删除、`PUT` 携带 `x-amz-copy-source` 的服务端拷贝，以及 `?location` / `?versioning` 等 SDK 初始化探测子资源的最小响应。

## Confirmed Facts

- 实现已随 commit `dc47f8b`（分支 `06-06-protocol-observability`）落地并推送；本 PRD 为事后补写，使其与已交付代码一致并据此归档。
- 落点：`pkg/handlers/object_ops.go`（Copy / DeleteObjects / parseCopySource / deleteErrorCode）、`pkg/handlers/bucket.go`（GetBucketLocation / GetBucketVersioning）、`pkg/server/router.go`（PUT copy 分发、POST `?delete` 分发、GET `?location`/`?versioning` 子资源路由）、`pkg/handlers/common.go`（`MalformedXML` / `InvalidRequest` 错误文案）。
- 测试：`pkg/server/ops_test.go` 经真实 `Router` 覆盖 Copy、批量 Delete、桶子资源。
- 服务端拷贝复用既有 `GetObject` + `PutObjectWithOptions` 流式管道，不把对象读入内存；与 06-06-object-write-integrity 的 `PutObjectWithOptions` 写入路径共享。

## Requirements

### CopyObject（PUT Object - Copy）

- `PUT /{bucket}/{key}` 当请求头含 `x-amz-copy-source` 时识别为服务端拷贝（`IsCopyRequest`），不消费请求 body。
- `x-amz-copy-source` 解析支持可选前导 `/`、URL 转义、以及 `?versionId=` 后缀剥离；格式非法返回 `InvalidArgument` HTTP 400。
- 源对象不存在映射为标准存储错误（`NoSuchKey` / `NoSuchBucket`）。
- 默认从源对象拷贝 content-type 与用户元数据；当 `x-amz-metadata-directive: REPLACE` 时改用请求头的 `Content-Type` 与 `x-amz-meta-*`。
- 同桶同键自我拷贝且非 REPLACE 时返回 `InvalidRequest` HTTP 400。
- 拷贝写入的目标字节计入配额：因无 body，Quota 中间件无法预检，需在 handler 内对认证非匿名身份执行 `quota.Check`，超限返回 `QuotaExceeded` HTTP 403。
- 成功返回 `200` 与 `CopyObjectResult`（含 quoted ETag 与 LastModified），并提交用量、发 `ObjectCreated` 事件。

### DeleteObjects（POST /{bucket}?delete）

- 解析 `Delete` XML 请求体（限制读取上限 8MiB）；XML 非法或对象列表为空返回 `MalformedXML` HTTP 400。
- 逐键删除：删除前 `HeadObject` 取得大小用于配额回退；缺键（`ErrNoSuchKey`）按 S3 语义视为成功，不报错。
- 单键失败计入结果的 `<Error>`（含映射后的 Code），不中断其余键。
- 成功删除的非零大小对象回退用量（`OpDelete`）并发 `ObjectDeleted` 事件。
- 支持 `Quiet` 模式：安静模式下不在响应中列出成功删除项。
- 返回 `200` 与 `DeleteResult`（`Deleted` / `Error` 列表）。

### 子资源探测

- `GET /{bucket}?location`：校验桶存在后返回空 `LocationConstraint`（客户端解读为 us-east-1）。
- `GET /{bucket}?versioning`：校验桶存在后返回空 `VersioningConfiguration`（未启用版本控制）。

### 约束

- 不引入新依赖，仅用 Go 标准库与现有项目依赖。
- 服务端拷贝必须流式，不得整体读入内存。
- 复用既有存储接口与错误映射（`writeStorageError`）；不破坏 `storage.Backend` 接口。

## Acceptance Criteria

- [x] `PUT` + `x-amz-copy-source` 拷贝已存在对象返回 200 与 `CopyObjectResult`，目标对象字节与大小同源对象。
- [x] 拷贝源不存在返回 404（`NoSuchKey`/`NoSuchBucket`）；`x-amz-copy-source` 格式非法返回 400 `InvalidArgument`。
- [x] 同桶同键非 REPLACE 自我拷贝返回 400 `InvalidRequest`；`REPLACE` 指令下允许并替换元数据。
- [x] 拷贝超配额返回 403 `QuotaExceeded`。
- [x] `POST /{bucket}?delete` 批量删除多个键返回 200 与 `DeleteResult`，列出已删除键；不存在的键不致整体失败。
- [x] `Delete` XML 非法或为空返回 400 `MalformedXML`。
- [x] `GET /{bucket}?location` 返回 200 `LocationConstraint`；`GET /{bucket}?versioning` 返回 200 `VersioningConfiguration`。
- [x] `pkg/server/ops_test.go` 经真实 `Router` 覆盖 Copy、批量 Delete、`?location`/`?versioning` 子资源。
- [x] `go build ./...` 通过。
- [x] `go test ./...` 通过。

## Notes

- 与 `06-06-object-write-integrity` 同批提交（commit `dc47f8b`）；二者共享 `common.go`、`router.go`、`ops_test.go`，故未拆分提交。
- `?location`/`?versioning` 仅为 SDK 初始化探测提供最小合法响应，不代表实现了 Region 选择或版本控制能力。
