# 子任务 3 执行计划：管理后台桶管理界面

> 必须在子任务 1（bucket-model）合并后开始。有序清单 + 验证 + 回滚。

## 前置
- [x] 子任务 1 已合并：`storage.BucketStore` 的 List/Create/Delete/SetACL/Invalidate 可用。
- [x] 阅读父 `prd.md`、本任务 `prd.md` + `design.md`。
- [x] Node 18+ 与 `pkg/webadmin/ui` 依赖可用（`npm ci`）。
- [x] `git status` 干净。

## 步骤

### S1. 后端 API 注入
- [x] `pkg/webadmin/api.go`：`API` 增 `buckets *storage.BucketStore` 字段；`NewAPI` 加参。
- [x] `cmd/natives3bridge/main.go` & `webadmin.NewServer`：透传 BucketStore。
- [x] 验证：`go build ./...`。

### S2. 后端 handlers
- [x] `api.go` 新增 `Buckets`（GET list / POST create）与 `BucketByName`（DELETE / PUT acl），结构与错误映射见 design §2/§3，复用 `writeJSON`/`writeJSONError`。
- [x] `server.go` 注册 `/api/admin/buckets` 与 `/api/admin/buckets/`（经 `authenticator.Middleware`）。
- [x] 验证：`go build ./... && go vet ./...`。

### S3. 后端 API 测试
- [x] 仿既有 webadmin 测试风格（若无则新建 `api_test.go`）覆盖：list、create（合法/非法名）、setACL（合法/非法值）、delete（空/非空）。
- [x] 验证：`go test ./pkg/webadmin/...`。

### S4. 前端 API 客户端
- [x] `src/api/client.ts` 增 `listBuckets/createBucket/deleteBucket/setBucketACL`。
- [x] 验证：`cd pkg/webadmin/ui && npx vue-tsc --noEmit`（或现有 lint/typecheck 命令）。

### S5. 前端页面与路由
- [x] 新建 `src/views/Buckets.vue`（范本 Credentials.vue）：列表 + 新建 + ACL 切换 + 删除二次确认。
- [x] `src/router.ts` 注册 `/buckets`（requiresAuth）。
- [x] `App.vue` 导航加"桶管理"入口。
- [x] 验证：`npm run build` 成功。

### S6. 构建嵌入
- [x] `cd pkg/webadmin/ui && npm run build` → 更新 `dist/`。
- [x] `cd ../../.. && go build -o natives3bridge ./cmd/natives3bridge`（确认 dist 被嵌入）。

### S7. 全量校验
- [x] `go build ./... && go vet ./... && go test ./...` 全绿。

### S8. 端到端冒烟（人工 + curl）
```bash
./natives3bridge -config configs/config.yaml &
COOKIE=/tmp/c.txt
curl -s -c $COOKIE -X POST http://127.0.0.1:9001/api/admin/login -H 'Content-Type: application/json' -d '{"password":"<你的密码>"}'

curl -s -b $COOKIE http://127.0.0.1:9001/api/admin/buckets
curl -s -b $COOKIE -X POST http://127.0.0.1:9001/api/admin/buckets -H 'Content-Type: application/json' -d '{"name":"pub"}'
curl -s -b $COOKIE -X PUT http://127.0.0.1:9001/api/admin/buckets/pub/acl -H 'Content-Type: application/json' -d '{"acl":"public-read"}'
curl -s -b $COOKIE http://127.0.0.1:9001/api/admin/buckets         # 确认 pub.acl=public-read
curl -s -o /dev/null -w "no-auth=%{http_code}\n" http://127.0.0.1:9001/api/admin/buckets  # 期望 401（不带 cookie）
```
- [ ] 浏览器打开 `http://127.0.0.1:9001` → 桶管理页：建桶、切公开、删桶可视化验证。（API/嵌入构建烟测通过；未做浏览器人工点击）
- [x] （子任务2 合并后）切 public-read 后用匿名 `curl` 验证 S3 侧 200，切回 private 验证 403。
- [x] 逐条对照 `prd.md` Acceptance Criteria 勾选。（`dist` 提交策略冲突已写 `research/change-request.md`）

## 回滚点
- 任一步失败：回退该文件。
- 整体回滚：移除桶 handler/路由/前端页/导航，重建 dist。

## 审查门
- [x] 所有桶 API 经 session 中间件，未登录 401。
- [x] ACL 只接受 `private`/`public-read` 白名单。
- [x] 删桶有二次确认与非空保护。
- [x] 既有 Login/Credentials/Dashboard 回归正常；dist 已重建并提交。（构建已重建；提交产物被 `.gitignore`/spec 阻止，见 `research/change-request.md`）
