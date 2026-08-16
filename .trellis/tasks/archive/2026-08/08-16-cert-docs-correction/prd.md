# 证书文档与备份清单纠偏

> 父任务：`.trellis/tasks/08-16-node-cert-lifecycle`
> **依赖**：`08-16-cert-auto-renew`、`08-16-cert-expiry-observability`、`08-16-panel-server-cert-renew` 三者全部完成后才启动——runbook 必须描述最终行为，不能描述计划中的行为。

## Goal

让仓库文档停止承诺不存在的能力，并补齐证书全生命周期的运维 runbook。运维照文档操作不应踩到「以为备份全了其实没有」「重跑安装脚本把 DB 删了」这类坑。

## 现状：已核实的文档缺陷清单

每一条都必须被处理，处理方式（改写 / 删除 / 补章节）在 design 阶段确定。

| # | 位置 | 缺陷 | 性质 |
|---|---|---|---|
| F1 | `docs/multi-node-operations.md:107` | 备份六件套第 3 项要求备份「Root CA certificate and **encrypted** root private key (offline)」，**实现里没有 root CA** —— `scripts/install-panel.sh:270` 是 `req -x509 -new` 自签，且 `pathlen:0`。照此清单备份的人会以为自己备全了。 | 虚假承诺，**运维风险最高** |
| F2 | `docs/docker-deployment.md:470` | 「生产级离线 root CA、在线 intermediate **轮换**、节点证书撤销、恢复演练和事故处理见 [多节点 mTLS 运维指南]」—— 指向的 `multi-node-operations.md` 里**没有** intermediate 轮换章节，也没有离线 root。 | 断链承诺 |
| F3 | `pkg/panel/pki.go:22` | `// Nodes renew over mTLS before expiry (see design §3.3).` 描述了一个（本轮之前）不存在的功能。 | 已由 `08-16-cert-auto-renew` R4.1 处理，此处**仅需复核** |
| F4 | `pkg/panel/pki.go:26-27` | `// The offline root CA is not loaded here: it only signs/rotates the intermediate and is kept off the panel's daily path (design §3.1).` —— 同 F1，离线 root 不存在。 | 虚假承诺 |
| F5 | `docs/multi-node-operations.md:121` | 「Re-registration is required only for nodes whose certs were revoked or expired」是全仓唯一正面承认「证书过期需重新注册」的文字，但**没有任何操作步骤**。 | 缺 runbook |
| F6 | 全仓 | 无「证书快到期怎么办」「如何续期」「CA 到期怎么换」「服务端证书 825 天到期怎么重签」任何章节。 | 缺 runbook |
| F7 | `docs/docker-deployment.md:480` | `--force` 会删除整个安装目录的警告存在，但**没有说明「服务端证书到期时正确的做法是什么」**，运维在到期时缺少正向指引，仍可能误用 `--force`。 | 缺正向指引 |
| F8 | `README.md:216`、`README.md:484-485`、`README.md:541` | 提到 `client_cert_ttl: 2160h` 与两个证书 API，但无续期/过期章节。本轮新增能力（`/renew`、到期展示、重签子命令）需在 README 有入口。 | 缺失同步 |
| F9 | `docs/multi-node-operations.md:145-155` | §8 恢复演练清单第 3 步「confirm existing nodes with valid certs reconnect without re-registration」—— 本轮改动后需复核该步骤仍然成立且表述准确。 | 需复核 |

## Requirements

### R1 删除/改写虚假承诺
- R1.1 F1：备份六件套第 3 项改写为与实现一致的表述（部署 CA 证书 + 私钥），并明确它同时是客户端证书签发者、服务端证书签发者与双向信任锚 —— 丢失即需全网重装。
- R1.2 F4：`pki.go:26-27` 注释改写为与实现一致（不存在离线 root）。
- R1.3 F2：`docker-deployment.md:470` 的引用改为指向真实存在的章节；被删除的承诺（离线 root、intermediate 轮换）不得再出现，或明确标注为「未实现的已知限制」。
- R1.4 遗留项 L1（CA 层级名不副实、不可轮换）必须在文档中作为**已知限制**如实登记，而非当作已实现能力。

