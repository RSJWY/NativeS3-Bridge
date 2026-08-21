# panel 服务端证书重签与 SAN 补全

> 父任务：`.trellis/tasks/08-16-node-cert-lifecycle`（承重决策与跨子任务验收标准见父 PRD，不得重新裁决）
> 与 `08-16-cert-auto-renew` 并行，无依赖。它管客户端证书，本任务管**服务端证书与 CA**，边界不重叠。

## Goal

给 panel 服务端证书提供**非破坏性**重签路径（顺带支持多 SAN，收口内网 IP 访问的老坑），并让 CA 自身临期/过期从 fail-open 变为 fail-closed。消除「825 天到期时运维重跑安装脚本、连带删掉 panel DB 和主密钥」这一灾难操作。

## 现状（实现前必读）

### 安装脚本

| 事实 | 位置 |
|---|---|
| **无任何子命令**，`usage` 只有 `--panel-host/--install-dir/--tag/--db-*/--force/--no-start/-h` | `scripts/install-panel.sh:17-45` |
| 参数解析是单个 while/case 循环，`:205` 结束后**自上而下一次性执行**到底 | `install-panel.sh:~180-205` |
| **`--force` 直接 `rm -rf -- "$install_dir"`** —— 连带 panel DB、`master.key`、既有 PKI 全删 | `install-panel.sh:254-259` |
| SAN **只支持单个**：`is_ipv4` → `san="IP:$x"`，`is_dns_name` → `san="DNS:$x"`，二者互斥 | `install-panel.sh:241-247`（`is_ipv4` 定义 `:60-69`，`is_dns_name` 定义 `:71-79`） |
| CA 自签 10 年，`pathlen:0`（实为自签根，非中间 CA） | `install-panel.sh:268-275` |
| 服务端证书 825 天，`extendedKeyUsage=serverAuth`，SAN 来自上面那个单值 | `install-panel.sh:277-295` |
| 中间文件（csr/ext/srl）签完即删 | `install-panel.sh:296-298` |
| `cp intermediate-ca.crt → panel-ca.crt`（给 node 拷的公共 CA） | `install-panel.sh:299` |
| 权限收口：`chown 10001:10001`、data 目录 700、key 600、crt 644 | `install-panel.sh:359-368` |
| compose 校验与启动 | `install-panel.sh:370-376` |
| 安装结束的输出模板（含 `Node control endpoint` 与 `Public CA to copy to nodes`） | `install-panel.sh:378-389` |
| 可复用的既有 helper：`die`（`:47`）、`require_command`（`:52`）、`validate_install_dir`（`:81`，带危险目录黑名单）、`yaml_quote`（`:105`） | 同文件 |
| **没有供安装器复用的公共 bash 库**：`scripts/lib/` 只有 `integration-test-helpers.sh`（测试用），`scripts/internal/` 是 python/smoke/upgrade-inspect。`die`/`require_command`/`is_dns_name`/`yaml_quote` 在 `install-node.sh:52,57,100,110` **各自重复实现了一份** | `scripts/lib/`、`scripts/internal/`、`install-node.sh` |

### Go 侧

| 事实 | 位置 |
|---|---|
| **CA 加载只校验 `IsCA`，不看自身 `NotAfter` → fail-open** | `pkg/panel/pki.go:37-61`，判定在 `:53` |
| CA 缺失/损坏时 panel 拒绝启动（既有 fail-closed 风格，要延续） | `cmd/panel/main.go:58` |
| 服务端证书在**进程启动时一次性加载**，无热重载 | `cmd/panel/main.go:129` `tls.LoadX509KeyPair` |
| 一张 CA 同时是：客户端证书签发者、服务端证书签发者、双向信任锚 | `pkg/panel/transport.go:754-755`（`pool.AddCert`）+ `pkg/nodeagent/client.go:459-467`（node 侧 RootCAs） |

### 文档

`docs/docker-deployment.md:272-312` 有一份**等价的手工 openssl 命令**（同为 3650 天 CA + 825 天服务端证书）。脚本改动后此处会漂移，需同步——正文更新归子任务 4，本任务负责在移交项里点明具体段落。

