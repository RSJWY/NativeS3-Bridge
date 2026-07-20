# 拆分 Panel 和 Node Docker 部署示例

## Goal

将当前混合在一个文件、依赖本地构建且说明过长的 Docker 部署示例，改成 panel 与 node 可在不同主机上独立使用的快速部署入口，并直接拉取 GitHub Actions 已发布到 GHCR 的镜像。

## Confirmed Facts

- GitHub Actions release workflow 已分别发布 `ghcr.io/rsjwy/natives3-panel:<tag>` 与 `ghcr.io/rsjwy/natives3-node:<tag>`，同时维护 `latest`。
- panel 和 node 是独立部署单元：panel 暴露管理端口 9001 与控制面端口 9443；node 仅暴露 S3 端口 9000，并主动连接 panel。
- panel 与 node 各自需要独立数据库，当前示例默认使用各自本地 SQLite。
- 当前 `docker-compose.example.yml` 同时包含 panel、node、MySQL、PostgreSQL，并通过本地 `build` 构建镜像，不适合跨主机直接部署。
- 当前 README Docker 部署章节包含大量内联 OpenSSL、curl 和数据库 profile 操作，用户明确要求改成 panel/node 各自的快速部署说明。

## Requirements

- 将 Docker Compose 示例拆分为 panel 与 node 两个独立文件，可分别复制到不同主机运行。
- Compose 不再包含本地 `build`，直接使用：
  - `ghcr.io/rsjwy/natives3-panel:latest`
  - `ghcr.io/rsjwy/natives3-node:latest`
- panel Compose 只包含 panel 部署所需内容；node Compose 只包含 node 部署所需内容。
- 默认继续使用 SQLite 和宿主机持久化目录，避免快速部署额外依赖数据库容器。
- 保留现有配置文件挂载、数据目录挂载、端口隔离、健康检查和非 root 文件权限要求。
- README 删除“镜像尚未发布/必须本地构建”等过时描述。
- 提供可通过 `curl`/`bash` 直接执行的 panel 与 node 独立部署脚本，用户无需克隆整个仓库。
- 每个脚本在目标目录生成对应配置文件、`docker-compose.yml` 和必要的数据目录，然后拉取 GHCR 镜像启动对应服务。
- README 只保留极简入口和新部署文档链接，清理现有冗长的 Docker 安装步骤。
- 新增独立 Docker 部署文档，集中说明：一键脚本用法、脚本参数、生成文件、升级/卸载，以及不执行远程脚本时的完整手动方法。
- 详细 PKI、备份恢复、外部 MySQL/PostgreSQL 和运维背景通过部署文档与现有运维文档衔接，不继续堆叠在 README 主流程中。
- node 快速部署必须明确填写 panel 的公网/可达 URL、逻辑 node ID、一次性注册令牌，并安装 panel CA。
- 仓库中的 panel/node Compose 示例仍需拆分，作为脚本生成内容的可审阅参考和手动部署模板。

## Acceptance Criteria

- [ ] 仓库存在两个独立 Compose 示例，panel 与 node 可分别执行 `docker compose up -d`。
- [ ] 两个 Compose 示例均直接拉取正确的 GHCR `latest` 镜像，且不存在 `build:`。
- [ ] panel 示例只发布 9001/9443；node 示例只发布 9000。
- [ ] 两个示例保持独立 SQLite 与持久化目录，不隐式共享数据库或对象数据。
- [ ] panel 与 node 均可通过 GitHub Raw URL 下载并直接执行部署脚本，无需克隆仓库。
- [ ] 部署脚本会在独立目标目录生成 `docker-compose.yml`、对应 YAML 配置和所需持久化目录。
- [ ] README 的旧 Docker 安装长文被清理，只保留简短的一键部署入口和独立部署文档链接。
- [ ] 新增独立 Docker 部署文档，同时覆盖一键脚本与完整手动部署方法。
- [ ] README 不再声称 GHCR panel/node 镜像尚未发布。
- [ ] README 中旧的混合 Compose 文件名、本地 build 命令和数据库 profile 命令被移除或替换。
- [ ] 脚本生成的 Compose 和仓库示例均可通过 `docker compose config` 校验。
- [ ] 现有非 Docker 文档和本地源码构建方式不受影响。

## Out of Scope

- 修改 panel/node 运行时代码或数据库结构。
- 修改 GitHub Actions 镜像发布流程。
- 在快速部署示例中内置 MySQL/PostgreSQL 服务。
- 自动化生产级离线根 CA 生命周期、证书轮换或灾难恢复。

## Resolved Decisions

- panel 部署脚本自动生成内部 CA、9443 服务端证书和 32 字节主密钥，用户不需要提前申请域名证书。
- 安装 panel 时由用户提供 node 实际连接的域名或 IP，脚本将其写入服务端证书 SAN。
- panel 脚本输出可公开复制的 `panel-ca.crt`；node 脚本通过 `--ca-file` 接收该文件。CA 私钥始终只保留在 panel 主机。
- 9001 管理端口默认仅绑定 `127.0.0.1`；公网管理访问由用户另行配置 HTTPS 反向代理。9443 控制面端口对 node 开放。
- 快速部署默认使用 `latest`，同时允许通过环境变量或参数固定镜像 tag。
- 脚本默认安装到 `/opt/natives3-panel` 与 `/opt/natives3-node`，缺少参数时在交互终端提示输入，非交互模式下给出明确错误。

## Open Questions

- 无阻塞问题；按上述安全默认值实施。
