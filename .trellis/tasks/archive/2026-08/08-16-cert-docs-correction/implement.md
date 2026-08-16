# 执行计划：证书文档与备份清单纠偏

> 读序：`prd.md` → `design.md` → 本文件。design 已裁决全部取舍，本文件只排顺序与验证。
> **纯文档任务**：Go 代码仅允许改注释（`pki.go`、`config/panel.go`）。任何产品逻辑改动 = 越界。

## 前置确认（开工前 2 分钟）

- [ ] P1 确认 HEAD 含三个前置子任务：`git log --oneline -4` 应见 `d260e6b` / `e3671b0` / `5894180` / `346943c`。
- [ ] P2 确认 design §0 的行号复核结论仍成立（若期间有人动过文档，行号需重新定位；以内容锚点为准，不迷信行号）。

---

## 阶段 A：代码注释纠偏（最小、最独立，先做完好收工）

- [ ] A1 `pkg/panel/pki.go:32-34` —— 改写 `CA` 结构体注释。删除 offline root 承诺，改为实况：部署 CA、`pathlen:0` 自签根、三重身份（客户端签发者/服务端签发者/唯一信任锚）、丢失即全网重装、指向 `docs/multi-node-operations.md` §10.6。
- [ ] A2 `pkg/panel/pki.go:25` —— `(see design §3.3)` 是悬空引用（指向任务目录，读代码的人跳不过去）。改为指向 `docs/multi-node-operations.md` §10.2。
- [ ] A3 `pkg/config/panel.go:55-56` —— `PKIConfig` 注释同 A1 措辞纠偏。**两处措辞保持一致**，便于日后 grep。
- [ ] A4 **不改任何标识符**：`LoadIntermediateCA`、`CA`、`PKIConfig.IntermediateCertFile`、yaml 键 `intermediate_cert_file` 全部保持原名（改名是破坏性变更，design §2.3）。
- [ ] A5 验证：`go build ./... && go vet ./... && gofmt -l .` —— 只改注释，必须全绿；若有 error 说明注释块误伤语法。

**阶段 A 门禁**：`grep -rn 'offline root\|离线 root' pkg/` 结果为空（或仅剩明确标注为已知限制的表述）。

---

## 阶段 B：runbook 主体（`docs/multi-node-operations.md` §10，英文）

新增 §10 置于 §9 之后。六小节顺序照 design §1.3，不重排。

- [ ] B1 §10.1 **What certificates exist** —— 三类证书对照表。列：名称 / 文件路径 / TTL / 签发者 / 到期后果 / 是否自动续期。
  - 部署 CA `data/pki/intermediate-ca.{crt,key}` / 3650d / 自签 / 全网重装 / 否
  - Panel 服务端证书 `data/pki/panel-server.{crt,key}` / 825d / 部署 CA / 节点连不上，重签即可 / 否
  - 节点客户端证书 容器内 `/data/pki/node.{crt,key}`（宿主 `<node_install_dir>/data/pki/`，实证 `install-node.sh:331-332`）/ 90d / 部署 CA / 需令牌重注册 / **是**
  - 必须写明：名为 `intermediate` 实为自签根，历史遗留，见 §10.6。
- [ ] B2 §10.2 **Automatic client-cert renewal**（R2.1）—— 阈值 TTL/3（90d 证书 = 剩 30d 触发）、走 `POST /renew` over mTLS、D1 语义（新证首次接入成功后才吊销旧证）、失败时行为（继续退避重连，日志 Error 级）。事实核对：`pkg/panel/pki.go` `DefaultClientCertTTL`、`transport.go:91` 路由、`nodeagent/register.go` `RenewalThreshold`。
- [ ] B3 §10.3 **Checking expiry (routine)**（R2.5）—— 两种方式：管理面节点详情页证书表格（四态 + 剩余天数）、`GET /api/admin/nodes/{id}/certs`（`status` / `days_until_expiry` 字段）。dashboard 「需要处理」含证书临期/已过期计数。**不提 Prometheus**（本轮明确未做）。
- [ ] B4 §10.4 **Recovering an expired node cert**（R2.2 / F5）—— design §2.5 的七步序列，逐步可执行。附「为何无宽限期」（D2：不为续期放宽 TLS 客户端证书校验）。
- [ ] B5 §10.5 **Re-signing the panel server cert**（R2.3 / R2.6 / F6 / F7）—— `renew-server-cert` 完整命令、多 SAN 语义（连接用的名字必须在 SAN 内）、不影响已注册节点、必须重启 panel（或 `--restart`）、备份与回滚。**醒目警告：不要用 `--force`**（它 `rm -rf` 安装目录，连带删 panel DB 与 master.key）。
- [ ] B6 §10.6 **CA expiry — known limitation L1**（R2.4 / R1.4）—— L1 的**唯一登记点**。如实写：`intermediate-ca.crt` 实为 `pathlen:0` 自签根，无法在保留信任锚前提下轮换；3650 天到期或私钥泄露只能全网重装；给出提前规划建议（到期前 6–12 个月排期）。措辞照父任务 PRD:127 的 L1 定义，不弱化。

