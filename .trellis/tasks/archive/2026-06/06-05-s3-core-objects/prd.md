# 子任务 2：S3 核心对象操作与 1:1 原生映射

> 父任务：`06-05-natives3-bridge`。本子任务交付 S3 网关的主干：HTTP 服务、路径映射、核心对象 API。

## ⛔ 执行者硬约束
需求、接口、目录、路由形态为**冻结规格**。执行者不得修改/删减/替换。问题写入 `research/change-request.md` 上报。**绝对红线**（1:1 原生映射）违反即判不合格。详见父任务 `prd.md`。

---

## Goal

实现 S3 HTTP 服务与 1:1 原生文件映射存储后端，提供核心对象操作：PutObject、GetObject、HeadObject、DeleteObject、ListBuckets、ListObjectsV2、HeadBucket。上传的文件以原生形态落盘，可被系统文件管理器直接打开。本期**不做鉴权**（S3 接口先全放行，鉴权在 S3 任务接入；留好中间件挂载点）。

## 依赖
- 子任务 1（config、db、main 骨架）已完成。

## Requirements

1. `pkg/server`：基于 `net/http` 启动 S3 服务（监听 `server.s3_addr`），支持优雅关闭（SIGINT/SIGTERM）。`router.go` 解析 S3 风格路径并按 HTTP 方法分发到 handler。
   - 路由形态：`PUT/GET/HEAD/DELETE /{bucket}/{key...}`；`GET /{bucket}`（ListObjectsV2，识别 `list-type=2` 查询参数）；`GET /`（ListBuckets）；`HEAD /{bucket}`（HeadBucket）。
   - 预留中间件链：`auth → quota → handler`，本期 auth/quota 为 no-op 占位（由 S3 任务替换为真实实现）。
2. `pkg/storage`：
   - `path.go`：`ResolveBucketPath`、`ResolveObjectPath`；`path.Clean` 规整 + 逃逸检查（禁止越出 `data_root/bucket`）；bucket 命名校验（小写字母数字与连字符，3–63 字符）。
   - `backend.go`：定义 `Backend` 接口（Put/Get/Head/Delete/ListObjects/ListBuckets）。
   - 实现 `FileBackend`：
     - **PutObject**：`MkdirAll` 父目录 → 写入临时文件 `<target>.tmp-<random>` → `fsync` → `os.Rename` 原子落地为原生文件。计算 ETag（MD5，符合 S3 单段上传约定）。
     - **GetObject**：流式 `io.Copy` 返回，支持 `Range` 头（bytes=start-end）。
     - **HeadObject**：返回 size、ETag、last-modified、content-type。
     - **DeleteObject**：删除文件；空父目录可选清理（不强制）。
     - **ListObjectsV2**：扫描 bucket 目录，支持 `prefix`、`delimiter=/`（CommonPrefixes 模拟"目录"）、`max-keys`、`continuation-token`（基于已排序 key 的游标）。
     - **ListBuckets**：列举 `data_root` 下的一级目录（排除隐藏的 `.multipart`、数据库文件等）。
3. `pkg/handlers`：
   - `common.go`：`WriteS3Error(w, code, httpStatus, resource)` 输出标准 `<Error>` XML；统一 XML 编码辅助；标准响应头（`x-amz-request-id`、`Server`）。
   - `object.go` / `bucket.go`：把 HTTP 请求翻译为 Backend 调用，组装标准 S3 XML/头响应。
4. ETag、Content-Type（按上传头或扩展名推断）、Last-Modified 正确返回。
5. 大文件下载用流式拷贝，避免整文件载入内存。

## 非目标
- 不做 SigV4 鉴权、配额（占位中间件即可）。
- 不做 Multipart、元数据 sidecar、Tagging（S4 负责；但 PutObject 落地方式需与 S4 合并落地兼容）。
- 不做预签名、钩子、前端。

## Acceptance Criteria

- [ ] `go build ./...` / `go vet ./...` 通过。
- [ ] aws-cli（`--endpoint-url`）可 `cp` 上传文件，磁盘 `data_root/{bucket}/{key}` 出现**原名原后缀**原生文件，字节与源文件一致。
- [ ] aws-cli 可下载该文件，`diff` 与源一致；带 `Range` 的部分下载返回 206 与正确片段。
- [ ] `HeadObject` 返回正确 size/ETag/last-modified/content-type。
- [ ] `aws s3 ls s3://bucket/prefix/` 列举正确，`delimiter=/` 时返回 CommonPrefixes。
- [ ] `aws s3 ls`（无 bucket）列出 data_root 下目录作为 buckets，且不含 `.multipart` 等隐藏项。
- [ ] DeleteObject 后文件从磁盘消失，再 GET 返回 404 NoSuchKey（标准 XML）。
- [ ] 含 `..` 的恶意 key 被拒（400），无法越出 bucket 目录。
- [ ] 非法 bucket 名返回标准错误。
- [ ] 上传 50MB 文件时进程内存不出现整文件级暴涨（流式验证）。

## Notes
- PutObject 的"临时文件→rename"落地方式要与 S4 的 multipart 合并产物保持同一最终形态（单原生文件）。
- 错误码集合：NoSuchKey、NoSuchBucket、InvalidArgument、InvalidBucketName、InternalError 等，统一走 common.go。
