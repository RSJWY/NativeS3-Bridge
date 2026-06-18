# Public secure deployment

## Goal

规划 NativeS3-Bridge 的公网安全部署改造方案，使 S3 API 可以被用户通过直连链接安全访问，同时允许管理后台在公网域名下使用，但不把当前单密码后台和默认配置裸露到互联网风险面中。

## Confirmed Facts

- 用户的目标场景包括：部分业务服务在用户访问时返回 S3 直连地址；管理后台也希望公网可访问。
- 项目当前支持 S3 Header SigV4 与 query 形式预签名 URL 校验。
- Bucket ACL 只有 `private` 与 `public-read`；`public-read` 允许匿名 `GET` / `HEAD` 对象，不允许匿名列桶、写入、删除、tagging、multipart 子资源。
- 匿名 `public-read` 对象读取有应用内按 IP 限流：`rate_limit.anonymous_rps` / `anonymous_burst`。
- 管理后台当前是单密码模型：bcrypt password hash + HMAC session cookie，无用户名、多用户、MFA、OIDC 或 RBAC。
- 管理后台当前登录页与 `/api/admin/login` 只有密码字段，没有 TOTP 或人机验证实现。
- 管理后台登录有同来源 IP 失败锁定：`webadmin.login_max_failures` / `login_lockout_window`。
- 管理端口 TLS 可单独配置：`server.admin_tls`；启用后 session cookie 会设置 `Secure=true`。
- 当前文档建议生产直连管理端口启用 TLS，或通过可信反向代理终止 HTTPS。
- 当前文档明确：明文管理端口只适用于受信任局域网或开发环境。
- 当前 `configs/config.yaml` 是示例级配置：S3 与管理端口监听 `0.0.0.0`，TLS 关闭，存在示例 bootstrap password 与示例 session secret，不适合公网直接使用。
- 管理 listener 当前暴露未认证运维端点：`/healthz`、`/readyz`、`/metrics`。这是既有可观测性任务的明确设计，用于探针和 Prometheus；公网管理域名化时必须重新决定这些端点的暴露边界。
- 当前访问密钥模型没有 AWS IAM 风格的按 bucket/path/action 精细授权；一个启用的 S3 credential 可通过合法签名访问服务支持的 S3 操作。
- 用户已接受 TOTP 作为单用户二次验证方案。

## Requirements

- S3 用户直连访问必须优先使用私有 bucket + 短 TTL 预签名 URL；只有明确公开资源才使用 `public-read`。
- 所有公网入口必须通过 HTTPS，TLS 可由应用本身或可信反向代理/CDN 终止。
- 管理后台公网访问不能依赖裸应用单密码作为唯一防线；本任务选择保留单用户模型，但在应用内增加二次验证、防暴力破解和人机验证能力。
- 管理后台与 S3 API 应作为不同安全边界处理，推荐不同域名、不同反代路由、不同限流策略。
- 管理后台公网方案必须处理 `/healthz`、`/readyz`、`/metrics` 的访问边界，避免把运行状态和聚合业务指标无意暴露给互联网。
- 生产配置必须替换示例密码、示例 session secret、示例 TLS 设置，并提供可验证的配置检查清单。
- 若使用反向代理传递真实客户端 IP，必须明确只有可信代理覆盖并校验转发头时才能启用 `rate_limit.trust_forwarded: true`。
- 管理后台认证增强必须保持单用户，不引入多用户、角色或租户权限模型。
- 二次验证采用 TOTP 动态验证码，必须成为后台登录流程的一部分，不能只依赖反向代理或部署文档。
- TOTP 方案必须覆盖初始化/启用、正常登录校验、禁用/重置、恢复访问的运维路径。
- 防暴力破解必须覆盖密码错误、二次验证错误和人机验证失败等登录失败路径，并避免攻击者绕过现有 IP 锁定。
- 人机验证必须纳入公网登录防护设计，方案需明确是否依赖第三方服务、是否可离线/内网部署、失败时如何处理登录。
- 方案必须保留 NativeS3-Bridge 的核心约束：原生文件 1:1 映射、Go 标准库 HTTP、单文件二进制、现有 Vue3 管理后台基础架构。
- 规划阶段不直接开始实现；需要先形成 `design.md` 与 `implement.md`，并经用户确认后再进入执行。

## Acceptance Criteria

- [ ] PRD 明确 S3 公网直连与管理后台公网访问的安全目标、边界和验收要求。
- [ ] `design.md` 给出推荐公网架构，包括域名划分、反向代理/TLS、应用内单用户增强认证、S3 预签名流、public-read 适用边界、ops endpoints 暴露策略。
- [ ] `design.md` 明确当前应用内需要改造的点，以及仅靠部署配置即可完成的点。
- [ ] `implement.md` 给出分阶段实施清单，至少区分“文档/配置安全化”“管理后台二次验证/人机验证/防爆破”“S3 直链安全策略”“运维端点边界”。
- [ ] 方案包含生产前检查清单：TLS、强密码/hash、session secret、bootstrap 清理、监听地址、反代限流、日志脱敏、真实 IP 信任边界。
- [ ] 方案明确哪些能力不在当前 MVP 内，例如完整 IAM/RBAC、多用户管理后台或对象级授权，除非用户后续选择纳入范围。

## Out of Scope Until Chosen

- 多用户后台、OIDC、RBAC、细粒度 IAM 策略。
- 重新实现 S3 协议栈或替换底层存储模型。
- CDN 缓存策略、对象防盗链、图片处理、水印等内容分发增强。
- 跨节点高可用、多副本、分布式锁或对象版本化。

## Planning Decisions

- 人机验证推荐采用 Cloudflare Turnstile-compatible server-side verification 作为 MVP 默认方案；配置上保留 provider/verify_url，以便后续替换 hCaptcha/reCAPTCHA 或内置挑战。
- 公网管理后台推荐采用应用内防护与反向代理边界双层方案：应用内提供 TOTP、captcha、ops endpoint gate；反向代理负责 HTTPS、域名划分、IP/路径级访问控制。
- `/healthz` 可继续作为低敏 liveness；`/readyz` 与 `/metrics` 默认应从公网隐藏或受 token/反代规则保护。
