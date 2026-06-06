# 子任务 1：Bucket 模型与桶管理（建桶/删桶/ACL 存储）

> 父任务：`06-06-public-access`。本子任务为公开访问能力提供**数据地基**：
> 让 bucket 从"纯磁盘目录"升级为"有 DB 记录、有 ACL 属性"的一等实体。
> 执行者硬约束见父任务 `prd.md`，此处不重复；冲突写 `research/change-request.md` 上报。

## Goal

引入 `Bucket` 数据模型与 ACL 字段，提供建桶/删桶的 S3 标准接口与 bucket ACL 的读写（带缓存），并对既有"历史桶"给出明确兼容策略。本子任务**不实现**匿名放行逻辑（属子任务 2）与前端（属子任务 3），但须提供它们依赖的 ACL 查询能力与管理 API 数据层。

## 依赖
- 基于 `06-05-natives3-bridge` 已完成系统。无同级前置依赖。

## Requirements

### A. 数据模型与迁移
1. 在 `pkg/db/models.go` 新增 `Bucket` 模型（三驱动通用，禁止单驱动专属类型）：
   ```go
   type Bucket struct {
       ID        uint   `gorm:"primaryKey"`
       Name      string `gorm:"uniqueIndex;size:63;not null"` // S3 bucket 命名
       ACL       string `gorm:"size:16;not null;default:private"` // private | public-read
       CreatedAt time.Time
       UpdatedAt time.Time
   }
   ```
2. 在 `pkg/db/migrate.go` 的 `AutoMigrate` 列表中加入 `&Bucket{}`。
3. ACL 取值集合**冻结**为 `private`、`public-read` 两种；其它值写入时拒绝（校验在存储/服务层）。

### B. BucketStore（ACL 读写 + 缓存）
4. 新增 bucket 元数据访问层（建议 `pkg/storage` 或新 `pkg/bucketmeta`，包名由 design.md 定，不得新增未在 design 列出的顶层包）。提供：
   - `GetACL(name string) (acl string, exists bool, err error)`：查 DB；带 TTL 内存缓存（仿 `pkg/auth/credential_store.go`，默认 TTL 60s，可配）。
   - `Create(name string) error`：校验命名（复用 `storage.ValidateBucketName`），DB 插入（默认 ACL=private），并 `MkdirAll` 磁盘目录；幂等性与已存在处理见 design。
   - `Delete(name string) error`：删除策略见 C。
   - `SetACL(name, acl string) error`：校验 acl 取值，更新 DB，失效缓存。
   - `List() ([]Bucket, error)`：列出所有 bucket 记录（供管理 API）。
   - `Invalidate(name string)`：缓存失效（供 SetACL/Delete 后调用，也供管理 API 改动后调用）。

### C. 建桶 / 删桶 S3 接口
5. 在 S3 路由（`pkg/server/router.go` 的 `dispatch`，bucket 非空、key 为空分支）接入：
   - `PUT /{bucket}`（无 key）→ CreateBucket：校验命名，创建 DB 记录 + 磁盘目录，返回 200。已存在时遵循 S3 语义（同一拥有者重复建桶返回 200；命名非法返回 `InvalidBucketName`）。
   - `DELETE /{bucket}`（无 key）→ DeleteBucket：**仅当桶为空**时允许删除，删除 DB 记录与空目录；非空返回 `BucketNotEmpty` (409)。
   - 这两个接口**仍走既有签名鉴权**（不在匿名放行范围）。
6. `CreateBucket`/`DeleteBucket` 错误统一走 `WriteS3Error`，错误码用标准 S3 码（`InvalidBucketName`/`BucketNotEmpty`/`NoSuchBucket` 等）。

### D. 历史桶兼容
7. 对磁盘上已存在、但 DB 无 `Bucket` 记录的目录（06-05 时期遗留）：
   - ACL 查询 `GetACL` 在 DB 无记录时返回 `(acl="", exists=false)`，**调用方（子任务 2）按 private 处理**；不得 panic / 500。
   - `ListBuckets`（S3 列举，已存在于 `pkg/handlers/bucket.go`，靠扫盘）行为**保持不变**（仍扫盘列举，不要求桶必须入库）。
   - 提供一个可选的"对账"思路记入 research（如启动时把扫盘发现的历史桶补登记为 private），是否实现由 design 决定；若不实现须在文档说明历史桶默认 private 的判定路径。

## 非目标
- 不实现匿名放行（子任务 2）。
- 不实现前端界面（子任务 3）。
- 不实现对象级 ACL（父任务已裁决仅做 bucket 级）。

## Acceptance Criteria
- [ ] `go build ./...` / `go vet ./...` / `go test ./...` 全绿。
- [ ] 三驱动均能 AutoMigrate 出 `buckets` 表（至少 sqlite 实测，mysql/postgres 通过字段类型审查确认无单驱动专属类型）。
- [ ] 带签名 `PUT /{bucket}` 建桶：DB 出现记录（ACL=private），磁盘出现目录。
- [ ] 重复建同名桶返回 200（不报错），非法桶名返回 `InvalidBucketName`。
- [ ] 带签名 `DELETE /{bucket}` 删空桶成功；删非空桶返回 `BucketNotEmpty` (409)。
- [ ] `GetACL` 对 DB 无记录的历史桶返回 `exists=false`，不报错；有记录时返回正确 ACL。
- [ ] `SetACL` 写入非法 ACL 值被拒绝；写入合法值后缓存失效、再次 `GetACL` 反映新值。
- [ ] `BucketStore` 有单元测试覆盖 Create/Delete/GetACL/SetACL/缓存失效。
- [ ] 既有 `ListBuckets` 扫盘行为未被破坏（回归通过）。

## Notes
- ACL 缓存 TTL 写入常量或 config，并在 design.md 记录其值——子任务 2 验收"改回 private 后生效"依赖此 TTL 数值。
- 包结构若需新增包，须在 design.md 明确并符合父任务"不得擅自新增顶层包"的约束（建议放在既有 `pkg/storage` 或与 db 同层，避免新顶层包）。
