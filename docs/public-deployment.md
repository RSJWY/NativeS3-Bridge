# 公网安全部署与监控

公网部署要把 S3 API 和管理后台视为不同安全边界。

推荐拓扑：

```text
Internet
  |
  | HTTPS
  v
Reverse proxy / CDN / WAF
  |-- s3.example.com    -> node S3 listener :9000
  |-- admin.example.com -> panel admin listener :9001

node ──outbound mTLS──▶ panel agent listener :9443
```

## 基本原则

- 所有公网入口必须使用 HTTPS。
- S3 API 和管理后台使用不同域名，便于独立 cookie、限流、WAF 和日志策略。
- 管理后台公网访问不要只依赖单密码。建议启用 TOTP 和 captcha。
- `admin_addr` 尽量绑定内网地址，公网只通过反向代理访问。
- `trust_forwarded` 只在可信代理覆盖转发头时启用。
  - panel 的管理面开关是 **顶层** `trust_forwarded`（`configs/panel.example.yaml`），不是单体版的 `rate_limit.trust_forwarded`——panel 没有 S3 数据面，该键只决定登录失败锁定按谁的 IP 计数。默认 `false`。
  - 不开启时按 TCP 来源地址计数：管理面在反代之后时，所有登录尝试都会算到反代这一个 IP 上，锁定会波及全部管理员。挂了反代就应当开启。
  - 取值口径是 `X-Forwarded-For` 的**最后一段**，与下面示例里 `$proxy_add_x_forwarded_for`（追加模式）配套：客户端伪造的值留在左侧被忽略，最右一段由反代写入。因此**不要**把反代改成只透传客户端原值，那样伪造的 IP 会被采信，登录失败锁定形同虚设。
- 业务直链优先使用 private bucket + 短 TTL presigned URL。
- `public-read` 只用于明确对所有知道 URL 的人公开的对象。
- panel 的 9443 端口只用于 node 注册和控制面连接，应使用正确的服务端证书并限制无关来源。

## Nginx 反向代理示例

```nginx
server {
    listen 443 ssl http2;
    server_name s3.example.com;

    ssl_certificate     /etc/letsencrypt/live/s3.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/s3.example.com/privkey.pem;

    client_max_body_size 0;

    location / {
        proxy_pass http://127.0.0.1:9000;
        proxy_http_version 1.1;
        proxy_cache off;
        proxy_cache_convert_head off;
        proxy_set_header Host $http_host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}

server {
    listen 443 ssl http2;
    server_name admin.example.com;

    ssl_certificate     /etc/letsencrypt/live/admin.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/admin.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:9001;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
```

> **已有反向代理部署必须更新配置：** NativeS3-Bridge 镜像或二进制不会自动修改宿主机 Nginx 配置。升级或迁移后，应重新编辑/保存 S3 站点的反向代理块，确保 `location /` 包含 `proxy_cache off`、`proxy_cache_convert_head off` 和 `proxy_set_header Host $http_host`，然后执行 `nginx -t` 并 reload；无需为此重新构建 NativeS3-Bridge 镜像。

S3 SigV4 会把 HTTP method 纳入签名。Nginx 的代理缓存配置可能把客户端 `HEAD` 转成上游 `GET`，导致 `HeadObject`/`HeadBucket` 返回 `SignatureDoesNotMatch`，而 PUT/GET/DELETE 仍然正常。使用宝塔等面板生成配置时，还要检查额外 include 文件是否重新启用了 `proxy_cache` 或覆盖 `proxy_cache_convert_head off`。修复后，Nginx access log 与 NativeS3 `s3 request` 日志应同时记录 `HEAD`。

> **严禁对 S3 数据面启用任何代理缓存。** 这不只是 HEAD 改写问题：Nginx `proxy_cache` 的默认缓存键是 `scheme$host$request_uri`，**不含 `Authorization`**——一个用户签名下载的私有对象被缓存后，匿名请求同一 URI 会直接命中缓存拿到内容，等价于私有桶公开。S3 站点的正确做法是彻底关闭缓存，而不是只修 `proxy_cache_convert_head`。
>
> 宝塔等面板生成的站点配置中，缓存/加速功能可能由插件以 include（如 `vhost/nginx/extension/<域名>/*.conf`）或模块方式启用，不一定出现在主 vhost 文件里；残留的 `proxy_cache_path` 定义说明这类功能被打开过，应与缓存目录一并删除。判定时直接对照两侧日志：Nginx access log 记的是客户端发来的方法，NativeS3 `s3 request` 日志记的是网关实际收到的方法，二者不一致即中间层改写了请求。
>
> 排障时可用客户端报错里的 request id 反查：`docker logs <node容器> 2>&1 | grep <req-...>`，命中行的 `reason`/`code` 即失败原因（`verify_failed`/`SignatureDoesNotMatch` 通常意味着中间层改写了签名覆盖的 method、path、query 或 header）。

若使用 Docker 部署并希望日志落盘，`log.dir` 必须指向已挂载的卷内路径（如 `/data/logs`）；指向容器内未挂载的路径（如 `/state/logs`）会导致日志只存在于容器层，宿主机不可见、重建即丢失。此时排查仍可用 `docker logs`（所有日志同时输出到 stdout）。

若下发给 node 的策略启用了 `rate_limit.trust_forwarded`，必须确保 node 不能被绕过代理直接访问。

## 公网生产检查清单

- `panel -check-config -config configs/panel.yaml` 与 `node -check-config -config configs/node.yaml` 均已通过。
- panel 主密钥、中间 CA、agent 服务端证书和 node CA 信任链均已备份并验证。
- HTTPS 已在应用或可信反向代理终止。
- `webadmin.password_hash` 已配置。
- `webadmin.admin_bootstrap_password` 已清空。
- `webadmin.session_secret` 已替换为随机值。
- `webadmin.totp.enabled: true`。
- `webadmin.captcha.enabled: true`，或有明确的内网/反代替代防护。
- `rate_limit.trust_forwarded` 仅在可信反代后启用。
- 日志不记录 Authorization、Cookie、captcha token、session secret、完整 presigned URL 或对象内容。
- public-read bucket 中只有明确公开的对象。

## 运维端点与监控

容器 healthcheck 的真实现状：

- panel Compose 使用 `panel -check-config`。该检查会读取主密钥和在线 CA 并校验配置字段，但不是请求级 liveness/readiness 探针，也不能替代完整启动检查。
- node Compose 使用 `node -health`。该探测会对已配置的 S3 listener 发起真实请求，属于进程级 liveness 信号。
- node `-health` 每次探测都是一次匿名 `GET /`，网关会按预期返回 403 并记录一条 `s3 auth denied reason=credentials_required` WARN（默认每 30s 一条）。这是健康检查的正常副产物，不是认证故障；真正的客户端认证失败看 `reason=verify_failed` 等非 `credentials_required` 的行。

不要按旧单体 README 暴露 `/healthz`、`/readyz` 或 `/metrics`；当前 panel 管理服务器没有注册这些旧端点。

除 healthcheck 外，生产监控还应覆盖：

- panel 与 node 容器状态、重启次数和退出码。
- panel 9001、9443 监听状态，以及 node 9000 S3 探测。
- panel 中节点的 online、last heartbeat、applied/desired version 和 drift 状态。
- panel/node 日志中的注册失败、证书错误、任务失败和数据库迁移错误。
