# Docker 部署：Panel 与 Node 独立主机

NativeS3-Bridge 的 panel 与 node 是两个独立部署单元，可以安装在不同主机：

- panel 发布本机管理端口 `127.0.0.1:9001` 和对 node 开放的控制面端口 `9443`；
- node 只发布 S3 端口 `9000`，并主动连接 panel；
- panel 与每个 node 默认各自使用本地 SQLite，不共享数据库或对象目录；
- 镜像直接从 GHCR 拉取：`ghcr.io/rsjwy/natives3-panel` 与 `ghcr.io/rsjwy/natives3-node`。

仓库中的 [`docker-compose.panel.yml`](../docker-compose.panel.yml) 和
[`docker-compose.node.yml`](../docker-compose.node.yml) 是可审阅的手动模板；安装脚本会在目标主机生成等价的 `docker-compose.yml`。

## 1. 前置条件

两台主机均需要：

- Linux；
- Docker Engine 与 Compose v2（`docker compose`）；
- OpenSSL；
- root 权限，或可以通过 `sudo` 写入 `/opt`、调整 UID/GID 并运行 Docker。

网络需要满足：

- node 能访问 panel 的 `9443/tcp`；
- S3 客户端能访问 node 的 `9000/tcp`；
- panel 的 `9001` 默认只绑定 panel 主机的 `127.0.0.1`，远程管理应使用 SSH 隧道或单独配置 HTTPS 反向代理。

## 2. 一键安装

以下命令直接从 GitHub Raw 下载脚本，不需要克隆仓库。生产环境建议先下载并审阅脚本，再执行。

### 2.1 安装 panel

`--panel-host` 必须是 node 实际用于连接 panel 的公网或内网可达域名/IPv4 地址。脚本会把它写入 9443 服务端证书的 SAN，因此不能填写只在 panel 本机有效、而 node 不使用的别名。

```bash
curl -fsSL https://raw.githubusercontent.com/RSJWY/NativeS3-Bridge/main/scripts/install-panel.sh \
  | sudo bash -s -- --panel-host panel.example.com
```

脚本默认安装到 `/opt/natives3-panel`，会：

1. 生成 32 字节 panel 主密钥；
2. 生成部署专用 CA 和匹配 `--panel-host` 的 9443 服务端证书；
3. 生成随机 bootstrap 管理员密码与 session secret；
4. 创建 SQLite 配置、Compose 文件和持久化目录；
5. 把数据文件所有者设为镜像 UID/GID `10001:10001`；
6. 校验 Compose，拉取 GHCR 镜像并启动 panel。

请立即保存脚本输出的 bootstrap 管理员密码。公共 CA 位于：

```text
/opt/natives3-panel/panel-ca.crt
```

CA 私钥 `/opt/natives3-panel/data/pki/intermediate-ca.key` 不得复制到 node。

在 panel 主机检查服务：

```bash
cd /opt/natives3-panel
sudo docker compose ps
sudo docker compose logs panel
```

远程访问本机绑定的管理界面可以使用 SSH 隧道：

```bash
ssh -L 9001:127.0.0.1:9001 root@panel.example.com
```

然后打开 `http://127.0.0.1:9001/`。

panel 首次启动会在日志中输出由 bootstrap 密码生成的 bcrypt `password_hash`。完成首次登录后，应把该 hash 写入 `panel.yaml` 的 `webadmin.password_hash`，清空 `admin_bootstrap_password`，再重启 panel。不要把日志中的 hash 或 bootstrap 密码提交到仓库。

### 2.2 在 panel 创建逻辑 node 和一次性令牌

可以在管理界面创建 node，也可以使用 Admin API。下面的例子假设 panel 在本机 `127.0.0.1:9001`，并使用安装脚本输出的 bootstrap 密码：

```bash
read -r -s -p 'Panel admin password: ' ADMIN_PASSWORD; printf '\n'

curl -fsS -c /tmp/natives3-panel.cookie \
  -H 'Content-Type: application/json' \
  -X POST http://127.0.0.1:9001/api/admin/login \
  --data "{\"password\":\"$ADMIN_PASSWORD\"}"

curl -fsS -b /tmp/natives3-panel.cookie \
  -H 'Content-Type: application/json' \
  -X POST http://127.0.0.1:9001/api/admin/nodes \
  --data '{"display_name":"node-1"}'

curl -fsS -b /tmp/natives3-panel.cookie \
  -X POST http://127.0.0.1:9001/api/admin/nodes/1/tokens
```

