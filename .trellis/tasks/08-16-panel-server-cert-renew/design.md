# 设计：panel 服务端证书重签与 SAN 补全

## 1. 边界与取舍

### 1.1 子命令 vs 独立脚本 —— 选**子命令**

`install-panel.sh` 现在是纯线性脚本：参数解析循环在 `:205` 结束，之后自上而下一路执行到 `:400`。加子命令意味着要把「装机」这段主体包进一个函数或加一道分支，是**结构性改动**。

**备选 A：独立脚本 `scripts/renew-panel-server-cert.sh`**
- 优点：零风险，装机路径一行不动。
- **否决理由**：`die` / `require_command` / `validate_install_dir` / `is_ipv4` / `is_dns_name` 会再被复制一份。仓库里这类重复已经有了（`install-node.sh:52,57,100,110`），再加一个文件就是第三份，且 SAN 判定逻辑一旦分叉，装机与重签会签出不一致的证书——这正是最不该出现的漂移。运维发现成本也更高：一个只在文档里出现的独立脚本，到期时没人想得起来。

**备选 B（采用）：`install-panel.sh` 加子命令**
- 形状：`install-panel.sh renew-server-cert --install-dir PATH --panel-host HOST[,HOST...]`。
- 不带子命令时**行为完全不变**（默认视为装机），保证 AC8 向后兼容与既有 CI/文档不破。
- 实现约束：把现有装机主体（`:249` 之后那段）收进一个函数，或在参数解析前先探测 `$1` 是否为已知子命令并分派。**推荐后者**——改动面更小，装机主体不必缩进重排，diff 可读、回滚容易。
- 复用 `die`/`require_command`/`validate_install_dir`/`is_ipv4`/`is_dns_name`/SAN 构造，天然消除分叉风险（R1.3 / R2.2）。

代价：`install-panel.sh` 从「一件事」变成「两件事」，usage 变长。可接受——它换来的是唯一一份 SAN 判定逻辑。

### 1.2 为什么不做服务端证书热重载

`cmd/panel/main.go:129` 是启动时 `tls.LoadX509KeyPair` 一次性加载。要热重载得改成 `tls.Config.GetCertificate` 回调 + 文件监听或信号触发。这在 825 天一次的操作面前收益极低，且引入新的失败模式（半加载状态）。**明确不做**，改为在输出里告知必须重启（R5.1/R5.2）。

### 1.3 为什么 CA 临期是 Warn 而不是拒绝启动

CA 过期 → 签出的证书链直接不可用，必须 fail-closed（R3.1）。但 CA **临期**时一切仍正常工作，此时拒绝启动等于自己制造停机。所以：过期/未生效 = 拒绝启动；临期 = Warn + 正常服务。

## 2. 契约

### 2.1 子命令 CLI

```
install-panel.sh renew-server-cert --install-dir PATH --panel-host HOST[,HOST...] [--days N] [--restart]
```

| 参数 | 说明 |
|---|---|
| `--install-dir` | 必填。先过 `validate_install_dir`（`install-panel.sh:81`，含危险目录黑名单） |
| `--panel-host` | 必填，逗号分隔的 SAN 列表；每项独立判 IPv4/DNS |
| `--days` | 可选，默认 825（与装机 `install-panel.sh:289` 一致） |
| `--restart` | 可选。默认重签后**不自动重启**（重启会中断所有节点控制面连接，不应默认发生）；加此 flag 显式要求重启 |

退出码：`0` 成功；非 0 由 `die` 统一产生（沿用 `install-panel.sh:47` 的 `install-panel: <msg>` 前缀格式）。

**已裁定**：采用「默认不重启、`--restart` 显式要求」语义。理由：重启会中断所有节点控制面连接，不该是默认行为。

前置检查（任一不满足即 `die`，不做任何写入）：
1. `id -u` 为 0（与装机 `:249` 一致）。
2. `require_command openssl`。
3. `<install_dir>/data/pki/intermediate-ca.crt` 与 `intermediate-ca.key` 存在且可读。
4. `<install_dir>/data/pki/panel-server.crt` 存在（重签的前提是已装机）。

### 2.2 SAN 构造（装机与重签共用）

