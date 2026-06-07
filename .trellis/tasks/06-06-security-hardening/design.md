# 安全加固 — 技术设计

本设计覆盖 PRD 三条加固线（R1 登录节流 / R2 匿名 per-IP 限流 / R3 admin TLS 独立配置）。已冻结决策：R2 用 `golang.org/x/time/rate`；R3 admin TLS 字段为空时继承 `ServerConfig.TLS`；三线一起交付。

## 架构现状锚点（实现时以这些为准）

- 配置：`pkg/config/config.go`。`Config`→`ServerConfig`(:22)→`TLSConfig`(:28)、`WebAdminConfig`(:54)；默认值 `applyDefaults`(:80)，校验 `Validate`(:122)。
- S3 中间件链：`pkg/server/router.go:33` `chain: []Middleware{Recover, Logging, Auth, Quota}`，`NewRouter`(:28) 倒序包裹。`Middleware = func(http.Handler) http.Handler`(:17)。
- 匿名放行：`Auth` 中间件(:177-209) 注入 `auth.AnonymousIdentity()`（`CredentialID=0`，`pkg/auth/identity.go:34`，`IsAnonymous` :38）。
- S3 错误输出：`handlers.WriteS3Error(w, code, httpStatus, resource)`（`pkg/handlers/common.go:21`），文案表 `errorMessage`(:67)。当前无 `SlowDown`。
- admin server：`pkg/webadmin/server.go`，`NewServer`(:25) 当前 `NewAuth(webCfg, serverCfg.TLS.Enabled)`(:26)，`Run`(:57) 据 `s.tls.Enabled` 选 `ListenAndServeTLS`。
- admin 登录：`pkg/webadmin/auth.go`，`Auth` 结构体(:21) 有状态、持 `now func()`；`Login`(:71) 唯一入口，失败仅 `time.Sleep(failureDelay=500ms)`。
- S3 server：`pkg/server/server.go`，`New`(:21)/`Run`(:32)，与 admin 共用 `cfg.TLS`。
- 装配：`cmd/natives3bridge/main.go:88-99`，`server.New(cfg.Server, ...)` 与 `webadmin.NewServer(cfg.Server, cfg.WebAdmin, ...)`。
- 依赖：`go.mod` go 1.21，已有 `golang.org/x/crypto`，**无 `golang.org/x/time`**——R2 需 `go get golang.org/x/time/rate`。

---

## R1 — admin 登录暴力破解节流

### 配置（pkg/config/config.go）
`WebAdminConfig` 新增字段：
```
LoginMaxFailures   int           `yaml:"login_max_failures"`    // 默认 5
LoginLockoutWindow time.Duration `yaml:"login_lockout_window"`  // 默认 15m
```
`applyDefaults` 补默认值（0 → 5 / 15m）。无需 `Validate` 强校验（非安全降级项）。

### 失败计数器（新文件 pkg/webadmin/loginlimiter.go）
进程内、并发安全、带过期清理：
```
type loginLimiter struct {
    mu          sync.Mutex
    maxFailures int
    window      time.Duration
    now         func() time.Time
    entries     map[string]*loginEntry  // key = client IP
}
type loginEntry struct {
    failures   int
    lockedUntil time.Time
    lastSeen    time.Time
}
```
方法：
- `locked(ip) (bool, time.Duration)` — 当前是否锁定 + 剩余时长（供 `Retry-After`）。
- `recordFailure(ip)` — 失败 +1；达到 `maxFailures` 时置 `lockedUntil = now+window` 并清零计数。
- `recordSuccess(ip)` — 删除该 IP entry。
- 过期清理：惰性清理即可——每次操作时顺带删除 `lastSeen` 超过 `window` 且未锁定的 entry；或加一个简单的计数阈值触发全表扫描。避免起后台 goroutine（保持与现有无后台限流器风格一致；如需 goroutine 须随 server 生命周期可停）。**推荐惰性清理**，无 goroutine。
- 注入 `now func()` 便于测试（与 `Auth.now` 一致风格）。

### 接入 Login（pkg/webadmin/auth.go）
- `Auth` 结构体增加 `limiter *loginLimiter` 字段；`NewAuth` 用配置构造。
- `Login` 开头取客户端 IP（见下「客户端 IP 解析」），先 `locked(ip)`：若锁定，返回 `429 Too Many Requests` + `Retry-After: <秒>` + JSON `{"error":"too many login attempts"}`（**不泄露密码是否正确**），直接 return，不做 bcrypt。
- bcrypt 失败分支：`recordFailure(ip)` 后保留现有 `time.Sleep(failureDelay)` 与 `401 invalid password`。
- bcrypt 成功分支：`recordSuccess(ip)` 后照常签发 session。

### Open Question 落定
返回 `Retry-After` 头（秒，向上取整）。

---

## R2 — 匿名下载 per-IP 限流

### 依赖
`go get golang.org/x/time/rate`（写入 go.mod/go.sum）。

