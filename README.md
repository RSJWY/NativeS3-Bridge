# NativeS3-Bridge

> 轻量、高性能的本地 S3 桥接中间件。把操作系统上的**真实目录**透明映射为标准的 Amazon S3 兼容 API，配套一个 Vue3 单密码管理后台（含 ECharts 仪表盘），编译为**无外部运行时依赖的单文件二进制**，便于局域网跨平台一键部署。

NativeS3-Bridge exposes a local native directory tree through a standard S3-compatible API and ships with a single-password web admin UI (Vue3 + ECharts). It builds into a single dependency-free binary for cross-platform LAN deployment.

---

## 核心特性

- **1:1 原生文件映射**：Bucket = 数据根目录下的一级目录，Object Key（含 `/`）= 该目录下的相对路径文件。文件以**原名、原后缀、原始字节**驻留在本地磁盘，可用系统文件管理器直接打开。
  - 严禁数据切块（chunking）、纠删码、卷合并、专有格式编码。
- **标准 S3 协议**：SigV4 签名校验，支持对象 CRUD、分桶列举、对象列举、分段上传、Object Tagging、自定义元数据、预签名 URL 校验。
- **三数据库驱动**：同一套 GORM 模型，通过配置切换 **SQLite / MySQL / PostgreSQL**，启动时自动建表。
- **按密钥配额**：每个访问密钥可设容量上限，用量在事务内原子累加，超额上传被拒绝。
- **事件钩子**：对象创建 / 删除后异步触发 Webhook（带队列、重试），不阻塞主请求路径。
- **单密码 Web 管理后台**：bcrypt + session 登录，密钥 CRUD、配额设置、ECharts 仪表盘；前端构建产物经 `go:embed` 打入二进制，保持单文件部署。

## 适用场景

- 局域网内部署工具的更新分发
- 互动引擎（Unity / Unreal）本地资源同步
- AI 自动化工作流的数据直连

---

## 架构概览

```
                    ┌───────────────────────────┐
                    │  cmd/natives3bridge/main   │
                    │  config → DB → 装配 → 启动  │
                    └─────────────┬─────────────┘
              S3 API (默认 :9000) │ Web 管理 (默认 :9001)
        ┌───────────────────────┐ │ ┌─────────────────────┐
        │ auth (SigV4) → quota  │ │ │ webadmin            │
        │   → handlers          │ │ │  单密码登录/session  │
        │   object / multipart  │ │ │  密钥CRUD/配额/统计   │
        │   bucket / presigned  │ │ │  go:embed dist/      │
        └───────────┬───────────┘ │ └──────────┬──────────┘
                    │             │            │
        ┌───────────▼─────────────▼────────────▼──────────┐
        │ storage (1:1 原生映射) │ db (GORM 三驱动)         │
        │  path/metadata/multipart │ credentials/stats/hooks│
        └───────────┬──────────────────────────────────────┘
            ┌────────▼─────────┐        ┌──────────────────┐
            │ 本地文件系统       │        │ hooks (异步 Webhook)│
            │ 原生目录 / 文件    │        └──────────────────┘
            └──────────────────┘
```

## 目录结构

```
NativeS3-Bridge/
├── cmd/natives3bridge/main.go     # 唯一入口：装配各模块并启动
├── pkg/
│   ├── config/                    # YAML 配置加载 + 校验 + 默认值
│   ├── db/                        # GORM 三驱动入口、模型、AutoMigrate
│   ├── server/                    # S3 http.Server、路由分发、中间件链
│   ├── auth/                      # SigV4 校验、密钥存储（带缓存）、身份
│   ├── quota/                     # 配额检查 + 用量事务累加
│   ├── handlers/                  # bucket / object / multipart / presigned
│   ├── storage/                   # 1:1 路径映射、sidecar 元数据、分段合并
│   ├── hooks/                     # 事件类型、异步分发管理器、Webhook
│   └── webadmin/                  # 管理 API、单密码鉴权、go:embed 前端
│       └── ui/                    # Vue3 + Vite 源码与 dist 构建产物
├── configs/                       # 配置示例
├── scripts/                       # 冒烟测试脚本
└── README.md
```

---

## 快速开始

### 1. 准备依赖

- Go 1.21+
- Node.js 18+（仅在需要重新构建前端时）
- 可选：AWS CLI（用于验证 S3 接口）

