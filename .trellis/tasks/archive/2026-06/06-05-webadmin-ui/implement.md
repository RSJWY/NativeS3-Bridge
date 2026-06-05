# 子任务 6 执行清单：Vue3 管理界面与 ECharts 仪表盘

> 按顺序执行，逐步勾选，不改规格。前后端可交替推进。

## 后端步骤

- [ ] 1. `pkg/webadmin/auth.go`：bcrypt 单密码登录、session 签发与中间件、登录节流、首启 bootstrap。
- [ ] 2. `pkg/webadmin/api.go`：密钥 CRUD（创建返回一次性 secret，列表不返回 secret）、改后 Invalidate。
- [ ] 3. 仪表盘数据 API：summary / usage-ranking / request-trend（聚合 request_stats）。
- [ ] 4. `pkg/webadmin/ui/embed.go`：`//go:embed dist`。
- [ ] 5. `pkg/webadmin/server.go`：管理端口路由（API + SPA 回退）；接入 main，监听 admin_addr；TLS 缺失告警。

## 前端步骤

- [ ] 6. 初始化 Vite + Vue3 工程（package.json / vite.config.ts / tsconfig）；装 vue-router、echarts、状态管理（Pinia 或 composable，记 research）。
- [ ] 7. `api/client.ts`：fetch 封装 + 401 跳登录。
- [ ] 8. `views/Login.vue`：单密码登录。
- [ ] 9. `views/Credentials.vue`：列表、新建弹窗（一次性 secret 展示）、启用/禁用、删除二次确认、编辑配额。
- [ ] 10. `views/Dashboard.vue`：ECharts 三图（环形使用率、柱状排行、折线趋势）+ bytes 人类可读。
- [ ] 11. 路由守卫 + dev proxy 配置。
- [ ] 12. `npm run build` 产物到 `dist/`。
- [ ] 13. research：session 实现选型、状态管理选型、secret 生成格式。

## 验证命令

```bash
# 前端
cd pkg/webadmin/ui && npm ci && npm run build && cd -
# 后端 + 嵌入
go build -o natives3bridge ./cmd/natives3bridge && go vet ./...
./natives3bridge --config configs/config.sqlite.yaml &

# 登录
curl -i -X POST http://localhost:9001/api/admin/login -d '{"password":"<pw>"}' -H 'Content-Type: application/json'
# 未登录访问受保护
curl -i http://localhost:9001/api/admin/credentials      # 401
# 创建密钥（带 session cookie）
curl -i -b cookie.txt -X POST http://localhost:9001/api/admin/credentials -d '{"name":"test","quota_bytes":1048576}'
# 用返回的 ak/sk 跑 S3
export AWS_ACCESS_KEY_ID=<ak> AWS_SECRET_ACCESS_KEY=<sk>
aws --endpoint-url http://localhost:9000 s3 cp /tmp/a.txt s3://test-bucket/x.txt   # 成功
# 禁用后该密钥应被拒（验证 Invalidate）
# 浏览器打开 http://localhost:9001/ 检查三图渲染
# 单文件部署：cp natives3bridge /tmp/ 并在 /tmp 运行，前端仍可访问
```

## 完成门
- 登录/鉴权/密钥CRUD/配额/仪表盘三图全部可用；界面创建的密钥能跑通 S3；禁用即时生效；单文件部署成立；TLS 告警存在。
- 对照 `prd.md` Acceptance Criteria 全勾。

## 提交
- 前端：`feat(webadmin): vue3 admin UI with echarts dashboard`
- 后端：`feat(webadmin): single-password admin API, key/quota management, dashboard data`