### 已知运维坑（本任务要正面解决）

装机时 `--panel-host` 写域名 → 服务端证书 SAN 只有域名 → 节点用内网 IP 连 9443 时 TLS 校验失败（证书无对应 IP SAN），且节点严格校验主机名不会跳过。当前只能靠 hosts / compose `extra_hosts` 把域名指到内网 IP 绕过。

## Requirements

### R1 非破坏性重签入口
- R1.1 提供重签服务端证书的能力，**绝不触碰** panel DB、`data/secrets/master.key`、`node_certs` 数据、既有 CA 私钥（CA 只作为签发者被读取，不被重写）。
- R1.2 入口形状：`install-panel.sh` 增加子命令（见 design §1.1 的取舍与最终选择）。必须在 `usage` 中可见，使运维能自行发现，而不是只存在于文档里。
- R1.3 **必须复用**既有 helper：`die`、`require_command`、`validate_install_dir`（含危险目录黑名单）。不得复制粘贴出第二份。
- R1.4 重签**幂等且可重入**：重复执行结果一致；执行失败不得留下半成品（见 R4 原子性）。
- R1.5 重签只在给定 `--install-dir` 下的 `data/pki/` 内操作，路径构造前先过 `validate_install_dir`。

### R2 多 SAN
- R2.1 装机（`--panel-host`）与重签都支持**同时**写入多个 SAN：域名与 IPv4 混合。
- R2.2 输入中每一项独立判定并加正确前缀：IPv4 → `IP:`，DNS 名 → `DNS:`。**复用** `is_ipv4`（`install-panel.sh:60`）与 `is_dns_name`（`:71`），不得新写判定逻辑。
- R2.3 任一项都不合法即 `die`，不得静默丢弃某一项（静默丢弃会导致证书看着签成功、实际缺 SAN，故障极难定位）。
- R2.4 保持**向后兼容**：单值 `--panel-host` 的既有用法与输出行为不变。
- R2.5 装机输出（`install-panel.sh:378-389`）要如实反映最终 SAN 集合，让运维当场看到节点可以用哪些名字连。

### R3 CA fail-closed 与临期告警
- R3.1 `LoadIntermediateCA`（`pki.go:37-61`）增加自身有效期校验：已过期 → 返回错误，panel 拒绝启动（延续 `cmd/panel/main.go:58` 的 fail-closed 风格）。
- R3.2 CA 尚未生效（`now < NotBefore`）同样拒绝启动。
- R3.3 CA 临近过期时告警：启动时打一条明确的 Warn，含剩余天数与「CA 到期需全网重装」的后果提示（阈值见 design §2.3）。
- R3.4 R3.1/R3.2 的错误信息要能让人一眼定位到是 **CA 自身**过期，而非节点证书或服务端证书过期——这三者混淆的代价很高。

### R4 原子性与回滚
- R4.1 重签前把既有 `panel-server.crt` / `panel-server.key` 备份到同目录带时间戳的文件。
- R4.2 新证书生成、校验通过后才替换现役文件；任一步失败即保留原状。
- R4.3 替换后权限与所有权与装机一致：key 600、crt 644、`chown 10001:10001`（对齐 `install-panel.sh:359-368`）。
- R4.4 中间文件（csr/ext/srl）用完即删，与装机行为一致（`install-panel.sh:296-298`）。
- R4.5 输出明确告知回滚方法（备份文件名 + 如何还原）。

### R5 生效方式
- R5.1 明确并在输出中告知：服务端证书在 panel 进程启动时一次性加载（`cmd/panel/main.go:129`），**重签后必须重启 panel 容器才生效**。本轮不实现热重载。
- R5.2 给出确切的重启命令（与装机使用的 compose 调用方式一致，参照 `install-panel.sh:370`）。
- R5.3 明确回答并写入输出与文档移交项：**重签服务端证书不会使已注册节点失效**（理由见 design §4.1）。

## Acceptance Criteria

