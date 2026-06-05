# 子任务 6 设计：Vue3 管理界面与 ECharts 仪表盘

> 仅细化本子任务。全局以父任务 `design.md` 为准。

## 1. 后端结构（pkg/webadmin）

```
pkg/webadmin/
├── auth.go        # 单密码登录、session 签发/校验中间件
├── api.go         # 密钥 CRUD + 仪表盘数据 handlers
├── server.go      # 管理端口路由装配（/api/admin/* + SPA 静态）
└── ui/
    ├── embed.go   # //go:embed dist  → fs.FS
    ├── index.html
    ├── package.json
    ├── vite.config.ts
    ├── src/
    │   ├── main.ts
    │   ├── App.vue
    │   ├── router.ts
    │   ├── api/client.ts        # fetch 封装，带 session
    │   ├── views/Login.vue
    │   ├── views/Credentials.vue
    │   └── views/Dashboard.vue
    │   └── components/*.vue
    └── dist/      # 构建产物（提交 .gitkeep 占位）
```

## 2. 管理 API 契约

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| POST | /api/admin/login | `{password}`→set session cookie | 否 |
| POST | /api/admin/logout | 注销 | 是 |
| GET  | /api/admin/credentials | 列表（无 secret 明文） | 是 |
| POST | /api/admin/credentials | `{name,quota_bytes}`→`{access_key,secret_key(一次性)}` | 是 |
| PATCH| /api/admin/credentials/{id} | `{status?,quota_bytes?,name?}` | 是 |
| DELETE| /api/admin/credentials/{id} | 删除 | 是 |
| GET  | /api/admin/dashboard/summary | 总览数字 | 是 |
| GET  | /api/admin/dashboard/usage-ranking | used_bytes 排行 | 是 |
| GET  | /api/admin/dashboard/request-trend?days=N | 按天聚合 | 是 |

- 创建/更新/删除密钥后调用注入的 `credentialStore.Invalidate(accessKey)`。
- 所有响应 JSON；错误 `{error: "..."}` + 合适 HTTP 状态。

## 3. 单密码 session（auth.go）

```
登录: bcrypt.CompareHashAndPassword(cfg.PasswordHash, input)
  成功 → 颁发 session：
    方案 A(默认) 签名 cookie: HMAC(session_secret, payload{exp})
    方案 B JWT（记入 research 二选一）
中间件: 解析 cookie/JWT，校验签名与 exp；失败 401
登录节流: 失败计数 + 固定延迟（如 sleep 500ms），防暴力
首启: PasswordHash 空且 BootstrapPassword 非空 → 生成 hash（写回 or 打印，research 定）
```

## 4. 静态资源服务（embed.go + server.go）

```go
//go:embed dist
var distFS embed.FS

// server.go: 管理端口
//   /api/admin/*  → API handlers（session 中间件）
//   /*            → 从 distFS 提供；找不到文件回退 index.html（SPA 深链）
```

## 5. 前端要点

- ECharts：按需引入（`echarts/core` + 需要的图表/组件）减小体积。
  - Dashboard：`PieChart`（容量使用率/环形）、`BarChart`（用量排行）、`LineChart`（请求趋势）。
- API client：`fetch` 带 `credentials: 'include'`（cookie）；401 → 跳登录。
- Vite dev proxy：
  ```ts
  // vite.config.ts
  server: { proxy: { '/api': 'http://localhost:9001' } }
  ```
- 数字展示：bytes 转人类可读（KB/MB/GB）。
- 路由守卫：未登录访问受保护路由 → 重定向登录页。

## 6. 与其它子任务接口
- 复用 S1 的 `db` 包模型查询。
- 注入 S3 的 `credentialStore.Invalidate`。
- secret_key 生成：随机（access key 20 字符大写字母数字，secret 40 字符 base64/hex），存储形态遵循 S3 research 决定。

## 7. 安全告警
- 启动时若 `tls.enabled=false`：`slog.Warn("admin UI served over plain HTTP; enable TLS for production")`。
- README 写明管理界面应仅在可信内网或经反向代理 TLS 暴露。
