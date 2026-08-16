# 设计：证书文档与备份清单纠偏

> 父任务：`.trellis/tasks/08-16-node-cert-lifecycle`
> 前三子任务已全部落地（346943c / 5894180 / e3671b0），本设计描述的是**最终行为**，不是计划中的行为。

## 0. 规划期核实结论（PRD 缺陷清单的修正）

PRD 的缺陷清单写于三个子任务落地之前。本次逐条复核，结论如下：

| # | PRD 原述 | 复核结论 |
|---|---|---|
| F1 | `multi-node-operations.md:107` 备份六件套第 3 项承诺 offline root | ✅ 属实，行号未变 |
| F2 | `docker-deployment.md:470` 断链承诺 | ✅ 属实，行号未变 |
| F3 | `pki.go:22` 注释描述不存在的续期 | ⚠️ **已失效**：`08-16-cert-auto-renew` 已重写该注释为 `Nodes renew via POST /renew over HTTPS mTLS before expiry`，现与实现一致。**本任务仅复核，无需改动** |
| F4 | `pki.go:26-27` offline root 注释 | ✅ 属实，行号漂移至 **`pki.go:32-34`** |
| F5 | `multi-node-operations.md:121` 承认过期需重注册但无步骤 | ✅ 属实，行号未变 |
| F6 | 全仓无证书运维章节 | ✅ 属实 |
| F7 | `docker-deployment.md:480` 有 `--force` 警告但无正向指引 | ✅ 属实，警告在 **`:487`**（PRD 的 480 已漂移） |
| F8 | README 缺证书生命周期入口 | ✅ 属实 |
| F9 | `multi-node-operations.md:145-155` §8 恢复演练需复核 | ✅ 需复核，见 §3.4 |

**新增 F10（PRD 清单外，本次核实发现）**：`pkg/config/panel.go:55-56` 有第三处 offline root 承诺 —— `// PKIConfig locates the online intermediate CA. The offline root CA is not referenced here: it only signs/rotates the intermediate out of band.` 与 F4 同源同性质。AC2 要求「全仓搜索 offline root」，若漏掉此处则 AC2 无法通过。**纳入本任务范围**。

全仓 offline root 承诺共 **4 处**：`multi-node-operations.md:107`、`docker-deployment.md:470`、`pki.go:33`、`config/panel.go:55`。

---

## 1. 边界与取舍

### 1.1 offline root 表述 —— 选**「改写为实况 + 集中登记 L1」**，不选「删除」

三处代码注释 + 一处文档清单都在承诺一个不存在的离线 root。

**备选 A：全部删除相关句子**
- 优点：最快，不留痕。
- **否决理由**：删掉等于抹掉「为什么这个 CA 不能轮换」这一关键信息。运维读到 `intermediate-ca.crt` 这个文件名，天然会以为上面还有个 root、以为可以轮换。沉默比错误陈述更危险——PRD 约束明写「不得为了让文档好看而弱化已知限制」。

**备选 B（采用）：改写为实况，并在文档中集中登记 L1**
- 代码注释改为陈述实况：这是 `pathlen:0` 的自签部署 CA，既是签发者也是唯一信任锚。
- 文档侧新增一处**唯一的** L1 已知限制登记点（位置见 §2.1），其余各处指向它，不重复展开。
- 保留 `intermediate-ca.crt` 这个**文件名不改**（改名是破坏性变更，会让既有安装的 `panel.yaml` 路径失效，超出纯文档任务范围）。但在 L1 登记点里明确说明「文件名叫 intermediate 是历史遗留，它实为自签根」。

### 1.2 runbook 落点 —— 选**「集中在 `multi-node-operations.md` 新增 §10」**

PRD 约束：三个文件各自保持原语言（`multi-node-operations.md` 英文，`docker-deployment.md` 与 `README.md` 中文），不做语言迁移。

**备选 A：按主题拆到各文件**（客户端证书续期→README、服务端重签→docker-deployment、CA 到期→multi-node）
- **否决理由**：证书生命周期是一件事的六个面（R2.1–R2.6）。拆开会导致运维在故障时要跨三个文件、两种语言拼出完整流程。更糟的是 R2.2（过期恢复）和 R2.3（服务端重签）都要引用同一套「不要用 --force」的警告，拆开必然重复且漂移。

