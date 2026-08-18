# NodeAgent 控制面连接加固 — 执行计划

> 前置:已读 `prd.md`、`design.md`、父任务 PRD 红线。本任务只动 node 侧:`pkg/nodeagent/`、`cmd/node/`。

## 环境基线

```bash
go build ./... && go vet ./...
go test -race ./pkg/nodeagent/... ./pkg/controlproto/... ./cmd/node/...
```

## 执行清单

### Step 1 R2 原子落盘 + R1 证书校验(自残防护优先)
1. `persistPEM` 改 tmp+fsync+rename,带 `.bak`(design D2);全调用点回归。
2. 新增 `validateIssuedCert`(design D1),接入 `renewCertificate` 与 `RegisterContext`。
3. 测试:坏证书三例(不可解析/公钥不匹配/链不合法)均不动旧文件;好证书正常落盘;`.bak` 生成。
- 验证:`go test ./pkg/nodeagent/ -run 'TestPersist|TestValidateIssued|TestRenew|TestRegister' -v`

### Step 2 R3 握手超时 + 看门狗 + 心跳失败即断
1. handshake 包 30s 超时。
2. serve 看门狗 goroutine(design D3),`max(3×interval, 60s)`。
3. 心跳发送失败 → abortServe(Close+cancel)。
4. 测试用内存 mock websocket 对端:不应答 hello / 静默 / 注入发送失败 三场景。
- 验证:`go test -race ./pkg/nodeagent/ -run 'TestHandshake|TestWatchdog|TestHeartbeat'`

### Step 3 R4 续期重连语义
1. 引入哨兵错误区分计划内断连(design D4)。
2. 测试:续期成功后 Run 不退避、日志 Info。
- 验证:`go test ./pkg/nodeagent/ -run TestRenew`

### Step 4 R5 task 校验 + R6 version 校验
1. handleTask 入口校验 + 台账 type 比对(design D5)。
2. 连接态记录协商版本,分发校验(design D6)。
3. 测试:空 task_id、未知 type、同 id 换 type、高版本帧丢弃不断连。
- 验证:`go test -race ./pkg/nodeagent/ ./pkg/controlproto/`

### Step 5 R7 health 探针
1. 先 curl 真实 `GET /` 记录响应样例写进 design.md D7。
2. 删死分支;响应判定函数 + 单测;InsecureSkipVerify 按 R7.3 收敛。
3. 手工验证 AC10(占用端口的 http.server → 非零;真实网关 → 0)。
- 验证:`go test ./cmd/node/`

### Step 6 全量回归
```bash
go build ./... && go vet ./...
go test -race ./pkg/nodeagent/... ./pkg/controlproto/... ./cmd/node/...
git diff --stat   # 确认未触碰 pkg/panel/、wire 字段、schema
```

## 审查门禁(trellis-check 要点)

- 对照 R1-R7;确认未越界做 timeout_ms / import_report(归 C4)。
- 红线:无协议变更、无 schema 变更、无新配置键。
- 重点盯:看门狗 goroutine 泄漏(`-race` + 测试后 NumGoroutine 稳定)、原子写权限位(私钥 0600 不回归)。

## 回滚点

每 Step 独立提交;部署回滚 = 换回旧 node 二进制,panel 无感知,无数据迁移。

## 部署顺序

node 独立滚动重启即可。注意滚动期间节点会经历一次正常重连,panel 侧表现为短暂 offline→online,属预期。
