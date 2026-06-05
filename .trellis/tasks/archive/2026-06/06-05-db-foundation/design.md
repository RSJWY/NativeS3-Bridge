# 子任务 1 设计：DB 层与配置基础设施

> 仅细化本子任务内部。全局架构、模型字段、配置格式以父任务 `design.md` 为准，本文件不得与之冲突。

## 1. 包与文件

```
cmd/natives3bridge/main.go     # 入口：config→db.Open→db.Migrate→ready 日志
pkg/config/config.go           # Config 结构 + Load(path) + 校验 + 默认值
pkg/db/db.go                   # Open(driver, dsn) 三驱动分发
pkg/db/models.go               # Credential / RequestStat / HookConfig
pkg/db/migrate.go              # Migrate(db)
configs/config.example.yaml
configs/config.sqlite.yaml
```

## 2. 配置加载契约

```go
// pkg/config/config.go
type Config struct {
    Server   ServerConfig   `yaml:"server"`
    Storage  StorageConfig  `yaml:"storage"`
    Database DatabaseConfig `yaml:"database"`
    WebAdmin WebAdminConfig `yaml:"webadmin"`
    Region   string         `yaml:"region"`
    LogLevel string         `yaml:"log_level"`
}
// 子结构字段严格对应父 design 第 5 节 YAML。

func Load(path string) (*Config, error)  // 读文件→yaml.Unmarshal→applyDefaults→Validate
```

默认值：`region=us-east-1`、`log_level=info`、`storage.metadata_suffix=.s3meta`、`server.s3_addr=0.0.0.0:9000`、`server.admin_addr=0.0.0.0:9001`、`webadmin.session_ttl_minutes=720`。

校验（缺失即返回 error）：`storage.data_root` 非空、`database.driver ∈ {sqlite,mysql,postgres}`、`database.dsn` 非空、`webadmin.session_secret` 非空。

## 3. 数据库分发

```go
// pkg/db/db.go
func Open(driver, dsn string) (*gorm.DB, error) {
    var d gorm.Dialector
    switch driver {
    case "sqlite":   d = sqlite.Open(dsn)
    case "mysql":    d = mysql.Open(dsn)
    case "postgres": d = postgres.Open(dsn)
    default:         return nil, fmt.Errorf("unsupported db driver: %q", driver)
    }
    return gorm.Open(d, &gorm.Config{ /* logger 按 log_level */ })
}
```

驱动包（写入 research 备查，版本锁定）：
- `gorm.io/gorm`
- `gorm.io/driver/sqlite`（CGO；若需纯 Go 用 `glebarez/sqlite`，由执行者在 research 记录选择并保持单文件构建可行）
- `gorm.io/driver/mysql`
- `gorm.io/driver/postgres`
- `gopkg.in/yaml.v3`

> 若 sqlite 驱动选 CGO 版会影响"单文件跨平台"目标，**优先选纯 Go 的 `glebarez/sqlite`**，并在 research 记录决策。这是冻结倾向，偏离需上报。

## 4. 迁移

```go
// pkg/db/migrate.go
func Migrate(db *gorm.DB) error {
    return db.AutoMigrate(&Credential{}, &RequestStat{}, &HookConfig{})
}
```

`RequestStat` 唯一索引：在结构体 tag 用复合唯一索引
`gorm:"uniqueIndex:idx_cred_day"` 同时标注 `CredentialID` 与 `Day`。

## 5. main 装配（本期到此为止）

```go
func main() {
    cfgPath := flag.String("config", "configs/config.yaml", "config file path")
    flag.Parse()
    cfg, err := config.Load(*cfgPath)          // 失败→log.Fatal
    setupSlog(cfg.LogLevel)
    gdb, err := db.Open(cfg.Database.Driver, cfg.Database.DSN)  // 失败→log.Fatal
    db.Migrate(gdb)                            // 失败→log.Fatal
    slog.Info("natives3bridge ready (db only)", "driver", cfg.Database.Driver)
    // TODO(S2): 启动 S3 HTTP server 与 admin server
}
```

## 6. 跨子任务一致性
- 日志：`log/slog` + 文本/JSON handler，级别映射 debug/info/warn/error。
- 错误：`Open`/`Migrate`/`Load` 返回 `error`，main 用 `log.Fatal` 退出（非零码）。