**备选 B（采用）：全部集中在 `multi-node-operations.md` 新增 §10 "Certificate lifecycle operations"（英文）**，其余文件只放**链接入口**：
- `README.md` §增加中文小节，3–5 行概述 + 链接到 §10。
- `docker-deployment.md:470` 的断链改为指向 §10 的具体锚点。
- 好处：runbook 单一权威来源；跨引用只有链接没有内容副本，不会漂移。
- 代价：中文使用者要跳到英文文档。**接受** —— `multi-node-operations.md` 本来就是运维指南且已是英文，证书运维属于它的天然领地；PRD 约束也禁止语言迁移。

### 1.3 §10 的内部结构

按「什么时候会遇到」而非「按组件」组织，因为运维是带着症状来查文档的：

```
## 10. Certificate lifecycle operations
### 10.1 What certificates exist          ← 三类证书一张表，先建立心智模型
### 10.2 Automatic client-cert renewal    ← R2.1，常态，无需人工
### 10.3 Checking expiry (routine)        ← R2.5，巡检
### 10.4 Recovering an expired node cert  ← R2.2，故障
### 10.5 Re-signing the panel server cert ← R2.3 + R2.6，到期/换 SAN
### 10.6 CA expiry — known limitation L1  ← R2.4 + R1.4，L1 唯一登记点
```

10.1 先行的理由：F1 的根本病因是运维分不清「root CA / intermediate CA / 服务端证书 / 客户端证书」哪个真实存在。一张对照表能一次性消解后面五节的歧义。

### 1.4 备份六件套 —— 改为**五件套**，不是改写第 3 项

PRD R1.1 说「第 3 项改写为部署 CA 证书 + 私钥」。但第 4 项已经是 `Online intermediate CA certificate and private key` —— 若把第 3 项也改成部署 CA，就会出现两项指向同一对文件。

**裁定：删除第 3 项，第 4 项改写为准确表述，六件套变五件套。** 并在紧邻处加一句说明「早期版本的清单含离线 root，实现中从未存在，见 §10.6」——避免照旧清单做过备份的人以为自己漏了东西而去找一个不存在的文件。

AC4（照单核对每项都能找到对应文件）据此变为核对**五项**。真实安装目录布局（`install-panel.sh` 实证）：

| 清单项 | 实际路径 |
|---|---|
| 1. Panel database | `<install_dir>/data/panel.db` |
| 2. Master key | `<install_dir>/data/secrets/master.key` |
| 3. Deployment CA cert + key | `<install_dir>/data/pki/intermediate-ca.{crt,key}` |
| 4. Panel configuration | `<install_dir>/panel.yaml` |
| 5. Audit data | 在 `panel.db` 内（`AuditLog` 模型对应的表，见 `pkg/panel/migrate.go` 的 `migrationModels`）—— 需说明它不是独立文件 |

注：服务端证书 `panel-server.{crt,key}` 与 `panel-ca.crt` **不进备份清单**，因为它们可由 CA 重新签出（`renew-server-cert`）。这一点要在清单里写明，否则运维会疑惑为何漏了。

---

## 2. 各缺陷的处理方式

### 2.1 F1 —— `multi-node-operations.md:101-110`

删除第 3 项，第 4 项改写。新表述要点：
- 名为 intermediate、实为自签根（`pathlen:0`）。
- 三重身份：客户端证书签发者 + 服务端证书签发者 + 双向信任锚。
- 丢失后果：全网重装，无恢复路径。
- 指向 §10.6。

### 2.2 F2 —— `docker-deployment.md:470`

原文承诺四件事，逐一核对后改写：

| 原承诺 | 是否真实存在 | 处理 |
|---|---|---|
| 生产级离线 root CA | ❌ 不存在 | 删除，改为指向 §10.6 的已知限制 |
| 在线 intermediate 轮换 | ❌ 不存在（L1） | 同上 |
| 节点证书撤销 | ✅ 存在（`/certs/revoke`） | 保留，指向 §5 |
| 恢复演练和事故处理 | ✅ 存在（§8、§5） | 保留 |

### 2.3 F4 / F10 —— 代码注释

`pki.go:32-34` 与 `config/panel.go:55-56` 改写为实况陈述。两处措辞保持一致，都点明「部署 CA，非真正的 intermediate；无离线 root；见 L1」。