把现有 `install-panel.sh:241-247` 的单值逻辑提为一个函数，语义：

```
build_san "host1,host2,..."   →  "DNS:host1,IP:host2,..."
```
- 逐项 trim；空项跳过但整体不得为空。
- IPv4 → `IP:`，DNS → `DNS:`，都不匹配 → `die`（R2.3，**不静默丢弃**）。
- 装机路径改为调用同一函数，单值输入结果与现状逐字节相同（AC8）。

### 2.3 CA 有效期校验（Go 侧）

`LoadIntermediateCA`（`pki.go:37-61`）在 `!cert.IsCA` 判定（`:53`）之后增加：

```
now < cert.NotBefore                    → error，含 "intermediate CA is not yet valid"
now >= cert.NotAfter                    → error，含 "intermediate CA expired at <t>"
cert.NotAfter - now < caExpiryWarnAfter  → slog.Warn（不阻断）
```

- 阈值 `caExpiryWarnAfter` 建议 **90 天**：与客户端证书 TTL 同量级，且 CA 到期意味着全网重装，90 天是能排期的最短窗口。定义为包级常量并写中文注释说明取值理由。
- 时间源：`LoadIntermediateCA` 目前无 `now` 参数。**为可测性增加一个 `now time.Time` 参数**（调用点 `cmd/panel/main.go:58` 同步改），不要在函数内部直接取当前时间——否则过期/临期分支无法写单测。
- 错误文案必须能区分「CA 自身」与服务端/客户端证书（R3.4）。

## 3. 数据流：重签前后

```
前置检查（全部通过后才动手）
  └─ 备份：panel-server.crt/key → panel-server.crt.bak.<ts> / .key.bak.<ts>   (R4.1)
     └─ 在 data/pki/ 内生成：新 key → csr → ext(SAN) → 用 CA 签发 → 新 crt   (临时文件名)
        ├─ 任一步失败 → die，现役文件未被触碰，备份可直接删除            (R4.2 / AC9)
        └─ 校验新证书：openssl 能解析、SAN 含预期条目、签发者为本 CA
           └─ 原子替换现役 panel-server.crt / panel-server.key
              └─ chmod 600 key / 644 crt；chown 10001:10001                (R4.3 / AC10)
                 └─ 删除 csr / ext / srl 中间文件                          (R4.4 / AC11)
                    └─ 输出：新 SAN 集合、到期日、重启命令、回滚方法       (R4.5 / R5.2)
```

**白名单保护（红线的结构性落地）**：所有写入路径都由 `<install_dir>/data/pki/` 前缀拼出，脚本中不出现任何指向 `panel.db`、`data/secrets/`、`intermediate-ca.key` 的写操作。`intermediate-ca.*` 仅以 `-CA` / `-CAkey` 参数被 openssl **读取**。

## 4. 兼容性

### 4.1 重签服务端证书**不会**使已注册节点失效 —— 结论与理由

不会。三条独立理由：

1. **信任锚未变**：节点验证 panel 服务端证书用的是 `Identity.CAFile`（`pkg/nodeagent/client.go:459-467`）里的 CA，而重签只换叶子证书、CA 一字未改（AC3）。新叶子证书仍由同一 CA 签发，链路照旧成立。
2. **客户端身份未变**：节点自己的客户端证书、`node_certs` 表、指纹全部不动（`pkg/panel/pki.go:151` `IsCertValid` 查的是客户端证书指纹，与服务端证书无关）。
3. **前提是 SAN 覆盖节点实际使用的连接名**。这是唯一的失效风险点：若重签时漏掉节点 `agent_url` 里用的那个名字，节点会因主机名不匹配而校验失败。所以 R2.3 坚持「非法/缺失即报错，不静默丢弃」，R2.5 坚持把最终 SAN 集合打出来。

### 4.2 版本与部署形态