记录实际响应中的逻辑 `node_id` 和 `token`。令牌默认 10 分钟有效、只能使用一次，明文只显示一次。

把公共 CA 安全复制到 node 主机，例如：

```bash
sudo install -m 0644 \
  /opt/natives3-panel/panel-ca.crt \
  /tmp/panel-ca.crt
scp /tmp/panel-ca.crt root@node.example.com:/root/panel-ca.crt
rm -f /tmp/panel-ca.crt
```

### 2.3 安装 node

在 node 主机执行，填写 panel 的可达基础 URL、真实逻辑 node ID、一次性令牌和刚复制的公共 CA：

```bash
curl -fsSL https://raw.githubusercontent.com/RSJWY/NativeS3-Bridge/main/scripts/install-node.sh \
  | sudo bash -s -- \
      --panel-url https://panel.example.com:9443 \
      --node-id 1 \
      --registration-token '一次性令牌' \
      --ca-file /root/panel-ca.crt
```

`--panel-url` 必须与 panel 安装时 `--panel-host` 对应，并且 node 能实际访问。脚本会生成：

- `register_url: https://panel.example.com:9443/register`；
- `agent_url: wss://panel.example.com:9443/agent`；
- `ca_file: /data/pki/panel-ca.crt`。

检查首次注册：

```bash
cd /opt/natives3-node
sudo docker compose ps
sudo docker compose logs -f node
```

注册成功后，`data/pki/node.key` 与 `data/pki/node.crt` 会出现在 node 主机。node 私钥由 node 本地生成，不会上传到 panel。随后应编辑 `/opt/natives3-node/node.yaml`，把 `registration_token` 清空，再重启：

```bash
sudoedit /opt/natives3-node/node.yaml
sudo docker compose -f /opt/natives3-node/docker-compose.yml \
  --project-directory /opt/natives3-node up -d node
```

## 3. 安装脚本参数

### Panel

| 参数 | 说明 |
|---|---|
| `--panel-host HOST` | 必填；node 连接 9443 时使用的 DNS 名或 IPv4，写入证书 SAN。 |
| `--install-dir PATH` | 安装目录，默认 `/opt/natives3-panel`；必须是安全的绝对路径。 |
| `--tag TAG` | 镜像 tag，默认 `latest`；生产环境可固定到发布版本。 |
| `--db-driver DRIVER` | 数据库驱动 `sqlite`/`mysql`/`postgres`，默认 `sqlite`；`mysql` 同时兼容 MariaDB。 |
| `--db-dsn DSN` | 数据库 DSN。sqlite 默认 `/data/panel.db`；mysql/postgres 传完整连接串。交互终端下未指定时改为逐项询问 host/port/user/password/dbname（postgres 还问 sslmode），密码明文输入、不校验格式。写入 `panel.yaml` 且不被脚本回显。 |
| `--force` | 删除并重建已存在的安装目录。会破坏该目录内的数据，使用前必须备份。 |
| `--no-start` | 只生成文件并执行 `docker compose config`，不拉取镜像、不启动容器。 |

### Node

| 参数 | 说明 |
|---|---|
| `--panel-url URL` | 必填；形如 `https://panel.example.com:9443`，不能带路径、query 或 fragment。 |
| `--node-id ID` | 必填；在 panel 中创建的正整数逻辑 ID。 |
| `--registration-token TOKEN` | 必填；panel 签发的一次性注册令牌。 |
| `--ca-file PATH` | 必填；可读取的 PEM panel 公共 CA 文件。 |
| `--install-dir PATH` | 安装目录，默认 `/opt/natives3-node`。 |
| `--tag TAG` | 镜像 tag，默认 `latest`。 |
| `--db-driver DRIVER` | 数据库驱动 `sqlite`/`mysql`/`postgres`，默认 `sqlite`；`mysql` 同时兼容 MariaDB。 |
| `--db-dsn DSN` | 数据库 DSN。sqlite 默认 `/data/natives3.db`；mysql/postgres 传完整连接串。交互终端下未指定时改为逐项询问 host/port/user/password/dbname（postgres 还问 sslmode），密码明文输入、不校验格式。写入 `node.yaml` 且不被脚本回显。 |
| `--force` | 删除并重建已存在的安装目录。不会自动迁移旧对象或数据库。 |
| `--no-start` | 只生成和校验文件，不拉取镜像、不启动容器。 |