**注意**：`pki.go` 的 `CA` 结构体与 `LoadIntermediateCA` 函数名、`config` 的 `PKIConfig.IntermediateCertFile` 字段名**均不改**（改名超出纯文档任务范围，且 yaml 字段改名是破坏性变更）。只改注释。

### 2.4 F3 / F9 —— 复核项

- F3：`pki.go:22-25` 现为 `Nodes renew via POST /renew over HTTPS mTLS before expiry (see design §3.3)`。核实 `/renew` 路由真实存在（`transport.go:91`）、走 mTLS、TTL 90 天（`DefaultClientCertTTL`）。**结论：与实现一致，不改**。但 `(see design §3.3)` 指向的是任务目录下的 design.md，对读代码的人是悬空引用 —— 建议改为指向 `docs/multi-node-operations.md §10.2`。
- F9：§8 恢复演练第 3 步 `confirm existing nodes with valid certs reconnect without re-registration` —— 经 `08-16-panel-server-cert-renew` 的 e2e 实证（`test-panel-node-e2e.sh:1267`）该表述**仍然成立**。但需补一句：证书**已过期**的节点不在此列，走 §10.4。

### 2.5 F5 —— 过期恢复步骤（§10.4）

`multi-node-operations.md:121` 那句「Re-registration is required only for nodes whose certs were revoked or expired」保留，末尾加指向 §10.4 的链接。

§10.4 的步骤序列（基于 D2：无宽限期，已过期只能令牌重注册）：
1. 在管理面确认节点证书状态为 `expired`（§10.3 的查看方式）。
2. 管理面为该节点签发一次性注册令牌（`POST /api/admin/nodes/{id}/tokens`，201 + 一次性明文 token；已 retired 的节点返回 409）。
3. 停止 node 容器。
4. 删除 node 侧过期证书（`<node_data>/pki/node.crt`、`node.key`）。
5. 在 `node.yaml` 填入 `registration_token`。
6. 启动 node，确认注册成功。
7. 清空 `node.yaml` 中的令牌。

要写明**为何不做宽限期**（D2）：放宽 TLS 客户端证书校验来接纳过期证书，等于给控制面开一个「过期证书也能连」的口子，这是认证体系的根本性削弱。宁可要一次人工重注册。

### 2.6 F6 / F7 —— 服务端重签（§10.5）

正确命令（实证自 `install-panel.sh:23`）：
```
sudo ./install-panel.sh renew-server-cert \
  --install-dir /opt/natives3-panel \
  --panel-host panel.example.com,10.0.0.5
```

要点：
- **不影响已注册节点**（CA 与客户端证书未变，e2e 已证）。
- **必须重启 panel** 才生效（进程启动时一次性加载）；`--restart` 可自动重启。
- 备份文件与回滚方法（脚本输出里有，文档需呼应）。
- **醒目警告：不要用 `--force` 重跑安装脚本** —— 它 `rm -rf` 整个安装目录，连带删除 panel DB 与 master.key。这是 F7 要求的正向指引，必须与 §10.5 的正确做法并置，而不是只在 §9 安全边界里孤立地警告一句。
- R2.6 多 SAN：说明 node 连接时使用的主机名/IP 必须在服务端证书 SAN 内，否则 TLS 校验失败；这正是 `--panel-host` 支持逗号分隔多值的原因。

`docker-deployment.md:487` 的 `--force` 警告处补一句指向 §10.5。

### 2.7 F8 —— README 入口

在 README 证书相关内容附近（`:216` 的 `client_cert_ttl` 或 `:484-485` 的证书 API 表格之后）加中文小节，含：
- 客户端证书 90 天自动续期，阈值 TTL/3（30 天），无需人工。
- 服务端证书 825 天，到期用 `renew-server-cert`，**不要用 `--force`**。
- CA 3650 天，到期需全网重装（已知限制）。
- 链接到 §10。

`README.md:484` 的 `GET /certs` 描述可补「含剩余天数与状态」（对接 `08-16-cert-expiry-observability`）。

### 2.8 R4.1 —— 手工 openssl 与脚本一致性

`docker-deployment.md:271-311` 的手工命令与 `install-panel.sh` 最终实现比对：

