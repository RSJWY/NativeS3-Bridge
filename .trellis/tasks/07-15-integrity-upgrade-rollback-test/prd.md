# 完整性与版本升级回滚专项测试

## Goal

对多节点 mTLS 硬切换版本执行可重复的完整性验证，重点证明升级前单体部署能够在保留 SQLite 数据库和对象目录的情况下切换到当前 `node`，并能按文档回滚到升级前单体二进制；同时验证当前源码、前端、配置、控制协议和双镜像交付契约。

## Confirmed Facts

- 多节点实现提交为 `1ae6101`，直接父提交 `5f0be5c` 是硬切换前的单体基线，当前 `main` 也指向该基线。
- 升级前入口是 `cmd/natives3bridge`；升级后部署入口是 `cmd/panel` 与 `cmd/node`。
- 节点基础业务表仍由 `pkg/db.MigrateConfigured` 管理，Agent 只通过 `nodeagent.MigrateState` 增加 `agent_meta` 与 `applied_tasks` 表。
- SQLite 升级路径要求迁移前后执行完整性检查，并用 `VACUUM INTO` 创建一致的 `*.pre-upgrade-*.bak`。
- 节点未注册或面板离线时必须继续使用本地数据库提供 S3 数据面。
- 完整回滚要求先停用面板节点，再用升级前单体二进制启动同一数据库和对象目录；旧二进制应忽略新增 Agent 表。
- 当前环境有 Go、Node/npm、OpenSSL 和 aws-cli；Docker CLI 路径存在但 WSL 集成未启用，当前无法连接 Docker 引擎，也没有 Podman/Buildah/Nerdctl。

## Requirements

- 增加一条可重复执行的真实进程级升级/回滚演练：构建 `5f0be5c` 单体二进制，创建旧库和对象，切换到当前 `node`，再回滚旧二进制。
- 升级演练必须验证原凭据、Bucket、对象字节和数据库业务数据在切换后仍可用。
- 当前 `node` 启动必须生成 SQLite 预升级备份，并只增 Agent 状态表；不得删除或改坏原业务表。
- 面板不可用、节点未注册时，当前 `node` 仍必须完成 S3 GET/PUT。
- 回滚后，升级前单体二进制必须能读取升级后的数据库和对象目录，并继续完成 S3 GET/PUT。
- 运行 Go 格式、静态检查、非缓存全量测试、竞态测试与全量构建。
- 运行 WebAdmin 前端类型检查和生产构建。
- 用真实主密钥和 CA 材料验证 `panel -check-config`，并验证缺失关键密钥时失败关闭；验证 `node -check-config` 能读取含旧字段的迁移配置。
- 验证控制协议的兼容协商、未知字段处理、mTLS 注册/握手、配置下发、迁移确认前零写入及确认后基线发布。
- 检查 Dockerfile/Compose 的双目标、暴露端口、入口和 healthcheck 契约；Docker 引擎可用时实际构建两个镜像，不可用时明确记录环境阻塞并完成静态检查。
- 测试发现的仓库内缺陷应在本任务内修复并复测；不修改外部 Docker Desktop/WSL 配置。

## Acceptance Criteria

- [x] `gofmt`、`go vet ./...`、`go build ./...`、`go test -count=1 ./...` 全部通过。
- [x] `go test -race -count=1 ./...` 通过，或记录明确且可复现的工具链/环境限制。
- [x] `npm ci` 与 `npm run build` 在 `pkg/webadmin/ui` 通过。
- [x] 升级前单体 → 当前 node → 升级前单体的真实进程级演练通过。
- [x] 演练前后相同的旧对象字节可读；node 阶段写入的新对象在回滚后仍可读。
- [x] SQLite 预升级备份存在、可重新打开，并保留升级前业务数据。
- [x] Agent 表只在 node 阶段新增，旧单体回滚后基础表和 Agent 表均保持存在。
- [x] node 在无注册证书/无可用面板时仍持续提供 S3 数据面。
- [x] panel/node 配置检查通过；panel 缺少主密钥或 CA 时拒绝通过。
- [x] 协议、mTLS、迁移、任务幂等和 Secret Key 加密相关测试通过。
- [x] Docker/Compose 静态契约通过；若 Docker 引擎可用，panel/node 两个目标镜像均构建成功。
- [x] 所有发现的问题均有修复或明确的残余风险说明，复测结果可追溯。

## Out of Scope

- 修改宿主机 Docker Desktop 或 WSL 集成设置。
- 在没有可用服务实例时强行验证生产 MySQL/PostgreSQL 的备份工具链；本轮只验证代码路径和可用环境中的 SQLite 实际升级。
- 真实跨境网络、长期断网、海量节点和性能压测。
- 发布镜像、推送仓库或部署到生产机器。

## Open Questions

- 无阻塞问题。用户已明确批准创建任务后直接开始执行。
