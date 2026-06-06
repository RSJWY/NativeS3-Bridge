# 子任务 3：管理后台桶管理界面（列表/建删/公开开关）

> 父任务：`06-06-public-access`。本子任务提供管理员设置 bucket ACL 的入口：后端管理 API + 前端 Vue 桶管理页。
> 执行者硬约束见父任务 `prd.md`；冲突写 `research/change-request.md` 上报。

## Goal

新增一组**管理后台**接口（session 鉴权，挂在 admin 端口 `/api/admin/buckets*`）用于列桶/建桶/删桶/设 ACL，并新增 Vue「桶管理」页面：展示桶列表与各自 ACL，支持创建、删除、在 `private` / `public-read` 间切换。设置后 S3 侧匿名访问行为随之变化。

## 依赖
- 子任务 1（`06-06-bucket-model`）：复用 `storage.BucketStore`（List/Create/Delete/SetACL/Invalidate）。本任务**必须**在子任务 1 合并后实现。
- 与子任务 2 可并行；端到端联调需 2 合并后用 UI 复测匿名行为。

## Requirements

### A. 管理 API（admin 端口，session 鉴权）
1. 在 `pkg/webadmin`（`api.go` + `server.go` 注册）新增，全部经 `authenticator.Middleware` 保护：
   - `GET /api/admin/buckets` → 返回桶列表：`[{name, acl, created_at}]`（数据来自 `BucketStore.List`）。
   - `POST /api/admin/buckets` → 建桶。请求体 `{"name": "<bucket>"}`；校验命名（复用 storage 校验）；成功返回 201/200 + 桶对象。非法名 400。
   - `DELETE /api/admin/buckets/{name}` → 删桶；空桶成功，非空返回 409（沿用子任务 1 的 `BucketStore.Delete` 语义）。
   - `PUT /api/admin/buckets/{name}/acl` → 设 ACL。请求体 `{"acl": "private"|"public-read"}`；非法值 400；成功后 `Invalidate(name)`。
2. 响应/错误风格沿用既有 `pkg/webadmin/json.go` 的 `writeJSON` / `writeJSONError`，与 credentials API 一致。
3. 路由注册仿 `server.go` 既有模式：`/api/admin/buckets`（集合）与 `/api/admin/buckets/`（带 name 的 ACL/删除）两个 mux 条目，handler 内解析路径尾段。

### B. 前端 Vue 桶管理页
4. 新增视图 `pkg/webadmin/ui/src/views/Buckets.vue`（范本：`views/Credentials.vue`）：
   - 表格列出桶：名称、ACL（用徽章区分 private/public-read）、创建时间。
   - "新建桶"：输入名称 → 调 POST，刷新列表；展示命名规则提示与错误。
   - 每行操作：删除（二次确认；非空删除失败给出明确提示）；ACL 切换（开关或下拉，private ↔ public-read），调 PUT acl 后刷新。
   - public-read 行显式标注"公开下载"，并提示该桶对象可被匿名 GET。
5. 路由与导航：
   - `pkg/webadmin/ui/src/router.ts` 注册 `/buckets`（受登录保护，与现有路由守卫一致）。
   - 主导航（App.vue 或现有导航处）增加"桶管理"入口。
6. API 客户端：`pkg/webadmin/ui/src/api/client.ts` 增 `listBuckets/createBucket/deleteBucket/setBucketACL`，复用既有请求封装与鉴权处理（401 跳登录等沿用现状）。

### C. 构建与嵌入
7. 前端改动后**重新构建** `npm run build`，更新 `pkg/webadmin/ui/dist/`（go:embed 嵌入）；保证 `go build` 后单二进制内含新页面。
8. 不破坏既有 Login/Credentials/Dashboard 页与既有 API。

## 非目标
- 不实现匿名放行逻辑（子任务 2）。
- 不实现对象级 ACL、不做对象浏览器/文件列表 UI（仅桶级管理）。
- 不实现 S3 协议侧的 PutBucketAcl（父任务裁决：本期 ACL 设置走管理后台，不做 aws-cli ACL 接口）。

## Acceptance Criteria
- [ ] `go build ./...` / `go vet ./...` / `go test ./...` 全绿；前端 `npm run build` 成功且 dist 更新。
- [ ] 登录后访问"桶管理"页，能看到桶列表及各自 ACL 与创建时间。
- [ ] 在页面新建桶：成功后列表出现该桶，S3 侧/磁盘侧（子任务1）随之生效。
- [ ] 将某桶切换为 `public-read` 并保存后，调用 `GET /api/admin/buckets` 反映新 ACL；（子任务2合并后）该桶对象可被匿名 GET。
- [ ] 将桶切回 `private` 后，匿名访问恢复 403（缓存 TTL 内）。
- [ ] 删除空桶成功；删除非空桶前端给出 409 的友好错误提示，不误删数据。
- [ ] 新建非法桶名（如含大写/过短）前端/后端返回明确校验错误。
- [ ] 所有 `/api/admin/buckets*` 未登录访问返回 401/未授权（经 session 中间件）。
- [ ] 既有 Login/Credentials/Dashboard 页与 API 回归正常。

## Notes
- ACL 徽章/开关文案用中文（"私有""公开下载"），与现有后台语言一致。
- 删桶二次确认必做，避免误操作。
- dist 构建产物需提交（仓库现有 dist 已提交，保持一致）。
