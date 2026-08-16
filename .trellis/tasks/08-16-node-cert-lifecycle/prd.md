# 节点证书生命周期治理（父任务）

## 背景

`07-13-multi-node-mtls-control-plane` 交付了 panel↔node 的 mTLS 控制面，但**证书续期从未落地**。

根因已精确定位到需求链的**唯一一个断点**：

| 环节 | 位置 | 状态 |
|---|---|---|
| 需求 | `prd.md:47`「面板日常节点证书签发和续期只使用在线中间 CA」、`prd.md:53`「面板必须管理节点 mTLS 身份的注册、签发、**续期**和吊销生命周期」 | ✅ 写了 |
| 设计 | `design.md:100-103` §3.3 续期与轮换：「证书临近到期，节点经 mTLS 通道用现证书请求续期，中间 CA 签发新证书，不需要新令牌」 | ✅ 写了，且方案与本轮采用的一致 |
| 风险预案 | `design.md:216`「证书过期 \| 到期前主动续期；已过期则拒绝接入，需管理员处理」 | ✅ 写了 |
| **执行清单** | `implement.md`（155 行 / 8 个阶段）**全文无「续期」二字**。阶段 1（`:33`）只列「中间 CA 签发客户端证书;吊销集」，阶段 2（`:46`）只列一次性注册端点 | ❌ **断点在此** |
| 实现 | 无 | ❌ |
| check | 只对着 `implement.md` 走 → 全绿 | ⚠️ 假绿 |

两个加剧因素：

1. **PRD 的 10 条验收标准全是规划级**（「明确……流程」，`prd.md:79-88`，全部未勾选），而流程**确实**被写进了 design §3.3 —— 所以这些 AC 在不实现任何代码的情况下就可被满足。全程没有任何一条**实现级** AC 覆盖续期。
2. **有一个通过的测试在提供虚假安心**：`implement.md:51` 要求覆盖「过期证书被拒」，`pkg/panel/pki_test.go:122` 如实实现了它并持续通过。风险预案的「拒绝接入」这一半被实现且被测试，「主动续期」那一半不存在——测试套件反而在确认坏行为工作正常。

另：全部归档任务中，`续期`/`renew` 只出现在 `07-13` 自己的 `prd.md` 与 `design.md`，**没有任何后续任务接手**（该任务在 `task.json` 里也无 children，五个「子任务」是同一任务内的阶段）。

`pkg/panel/pki.go:22` 至今留着 `// Nodes renew over mTLS before expiry (see design §3.3).` —— 注释忠实地指向了那份设计，而实现不存在。

### 流程教训（应沉淀进 spec）

需求与设计写了、执行清单没写 → 实现不会发生 → check 对着执行清单走仍然全绿。**`implement.md` 是需求可追溯性的最后一道闸门**：PRD 的每条 Requirement 必须能在 implement.md 找到对应步骤，否则该需求会静默蒸发。本轮结束后应把这条写进 `.trellis/spec`。

### 能力现状（2026-08-16 全量核查）

| 能力 | 现状 | 证据 |
|---|---|---|
| 叶子证书签发 | 有，仅注册路径 | `pkg/panel/pki.go:84` `SignNodeCSR`，唯一生产调用点 `pkg/panel/registration.go:81` |
| 客户端证书有效期 | 90 天（可配） | `pki.go:23`、`pkg/config/panel.go:61,123`、`scripts/install-panel.sh:312` |
| 续期 / 重签 | **无** | 路由只有 `/register` `/agent`（`transport.go:89-90`）；协议无证书消息（`pkg/controlproto/envelope.go:32-47`）；admin API 只有 list/revoke（`adminapi.go:347-378`） |
| 节点过期自检 | **无** | `HasCertificate()` 只 `os.Stat`，不解析证书（`pkg/nodeagent/register.go:45-55`） |
| 到期告警/指标/UI 状态 | **无** | `dashboard.go` 无 cert 字段；`PanelNodeDetail.vue:102` 仅按 `Revoked` 渲染两态 |
| CA 自身有效期校验 | **无（fail-open）** | `LoadIntermediateCA` 只查 `IsCA`（`pki.go:53`） |
| panel 服务端证书续期 | **无** | `install-panel.sh:289` 签 825 天，脚本无子命令 |
| 吊销 | 有（DB 指纹白名单，无 CRL/OCSP） | `pki.go:151-186`、`transport.go:185-200` |