### 2. 构建

前端构建产物已通过 `go:embed` 打入二进制。如需从源码完整构建：

```bash
# 先构建嵌入的 Vue 管理界面
cd pkg/webadmin/ui && npm ci && npm run build
cd ../../..

# 再构建 Go 二进制
go build -o natives3bridge ./cmd/natives3bridge
```

> 如果 `pkg/webadmin/ui/dist/` 已存在有效构建产物，可直接 `go build` 跳过前端步骤。

### 3. 准备配置

二进制默认读取 `configs/config.yaml`（可用 `-config` 覆盖）。仓库未提供该文件，请从示例复制：

```bash
cp configs/config.example.yaml configs/config.yaml
```

编辑 `configs/config.yaml`，至少设置管理后台首次启动密码：

```yaml
webadmin:
  admin_bootstrap_password: "your-strong-password"   # 首次启动用于生成 bcrypt hash
```

### 4. 启动

```bash
# 方式 A：使用默认配置路径
./natives3bridge

# 方式 B：指定配置文件
./natives3bridge -config configs/config.yaml

# 方式 C：启动并播种一个测试密钥（便于立即用 aws-cli 验证）
./natives3bridge -config configs/config.yaml \
  -seed-access-key TESTKEY -seed-secret-key TESTSECRET -seed-quota-bytes 0
```

启动后：
- S3 API 监听 `server.s3_addr`（默认 `0.0.0.0:9000`）
- 管理后台监听 `server.admin_addr`（默认 `0.0.0.0:9001`），浏览器访问该地址登录

首次启动若 `password_hash` 为空而 `admin_bootstrap_password` 不为空，日志会打印生成的 bcrypt hash：

```text
webadmin password_hash generated from admin_bootstrap_password; copy this hash into config and clear admin_bootstrap_password
```

请将该 hash 填入 `webadmin.password_hash`，并清空 `admin_bootstrap_password`。

---

## 启动参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-config` | `configs/config.yaml` | 配置文件路径 |
| `-seed-access-key` | `""` | 临时播种的访问密钥（用于本地测试）。须与 `-seed-secret-key` 成对出现 |
| `-seed-secret-key` | `""` | 临时播种的密钥 |
| `-seed-quota-bytes` | `0` | 播种密钥的容量上限（字节），`0` 表示不限 |

> 生产环境的密钥应通过管理后台创建，而非长期使用 `-seed-*` 参数。

## 配置说明

完整字段见 `configs/config.example.yaml`：

```yaml
server:
  s3_addr: "0.0.0.0:9000"       # S3 API 监听地址
  admin_addr: "0.0.0.0:9001"    # Web 管理界面监听地址（独立端口）
  tls:
    enabled: false              # 生产建议启用，或在前置反代终止 HTTPS
    cert_file: ""
    key_file: ""
  admin_tls:                    # 可省略；省略时管理端口继承 server.tls
    enabled: false              # 可独立于 S3 端口启用/关闭管理端口 TLS
    cert_file: ""
    key_file: ""

storage:
  data_root: "./data"                 # 所有 bucket 的根目录
  multipart_tmp: "./data/.multipart"  # 分段上传临时目录（隐藏；生产建议置于 data_root 之外）
  metadata_suffix: ".s3meta"          # sidecar 元数据文件后缀
  multipart_gc_interval: "1h"         # 残留分片 GC 周期
  multipart_ttl: "24h"                # 未完成分段上传的存活时间

database:
  driver: "sqlite"              # sqlite | mysql | postgres
  dsn: "./natives3.db"          # sqlite: 文件路径；mysql/postgres 见示例文件中的注释
  # mysql:    "user:pass@tcp(127.0.0.1:3306)/natives3?charset=utf8mb4&parseTime=True&loc=Local"
  # postgres: "host=127.0.0.1 user=postgres password=pass dbname=natives3 port=5432 sslmode=disable"

hooks:
  queue_size: 1024              # 事件队列容量
  workers: 4                    # 投递 worker 数
  max_retry: 3                  # 失败重试次数（指数退避）
  timeout: "5s"                 # 单次 Webhook 请求超时

webadmin:
  password_hash: ""             # bcrypt 哈希
  admin_bootstrap_password: ""  # 仅首次启动用于生成 hash，生成后请清空
  session_secret: "change-me-32bytes-random"
  session_ttl_minutes: 720
  login_max_failures: 5          # 同一来源 IP 连续失败达到阈值后锁定
  login_lockout_window: "15m"    # 登录失败锁定窗口

rate_limit:
  anonymous_rps: 10              # 匿名对象 GET/HEAD 每 IP 每秒请求数
  anonymous_burst: 20            # 匿名对象 GET/HEAD 每 IP 突发桶容量
  trust_forwarded: false         # 仅在可信反向代理后开启，信任 X-Forwarded-For/X-Real-IP

region: "us-east-1"             # SigV4 region
log_level: "info"               # debug | info | warn | error
```

