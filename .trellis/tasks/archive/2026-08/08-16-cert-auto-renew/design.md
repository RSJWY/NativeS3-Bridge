# 设计：节点客户端证书自动续期

## 1. 边界与取舍

### 1.1 为什么走 HTTP `/renew`，不走 WebSocket 控制协议消息

备选方案是给 `pkg/controlproto` 加 `cert_renew_request` / `cert_renew_response` 两条消息（`envelope.go:32-47`）。**否决**，理由：

- 协议版本协商：`controlproto` 带版本约束，加消息类型意味着版本抬升，旧 panel/新 node（或反向）在 hello 握手期被拒（`client.go:213`）。证书续期恰恰是**存量节点**才需要的能力，让它依赖协议升级是自相矛盾的。
- 复用成本：`/renew` 能直接复用 `authenticateMTLS`（`transport.go:185-200`）、`registrationBodyLimit`、`writeTransportJSON/Error`、`SignNodeCSR`。走 WS 则要在 serve loop 里手写一套请求-响应关联。
- 连接语义：续期成功后节点**必须断连重连**（R3.4，才能触发 R2 激活）。在 WS 消息里做「回一个响应然后自己把连接掀掉」比一次独立 HTTP 请求更别扭。

代价：节点需要一个额外的 HTTPS 客户端往返。可接受——注册路径（`register.go:176-199`）已有同形状的代码可参照。

### 1.2 为什么身份取自客户端证书而非请求体

`/register` 必须带 `node_id`（那时还没有证书）。`/renew` 有证书，`authenticateMTLS` 已经能把指纹解析成 `nodeID`（`pki.go:151-176`）。若再接受请求体的 `node_id`，就多出一条「A 节点的证书给 B 节点换证」的越权路径。**请求体只含 `csr_pem`**。

### 1.3 为什么允许复用现有私钥

强制换私钥会让节点在续期时多一次私钥落盘（0600 写 `node.key`），失败窗口更长且可能与正在使用的私钥打架。允许复用私钥 → 续期只写一个文件（`node.crt`），原子性更好。安全上，90 天一换证 + 私钥不动，仍显著优于现状（永不换）。

### 1.4 为什么「新证接入后才吊销旧证」（父任务 D1）

见父任务 PRD D1。此处补一条实现层理由：panel 无法确认节点是否真的落盘成功（HTTP 200 发出后节点可能崩），只有「节点用新证成功接入」这一事件才是**落盘成功的证明**。把吊销挂在这个事件上，是唯一不需要额外确认往返就能保证不锁死的做法。

## 2. 契约

### 2.1 `POST /renew`

挂载点：`TransportServer.Handler()`（`transport.go:87-92`），与 `/agent` 同 listener。

请求：
```json
{ "csr_pem": "-----BEGIN CERTIFICATE REQUEST-----\n..." }
```

响应 200（与 `registerResponse` 同形状，`transport.go:102-106`）：
```json
{ "cert_pem": "...", "ca_cert_pem": "...", "not_after": "2026-11-14T03:04:05Z" }
```

错误：

| 状态 | 触发条件 |
|---|---|
| 405 | 非 POST |
| 400 | body 非法 / 含未知字段 / `csr_pem` 空 / CSR 解析失败 / CSR 签名自校验失败 / CSR CN ≠ `node-<id>` |
| 401 | 无客户端证书；或 `authenticateMTLS` 判定不通过（过期 / 吊销 / 节点非 active / 指纹不在表内） |
| 500 | 签发或 DB 写入失败 |

### 2.2 `NodeCert` 增列

```go
// ActivatedAt 记录该证书首次成功接入控制面的时刻。为空表示已签发但节点尚未
// 用它连上来——此时旧证书仍然有效，节点可以安全回落（父任务 D1）。
ActivatedAt *time.Time
```

GORM `AutoMigrate` 对新增可空列是加列操作，符合增量迁移约束。`migrate.go` 的 `expectedTables` 已包含 `node_certs`，无需改动结构；`expectedIndexes` 不新增索引（按 `node_id` 查已有 `idx` 覆盖，`models.go:47`）。

## 3. 数据流

### 3.1 续期流程

