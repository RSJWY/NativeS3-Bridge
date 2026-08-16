# NativeS3-Bridge

panel/node 分离的轻量 S3 桥接系统。node 把操作系统上的真实目录映射为标准 S3 兼容 API；panel 提供集中管理界面、节点注册、配置下发和运维任务控制。S3 对象流量直接进入 node，不经过 panel。

项目目标：在不引入专有对象格式的前提下，让本地文件系统可以被 S3 客户端、业务服务、脚本和浏览器直链安全访问。

## 核心特性

- **原生文件 1:1 映射** — Bucket 是 `storage.data_root` 下的一级目录，Object Key 是 bucket 内的相对路径；对象字节原样落盘，不切块、不封装、不改名。
- **S3 兼容 API** — Header SigV4 / query presigned、对象 CRUD、bucket 操作、分段上传、批量删除、服务端复制、tagging、自定义元数据、Range 下载。
- **管理面/数据面分离** — panel 只承载管理 UI、REST 与 mTLS 控制面；node 只承载 S3 数据面，并主动拨号连接 panel。
- **多数据库** — panel 与 node 数据库物理独立，均支持 SQLite、MySQL、PostgreSQL（GORM）。
- **配额与统计** — 每个 S3 credential 可设 quota，PUT 和 multipart complete 按最终对象大小计入用量，请求统计按 UTC 日期聚合。
- **匿名 public-read** — public-read bucket 仅开放匿名对象级 `GET`/`HEAD`。
- **集中管理** — 单管理员登录（TOTP、captcha 可选）、节点生命周期、一次性注册令牌、credential 管理、版本化期望配置发布和远程任务。
- **双二进制部署** — Vue3 管理界面通过 `go:embed` 打入 panel；运行时不需要 Node.js。
- **异步 Webhook** — 对象创建、删除和 multipart complete 异步投递事件，失败重试不阻塞 S3 请求。

## 适用场景

适合：内网把已有目录快速暴露为 S3 接口；游戏、AI 工作流、媒体脚本需要 S3 API 但希望对象仍是普通文件；业务生成私有对象的短时预签名直链供浏览器直接下载；小团队用集中 panel 管理多个轻量 node。

不适合：AWS IAM 级策略、多租户隔离、对象级授权或多用户 RBAC；分布式高可用、跨节点副本、纠删码、版本化或对象锁；把文件存成专有块格式。

## 架构

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

- panel 不监听 S3 端口，也不保存或转发对象字节；node 没有管理端口，只暴露 S3 listener 并主动连接 panel。
- 两库物理分离：panel 保存节点生命周期、证书指纹、审计、加密后的 S3 secret 和版本化期望配置；node 保存实际生效的 credentials、buckets、请求统计和 hooks。
- panel 主密钥（32 字节 AEAD key）用于加密 S3 secret，必须与 panel 数据库分开备份。
- node 断开控制面后，仍按最后一次成功应用的本地配置继续提供 S3 服务。
- 当前拆分是对旧单体入口的硬切换；新部署使用 `cmd/panel` 与 `cmd/node`。

### 原生文件布局

对象只落在 node。上传 `media/images/cover.jpg`（`data_root: /data/objects`）后落盘为：

```text
/data/objects/
└── media/
    └── images/
        ├── cover.jpg          # 原始对象字节，可直接用文件管理器/脚本读取
        └── cover.jpg.s3meta   # ETag、Content-Type、metadata、tags、size、上传时间
```

缺少 sidecar 时服务仍能读取原生对象，只是 metadata/tags 为空或按扩展名推断。对象字节、metadata 和 tags 不进入 panel，也不存入关系数据库。

## 快速开始

### 方式一：Docker 一键安装（推荐）

panel 与 node 分别部署在不同主机，直接拉取 GHCR `latest` 镜像，无需克隆仓库：

```bash
# Panel 主机：--panel-host 必须是 node 实际连接的域名或 IPv4
curl -fsSL https://raw.githubusercontent.com/RSJWY/NativeS3-Bridge/main/scripts/install-panel.sh \
  | sudo bash -s -- --panel-host panel.example.com

# Node 主机：先在 panel 创建逻辑 node 和一次性注册令牌，并复制 panel 公共 CA
curl -fsSL https://raw.githubusercontent.com/RSJWY/NativeS3-Bridge/main/scripts/install-node.sh \
  | sudo bash -s -- \
      --panel-url https://panel.example.com:9443 \
      --node-id 1 \
      --registration-token '一次性令牌' \
      --ca-file /root/panel-ca.crt
```

`curl | bash` 不会交互提示；需要外部 MySQL/PostgreSQL 或交互式输入时，先下载脚本再运行 `sudo bash install-panel.sh`。完整参数、生成文件、升级卸载与手动 Compose 部署见 [Docker 部署文档](docs/docker-deployment.md)。

### 方式二：从源码构建

环境要求：Go 1.21+；Node.js 18+（仅重建前端时需要）；OpenSSL（生成本地验证用的主密钥和 PKI）；AWS CLI 可选。

