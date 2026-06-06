# 子任务 3 设计：管理后台桶管理界面

> 细化内部设计，不得与父 `prd.md` 冲突。沿用既有 webadmin 与 Vue 结构。

## 1. 改动落点

```
后端:
  pkg/webadmin/api.go      # +Buckets / BucketByName handlers + 请求/响应结构
  pkg/webadmin/server.go   # +注册 /api/admin/buckets 与 /api/admin/buckets/
  pkg/webadmin/api_test.go # （若有既有测试风格）补桶 API 测试
  cmd/natives3bridge/main.go # webadmin.NewServer 注入 BucketStore
前端:
  pkg/webadmin/ui/src/views/Buckets.vue   # 新页面（范本 Credentials.vue）
  pkg/webadmin/ui/src/router.ts           # +/buckets 路由
  pkg/webadmin/ui/src/api/client.ts       # +bucket API 封装
  pkg/webadmin/ui/src/App.vue             # +导航入口
  pkg/webadmin/ui/dist/**                 # npm run build 产物（嵌入）
```

> webadmin `API` 结构体需新增持有 `*storage.BucketStore`（或其接口）。main.go 把子任务1 的 BucketStore 传入 `NewServer` → `NewAPI`。

## 2. 后端 API 契约

```
GET  /api/admin/buckets
  → 200 [{ "name": "...", "acl": "private|public-read", "created_at": "RFC3339" }]

POST /api/admin/buckets
  body { "name": "mybucket" }
  → 201 { name, acl:"private", created_at }   名非法→400 "invalid bucket name"

DELETE /api/admin/buckets/{name}
  → 200 {ok:true}    非空→409 "bucket not empty"   不存在→404

PUT  /api/admin/buckets/{name}/acl
  body { "acl": "public-read" }
  → 200 { name, acl, created_at }   acl 非法→400
```

- 路由解析：`server.go` 注册两条
  - `mux.Handle("/api/admin/buckets", mw(http.HandlerFunc(api.Buckets)))` → GET(list)/POST(create) 按 method 分。
  - `mux.Handle("/api/admin/buckets/", mw(http.HandlerFunc(api.BucketByName)))` → 解析 `strings.TrimPrefix(path, "/api/admin/buckets/")`：
    - 末段无 `/acl` 且 method=DELETE → 删桶；
    - 形如 `{name}/acl` 且 method=PUT → 设 ACL；
    - 其余 → 405/404。
- 错误用 `writeJSONError`；成功用 `writeJSON`，与 credentials handler 完全同风格。

## 3. handler 实现要点

- `Buckets`：GET → `bucketStore.List()` 映射为响应；POST → decodeJSON 取 name → `bucketStore.Create(name)`（已含命名校验与建目录）→ 返回。
- `BucketByName`：解析 name 与是否 `/acl` 子路径。
  - DELETE → `bucketStore.Delete(name)`，区分 `BucketNotEmpty`/`NoSuchBucket` 错误映射 409/404。
  - PUT acl → decode `{acl}` → `bucketStore.SetACL(name, acl)`（含取值校验 + Invalidate）→ 返回新对象。
- 复用子任务1 的 sentinel error（`storage.ErrInvalidACL` 等）做状态码映射，避免字符串匹配。

## 4. 前端设计

- `Buckets.vue`：`onMounted` 调 `listBuckets`；表格 + "新建"表单 + 行内 ACL 开关 + 删除按钮（`confirm` 二次确认）。
- ACL 切换：`<select>` 或 toggle，change 时调 `setBucketACL(name, acl)`，成功后刷新列表；失败 toast/inline 错误。
- public-read 行加徽章"公开下载"，hover/提示说明"该桶对象可被任何人匿名下载"。
- `client.ts`：四个函数复用现有 `request`/fetch 封装（带 cookie、401 处理）。
- `router.ts`：`{ path: '/buckets', component: Buckets, meta: { requiresAuth: true } }`，沿用现有守卫。
- `App.vue` 导航：在 Credentials/Dashboard 旁加"桶管理"链接。

## 5. 构建与嵌入
- 改动后 `cd pkg/webadmin/ui && npm run build` 生成新 `dist/`；`go build` 经 `//go:embed all:dist` 嵌入。
- 提交更新后的 dist（与仓库现状一致）。

## 6. 兼容与回滚
- 纯新增 API/页面，不动既有 credentials/dashboard 与 S3 侧。
- 回滚：移除新路由/handler/前端页与导航项，重建 dist。
- 安全：所有桶 API 经 session 中间件；未登录 401。设 ACL 只接受白名单值，杜绝注入。
