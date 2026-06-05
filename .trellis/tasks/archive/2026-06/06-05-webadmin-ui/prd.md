# 子任务 6：Vue3 管理界面与 ECharts 仪表盘

> 父任务：`06-05-natives3-bridge`。提供单密码管理后台：密钥管理、配额设置、ECharts 仪表盘；前端构建产物用 go:embed 嵌入二进制，保持单文件部署。

## ⛔ 执行者硬约束
需求、技术栈（Vue3+Vite+ECharts）、单密码鉴权、API 契约、嵌入方式为**冻结规格**。不得修改/删减/替换为其它前端框架或图表库。问题写 `research/change-request.md` 上报。详见父任务 `prd.md`。

---

## Goal

实现管理后端 REST API（单密码登录 + session）与 Vue3 前端界面。功能三块：① 密钥管理（创建/启用禁用/删除），② 容量配额设置（按密钥），③ ECharts 仪表盘（容量使用率、各密钥用量排行、请求次数趋势）。前端用 Vite 构建，产物 `dist/` 通过 `go:embed` 嵌入，由管理端口（`server.admin_addr`）提供服务，保持单文件部署。

## 依赖
- 子任务 1（db、config、Credential/RequestStat 模型）。
- 子任务 3（CredentialStore 的 Invalidate；密钥与配额语义）。
- （弱依赖）子任务 5 的 `hooks.Manager.Reload`，若纳入钩子管理界面则调用；否则本任务仅做密钥/配额/仪表盘。

## Requirements

### A. 管理后端（pkg/webadmin）
1. `auth.go`：单密码登录。
   - `POST /api/admin/login` body `{password}` → 校验 bcrypt（`config.webadmin.password_hash`）→ 颁发 session（签名 cookie 或 JWT，TTL=`session_ttl_minutes`）。
   - 首次启动：若 `password_hash` 为空且 `admin_bootstrap_password` 非空，启动时用 bcrypt 生成 hash 并写回/打印提示（记入 research 决定写回文件还是仅打印）。
   - session 中间件保护所有 `/api/admin/*`（login except）。
   - `POST /api/admin/logout` 注销。
2. `api.go`：密钥管理 API（均需 session）。
   - `GET /api/admin/credentials` → 列出密钥（access_key、name、status、quota_bytes、used_bytes、created_at；**不返回 secret_key 明文**，仅创建时返回一次）。
   - `POST /api/admin/credentials` body `{name, quota_bytes}` → 生成 access_key/secret_key（随机），返回**含 secret 的一次性响应**。
   - `PATCH /api/admin/credentials/{id}` body `{status?, quota_bytes?, name?}` → 更新；改后调用 `CredentialStore.Invalidate`。
   - `DELETE /api/admin/credentials/{id}` → 删除；调用 Invalidate。
3. 仪表盘数据 API：
   - `GET /api/admin/dashboard/summary` → 总密钥数、总配额、总已用、总文件量估算（可选）。
   - `GET /api/admin/dashboard/usage-ranking` → 各密钥 used_bytes 排行（取前 N）。
   - `GET /api/admin/dashboard/request-trend?days=30` → 按天聚合的 put/get/delete 计数与 bytes_in/out（来自 request_stats）。
4. 静态资源：`ui/embed.go` 用 `//go:embed dist` 提供前端；管理端口根路径 `/` 返回 SPA，深链回退 `index.html`。
5. 安全：管理 API 仅在 `admin_addr` 暴露；登录失败有节流/延迟（防暴力）；session secret 来自 config；**必须提示**：未配置 TLS 时管理界面为明文 HTTP（在 README 与启动日志告警）。

### B. 前端（pkg/webadmin/ui，Vue3 + Vite）
6. 技术栈：Vue3（组合式 API）+ Vite + 路由（vue-router）+ ECharts（`echarts` npm 包，对应 github.com/apache/echarts）。状态管理可用 Pinia 或轻量 composable（二选一，记入 research）。
7. 页面：
   - 登录页（单密码输入）。
   - 密钥管理页：表格列出密钥；新建（弹窗，创建后显著展示一次性 secret 并提示保存）；启用/禁用切换；删除（二次确认）；编辑配额。
   - 仪表盘页：
     - 容量使用率（环形图：总已用/总配额，或各密钥使用率）。
     - 各密钥用量排行（柱状图）。
     - 请求次数趋势（折线图：近 30 天 put/get/delete）。
8. 开发模式：Vite dev server 代理 `/api` 到 Go 后端（`vite.config.ts` 配 proxy）。
9. 构建：`npm run build` 输出到 `dist/`，被 go:embed 打包。

## 非目标
- 不做多管理员/RBAC（单密码）。
- 不做对象浏览器/文件管理 UI（仅密钥/配额/仪表盘）。
- 不做国际化（中文界面即可）。

## Acceptance Criteria

- [ ] `go build`（在 `dist/` 已构建前提下）产出包含前端的单二进制；`go vet` 通过。
- [ ] 前端 `npm ci && npm run build` 成功，产物落在 `ui/dist/`。
- [ ] 浏览器访问 `http://<admin_addr>/` 显示登录页；正确密码登录成功，错误密码被拒。
- [ ] 未登录访问 `/api/admin/credentials` 返回 401。
- [ ] 创建密钥：响应一次性返回 secret；列表接口此后不再返回 secret 明文。
- [ ] 用界面创建的密钥可直接用于 aws-cli 跑通上传（与 S3 任务联动验证）。
- [ ] 禁用密钥后，该密钥的 S3 请求被拒（验证 Invalidate 生效，缓存失效）。
- [ ] 设置/修改配额后，仪表盘显示更新后的配额与使用率。
- [ ] 仪表盘三图（容量使用率、用量排行、请求趋势）正常渲染，数据来自 request_stats / credentials。
- [ ] 单文件部署：移动二进制到无源码目录仍能正常提供前端与 API。
- [ ] 未配置 TLS 时，启动日志与 README 明确告警管理界面为明文 HTTP。

## Notes
- secret_key 仅创建时明文返回一次，存储形态遵循 S3 任务 research 的决定。
- 若纳入钩子配置界面，改钩子后调用 `hooks.Manager.Reload`；否则本任务范围内不含钩子 UI。
- 状态管理选型、session 实现（cookie vs JWT）记入 research。
