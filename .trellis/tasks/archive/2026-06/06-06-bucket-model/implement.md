# 子任务 1 执行计划：Bucket 模型与桶管理

> 有序清单 + 验证命令 + 回滚点。每步完成后跑对应验证再进入下一步。

## 前置
- [ ] 阅读父 `prd.md` 硬约束、本任务 `prd.md`、`design.md`。
- [ ] 确认工作区干净：`git status`。

## 步骤

### S1. 数据模型与迁移
- [ ] `pkg/db/models.go` 新增 `Bucket` 模型（字段见 design §2 / prd A1）。
- [ ] `pkg/db/migrate.go` 的 `AutoMigrate(...)` 加入 `&Bucket{}`。
- [ ] 验证：`go build ./pkg/db/...`。

### S2. BucketStore
- [ ] 新建 `pkg/storage/bucketmeta.go`，实现 `BucketStore`（GetACL 含 negative cache、Create、Delete、SetACL、List、Invalidate）及 `ErrInvalidACL`、ACL 常量。
- [ ] 复用 `ValidateBucketName` / `ResolveBucketPath`，不重复实现命名校验。
- [ ] 验证：`go build ./pkg/storage/...`。

### S3. BucketStore 单测
- [ ] `pkg/storage/bucketmeta_test.go`：用内存/临时 sqlite + 临时 dataRoot，覆盖
  Create（建目录+记录、幂等）、GetACL（存在/历史桶 exists=false/缓存命中）、
  SetACL（合法/非法值、失效后取到新值）、Delete（空桶成功、非空报错）。
- [ ] 验证：`go test ./pkg/storage/...`。

### S4. CreateBucket / DeleteBucket handler
- [ ] `pkg/handlers/bucket.go`：`BucketHandler` 增 `bucketStore *storage.BucketStore` 字段与构造参；新增 `CreateBucket`、`DeleteBucket`（语义见 design §3/§4），错误走 `WriteS3Error`。
- [ ] 验证：`go build ./pkg/handlers/...`。

### S5. 路由接入
- [ ] `pkg/server/router.go` `dispatch` 的 `key == ""` 分支补 `PUT`→CreateBucket、`DELETE`→DeleteBucket。
- [ ] `server.New` / `NewRouter` 透传 `BucketStore` 到 `BucketHandler`。
- [ ] 验证：`go build ./pkg/server/...`。

### S6. 装配
- [ ] `cmd/natives3bridge/main.go`：构造 `storage.NewBucketStore(gdb, cfg.Storage.DataRoot, ttl)`，注入 server/handler。TTL 用 `storage.DefaultBucketACLCacheTTL`（或 config 字段，若加须更新 config 示例与文档）。
- [ ] 验证：`go build ./... && go vet ./...`。

### S7. 全量校验与冒烟
- [ ] `go test ./...` 全绿。
- [ ] 冒烟（启动 + aws-cli，带 seed 密钥）：
  ```bash
  ./natives3bridge -config configs/config.yaml -seed-access-key K -seed-secret-key S &
  export AWS_ACCESS_KEY_ID=K AWS_SECRET_ACCESS_KEY=S AWS_DEFAULT_REGION=us-east-1
  EP="--endpoint-url http://127.0.0.1:9000"
  aws $EP s3api create-bucket --bucket testpub        # → 200，DB 有记录、磁盘有目录、ACL=private
  aws $EP s3api create-bucket --bucket testpub        # → 200（幂等）
  aws $EP s3api put-object --bucket testpub --key a.txt --body /etc/hostname
  aws $EP s3api delete-bucket --bucket testpub        # → BucketNotEmpty 409（非空）
  aws $EP s3api delete-object --bucket testpub --key a.txt
  aws $EP s3api delete-bucket --bucket testpub        # → 成功，DB 记录与空目录消失
  ```
- [ ] 逐条对照 `prd.md` Acceptance Criteria 勾选。

## 回滚点
- 任一步构建失败：回退该步改动（`git checkout -- <file>`）。
- 整体回滚：移除 router 的 PUT/DELETE bucket 分支、migrate 的 `&Bucket{}`、新增文件；`buckets` 表残留无害。

## 审查门（交付前）
- [ ] 未新增未在 design 列出的顶层包。
- [ ] 未改既有表结构、未改 1:1 映射逻辑。
- [ ] 错误全部走 `WriteS3Error`，无 `fmt.Println`。
- [ ] ACL TTL 数值已在 design/代码常量明确（子任务 2 依赖）。
