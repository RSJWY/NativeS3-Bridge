# NativeS3-Bridge

NativeS3-Bridge 是一个 panel/node 分离的轻量 S3 桥接系统。node 把操作系统上的真实目录映射为标准 S3 兼容 API；panel 提供集中管理界面、节点注册、配置下发和运维任务控制。S3 对象流量直接进入 node，不经过 panel。

项目目标很明确：在不引入专有对象格式的前提下，让本地文件系统可以被 S3 客户端、业务服务、脚本和浏览器直链安全访问。

## 文档导航

- [核心能力](#核心能力)
- [界面预览](#界面预览)
- [适用场景](#适用场景)
- [架构与数据模型](#架构与数据模型)
- [快速开始](#快速开始)
- [配置说明](#配置说明)
- [S3 API 使用](#s3-api-使用)
- [管理后台](#管理后台)
- [公网安全部署](#公网安全部署)
- [运维端点与监控](#运维端点与监控)
- [事件钩子](#事件钩子)
- [Docker 部署](#docker-部署)
- [发布流程](#发布流程)
- [开发与验证](#开发与验证)
- [仓库文件与忽略规则](#仓库文件与忽略规则)
- [License](#license)

## 核心能力

| 能力 | 说明 |
|---|---|
| 原生文件 1:1 映射 | Bucket 是 `storage.data_root` 下的一级目录，Object Key 是 bucket 内的相对路径文件。对象字节以原始文件形式落盘，不切块、不封装、不改名。 |
| S3 兼容 API | 支持 Header SigV4、query presigned URL、对象 CRUD、bucket 操作、分段上传、批量删除、服务端复制、tagging、自定义元数据和 Range 下载。 |
| 管理面/数据面分离 | panel 只承载管理 UI、REST 与 mTLS 控制面；node 只承载 S3 数据面和主动连接 panel 的 agent。 |
| 多数据库 | panel 与每个 node 使用物理独立的数据库，均可通过 GORM 使用 SQLite、MySQL 或 PostgreSQL。 |
| 配额与统计 | 每个 S3 credential 可设置 quota，PUT 和 multipart complete 会按最终对象大小计入用量，请求统计按 UTC 日期聚合。 |
| 匿名 public-read | Bucket ACL 支持 `private` 与 `public-read`。匿名访问仅允许 public-read bucket 的对象级 `GET`/`HEAD`。 |
| 集中管理 | panel 使用单管理员登录，支持 TOTP、Turnstile-compatible captcha、节点生命周期、一次性注册令牌、credential 管理、期望配置发布和远程任务。 |
| 双二进制部署 | Vue3 管理界面通过 `go:embed` 打入 `panel`；`panel` 与 `node` 可分别构建、升级和回滚，运行时不需要 Node.js。 |
| 异步 Webhook | 对象创建、删除和 multipart complete 可异步投递事件，失败重试不阻塞 S3 请求。 |

## 界面预览

仓库中的现有截图来自拆分前的单节点管理界面，不代表当前 panel/node 控制面的节点注册和管理流程，因此不再把它们作为当前部署预览。当前分支以 panel Admin API、节点在线状态、期望版本和任务结果为准。

## 适用场景

- 局域网或内网环境中，把已有目录快速暴露为 S3 接口。
- 游戏、互动引擎、AI 工作流、媒体处理脚本需要 S3 API，但希望对象仍是普通文件。
- 业务服务生成私有对象的短时预签名直链，终端用户通过浏览器或 HTTP 客户端直接下载。
- 小型团队需要一个集中 panel 管理多个轻量 node、同时保留原生文件布局的对象网关。

不适合的场景：

- 需要 AWS IAM 级别策略、多租户隔离、对象级授权或多用户 RBAC。
- 需要分布式高可用、跨节点副本、纠删码、版本化或对象锁。
- 需要把文件存储为专有块格式或跨盘卷合并。

## 架构与数据模型

```text
管理员 ──HTTPS──▶ panel :9001
                  ├─ 管理 UI / REST
                  ├─ panel 独立数据库
                  └─ node 控制面 :9443
                           ▲
                           │ node 主动拨号
                           │ 首次 server TLS 注册，之后 mTLS WebSocket
                           │
S3 客户端 ───────────────▶ node :9000
                           ├─ SigV4 / S3 handlers / quota / hooks
                           ├─ node 本地数据库
                           └─ 原生对象文件与 sidecar
```

- panel 不监听 S3 端口，也不保存或转发对象字节。
- node 不提供管理 UI 或管理端口；它只暴露 S3 listener，并主动连接 panel。
- panel 与 node 数据库物理分离。panel 保存节点、证书指纹、加密后的 credential 和期望配置；node 保存实际生效的 S3 业务状态、统计和 agent 状态。
- node 断开控制面后仍按最后一次成功应用的本地配置继续提供 S3 服务。
- 当前拆分是对旧单体入口的硬切换；新部署应使用 `cmd/panel` 与 `cmd/node`。

### 原生文件布局

对象只落在 node。使用示例 node 配置时：

```yaml
storage:
  data_root: "/data/objects"
  metadata_suffix: ".s3meta"
```

上传对象：

```text
bucket: media
key:    images/cover.jpg
```

落盘结果：

```text
/data/objects/
└── media/
    └── images/
        ├── cover.jpg
        └── cover.jpg.s3meta
```

`cover.jpg` 是原始对象字节，可直接用系统文件管理器、图片查看器或脚本读取。`.s3meta` 保存 ETag、Content-Type、自定义 metadata、tags、size 和上传时间。缺少 sidecar 时，服务仍能读取原生对象，只是 metadata/tags 为空或按扩展名推断。

### 数据归属

| 位置 | 主要内容 |
|---|---|
| panel 数据库 | 节点生命周期、注册令牌 hash、节点证书指纹、审计、加密后的 S3 secret、版本化期望配置和任务结果。 |
| panel 主密钥文件 | 解密 panel 数据库中 S3 secret 的 32 字节 AEAD 主密钥；必须与数据库分开备份。 |
| panel PKI | 在线中间 CA 与 agent listener 服务端证书；用于首次注册和后续 mTLS。 |
| node 数据库 | 实际生效的 credentials、buckets、request stats、hooks，以及控制面应用版本和证书状态。 |
| node 数据目录 | 原始对象、`.s3meta` sidecar、multipart 临时文件和 node 私钥/证书。 |

对象字节、对象 metadata 和 tags 不进入 panel，也不存入关系数据库。

## 快速开始

> **当前发布状态：** panel/node 拆分代码目前只存在于 `07-13-multi-node-mtls-control-plane` 分支，尚未进入远端 `main`。现有远端 tag 早于本次拆分，`natives3-panel` / `natives3-node` GHCR 包尚未正式发布。请从当前 checkout 本地构建，不要使用尚不存在的 `latest` 镜像。

### 1. 环境要求

- Go 1.21+
- Node.js 18+，仅在需要重新构建管理后台前端时使用
- OpenSSL，用于生成本地验证所需的主密钥和 PKI
- AWS CLI，可选，用于端到端验证 S3 API
- Docker，可选，用于容器部署

### 2. 构建 panel 与 node

从完整源码构建时先构建前端，再构建 Go：

```bash
npm ci --prefix pkg/webadmin/ui
npm run build --prefix pkg/webadmin/ui
go build -o panel ./cmd/panel
go build -o node ./cmd/node
```

如果 `pkg/webadmin/ui/dist/` 已经存在有效构建产物，可以直接执行 Go 构建：

```bash
go build -o panel ./cmd/panel
go build -o node ./cmd/node
```

### 3. 准备两套配置

仓库不会提交真实运行配置。panel 与 node 使用不同配置：

```bash
cp -n configs/panel.example.yaml configs/panel.yaml
cp -n configs/node.example.yaml configs/node.yaml
```

- `configs/panel.yaml`：管理监听、控制面 TLS、在线中间 CA、主密钥、panel 数据库和管理员认证。
- `configs/node.yaml`：S3 listener、对象目录、node 本地数据库、panel URL、逻辑节点 ID 和 node mTLS 文件。

示例里的 `/data/...` 是容器内路径。本机直接运行二进制时，应改成当前用户可读写的绝对路径。完整可复制的本地镜像、PKI 和挂载流程见 [Docker 部署](#docker-部署)。

### 4. 按顺序启动

panel 会在缺少主密钥、中间 CA 或 agent 服务端证书时拒绝启动。准备好这些文件后先校验并启动 panel：

```bash
./panel -check-config -config configs/panel.yaml
./panel -config configs/panel.yaml
```

然后通过 panel Admin API（Curl 示例见下文）完成：

1. 登录 `http://127.0.0.1:9001/`。
2. 创建逻辑节点，记录 `node_id`。
3. 为该节点签发一次性注册令牌；令牌默认 10 分钟有效且只显示一次。
4. 把 `node_id`、令牌、`register_url` 和 `agent_url` 写入 `configs/node.yaml`。
5. 确保 node 配置中的 `panel.ca_file` 指向能够验证 panel agent 服务端证书的 CA 文件。

最后校验并启动 node：

```bash
./node -check-config -config configs/node.yaml
./node -config configs/node.yaml
```

node 首次启动会在本地生成私钥和 CSR，用一次性令牌换取客户端证书，随后使用 mTLS 连接 panel。注册成功后可清空配置中的 `registration_token`；node 私钥不会上传到 panel。

默认网络边界：

| 进程 | 默认监听 | 用途 |
|---|---|---|
| panel admin | `127.0.0.1:9001`（示例容器内为 `0.0.0.0:9001`） | 管理 UI 和 REST。 |
| panel agent | `0.0.0.0:9443` | node 首次注册和 mTLS WebSocket。 |
| node S3 | `0.0.0.0:9000` | AWS CLI、SDK 和 HTTP 客户端。 |

## 配置说明

完整示例见 [configs/panel.example.yaml](configs/panel.example.yaml) 和 [configs/node.example.yaml](configs/node.example.yaml)。不要使用旧单体配置示例作为 panel/node 新部署入口。

### panel 配置

```yaml
admin_addr: "0.0.0.0:9001"

agent:
  addr: "0.0.0.0:9443"
  cert_file: "/data/pki/panel-server.crt"
  key_file: "/data/pki/panel-server.key"

pki:
  intermediate_cert_file: "/data/pki/intermediate-ca.crt"
  intermediate_key_file: "/data/pki/intermediate-ca.key"
  client_cert_ttl: 2160h

master_key_file: "/data/secrets/master.key"

database:
  driver: "sqlite"
  dsn: "/data/panel.db"

webadmin:
  password_hash: ""
  admin_bootstrap_password: ""
  session_secret: "replace-with-a-random-32-byte-secret-value"
```

- `agent.cert_file` / `agent.key_file`：9443 listener 的服务端 TLS 身份；证书 SAN 必须覆盖 node 使用的主机名。
- `pki.intermediate_*`：在线中间 CA。panel 用它签发 node 客户端证书，缺失时 fail-closed。
- `master_key_file`：恰好 32 字节的原始主密钥，用于加密 S3 secret；不得只和 panel 数据库放在同一备份中。
- `database`：只保存 panel 控制面状态，不保存对象或 node 的请求统计。
- `webadmin.admin_bootstrap_password`：仅用于首次生成 bcrypt hash。启动日志输出 hash 后，把它写入 `password_hash` 并清空 bootstrap password。
- `webadmin.session_secret`：生产环境必须替换为至少 32 字节的随机值。

panel 配置检查会实际加载主密钥和在线 CA，并校验 agent 服务端证书路径字段；完整启动时才会真正加载 agent listener 证书和私钥：

```bash
./panel -check-config -config configs/panel.yaml
```

### node 配置

```yaml
server:
  s3_addr: "0.0.0.0:9000"
  tls:
    enabled: false

storage:
  data_root: "/data/objects"
  metadata_suffix: ".s3meta"

database:
  driver: "sqlite"
  dsn: "/data/natives3.db"

panel:
  node_id: 1
  register_url: "https://panel:9443/register"
  agent_url: "wss://panel:9443/agent"
  registration_token: ""
  cert_file: "/data/pki/node.crt"
  key_file: "/data/pki/node.key"
  ca_file: "/data/pki/panel-ca.crt"
  heartbeat_interval: 15s
```

- `storage.data_root`：对象根目录。示例挂载 `./node-data:/data`，所以宿主机对象位于 `./node-data/objects`。
- `database.dsn`：node 本地状态库。示例中位于宿主机 `./node-data/natives3.db`。
- `panel.node_id`：必须先由 panel 创建，不能自行猜测。
- `registration_token`：只在首次无证书启动时使用；注册成功后可清空。
- `cert_file` / `key_file`：node 本地 mTLS 身份。私钥由 node 首次注册时生成并始终留在 node。
- `ca_file`：验证 panel agent 服务端证书的 CA；Docker 示例应放在 `./node-data/pki/panel-ca.crt`。

node 不读取管理端监听、管理员密码等 panel 字段。credentials、bucket、quota、webhook 和 rate-limit 等业务配置由 panel 形成版本化期望状态并下发；升级旧节点时，遗留业务字段会被忽略，而不是作为新部署配置继续维护。

```bash
./node -check-config -config configs/node.yaml
```

### 数据库驱动与备份

panel 和 node 都支持 SQLite、MySQL、PostgreSQL，但 DSN 必须分别写在 `configs/panel.yaml` 与 `configs/node.yaml` 中。

```yaml
# SQLite
database:
  driver: "sqlite"
  dsn: "/data/panel.db" # panel；node 示例为 /data/natives3.db

# MySQL
database:
  driver: "mysql"
  dsn: "user:pass@tcp(mysql:3306)/natives3panel?charset=utf8mb4&parseTime=True&loc=Local"

# PostgreSQL
database:
  driver: "postgres"
  dsn: "host=postgres user=natives3 password=pass dbname=natives3panel port=5432 sslmode=disable"
```

- 启动时会打开并迁移各自数据库；迁移或 schema 校验失败时，对应进程退出。
- SQLite 备份只保护关系数据。panel 还必须单独备份主密钥、CA、配置和审计；node 还必须备份对象目录、sidecar、本地数据库和 node 私钥/证书。
- panel 数据库备份与主密钥备份必须位于不同信任域。只有数据库备份不应能恢复明文 S3 secret。
- MySQL/PostgreSQL 应使用数据库原生一致备份、托管快照、物理备份或 PITR；应用不会在启动时替你复制远端数据库。
- 完整恢复集合与演练步骤见 [多节点运维文档](docs/multi-node-operations.md)。

## S3 API 使用

### AWS CLI 环境变量

```bash
export AWS_ACCESS_KEY_ID=TESTKEY
export AWS_SECRET_ACCESS_KEY=TESTSECRET
export AWS_DEFAULT_REGION=us-east-1
EP="--endpoint-url http://127.0.0.1:9000"
```

### 常用操作

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

### 支持范围

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
| Auth | Header SigV4 和 query presigned URL |
| Anonymous | public-read bucket 的对象级 `GET`/`HEAD` |

不支持或不属于当前目标：

- AWS IAM policy、bucket policy、ACL XML 兼容写接口。
- S3 versioning 的真实版本存储。
- Object Lock、SSE、Lifecycle、Replication。
- 匿名列 bucket、匿名写入、匿名删除。

### 预签名 URL

业务服务应优先使用 private bucket 加短 TTL 预签名 URL 暴露用户直链：

```bash
aws $EP s3 presign s3://mybucket/docs/readme.txt --expires-in 300
```

服务端会按 query SigV4 校验 `X-Amz-*` 参数。不要把完整预签名 URL 写入日志，因为 query string 中包含签名材料。

### public-read 直链

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

### 错误格式

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

## 管理后台

浏览器访问 panel 的 `http://127.0.0.1:9001/`。管理后台是单管理员模型，不提供多用户、RBAC 或 OIDC。它管理节点和期望状态，不直接访问 node 的对象目录。

### 登录流程

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

前端可读取非敏感登录设置：

```http
GET /api/admin/auth-settings
```

该接口只返回是否需要 TOTP、是否启用 captcha、captcha provider 和 site key，不返回 secret。

### Panel Admin API

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
| `GET` | `/api/admin/nodes/{id}/certs` | 查看 node 客户端证书。 |
| `POST` | `/api/admin/nodes/{id}/certs/revoke` | 撤销该节点的全部证书并断开控制面连接。 |

创建节点和令牌的 Curl 示例：

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

## 公网安全部署

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

### 基本原则

- 所有公网入口必须使用 HTTPS。
- S3 API 和管理后台使用不同域名，便于独立 cookie、限流、WAF 和日志策略。
- 管理后台公网访问不要只依赖单密码。建议启用 TOTP 和 captcha。
- `admin_addr` 尽量绑定内网地址，公网只通过反向代理访问。
- `trust_forwarded` 只在可信代理覆盖转发头时启用。
- 业务直链优先使用 private bucket + 短 TTL presigned URL。
- `public-read` 只用于明确对所有知道 URL 的人公开的对象。
- panel 的 9443 端口只用于 node 注册和控制面连接，应使用正确的服务端证书并限制无关来源。

### Nginx 反向代理示例

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

若下发给 node 的策略启用了 `rate_limit.trust_forwarded`，必须确保 node 不能被绕过代理直接访问。

### 公网生产检查清单

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

当前 panel/node Compose 使用各自二进制的 `-check-config` 作为容器 healthcheck。panel 检查会读取主密钥和在线 CA，node 检查会校验基础设施字段；它们都不是请求级 liveness/readiness 探针，也不能替代完整启动检查。生产监控还应覆盖：

- panel 与 node 容器状态、重启次数和退出码。
- panel 9001、9443 监听状态，以及 node 9000 S3 探测。
- panel 中节点的 online、last heartbeat、applied/desired version 和 drift 状态。
- panel/node 日志中的注册失败、证书错误、任务失败和数据库迁移错误。

不要继续按旧单体 README 暴露 `/healthz`、`/readyz` 或 `/metrics`；当前 panel 管理服务器没有注册这些旧端点。

## 事件钩子

Hook manager 从数据库的 `hook_configs` 表加载启用的 Webhook 配置。对象创建、对象删除和 multipart complete 会投递事件。

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

当前 panel Admin API 尚未提供 webhook CRUD。不要把直接修改 node 的 `hook_configs` 当作长期配置入口；业务配置应由 panel 权威状态管理，缺少的管理能力需在后续版本补齐。

## Docker 部署

当前 panel/node 镜像尚未正式发布。以下流程使用 `docker-compose.example.yml` 从当前 checkout 构建本地镜像 `natives3-panel:local` 与 `natives3-node:local`，不会拉取 GHCR `latest`。

### 1. 准备配置、主密钥和本地验证 PKI

下面生成的是本地验证用短期自签 CA。生产环境应使用离线 root + 在线 intermediate 的完整流程，并按 [多节点运维文档](docs/multi-node-operations.md) 分离备份。

```bash
cp -n configs/panel.example.yaml configs/panel.yaml
cp -n configs/node.example.yaml configs/node.yaml
mkdir -p panel-data/pki panel-data/secrets node-data/pki node-data/objects

# panel 加密 S3 secret 的原始 32 字节主密钥
openssl rand -out panel-data/secrets/master.key 32

# 本地验证用在线 CA
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out panel-data/pki/intermediate-ca.key
openssl req -x509 -new -sha256 -days 30 \
  -key panel-data/pki/intermediate-ca.key \
  -subj '/CN=NativeS3 Local Intermediate CA' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -out panel-data/pki/intermediate-ca.crt

# panel 9443 agent listener 服务端证书；SAN=panel 对应 Compose 服务名
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out panel-data/pki/panel-server.key
openssl req -new \
  -key panel-data/pki/panel-server.key \
  -subj '/CN=panel' \
  -addext 'subjectAltName=DNS:panel,DNS:localhost,IP:127.0.0.1' \
  -out panel-data/pki/panel-server.csr
printf '%s\n' \
  'basicConstraints=CA:FALSE' \
  'keyUsage=digitalSignature,keyEncipherment' \
  'extendedKeyUsage=serverAuth' \
  'subjectAltName=DNS:panel,DNS:localhost,IP:127.0.0.1' \
  > panel-data/pki/panel-server.ext
openssl x509 -req -sha256 -days 30 \
  -in panel-data/pki/panel-server.csr \
  -CA panel-data/pki/intermediate-ca.crt \
  -CAkey panel-data/pki/intermediate-ca.key \
  -CAcreateserial \
  -extfile panel-data/pki/panel-server.ext \
  -out panel-data/pki/panel-server.crt

# node 用该 CA 验证 panel 服务端证书
cp panel-data/pki/intermediate-ca.crt node-data/pki/panel-ca.crt

# 写入可用于首次登录的随机密码和 session secret
ADMIN_PASSWORD="$(openssl rand -hex 16)"
SESSION_SECRET="$(openssl rand -hex 32)"
sed -i "s|^  admin_bootstrap_password:.*|  admin_bootstrap_password: \"$ADMIN_PASSWORD\"|" configs/panel.yaml
sed -i "s|^  session_secret:.*|  session_secret: \"$SESSION_SECRET\"|" configs/panel.yaml
printf 'save this bootstrap admin password: %s\n' "$ADMIN_PASSWORD"

# Compose 内部通过服务名 panel 连接控制面
sed -i 's|https://panel.example.com:9443/register|https://panel:9443/register|' configs/node.yaml
sed -i 's|wss://panel.example.com:9443/agent|wss://panel:9443/agent|' configs/node.yaml

# 镜像默认 UID/GID 为 10001:10001
sudo chown -R 10001:10001 panel-data node-data
```

不要删除或丢失终端输出的 bootstrap password。panel 首次启动还会在日志中输出 bcrypt `password_hash`；完成验证后应把 hash 写回 `configs/panel.yaml`，并清空 `admin_bootstrap_password`。

### 2. 构建并只启动 panel

```bash
docker compose -f docker-compose.example.yml build panel node
docker compose -f docker-compose.example.yml up -d panel
docker compose -f docker-compose.example.yml ps panel
docker compose -f docker-compose.example.yml logs panel
```

此时不要启动 node。panel 必须先可登录，并创建逻辑节点和一次性注册令牌。

### 3. 创建逻辑节点和注册令牌

保持在上一步的同一 shell 中，使用 `ADMIN_PASSWORD` 登录并创建节点；如果换了 shell，请先把保存的密码重新赋给该变量。示例假设新节点 ID 为 `1`，以实际响应为准：

```bash
curl -c /tmp/natives3-panel.cookie \
  -H 'Content-Type: application/json' \
  -X POST http://127.0.0.1:9001/api/admin/login \
  -d "{\"password\":\"$ADMIN_PASSWORD\"}"

curl -b /tmp/natives3-panel.cookie \
  -H 'Content-Type: application/json' \
  -X POST http://127.0.0.1:9001/api/admin/nodes \
  -d '{"display_name":"node-1"}'

curl -b /tmp/natives3-panel.cookie \
  -X POST http://127.0.0.1:9001/api/admin/nodes/1/tokens
```

最后一个响应中的 `token` 只显示一次且默认 10 分钟有效。把实际 `node_id` 和 `token` 写入 `configs/node.yaml`：

```yaml
panel:
  node_id: 1
  register_url: "https://panel:9443/register"
  agent_url: "wss://panel:9443/agent"
  registration_token: "把一次性令牌粘贴到这里"
  cert_file: "/data/pki/node.crt"
  key_file: "/data/pki/node.key"
  ca_file: "/data/pki/panel-ca.crt"
```

### 4. 启动 node 并检查注册

```bash
docker compose -f docker-compose.example.yml up -d node
docker compose -f docker-compose.example.yml ps panel node
docker compose -f docker-compose.example.yml logs node
```

注册成功后，`node-data/pki/` 中应出现 node 私钥和客户端证书，并可通过以下请求确认节点 `online: true`：

```bash
curl -b /tmp/natives3-panel.cookie http://127.0.0.1:9001/api/admin/nodes
```

随后清空宿主机 `configs/node.yaml` 中的 `registration_token`；已经签发的证书会用于后续重启。

两个容器内的配置路径分别为 `/app/configs/panel.yaml` 和 `/app/configs/node.yaml`。宿主机 `panel-data` 挂载为 panel 的 `/data`，`node-data` 挂载为 node 的 `/data`。

### Compose 数据库用法

SQLite 是默认配置，panel 与 node 各有自己的数据库：

```yaml
database:
  driver: "sqlite"
  dsn: "/data/panel.db"
```

- 对象字节写入 `./node-data/objects`，node 本地数据库位于 `./node-data/natives3.db`。
- panel SQLite 数据库位于 `./panel-data/panel.db`。
- `./panel-data/secrets/master.key` 必须与 panel 数据库分开备份；`panel-data/pki`、`node-data/pki` 和对象目录也属于恢复集合。
- 启动迁移前会自动做 SQLite 完整性检查；已有业务表时会生成
  同目录的 `.pre-upgrade-*.bak` 备份。
- 升级前仍建议整体备份 `./panel-data` 和 `./node-data`。

MySQL profile 只提供一个可选数据库容器，不会自动改写应用配置。若 panel 使用 MySQL，先修改 `configs/panel.yaml`：

```yaml
database:
  driver: "mysql"
  dsn: "natives3:change-me-mysql-password@tcp(mysql:3306)/natives3panel?charset=utf8mb4&parseTime=True&loc=Local"
```

然后启动 MySQL profile：

```bash
docker compose -f docker-compose.example.yml --profile mysql up -d mysql panel
```

- DSN 里的 `mysql` 是 compose 文件中的服务名。
- `change-me-mysql-password` 必须和 `docker-compose.example.yml` 中
  `MYSQL_PASSWORD` 一致。
- 应用不会在启动时复制 MySQL 表；生产升级前用 MySQL 原生一致备份、
  托管快照或物理备份。

PostgreSQL 用法类似。先修改 `configs/panel.yaml`：

```yaml
database:
  driver: "postgres"
  dsn: "host=postgres user=natives3 password=change-me-postgres-password dbname=natives3panel port=5432 sslmode=disable"
```

然后启动 PostgreSQL profile：

```bash
docker compose -f docker-compose.example.yml --profile postgres up -d postgres panel
```

- DSN 里的 `postgres` 是 compose 文件中的服务名。
- `change-me-postgres-password` 必须和 `docker-compose.example.yml` 中
  `POSTGRES_PASSWORD` 一致。
- 应用不会在启动时复制 PostgreSQL 表；生产升级前用 PostgreSQL 原生一致
  备份、托管快照、物理备份或 PITR。

这些 profile 默认只替换 panel 数据库。node 仍使用 `configs/node.yaml` 中的独立 DSN；如需让 node 使用远端数据库，必须单独修改 node 配置。无论数据库驱动为何，示例 node 的 `storage.data_root` 都是 `/data/objects`，对象文件不进入关系数据库。

## 发布流程

> **尚未正式发布：** 当前远端 tag 都早于 panel/node 拆分，拆分分支也尚未合入远端 `main`。因此下面描述的是仓库中 release workflow 的目标产物合同，不代表 GHCR 包或 Release 归档现在已经存在。正式发布前请继续使用当前 checkout 本地构建。

代码进入发布分支并创建新的正式 tag 后，GitHub Actions release workflow 计划执行：

- `npm ci && npm run build` 构建 Web 管理后台。
- Go 1.21 `go vet ./...`、`go test ./...` 和 `go test -race ./...`。
- 分别交叉编译 panel/node 的 Linux amd64/arm64、macOS amd64/arm64、Windows amd64，共 10 个归档。
- 每个归档包含对应示例配置与 `docs/multi-node-operations.md`，并上传统一的 `checksums.txt`。
- 并行构建并推送 panel/node 的 amd64/arm64 多架构镜像到 GHCR。

正式发布后的目标镜像地址为：

```text
ghcr.io/rsjwy/natives3-panel:<tag>
ghcr.io/rsjwy/natives3-node:<tag>
```

正式发布后，GitHub Release 归档名为 `natives3-panel-<version>-<os>-<arch>.tar.gz` 和 `natives3-node-<version>-<os>-<arch>.tar.gz`。在对应 tag、Release 和 GHCR package 页面均可访问前，不要把这些地址用于部署。

每个多架构 tag 指向 OCI image index。除 amd64/arm64 的可运行 manifest 外，BuildKit 还会为每个平台发布最小 provenance attestation；GHCR 可能把这些子 manifest/attestation digest 显示为 untagged，这是正常的索引结构，并非重复发布的镜像。workflow 显式使用 `provenance: mode=min`、`sbom: false`，避免 Action 默认值变化影响产物。

手动运行 `Release` workflow 时可以输入发布 tag。若 tag 不存在，workflow 会基于当前构建提交创建该 tag；如需指定源码，可填写 `source_ref`。

## 开发与验证

### 常用命令

```bash
npm ci --prefix pkg/webadmin/ui
npm run build --prefix pkg/webadmin/ui
go build ./cmd/panel ./cmd/node
go vet ./...
go test ./...
```

### 冒烟测试

panel/node 分发合同和本地 PKI 启动路径可用以下脚本验证：

```bash
./scripts/test-release-integrity.sh
./scripts/test-distribution-contract.sh
./scripts/test-upgrade-rollback.sh
```

真实容器注册流程按 [Docker 部署](#docker-部署) 的顺序执行。需要验证 S3 CRUD 时，先在 panel 为目标节点创建 credential、发布期望状态并等待 node 应用，再把返回的 access/secret 用于 `scripts/smoke-test.sh`；不要再使用单体的 `-seed-access-key` 启动参数。

### 代码结构

```text
cmd/panel/               # 管理 UI/REST 与 node 控制面入口
cmd/node/                # S3 数据面与主动连接 panel 的 agent 入口
pkg/panel/               # 节点、PKI、令牌、期望状态、任务和迁移
pkg/nodeagent/           # 注册、mTLS 客户端、配置应用和本地任务
pkg/controlproto/        # panel/node 版本约束的控制面协议
pkg/config/              # panel/node YAML 配置、默认值和校验
pkg/db/                  # node 业务数据库连接、模型和迁移
pkg/server/              # S3 listener、路由、中间件、匿名限流
pkg/auth/                # Header/query SigV4、credential cache、identity
pkg/quota/               # quota check 和 usage/stat 事务提交
pkg/handlers/            # bucket/object/multipart/tagging/presigned handlers
pkg/storage/             # 原生文件 backend、bucket metadata、sidecar、multipart
pkg/hooks/               # Webhook event manager
pkg/webadmin/            # 复用的管理员认证与 embedded SPA
pkg/webadmin/ui/         # Vue3 + Vite + ECharts 前端
configs/                 # 示例配置
scripts/                 # 冒烟测试脚本
```

## 仓库文件与忽略规则

提交前建议检查：

```bash
git status --short
git status --ignored --short
```

应提交：

- 业务代码：`cmd/`、`pkg/`、`configs/*.example.yaml`、`scripts/`。
- 文档：`README.md`、`AGENTS.md`、`.trellis/spec/`、已归档任务记录。
- 项目级 AI 工作流配置：`.agents/`、`.codex/`。
- 前端源码和锁文件：`pkg/webadmin/ui/src/`、`package.json`、`package-lock.json`。

不应提交：

- 真实配置：`configs/panel.yaml`、`configs/node.yaml`。
- 本地数据：`panel-data/`、`node-data/`、`data/`、`state/`。
- 本地数据库：`*.db`、`*.sqlite`、`*.sqlite3`。
- SQLite 升级备份：`*.pre-upgrade-*.bak*`。
- 构建产物：`panel`、`node`、`bin/`、`*.tar.gz`。
- 前端依赖和产物：`pkg/webadmin/ui/node_modules/`、`pkg/webadmin/ui/dist/assets/`、`pkg/webadmin/ui/dist/index.html`。
- Trellis 运行态：`.trellis/.developer`、`.trellis/.runtime/`、`__pycache__/`、`.trellis/.template-hashes.json` 的本地模板哈希改动。

`.trellis/.template-hashes.json` 当前在仓库中已跟踪，但它容易记录本地模板刷新、runtime session 和 Python cache 哈希。除非明确在升级 Trellis 模板并审查了 diff，否则不要把它和业务或文档提交混在一起。

## License

见仓库 LICENSE。若仓库尚未提供 LICENSE，请在正式分发前补充。
