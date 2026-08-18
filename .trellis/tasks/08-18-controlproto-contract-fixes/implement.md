# 控制协议契约修复 — 执行计划

> 前置:已读 `prd.md`、`design.md`;确认 C2 已合并(或先读 C2 对 agentconn 的改动)。本任务动 `pkg/controlproto/`、`pkg/panel/`、`pkg/nodeagent/` 三处。

## 环境基线

```bash
go build ./... && go vet ./...
go test -race ./pkg/controlproto/... ./pkg/panel/... ./pkg/nodeagent/...
# 复核:控制面 payload 解码未开 DisallowUnknownFields(新增可选字段的前提)
grep -rn "DisallowUnknownFields" pkg/controlproto/ pkg/panel/transport.go pkg/nodeagent/
```

## 执行清单

### Step 1 R1 心跳间隔协商
1. `HelloPayload` 加 `heartbeat_interval_ms`(omitempty);node hello 填本地间隔。
2. panel 握手记录 + 钳制(1s~10min);`SweepOffline` 按节点阈值(design D1,先查 NodeState 是否需加列,需加列则改内存方案)。
3. node_id 与证书身份不一致 Warn(R1.4)。
4. 测试:60s 心跳不抖动;旧节点(无字段)行为不变;钳制边界三例。
- 验证:`go test -race ./pkg/controlproto/ ./pkg/panel/ -run 'TestHello|TestSweep|TestHeartbeat'`

### Step 2 R2 静默连接回收
1. 看门狗(design D5)仅对上报节点启用。
2. 测试:上报节点静默被回收;未上报不回收。
- 验证:`go test -race ./pkg/panel/ -run 'TestTransport'`

### Step 3 R3 node 执行 timeout_ms
1. handleTask 包 WithTimeout;三个 task 实现响应 ctx(design D4)。
2. 核对台账「只记成功」语义,超时失败不落台账。
3. 测试:假任务 60s 内被中止回 failed;reconcile 取消安全。
- 验证:`go test -race ./pkg/nodeagent/ -run 'TestTask|TestReconcile'`

### Step 4 R4 版本 v2 + import_report 分页
1. `version.go` MaxSupported=2 + 能力注释。
2. `import_report_chunk` payload + node 分块发送(≤512 KiB/块,仅 v2)。
3. panel 重组器(上限 32 块/5 分钟超时,连接断开清理)。
4. 测试:design D6 的混跑三组合 + 重组五用例。
- 验证:`go test -race ./pkg/controlproto/... ./pkg/panel/... ./pkg/nodeagent/...`

### Step 5 文档与全量回归
1. `docs/multi-node-operations.md` 协议章节:v2 能力、部署顺序(先 panel 后 node)。
2. 全量:
```bash
go build ./... && go vet ./...
go test -race ./pkg/controlproto/... ./pkg/panel/... ./pkg/nodeagent/... ./cmd/...
git diff pkg/controlproto/   # 人工复核:既有字段零改动
```

## 审查门禁(trellis-check 要点)

- 逐条核 R1-R5,重点 R5 三种混跑组合的测试是否真实存在且有效(不是同名空测试)。
- 红线:除本任务列明处,既有 wire 字段/DB schema 零改动;「需要加列」一旦出现必须退回内存方案或上报父任务,**不得擅自迁移**。
- 与 C2/C3 的交接面:agentconn 写超时形态、node 看门狗形态,两侧机制应同构。

## 回滚点

Step 1-2、3、4 各自独立提交;部署回滚 = 二进制回退,v2 协商自动退回 v1 行为。

## 部署顺序(必须遵守)

1. 全量测试通过后先发布 **panel**,观察旧节点在线/心跳无抖动。
2. 再逐台滚动 **node**,每台观察协商版本与心跳状态。
3. 任一环节异常:回退对应二进制即可,无数据迁移。
