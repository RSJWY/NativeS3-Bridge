# 开发指南与发布流程

## 常用命令

```bash
npm ci --prefix pkg/webadmin/ui
npm run build --prefix pkg/webadmin/ui
go build ./cmd/panel ./cmd/node
go vet ./...
go test ./...
```

源码构建完整流程（先前端后 Go、配置准备、启动顺序）见 [README · 快速开始](../README.md#快速开始)。

## 冒烟测试

panel/node 分发合同和本地 PKI 启动路径可用以下脚本验证：

```bash
./scripts/test-release-integrity.sh
./scripts/test-distribution-contract.sh
./scripts/test-upgrade-rollback.sh
```

真实容器注册流程按 [Docker 部署文档](docker-deployment.md)的顺序执行。需要验证 S3 CRUD 时，先在 panel 为目标节点创建 credential、发布期望状态并等待 node 应用，再把返回的 access/secret 用于 `scripts/smoke-test.sh`；不要再使用单体的 `-seed-access-key` 启动参数。

## 代码结构

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

## 发布流程

创建正式 tag 后，GitHub Actions release workflow 会执行：

- `npm ci && npm run build` 构建 Web 管理后台。
- Go 1.21 `go vet ./...`、`go test ./...` 和 `go test -race ./...`。
- 分别交叉编译 panel/node 的 Linux amd64/arm64、macOS amd64/arm64、Windows amd64，共 10 个归档。
- 每个归档包含对应示例配置与 `docs/multi-node-operations.md`，并上传统一的 `checksums.txt`。
- 并行构建并推送 panel/node 的 amd64/arm64 多架构镜像到 GHCR。

镜像地址为：

```text
ghcr.io/rsjwy/natives3-panel:<tag>
ghcr.io/rsjwy/natives3-node:<tag>
```

GitHub Release 归档名为 `natives3-panel-<version>-<os>-<arch>.tar.gz` 和 `natives3-node-<version>-<os>-<arch>.tar.gz`。快速部署默认使用 `latest`；需要可重复部署和可控回滚时应固定正式版本 tag。

每个多架构 tag 指向 OCI image index。除 amd64/arm64 的可运行 manifest 外，BuildKit 还会为每个平台发布最小 provenance attestation；GHCR 可能把这些子 manifest/attestation digest 显示为 untagged，这是正常的索引结构，并非重复发布的镜像。workflow 显式使用 `provenance: mode=min`、`sbom: false`，避免 Action 默认值变化影响产物。

手动运行 `Release` workflow 时可以输入发布 tag。若 tag 不存在，workflow 会基于当前构建提交创建该 tag；如需指定源码，可填写 `source_ref`。

分发合同由 `scripts/test-distribution-contract.sh` 固化：README 必须链接 Docker 部署文档，发布产物、Compose 文件和安装脚本的关键内容都有硬断言。改动相关文件后先跑它。

## 仓库文件与忽略规则

提交前建议检查：

```bash
git status --short
git status --ignored --short
```

应提交：

- 业务代码：`cmd/`、`pkg/`、`configs/*.example.yaml`、`scripts/`。
- 文档：`README.md`、`docs/`、`AGENTS.md`、`.trellis/spec/`、已归档任务记录。
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