### 切换数据库

同一份二进制，只改 `database.driver` 与 `database.dsn` 即可切换后端，启动时自动建表：

```yaml
# SQLite（本地开发，零部署）
database: { driver: "sqlite", dsn: "./natives3.db" }

# MySQL
database: { driver: "mysql", dsn: "user:pass@tcp(127.0.0.1:3306)/natives3?charset=utf8mb4&parseTime=True&loc=Local" }

# PostgreSQL
database: { driver: "postgres", dsn: "host=127.0.0.1 user=postgres password=pass dbname=natives3 port=5432 sslmode=disable" }
```

---

## 使用示例（AWS CLI）

```bash
export AWS_ACCESS_KEY_ID=TESTKEY
export AWS_SECRET_ACCESS_KEY=TESTSECRET
export AWS_DEFAULT_REGION=us-east-1
EP="--endpoint-url http://127.0.0.1:9000"

# 上传对象（含自定义元数据）
aws $EP s3api put-object --bucket mybucket --key docs/readme.txt \
  --body ./readme.txt --metadata author=alice,project=demo

# 查看元数据（HEAD）
aws $EP s3api head-object --bucket mybucket --key docs/readme.txt

# 列举对象
aws $EP s3api list-objects-v2 --bucket mybucket

# 下载对象
aws $EP s3api get-object --bucket mybucket --key docs/readme.txt ./download.txt

# 删除对象
aws $EP s3api delete-object --bucket mybucket --key docs/readme.txt

# 大文件分段上传（aws-cli 自动分片，完成后落地为单一原生文件）
aws $EP s3 cp ./big.bin s3://mybucket/big/big.bin
```

上传后，对应文件会以原名出现在 `<data_root>/<bucket>/<key>` 路径下，自定义元数据与标签保存在同目录的 `<file>.s3meta` sidecar 文件中。

---

## S3 API 支持范围

| 类别 | 操作 |
|---|---|
| Service | `ListBuckets`（`GET /`） |
| Bucket | `HeadBucket`、`ListObjectsV2`（支持 `prefix` / `delimiter` / `continuation-token` / `max-keys`） |
| Object | `PutObject`、`GetObject`（支持 Range / 206）、`HeadObject`、`DeleteObject` |
| Tagging | `PutObjectTagging`、`GetObjectTagging`、`DeleteObjectTagging`（`?tagging`） |
| Multipart | `CreateMultipartUpload`、`UploadPart`、`CompleteMultipartUpload`、`AbortMultipartUpload`、`ListParts`、`ListMultipartUploads`（`?uploads`） |
| Metadata | `x-amz-meta-*` 自定义元数据上传与原样取回 |
| 鉴权 | Header SigV4 与 query 形式预签名 URL（校验侧） |

错误响应统一为标准 S3 `<Error>` XML（如签名错误返回 403 `SignatureDoesNotMatch`，配额超限返回 `QuotaExceeded`）。

## 管理后台 API

所有 `/api/admin/*`（除登录外）均需先登录获取 session cookie。

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/admin/login` | 单密码登录，返回 session cookie 与过期时间 |
| `POST` | `/api/admin/logout` | 注销 |
| `GET` | `/api/admin/credentials` | 列举访问密钥 |
| `POST` | `/api/admin/credentials` | 创建密钥（ak/sk 由服务端随机生成），请求体：`{"name": "...", "quota_bytes": 0}` |
| `PATCH` / `DELETE` | `/api/admin/credentials/{id}` | 更新（名称 / 状态 / 配额）或删除指定密钥 |
| `GET` | `/api/admin/dashboard/summary` | 总览统计（密钥数、总配额、总用量） |
| `GET` | `/api/admin/dashboard/usage-ranking` | 各密钥用量排行 |
| `GET` | `/api/admin/dashboard/request-trend` | 按日请求趋势（用于 ECharts 折线图） |

登录示例：

```bash
curl -c cookie.txt -X POST http://127.0.0.1:9001/api/admin/login \
  -H "Content-Type: application/json" -d '{"password":"your-password"}'

