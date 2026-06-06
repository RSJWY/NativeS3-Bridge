# 子任务 2 执行计划：匿名公开下载鉴权改造

> 必须在子任务 1（bucket-model）合并后开始。有序清单 + 验证 + 回滚。

## 前置
- [ ] 子任务 1 已合并：`storage.BucketStore.GetACL` 可用。
- [ ] 阅读父 `prd.md` 访问矩阵、本任务 `prd.md` + `design.md`。
- [ ] `git status` 干净。

## 步骤

### S1. 匿名身份
- [ ] `pkg/auth/identity.go` 增 `AnonymousAccessKey` 常量、`AnonymousIdentity()`、`IsAnonymous()`。
- [ ] 验证：`go build ./pkg/auth/...`。

### S2. ACLLookup 注入点
- [ ] `pkg/server/router.go`：`NewRouter` / `server.New` 增参 `aclLookup ACLLookup`（`func(bucket)(acl string,exists bool,err error)`）。
- [ ] `cmd/natives3bridge/main.go`：传入 `bucketStore.GetACL`。
- [ ] 验证：`go build ./...`（此步仅接线，行为未变）。

### S3. Auth 中间件匿名分支
- [ ] 按 design §3 流程改造 `Auth`：`hasCredentials` 判定；匿名仅 GET/HEAD + 对象级 + 无写子资源 + ACL=public-read 才注入 `AnonymousIdentity` 放行；否则 403 `AccessDenied`；ACL 查询错误 500。
- [ ] 验证：`go build ./pkg/server/...`。

### S4. commitUsage 短路
- [ ] `pkg/handlers/object.go` 的用量提交路径：`IsAnonymous(id)` 时跳过统计累加（design §5）。
- [ ] 验证：`go build ./pkg/handlers/...`。

### S5. 单测（放行判定矩阵）
- [ ] 新建 `pkg/server/router_test.go`，用 stub `aclLookup` 与 stub authenticator，覆盖矩阵：
  - 匿名 GET 对象 × {public-read→200/放行, private→403, exists=false→403}
  - 匿名 HEAD 对象 public-read → 放行
  - 匿名 GET `/{bucket}`（列举）→ 403
  - 匿名 PUT/DELETE（public-read）→ 403
  - 匿名 GET 带 `?tagging`/`?uploadId` → 403
  - 带 Authorization 头 → 始终走 Verify（放行判定不介入）
- [ ] 验证：`go test ./pkg/server/...`。

### S6. 全量校验
- [ ] `go build ./... && go vet ./... && go test ./...` 全绿。

### S7. 端到端冒烟
```bash
./natives3bridge -config configs/config.yaml -seed-access-key K -seed-secret-key S &
export AWS_ACCESS_KEY_ID=K AWS_SECRET_ACCESS_KEY=S AWS_DEFAULT_REGION=us-east-1
EP="--endpoint-url http://127.0.0.1:9000"
aws $EP s3api create-bucket --bucket pub
echo "native bytes" > /tmp/o.txt
aws $EP s3api put-object --bucket pub --key d/o.txt --body /tmp/o.txt   # 签名上传 OK

# 此时 private：匿名应 403
curl -s -o /dev/null -w "private-anon=%{http_code}\n" http://127.0.0.1:9000/pub/d/o.txt

# 经子任务3的 API 或直接 DB 设 public-read 后（子任务3未完成时可临时用 sqlite 改 acl 验证）
#   UPDATE buckets SET acl='public-read' WHERE name='pub';
curl -s -o /dev/null -w "public-anon=%{http_code}\n" http://127.0.0.1:9000/pub/d/o.txt   # 期望 200
curl -s -r 0-3 http://127.0.0.1:9000/pub/d/o.txt -o - -w "\nrange=%{http_code}\n"          # 期望 206
curl -s -o /dev/null -w "anon-list=%{http_code}\n" http://127.0.0.1:9000/pub                # 期望 403
curl -s -o /dev/null -X PUT --data x -w "anon-put=%{http_code}\n" http://127.0.0.1:9000/pub/d/x.txt  # 期望 403
```
- [ ] 逐条对照 `prd.md` Acceptance Criteria 勾选。

> 注：S7 中"设 public-read"在子任务 3 完成前可临时直接改 DB 验证（记得 ACL 缓存 TTL，或重启进程）。子任务 3 合并后改用管理 API/UI 复测。

## 回滚点
- 任一步构建失败：回退该文件。
- 整体回滚：删除 `Auth` 匿名分支、`AnonymousIdentity`、commitUsage 短路、`aclLookup` 参数 → 恢复"全需签名"。

## 审查门
- [ ] 带凭证路径（含预签名）零行为变化（回归通过）。
- [ ] 匿名仅能 GET/HEAD 单对象；写/列举/管理一律 403。
- [ ] 统一 403 不泄露桶存在性。
- [ ] 匿名读不写统计、不触发 hooks。