### 故障形态（P0）

90 天后：证书过期但文件仍在磁盘 → `cmd/node/main.go:199` 判定「已有证书」跳过注册 → 握手被拒（或 `IsCertValid` 在 `pki.go:162` 拒绝）→ `client.go:111-130` 以 60s 上限**无限退避重试，永不自愈**，只打一条 `slog.Warn("control-plane connection ended")`。

安全网 A 仍生效（S3 数据面照常服务本地 DB），因此故障**完全静默**：期望状态推送、凭据下发、任务全停，无人知情。

### 附带发现的运维陷阱

- `docs/multi-node-operations.md:107` 备份六件套第 3 项要求备份「离线 root CA + 加密 root 私钥」，但实现里**没有 root**（`install-panel.sh:270` 是 `req -x509 -new` 自签，且 `pathlen:0`）。照清单备份的人会以为自己备全了。
- `docs/docker-deployment.md:470` 承诺「在线 intermediate 轮换见多节点 mTLS 运维指南」——指向的文档没有这一节，断链承诺。
- `install-panel.sh` 无子命令，只有 `--force`，而 `--force` 会 `rm -rf` 整个安装目录（`install-panel.sh:254-259`）。825 天服务端证书到期时最直觉的「重跑安装脚本」会**连带删掉 panel DB 和 master.key**。

## Goal

让节点证书在到期前自动续期、到期风险可被看见、服务端证书可被非破坏性重签，并且文档不再承诺不存在的能力。彻底消除「90 天静默失联」这一类定时炸弹。

## Requirements

### R1 自动续期（子任务 1）
节点在证书临期时经现有 mTLS 通道自主换取新证书，无需管理员介入、无需一次性令牌。

### R2 到期可观测（子任务 2）
到期风险在 admin API、UI、dashboard、日志四处可见；证书类永久错误不得混在普通重连 warn 里。

### R3 服务端证书与 CA 可维护（子任务 3）
提供非破坏性的服务端证书重签路径（顺带支持多 SAN）；CA 自身临期/过期不得 fail-open。

### R4 文档纠偏（子任务 4）
删除 offline root CA 等不存在的承诺与断链引用，补齐证书过期/轮换/重签 runbook。

### R5 流程修复（子任务 5）
把「PRD 每条 Requirement 必须能在 implement.md 找到对应步骤」固化进 `.trellis/spec`，使本次的需求蒸发不再重演。此项与证书功能无关，是从根因分析析出的独立交付物。

## 承重决策（已裁决，子任务不得擅自变更）

### D1 旧证处置：新证首次接入成功后再吊销旧证
签发时旧证仍有效 → node 落盘新证 → 用新证重连 → panel 在 `authenticateMTLS` 首次见到该指纹时才吊销同节点其他证书。

**理由**：任何一步失败都能回落旧证继续运行，不会把节点锁死。代价是 `node_certs` 增一列 `activated_at`（增量加列）。

**否决**：签发同事务立即吊销旧证 —— node 落盘失败或响应丢包即锁死，只能人工令牌重注册。

### D2 已过期节点：只能令牌重注册，不做宽限期
`/renew` 复用 `authenticateMTLS`，只对**未过期、未吊销、节点 active** 的证书开放。

**红线：不得为实现续期而放宽 TLS 客户端证书校验**（不得引入放宽标准校验的 `VerifyPeerCertificate`、不得改动 `transport.go:761` 的 `ClientAuth` 语义）。附带好处：被吊销的节点无法自我续命。

### D3 client_cert_ttl 保持 90 天
续期阈值用**比例** `TTL/3`（默认 30 天）而非绝对值，使 `client_cert_ttl` 被调整时阈值自动跟随。

## 任务图

