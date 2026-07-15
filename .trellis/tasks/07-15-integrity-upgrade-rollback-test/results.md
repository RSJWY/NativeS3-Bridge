# 完整性与版本升级回滚专项测试结果

## 结论

专项测试通过。硬切换的核心路径已用真实进程验证：`5f0be5c` 单体创建的 SQLite 数据与对象可由当前 `cmd/node` 继续服务，控制面不可用时 S3 数据面不受影响；随后回滚到 `5f0be5c` 单体仍可读取升级前和 node 阶段写入的对象。

## 版本升级/回滚验证

- 基线：`5f0be5c`（多节点实现 `1ae6101` 的直接父提交）。
- 自动化：`bash scripts/test-upgrade-rollback.sh`。
- 阶段 1：旧单体创建凭据、Bucket 和 `legacy.txt`，aws-cli GET 字节一致。
- 阶段 2：当前 node 使用同一 DB/data_root，在无注册证书、无可用 panel 时读取 `legacy.txt` 并写入 `node.txt`。
- SQLite：node 启动创建 `*.pre-upgrade-*.bak`；备份可打开、完整性检查为 `ok`、保留业务行且不含 Agent 表。
- 增量迁移：当前 DB 新增 `agent_meta` / `applied_tasks`，原 credentials/buckets/indexes 保持有效。
- 阶段 3：旧单体回滚后可读取 `legacy.txt` 与 `node.txt`，并能继续写入/读取 `rollback.txt`；Agent 表未被删除。

## 配置、PKI 与交付验证

- `bash scripts/test-release-integrity.sh` 通过。
- 使用真实 32 字节主密钥、中间 CA 和服务器证书完成 panel/node 构建与配置检查。
- panel 实际启动并通过 `/healthz`。
- 删除主密钥或中间 CA 后 `panel -check-config` 均失败关闭。
- node 配置携带旧 `admin_addr`、`webadmin`、`rate_limit` 字段时检查通过，这些旧字段被忽略。
- Dockerfile 已拆为独立 `panel-build` / `node-build`；node 构建阶段不再依赖 WebAdmin 或 panel 编译。
- Compose healthcheck 已改为镜像内真实路径 `/usr/local/bin/panel` 与 `/usr/local/bin/node`。
- `bash scripts/test-distribution-contract.sh` 通过。
- `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 的 panel/node 交叉构建通过。

## 质量门禁

- `gofmt`、`git diff --check`：通过。
- `go vet ./...`：通过。
- 默认 Go 1.26.3：`go build ./...`、`go test -count=1 ./...`、`go test -race -count=1 ./...` 均通过。
- 发布基线 Go 1.21.13：`go build ./...`、`go test -count=1 ./...` 均通过。
- Go 1.21 稳定性复测：panel 清理目标测试连续 100 次、panel 全套 5 次通过；日志轮转目标测试连续 100 次通过。
- `npm ci && npm run build`：通过（Vue 类型检查与 Vite 生产构建成功）。
- 协议、mTLS、注册令牌、Secret Key 加密、迁移确认红线、任务幂等/断连行为的定向 Go 测试通过。

## 修复的问题

1. Compose healthcheck 使用不存在的 `/app/panel`、`/app/node`，导致容器被错误标为 unhealthy；已改为 `/usr/local/bin/*` 并增加静态回归检查。
2. `--target node` 实际仍依赖 WebAdmin 构建并同时编译 panel；已拆分独立构建阶段，恢复镜像独立构建契约。
3. panel WebSocket 测试关闭客户端后未等待服务端完成下线 DB 写入，Go 1.21 下会偶发临时目录清理失败/只读 DB；已加入 Hub 下线屏障。
4. lumberjack `MaxBackups` 清理由后台 goroutine 完成，测试立即 glob 会偶发看到两个备份；已改为有界等待异步裁剪。

## 环境限制与残余风险

- Docker Desktop 的 WSL 集成未启用，`docker build` 返回 “The command 'docker' could not be found in this WSL 2 distro.”；因此未声称真实 OCI 镜像构建通过。已完成 Dockerfile/Compose 静态契约、等价 Linux 静态 Go 构建和运行时配置验证。
- `npm audit` 报告 3 个现有依赖漏洞：ECharts `<6.1.0` 1 个 moderate，Vite/esbuild 链 1 个 high + 1 个 moderate。自动修复需要升级到 ECharts 6 / Vite 8（主版本变更），本任务未进行高风险依赖升级；应单独建任务做前端兼容迁移与安全复测。
- Vite 生产构建提示主 bundle 约 641 kB，超过 500 kB 建议阈值；这是性能告警，不影响本次功能与升级验收。
