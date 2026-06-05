# 子任务 4 设计：分段上传与元数据 sidecar

> 仅细化本子任务。全局以父任务 `design.md` 为准。

## 1. 临时分片布局

```
{multipart_tmp}/
└── {uploadID}/
    ├── manifest.json        # {bucket, key, content_type, metadata{}, tags{}, created_at}
    ├── part-00001           # 各分片原始字节
    ├── part-00002
    └── ...
```
- `uploadID` = UUIDv4（无 Date/random 限制问题，用库生成）。
- part 文件名零填充 5 位，保证字典序 == 数字序。

## 2. Multipart 接口（pkg/storage/multipart.go）

```go
type MultipartStore interface {
    Create(bucket, key, contentType string, meta map[string]string, tags map[string]string) (uploadID string, err error)
    UploadPart(uploadID string, partNumber int, r io.Reader) (etag string, err error)
    Complete(uploadID string, parts []CompletedPart) (ObjectInfo, error)   // parts 已按 PartNumber 排序校验
    Abort(uploadID string) error
    ListParts(uploadID string) ([]PartInfo, error)
}
```

### Complete 合并落地
```
target := ResolveObjectPath(root, bucket, key)
tmp := target + ".merge-" + uploadID
out, _ := os.Create(tmp)
for _, p := range sortedParts {
    in, _ := os.Open(partPath(uploadID, p.PartNumber))
    io.Copy(io.MultiWriter(out, fullHash?), in)   // 流式
}
out.Sync(); out.Close()
os.MkdirAll(filepath.Dir(target)); os.Rename(tmp, target)
writeSidecar(target, meta, tags, multipartETag, size)
os.RemoveAll(multipartDir(uploadID))
```
- multipart ETag：`md5( concat(每片的 md5 原始字节) )` 的 hex + `"-" + len(parts)`。
- part 校验：客户端提交的每个 part 的 ETag 与服务端存的 part MD5 一致，否则 `InvalidPart`；PartNumber 必须连续递增。

## 3. Sidecar 元数据（pkg/storage/metadata.go）

```go
type Sidecar struct {
    ETag        string            `json:"etag"`
    ContentType string            `json:"content_type"`
    Metadata    map[string]string `json:"metadata"`  // 不含 x-amz-meta- 前缀，取回时再加
    Tags        map[string]string `json:"tags"`
    Size        int64             `json:"size"`
    UploadedAt  string            `json:"uploaded_at"` // RFC3339 UTC
}

func WriteSidecar(objPath, suffix string, s Sidecar) error  // 原子写
func ReadSidecar(objPath, suffix string) (Sidecar, bool, error)  // 第二返回值=是否存在
func DeleteSidecar(objPath, suffix string) error
```
- 写：`objPath+suffix+".tmp"` → rename。
- 读缺失：返回 `exists=false`，调用方降级（扩展名推断 content-type）。

## 4. 路由接入（扩展 S2 router 的查询参数分发）

```
POST   /{bucket}/{key}?uploads                       → Create   → 返回 InitiateMultipartUploadResult(UploadId)
PUT    /{bucket}/{key}?partNumber=N&uploadId=ID       → UploadPart → 返回 ETag header
POST   /{bucket}/{key}?uploadId=ID  (body XML)        → Complete  → CompleteMultipartUploadResult
DELETE /{bucket}/{key}?uploadId=ID                    → Abort     → 204
GET    /{bucket}/{key}?uploadId=ID                    → ListParts → ListPartsResult
PUT    /{bucket}/{key}?tagging  (body XML)            → PutObjectTagging
GET    /{bucket}/{key}?tagging                        → GetObjectTagging
DELETE /{bucket}/{key}?tagging                        → DeleteObjectTagging
```
- router 需先看 query 参数再决定走普通对象操作还是 multipart/tagging 分支。

## 5. 配额结算时机
- UploadPart 不计 used_bytes。
- Complete 成功后：`quota.Check`（用合并后总大小，超限则拒绝并清理临时目录）→ 落地 → `quota.Commit(OpPut, totalSize)`。
- 注意：Check 在合并前做，避免合并大文件后才发现超限浪费 I/O；总大小 = 各 part size 之和（manifest/分片可得）。

## 6. GC（后台清理）
```go
// 启动一个 goroutine（main 装配）
ticker := every(cfg.Storage.MultipartGCInterval)  // 默认 1h
for range ticker {
    遍历 multipart_tmp/*：读 manifest.created_at，超过 cfg.Storage.MultipartTTL（默认24h）→ RemoveAll
}
```
- 新增 config 项 `storage.multipart_gc_interval`、`storage.multipart_ttl`（记入 research 请规划者确认后固化）。

## 7. 与 S2 PutObject 的 sidecar 接入
- S2 PutObject 完成后调用 `WriteSidecar`，把请求头里的 `x-amz-meta-*`、`Content-Type` 落入 sidecar。
- GetObject/HeadObject 读 sidecar 回填响应头：`Content-Type`、每个 metadata key 加 `x-amz-meta-` 前缀输出。
