# 子任务 1 设计：Bucket 模型与桶管理

> 细化本子任务内部设计，不得与父任务 `design.md`（06-05）及本任务树父 `prd.md` 冲突。

## 1. 包与文件落点（冻结，不新增顶层包）

复用既有 `pkg/storage` 包，新增文件：

```
pkg/db/models.go        # +Bucket 模型
pkg/db/migrate.go       # AutoMigrate 列表 +&Bucket{}
pkg/storage/bucketmeta.go      # BucketStore：ACL 读写 + 缓存 + Create/Delete/List/SetACL
pkg/storage/bucketmeta_test.go # 单测
pkg/handlers/bucket.go  # +CreateBucket / DeleteBucket handler
pkg/server/router.go    # dispatch 接入 PUT/DELETE /{bucket}
cmd/natives3bridge/main.go     # 装配 BucketStore，注入 server
```

> 选 `pkg/storage` 而非新顶层包：bucket 元数据与磁盘目录强相关，且需复用 `ValidateBucketName`/`ResolveBucketPath`。BucketStore 持有 `*gorm.DB` 与 `data_root`。

## 2. BucketStore 契约

```go
// pkg/storage/bucketmeta.go
const (
    ACLPrivate    = "private"
    ACLPublicRead = "public-read"
    DefaultBucketACLCacheTTL = 60 * time.Second
)

type BucketStore struct {
    db       *gorm.DB
    dataRoot string
    ttl      time.Duration
    mu       sync.RWMutex
    cache    map[string]cachedBucketACL // name -> {acl, exists, expiresAt}
}

func NewBucketStore(gdb *gorm.DB, dataRoot string, ttl time.Duration) *BucketStore

// GetACL: DB 无记录 → (acl="", exists=false, nil)。带 TTL 缓存（命中/未命中都缓存，含 negative cache）。
func (s *BucketStore) GetACL(name string) (acl string, exists bool, err error)
func (s *BucketStore) Create(name string) error            // 校验名→建目录(MkdirAll)→DB FirstOrCreate(ACL=private)
func (s *BucketStore) Delete(name string) error            // 见 §4
func (s *BucketStore) SetACL(name, acl string) error       // 校验 acl∈{private,public-read}→DB Update→Invalidate
func (s *BucketStore) List() ([]db.Bucket, error)
func (s *BucketStore) Invalidate(name string)
```

- 缓存含 **negative cache**（exists=false 也缓存，TTL 相同），避免历史桶/不存在桶每请求查库；`Create`/`SetACL`/`Delete` 后必须 `Invalidate`。
- 校验 acl 非法 → 返回 sentinel error `ErrInvalidACL`。

## 3. CreateBucket / DeleteBucket（S3 语义）

router `dispatch` 中 `key == ""` 分支扩展：

```
case http.MethodPut:                 // 新增
    bucketHandler.CreateBucket(w, req, bucket)
case http.MethodDelete:              // 新增
    bucketHandler.DeleteBucket(w, req, bucket)
```

`BucketHandler` 增持 `*storage.BucketStore`（构造函数加参，main.go 注入）。

- **CreateBucket**：`ValidateBucketName` 失败 → `InvalidBucketName` (400)。`BucketStore.Create` 幂等（`FirstOrCreate`）→ 200，空 body。
- **DeleteBucket**：先判桶是否为空（复用 `backend.ListObjects(bucket,"","","",1)`，有对象 → `BucketNotEmpty` 409）；空则删 DB 记录 + `os.Remove` 空目录 → 204/200。桶不存在 → `NoSuchBucket` 404。

> 两接口在既有中间件链下，签名鉴权照旧（子任务 2 不会放行 bucket 级写操作）。

## 4. 删桶的目录处理

- 仅删**空目录**（`os.Remove`，非 `RemoveAll`），避免误删用户数据。
- sidecar/隐藏文件不计入"非空"判断：判空以 `backend.ListObjects` 的对象视图为准（它本就过滤 `.s3meta` 等）。design 阶段确认 ListObjects 过滤逻辑后据此实现；若存在仅剩 sidecar 的残留导致 `os.Remove` 失败，返回 `BucketNotEmpty` 并记 warn（不静默吞错）。

## 5. 历史桶兼容（判定路径）

```
匿名/鉴权请求 → 子任务2 调 BucketStore.GetACL(name)
  exists=false（历史桶/未登记） → 按 private 处理（匿名拒绝；签名照常）
  exists=true,acl=private        → 私有
  exists=true,acl=public-read    → 公开只读
```

本子任务**不做**启动时自动补登记（避免扫盘副作用与三驱动事务复杂度）；历史桶保持"扫盘可见、ACL 视为 private、可被管理员显式建/设为登记态"。此决定记入本设计，作为子任务 2 的输入契约。

## 6. 数据流：SetACL 生效时延

`SetACL` 立即写 DB 并 `Invalidate(name)` → 本进程下次 `GetACL` 即时反映新值。**跨进程/多实例**场景下，其它实例最多在 TTL（默认 60s）后生效——本项目为单进程单文件部署，单实例即时生效；TTL 仅影响理论多实例，记录备查。

## 7. 兼容性与回滚
- 新增表与新增接口，**不改既有表结构**，对 06-05 行为零破坏。
- 回滚：移除 router 的 PUT/DELETE bucket 分支与 `&Bucket{}` 迁移即可；`buckets` 表残留无害。
