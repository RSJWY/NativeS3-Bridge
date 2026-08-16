# 节点客户端证书自动续期

> 父任务：`.trellis/tasks/08-16-node-cert-lifecycle`（承重决策 D1/D2/D3 见父任务 PRD，本任务不得擅自变更）

## Goal

节点在客户端证书临期时，经**现有 mTLS 通道**自主换取新证书并热切换，无需管理员介入、无需一次性令牌。消除「90 天后节点静默失联且永不自愈」这一 P0 故障。

## 现状（实现前必读）

| 事实 | 位置 |
|---|---|
| 客户端证书 TTL 90 天，三层兜底 | `pkg/panel/pki.go:23`、`pkg/config/panel.go:61,123`、`pkg/panel/transport.go:80-82`、`pki.go:88-90` |
| 签发函数，唯一生产调用点在注册事务内 | `pkg/panel/pki.go:84` `SignNodeCSR` ← `pkg/panel/registration.go:81` |
| panel 只有两条路由，无续期端点 | `pkg/panel/transport.go:89-90` |
| mTLS 应用层校验（本任务要复用） | `pkg/panel/transport.go:185-200` `authenticateMTLS` → `pkg/panel/pki.go:151` `IsCertValid` |
| listener TLS 配置（**本任务不得改动其语义**） | `pkg/panel/transport.go:753-764`，`ClientAuth: tls.VerifyClientCertIfGiven` |
| 节点「已注册」判定只 `os.Stat`，不解析证书 | `pkg/nodeagent/register.go:45-55` `HasCertificate()` |
| 节点跳过注册的分支 | `cmd/node/main.go:199` |
| 节点 TLS 材料加载，不看有效期 | `pkg/nodeagent/client.go:453-473` `clientTLS()` |
| 节点重连循环，60s 上限无限退避 | `pkg/nodeagent/client.go:111-130` |
| CSR 构造（本任务要复用） | `pkg/nodeagent/register.go:97-106` `buildCSR`，节点私钥 P-256（`register.go:76`） |
| 私钥 0600 / 证书 0644 落盘 | `pkg/nodeagent/register.go:88` / `register.go:229`、`persistPEM` 在 `register.go:286` |
| `NodeCert` 模型 | `pkg/panel/models.go:44-55` |
| panel 迁移注册表与校验 | `pkg/panel/migrate.go:13-24`（models）、`:27-41`（expectedTables）、`:44-57`（expectedIndexes） |

## Requirements

### R1 panel 续期端点
- R1.1 新增 `POST /renew`，挂在与 `/agent` 相同的 mTLS listener 上（`transport.go:87-92` `Handler()`）。
- R1.2 **必须复用 `authenticateMTLS`** 完成身份认定：只接受未过期、未吊销、节点 `active` 的客户端证书。请求体只含 `csr_pem`，节点身份**一律取自客户端证书**，不接受请求体传入 `node_id`（防越权换证）。
- R1.3 CSR 中的公钥必须与当前客户端证书**不同**才允许签发？→ **不作此要求**：节点可复用现有私钥，也可换新私钥，两者都允许。但 CSR 的 CN 必须与证书绑定的节点一致（复用 `nodeSubject`，不一致则 400）。
- R1.4 签发复用 `SignNodeCSR(csrPEM, nodeID, deps.ClientCTTL, now)`，插入新 `NodeCert` 行。**不在此刻吊销旧证**（D1）。
- R1.5 响应体与注册端点对齐：`{cert_pem, ca_cert_pem, not_after}`。
- R1.6 body 大小限制沿用 `registrationBodyLimit`（`transport.go:28`）；`dec.DisallowUnknownFields()` 沿用。
- R1.7 审计一条 `Action: "node_cert_renew"`，`Result` 为 `issued` / `denied`，`TargetNode` 为节点 ID，指纹字段填**新证**指纹。不得记录 CSR 或任何私钥材料。

### R2 新证激活与旧证吊销（D1）
- R2.1 `node_certs` 增列 `activated_at *time.Time`（增量加列，安全网 C 同理适用于 panel DB：只增不改不删）。
- R2.2 `authenticateMTLS` 在证书校验通过后，若该指纹 `activated_at IS NULL`，则在**一个事务内**：置本证 `activated_at = now`，并吊销该节点**其他所有** `activated_at IS NOT NULL OR activated_at IS NULL` 的未吊销证书（即除自己以外全部）。
- R2.3 R2.2 的写入失败**不得阻断连接**：记 Error 日志后按认证通过继续（连接可用性优先；下次连接会重试激活）。
- R2.4 激活/吊销要幂等：同一指纹重复接入不得重复吊销、不得把自己吊销。
- R2.5 首次注册签发的证书同样走这条激活路径（无旧证时吊销集为空，天然无副作用）。

