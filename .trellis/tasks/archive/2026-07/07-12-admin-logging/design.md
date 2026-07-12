# Design: 日志落盘与管理端日志查看

## 1. 目标

```text
slog ──► Tee handler
           ├─ stdout（始终）
           ├─ lumberjack file（log.file 非空）
           └─ memory ring（始终，供管理页 / 无文件时）
                    │
Admin UI /logs ──► GET /api/admin/logs ──► ring 和/或 tail(file)
```

## 2. 配置

扩展 `pkg/config`：

```go
type Config struct {
    // ...
    LogLevel string    `yaml:"log_level"`
    Log      LogConfig `yaml:"log"`
}

type LogConfig struct {
    File       string `yaml:"file"`
    MaxSizeMB  int    `yaml:"max_size_mb"`
    MaxBackups int    `yaml:"max_backups"`
    MaxAgeDays int    `yaml:"max_age_days"`
    Compress   bool   `yaml:"compress"`
}
```

默认值：

| 字段 | 默认 |
|------|------|
| file | `""` |
| max_size_mb | `100` |
| max_backups | `5` |
| max_age_days | `0`（不按天删；示例可写 14） |
| compress | `false` |
| ring 容量 | 代码常量 `2000` 条（第一期不做成配置） |

校验（`Validate` / defaults 之后）：

- `file == ""`：忽略轮转字段合法性中与文件相关的 fail-fast（仍可填默认值）
- `file != ""` 且 `max_size_mb < 1` → 配置错误
- `max_backups < 0` → 配置错误（`0` 表示 lumberjack 不保留旧文件，文档写清）
- `max_age_days < 0` → 配置错误

路径：文档推荐 `/state/logs/natives3bridge.log`；代码不强制前缀，README 警告勿写入 `data_root`。

## 3. 落盘与 slog 接线

### 3.1 setupSlog 改造

`cmd/natives3bridge/main.go`：

```text
setupSlog(cfg.LogLevel, cfg.Log) (*logging.Ring, error)
```

1. 解析 level（现有逻辑）
2. `writers := []io.Writer{os.Stdout}`
3. 若 `cfg.Log.File != ""`：
   - `MkdirAll(filepath.Dir(file), 0o750)`
   - `&lumberjack.Logger{Filename, MaxSize: MaxSizeMB, MaxBackups, MaxAge: MaxAgeDays, Compress, LocalTime: true}`
   - append 到 writers
4. `mw := io.MultiWriter(writers...)`
5. 底层 `slog.NewTextHandler(mw, opts)`
6. 包一层 `logging.NewRingHandler(base, ring)`：`Handle` 时 Append ring 再委托 base
7. `slog.SetDefault(...)`
8. 返回 `ring` 供注入 webadmin

依赖：

```text
gopkg.in/natefinch/lumberjack.v2
```

轮转后典型文件：

```text
/state/logs/natives3bridge.log
/state/logs/natives3bridge-2026-07-12T15-04-05.001.log
...
```

### 3.2 Ring 结构

```go
// pkg/logging/ring.go
type Entry struct {
    Time    time.Time      `json:"time"`
    Level   string         `json:"level"`
    Message string         `json:"msg"`
    Attrs   map[string]any `json:"attrs,omitempty"`
}

type Ring struct { /* mutex + 环形缓冲 */ }
func (r *Ring) Append(e Entry)
func (r *Ring) Snapshot(limit int, level, q string) []Entry // 最新在前
```

- Attrs：从 `slog.Record` 收集；跳过 `secret_key` / `password` / `password_hash` 等 key（大小写不敏感匹配）。
- Snapshot：level 过滤（空=全部）；q 子串匹配 msg 或 attrs 文本；limit clamp 1..1000。

### 3.3 与 GORM

`db.SetLogLevel` 仍走 slog → 自动进 MultiWriter + ring。

## 4. 管理 API

### 4.1 路由

```http
GET /api/admin/logs?limit=200&level=&q=
```

`pkg/webadmin/server.go`：

```go
mux.Handle("/api/admin/logs", authenticator.Middleware(http.HandlerFunc(api.Logs)))
```

`API` 增加字段：`logRing *logging.Ring`、`logFile string`。  
`NewServer` / `NewAPI` 由 main 注入 ring 与 `cfg.Log.File`。

### 4.2 响应

```json
{
  "source": "ring",
  "file_enabled": false,
  "limit": 200,
  "entries": [
    {
      "time": "2026-07-12T05:14:03Z",
      "level": "INFO",
      "msg": "s3 request",
      "attrs": {
        "request_id": "...",
        "method": "GET",
        "path": "/bucket/key",
        "elapsed": "1.5ms"
      }
    }
  ]
}
```

**MVP 读取策略：**

1. 若 `logFile != ""`：只读打开文件，从末尾近似 tail 最后 N 行，宽松解析 text slog → `source=file`
2. 否则或 tail/解析失败：`ring.Snapshot` → `source=ring`（可选 `warning` 字段说明回退原因）

Text 解析：尽力取 `time=` / `level=` / `msg=` 与其余 key=value；解析失败则整行作 `msg`，level 默认 INFO。

### 4.3 错误

| 条件 | HTTP |
|------|------|
| 无会话 | 401 |
| 方法非 GET | 405 |
| limit 非法 | clamp 到默认/上限，或 400（推荐 clamp） |
| 内部错误 | 500 |

### 4.4 安全

- 不在公开 ops 暴露。
- 路径仅来自进程配置，禁止 query 指定任意 path。
- ring 过滤敏感 attr；响应不返回 secret。

## 5. 管理 UI

| 项 | 内容 |
|----|------|
| 路由 | `/logs` → `Logs.vue` |
| 侧栏 | `App.vue` 增加「日志」 |
| client | `adminApi.logs({ limit, level, q })` |
| 交互 | 刷新；limit 100/200/500；可选 level、搜索 |
| 展示 | 等宽列表：time / level / msg / attrs |
| 空态 | 无日志提示；`file_enabled=false` 提示仅内存且重启丢失 |

风格与现有中文管理页一致。

## 6. 文档

- `configs/config.example.yaml`、docker example：增加 `log:` 段注释
- README：落盘、轮转、管理页、Docker state 卷示例
- 实现阶段更新 `.trellis/spec/backend/webadmin-guidelines.md`

## 7. 测试计划

| 层 | 用例 |
|----|------|
| config | 默认值；file 非空 + 非法 max_size；file 空可启动 |
| ring | 并发 Append；Snapshot limit/level/q；敏感 key |
| lumberjack | TempDir 小 MaxSize 写超后出现备份（可选集成） |
| API | 无 cookie 401；注入 ring 200；limit clamp |

## 8. 风险

| 风险 | 缓解 |
|------|------|
| 高 QPS ring 锁 | 短临界区；丢 ring 不影响文件/stdout |
| text 解析脆弱 | 失败回退整行 msg；后续可选 JSON handler |
| 磁盘满 | 写失败不拖垮请求 |
| 日志含对象 path | 与现网 access log 一致；仅管理员可见 |

## 9. 明确不做

- DB 审计表  
- 强制默认落盘  
- 用户指定 tail 任意路径  
- WebSocket  
- 与 storage-reconcile 合并实现  

## 10. 实现分期（同一任务内可两步）

1. **Backend**：config + lumberjack + ring + setupSlog + GET /api/admin/logs + tests  
2. **Frontend**：Logs.vue + 侧栏 + client  

两者都完成后再勾 prd AC。
