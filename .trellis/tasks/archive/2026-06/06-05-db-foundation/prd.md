# 子任务 1：DB 层与配置基础设施

> 父任务：`06-05-natives3-bridge`。本子任务是整个项目的地基，其它子任务全部依赖它。

## ⛔ 执行者硬约束
本任务的需求、目录、模型字段、配置格式均为**冻结规格**，由规划者制定。执行者**不得修改、删减、替换技术选型**。发现问题写入本任务 `research/change-request.md` 上报，等待规划者裁决，禁止自行实现变更版本。详见父任务 `prd.md` 的硬约束章节。

---

## Goal

搭建 Go 项目骨架，实现配置加载与 GORM 三驱动（SQLite/MySQL/PostgreSQL）数据库抽象，定义并自动迁移数据模型。产出可被其它子任务直接复用的 `config`、`db` 两个包，以及可启动（空转）的 `main.go`。

## 依赖
- 无（首个子任务）。

## Requirements

1. 初始化 Go module（`go.mod`，module path 由执行者按仓库实际设定，记录在 research/）。Go 版本 ≥ 1.21（需 `log/slog`、`go:embed`）。
2. `pkg/config`：从 YAML 文件加载配置，结构与父任务 `design.md` 第 5 节**完全一致**；缺省值填充；启动期校验（必填项缺失即报错退出）。支持 `--config <path>` 命令行参数，默认 `configs/config.yaml`。
3. `pkg/db`：
   - `Open(driver, dsn string) (*gorm.DB, error)`：按 driver 分发到 sqlite / mysql / postgres，三者用同一套 GORM 模型。
   - `Migrate(db *gorm.DB) error`：`AutoMigrate` 父任务 design 第 3 节的三个模型。
   - 模型定义与父任务 `design.md` 第 3 节**逐字段一致**。禁止使用单驱动专属列类型；JSON 数据用 TEXT(string) 列。
   - `RequestStat` 的 `(CredentialID, Day)` 建唯一索引以支持后续 upsert 聚合。
4. `cmd/natives3bridge/main.go`：加载 config → `db.Open` → `db.Migrate` → 打印就绪日志（本子任务暂不起 HTTP 服务，留 TODO 注释给 S2）。
5. 统一日志用 `log/slog`，级别由 `config.log_level` 控制。
6. 提供 `configs/config.example.yaml` 与 `configs/config.sqlite.yaml`（本地开发用）。

## 非目标（本任务不做）
- 不实现任何 S3 API、HTTP 路由、鉴权、前端。仅地基。

## Acceptance Criteria

- [x] `go build ./...` 通过，无编译错误。
- [x] `go vet ./...` 通过。
- [x] 用 `config.sqlite.yaml` 运行 `main.go`，自动创建 SQLite 文件并建出 `credentials` / `request_stats` / `hook_configs` 三张表（可用 sqlite3 查 `.tables` 验证）。
- [x] 将 driver 改为 `mysql` / `postgres`（提供对应 dsn）时，同一份代码能连接并 AutoMigrate 成功（有实例时验证；无实例则代码路径审查确认三分支齐全）。
- [x] 配置缺失必填项（如 `storage.data_root` 为空）时，启动报清晰错误并非零退出。
- [x] 模型字段与父任务 `design.md` 第 3 节逐字段一致（含索引、默认值、约束）。
- [x] 无任何单驱动专属类型（grep 确认无 `jsonb`、无 `datetime(6)` 之类硬编码）。

## Notes
- module path、所选 GORM 驱动包版本写入 `research/` 备查。
- `.trellis/spec/backend/*` 为空模板，约定以本任务与父任务文档为准。
