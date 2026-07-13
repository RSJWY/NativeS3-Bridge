# 定位并修复 HEAD SigV4 验签失败

## Goal

定位并修复生产环境中签名 `HEAD` 请求返回 `403 SignatureDoesNotMatch`、导致 S3 客户端无法创建目录占位对象的问题。

## Confirmed Facts

- 生产 Endpoint 使用 path-style；`ListObjectsV2`、`GET`、`DELETE` 能通过签名验证。
- 生产 `HeadObject` 与 `HeadBucket` 均返回 403；客户端 Canonical Request 使用标准 `HEAD`、正确 URI、`hk-1` scope 和空载荷 SHA-256。
- 临时生产凭证的 `PUT` 另行返回 `QuotaExceeded`，与 HEAD 验签失败是两个独立问题。
- 当前仓库源码在隔离环境中通过真实 AWS CLI：`HeadBucket` 成功、缺失对象 `HeadObject` 返回 404、零字节目录对象 PUT 后 `HeadObject` 成功。
- 生产 Nginx access log 记录客户端方法为 `HEAD`，而 NativeS3 服务日志记录同一失败请求的方法为 `GET`。
- 受控签名探针确认：标准 HEAD 签名返回 403；将 Canonical Request method 改为 `GET`、但实际仍发送 `HEAD` 时返回 200。
- 根因是 Nginx 代理缓存的 HEAD 转 GET 行为。SigV4 将 HTTP method 纳入签名，因此上游收到 GET 后必然验签失败。

## Requirements

1. 在 Nginx 部署示例中禁用 S3 API 代理缓存及 `proxy_cache_convert_head`。
2. 使用 `$http_host` 保持客户端签名中的 Host，包括非标准端口。
3. 文档解释 HEAD 转 GET 会破坏 SigV4，并提醒检查面板生成的额外 include 配置。
4. 文档明确已有部署需要重写/重新保存宿主机反代配置，且不需要重新构建 NativeS3-Bridge 镜像。
5. 增加 HEAD SigV4 回归测试，证明原始 HEAD 可验签，而代理将 method 改为 GET 后返回 `SignatureDoesNotMatch`。
6. 保持服务端鉴权实现和 S3 响应行为不变。

## Acceptance Criteria

- [x] README S3 Nginx location 包含 `proxy_cache off` 与 `proxy_cache_convert_head off`。
- [x] README 明确说明 SigV4 method 签名与 HEAD 转 GET 故障表现。
- [x] README 明确说明已有部署必须更新宿主机反代配置，无需重建镜像。
- [x] 正确签名的 HEAD 请求通过验证。
- [x] 同一请求在 method 被代理改为 GET 后返回 `SignatureDoesNotMatch`。
- [x] 现有 GET/PUT/DELETE/presigned 行为保持不变。
- [x] 目标测试、全量测试、vet 与 build 通过。

## Out of Scope

- 直接登录或修改生产主机配置。
- 调整临时凭证配额。
- 在客户端绕过 HEAD 检查。

## Notes

- 生产配置应在 S3 `location` 中加入 `proxy_cache off; proxy_cache_convert_head off;`，执行 `nginx -t` 后 reload，再用 `head-bucket` 与 `head-object` 复测。