| # | 子任务 | 目录 | 交付物 | 依赖 |
|---|---|---|---|---|
| 1 | 节点客户端证书自动续期 | `08-16-cert-auto-renew` | panel `/renew` 端点 + 节点临期自检与续期 + `HasCertificate` 解析有效期 + `activated_at` 增量迁移 | 无 |
| 2 | 证书到期可观测性 | `08-16-cert-expiry-observability` | admin API 剩余天数 + UI 三态 + dashboard 聚合 + 证书类永久错误明确报错 | 无（可与 1 并行；若 1 先落地则复用其到期判定工具函数） |
| 3 | panel 服务端证书重签与 SAN 补全 | `08-16-panel-server-cert-renew` | `install-panel.sh renew-server-cert` 子命令 + 多 SAN + CA 有效期校验 | 无 |
| 4 | 证书文档与备份清单纠偏 | `08-16-cert-docs-correction` | `docs/` + `README.md` 纠偏与 runbook | **依赖 1/2/3 完成**（runbook 需描述最终行为） |
| 5 | implement.md 需求可追溯性闸门 | `08-16-requirement-traceability-gate` | `.trellis/spec/guides/` 新指南 + index 挂载 | 无（纯 spec，可随时做；其 AC6 需拿 1/2/3 的产物自证） |

顺序：`{1, 2, 3, 5}` 可并行 → `4` 收尾。父任务不承载直接实现工作，只做集成验收。

## 跨子任务验收标准

- [ ] **AC1 端到端续期闭环**：`scripts/test-panel-node-e2e.sh` 增加短 TTL 场景，实证「签发 → 临期 → 自动续期 → 用新证重连 → 旧证被吊销」全链路，且过程中控制面不中断。
- [ ] **AC2 无静默失败**：证书过期/被吊销时，节点日志出现 Error 级、含「证书」语义、指明恢复动作的条目；不再只有 `control-plane connection ended`。
- [ ] **AC3 安全网 A 未破坏**：续期失败、过期、被吊销三种情况下，node 的 S3 数据面持续正常服务本地 DB。
- [ ] **AC4 安全网 C 未破坏**：panel DB 迁移严格增量（只增列），node DB 不因本轮改动新增任何表/列。
- [ ] **AC5 D2 红线未破**：`transport.go` 的 `ClientAuth` 语义未变，未引入放宽标准校验的 `VerifyPeerCertificate`；已过期证书调 `/renew` 返回 401。
- [ ] **AC6 注释与文档零虚假承诺**：`pki.go:22` 的续期注释与实现一致；仓库中不再出现与实现不符的 offline root CA 描述；`docker-deployment.md:470` 的引用不再断链。
- [ ] **AC7 全绿**：`go build ./... && go vet ./... && go test ./... && gofmt -l .` 无输出；e2e 脚本通过。
- [ ] **AC8 敏感材料零泄露**：新增日志与审计条目不含私钥内容或 secret；审计沿用 `Auditor` 脱敏约定。
- [ ] **AC9 追溯闸门自证**：子任务 5 的闸门套在子任务 1/2/3/4 的 prd.md 上，每条 Requirement 都能在对应 implement.md 找到承载 Step，无断点（对应子任务 5 的 AC6）。

## 约束

- 单租户部署，panel 单实例（沿用既有假设）。
- 保持三条既有安全网：A 未注册/失联时 node 本地直供 S3；B node 忽略旧 config.yaml 业务字段；C 节点 DB 迁移严格增量。
- 不引入新的第三方依赖（WebSocket 仍用 `github.com/coder/websocket`，不引入 gRPC / cert-manager 类组件）。
- CA 层级重构（引入真正的离线 root、使 intermediate 可轮换）**不在本轮范围**，列为待评估遗留项——它需要全网重装或双信任锚过渡期，量级独立。

## 遗留项（本轮不做，需登记）

- L1 CA 层级名不副实：`intermediate-ca.crt` 实为 `pathlen:0` 自签根，无法在保留信任锚前提下轮换。CA 到期（3650 天）或私钥泄露只能全网重装。
- L2 无 CRL/OCSP：现为「有效指纹白名单」模型，吊销依赖 panel DB 可用性。单 panel 下可接受。
- L3 节点私钥 ECDSA P-256 与 CA RSA-3072 混用（`register.go:76` vs `install-panel.sh:268`）。Go 支持混签，非缺陷，仅备忘。
- L4 注册成功后 panel 返回的 `ca_cert_pem` 会覆盖运维手装的 `ca_file`（`register.go:232-236`）；内容正常一致，但信任锚被服务端响应决定过一次。

## Notes

- 本轮由本会话负责任务创建、规划产物与最终验收；**代码落盘在独立会话执行**。
- 端口事实核对：9443 = 节点控制面，9001 = admin UI（默认绑 127.0.0.1）。`25892` 在仓库中零引用，若部署中存在则来自仓库外配置或反向代理，不在代码控制范围内。