缺少必填参数时，脚本只在交互终端中提示输入；通过 `curl | bash` 或 CI 非交互执行时会明确报错。脚本默认拒绝覆盖已存在目录。交互终端下，未通过命令行指定的数据库选项也会被询问：sqlite 直接回车用默认路径，`mysql`/`postgres` 改为逐项询问 host/port/user/password/dbname（postgres 额外询问 sslmode），密码明文输入、不校验格式，脚本自动拼出 DSN；非交互模式下未指定则回退到 sqlite 默认路径，但 `mysql`/`postgres` 必须显式提供 `--db-dsn`，否则报错。

### 使用外部 MySQL/MariaDB/PostgreSQL

默认使用容器内 SQLite 文件。`mysql` 驱动同时兼容 MariaDB。如需指向外部数据库，推荐用交互式安装，逐项输入连接信息，脚本自动拼出 DSN（密码明文输入，不校验格式；凭据中的特殊字符会被百分号编码以保证 DSN 结构正确，数据库驱动会解码回原值）：

```bash
sudo ./scripts/install-panel.sh --panel-host panel.example.com
# 选 mysql 或 postgres 后，依次输入 host、port、dbname、user、password（postgres 还问 sslmode）
```

非交互场景（CI、`curl | bash`）或想直接传完整连接串时，用 `--db-driver`/`--db-dsn`：

```bash
# panel -> PostgreSQL
sudo ./scripts/install-panel.sh --panel-host panel.example.com \
  --db-driver postgres \
  --db-dsn "postgres://panel:secret@10.0.0.5:5432/panel?sslmode=disable"

# node -> MySQL/MariaDB
sudo ./scripts/install-node.sh --panel-url https://panel.example.com:9443 \
  --node-id 1 --registration-token TOKEN --ca-file ./panel-ca.crt \
  --db-driver mysql \
  --db-dsn "natives3:secret@tcp(10.0.0.5:3306)/natives3"
```

容器默认无法通过 `localhost` 访问宿主机服务，请使用宿主机内网 IP 或 `host.docker.internal`（Linux 需在 Compose 里加 `extra_hosts`）等可达地址。SQLite 的升级前备份由应用内的 `MigrateConfigured` 处理；MySQL/PostgreSQL 的一致性备份由运维侧用原生工具负责。

也可以用环境变量固定仓库模板的 tag：

```bash
NATIVES3_TAG=v1.2.3 docker compose -f docker-compose.panel.yml config
NATIVES3_TAG=v1.2.3 docker compose -f docker-compose.node.yml config
```

安装脚本使用 `--tag` 把选定版本直接写入目标 Compose 文件。

## 4. 生成的文件

Panel 默认布局：

```text
/opt/natives3-panel/
├── docker-compose.yml
├── panel.yaml
├── panel-ca.crt                      # 可复制给 node 的公共 CA
└── data/
    ├── panel.db                         # 首次启动后创建
    ├── pki/
    │   ├── intermediate-ca.crt          # panel 运行时使用的同一公共 CA
    │   ├── intermediate-ca.key          # 仅保留在 panel
    │   ├── panel-server.crt
    │   └── panel-server.key
    └── secrets/
        └── master.key
```

Node 默认布局：

```text
/opt/natives3-node/
├── docker-compose.yml
├── node.yaml
└── data/
    ├── natives3.db                      # 首次启动后创建
    ├── objects/
    └── pki/
        ├── panel-ca.crt
        ├── node.key                     # 首次注册时生成
        └── node.crt                     # 首次注册后写入
```

私钥、主密钥和配置默认不向其他用户开放。容器以 `10001:10001` 运行，因此持久化数据目录归该 UID/GID 所有。

## 5. 完整手动部署

如果不执行远程脚本，先下载并审阅仓库模板：

可以分别在目标主机通过 GitHub Raw 下载所需模板，无需克隆仓库：