```bash
npm ci --prefix pkg/webadmin/ui && npm run build --prefix pkg/webadmin/ui
go build -o panel ./cmd/panel
go build -o node ./cmd/node

cp -n configs/panel.example.yaml configs/panel.yaml
cp -n configs/node.example.yaml configs/node.yaml
```

示例里的 `/data/...` 是容器内路径；本机直接运行时应改成当前用户可读写的绝对路径。

1. 先启动 panel（缺少主密钥、中间 CA 或 agent 服务端证书时会拒绝启动）：

   ```bash
   ./panel -check-config -config configs/panel.yaml
   ./panel -config configs/panel.yaml
   ```

2. 登录 `http://127.0.0.1:9001/`，创建逻辑节点并签发一次性注册令牌（默认 10 分钟有效、只显示一次），把 `node_id`、令牌、`register_url`、`agent_url` 写入 `configs/node.yaml`。Curl 示例见[管理后台文档](docs/admin-api.md)。

3. 再启动 node。首次启动会本地生成私钥和 CSR，用一次性令牌换取客户端证书，随后使用 mTLS 连接 panel；注册成功后可清空 `registration_token`，node 私钥不会上传到 panel：

   ```bash
   ./node -check-config -config configs/node.yaml
   ./node -config configs/node.yaml
   ```

默认网络边界：

| 进程 | 默认监听 | 用途 |
|---|---|---|
| panel admin | `127.0.0.1:9001`（示例容器内为 `0.0.0.0:9001`） | 管理 UI 和 REST |
| panel agent | `0.0.0.0:9443` | node 首次注册和 mTLS WebSocket |
| node S3 | `0.0.0.0:9000` | AWS CLI、SDK 和 HTTP 客户端 |

## 配置

全部字段及注释见 [configs/panel.example.yaml](configs/panel.example.yaml) 和 [configs/node.example.yaml](configs/node.example.yaml)，启动前用 `-check-config` 校验。要点：

- `master_key_file`：恰好 32 字节的主密钥，加密 S3 secret；不得与 panel 数据库放在同一备份。
- `webadmin.admin_bootstrap_password`：仅首次生成 bcrypt hash 用，之后写入 `password_hash` 并清空。
- credentials、bucket、quota、webhook、rate-limit 等业务配置不写在 node 配置里，由 panel 形成版本化期望状态下发。
- 数据库均支持 SQLite/MySQL/PostgreSQL，DSN 分别写在各自配置中；启动时自动迁移，失败即退出。
- 备份红线（数据库与主密钥分域、node 对象目录与私钥、恢复演练）见[多节点运维文档](docs/multi-node-operations.md)。

## S3 API

```bash
export AWS_ACCESS_KEY_ID=TESTKEY AWS_SECRET_ACCESS_KEY=TESTSECRET AWS_DEFAULT_REGION=us-east-1
EP="--endpoint-url http://127.0.0.1:9000"

aws $EP s3 mb s3://mybucket
aws $EP s3api put-object --bucket mybucket --key docs/readme.txt --body ./README.md
aws $EP s3api get-object --bucket mybucket --key docs/readme.txt ./download.txt
aws $EP s3 presign s3://mybucket/docs/readme.txt --expires-in 300
```

支持范围矩阵、SigV2 兼容开关、public-read 匿名访问矩阵、错误格式和对象事件 Webhook 见 [S3 API 文档](docs/s3-api.md)。

## 管理后台

浏览器访问 panel 的 `http://127.0.0.1:9001/`，单管理员模型。登录 API、Admin API 一览（节点、令牌、credential、期望状态、任务、证书、日志）、证书生命周期和日志契约见[管理后台文档](docs/admin-api.md)。

## 安全要点

- 公网部署时 S3 API 和管理后台是不同安全边界，所有公网入口走 HTTPS。
- 示例配置未启用 admin TLS，管理后台默认以明文 HTTP 提供，只能在内网或可信反向代理后使用。
- 管理后台不要只依赖单密码，建议启用 TOTP 和 captcha；`session_secret` 生产必须替换为随机值。
- 业务直链用 private bucket + 短 TTL presigned URL；`public-read` 只用于真正公开的对象。
- Nginx 反代配置、代理缓存导致的 HEAD 签名失败、公网生产检查清单和监控项见[公网部署文档](docs/public-deployment.md)。

## 文档

- [Docker 部署](docs/docker-deployment.md) — 一键/交互安装、手动 Compose、升级卸载、外部数据库
- [多节点运维](docs/multi-node-operations.md) — 拓扑、注册流程、备份恢复、证书生命周期
- [S3 API](docs/s3-api.md) — 支持范围、签名版本、presigned、匿名访问、Webhook 事件
- [管理后台](docs/admin-api.md) — 登录、Panel Admin API、日志
- [公网部署](docs/public-deployment.md) — 反向代理、检查清单、监控
- [开发与发布](docs/contributing.md) — 构建、冒烟测试、代码结构、发布流程、仓库规则

## 开发

```bash
go vet ./... && go test ./...
```

冒烟测试脚本、代码结构导览和发布流程见[开发与发布文档](docs/contributing.md)。

## License

见仓库 LICENSE。若仓库尚未提供 LICENSE，请在正式分发前补充。
