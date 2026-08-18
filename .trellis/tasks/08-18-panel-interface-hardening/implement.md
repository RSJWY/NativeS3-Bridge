# Panel 服务端接口加固 — 执行计划

> 前置:已读 `prd.md`、`design.md`、父任务 PRD 的部署安全红线。本任务只动 panel 侧文件。

## 环境基线(动手前先跑,确认起点干净)

```bash
go build ./... && go vet ./...
go test ./pkg/panel/...   # 注意:TestCertsRouteReturnsSnakeCaseDTO 红是已知日期炸弹,归 C1,本任务不管
```

## 执行清单(按序,每步后可独立提交)

### Step 1 R9 管理 API 语义修正(纯 handler 修复,风险最低,先热身)
1. `adminapi.go:271-272` 与 `307-308`:`loadNode` 返回 false 时直接 return。
2. `adminapi.go:417-453` certsRoute 入口加 `loadNode` 404。
3. `adminapi.go:150-151` tokens 分支、`:438` revoke 分支加路径段数校验,多余段 404。
4. `adminapi.go:708-714`:hash mismatch → 409,用 `desiredPushAdminMessage` 文案。
5. 退役节点 409:credentials/buckets/webhooks/rate-limit 路由,对齐 `updateNode:226` 的写法。
6. 每项补测试进 `adminapi_test.go` / `adminresources_test.go`。
- 验证:`go test ./pkg/panel/ -run 'TestAdmin|TestCerts|TestNodes' -v`

### Step 2 R1 trustForwarded 接线
1. `pkg/config/panel.go` webadmin 段加 `trust_forwarded`(yaml tag 与现有键风格一致)。
2. `cmd/panel/main.go` → `adminserver.go` → webadmin 的接线,参照 `cmd/natives3bridge/main.go:113-114`。
3. `configs/panel.example.yaml` 加注释键;`docs/public-deployment.md` 补指引。
4. 测试:默认 false 行为不变;true 时 X-Forwarded-For 生效(参照 `pkg/webadmin` 现有 clientIP 测试)。
- 验证:`go test ./pkg/config/ ./pkg/webadmin/ ./pkg/panel/`

### Step 3 R2 管理 server 超时
1. 先跑 `grep -rn "Flush\|SSE\|text/event-stream\|Hijack" pkg/panel pkg/webadmin` 核实流式端点,结论写进 design.md D3。
2. `adminserver.go:75-79` 加 `ReadTimeout: 30s, IdleTimeout: 120s`。
- 验证:`go test ./pkg/panel/ -run TestAdminServer`;手工 curl 一个慢 body POST 验证 30s 内被切断(可选)。

### Step 4 R4 任务状态守卫
1. `tasks.go:138` 的 running 迁移加 `state='pending'` 前置;`markState:225-235` 不再无条件清 error。
2. RowsAffected=0 时 Info 日志。
3. 竞态测试:先写终态再调 markState,断言不被覆盖。
- 验证:`go test -race ./pkg/panel/ -run 'TestTask|TestDispatch'`

### Step 5 R3 写路径可中断
1. 按 design D1 把 writeMu 换成 channel 信号量;三处调用点(心跳 ack、Dispatch、PushDesiredState)包 10s timeout ctx。
2. 并发测试:慢消费者连接 + 推送,断言 ~10s 返回、清理发生、无泄漏。
- 验证:`go test -race ./pkg/panel/ -run 'TestTransport|TestAgentConn'`

### Step 6 R5 心跳节流 + R6 错误分级 + R7 renew 限频 + R8 content_hash 消毒
按 design D2/D4/D5 实施;每项一个测试(R5:狂发节流;R6:单次错误不断连、连续 5 次断连;R7:第 11 次 429;R8:消毒后落库)。
- 验证:`go test -race ./pkg/panel/`

### Step 7 全量回归与兼容性自证
```bash
go build ./... && go vet ./...
go test -race ./pkg/panel/... ./pkg/controlproto/...
git diff --stat   # 确认未触碰 pkg/nodeagent/、pkg/controlproto/、cmd/node/、models.go、migrate.go
```

## 审查门禁(trellis-check 要点)

- 对照 PRD 的 R1-R9 逐条核对;R10 确认未越界做连接回收。
- 父任务红线:无 schema 变更、无 wire 变更、默认值不变。
- `go test -race` 必须全绿。

## 回滚点

每个 Step 独立提交;任一步出问题 `git revert` 对应提交即可。部署回滚 = 换回旧 panel 二进制,无数据迁移、node 无感知。

## 部署顺序

panel 单二进制替换,滚动重启即可;node 不需要同步升级,无顺序要求。