### 配置（pkg/config/config.go）
新增顶层 `RateLimit` 段（或并入 `ServerConfig`，推荐独立段便于扩展）：
```
type RateLimitConfig struct {
    AnonymousRPS   float64 `yaml:"anonymous_rps"`    // 默认 10
    AnonymousBurst int     `yaml:"anonymous_burst"`  // 默认 20
    TrustForwarded bool    `yaml:"trust_forwarded"`  // 默认 false
}
```
挂在 `Config.RateLimit`。`applyDefaults`：rps<=0→10，burst<=0→20。`TrustForwarded` 默认 false（安全默认，防伪造 XFF 绕过）。

### per-IP 限流器（新文件 pkg/server/ratelimit.go）
```
type ipRateLimiter struct {
    mu       sync.Mutex
    limiters map[string]*rate.Limiter
    r        rate.Limit
    burst    int
    now      func() time.Time
    lastSeen map[string]time.Time
}
func (l *ipRateLimiter) allow(ip string) bool
```
- `allow`：取/建该 IP 的 `rate.NewLimiter(r, burst)`，调用 `.Allow()`，更新 `lastSeen`。
- 惰性过期清理：操作时删除长时间未见的 IP entry（阈值如 10*window 或固定 10min），防内存无界。
- 客户端 IP 解析见下，受 `TrustForwarded` 控制。

### 中间件（pkg/server/router.go）
- 新增 `AnonRateLimit(cfg RateLimitConfig) Middleware`，内部持一个 `*ipRateLimiter`。
- 仅对**匿名对象读**生效：复用 `!hasCredentials(r) && isAnonymousObjectRead(r, bucket, key)` 判定；带凭证或非匿名读直接放行。
- 触发限流时返回 `503` + S3 XML `SlowDown`：`handlers.WriteS3Error(w, "SlowDown", http.StatusServiceUnavailable, r.URL.Path)`。
- **位置**：插在 `Auth` 之前（`chain` 中放到 `Logging` 与 `Auth` 之间），这样限流在 ACL 查库之前就拦截，省掉无谓的 DB 查询，也对「未签名洪水」更早止血。链变为 `{Recover, Logging, AnonRateLimit, Auth, Quota}`。
  - 注意：此时尚未确定 bucket 是否 public-read，但匿名读判定（方法+路径形态）已足够；对非 public 桶的匿名请求最终仍由 Auth 拒 403，限流只是额外保护，不影响正确性。
- 「匿名读跳过统计」零回退：本中间件不触碰 quota/usage，`Quota` 中间件逻辑不变。

### 错误码（pkg/handlers/common.go）
`errorMessage` 增加：
```
case "SlowDown":
    return "Please reduce your request rate."
```
HTTP 503 由调用方传入，无需改 `WriteS3Error`。

### 装配（main.go / server）
`RateLimitConfig` 需传入 `server.New` → `NewRouter`。改 `New`/`NewRouter` 签名增加该参数，或把它并入已传入的 `config.ServerConfig`。**推荐**：将 `RateLimit` 放进 `ServerConfig`（`ServerConfig.RateLimit RateLimitConfig`），这样 `server.New(cfg.Server, ...)` 签名不变，`NewRouter` 内部从已有路径取用——但 `NewRouter` 当前不接收 `ServerConfig`。最小改动方案：给 `NewRouter` 增加一个 `RateLimitConfig` 参数，`server.New` 透传 `cfg.RateLimit`。实现者择一，保持调用点最少。

---

## R3 — admin 端口 TLS 独立配置

### 配置（pkg/config/config.go）
`ServerConfig` 新增 admin 专属 TLS：
```
AdminTLS TLSConfig `yaml:"admin_tls"`
```
继承语义（冻结决策）：新增方法
```
func (c ServerConfig) EffectiveAdminTLS() TLSConfig {
    if !c.AdminTLS.Enabled && c.AdminTLS.CertFile == "" && c.AdminTLS.KeyFile == "" {
        return c.TLS  // 未配置 admin_tls → 继承 S3 TLS（零破坏现状）
    }
    return c.AdminTLS
}
```
（判定「未配置」以三字段全零为准，避免显式 `enabled:false` 被误判为继承。）

### Validate（pkg/config/config.go:122）
对 S3 TLS 与 admin 有效 TLS 都加校验：`Enabled==true` 但 `CertFile`/`KeyFile` 任一为空 → 返回明确错误，禁止明文静默启动。
```
if c.Server.TLS.Enabled && (c.Server.TLS.CertFile=="" || c.Server.TLS.KeyFile=="") { return err }
adminTLS := c.Server.EffectiveAdminTLS()
if adminTLS.Enabled && (adminTLS.CertFile=="" || adminTLS.KeyFile=="") { return err }
```

### admin server（pkg/webadmin/server.go）
- `NewServer` 内 `effective := serverCfg.EffectiveAdminTLS()`。
- `s.tls = effective`（替换当前 `serverCfg.TLS`）。
- cookie Secure 解耦：`NewAuth(webCfg, effective.Enabled)`（替换 `serverCfg.TLS.Enabled`）。
- `Run` 逻辑不变（已据 `s.tls.Enabled` 分支），warn 文案保留。

