# Implement — aws-chunked 解码

## 落地顺序

### S1 解码器（可独立测试，无外部依赖）

1. 新建 `pkg/awschunked/reader.go`
   - `Reader`、`NewReader(r io.Reader, expectedSize int64, algos []string)`、`Read`、`Trailers`
   - `ErrMalformedChunk` / `ErrSizeMismatch` / `ErrChecksumMismatch`
   - 单块上限常量（64 MiB）、trailer 行数与行长上限
   - 滚动哈希按 `algos` 惰性启用：crc32 IEEE / crc32c Castagnoli / sha1 / sha256
2. 新建 `pkg/awschunked/reader_test.go`，覆盖 design.md §6 的单元清单
3. 门禁：`go test -count=1 ./pkg/awschunked/`

`crc32c` 需要 `hash/crc32` 的 `crc32.MakeTable(crc32.Castagnoli)`，标准库即可，不引新依赖。

### S2 中间件接入

1. `pkg/server/awschunked.go`
   - `isAwsChunkedRequest(r *http.Request) bool`
   - `trailerAlgorithms(r *http.Request) []string`（解析 `x-amz-trailer`）
   - `AwsChunked(next http.Handler) http.Handler`：替换 `r.Body`、覆写 `r.ContentLength`、清理 `Content-Encoding` 中的 `aws-chunked`
2. `pkg/server/router.go:57` 的 `chain` 插入 `AwsChunked`，位置在 `Auth(...)` 与 `quotaMiddleware` 之间
   - 注意 `newRouter` 是三个构造函数的公共入口（`NewRouter` / `NewRouterWithQuotaManager` / `NewManagedRouterWithQuotaManager`），改一处即三种部署形态全覆盖
3. `pkg/server/awschunked_middleware_test.go`：判定矩阵 + 头清理 + 非法 declared 长度
4. 门禁：`go test -count=1 ./pkg/server/`

### S3 错误映射

1. `pkg/handlers/common.go:93` 附近的消息表新增 `IncompleteBody`
2. `pkg/handlers/object.go:455` `writeStorageError` 新增三个 `awschunked.*` 分支
3. `pkg/handlers/multipart.go` 的 `writeMultipartStorageError` 同步
4. 确认 `errors.Is` 能穿透 `FileBackend` 的包装；若 `FileBackend` 用了 `fmt.Errorf` 不带 `%w`，需修正为可穿透（只改包装方式，不改写入语义）
5. 门禁：`go test -count=1 ./pkg/handlers/`

### S4 端到端测试

1. `pkg/server/router_test.go` 追加 design.md §6 的端到端用例
2. 必须包含"非法框架 → 断言 code != QuotaExceeded"这条回归断言
3. 复用既有辅助函数 `newServerTestDB` / `headerSignedRequest` / `stubAuthenticator`（`pkg/server/router_test.go:1030` 起）
4. 门禁：`go test -count=1 ./...`

### S5 spec 与文档

1. `.trellis/spec/backend/auth-quota-guidelines.md`
   - Contracts 增补：aws-chunked 判定条件；配额与统计以解码后长度为准；`quotaLimitReadCloser` 只能包裹解码后的流
   - 错误矩阵增补：框架错误 → 400 `IncompleteBody`；trailer 校验和不匹配 → 400 `BadDigest`
   - Bad case 增补：用解码后长度截断未解码流导致误报 `QuotaExceeded`
   - 已知限制：`chunk-signature` 不验证
2. `.trellis/spec/backend/storage-guidelines.md`：写入路径收到的是解码后字节；解码错误不得留下 `.tmp-*`
3. 门禁：`go build ./... && go vet ./... && go test -count=1 ./... && git diff --check`

## 不要做

- 不改 `FileBackend` 的写入/sidecar 语义（R6）
- 不迁移或改写历史对象（R6）
- 不实现 `chunk-signature` 验证（本任务范围外，记入 spec 限制）
- 不动 SigV4 验签逻辑（那是 #3 的范围）
- 不碰前端（那是 #1 的范围）

## 完成判据

`prd.md` 的 Acceptance Criteria 全部勾选，且 `go test -count=1 ./...` 全绿。
