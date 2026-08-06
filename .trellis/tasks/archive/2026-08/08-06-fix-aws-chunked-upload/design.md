# Design — aws-chunked 请求体解码

## 1. 当前数据流与断点

```
Recover → Logging → RateLimit → Auth → quotaMiddleware → dispatch → ObjectHandler.Put → FileBackend
                                            │                              │
                                            │                              └─ 直接写 r.Body（含分块框架）← 故障 B
                                            └─ quotaLimitReadCloser{remaining: 解码后长度} 包裹未解码流 ← 故障 A
```

两个故障共用一个成因：**中间件知道解码后长度，但流仍是编码态。**

## 2. 方案选择

考虑过三种放置解码的位置：

| 方案 | 位置 | 评价 |
|---|---|---|
| A | 新中间件，置于 `quotaMiddleware` 之前 | **采用。** 解码先于配额，配额天然作用在真实字节上；`dispatch` 及所有 handler 无需改动即可覆盖 PutObject 与 UploadPart |
| B | 在各 handler 内部包裹 `r.Body` | 需改 `Put` / `UploadPart` 两处，且配额中间件仍在上游看到编码态长度，故障 A 不能自动消解 |
| C | 在 `FileBackend` 内解码 | 违反 R6（存储层不应懂 HTTP 传输编码），且配额层依旧错 |

选 A。中间件链改为：

```go
chain: []Middleware{Recover, Logging, rateLimitMiddleware, Auth(...), AwsChunked, quotaMiddleware}
```

置于 `Auth` **之后**的理由：SigV4 的 `PayloadHash` 读的是 `x-amz-content-sha256` 头而非 body（`pkg/auth/sigv4.go:123-132`），且 STREAMING-* 场景下该头就是字面量，验签不消费 body，因此解码放在 Auth 后不影响验签。放在 Auth 前反而会让未认证请求付出解码代价，扩大匿名攻击面。

## 3. 新增组件

### 3.1 `pkg/awschunked`（新包）

放独立包而不是塞进 `pkg/server`，因为它是纯粹的流解码器，需要能被 `pkg/handlers` 测试直接引用，且与 HTTP 中间件解耦。

```go
package awschunked

// Reader 解码 aws-chunked 传输编码流。
type Reader struct { ... }

// NewReader 返回解码 r 的 Reader。expectedSize 为
// x-amz-decoded-content-length；-1 表示未声明，此时不校验总长。
func NewReader(r io.Reader, expectedSize int64) *Reader

func (r *Reader) Read(p []byte) (int, error)

// Trailers 返回终止分块后出现的 trailer 头，仅在 Read 返回 io.EOF 后有效。
func (r *Reader) Trailers() map[string]string

var (
	ErrMalformedChunk   = errors.New("malformed aws-chunked encoding")
	ErrSizeMismatch     = errors.New("decoded length does not match declared length")
	ErrChecksumMismatch = errors.New("trailer checksum mismatch")
)
```

分块语法（两种形式都要支持）：

```
<hex-len>\r\n<data>\r\n                                  # 无签名
<hex-len>;chunk-signature=<64 hex>\r\n<data>\r\n          # 签名分块
0\r\n                                                     # 终止分块
0;chunk-signature=<64 hex>\r\n                            # 签名终止分块
<name>:<value>\r\n ... \r\n                               # 可选 trailer 段
```

实现要点：

- 用 `bufio.Reader` 逐行读长度行；`;` 后的参数一律解析后丢弃。**不验证 `chunk-signature`**——验证需要沿分块链维护滚动签名并复用 SigV4 派生密钥，属于独立的安全增强；当前 v4 头部签名已保证请求头完整性，而分块签名缺失只影响 body 完整性，风险等级低于当前的数据损坏 bug。此决定必须写入 spec 的已知限制。
- 十六进制长度行需设上限（如单块 ≤ 64 MiB），防御恶意超长声明导致的内存放大。
- 每块数据后必须恰好是 `\r\n`，否则 `ErrMalformedChunk`。
- `expectedSize >= 0` 时，累计解码字节超出即 `ErrSizeMismatch`；读到终止分块时不足也是 `ErrSizeMismatch`。
- trailer 段解析到空行结束；trailer 数量与单行长度同样设上限。

### 3.2 校验和校验

trailer 中的 `x-amz-checksum-*` 需要和边读边算的摘要比对。`Reader` 内部按需维护 CRC32(IEEE)、CRC32C(Castagnoli)、SHA1、SHA256 的滚动哈希——由构造时传入的"待校验算法集合"决定启用哪些，避免无谓开销。算法集合来自请求头 `x-amz-trailer`。

值编码：AWS 用 base64。CRC32/CRC32C 是 4 字节大端的 base64，SHA1/SHA256 是原始摘要的 base64。识别不了的算法名直接跳过（R4）。