**阶段 B 门禁**：§10 内所有命令、路径、TTL、字段名与代码一致（AC7）。逐条 grep 自证，不靠记忆。

---

## 阶段 C：备份清单纠偏（`docs/multi-node-operations.md` §6、§8）

- [ ] C1 §6.1 六件套 → **五件套**（design §1.4）。删除原第 3 项（offline root），原第 4 项改写为准确的部署 CA 表述。
- [ ] C2 §6.1 补一句：早期清单含离线 root，实现中从未存在，见 §10.6 —— 避免照旧清单备份过的人去找不存在的文件。
- [ ] C3 §6.1 补说明：服务端证书与 `panel-ca.crt` **不在备份清单内**，因为可由 CA 重签（§10.5）。不写会让运维疑惑漏项。
- [ ] C4 §6.1 第 5 项「audit data」标注清楚：它在 `panel.db` 内（`audit` 表），不是独立文件。
- [ ] C5 §6.2 第二条红线末尾、§121 那句 `Re-registration is required only for nodes whose certs were revoked or expired` —— 加指向 §10.4 的链接。
- [ ] C6 §8 恢复演练第 3 步复核（F9）—— 表述仍成立（e2e `test-panel-node-e2e.sh:1267` 已证），补一句：证书**已过期**的节点不在此列，走 §10.4。

**阶段 C 门禁**：AC4 照单核对 —— 在真实 install-dir 下逐项 `ls`，五项全部命中。

---

## 阶段 D：入口与断链修复

- [ ] D1 `docs/docker-deployment.md:470` 断链改写（design §2.2 的四项逐一处理）：删除「离线 root CA」「intermediate 轮换」两项不存在的承诺 → 改为指向 §10.6 已知限制；保留「节点证书撤销」「恢复演练和事故处理」并指向 §5 / §8。
- [ ] D2 `docs/docker-deployment.md:487` 的 `--force` 警告处补一句：服务端证书到期的正确做法见 §10.5（F7 正向指引）。
- [ ] D3 `docs/docker-deployment.md:271-311` 手工 openssl 示例 —— 补多 SAN 写法说明（`subjectAltName=DNS:panel.example.com,IP:10.0.0.5`），指向 §10.5。**不改既有单值示例本身**（它仍正确）。
- [ ] D4 `README.md` 新增证书生命周期小节（中文，位置在 `:216` `client_cert_ttl` 附近或 `:484` 证书 API 表格之后）：90 天自动续期/阈值 30 天、服务端 825 天用 `renew-server-cert` 且不要用 `--force`、CA 3650 天到期需全网重装、链接到 §10。
- [ ] D5 `README.md:484` `GET /certs` 描述补「含剩余天数与状态」。
- [ ] D6 `README.md:715` 已有指向 multi-node 文档的链接，核对措辞是否需随 §10 新增而微调。

**阶段 D 门禁**：AC3 + AC8 —— 所有新增/修改的文档内链接可跳转（锚点真实存在），逐个点击或 grep 标题验证。

---

## 阶段 E：实测验证（本任务最重的一环，不可跳过）