| 项 | 文档 | 脚本 | 一致 |
|---|---|---|---|
| CA 3650 天 / pathlen:0 / RSA 3072 | ✅ | ✅ | ✅ |
| 服务端证书 825 天 / serverAuth | ✅ | ✅ | ✅ |
| SAN | `subjectAltName=DNS:panel.example.com`（**单值**） | `build_san` 支持逗号分隔多值 | ⚠️ **漂移** |

处理：手工示例补一行注释说明多 SAN 写法（`subjectAltName=DNS:panel.example.com,IP:10.0.0.5`），并指向 §10.5。不改动既有单值示例本身（它仍然正确）。

---

## 3. 契约与影响面

本任务**不产生任何运行时行为变更**。改动集中在：

| 文件 | 改动性质 |
|---|---|
| `docs/multi-node-operations.md` | 新增 §10（约 6 小节）；修改 §6.1 备份清单；§6.2、§8 微调 |
| `docs/docker-deployment.md` | `:470` 断链改写；`:487` 补指引；`:271-311` 补多 SAN 说明 |
| `README.md` | 新增证书生命周期小节；`:484` 描述微调 |
| `pkg/panel/pki.go` | **仅注释**（`:32-34`，另 `:25` 的悬空引用） |
| `pkg/config/panel.go` | **仅注释**（`:55-56`） |

Go 侧只动注释，因此 `go build` / `go test` 必然不受影响 —— 但仍要跑，防止注释块误伤语法。

---

## 4. 验证策略

AC5 / AC6 要求 runbook「可被照着执行成功」，这是本任务最重的验证项，不能靠读文档确认。

### 4.1 AC6（服务端重签）—— 可用现成 e2e 覆盖

`scripts/test-panel-node-e2e.sh:1179-1279` 已有完整的重签场景，且断言了「节点无需重注册」。**验证方式：跑 e2e，并逐条核对 §10.5 写的命令/顺序/预期与 e2e 实际执行的一致。**

注意 e2e 因目录布局差异是直接调 openssl 而非调 `renew-server-cert` 子命令（见其注释）。所以 §10.5 的命令本身还需**手工在一个真实 install-dir 上跑一次**，确认脚本子命令可用、输出与文档描述相符。

### 4.2 AC5（过期节点恢复）—— 无现成覆盖，需手工实测

e2e 现有的是「短 TTL 触发续期」场景（`:1096` 附近），不是「已过期后重注册」。§10.4 的七步序列**必须在测试环境真跑一遍**：

1. 起一套 panel + node。
2. 人为制造过期：停 node，改系统时间或直接签一张已过期证书替换 `node.crt`。
3. 确认 node 连不上、日志报证书错误（`08-16-cert-expiry-observability` 的路径 A）。
4. 严格照 §10.4 七步操作。
5. 确认 node 恢复上线、S3 数据面可用。

任何一步与文档不符 → 改文档（不是改流程，因为 D2 已裁定流程）。

### 4.3 AC4（备份清单照单核对）

在真实 install-dir（或 e2e 产生的目录）下逐项 `ls`，五项全部命中。特别确认第 5 项「audit data」的说明准确 —— 它在 DB 内而非独立文件。

---

## 5. 回滚形状

纯文档任务，回滚 = `git revert`。无数据迁移、无兼容性问题。

唯一需注意：若 §10 的锚点被其他文档引用后再回滚，会产生新的断链 —— 但本任务是**修**断链的，回滚只会退回到原有断链状态，不制造新问题。

---

## 6. 未决与移交

- **L1 本身不在本任务解决**：CA 层级重构（引入真正的离线 root、使 intermediate 可轮换）需全网重装或双信任锚过渡期，是独立量级的任务。本任务只负责**如实登记**它。
- **`intermediate-ca.crt` 文件名名不副实**：改名是破坏性变更（既有安装的 `panel.yaml` 路径失效）。本轮只在文档中说明历史成因，不改名。若日后做 L1 重构，可一并正名。
- **`/metrics` 无证书指标**：`08-16-cert-expiry-observability` design §1.4 已明确本轮不做。§10.3 的巡检指引因此只写管理面/API 两种方式，不提 Prometheus。
- **移交 `08-16-requirement-traceability-gate`**：本任务的 prd/design/implement 三件套齐备后，正是该任务 AC6「拿四个证书子任务逐条套映射闸门」的第四个样本。