### 3.3 中间件 `server.AwsChunked`

```go
func AwsChunked(next http.Handler) http.Handler
```

- 判定：`isAwsChunkedRequest(r)` —— `x-amz-content-sha256` ∈ STREAMING-* 集合，或 `Content-Encoding` 含 `aws-chunked`（大小写不敏感、逗号分隔取值）。
- 命中后：
  - `declared := r.Header.Get("x-amz-decoded-content-length")`，解析失败 → `400 InvalidArgument`；缺失则传 `-1`。
  - `r.Body = awschunked.NewReadCloser(r.Body, declared, trailerAlgos)`
  - 覆写 `r.ContentLength = declared`（declared ≥ 0 时），使下游一切按解码后长度工作。
  - 从 `Content-Encoding` 中移除 `aws-chunked` 值（若移除后为空则删除该头），避免它被当作对象元数据存进 sidecar。
- 未命中：原样透传，零开销。

## 4. 配额路径改动

`contentLengthForQuota`（`pkg/server/router.go:458`）保持不变——因为 `AwsChunked` 已经把 `r.ContentLength` 覆写成解码后长度，`x-amz-decoded-content-length` 分支与之一致，两条路径殊途同归。

`quotaLimitReadCloser` 保留但语义变正确：它现在包裹的是**解码后**的流，`remaining` 与之同一坐标系，因此"读超即 ErrQuotaExceeded"重新成立（防御客户端少报 `x-amz-decoded-content-length` 骗过预检，即既有 `TestRouterQuotaManagerUnderreportedPutPreservesObject` 覆盖的场景）。

包裹顺序：`AwsChunked` 在外先解码，`quotaMiddleware` 在内后限长。即 `quotaLimitReadCloser{reader: awschunked.Reader{...}}`。

## 5. 错误映射

解码错误需在 handler 层转成正确的 S3 错误码。`writeStorageError`（`pkg/handlers/object.go:455`）新增分支：

| 解码错误 | S3 错误码 | HTTP |
|---|---|---|
| `awschunked.ErrMalformedChunk` | `IncompleteBody` | 400 |
| `awschunked.ErrSizeMismatch` | `IncompleteBody` | 400 |
| `awschunked.ErrChecksumMismatch` | `BadDigest` | 400 |
| `quota.ErrQuotaExceeded` | `QuotaExceeded`（不变） | 403 |

`pkg/handlers/common.go:93` 的错误消息表同步新增 `IncompleteBody`。`writeMultipartStorageError` 做同样处理。

关键：这些错误由 `FileBackend` 读流时冒泡上来，落盘失败路径已有的 temp 清理逻辑必须保证不留 `.tmp-*` 残留（`storage-guidelines.md:33` 已有同类契约，复用即可）。

## 6. 测试计划

`pkg/awschunked/reader_test.go`（单元）：
- 单块 / 多块 / 空对象（仅终止分块）
- 带 `chunk-signature=` 与不带
- 有 trailer / 无 trailer
- CRC32、CRC32C、SHA1、SHA256 匹配与不匹配
- 未知算法名忽略
- 非法长度行、缺 CRLF、块长与实际不符、超长声明
- `expectedSize` 过大 / 过小 / 未声明
- 分片 `Read`（小 buffer 多次读）与一次性 `ReadAll` 结果一致

`pkg/server/awschunked_middleware_test.go`（中间件）：
- 命中/未命中判定矩阵（STREAMING-* 三值 × Content-Encoding 有无）
- `Content-Encoding` 清理后不进 sidecar
- 非法 `x-amz-decoded-content-length` → 400

`pkg/server/router_test.go`（端到端，对应验收标准）：
- 带 / 不带 `x-amz-decoded-content-length` 的 aws-chunked PUT，断言落盘字节、ETag、`used_bytes`
- trailer CRC32 不匹配 → 400 BadDigest 且无残留
- 非法框架 → 400 且**断言响应码不是 QuotaExceeded**（这是回归本 issue 的关键断言）
- aws-chunked UploadPart + Complete 全流程
- 配额确实不足 → 403 QuotaExceeded 仍成立

## 7. 风险

- **误判非 chunked 请求。** 若判定过宽（例如只看 `Content-Encoding` 存在），普通 PUT 会被错误解码而全面损坏。判定必须严格按 R1 的两个条件，并有未命中矩阵测试兜底。
- **`Content-Encoding` 清理过度。** 客户端可能发 `aws-chunked, gzip`；只能移除 `aws-chunked` 这一个值，保留其余。
- **分块签名不验证。** 属已知限制，需在 spec 明确记录，避免后续误认为已具备 body 完整性保证。
