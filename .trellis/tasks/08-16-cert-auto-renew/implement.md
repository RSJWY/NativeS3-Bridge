# 执行计划：节点客户端证书自动续期

> 先读 `prd.md` → `design.md`。承重决策 D1/D2/D3 在父任务 PRD，不得擅自变更。
> 每个 Step 结束跑一次该步的验证命令；每个 Gate 处停下自查，不通过不进下一步。

## Step 1 — panel 模型与迁移

- [ ] 1.1 `pkg/panel/models.go` 的 `NodeCert`（:44-55）增 `ActivatedAt *time.Time`，带中文注释说明「为空 = 已签发未激活，旧证仍可回落」。
- [ ] 1.2 确认 `pkg/panel/migrate.go` 无需改动（`node_certs` 已在 `expectedTables:29`；不新增索引）。
- [ ] 1.3 写迁移增量性测试：用旧 schema（无该列）建库 → 塞一行 `NodeCert` → 跑 `Migrate` → 断言列存在、旧行完好、`ActivatedAt` 为 NULL。放 `pkg/panel/migrate_test.go`。

验证：`go test ./pkg/panel/ -run 'Migrat' -v`

## Step 2 — panel `/renew` 端点

- [ ] 2.1 `pkg/panel/transport.go`：`Handler()`（:87-92）注册 `mux.HandleFunc("/renew", s.handleRenew)`。
- [ ] 2.2 定义 `renewRequest{ CSRPEM string \`json:"csr_pem"\` }`。响应**复用** `registerResponse`（:102-106），不要另定义。
- [ ] 2.3 `handleRenew`：405 非 POST → `authenticateMTLS` 不通过则 401 → 解码 body（`io.LimitReader(r.Body, registrationBodyLimit)` + `DisallowUnknownFields`）→ `csr_pem` 空则 400。
- [ ] 2.4 CSR 主体校验：解析 CSR，校验 `CheckSignature()`，校验 `csr.Subject.CommonName == nodeSubject(nodeID).CommonName`（复用 `pki.go:136`），不符 → 400。
- [ ] 2.5 签发：`s.deps.CA.SignNodeCSR(csrPEM, nodeID, s.deps.ClientCTTL, now)` → 插入 `NodeCert{NodeID, Fingerprint, Serial, NotBefore, NotAfter}`，`ActivatedAt` 留空。**不吊销旧证**。
- [ ] 2.6 审计 `s.audit("node_cert_renew", nodeID, <新证指纹>, "issued"|"denied")`。CSR 不入日志。
- [ ] 2.7 返回 `{cert_pem, ca_cert_pem: s.deps.CA.CertificatePEM(), not_after}`。

验证：`go build ./... && go test ./pkg/panel/ -run 'Renew' -v`

### Gate A — D2 红线自证
```bash
grep -n 'ClientAuth' pkg/panel/transport.go          # 必须仍是 tls.VerifyClientCertIfGiven
grep -rn 'VerifyPeerCertificate\|VerifyConnection' pkg/panel/ pkg/nodeagent/ --include=*.go
```
第二条命令若在**非测试**文件中出现新增命中 → 违反 D2，停下重做。

## Step 3 — 新证激活与旧证吊销

- [ ] 3.1 `pkg/panel/pki.go` 新增 `ActivateCert(db *gorm.DB, fingerprint string, nodeID uint, now time.Time) error`：事务内 ① `UPDATE node_certs SET activated_at=? WHERE fingerprint=? AND activated_at IS NULL`；② `UPDATE node_certs SET revoked=1, revoked_at=? WHERE node_id=? AND fingerprint<>? AND revoked=0`。
  - 幂等要点：①的 `AND activated_at IS NULL` 保证不改写；若①的 `RowsAffected == 0` 说明已激活过，直接返回 nil**不执行②**。
  - `fingerprint<>?` 保证不自吊销。
- [ ] 3.2 `authenticateMTLS`（`transport.go:185-200`）在 `return fp, id, true` 前调 `ActivateCert`。失败只 `slog.Error("activate node certificate failed", ...)`，**仍返回 true**（R2.3）。
- [ ] 3.3 测试：
  - 新证首次接入 → `activated_at` 非空、同节点其余未吊销证书全 `revoked`、自己未被吊销（AC7）。
  - 同证接入 3 次 → 无额外吊销、`activated_at` 不变（AC8）。
  - 注入 DB 错误 → 连接仍建立、有 Error 日志（AC9）。
  - 无旧证场景（首次注册）→ 无副作用（R2.5）。

验证：`go test ./pkg/panel/ -run 'Activate|Renew|MTLS' -v`

## Step 4 — 节点侧证书解析与 `HasCertificate` 语义变更

- [ ] 4.1 `pkg/nodeagent/register.go`：新增 `(id Identity) LoadCertificate() (*x509.Certificate, error)` —— 读 `CertFile`、PEM 解码、`x509.ParseCertificate`。
- [ ] 4.2 改 `HasCertificate()`（:45-55）为：`KeyFile` 与 `CertFile` 均存在 **且** `LoadCertificate()` 成功 **且** `now < NotAfter`。保留原有的中文/英文注释风格，更新注释说明新语义。
- [ ] 4.3 新增 `RenewalThreshold(cert *x509.Certificate) time.Duration` = `cert.NotAfter.Sub(cert.NotBefore) / 3`，以及 `NeedsRenewal(cert, now) bool`。
- [ ] 4.4 测试（AC10）：有效证书 → true；已过期 → false；非法 PEM → false；缺 key → false。阈值函数：90 天证书 → 30 天阈值；剩 31 天 → 不需续期；剩 29 天 → 需续期（AC11/AC12 的单元层）。

