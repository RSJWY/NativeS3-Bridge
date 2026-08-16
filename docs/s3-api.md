# S3 API 参考

node 在 `server.s3_addr`（默认 `0.0.0.0:9000`）暴露 S3 兼容数据面。本文覆盖 AWS CLI 用法、支持范围、签名版本、预签名 URL、匿名访问、错误格式和对象事件 Webhook。

## AWS CLI 环境变量

```bash
export AWS_ACCESS_KEY_ID=TESTKEY
export AWS_SECRET_ACCESS_KEY=TESTSECRET
export AWS_DEFAULT_REGION=us-east-1
EP="--endpoint-url http://127.0.0.1:9000"
```

## 常用操作

```bash
# 创建 bucket
aws $EP s3 mb s3://mybucket

# 上传对象
aws $EP s3api put-object \
  --bucket mybucket \
  --key docs/readme.txt \
  --body ./README.md \
  --metadata author=alice,project=demo

# 查看对象 metadata
aws $EP s3api head-object --bucket mybucket --key docs/readme.txt

# 列举对象
aws $EP s3api list-objects-v2 --bucket mybucket --prefix docs/

# 下载对象
aws $EP s3api get-object --bucket mybucket --key docs/readme.txt ./download.txt

# Range 下载
aws $EP s3api get-object \
  --bucket mybucket \
  --key docs/readme.txt \
  --range bytes=0-99 \
  ./partial.txt

# 删除对象
aws $EP s3api delete-object --bucket mybucket --key docs/readme.txt
```

## 支持范围

| 类别 | 操作 |
|---|---|
| Service | `GET /`，ListBuckets |
| Bucket | `PUT /{bucket}`、`DELETE /{bucket}`、`HEAD /{bucket}`、`GET /{bucket}` |
| Bucket probes | `GET /{bucket}?location`、`GET /{bucket}?versioning` |
| List objects | `ListObjectsV2`，支持 `prefix`、`delimiter`、`continuation-token`、`max-keys` |
| Object | `PUT`、`GET`、`HEAD`、`DELETE` |
| Object copy | `PUT` + `x-amz-copy-source` |
| Bulk delete | `POST /{bucket}?delete` |
| Multipart | Create、UploadPart、Complete、Abort、ListParts、ListMultipartUploads |
| Tagging | `PUT/GET/DELETE /{bucket}/{key}?tagging` |
| Metadata | `x-amz-meta-*` 自定义 metadata |
| Integrity | `Content-MD5` 校验，失败返回 `InvalidDigest` 或 `BadDigest` |
| Auth | Header SigV4 和 query presigned URL；可选 SigV2（`auth.allow_sigv2`，默认关闭） |
| Anonymous | public-read bucket 的对象级 `GET`/`HEAD` |

不支持或不属于当前目标：

- AWS IAM policy、bucket policy、ACL XML 兼容写接口。
- S3 versioning 的真实版本存储。
- Object Lock、SSE、Lifecycle、Replication。
- 匿名列 bucket、匿名写入、匿名删除。

## 签名版本

默认仅接受 Signature Version 4（header 或 query presigned）。旧客户端若只能发 Signature Version 2，可在 node 配置中显式开启：

```yaml
auth:
  allow_sigv2: true   # 默认 false
```

注意：v2 用 HMAC-SHA1、不签请求体、无 region/service scope，安全性明显弱于 v4；v2 + aws-chunked 的 payload 完整性完全依赖 aws-chunked 解码器校验。仅在必须兼容仅支持 v2 的客户端时开启。

## 预签名 URL

业务服务应优先使用 private bucket 加短 TTL 预签名 URL 暴露用户直链：

```bash
aws $EP s3 presign s3://mybucket/docs/readme.txt --expires-in 300
```

服务端会按 query SigV4 校验 `X-Amz-*` 参数。不要把完整预签名 URL 写入日志，因为 query string 中包含签名材料。

## public-read 直链与匿名访问矩阵

`public-read` bucket 只允许匿名对象级读取：

```bash
curl -I http://127.0.0.1:9000/public-bucket/path/file.txt
curl -o file.txt http://127.0.0.1:9000/public-bucket/path/file.txt
```

匿名访问矩阵：

| 请求 | private | public-read |
|---|---:|---:|
| `GET /bucket/key` | 403 | 200 或对象错误 |
| `HEAD /bucket/key` | 403 | 200 或对象错误 |
| `GET /bucket` list | 403 | 403 |
| `PUT/DELETE/POST` | 403 | 403 |
| `?tagging`、multipart 子资源 | 403 | 403 |

## 错误格式

S3 API 错误统一返回标准 XML：

```xml
<Error>
  <Code>AccessDenied</Code>
  <Message>access denied</Message>
  <Resource>/bucket/key</Resource>
  <RequestId>req-...</RequestId>
</Error>
```

每个 S3 响应都会带 `x-amz-request-id`，该 ID 也会出现在错误 XML 和访问日志中。

## 事件钩子（Webhook）

Hook manager 从 node 数据库的 `hook_configs` 表加载启用的 Webhook 配置。对象创建、对象删除和 multipart complete 会投递事件。

事件示例：

```json
{
  "type": "ObjectCreated",
  "bucket": "mybucket",
  "key": "docs/readme.txt",
  "size": 1234,
  "etag": "5d41402abc4b2a76b9719d911017c592",
  "metadata": {
    "author": "alice"
  },
  "credential_id": 1,
  "timestamp": "2026-06-19T12:00:00Z"
}
```

投递规则：

- 投递为异步后台任务，不阻塞 S3 响应。
- 队列满会丢弃事件并记录 warning。
- 非 2xx、连接失败或超时会按 `hooks.max_retry` 指数退避重试。
- 禁用的 hook config 不会投递。

Panel 节点详情页和 `/api/admin/nodes/{id}/webhooks` 已提供 webhook 草稿 CRUD，事件类型使用 `ObjectCreated` / `ObjectDeleted` 显式选择。修改只有在管理员发布草稿且 node 成功应用后才替换运行时 hook 集合；不要直接修改 node 的 `hook_configs` 绕过 Panel 权威状态。
