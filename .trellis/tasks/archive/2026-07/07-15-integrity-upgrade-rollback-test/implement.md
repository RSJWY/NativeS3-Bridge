# 完整性与版本升级回滚专项测试 — 执行计划

## 1. 测试基础设施

- [x] 新增可清理临时资源的 `scripts/test-upgrade-rollback.sh`。
- [x] 脚本构建固定旧基线与当前 node，生成独立配置并选择空闲端口。
- [x] 用 aws-cli 完成旧单体、node、回滚单体三阶段的对象读写和字节比较。
- [x] 增加 SQLite 备份、业务表/Agent 表和回滚兼容断言。

验证：`bash scripts/test-upgrade-rollback.sh`

## 2. 配置、协议与交付检查

- [x] 生成临时主密钥、中间 CA、服务器证书，运行 panel/node `-check-config`。
- [x] 验证 panel 缺失关键材料时失败关闭。
- [x] 定向运行 `pkg/controlproto`、`pkg/panel`、`pkg/nodeagent`、`pkg/db`、`pkg/config` 测试。
- [x] 检查 Dockerfile/Compose 的目标、端口、入口和 healthcheck 二进制路径。
- [x] Docker 引擎可用时构建 `panel` 与 `node` 目标；不可用则记录准确错误。

## 3. 全量质量门禁

- [x] `gofmt` 检查。
- [x] `go vet ./...`。
- [x] `go build ./...`。
- [x] `go test -count=1 ./...`。
- [x] `go test -race -count=1 ./...`。
- [x] `cd pkg/webadmin/ui && npm ci && npm run build`。

## 4. 缺陷处理与复测

- [x] 对实际失败做根因定位并补回归断言。
- [x] 仅修复本任务测试暴露的仓库缺陷。
- [x] 重新运行受影响的定向测试、升级脚本和全量门禁。

## 5. 结果记录

- [x] 在任务目录记录各命令、结果和环境限制。
- [x] 对未能执行的真实 Docker 构建明确标为外部环境阻塞，而不是通过。
- [x] 汇总升级、回滚、数据保真和残余风险。

## 风险文件与回滚点

- `scripts/test-upgrade-rollback.sh`：测试编排，失败不影响运行时代码。
- `Dockerfile` / `docker-compose.example.yml`：若测试发现路径或目标错误，修复后需做静态检查；有 Docker 时必须重建。
- `pkg/db` / `pkg/nodeagent` / `cmd/node`：升级兼容核心，任何修复必须先跑包级测试再跑完整升级演练。
- `pkg/config` / `cmd/panel`：失败关闭和迁移配置，修复后必须正反配置检查。
