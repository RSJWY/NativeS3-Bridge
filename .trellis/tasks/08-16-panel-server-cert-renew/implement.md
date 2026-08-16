# 执行计划：panel 服务端证书重签与 SAN 补全

> 先读 `prd.md` → `design.md`。父任务承重决策见 `.trellis/tasks/08-16-node-cert-lifecycle/prd.md`。
> 每个 Step 结束跑该步验证命令；Gate 处停下自查，不通过不进下一步。

## 环境已核实

- **`shellcheck` 在本机不可用**（已确认）。AC16 的 shellcheck 部分按「有则跑、无则跳过」处理，`bash -n` 是必跑项。
- **CI 门禁**：`.github/workflows/release.yml:143` 会跑 `scripts/test-distribution-contract.sh`，该脚本 `:55-64` 对两个安装器逐个断言存在 `--install-dir` / `--tag` / `--force` / `--no-start` / `docker compose` 文本，`:66-69` 另对 `install-panel.sh` 断言 `ghcr.io/rsjwy/natives3-panel`、`openssl rand -out`、`panel-ca.crt`、`127.0.0.1:9001:9001`。**改动 usage 或参数解析时不得让这些文本消失**，否则发布门禁红。

## Step 1 — Go 侧 CA fail-closed 与临期告警

- [ ] 1.1 `pkg/panel/pki.go`：`LoadIntermediateCA` 增加 `now time.Time` 参数（design §2.3，为可测性；不得在函数内部取当前时间）。
- [ ] 1.2 在 `!cert.IsCA` 判定（`pki.go:53`）之后增加：`now < NotBefore` → error「intermediate CA is not yet valid」；`now >= NotAfter` → error「intermediate CA expired at <t>」。文案必须能区分 CA 自身与服务端/客户端证书（R3.4）。
- [ ] 1.3 增加包级常量 `caExpiryWarnAfter = 90 * 24 * time.Hour`，带中文注释说明取值理由（与客户端证书 TTL 同量级；CA 到期意味全网重装，90 天是能排期的最短窗口）。剩余期低于此值时 `slog.Warn`，含剩余天数与后果提示，**不阻断**。
- [ ] 1.4 `grep -rn 'LoadIntermediateCA' --include=*.go .` 全量改调用点（已知 `cmd/panel/main.go:58` 与 `pkg/panel/pki_test.go`）。
- [ ] 1.5 单测：CA 过期 → error；未生效 → error；临期 → 成功且有 Warn；有效期充足 → 成功且无 Warn（AC12–AC15）。

验证：`go build ./... && go test ./pkg/panel/ -run 'IntermediateCA|LoadCA' -v`

### Gate A — D2 红线自证
```bash
grep -n 'ClientAuth' pkg/panel/transport.go     # 必须仍是 tls.VerifyClientCertIfGiven
grep -rn 'VerifyPeerCertificate\|VerifyConnection' pkg/panel/ --include=*.go
```
第二条在非测试文件中若有新增命中 → 违反父任务 D2，停下重做（AC19）。

## Step 2 — SAN 构造函数化（装机路径行为不变）

- [ ] 2.1 `scripts/install-panel.sh`：把 `:241-247` 的单值 SAN 逻辑提为 `build_san "host1,host2,..."`，逐项 trim、复用既有 `is_ipv4`（`:60`）与 `is_dns_name`（`:71`）判定，IPv4 → `IP:`、DNS → `DNS:`，任一项不合法即 `die`（**不静默丢弃**，R2.3）。整体为空亦 `die`。
- [ ] 2.2 装机路径改为调用 `build_san "$panel_host"`。
- [ ] 2.3 **回归确认**：单值输入下生成的 `san` 变量值与改动前逐字节相同（AC8）。用一次 `--no-start` 装机到临时目录，比对 `openssl x509 -noout -text` 的 SAN 段。
- [ ] 2.4 `--panel-host` 的 usage（`:25`）与提示文案（`:209`）更新为支持逗号分隔多值；`:246` 的错误文案同步。

验证：`bash -n scripts/install-panel.sh` && 临时目录 `--no-start` 装机比对 SAN

## Step 3 — 子命令分派

- [ ] 3.1 在参数解析循环之前（`:~180` 之前）加子命令探测：若 `$1` 为 `renew-server-cert` 则 `shift` 后进入重签分支；否则走现有装机路径（design §1.1 推荐方案——**不重排装机主体**，保证 diff 干净、回滚容易）。
- [ ] 3.2 `usage()`（`:17-45`）增加子命令段落，描述准确（AC1）。**不得删除**现有 `--install-dir` / `--tag` / `--force` / `--no-start` 文本（CI 门禁，见「环境已核实」）。
- [ ] 3.3 重签分支的参数：`--install-dir`（必填）、`--panel-host`（必填，多值）、`--days`（默认 825，与 `:289` 一致）、`--restart`（默认**不**重启；design §2.2 已裁定用 `--restart` 而非 `--no-restart`，并同步修正 design 该节的参数表）。

验证：`bash -n scripts/install-panel.sh && bash scripts/install-panel.sh -h && bash scripts/test-distribution-contract.sh`

### Gate B — CI 门禁不破
```bash
bash scripts/test-distribution-contract.sh
```
必须通过。这是发布门禁（`release.yml:143`），改 usage 极易踩。

## Step 4 — 重签实现（红线所在）