### 文档（README / configs/config.example.yaml）
- `config.example.yaml` 增加 `server.admin_tls` 段与注释（留空=继承 server.tls）。
- README 安全章节补两种部署方式：(a) admin 直连 TLS（填 admin_tls cert/key）；(b) 反代部署（admin 明文 + 前置 Nginx/Caddy 终止 TLS，给出 X-Forwarded 与 trust_forwarded 配合说明）。推荐路径任选其一写明。

---

## 客户端 IP 解析（R1 与 R2 共用）

新增共享 helper（位置：R2 在 `pkg/server`，R1 在 `pkg/webadmin`，二者不跨包复用以免引入耦合；各写一份小函数，逻辑一致）：
```
func clientIP(r *http.Request, trustForwarded bool) string {
    if trustForwarded {
        if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
            // 取第一个非空段（最左 = 原始客户端），TrimSpace
        }
        if xrip := r.Header.Get("X-Real-IP"); xrip != "" { return xrip }
    }
    host, _, err := net.SplitHostPort(r.RemoteAddr)
    if err != nil { return r.RemoteAddr }
    return host
}
```
- R2：`trustForwarded` 来自 `RateLimitConfig.TrustForwarded`（默认 false）。
- R1：admin 登录默认也用 `RemoteAddr`；若 admin 走反代，可复用同一 `trust_forwarded` 或单独 webadmin 开关。**推荐**：R1 复用 `Config.RateLimit.TrustForwarded` 以减少配置项（实现时把该布尔传入 `NewServer`/`NewAuth`），design 不强制，实现者保持单一可信源即可。

---

## 文件改动清单

| 文件 | 改动 |
|---|---|
| `pkg/config/config.go` | +`WebAdminConfig` 两字段；+`RateLimitConfig` 与 `Config.RateLimit`；+`ServerConfig.AdminTLS` 与 `EffectiveAdminTLS()`；`applyDefaults` 补默认；`Validate` 补 TLS 一致性校验 |
| `pkg/webadmin/loginlimiter.go` | 新增：per-IP 登录失败计数/锁定，惰性清理 |
| `pkg/webadmin/auth.go` | `Auth` +`limiter`；`NewAuth` 构造；`Login` 接入锁定检查+429+Retry-After+记录成败 |
| `pkg/webadmin/server.go` | `EffectiveAdminTLS` 接入；cookie Secure 与 admin TLS 解耦 |
| `pkg/server/ratelimit.go` | 新增：`ipRateLimiter`（x/time/rate）+`clientIP` |
| `pkg/server/router.go` | +`AnonRateLimit` 中间件；插入 `chain` 于 Auth 之前；`NewRouter` 增 `RateLimitConfig` 参数 |
| `pkg/server/server.go` | `New` 透传 `cfg.RateLimit` 给 `NewRouter` |
| `pkg/handlers/common.go` | `errorMessage` +`SlowDown` |
| `cmd/natives3bridge/main.go` | 装配无需大改（配置经 `cfg.Server`/`cfg.WebAdmin`/`cfg.RateLimit` 流入）；确认 admin TLS 走有效值 |
| `configs/config.example.yaml`、`README` | 文档与示例 |
| `go.mod` / `go.sum` | +`golang.org/x/time` |

---

## 测试策略

- `pkg/webadmin/loginlimiter_test.go`：阈值锁定、窗口过期恢复、成功清零、IP 隔离、并发安全（`-race`）、惰性清理。
- `pkg/webadmin/auth_test.go`（扩展）：锁定后 `Login` 返回 429+Retry-After 且不调用 bcrypt（可用计数或 `now` 桩验证未走 sleep 路径）；成功后计数清零。
- `pkg/server/ratelimit_test.go`：超速返回 false / 限速内 true；`clientIP` 在 trust on/off 下的取值；伪造 XFF 在默认 off 下被忽略；并发安全。
- `pkg/server/router_test.go`（扩展）：匿名 GET 超速返回 503 SlowDown；带签名请求不受限；非匿名读不受限；「匿名读跳过统计」未回退（Quota 行为不变）。
- `pkg/config/config_test.go`（扩展）：`EffectiveAdminTLS` 继承/独立；`Validate` 在 TLS enabled 缺 cert/key 时报错；默认值。
- 全量回归：`go build ./...`、`go test ./...`，重点 `pkg/server`、`pkg/webadmin`、`pkg/config`、`pkg/handlers`。

## 风险与边界

- 中间件顺序：`AnonRateLimit` 放 Auth 之前，对所有「形似匿名读」的请求限流（即便桶非 public），这是更强的保护、不影响最终鉴权结果，已在 R2 设计中说明。
- 内存增长：两个限流器都必须惰性清理，禁止无界 map。测试需覆盖清理。
- 反代 IP 伪造：`trust_forwarded` 默认 false 是安全默认；文档必须警告「仅在受信反代后开启」。
- TLS 继承判定：以三字段全零判「未配置」，防止显式 `enabled:false` 误继承。
