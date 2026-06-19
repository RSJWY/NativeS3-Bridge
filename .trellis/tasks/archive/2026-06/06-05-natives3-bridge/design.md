# NativeS3-Bridge 总体技术设计（父任务）

> 本文件定义**全局架构、跨子任务共享契约、目录结构、数据模型、配置格式**。
> 子任务的 design.md 只细化本子任务内部，不得与本文件冲突；如有冲突以本文件为准，
> 且须按"硬约束"流程上报规划者，不得自行改动。

---

## 1. 全局分层架构

```
┌───────────────────────────────────────────────────────────────┐
│ cmd/natives3bridge/main.go                                     │
│   加载 config → 初始化 DB → 装配各模块 → 启动 HTTP 服务         │
└───────────────────────────────────────────────────────────────┘
        │                                   │
   S3 API 路由 (/{bucket}/{key})       Web 管理路由 (/admin, /api/admin/*)
        │                                   │
┌───────▼───────────┐             ┌─────────▼──────────┐
│ pkg/auth (SigV4)  │             │ pkg/webadmin       │
│   ↓ 验签 + 取身份  │             │  单密码登录/session│
│ pkg/quota         │             │  密钥CRUD/配额/统计 │
│   ↓ 配额检查/累加  │             │  go:embed dist/    │
│ pkg/handlers      │             └─────────┬──────────┘
│   object/multipart│                       │
│   bucket/presigned│                       │
└───────┬───────────┘                       │
        │                                   │
┌───────▼───────────────────────────────────▼──────────┐
│ pkg/storage  (1:1 原生映射核心)                        │
│   path / metadata(sidecar) / multipart(临时分片合并)   │
└───────┬───────────────────────────────────┬──────────┘
        │                                   │
┌───────▼──────────┐              ┌─────────▼──────────┐
│ 本地文件系统      │              │ pkg/db (GORM三驱动)│
│ 原生目录/文件     │              │ credentials/usage  │
└──────────────────┘              └────────────────────┘
        │
┌───────▼──────────┐
│ pkg/hooks (事件) │  PutObject/Complete/Delete 后异步触发
└──────────────────┘
```

---

## 2. 冻结的目录结构

```
NativeS3-Bridge/
├── cmd/
│   └── natives3bridge/
│       └── main.go                 # 唯一入口
├── pkg/
│   ├── config/
│   │   └── config.go               # YAML 加载 + 校验 + 默认值
│   ├── db/
│   │   ├── db.go                   # Open(driver, dsn) 三驱动统一入口
│   │   ├── models.go               # Credential / RequestStat / HookConfig
│   │   └── migrate.go              # AutoMigrate 封装
│   ├── server/
│   │   ├── server.go               # http.Server 装配、路由注册、优雅关闭
│   │   └── router.go               # S3 路径解析 + 方法分发
│   ├── auth/
│   │   ├── sigv4.go                # SigV4 校验
│   │   ├── credential_store.go     # 从 DB 读密钥（带缓存）
│   │   └── identity.go             # Identity 类型 + Authenticator 接口
│   ├── quota/
│   │   └── quota.go                # 配额检查 + 用量累加（事务安全）
│   ├── handlers/
│   │   ├── common.go               # S3 错误码、XML 响应辅助
│   │   ├── bucket.go               # ListBuckets / HeadBucket
│   │   ├── object.go               # PUT/GET/HEAD/DELETE Object, ListObjectsV2
│   │   ├── multipart.go            # Create/UploadPart/Complete/Abort/ListParts
│   │   └── presigned.go            # 预签名 URL 生成与校验
│   ├── storage/
│   │   ├── backend.go              # Backend 接口定义
│   │   ├── path.go                 # bucket/key ↔ 本地路径，安全过滤
│   │   ├── metadata.go             # sidecar 元数据读写（原子写）
│   │   └── multipart.go            # 临时分片存储 + 合并落地
│   ├── hooks/
│   │   ├── event.go                # Event 类型定义
│   │   ├── manager.go              # 注册 + 异步分发 + 重试
│   │   └── webhook.go              # Webhook 实现
│   └── webadmin/
│       ├── api.go                  # 管理 REST API handlers
│       ├── auth.go                 # 单密码登录 + session 中间件
│       └── ui/
│           ├── embed.go            # //go:embed dist
│           ├── src/                # Vue3 源码
│           ├── index.html
│           ├── package.json
│           ├── vite.config.ts
│           └── dist/               # 构建产物（嵌入用；提交占位 .gitkeep）
├── configs/
│   └── config.example.yaml
├── scripts/
│   └── smoke-test.sh               # aws-cli 冒烟测试
├── go.mod
├── go.sum
└── README.md
```

> 包名、文件名、路径为冻结规格。执行者不得新增顶层包或重命名既有包，除非规划者批准。

---

## 3. 共享数据模型（GORM，三驱动通用）

所有模型字段类型必须在 sqlite/mysql/postgres 下均可 AutoMigrate。**禁止使用任何单驱动专属类型**（如 PG 的 jsonb 列类型）；JSON 数据统一用 `string`（TEXT）列存序列化后的 JSON。