```bash
# Panel 主机
curl -fsSLo docker-compose.yml \
  https://raw.githubusercontent.com/RSJWY/NativeS3-Bridge/main/docker-compose.panel.yml
curl -fsSLo panel.yaml \
  https://raw.githubusercontent.com/RSJWY/NativeS3-Bridge/main/configs/panel.example.yaml

# Node 主机
curl -fsSLo docker-compose.yml \
  https://raw.githubusercontent.com/RSJWY/NativeS3-Bridge/main/docker-compose.node.yml
curl -fsSLo node.yaml \
  https://raw.githubusercontent.com/RSJWY/NativeS3-Bridge/main/configs/node.example.yaml
```

### 5.1 手动准备 panel

在 panel 主机创建独立目录：

```bash
sudo install -d -m 0700 /opt/natives3-panel/data/pki
sudo install -d -m 0700 /opt/natives3-panel/data/secrets
sudo cp docker-compose.yml /opt/natives3-panel/docker-compose.yml
sudo cp panel.yaml /opt/natives3-panel/panel.yaml
sudo openssl rand -out /opt/natives3-panel/data/secrets/master.key 32
```

生成部署 CA：

```bash
sudo openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 \
  -out /opt/natives3-panel/data/pki/intermediate-ca.key
sudo openssl req -x509 -new -sha256 -days 3650 \
  -key /opt/natives3-panel/data/pki/intermediate-ca.key \
  -subj '/CN=NativeS3 Deployment CA' \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -out /opt/natives3-panel/data/pki/intermediate-ca.crt
```

为 node 实际使用的 panel 主机名签发服务端证书。以下示例使用 `panel.example.com`：

```bash
sudo openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 \
  -out /opt/natives3-panel/data/pki/panel-server.key
sudo openssl req -new -sha256 \
  -key /opt/natives3-panel/data/pki/panel-server.key \
  -subj '/CN=panel.example.com' \
  -out /tmp/panel-server.csr
cat >/tmp/panel-server.ext <<'EOF'
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:panel.example.com
EOF
sudo openssl x509 -req -sha256 -days 825 \
  -in /tmp/panel-server.csr \
  -CA /opt/natives3-panel/data/pki/intermediate-ca.crt \
  -CAkey /opt/natives3-panel/data/pki/intermediate-ca.key \
  -CAcreateserial -extfile /tmp/panel-server.ext \
  -out /opt/natives3-panel/data/pki/panel-server.crt
rm -f /tmp/panel-server.csr /tmp/panel-server.ext
```

编辑 `panel.yaml`：保持容器内路径 `/data/...`，使用独立 SQLite `/data/panel.db`，并设置随机 session secret 与 bootstrap 密码：

```bash
openssl rand -hex 16   # admin_bootstrap_password
openssl rand -hex 32   # session_secret
sudoedit /opt/natives3-panel/panel.yaml
```

设置权限并校验启动：

```bash
sudo chown -R 10001:10001 /opt/natives3-panel/data /opt/natives3-panel/panel.yaml
sudo chmod 600 /opt/natives3-panel/panel.yaml \
  /opt/natives3-panel/data/secrets/master.key \
  /opt/natives3-panel/data/pki/intermediate-ca.key \
  /opt/natives3-panel/data/pki/panel-server.key
sudo chmod 644 /opt/natives3-panel/docker-compose.yml \
  /opt/natives3-panel/data/pki/intermediate-ca.crt \
  /opt/natives3-panel/data/pki/panel-server.crt
sudo docker compose --project-directory /opt/natives3-panel \
  -f /opt/natives3-panel/docker-compose.yml config
sudo docker compose --project-directory /opt/natives3-panel \
  -f /opt/natives3-panel/docker-compose.yml pull panel
sudo docker compose --project-directory /opt/natives3-panel \
  -f /opt/natives3-panel/docker-compose.yml up -d panel
```

### 5.2 手动准备 node

先在 panel 创建逻辑 node 和一次性令牌，并只把 `intermediate-ca.crt` 复制到 node。然后：

```bash
sudo install -d -m 0700 /opt/natives3-node/data/objects
sudo install -d -m 0700 /opt/natives3-node/data/pki
sudo cp docker-compose.yml /opt/natives3-node/docker-compose.yml
sudo cp node.yaml /opt/natives3-node/node.yaml
sudo install -m 0644 /root/panel-ca.crt \
  /opt/natives3-node/data/pki/panel-ca.crt
sudoedit /opt/natives3-node/node.yaml
```

至少设置：