验证：`go test ./pkg/nodeagent/ -run 'HasCertificate|Renewal|LoadCertificate' -v`

### Gate B — 语义变更影响面
```bash
grep -rn 'HasCertificate' --include=*.go .
```
逐个确认调用点在新语义下行为正确（当前已知只有 `cmd/node/main.go:199` 与测试）。

## Step 5 — 节点侧启动分支的明确报错

- [ ] 5.1 `cmd/node/main.go:199-203`：无证书/证书过期且无令牌时，把 `slog.Warn` 升为 `slog.Error`，文案明确区分「从未注册」与「本地证书已过期或损坏」两种情况，并写清恢复动作（管理面签发一次性注册令牌，填入 node.yaml 后重启）。
- [ ] 5.2 保持 return 后不影响 S3 数据面（安全网 A，不要改成 fatal）。

验证：`go build ./cmd/node/`

## Step 6 — 节点侧续期执行

- [ ] 6.1 `pkg/nodeagent/client.go`：新增 `renewURLFromAgentURL(agentURL string) (string, error)` —— 用 `net/url.Parse`，scheme `wss`→`https` / `ws`→`http`，path 末段 `agent`→`renew`，保留 host/port/路径前缀。**不用 strings.Replace**（design §3.3）。
- [ ] 6.2 单测（AC14）：`wss://h:9443/agent` → `https://h:9443/renew`；`wss://agent.example.com:9443/agent` → host 不被误改；`wss://h/x/agent` → `https://h/x/renew`；非法 URL → error。
- [ ] 6.3 新增 `(c *Client) renewCertificate(ctx) error`：`LoadCertificate` → `buildCSR`（复用 `register.go:97`，需导出或在包内直接调用）→ 用 `clientTLS()`（`client.go:453`）建 HTTP 客户端 → POST → 解析响应 → `persistPEM(CertFile, cert, 0644)`（复用 `register.go:286`）。
- [ ] 6.4 在连接建立成功后的位置（`serve`/handshake 完成之后）加一次检查：`NeedsRenewal` 为真 → 调 `renewCertificate`。
  - 成功 → Info 日志 + 主动关闭当前 ws，让 `Run`（:111-130）重连（R3.4）。
  - 失败 → Warn 日志，**本次连接内不再重试**（用连接级 flag，R3.5）。
- [ ] 6.5 确认失败路径不影响 S3 与当前连接（AC13）。

验证：`go test ./pkg/nodeagent/ -v`

## Step 7 — 注释与承诺对齐

- [ ] 7.1 核对 `pkg/panel/pki.go:22` 的 `// Nodes renew over mTLS before expiry (see design §3.3).`：若实现与之一致则保留，否则改写为与代码一致的描述（R4.1 / 父任务 AC6）。

## Step 8 — e2e

- [ ] 8.1 `scripts/test-panel-node-e2e.sh` 增短 TTL 场景：panel 配一个很短的 `client_cert_ttl`（脚本内已有 `-days 2` 的 CA 惯例，:341-352 可参照），实证「签发 → 临期 → 自动续期 → 新证重连 → 旧证被吊销」，并断言过程中控制面未长时间中断（父任务 AC1）。
- [ ] 8.2 断言 S3 数据面在整个过程中可用（父任务 AC3）。

验证：`bash scripts/test-panel-node-e2e.sh`

### Gate C — 全量收口
```bash
go build ./... && go vet ./... && go test ./... && gofmt -l .
grep -n 'ClientAuth' pkg/panel/transport.go
bash scripts/test-panel-node-e2e.sh
```
`gofmt -l .` 必须无输出。逐条对 `prd.md` 的 AC1–AC18 打勾。

## 回滚点

| 回滚到 | 命令/动作 |
|---|---|
| Step 6 之前 | 节点不发起续期，panel 端点空转，行为等同现状 |
| Step 4 之前 | `HasCertificate` 恢复 `os.Stat` 语义（**风险最高的改动**，优先单独回滚这一处） |
| Step 1 之前 | 完整回退；`activated_at` 列保留不删，旧代码忽略之 |

回滚顺序：**先 node 再 panel**（避免新 node 对旧 panel 反复 404）。

## 已知坑（务必注意）

- `Edit` 工具在本仓库历史会话中出现过「报成功但未落盘」，大文件改动后务必读回确认（见 `[[multi-node-mtls-task]]` 记录）。
- 旧 panel DB 升级后，历史上存在多张有效证书的节点，首次接入会把其余证书吊销。这是期望的收敛行为，但 Step 3 验证时要显式跑一遍这个场景确认无意外（design §4）。
- `buildCSR` 目前是包内小写函数（`register.go:97`），Step 6.3 在同包内可直接调用，不要为此导出。