- [ ] 4.1 前置检查，全部通过后才动手（design §2.1）：`id -u` 为 0；`require_command openssl`；`validate_install_dir`（复用 `:81`，含危险目录黑名单）；`intermediate-ca.crt`/`.key` 存在可读；`panel-server.crt` 存在。
- [ ] 4.2 `umask 077`（重签路径需自行设置，装机的 `:261` 不覆盖此分支）。
- [ ] 4.3 备份：`panel-server.crt` → `panel-server.crt.bak.<ts>`，`panel-server.key` → `panel-server.key.bak.<ts>`。**备份的 .key 也必须 chmod 600**（易漏点，design §5）。
- [ ] 4.4 在 `data/pki/` 内用临时文件名生成：新 key → csr → ext（含 `basicConstraints=critical,CA:FALSE`、`keyUsage=critical,digitalSignature,keyEncipherment`、`extendedKeyUsage=serverAuth`、`subjectAltName=$san`，与装机 `:283-287` 一致）→ `openssl x509 -req` 用 CA 签发（`-CA`/`-CAkey` 只读 CA）。
- [ ] 4.5 校验新证书后再替换：能解析、SAN 含预期条目、签发者为本 CA。任一步失败 → `die`，现役文件未被触碰（AC9）。
- [ ] 4.6 原子替换现役 `panel-server.crt`/`.key`；`chmod 600` key、`644` crt、`chown 10001:10001`（对齐 `:359-368`，AC10）。
- [ ] 4.7 删除 csr/ext/srl 中间文件（与 `:296-298` 一致，AC11）。
- [ ] 4.8 **红线结构性保护**：重签分支内所有写入路径均由 `<install_dir>/data/pki/` 前缀拼出；脚本中不出现任何指向 `panel.db`、`data/secrets/`、`intermediate-ca.key` 的写操作。
- [ ] 4.9 输出：最终 SAN 集合、新证到期日、重启命令（参照 `:370` 的 compose 调用形式）、回滚方法（备份文件名 + 改回原名 + 重启，R4.5）、以及提示确认无误后删除备份。

验证：见 Gate C

### Gate C — 红线与原子性实证
```bash
# 在临时目录先 --no-start 装一套，记录基线
sha256sum <dir>/data/panel.db <dir>/data/secrets/master.key \
          <dir>/data/pki/intermediate-ca.crt <dir>/data/pki/intermediate-ca.key > /tmp/before.txt
# 重签（补入内网 IP）
bash scripts/install-panel.sh renew-server-cert --install-dir <dir> --panel-host s3admin.example.com,10.10.80.106
# 红线断言：以上四个文件必须一字未变
sha256sum -c /tmp/before.txt
# SAN 断言（注意：以实机输出为准调整匹配方式）
openssl x509 -noout -text -in <dir>/data/pki/panel-server.crt | grep -A1 'Subject Alternative Name'
# 权限断言
stat -c '%a %U:%G' <dir>/data/pki/panel-server.key <dir>/data/pki/panel-server.crt
# 中间文件不残留
ls <dir>/data/pki/ | grep -E '\.(csr|ext|srl)$' && echo "残留！" || echo "干净"
```
`sha256sum -c` 必须全 OK（AC2/AC3，红线）。SAN 段应同时出现 `DNS:` 与 `IP:` 条目（AC5/AC6）；**若 openssl 输出格式与预期不符，以实机输出为准调整断言写法，不要改证书**。

失败注入（AC9）：临时把 CA key 改成不可读或塞入损坏内容，跑重签，断言 `panel-server.crt` 的 sha256 未变。

## Step 5 — 已注册节点不失效实证

- [ ] 5.1 在 `scripts/test-panel-node-e2e.sh` 的既有 PKI 搭建基础上（`:341-352` 有 `-days 2` 的自签惯例可参照）加一段：节点注册并连上 → 重签服务端证书（SAN 保留节点使用的连接名）→ 重启 panel → **断言节点无需重新注册即重连成功**（AC4）。
- [ ] 5.2 断言 `node_certs` 表内容与节点本地 `node.crt` 均未改变。

验证：`bash scripts/test-panel-node-e2e.sh`

### Gate D — 全量收口
```bash
bash -n scripts/install-panel.sh
command -v shellcheck >/dev/null && shellcheck scripts/install-panel.sh || echo "shellcheck 不可用，跳过"
bash scripts/test-distribution-contract.sh
go build ./... && go vet ./... && go test ./... && gofmt -l .
bash scripts/test-panel-node-e2e.sh
```
`gofmt -l .` 必须无输出。逐条对 `prd.md` 的 AC1–AC19 打勾。

## 回滚点

| 回滚到 | 动作 |
|---|---|
| Step 4 之前 | 子命令存在但不做事；装机路径已受 Step 2 影响，但 AC8 保证行为不变 |
| Step 3 之前 | 无子命令，脚本回到纯线性；Step 2 的 `build_san` 可保留（装机行为不变） |
| Step 2 之前 | 脚本完全回退 |
| Step 1 之前 | Go 侧回到 fail-open。**这是安全能力回退**，仅在校验本身出错时才回退 |

## 已知坑

- **CI 文本门禁**：`test-distribution-contract.sh:55-69` 靠 grep 断言脚本内文本。改 usage / 参数解析时保留 `--install-dir`、`--tag`、`--force`、`--no-start`、`docker compose`、`openssl rand -out`、`panel-ca.crt`、`127.0.0.1:9001:9001` 这些串。
- **备份私钥权限**：`.key.bak.<ts>` 含可用旧私钥，必须 600。这是最容易漏的一处。
- **容器读不到新证书**：`chown 10001:10001` 漏掉会导致 panel 容器启动时读不了新 key，症状像「证书坏了」。
- **`--force` 是灾难操作**：本任务的存在意义就是让运维不必用它。实现时不要在重签分支里复用任何带 `rm -rf` 的代码路径。
- **`LoadIntermediateCA` 签名变更**是破坏性的，用 grep 全量确认调用点再改。
- `Edit` 工具在本仓库历史会话出现过「报成功但未落盘」，大文件改完务必读回确认。
