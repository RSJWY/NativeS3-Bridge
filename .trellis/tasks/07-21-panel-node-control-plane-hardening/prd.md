# 修复 Panel Node 完整控制面链路

## Goal

把当前“组件分别可运行”提升为可验证的完整控制面：Panel 能可靠接入和观察 Node，Panel 对业务配置形成可操作的唯一权威，发布门真实覆盖 Panel→Node→S3 数据面，而不是仅检查构建、配置或首页 HTTP 200。

## Confirmed Facts

- 当前 Panel UI 404 已修复并有浏览器网络断言，但尚未覆盖真实 node 注册、mTLS、期望状态和 S3 CRUD。
- node 首次注册失败后 goroutine 直接退出，现有注释所称“稍后重试”并不存在。
- 一次性注册令牌在证书签发/持久化前就被消费，响应丢失会导致 node 无证书且令牌不可重用。
- Panel 缺少 bucket/webhook/rate-limit CRUD，以及 credential 更新/停用/删除能力，但 desired state 会全量删除 Panel 未声明的 node 本地配置。
- Panel `/healthz` 仍回退到 SPA；Panel/Node Compose healthcheck 仅做静态 `-check-config`。
- `scripts/test-release-integrity.sh` 不启动 node，不验证注册、同步或 S3 CRUD。

## Task Map

1. `07-21-node-registration-health`：可靠注册、幂等重试、真实健康/就绪信号。
2. `07-21-panel-authoritative-config-crud`：补齐 Panel 权威配置 API/UI 和安全全量下发。
3. `07-21-panel-node-e2e-release-gate`：真实 TLS Panel/Node/S3 生命周期发布门。

## Requirements

- 子任务按上述顺序实施；最终发布门依赖前两个子任务完成。
- 不破坏 node 在 Panel 暂时不可用时继续使用最后成功配置提供 S3 的安全网。
- 不把敏感 token、secret、私钥写入日志或持久测试输出。
- 支持 SQLite、MySQL、Postgres 的 GORM 增量迁移语义。

## Acceptance Criteria

- [ ] 首次注册的临时故障无需重启 node 即可恢复；响应丢失重试不会烧毁令牌或签发不受控的重复身份。
- [ ] Panel/Node 的健康检查验证实际运行服务，并能区分数据面存活与控制面离线。
- [ ] Panel 可完整管理 credentials、buckets/ACL、webhooks 和匿名限流，并安全发布全量快照。
- [ ] 真实 Panel 与 Node 完成注册、mTLS 在线、配置下发、S3 bucket/object CRUD、重启重连和 Panel 暂停恢复。
- [ ] 发布 CI 执行该端到端门，且失败时输出脱敏、可定位的证据。

## Out of Scope

- 多管理员/RBAC/OIDC。
- 跨地域高可用 Panel 集群。
- 大规模性能与压力测试。

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