### R3 节点侧临期自检与续期
- R3.1 `Identity` 新增方法解析本地证书并返回其 `NotAfter`（以及解析失败的错误）。**`HasCertificate()` 语义改为**：文件存在 **且** 能解析为合法证书 **且** 尚未过期。解析失败或已过期返回 false。
- R3.2 `cmd/node/main.go:199` 的分支因 R3.1 自动获得「过期即走令牌重注册」能力。若此时无令牌/无 register_url，必须打 **Error** 级日志，明确指出「本地证书已过期/损坏，需管理员签发注册令牌」（对齐父任务 AC2），然后 return（安全网 A：不影响 S3）。
- R3.3 节点在**每次成功建立控制面连接后**检查剩余有效期；剩余 < `TTL/3` 时触发一次续期。由于节点不知道 panel 配置的 TTL，用**本地证书自身的 `NotAfter - NotBefore`** 作为 TTL 基准计算阈值（D3 的比例语义在节点侧的等价实现）。
- R3.4 续期成功后：先落盘新证（沿用 `persistPEM`，0644），再主动关闭当前连接，让 `Run` 的重连循环用新证重连。**不得**在不断开的情况下继续用旧证跑（否则 R2 的激活永不触发）。
- R3.5 续期失败**不得**影响当前连接与 S3 数据面：记 Warn 并在下次连接后重试。同一连接内不得反复重试（避免打爆 panel）。
- R3.6 续期 URL：从 `AgentURL` 推导（把 scheme `wss`→`https`、path `/agent`→`/renew`），**不新增配置字段**，避免存量部署改配置。推导逻辑要有单测覆盖含端口、含子路径的情况。

### R4 注释与承诺对齐
- R4.1 `pkg/panel/pki.go:22` 的 `// Nodes renew over mTLS before expiry (see design §3.3).` 在本任务落地后成为事实；若最终实现与该描述有出入，必须改注释使其与代码一致。

## Acceptance Criteria

- [ ] AC1 `POST /renew` 用有效客户端证书 + 合法 CSR → 200，返回新证书，`node_certs` 多一行，旧证此刻**仍未被吊销**。
- [ ] AC2 用**已过期**证书调 `/renew` → 401，不签发（D2 红线）。
- [ ] AC3 用**已吊销**证书调 `/renew` → 401，不签发。
- [ ] AC4 节点 `retired`/`disabled` 时调 `/renew` → 401，不签发。
- [ ] AC5 无客户端证书调 `/renew` → 401。
- [ ] AC6 CSR 的 CN 与证书节点不匹配 → 400，不签发。
- [ ] AC7 新证首次接入 `/agent` 后：新证 `activated_at` 非空，该节点其余未吊销证书全部 `revoked=true`，新证自身**未**被吊销。
- [ ] AC8 同一新证第二次、第三次接入不产生额外吊销，`activated_at` 不被改写（幂等）。
- [ ] AC9 R2.2 事务失败被注入时，连接仍然建立成功（可用性优先），且有 Error 日志。
- [ ] AC10 `HasCertificate()` 对「文件存在但已过期」「文件存在但非合法 PEM」均返回 false；对有效证书返回 true。
- [ ] AC11 节点侧：给一张剩余期 < TTL/3 的证书，连接建立后自动触发续期、落盘新证、断开重连，最终以新证在线。
- [ ] AC12 节点侧：给一张剩余期 > TTL/3 的证书，**不触发**续期（不产生 `/renew` 请求）。
- [ ] AC13 续期端点返回 5xx / 网络失败时，节点当前连接不中断，S3 数据面正常（安全网 A）。
- [ ] AC14 `/renew` URL 推导单测：`wss://h:9443/agent` → `https://h:9443/renew`；含子路径与非标准端口的用例同样正确。
- [ ] AC15 审计表出现 `node_cert_renew` 条目，且**不含** CSR、私钥或 secret 材料。
- [ ] AC16 panel 迁移增量性：`activated_at` 以加列方式引入，`migrate.go` 的 `expectedTables`/`expectedIndexes` 校验通过；旧 panel DB 原地升级后既有数据完好。
- [ ] AC17 `transport.go` 的 `ClientAuth` 仍为 `tls.VerifyClientCertIfGiven`，未新增放宽标准校验的 `VerifyPeerCertificate`/`VerifyConnection`（D2 红线，用 grep 自证）。
- [ ] AC18 `go build ./... && go vet ./... && go test ./... && gofmt -l .` 全绿。

## 约束

- 不新增第三方依赖。
- 不改动 `pkg/controlproto` 的消息类型（续期走 HTTP `/renew`，不走 WebSocket 消息；理由见 design.md）。
- 不改动节点 DB schema（安全网 C）。
- 不新增 node.yaml 配置字段（R3.6）。
- 节点私钥永不上传；`/renew` 只接受 CSR。

## Notes

- 与 `08-16-cert-expiry-observability` 并行开发时，若本任务先落地了「解析本地证书取 NotAfter」「按比例算临期阈值」的工具函数，另一子任务应复用而非重写。
- 代码落盘由独立会话执行；本会话只负责规划与验收。
