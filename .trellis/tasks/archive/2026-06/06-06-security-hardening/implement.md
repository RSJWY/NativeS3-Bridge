# 安全加固 — 执行计划

照此顺序执行。每步先读 design.md 对应小节。**禁止破坏「匿名读跳过统计」与现有部署向后兼容。** 每个阶段结束跑 `go build ./...` + 对应包测试。

## 阶段 0：依赖与配置骨架
1. `go get golang.org/x/time/rate`，确认 go.mod/go.sum 更新。
2. 改 `pkg/config/config.go`：
   - `WebAdminConfig` 加 `LoginMaxFailures int`、`LoginLockoutWindow time.Duration`。
   - 新增 `RateLimitConfig{AnonymousRPS float64; AnonymousBurst int; TrustForwarded bool}`，挂 `Config.RateLimit`。
   - `ServerConfig` 加 `AdminTLS TLSConfig` + 方法 `EffectiveAdminTLS()`（三字段全零→继承 `c.TLS`）。
   - `applyDefaults`：LoginMaxFailures→5、LoginLockoutWindow→15m、AnonymousRPS→10、AnonymousBurst→20。
   - `Validate`：S3 TLS 与有效 admin TLS，enabled 但缺 cert/key → 报错。
3. 扩展 `pkg/config/config_test.go`：默认值、`EffectiveAdminTLS` 继承/独立、Validate TLS 校验。
4. 验证：`go build ./...` && `go test ./pkg/config/`。

## 阶段 1：R3 admin TLS（最小、先落地降风险）
1. `pkg/webadmin/server.go`：`NewServer` 内 `effective := serverCfg.EffectiveAdminTLS()`；`s.tls = effective`；`NewAuth(webCfg, effective.Enabled)`。
2. 确认 `Run` 分支与 warn 不变。
3. `configs/config.example.yaml` 加 `server.admin_tls` 段+注释（留空=继承）。
4. 验证：`go build ./...` && `go test ./pkg/webadmin/ ./pkg/config/`。

## 阶段 2：R1 登录节流
1. 新建 `pkg/webadmin/loginlimiter.go`：`loginLimiter`（mu/maxFailures/window/now/entries），方法 `locked/recordFailure/recordSuccess` + 惰性清理。注 `now func()` 便于测试。
2. 新建 `pkg/webadmin/loginlimiter_test.go`：锁定、过期恢复、成功清零、IP 隔离、`-race`、清理。
3. 改 `pkg/webadmin/auth.go`：`Auth` 加 `limiter`；`NewAuth` 构造（用 webCfg 的两字段）；`Login` 开头取 clientIP → `locked` → 429+`Retry-After`+JSON（不泄露密码对错）；bcrypt 失败 `recordFailure`；成功 `recordSuccess`。加本包 `clientIP` helper。
4. 扩展 `pkg/webadmin/auth_test.go`：锁定后 429 且不走 bcrypt；成功清零。
5. 验证：`go build ./...` && `go test -race ./pkg/webadmin/`。

## 阶段 3：R2 匿名 per-IP 限流
1. 新建 `pkg/server/ratelimit.go`：`ipRateLimiter`（map[string]*rate.Limiter + lastSeen + 惰性清理）+ `clientIP(r, trustForwarded)`。
2. 新建 `pkg/server/ratelimit_test.go`：超速/限速、clientIP trust on/off、伪造 XFF 默认忽略、`-race`。
3. 改 `pkg/server/router.go`：加 `AnonRateLimit(cfg RateLimitConfig) Middleware`，仅对 `!hasCredentials && isAnonymousObjectRead` 限流，超限 `WriteS3Error(w,"SlowDown",503,path)`；插入 `chain` 于 Auth 之前；`NewRouter` 增 `RateLimitConfig` 参数。
4. 改 `pkg/server/server.go`：`New` 透传 `cfg.RateLimit` 给 `NewRouter`。
5. 改 `pkg/handlers/common.go`：`errorMessage` 加 `SlowDown` 文案。
6. 扩展 `pkg/server/router_test.go`：匿名 GET 超速 503 SlowDown；带签名不受限；非匿名读不受限；Quota/统计行为不变。
7. 验证：`go build ./...` && `go test -race ./pkg/server/ ./pkg/handlers/`。

## 阶段 4：装配与文档
1. `cmd/natives3bridge/main.go`：确认 admin TLS 走 `EffectiveAdminTLS`、`RateLimit` 流入 `server.New`。若 `NewRouter` 改了签名，更新 `server.New` 调用链。
2. README 安全章节：admin TLS 直连 vs 反代两种部署 + `trust_forwarded` 警告（仅受信反代后开启）。
3. `configs/config.yaml` 占位/示例同步（注意当前明文 bootstrap 密码与占位 session_secret 仅示例，勿当真实凭证）。

## 阶段 5：全量验收
1. `go build ./...`。
2. `go test ./...`（重点 server/webadmin/config/handlers，关键限流/锁定测试加 `-race`）。
3. 逐条对照 prd.md Acceptance Criteria 打勾。
4. 清理临时文件。
5. 交回规划/验收会话做最终验收 + 归档 + 提交（**本任务不自行 commit**，遵循流程由验收环节处理）。

## 红线提醒
- 「匿名读跳过 usage/quota 统计」零回退——R2 中间件不得触碰 quota。
- admin_tls 未配置时行为必须与今天完全一致。
- 两个限流器必须有过期清理，禁止无界 map。
- trust_forwarded 默认 false；锁定/限流响应不得泄露密码或账号信息。