| 场景 | 行为 |
|---|---|
| 旧安装 + 新脚本 | 重签子命令可直接用于 `--force` 装出来的既有目录，无需先升级什么 |
| 新脚本装机（单 SAN） | 与现状逐字节一致（AC8） |
| 新脚本装机（多 SAN） | 证书含多条 SAN；节点可用其中任一名字连 |
| docker compose 部署 | 重签只改宿主机 `data/pki/` 下的文件；容器通过 volume 看到新文件，但**进程需重启才加载**（R5.1）。`chown 10001:10001` 必须做对，否则容器内读不到 |
| 旧 panel 二进制 + 重签后的证书 | 无影响，服务端证书加载方式未变 |

### 4.3 Go 侧签名变更

`LoadIntermediateCA` 增加 `now` 参数是一处**破坏性签名变更**。调用点经核查为 `cmd/panel/main.go:58` 与 `pkg/panel/pki_test.go` 内的测试；实现时用 grep 全量确认后一并改。

## 5. 安全考量

- **私钥**：新服务端私钥生成时保持 `umask 077`（装机在 `:261` 已设，重签路径需自行设置），落盘后 chmod 600。备份的 `.key.bak.<ts>` 同样必须 600 —— 备份文件权限是容易漏的点。
- **备份留存**：备份文件含可用的旧私钥。输出中要提示运维在确认新证书工作正常后删除备份，不要长期堆在 `data/pki/`。
- **CA 私钥**：全程只读。不得因「顺手」而重新生成或改动。
- **误操作防护**：`validate_install_dir` 的危险目录黑名单（`install-panel.sh:81-90`）必须在重签路径也生效，避免 `--install-dir /` 这类输入。
- **不削弱客户端认证**：本任务不碰 `transport.go` 的 `ClientAuth`（父任务 D2 红线，AC19 用 grep 自证）。
- **SAN 扩大即攻击面扩大**：把内网 IP 写进 SAN 意味着该证书对该 IP 也有效。单租户内网场景可接受，但输出里应如实列出最终 SAN，不要让运维不知道自己签了什么。

## 6. 回滚形状

| 层面 | 回滚方式 |
|---|---|
| 单次重签失败 | 现役文件未被触碰（R4.2），删掉备份即可，无需动作 |
| 重签成功但新证书有问题 | 把 `panel-server.crt.bak.<ts>` / `.key.bak.<ts>` 改回原名，重启 panel。**输出中必须给出这两条确切命令**（R4.5） |
| 脚本改动整体回退 | 子命令分派是新增分支，删除即回到线性脚本；装机主体若采用「探测 `$1` 后分派」方案则未被重排，diff 干净 |
| Go 侧回退 | `LoadIntermediateCA` 去掉有效期校验与 `now` 参数即回到 fail-open。注意这是**安全能力回退**，仅在校验本身出错时才应回退 |

## 7. 未决与移交

### 移交给子任务 4（`08-16-cert-docs-correction`）的确切段落

- `docs/docker-deployment.md:272-312` —— 手工 openssl 命令需与脚本最终形态同步（SAN 构造若改为多值，此处也要改）。
- `docs/docker-deployment.md:476` —— 「不要关闭 TLS 验证」附近应补「连接名必须在服务端证书 SAN 内，否则用 renew-server-cert 补 SAN」。
- `docs/docker-deployment.md:480` —— `--force` 会删安装目录的警告旁边，必须补**正向指引**：服务端证书到期请用 `renew-server-cert`，不要用 `--force`（这是父 PRD 缺陷清单 F7）。
- `docs/multi-node-operations.md` —— 新增服务端证书重签 runbook（父 PRD 缺陷清单 F6）；CA 到期只能全网重装的说明（F6 / 遗留项 L1）。
- `README.md` —— 证书生命周期入口需含本子命令。

### 本轮不做

- **遗留项 L1：CA 层级重构。** 当前 `intermediate-ca.crt` 是 `pathlen:0` 自签根（`install-panel.sh:270-275`），无法在保留信任锚的前提下轮换；CA 到期（3650 天）或私钥泄露只能全网重装。真正修复需引入离线 root 并给所有节点换信任锚，涉及双信任锚过渡期，量级独立于本任务。本任务只做到「CA 过期不再 fail-open」和「如实告知后果」。
- 服务端证书热重载（§1.2）。
- 跨安装脚本抽公共 bash 库（`install-node.sh` 已有重复实现）—— 若认为值得，单独登记后续任务。
