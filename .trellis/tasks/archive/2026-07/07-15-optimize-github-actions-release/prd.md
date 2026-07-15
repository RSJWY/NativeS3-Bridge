# 优化 GitHub Actions 双镜像发布

## Goal

将 release workflow 从已废弃的单体 `cmd/natives3bridge` 发布流程迁移为符合硬切换架构的 panel/node 双程序、双镜像发布流程，并提升并行度、权限隔离、缓存、制品传递和工作流可验证性。

## Confirmed Facts

- 当前 `.github/workflows/release.yml` 仍交叉编译 `cmd/natives3bridge`，并只发布 `ghcr.io/rsjwy/natives3-bridge`。
- 当前受支持的部署入口是 `cmd/panel` 与 `cmd/node`；单体入口不再是发布目标。
- Dockerfile 已提供独立 `panel` 与 `node` target，且 node target 不依赖 WebAdmin 构建。
- panel 与 node 镜像必须能够独立构建、升级和回滚。
- 当前镜像使用 `linux/amd64,linux/arm64` 多架构 index；BuildKit 默认 provenance 会为每个平台增加 attestation manifest，GHCR 将其显示为 untagged digest。
- provenance 属于构建来源证明而非重复可运行镜像，本次保留并显式配置 `mode=min`。
- 仓库本地镜像命名已使用 `natives3-panel` 与 `natives3-node`，新 GHCR 包沿用该命名。
- 当前 README 的 Docker 部署与发布章节仍描述旧单体镜像，需要同步。

## Requirements

- release workflow 不再构建或发布 `cmd/natives3bridge`。
- GitHub Release 必须分别提供 panel/node 的 Linux、macOS、Windows 二进制压缩包及统一 checksums。
- panel 制品必须包含 panel 示例配置和多节点运维文档；node 制品必须包含 node 示例配置和多节点运维文档。
- GHCR 必须发布两个独立包：`ghcr.io/<owner>/natives3-panel` 与 `ghcr.io/<owner>/natives3-node`。
- 两个镜像均发布版本 tag 与 `latest`，支持 `linux/amd64`、`linux/arm64`。
- Docker build 必须分别指定 `target: panel` / `target: node`，并使用独立 GHA cache scope。
- provenance 必须显式设置为 `mode=min`；SBOM 行为也应显式声明，避免 Action 默认值变化造成不透明产物。
- workflow 应拆分 metadata、quality、artifacts、images、release jobs，使质量门禁完成后制品和镜像可并行构建。
- 权限按 job 最小化：普通任务只读，镜像任务写 packages，Release 任务写 contents。
- 更新官方 Action 主版本到当前稳定大版本，并使用官方/项目维护方 Action。
- README 必须改为 panel/node 部署与新镜像地址，并解释多架构子 manifest/attestation digest。
- 不删除已有 legacy `natives3-bridge` GHCR 包或历史版本。

## Acceptance Criteria

- [x] `release.yml` 只引用 `cmd/panel`、`cmd/node` 和 Docker `panel`/`node` target。
- [x] workflow 生成 10 个二进制归档（2 个组件 × 5 个 OS/arch 目标）及 checksums。
- [x] panel/node 镜像名称、版本 tag、latest、labels、cache scope 和 target 均正确。
- [x] provenance/SBOM 配置显式且 README 对 GHCR digest 结构说明准确。
- [x] workflow job 依赖保证测试失败时不会发布制品、镜像或 Release。
- [x] job 权限满足最小权限原则。
- [x] README 不再把单体镜像作为当前 Docker 部署方式。
- [x] `actionlint` 通过。
- [x] 本地等价的 UI 构建、Go 1.21 vet/test、panel/node 跨平台构建与发布脚本语法验证通过。

## Out of Scope

- 删除或改名现有 GHCR `natives3-bridge` 历史包。
- 实际创建新的 GitHub Release 或推送新的镜像 tag。
- 修改 GitHub 仓库 Packages/Actions 设置。
- 迁移前端依赖主版本或解决 npm audit 主版本升级问题。

## Open Questions

- 无阻塞问题。用户已明确要求直接优化并实施。