### R2 补齐 runbook
- R2.1 客户端证书自动续期机制说明：触发阈值（TTL/3）、旧证何时被吊销、失败时的行为与影响面。
- R2.2 客户端证书已过期的恢复步骤（F5）：完整的令牌重注册操作序列，含为何不做宽限期（父任务 D2）。
- R2.3 panel 服务端证书重签步骤（F6/F7）：正确命令、是否影响已注册节点、是否需重启 panel、**明确警告不要用 `--force`**。
- R2.4 CA 到期（3650 天）的处置说明：如实写明当前只能全网重装，并给出提前规划建议。
- R2.5 到期巡检指引：如何用管理面/API 查看证书剩余天数（对接 `08-16-cert-expiry-observability` 的最终形状）。
- R2.6 多 SAN 与「连接名必须匹配证书 SAN」的说明（对接 `08-16-panel-server-cert-renew`），收口既有的内网 IP 访问坑。

### R3 同步入口
- R3.1 README 增加证书生命周期的简短小节与到 `docs/multi-node-operations.md` 对应章节的链接。
- R3.2 F9 复核 §8 恢复演练清单，必要时修正表述。

### R4 一致性
- R4.1 `docs/docker-deployment.md` 的手工 openssl 命令与 `scripts/install-panel.sh` 最终实现保持一致（由 `08-16-panel-server-cert-renew` 移交具体差异点）。
- R4.2 文档中所有 file:line 之外的事实性描述（端口、TTL、字段名）与最终代码一致。注意：9443 = 节点控制面，9001 = admin UI；`25892` 不是本仓库端口。

## Acceptance Criteria

- [ ] AC1 F1–F9 九项逐条处理完毕，每项在提交说明或任务 notes 中可追溯到具体处理方式。
- [ ] AC2 全仓搜索 offline root / 离线 root 相关表述，结果要么不存在，要么明确标注为未实现的已知限制（父任务 AC6）。
- [ ] AC3 `docs/docker-deployment.md:470` 指向的章节真实存在（人工点击/搜索验证，不再断链）。
- [ ] AC4 备份六件套清单中的每一项在真实安装目录下都能找到对应文件（照单核对一次，无「找不到的项」）。
- [ ] AC5 客户端证书过期恢复步骤可被**照着执行成功**：在测试环境实际制造一次过期节点，按 runbook 恢复上线。
- [ ] AC6 服务端证书重签步骤可被照着执行成功，且执行后已注册节点无需重新注册即可连上。
- [ ] AC7 runbook 中不出现与最终实现不符的命令、字段名、端口、阈值。
- [ ] AC8 README 有证书生命周期入口且链接有效。
- [ ] AC9 `pki.go` 的证书相关注释与实现一致（F3/F4 复核）。
- [ ] AC10 文档中不含任何真实密钥、令牌、secret 示例值（沿用既有脱敏约定）。

## 约束

- **纯文档与注释任务**：不得改动任何产品逻辑。允许改动的代码范围仅限注释（`pki.go` 的 F3/F4）。
- 不得为了让文档好看而弱化已知限制——L1（CA 不可轮换）必须如实登记。
- 沿用既有文档语言约定：`docs/multi-node-operations.md` 现为英文，`docs/docker-deployment.md` 与 `README.md` 现为中文，**各自保持原语言**，不做语言迁移。
- 保持既有文档结构与编号，避免大规模重排导致外部引用失效。

## Notes

- 启动前先跑一遍父任务 PRD 的「跨子任务验收标准」，确认前三个子任务的最终行为已定型。
- design.md 与 implement.md 在本任务启动时（前三子任务完成后）再写，届时才能确定 runbook 的准确内容。当前先固定缺陷清单与验收标准，避免这些已核实的发现在等待期间丢失。
- 代码落盘由独立会话执行；本会话负责规划与验收。