```
node: 连接建立成功 (client.go serve loop 入口之后)
  └─ 读本地 node.crt → NotAfter, NotBefore
     └─ remaining < (NotAfter-NotBefore)/3 ?
        ├─ 否 → 什么都不做
        └─ 是 → buildCSR(现有私钥, nodeID)
                 └─ POST {renewURL} over mTLS(现证书 + CAFile)
                    ├─ 非 200 → Warn，本连接内不再重试，等下次连接
                    └─ 200   → persistPEM(node.crt, 0644)
                               └─ 主动 close 当前 ws
                                  └─ Run 循环退避后用新证重连
panel: authenticateMTLS 通过
  └─ 该指纹 activated_at IS NULL ?
     ├─ 否 → 直接放行
     └─ 是 → tx { set activated_at=now;
                  UPDATE node_certs SET revoked=1,revoked_at=now
                    WHERE node_id=? AND id<>? AND revoked=0 }
             ├─ tx 失败 → Error 日志，仍然放行（R2.3）
             └─ tx 成功 → 放行
```

### 3.2 mTLS 客户端复用

节点调 `/renew` 需要的 TLS 配置与 `client.go:453-473` `clientTLS()` 完全一致（客户端证书 + `CAFile` 作 RootCAs）。**复用该函数**，不要另写一份。

### 3.3 URL 推导（R3.6）

从 `AgentURL` 推导 `renewURL`：用 `net/url.Parse`，替换 scheme（`wss`→`https`，`ws`→`http`）、把 path 的最后一段 `/agent` 换成 `/renew`，保留 host、port、其余路径前缀。

不用字符串 `strings.Replace` —— host 里若含 `agent`（如 `agent.example.com`）会误替换。这是单测必须覆盖的点。

## 4. 兼容性

| 组合 | 行为 |
|---|---|
| 新 panel + 旧 node | 旧 node 不会调 `/renew`，行为完全不变（仍会 90 天到期）。新 panel 的 R2 激活逻辑对旧 node 首次接入同样生效（吊销集为空，无副作用）。 |
| 旧 panel + 新 node | 新 node 调 `/renew` 得 404 → 按 R3.5 记 Warn 不中断。降级为现状行为，不会崩。 |
| 旧 panel DB 原地升级 | `activated_at` 加列，既有行为 NULL。既有证书首次接入时会被判为「未激活」→ 触发一次激活 + 吊销同节点其他证书。**注意**：若某节点历史上存在多张有效证书（重放场景），升级后首次接入会把其余的吊销掉。这是期望行为（收敛到单证），但需在 implement.md 的验证步骤里显式确认一次。 |

## 5. 安全考量

- **D2 红线**：不得引入放宽标准校验的 `VerifyPeerCertificate`/`VerifyConnection`，不得改 `ClientAuth`。已过期证书在 TLS 握手阶段即被 Go 拒绝，这是**期望行为**，`/renew` 因此天然只服务未过期证书；`authenticateMTLS` 的 `IsCertValid` 是第二道（DB 侧独立时间校验，`pki.go:162`）。
- **越权**：身份只取自证书（1.2）。
- **审计**：`node_cert_renew` 只记节点 ID、新证指纹、结果。CSR 内容不入库不入日志。
- **旧证吊销窗口**：签发到激活之间，同节点存在 2 张有效证书。窗口长度 = 节点落盘 + 重连时间（秒级）。若节点永不重连，旧证在剩余有效期内仍可用、新证悬空未激活——这与现状（单张证书直到过期）风险等价，可接受。
- **DoS**：`/renew` 需有效客户端证书才能到达业务逻辑，攻击面等同 `/agent`。R3.5 的「同连接内不重试」防止节点侧循环打爆。

## 6. 回滚形状

- panel：`/renew` 路由与 R2 激活逻辑可整体回退；`activated_at` 列保留不删（只增不删原则），旧代码忽略该列。
- node：`HasCertificate()` 语义变更是本任务风险最高的改动——它会让「证书已过期」的节点从「静默重试」变成「走注册分支」。回滚即恢复 `os.Stat` 版本。
- 回滚顺序：先回 node 再回 panel（避免新 node 对着旧 panel 反复 404）。

## 7. 未决/移交

- 节点若长期离线跨过 TTL，回来后证书已过期 → 按 D2 只能令牌重注册。**这条恢复路径的运维文档由子任务 4 `08-16-cert-docs-correction` 负责**，本任务只需保证 R3.2 的 Error 日志把动作说清楚。
- Dashboard/UI 的到期展示归 `08-16-cert-expiry-observability`，本任务不做。
