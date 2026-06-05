# 子任务 2 设计：S3 核心对象操作与 1:1 原生映射

> 仅细化本子任务。全局以父任务 `design.md` 为准。

## 1. 路由分发（pkg/server/router.go）

S3 路径无前缀。解析规则：
```
GET    /                      → ListBuckets
HEAD   /{bucket}              → HeadBucket
GET    /{bucket}             → ListObjectsV2 (要求 list-type=2，否则按 V1 兼容降级或返回)
PUT    /{bucket}/{key...}    → PutObject
GET    /{bucket}/{key...}    → GetObject (支持 Range)
HEAD   /{bucket}/{key...}    → HeadObject
DELETE /{bucket}/{key...}    → DeleteObject
```
- `{key...}` 保留内部 `/`，映射为子目录。
- 中间件链：`Recover → Logging → Auth(占位) → Quota(占位) → dispatch`。占位中间件签名固定，S3 任务替换实现而不改签名：
  ```go
  type Middleware func(http.Handler) http.Handler
  // 占位 Auth/Quota: 直接 next.ServeHTTP，但已把 chain 顺序固定
  ```

## 2. Backend 接口（pkg/storage/backend.go）

```go
type ObjectInfo struct {
    Key          string
    Size         int64
    ETag         string
    LastModified time.Time
    ContentType  string
}

type ListResult struct {
    Objects        []ObjectInfo
    CommonPrefixes []string
    IsTruncated    bool
    NextToken      string
}

type Backend interface {
    PutObject(bucket, key string, r io.Reader, contentType string) (ObjectInfo, error)
    GetObject(bucket, key string, rng *Range) (io.ReadCloser, ObjectInfo, error)
    HeadObject(bucket, key string) (ObjectInfo, error)
    DeleteObject(bucket, key string) error
    ListObjects(bucket, prefix, delimiter, token string, maxKeys int) (ListResult, error)
    ListBuckets() ([]BucketInfo, error)
}
```

## 3. 路径映射与安全（pkg/storage/path.go）

```go
func ResolveBucketPath(root, bucket string) (string, error) // 校验 bucket 名
func ResolveObjectPath(root, bucket, key string) (string, error) {
    p := filepath.Join(root, bucket, filepath.FromSlash(path.Clean("/"+key)))
    // 确认 p 在 filepath.Join(root, bucket) 之内，否则 ErrInvalidPath
}
```
- bucket 名正则：`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`。
- 逃逸检查：清理后用 `strings.HasPrefix(absPath, absBucketDir+sep)`。

## 4. PutObject 原子落地

```
tmp := target + ".tmp-" + randomHex
f, _ := os.Create(tmp); io.Copy(io.MultiWriter(f, md5hash), r); f.Sync(); f.Close()
os.Rename(tmp, target)        // 原子替换；失败则清理 tmp
etag := hex(md5hash)          // 单段对象 ETag = MD5
```
- 父目录 `os.MkdirAll(filepath.Dir(target), 0o755)`。
- Content-Type：优先请求头 `Content-Type`，否则 `mime.TypeByExtension`。

## 5. GetObject + Range
- 解析 `Range: bytes=a-b`，用 `os.File.Seek` + `io.CopyN`，返回 206 + `Content-Range`。
- 无 Range：200 + 全量流式 `io.Copy`。
- 始终设 `Content-Length`、`ETag`、`Last-Modified`、`Content-Type`、`Accept-Ranges: bytes`。

## 6. ListObjectsV2
- 遍历 `data_root/bucket`（`filepath.WalkDir`），相对路径转为 key（`/` 分隔）。
- `prefix` 过滤；`delimiter=/` 时把 prefix 之后第一个 `/` 前的段聚合为 CommonPrefixes。
- 结果按 key 升序排序后，用 `continuation-token`（= 最后一个 key 的编码）做游标分页，`max-keys` 默认 1000。
- **跳过隐藏辅助项**：`.multipart` 目录、`*.s3meta` sidecar（S4 引入）、数据库文件。

## 7. 标准响应（pkg/handlers/common.go）
- `<Error><Code>..</Code><Message>..</Message><Resource>..</Resource><RequestId>..</RequestId></Error>`。
- ListBuckets / ListObjectsV2 用 `encoding/xml` 输出 S3 标准结构（`ListAllMyBucketsResult` / `ListBucketResult`）。
- 统一响应头：`x-amz-request-id`（随机）、`Server: NativeS3-Bridge`。

## 8. 与 S4 的兼容约定
- PutObject 落地的最终文件就是用户原生文件；S4 的 multipart 合并产物落地到同一路径、同一形态。
- sidecar 元数据本期不写；S4 接入后 Put 也要写 sidecar，预留 hook 点（在 backend.PutObject 完成后调用可选 metadata writer，本期传 nil）。
