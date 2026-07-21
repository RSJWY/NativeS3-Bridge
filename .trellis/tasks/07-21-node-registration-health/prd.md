# Node 注册重试与健康状态

## Goal

修复 Node 首次注册和运行状态可观测性，使临时网络/Panel 故障可以自动恢复，一次性令牌在响应丢失场景下可安全幂等重试，并让健康检查反映真实服务而不是静态配置可解析。

## Requirements

- Node 无证书时对可重试注册错误执行带抖动的指数退避，成功后立即进入 mTLS 客户端循环，不要求重启容器。
- HTTP 400/401/403 等永久拒绝停止当前 token 的重试并输出明确但不泄密的错误；网络、TLS 临时错误、429 和 5xx 可重试。
- Panel 注册必须以“token + node + node 公钥”为幂等边界：同一 token、同一 node 私钥的重试返回同一证书；不同公钥重放必须拒绝。
- token 消费、证书记录和可重放响应必须在同一数据库事务中提交，避免半完成状态。
- Panel 提供真实 `/healthz` 和数据库 `/readyz`，API fallback 不得伪装为健康端点。
- disabled/retired 节点不得注册或建立 mTLS 控制连接；重新启用后现有未撤销证书可恢复连接。
- Node `-check-config` 必须要求控制面 CA 路径；安装脚本继续验证 CA 文件。
- Node Docker healthcheck 必须探测实际 S3 listener；控制面离线只在 Panel 中显示 offline，不把仍可服务的 S3 数据面标记为 unhealthy。

## Acceptance Criteria

- [ ] 首次请求网络失败、Panel 5xx 后恢复时，node 在同一进程内注册成功并上线。
- [ ] Panel 成功签发但客户端未收到响应时，同 token/同私钥重试返回同一证书，数据库只有一条证书记录。
- [ ] 同 token 换私钥/CSR 重放返回 401；过期或错误 token 不进入无限重试。
- [ ] `/healthz` 返回明确健康正文；DB 不可用时 `/readyz` 返回 503；未知 `/api/*` 仍返回 JSON 404。
- [ ] disabled 节点断开后不能立即重连，active 后可使用未撤销证书重新上线。
- [ ] Node 缺少 `panel.ca_file` 配置时 `-check-config` 失败。
- [ ] Node healthcheck 对运行中的 S3 listener 成功，对未监听端口失败，同时不依赖 Panel 在线。
- [ ] 相关单元/集成测试、`go test ./...`、`go vet ./...` 和二进制构建通过。

## Out of Scope

- 本子任务不新增业务配置 CRUD。
- 不改变“Panel 离线时 Node 继续服务最后配置”的数据面可用性策略。