```go
// pkg/db/models.go

type Credential struct {
    ID         uint   `gorm:"primaryKey"`
    AccessKey  string `gorm:"uniqueIndex;size:128;not null"`
    SecretKey  string `gorm:"size:256;not null"`   // 明文或加密，见 auth-quota 子任务规格
    Name       string `gorm:"size:128"`            // 备注名
    Status     string `gorm:"size:16;not null;default:enabled"` // enabled / disabled
    QuotaBytes int64  `gorm:"not null;default:0"`   // 容量上限，0 = 不限
    UsedBytes  int64  `gorm:"not null;default:0"`   // 已用容量
    CreatedAt  time.Time
    UpdatedAt  time.Time
}

type RequestStat struct {
    ID           uint   `gorm:"primaryKey"`
    CredentialID uint   `gorm:"index;not null"`
    Day          string `gorm:"size:10;index;not null"` // YYYY-MM-DD（UTC）
    PutCount     int64  `gorm:"not null;default:0"`
    GetCount     int64  `gorm:"not null;default:0"`
    DeleteCount  int64  `gorm:"not null;default:0"`
    BytesIn      int64  `gorm:"not null;default:0"`
    BytesOut     int64  `gorm:"not null;default:0"`
    // 唯一约束 (CredentialID, Day) 用于 upsert 聚合
}

type HookConfig struct {
    ID        uint   `gorm:"primaryKey"`
    URL       string `gorm:"size:512;not null"`
    Events    string `gorm:"size:256;not null"` // 逗号分隔: ObjectCreated,ObjectDeleted
    Enabled   bool   `gorm:"not null;default:true"`
    CreatedAt time.Time
}
```

> 注：本设计采用"按密钥限容量"，无 admins 表（管理端单密码存配置）。Bucket 不建表——bucket 即目录，列举靠扫盘。元数据/标签存 sidecar 文件，不入库。

---

## 4. 共享契约：Authenticator 接口（为对接外部鉴权中心预留）

```go
// pkg/auth/identity.go
type Identity struct {
    CredentialID uint
    AccessKey    string
    QuotaBytes   int64
    UsedBytes    int64
}

type Authenticator interface {
    // Verify 校验请求签名，返回身份；失败返回标准 S3 错误
    Verify(r *http.Request) (*Identity, error)
}
```

第一版实现 `LocalSigV4Authenticator`（密钥来自 DB）。后续可加 `RemoteAuthenticator` 对接外部中心，**接口不变**。

---

## 5. 配置文件格式（冻结）

```yaml
# configs/config.example.yaml
server:
  s3_addr: "0.0.0.0:9000"       # S3 API 监听
  admin_addr: "0.0.0.0:9001"    # Web 管理界面监听（独立端口）
  tls:
    enabled: false
    cert_file: ""
    key_file: ""

storage:
  data_root: "./data"           # 所有 bucket 的根目录
  multipart_tmp: "./data/.multipart"  # 分段上传临时目录（隐藏）
  metadata_suffix: ".s3meta"    # sidecar 后缀

database:
  driver: "sqlite"              # sqlite | mysql | postgres
  dsn: "./natives3.db"          # sqlite: 文件路径
  # mysql:    "user:pass@tcp(127.0.0.1:3306)/natives3?charset=utf8mb4&parseTime=True&loc=Local"
  # postgres: "host=127.0.0.1 user=postgres password=pass dbname=natives3 port=5432 sslmode=disable"

webadmin:
  password_hash: ""             # bcrypt 哈希；为空时首启用 admin_bootstrap_password 生成
  admin_bootstrap_password: ""  # 仅首次启动用于生成 hash，生成后建议清空
  session_secret: "change-me-32bytes-random"
  session_ttl_minutes: 720

region: "us-east-1"             # SigV4 region，默认 us-east-1
log_level: "info"               # debug | info | warn | error
```

---

## 6. 关键数据流

### 6.1 PutObject 主链路
```
HTTP PUT /{bucket}/{key}
 → auth.Verify(r)        // SigV4，DB 取密钥；失败→403 SignatureDoesNotMatch
 → quota.Check(id, size) // UsedBytes+size > QuotaBytes(>0) → 403 配额超限
 → storage.PutObject     // MkdirAll 父目录 → 原子写临时文件 → rename 落地原生文件
 → storage.metadata.Write// 写 sidecar：etag/content-type/x-amz-meta-*/tags
 → quota.AddUsage(id, size) + stat.Incr(put, bytesIn)  // 同事务
 → hooks.Emit(ObjectCreated)  // 异步
 → 返回 200 + ETag header
```

### 6.2 配额与统计的并发安全
- `UsedBytes` 增减与 `RequestStat` 累加在**同一个 DB 事务**内完成。
- 用 `UPDATE credentials SET used_bytes = used_bytes + ? WHERE id = ?` 原子自增，避免读改写竞态。
- DeleteObject 成功后 `used_bytes = max(0, used_bytes - size)`。

### 6.3 路径安全（防逃逸）
- `key` 经 `path.Clean` 规整后，必须仍位于 `data_root/bucket` 之内；出现 `..` 逃逸→400 InvalidArgument。
- bucket 名校验：仅允许 S3 合法 bucket 命名（小写字母数字与连字符，3–63 字符）。

---

## 7. 跨子任务一致性约定（执行者必须共同遵守）

1. **S3 错误响应**统一走 `pkg/handlers/common.go` 的 `WriteS3Error(w, code, httpStatus, resource)`，输出标准 `<Error>` XML，不得各 handler 自造格式。
2. **日志**统一用标准库 `log/slog`，级别由 config 控制；禁止 `fmt.Println` 散落。
3. **时间**统一 UTC；统计按 UTC 日期分桶。
4. **错误传播**：底层返回 `error`，handler 层翻译为 S3 错误码；禁止把内部错误信息原样泄露给客户端。
5. **依赖最小化**：仅允许 gorm + 三 driver、yaml 解析、bcrypt、（可选）aws sdk signer、uuid 库。新增依赖须规划者批准。