- [ ] AC1 重签子命令在 `install-panel.sh -h` 的 usage 中可见，描述准确。
- [ ] AC2 **重签后 `panel.db`、`data/secrets/master.key` 的 mtime 与内容校验和均未改变**（红线，须显式断言）。
- [ ] AC3 重签后 `intermediate-ca.key` / `intermediate-ca.crt` 内容未改变（CA 只被读取）。
- [ ] AC4 重签后已注册节点**无需重新注册**即可连上控制面（用 e2e 或手工实证）。
- [ ] AC5 多 SAN 装机：`--panel-host` 同时给域名与 IPv4，生成证书的 SAN 同时包含 `DNS:` 与 `IP:` 两类条目。
- [ ] AC6 多 SAN 重签：对既有安装补入内网 IP 后，节点用 IP 连 9443 TLS 校验通过（收口老坑）。
- [ ] AC7 非法 SAN 输入（如 `1.2.3.999`、含下划线的名字）→ 脚本 `die`，不静默丢弃、不生成证书。
- [ ] AC8 单值 `--panel-host` 的既有装机行为与输出完全不变（向后兼容）。
- [ ] AC9 重签中途失败（注入 openssl 失败）→ 现役 `panel-server.crt/key` 保持原内容，panel 仍可正常启动。
- [ ] AC10 重签成功后现役文件权限为 key 600 / crt 644，owner 为 `10001:10001`。
- [ ] AC11 中间文件 csr/ext/srl 在 `data/pki/` 下不残留。
- [ ] AC12 CA 已过期时 panel **拒绝启动**，错误信息明确指向 CA 自身过期。
- [ ] AC13 CA 未生效（NotBefore 在未来）时 panel 拒绝启动。
- [ ] AC14 CA 临期时 panel 启动并打出含剩余天数的 Warn，正常提供服务（临期不阻断）。
- [ ] AC15 CA 有效期充足时无该 Warn（不制造噪音）。
- [ ] AC16 `bash -n scripts/install-panel.sh` 通过；若仓库/环境有 shellcheck 则 shellcheck 亦通过。
- [ ] AC17 `go build ./... && go vet ./... && go test ./... && gofmt -l .` 全绿。
- [ ] AC18 输出中包含重启命令与回滚方法（R4.5 / R5.2）。
- [ ] AC19 **未触碰客户端证书签发/校验语义**：`pkg/panel/transport.go` 的 `ClientAuth` 未变，未引入放宽标准校验的 `VerifyPeerCertificate`（父任务 D2 红线，grep 自证）。

## 约束

- **CA 层级重构不在本轮范围**：引入真正的离线 root、使 intermediate 可轮换 = 父任务遗留项 L1，需全网重装或双信任锚过渡期，量级独立。可在 design 的移交节描述，不得变成实现步骤。
- **红线：重签流程绝不触碰 panel DB、`master.key`、`node_certs` 数据。** 要在脚本层面做成结构性保护（只在 `data/pki/` 下白名单操作），而非靠注释提醒。
- 不得改动客户端证书的签发/校验语义（那是 `08-16-cert-auto-renew` 的地盘）。
- 脚本改动必须与 `docs/docker-deployment.md:272-312` 的手工 openssl 命令保持一致。文档正文更新归子任务 4，本任务在 design 移交节列出需同步的确切段落。
- 延续既有 fail-closed 风格：CA / 主密钥缺失或不可用时 panel 拒绝启动。
- 不引入 openssl 之外的新 CLI 依赖，不新增 Go 第三方依赖。
- 不实现服务端证书热重载（R5.1）。

## Notes

- 本任务同时解决一个长期运维痛点（内网 IP 连不上 9443），价值不止于「到期前有路可走」。
- `install-node.sh` 里 `die`/`require_command`/`is_dns_name`/`yaml_quote` 已各自重复一份；本任务**不做**跨脚本抽公共库的重构（范围外），但不得让重复再增加一处。若认为值得抽库，单独登记后续任务。
- 代码落盘由独立会话执行；本会话负责规划与验收。
