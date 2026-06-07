# 安全加固：admin 端口 TLS 支持、匿名下载 per-IP 限流、登录暴力破解节流

## Goal

把 NativeS3-Bridge 从「功能可用」推向「生产可部署」，堵三个安全口子：admin 端口可独立配置 TLS（或明确反代部署文档）；public-read 桶匿名 GET/HEAD 加 per-IP 限流防滥用；admin 登录接口加失败节流/锁定防暴力破解。三条加固线相对独立，可分别交付与验收。

## User Value

- 攻击者无法对 admin 登录无限撞密码：失败累积后按 per-IP 退避/锁定，暴力破解成本陡增。
- 公开桶不再是任意带宽/IO 放大器：单个来源 IP 的匿名拉取受速率约束，防止被刷爆。
- admin 凭证与 session cookie 不再裸奔明文 HTTP：可单独为 admin 端口启用 TLS，cookie `Secure` 随之生效。

## Confirmed Facts

### 通用架构
- 纯标准库 `net/http`。S3 端用自研中间件链 `pkg/server/router.go:33`：`chain: []Middleware{Recover, Logging, Auth, Quota}`，倒序包裹，是挂限流中间件的天然位置。admin 端用 `http.NewServeMux()`（`pkg/webadmin/server.go`），无中间件链，逐路由 `Auth.Middleware` 包裹。
- 配置为 YAML，`config.Load`（`pkg/config/config.go:61`）。结构体 `Config`/`ServerConfig`/`TLSConfig`/`WebAdminConfig` 全在 `pkg/config/config.go`，默认值在 `applyDefaults`（:80），校验在 `Validate`（:122）。
- 双端口：S3 默认 `0.0.0.0:9000`，admin 默认 `0.0.0.0:9001`，分别在 `cmd/natives3bridge/main.go:88-99` 各起 goroutine。
- 无 `golang.org/x/time/rate` 等限流依赖。

### 1. admin 登录
- 路由 `POST /api/admin/login` → `(*Auth).Login`（`pkg/webadmin/auth.go:71-106`），未经 `Auth.Middleware`（登录前无 session，合理）。
- 仅校验密码（无用户名），bcrypt 比对 `webadmin.password_hash`。session 为自研 HMAC-SHA256 签名 cookie `natives3_admin_session`，HMAC key = `webadmin.session_secret`。cookie `HttpOnly`/`SameSite=Lax`，`Secure` 仅当 TLS 启用时为 true。
- 现有唯一防护：登录失败固定 `time.Sleep(500ms)`（`auth.go:65,83`）。无失败计数、无 per-IP 锁定、无退避。`Auth` 是有状态结构体（持 `now func()`），可在此挂状态。

### 2. 匿名下载
- 匿名放行在 S3 `Auth` 中间件（`pkg/server/router.go:177-223`）：无凭证 + GET/HEAD + bucket/key 非空 + 无受限子资源 + bucket ACL == `public-read` 才放行，注入 `auth.AnonymousIdentity()`（匿名身份 CredentialID=0）。
- GetObject handler `(*ObjectHandler).Get`（`pkg/handlers/object.go:76-111`）流式输出。
- 已冻结规格（见 public-access 红线裁决）：**匿名读跳过 usage/quota 统计**，故 `Quota` 中间件不约束匿名 GET。当前无任何 per-IP 或全局限流中间件。
- 因匿名请求无 credential 身份，限流必须按来源 IP 维度，且需正确解析客户端 IP（反代场景）。

### 3. admin TLS
- admin server 在 `(*Server).Run`（`pkg/webadmin/server.go:57-87`）：TLS 启用走 `ListenAndServeTLS(cert,key)`，否则 `ListenAndServe()` 并打印 warn。
- **当前 S3 与 admin 共用同一个 `ServerConfig.TLS`（`config.go:25`）**，无法独立开关或用不同证书。`TLSConfig` 字段 `enabled`/`cert_file`/`key_file`。admin cookie `Secure` 由 `serverCfg.TLS.Enabled` 驱动（`webadmin/server.go:26`）。
- 已支持 `ListenAndServeTLS`，但无证书自动加载/自签、无 HSTS、无 HTTP→HTTPS 跳转。

## Requirements

### R1 — admin 登录暴力破解节流
- 在 `(*Auth).Login` 上增加按来源 IP 的失败计数与节流/锁定：连续失败达到阈值后，对该 IP 的后续登录尝试在锁定窗口内直接拒绝（返回 `429 Too Many Requests`），不再执行 bcrypt 比对。
- 阈值与锁定窗口可配置（新增 `webadmin` 配置项，提供安全默认值，如失败 5 次锁定 15 分钟）；登录成功后清零该 IP 计数。
- 计数状态为进程内内存即可（重启清零可接受），但必须并发安全（带锁），且有过期清理避免内存无界增长。
- 保留现有失败固定延迟行为或以退避替代，二者不冲突。
- 锁定/拒绝响应不得泄露密码是否正确、账号是否存在等信息。