```yaml
panel:
  node_id: 1
  register_url: "https://panel.example.com:9443/register"
  agent_url: "wss://panel.example.com:9443/agent"
  registration_token: "一次性令牌"
  cert_file: "/data/pki/node.crt"
  key_file: "/data/pki/node.key"
  ca_file: "/data/pki/panel-ca.crt"
```

保持 `storage.data_root: /data/objects` 和 node 独立 SQLite DSN `/data/natives3.db`。然后设置权限、校验并启动：

```bash
sudo chown -R 10001:10001 /opt/natives3-node/data /opt/natives3-node/node.yaml
sudo chmod 600 /opt/natives3-node/node.yaml
sudo chmod 644 /opt/natives3-node/docker-compose.yml \
  /opt/natives3-node/data/pki/panel-ca.crt
sudo docker compose --project-directory /opt/natives3-node \
  -f /opt/natives3-node/docker-compose.yml config
sudo docker compose --project-directory /opt/natives3-node \
  -f /opt/natives3-node/docker-compose.yml pull node
sudo docker compose --project-directory /opt/natives3-node \
  -f /opt/natives3-node/docker-compose.yml up -d node
```

注册成功后清空 `registration_token`。

## 6. 升级、日志和停止

### 升级到最新镜像

分别在对应主机执行：

```bash
cd /opt/natives3-panel   # 或 /opt/natives3-node
sudo docker compose pull
sudo docker compose up -d
sudo docker compose ps
```

### 固定或切换版本

安装时使用 `--tag v1.2.3`。已有安装直接编辑 `docker-compose.yml` 中的镜像 tag，然后：

```bash
sudo docker compose pull
sudo docker compose up -d
```

panel 与 node 可以独立升级，但控制面协议不兼容的组合会拒绝连接。升级前应查看发布说明并备份。

### 日志与停止

```bash
sudo docker compose logs -f
sudo docker compose down
```

`docker compose down` 不会删除 bind mount 中的 `data/`。重新 `up -d` 会继续使用原数据。

## 7. 卸载

先停止容器：

```bash
sudo docker compose --project-directory /opt/natives3-panel \
  -f /opt/natives3-panel/docker-compose.yml down
sudo docker compose --project-directory /opt/natives3-node \
  -f /opt/natives3-node/docker-compose.yml down
```

确认备份后再删除目录：

```bash
sudo rm -rf /opt/natives3-panel
sudo rm -rf /opt/natives3-node
```

删除 panel 会丢失节点、证书状态和加密后的 S3 credential；删除 node 会丢失对象、sidecar、本地数据库和 node 身份。不要在没有验证备份的情况下执行。

## 8. 外部数据库与备份

快速部署故意不内置 MySQL/PostgreSQL 容器。panel 与 node 都支持外部 MySQL/PostgreSQL，但必须分别修改各自主机的 YAML 配置，并独立管理 DSN、迁移和备份。不要让 panel 与 node 隐式共用 SQLite 文件或对象目录。

最低备份要求：

- panel：`panel.db`、`panel.yaml`、在线 CA、服务端证书，以及单独信任域中的 `master.key`；
- node：`natives3.db`、`objects/`、sidecar、`node.yaml`、`node.key` 与 `node.crt`；
- panel 数据库与主密钥必须分开保存，只有数据库备份不应能解密 S3 secret。

生产级离线 root CA、在线 intermediate 轮换、节点证书撤销、恢复演练和事故处理见
[多节点 mTLS 运维指南](multi-node-operations.md)。

## 9. 安全边界

- panel `9001` 默认只绑定 `127.0.0.1`；不要直接暴露到公网。远程管理请使用 SSH 隧道或 HTTPS 反向代理，并启用适当的 TOTP/captcha/访问控制。
- panel `9443` 使用安装时生成的服务端证书，node 必须通过明确安装的 panel CA 验证它；不要关闭 TLS 验证。
- node 只发布 `9000`，不提供管理端口。公网 S3 应在应用或可信反向代理层启用 HTTPS。
- CA 私钥和 panel 主密钥都不得复制到 node。node 只需要公共 CA。
- 注册令牌只用于首次注册。成功后清空 `node.yaml` 中的令牌，并限制配置文件读取权限。
- `--force` 会删除已有安装目录，不是升级命令。
- `latest` 适合快速体验；需要可重复部署和可控回滚时应固定发布 tag。
