# 子任务 1 执行清单：DB 层与配置基础设施

> 按顺序执行。每步完成即勾选。不得跳步，不得改动规格。

## 步骤

- [x] 1. `go mod init <module-path>`（path 记入 `research/decisions.md`）。设 `go 1.21`。
- [x] 2. 添加依赖：`gorm.io/gorm`、sqlite 驱动（优先 `github.com/glebarez/sqlite` 纯 Go）、`gorm.io/driver/mysql`、`gorm.io/driver/postgres`、`gopkg.in/yaml.v3`。`go mod tidy`。
- [x] 3. 写 `pkg/config/config.go`：Config 及子结构、`Load`、`applyDefaults`、`Validate`。字段严格对齐父 design 第 5 节。
- [x] 4. 写 `pkg/db/models.go`：三个模型，逐字段对齐父 design 第 3 节（索引/默认值/约束/复合唯一索引）。
- [x] 5. 写 `pkg/db/db.go`：`Open` 三驱动分发 + GORM logger 接 slog 级别。
- [x] 6. 写 `pkg/db/migrate.go`：`Migrate`。
- [x] 7. 写 `cmd/natives3bridge/main.go`：装配链 + slog 初始化 + ready 日志 + S2 TODO。
- [x] 8. 写 `configs/config.example.yaml`（含 mysql/postgres dsn 注释）与 `configs/config.sqlite.yaml`。
- [x] 9. 写 `research/decisions.md`：module path、sqlite 驱动选择及理由、各依赖版本。

## 验证命令

```bash
go build ./...
go vet ./...
go run ./cmd/natives3bridge --config configs/config.sqlite.yaml
# 另开终端确认建表（sqlite 文件路径见 dsn）
sqlite3 ./natives3.db ".tables"     # 期望: credentials  hook_configs  request_stats
# 负向：删掉 config 里的 data_root 再跑，应报错非零退出
```

## 完成门
- 上述命令全过；三张表建出；缺失必填项能报错退出。
- 对照本任务 `prd.md` Acceptance Criteria 全部勾选。

## 提交
- 单独 commit：`feat(db): project skeleton, config loader, GORM 3-driver db layer`（风格随仓库 `git log` 调整）。