### R2 — 匿名下载 per-IP 限流
- 为 public-read 桶的匿名 GET/HEAD 增加 per-IP 速率限制：超过配额返回 S3 XML 错误（HTTP `503 SlowDown`，S3 标准节流码）。
- 限流维度为来源 IP；客户端 IP 解析需支持可信反代场景（可配置是否信任 `X-Forwarded-For`/`X-Real-IP`，默认不信任、用 `RemoteAddr`，防伪造绕过）。
- 限流仅作用于匿名（无凭证）对象读路径；带签名的请求不受此限流影响（其约束由配额体系负责），以免影响正常 S3 客户端。
- 速率参数（每 IP 每秒请求数 / 突发桶容量）可配置，提供安全默认值；限流器状态进程内内存、并发安全、过期清理。
- 实现为可挂载到 `pkg/server/router.go` 中间件链的 `Middleware`，且不破坏「匿名读跳过统计」的既有规格。

### R3 — admin 端口 TLS 独立配置
- 允许 admin 端口独立于 S3 端口启用 TLS：新增 admin 专属 TLS 配置（cert/key），不再强制与 S3 共用 `ServerConfig.TLS`。
- 当 admin TLS 启用时，admin server 走 `ListenAndServeTLS`，且 session cookie `Secure` 随 admin TLS 状态置位（解除当前与 `ServerConfig.TLS.Enabled` 的耦合）。
- 向后兼容：未配置 admin 专属 TLS 时，行为回退到现状（沿用或继承现有 `ServerConfig.TLS`），不破坏既有部署。
- 配置缺失/不一致（启用 TLS 但缺 cert/key）须在 `Validate` 阶段报错，不允许静默以明文启动。
- 在 README/部署文档中补充 admin TLS 配置示例与反代部署说明（任选 TLS 直连或反代二者之一作为推荐路径，文档需明确）。

## Acceptance Criteria

### R1 登录节流
- [x] 单元测试：同一 IP 连续失败达到阈值后，下次登录在锁定窗口内返回 `429` 且不调用 bcrypt 比对；锁定窗口过期后恢复可尝试。
- [x] 单元测试：登录成功后该 IP 失败计数清零；不同 IP 的计数互不影响。
- [x] 计数器并发安全（race 测试通过）且具备过期清理，长时间运行不会无界增长。
- [x] 阈值/窗口可经配置覆盖，默认值生效且记录在配置示例中。

### R2 匿名限流
- [x] 单元/中间件测试：同一 IP 匿名 GET 超过速率上限返回 `503` + S3 XML `SlowDown`；限速内正常返回对象。
- [x] 带签名的请求不受匿名限流影响（不返回 SlowDown）。
- [x] 默认不信任 `X-Forwarded-For`，开启信任开关后才按转发头取 IP；伪造转发头在默认配置下无法绕过限流。
- [x] 「匿名读跳过 usage/quota 统计」的既有行为零回退。
- [x] 限流器并发安全、过期清理，速率参数可配置且默认值记录在配置示例中。

### R3 admin TLS
- [x] admin 端口可在 S3 端口不启用 TLS 的情况下独立启用 TLS（反之亦然）。
- [x] admin TLS 启用时 session cookie `Secure=true`；未启用时为 false。
- [x] 启用 admin TLS 但缺 cert/key 时 `config.Validate` 返回明确错误，不会以明文静默启动。
- [x] 未配置 admin 专属 TLS 时行为与现状一致（向后兼容）。
- [x] 部署文档含 admin TLS 直连与反代两种方式的说明与配置示例。

### 通用
- [x] 现有 server/webadmin/config/handlers 测试全部继续通过。
- [x] `go build ./...` 与相关包测试通过。

## Out Of Scope
- 分布式/跨进程共享的限流与登录锁定状态（Redis 等）——本期进程内内存即可。
- 验证码、多因素认证、账号体系（当前仅单密码登录）。
- S3 端口的 TLS 行为变更（仅在为拆分 admin TLS 而触碰共享结构时做最小兼容改造）。
- HSTS、HTTP→HTTPS 自动跳转、证书自动签发/续期（ACME）。
- 对带签名请求的速率限制 / 全局 QPS 限流（仅做匿名 per-IP）。
- S3 `PutBucketAcl` 等 S3 原生 ACL 接口（沿用既定管理后台 ACL 方案）。

## Resolved Decisions (2026-06-07)
- R2 限流算法：**引入 `golang.org/x/time/rate`** 做 per-IP token bucket（每 IP 一个 limiter，带过期清理）。
- R3 向后兼容：admin TLS 字段为空时**继承 `ServerConfig.TLS`**，零破坏现有部署。
- 交付范围：**R1 + R2 + R3 三线在本任务内一起做**，统一验收。

## Open Questions
- R1 锁定响应是否返回 `Retry-After` 头告知剩余锁定时间？倾向返回以便客户端友好，design 阶段定。