curl -b cookie.txt http://127.0.0.1:9001/api/admin/dashboard/summary
```

---

## 数据模型

| 模型 | 用途 | 关键字段 |
|---|---|---|
| `Credential` | 访问密钥 | `AccessKey`（唯一）、`SecretKey`、`Status`、`QuotaBytes`、`UsedBytes` |
| `RequestStat` | 按日按密钥统计 | `CredentialID`、`Day`(UTC)、`Put/Get/DeleteCount`、`BytesIn/Out` |
| `HookConfig` | 事件钩子 | `URL`、`Events`（逗号分隔）、`Enabled` |

> Bucket 不入库——bucket 即目录，列举靠扫盘；对象元数据 / 标签存 sidecar 文件，不入库。JSON 数据统一用 TEXT 列存储以保证三驱动通用。

## 安全提示

- **管理后台 TLS 直连**：`server.admin_tls` 可独立控制管理端口 TLS；省略 `admin_tls` 时继承 `server.tls`，保持旧配置行为不变。生产直连管理端口时建议显式配置管理端口证书：

  ```yaml
  server:
    tls:
      enabled: false
    admin_tls:
      enabled: true
      cert_file: "/etc/natives3bridge/admin.crt"
      key_file: "/etc/natives3bridge/admin.key"
  ```

  启用 `admin_tls.enabled` 但缺少 `cert_file` 或 `key_file` 时，服务会在配置校验阶段拒绝启动。管理端口 TLS 启用后，admin session cookie 会自动设置 `Secure=true`。
- **反向代理部署**：也可以让管理端口只监听内网明文 HTTP，由 Nginx/Caddy 等可信反向代理终止 HTTPS。此时不要把 `admin_addr` 暴露到公网；只有当代理会覆盖并校验转发头时，才开启 `rate_limit.trust_forwarded: true`，让登录锁定和匿名限流按真实客户端 IP 计算。
- **明文管理端口警告**：当有效 admin TLS 为关闭状态时，管理界面与 session cookie 以明文 HTTP 提供，仅适用于受信任的局域网或开发环境。启动时会打印警告：

  ```text
  admin UI served over plain HTTP; enable TLS for production
  ```
- **登录节流**：`webadmin.login_max_failures` 和 `webadmin.login_lockout_window` 控制同一来源 IP 的登录失败锁定；锁定期间登录接口返回 `429` 与 `Retry-After`，不会继续进行密码校验。
- **匿名下载限流**：public-read 桶的匿名对象 `GET`/`HEAD` 受 `rate_limit.anonymous_rps` 与 `rate_limit.anonymous_burst` 限制，超限返回 S3 XML `503 SlowDown`。带签名请求不受该匿名限流影响，仍按密钥配额体系处理。
- **会话密钥**：务必将 `webadmin.session_secret` 改为足够长的随机值。
- **首启密码**：生成 `password_hash` 后清空 `admin_bootstrap_password`。
- **分段临时目录**：生产环境建议将 `storage.multipart_tmp` 配置到 `data_root` 之外的独立路径。

---

## 开发

```bash
go build ./...        # 编译全部包
go vet ./...          # 静态检查
go test ./...         # 运行单元测试
```

冒烟测试脚本见 `scripts/smoke-test.sh`（基于 aws-cli 验证端到端流程）。

## 技术选型

| 维度 | 选型 |
|---|---|
| 语言 | Go（标准库 `net/http` + `io`，goroutine 并发，单文件二进制） |
| 数据库 | GORM + sqlite / mysql / postgres 三驱动 |
| S3 XML | `encoding/xml` 手写标准响应 |
| 签名 | SigV4（含 query 预签名校验） |
| 前端 | Vue3 + Vite + Apache ECharts |
| 前端嵌入 | `go:embed`（构建产物打入二进制） |
| 管理鉴权 | 单密码（bcrypt + 签名 session cookie） |

## License

见仓库 LICENSE（如未提供，请按项目要求补充）。
