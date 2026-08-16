# 管理后台与 Panel Admin API

panel 管理后台默认监听 `http://127.0.0.1:9001/`（示例容器内为 `0.0.0.0:9001`）。管理后台是单管理员模型，不提供多用户、RBAC 或 OIDC。它管理节点和期望状态，不直接访问 node 的对象目录。

## 登录流程

登录 API：

```http
POST /api/admin/login
```

请求体：

```json
{
  "password": "admin-password",
  "totp_code": "123456",
  "captcha_token": "provider-token"
}
```

- `totp_code` 仅在 `webadmin.totp.enabled=true` 时需要。
- `captcha_token` 仅在 `webadmin.captcha.enabled=true` 时需要。
- 登录失败、TOTP 错误、captcha 失败都会计入同一来源 IP 的失败锁定。
- 登录成功后设置 `natives3_admin_session` HTTP-only cookie。

首次启动用 `webadmin.admin_bootstrap_password` 生成 bcrypt hash：启动日志输出 hash 后，把它写入 `password_hash` 并清空 bootstrap password。

前端可读取非敏感登录设置：

```http
GET /api/admin/auth-settings
```

该接口只返回是否需要 TOTP、是否启用 captcha、captcha provider 和 site key，不返回 secret。

## Admin API 一览

除 `/api/admin/login` 和 `/api/admin/auth-settings` 外，所有 `/api/admin/*` API 都需要 session cookie。主要节点作用域接口如下：

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET/POST` | `/api/admin/nodes` | 列出或创建逻辑节点。 |
| `GET/PATCH/DELETE` | `/api/admin/nodes/{id}` | 查看、启用/禁用或永久退役节点；退役会撤销证书和未使用令牌，但不会自动停止 node 的 S3 进程。 |
| `POST` | `/api/admin/nodes/{id}/tokens` | 签发一次性、默认 10 分钟有效的注册令牌；明文只返回一次。 |
| `GET/POST` | `/api/admin/nodes/{id}/credentials` | 列出或创建该节点的 S3 credential；secret 只在创建响应中返回一次。 |
| `POST` | `/api/admin/nodes/{id}/credentials/{accessKey}/rotate` | 轮换 secret；新 secret 只返回一次。 |
| `POST` | `/api/admin/nodes/{id}/desired-state` | 从 panel 权威数据生成新版本并在节点在线时尽力立即下发。 |
| `POST` | `/api/admin/nodes/{id}/desired-state/push` | 向在线节点重推当前期望状态。 |
| `POST` | `/api/admin/nodes/{id}/tasks` | 下发日志查询、存储扫描或存储对账等一次性任务。 |
| `GET` | `/api/admin/nodes/{id}/tasks/{taskId}` | 查询任务结果。 |
| `GET` | `/api/admin/nodes/{id}/certs` | 查看 node 客户端证书，含剩余天数（`days_until_expiry`）与四态状态（active/expiring/expired/revoked）。 |
| `POST` | `/api/admin/nodes/{id}/certs/revoke` | 撤销该节点的全部证书并断开控制面连接。 |
| `GET` | `/api/admin/logs` | 查看 Panel 自身的内存 ring、当前日志文件和安全枚举的轮转历史。 |

Webhook 草稿 CRUD 见 `/api/admin/nodes/{id}/webhooks`，行为约定见 [S3 API 参考 · 事件钩子](s3-api.md#事件钩子webhook)。

## 证书生命周期

- node 客户端证书默认 90 天（`pki.client_cert_ttl`），剩余有效期低于 TTL/3（默认 30 天）时自动经 `POST /renew` 续期，无需人工干预；已过期的证书只能令牌重注册，无宽限期。
- panel 服务端证书默认 825 天，到期用 `install-panel.sh renew-server-cert` 重签（不影响已注册节点，需重启 panel 生效），**不要用 `--force`**。
- 部署 CA 默认 3650 天，不可轮换，到期只能全网重装（已知限制 L1）。

完整 runbook（到期巡检、过期节点恢复七步、多 SAN 重签、CA 已知限制）见
[多节点运维文档 §10 Certificate lifecycle operations](multi-node-operations.md#10-certificate-lifecycle-operations)。

## Panel 日志

登录后可从侧栏进入 `/logs` 查看 Panel 自身日志。页面和
`GET /api/admin/logs` 共用同一契约，支持级别、关键字、条数和日志文件选择；
轮转历史包括 lumberjack 生成的普通文件与 gzip 文件。该接口只接受服务端枚举的
文件 ID，不接受路径，也不提供下载、删除或实时流。

Panel 与 Node 始终同时写 stdout 和最近 2000 条内存 ring。配置 `log.dir` 后还会
写入 `<dir>/natives3bridge.log` 并按 lumberjack 轮转；旧 `log.file` 完整路径仍兼容，
但不能和 `log.dir` 同时设置：

```yaml
log_level: "info"
log:
  dir: "/data/logs"
  max_size_mb: 100
  max_backups: 5
  max_age_days: 14
  compress: false
```

Panel `/logs` 只查看 Panel 本机文件。Node 的轮转原始文件留在各 Node 主机，不通过
控制面传输。

## Node 日志拉取

Node 详情页的“节点日志”通过现有 mTLS 控制面发送预定义 `log_query` 任务，只查询
该 Node 当前进程的内存 ring。查询支持级别、关键字、RFC3339 时间范围和条数过滤，
结果最新在前，最多 500 条且序列化结果不超过 256 KiB；页面会显示离线、超时、
控制面断开、失败、空结果和截断状态。新 Node 返回结构化时间/级别/消息/属性，
Panel 仍能显示旧 Node 的 `log_lines` 文本结果。

远程查询不会读取或传输 Node 当前/轮转日志文件，不提供实时流、下载、删除或任意
命令执行。查询关键字不会写入 Panel 的任务参数或审计记录，跨控制面前后都会再次
过滤 secret、token、Authorization、Cookie 和签名类属性。

## Curl 示例

创建节点和令牌：

```bash
curl -c cookie.txt \
  -H "Content-Type: application/json" \
  -X POST http://127.0.0.1:9001/api/admin/login \
  -d '{"password":"your-password"}'

curl -b cookie.txt \
  -H "Content-Type: application/json" \
  -X POST http://127.0.0.1:9001/api/admin/nodes \
  -d '{"display_name":"node-a"}'

curl -b cookie.txt \
  -X POST http://127.0.0.1:9001/api/admin/nodes/1/tokens
```

注册令牌、credential secret 和预签名 URL 都属于敏感材料，不要写入持久日志。节点退役或证书撤销只切断控制面；若需要停止对象访问，还必须停止 node 容器或轮换受影响的 S3 credential。