- [ ] E1 **AC6 服务端重签**：跑 `bash scripts/test-panel-node-e2e.sh`，通过后逐条核对 §10.5 写的命令/顺序/预期与 e2e 实际一致。
- [ ] E2 **AC6 补充**：e2e 因目录布局差异是直接调 openssl 而非 `renew-server-cert` 子命令。**需在一个真实 install-dir 上手工跑一次该子命令**，确认可用、输出与 §10.5 描述相符、节点无需重注册。
- [ ] E3 **AC5 过期节点恢复 —— 自动化实测**（规划期已验证可行，不需改系统时间）。

  造过期证书的方法（openssl 3.0.13 实测通过）：`-not_before/-not_after` 是 openssl 3.4+ 的 flag，本机没有；改用 `openssl ca` 的 `-startdate/-enddate` 回拨签发，签出的证书链路合法但已过 `NotAfter`：

  ```bash
  # 需要 demoCA 骨架：mkdir -p demoCA/newcerts && touch demoCA/index.txt && echo 1000 > demoCA/serial
  openssl ca -config ca.cnf -cert "$CA_CRT" -keyfile "$CA_KEY" \
    -in node.csr -out node.crt \
    -startdate 20260501000000Z -enddate 20260815000000Z \
    -extensions client_ext -batch -notext
  ```
  `client_ext` 必须与 panel 签发的客户端证书一致：`basicConstraints=critical,CA:FALSE` / `keyUsage=critical,digitalSignature` / `extendedKeyUsage=clientAuth`（对齐 `pkg/panel/pki.go` 的 `SignNodeCSR` template）。
  自证已过期：`openssl x509 -checkend 0 -noout -in node.crt` 退出码非 0。

  在 `scripts/test-panel-node-e2e.sh` 末尾（§10.5 重签场景之后）追加场景，序列：
  1. 停 node，用上述方法用**真实 e2e CA** 重签一张已过期的 `node.crt`（复用节点现有 `node.key`，保证公钥匹配）。
  2. 起 node，断言：控制面连不上；node 日志出现路径 A 的 Error（`certError{kind:"local"}`，含证书语义与恢复动作）。
  3. **断言安全网 A**：此时 S3 数据面仍可服务本地 DB（`s3_expect 200 GET` 既有对象）—— 这同时是父任务 AC3 的回归。
  4. 严格照 §10.4 七步恢复：签发令牌 → 停 node → 删过期 `node.crt`/`node.key` → 写入 `registration_token` → 起 node → 确认注册成功 → 清空令牌。
  5. 断言：node 重新上线、`poll_node_synced` 通过、S3 数据面正常、新证书 `checkend` 通过。
  6. 断言 D1：重注册后旧证书在 panel 侧为 revoked，新证书 activated。

  任何一步与 §10.4 文档不符 → **改文档**（不改流程，D2 已裁定无宽限期）。

  注：本步骤会改 `scripts/test-panel-node-e2e.sh`。这是**测试脚本**不是产品代码，不违反「纯文档任务」约束 —— 但 V4 的改动面核对要相应放行这一个文件。
- [ ] E4 **AC2 全仓自证**：`grep -rni 'offline root\|离线 root\|root private key\|root CA' docs/ README.md pkg/ --include='*.md' --include='*.go'` —— 结果要么为空，要么全部落在 §10.6 的已知限制登记里。
- [ ] E5 **AC10** 文档中无真实密钥/令牌/secret 示例值，沿用既有脱敏约定（占位符形如 `panel.example.com`、`<token>`）。

---

## 收尾验证（全部必跑）

- [ ] V1 `go build ./... && go vet ./... && go test -count=1 ./... && gofmt -l .` 全绿（只改注释，必须绿）。
- [ ] V2 `bash -n scripts/install-panel.sh`（本任务不改脚本，但文档引用了它的命令，确认脚本无恙）。
- [ ] V3 D2 红线自证：`grep -n 'ClientAuth' pkg/panel/transport.go` 仍为 `VerifyClientCertIfGiven`；`grep -rn 'VerifyPeerCertificate\|VerifyConnection' pkg/ cmd/ --include='*.go' | grep -v _test.go` 为空。
- [ ] V4 `git diff --stat` 核对改动面：应只含 `docs/multi-node-operations.md`、`docs/docker-deployment.md`、`README.md`、`pkg/panel/pki.go`、`pkg/config/panel.go`、`scripts/test-panel-node-e2e.sh`（仅 E3 新增场景）。**出现任何其他产品代码文件 = 越界，需回退**。
- [ ] V5 AC1 可追溯性：F1–F10 十项逐条在提交说明或 task notes 里记录处理方式（F3 记「复核无需改动」，F10 记「PRD 清单外新发现」）。

---

## 回滚点

| 阶段 | 回滚方式 |
|---|---|
| A（注释） | 独立可回滚，不影响 B–D |
| B（§10 主体） | 整节删除即可；但 C/D 的链接会断，需一并回退 |
| C（备份清单） | 独立可回滚 |
| D（入口） | 独立可回滚 |

阶段 B 是 C5/C6/D1/D2/D3/D4 的链接目标，**若 B 需回滚，C/D 中指向 §10 的链接必须同步回退**，否则制造新断链。

---

## 已知易错点（前车之鉴）

1. **F10 容易漏**：`config/panel.go:55` 的 offline root 不在 PRD 原清单里，是规划期新发现的。漏掉它 AC2 直接失败。
2. **备份清单是「删一项」不是「改一项」**：PRD R1.1 的字面表述会误导成改第 3 项，但那样会与第 4 项重复指向同一对文件。见 design §1.4。
3. **行号已漂移**：PRD 的 F4（`pki.go:26-27`）实际在 `:32-34`，F7（`:480`）实际在 `:487`。以内容锚点定位，不信行号。
4. **AC5 没有现成 e2e**：e2e 有的是「短 TTL 触发续期」，不是「已过期后重注册」。必须手工实测，不能拿 e2e 通过来充当 AC5 证据。
5. **不要顺手改标识符**：`intermediate-ca.crt` 文件名、`IntermediateCertFile` 字段名名不副实但**不能改**（破坏性）。只在文档里解释成因。
