# NodeAgent 控制面连接加固 — 执行计划

> 前置:已读 `prd.md`、`design.md`、父任务 PRD 红线。本任务只动 node 侧:`pkg/nodeagent/`、`cmd/node/`。
> **部署策略(2026-08-18 更新)**:panel 与 node 同步升级,不做新旧混跑兼容;不匹配时握手失败断连,node 带退避重连。

## 环境基线

```bash
go build ./... && go vet ./...
go test -race ./pkg/nodeagent/... ./pkg/controlproto/... ./cmd/node/...
```

## 执行清单

### Step 1 R2 原子落盘 + R1 证书校验(自残防护优先)
1. `persistPEM` 改 tmp+fsync+rename,带 `.bak`(design D2);全调用点回归。
2. 新增 `validateIssuedCert`(design D1),接入 `renewCertificate` 与 `RegisterContext`;R1.2 增加 `not_after` 交叉核对。
3. 测试:`TestValidateIssuedCertRejections` 覆盖坏证书三例 + 过期/超长;`TestPersistPEMAtomicWrite` 验证原子写与 `.bak`;注册/续期测试 mock 返回真实证书链。
- 验证:`go test -race ./pkg/nodeagent/ -run 'TestPersistPEMAtomicWrite|TestValidateIssuedCertRejections|TestRegister|TestRenew' -v`

### Step 2 R3 握手超时 + 看门狗 + 心跳失败即断
1. `ClientConfig.HandshakeTimeout` 默认 30s,handshake 包独立超时(design D3)。
2. serve 看门狗 goroutine,`max(3×interval, 60s)`。
3. 心跳发送失败 → `ws.Close(...)` + `return`,serveLoop 的 `Read` 感知关闭后退出。
4. 测试:`TestHandshakeTimeout`、`TestWatchdogTimeout`。
- 验证:`go test -race ./pkg/nodeagent/ -run 'TestHandshakeTimeout|TestWatchdogTimeout'`

### Step 3 R4 续期重连语义
1. 引入哨兵错误 `errRenewedReconnect` 区分计划内断连(design D4)。
2. `Run` 中识别哨兵后 Info 日志、退避清零、立即重连。
3. 测试:`TestRenewedReconnectSentinel`;完整续期-重连路径留待集成测试。
- 验证:`go test -race ./pkg/nodeagent/ -run TestRenewedReconnectSentinel`

### Step 4 R5 task 校验 + R6 version 校验
1. `handleTask` 入口校验 + 台账 type 比对(design D5)。
2. 连接态记录协商版本,分发校验(design D6)。
3. 测试:`TestHandleTaskRejectsEmptyID`、`TestHandleTaskRejectsUnknownType`、`TestVersionFrameDropped`、`TestTaskTimeoutClamping`、`TestIsKnownTaskType`。
- 验证:`go test -race ./pkg/nodeagent/ -run 'TestHandleTask|TestVersionFrameDropped|TestTaskTimeout|TestIsKnownTaskType'`

### Step 5 R7 health 探针
1. 真实 `GET /` 响应样例已记录进 design.md D7。
2. 删 `"[::]"` 死分支;响应判定函数 `probeS3Listener` + 单测;InsecureSkipVerify 仅对 loopback 保留。
3. 测试:`cmd/node` 的 `TestProbeS3ListenerAcceptsS3ErrorResponse`、`TestProbeS3ListenerRejectsPlainHTTPService`、`TestProbeS3ListenerFailsWhenPortIsClosed`。
- 验证:`go test -race ./cmd/node/`

### Step 6 全量回归
```bash
go build ./... && go vet ./...
go test -race ./pkg/nodeagent/... ./pkg/controlproto/... ./cmd/node/...
git diff --stat   # 确认未触碰 pkg/panel/、wire 字段、schema
```

## 审查门禁(trellis-check 要点)

- 对照 R1-R7;确认未越界做 timeout_ms / import_report(归 C4)。
- 红线:无协议变更、无 schema 变更、无新用户配置键(`ClientConfig.HandshakeTimeout` 仅内部/测试)。
- 重点盯:看门狗 goroutine 泄漏(`-race` + 测试后 NumGoroutine 稳定)、原子写权限位(私钥 0600 不回归)。

## 关键测试「临时改坏→变红→恢复」记录

- `TestValidateIssuedCertRejections`:临时把 PEM 类型判定改为恒过,测试变红。
- `TestWatchdogTimeout`:临时把超时阈值乘 1000,测试变红。
- `TestVersionFrameDropped`:临时把版本判定阈值改成 100,测试变红。
- `TestHandleTaskRejectsEmptyID`:临时把空 task_id 校验短路,测试变红。

## 回滚点

每 Step 独立提交;部署回滚 = 两侧二进制同时回退。无 DB schema 变更,无数据迁移。
