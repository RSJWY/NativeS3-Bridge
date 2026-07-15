# 多节点 mTLS 控制面与节点部署 — 实施计划

> 本文件是 `design.md` 的执行落地清单。控制协议 = WebSocket + JSON over mTLS。
> **本任务仍处规划阶段;下列阶段在用户明确批准实现后才逐个执行,不得提前改业务代码。**
> 规模较大,建议在批准实现时拆为父/子任务(见 §Task Split),本清单按端到端顺序给出。

---

## 前置校验(实现开始前)

- [ ] `design.md` 与 `prd.md` 已用户审阅通过。
- [ ] 确认拆分方式:单任务顺序推进,还是父任务 + 子任务并行(见 §Task Split)。
- [ ] 记录当前 baseline:`go build ./... && go test ./...` 全绿(作为回归基线)。

---

## 阶段 0:共享协议包 `pkg/controlproto`(骨架先行)

- [ ] 定义消息信封结构、`type` 常量、`ProtocolVersion` 与最小兼容版本。
- [ ] 定义各 payload:`hello/hello_ack`、`heartbeat`、`desired_state`、`ack`、`task`、`task_result`、`error`。
- [ ] 定义期望状态 schema(凭据/Bucket/ACL/配额/Webhook/限流的下发表示)与内容哈希算法。
- [ ] 单元测试:信封编解码、版本协商判定、未知字段向后兼容(忽略)。

**验证**:`go test ./pkg/controlproto/...`
**回滚点**:纯新增包,删除目录即可回滚。

---

## 阶段 1:面板 PKI 与数据模型

- [ ] `pkg/panel` 新增 GORM 模型:`nodes`、`node_certs`、`registration_tokens`、`desired_configs`、`node_status`、`node_credentials`、`tasks`、`audit_logs`(见 design §6)。
- [ ] 独立迁移注册表(仿 `pkg/db/migrate.go`:`migrationModels` + `validateSchema`),与节点 DB 迁移隔离。
- [ ] PKI:离线根 CA / 在线中间 CA 加载;中间 CA 签发客户端证书;吊销集(有效指纹表)。
- [ ] Secret Key 加密:AEAD 加解密,主密钥从面板配置指向的外置文件/KMS 加载(**不落 DB**)。
- [ ] 单次注册令牌:生成(高强度、10 分钟默认有效、存哈希)、校验、用后失效。

**验证**:`go test ./pkg/panel/...`(迁移、签发、加解密、令牌生命周期)
**风险文件**:加密与主密钥加载路径——错误会导致 Secret Key 不可恢复;必须有"主密钥缺失 → 拒绝启动"的 fail-closed 测试。
**回滚点**:面板 DB 独立,回滚不影响节点。

---

## 阶段 2:节点接入端点与控制协议服务端(面板侧)

- [ ] 面板节点接入监听(独立地址/端口),mTLS(中间 CA 校验客户端证书 + 吊销集校验)。
- [ ] 一次性注册 HTTPS 端点(服务器 TLS,非 mTLS):接受 `{token, csr}` → 签发证书 → 令牌失效。
- [ ] WebSocket `/agent` 处理:握手、`hello` 版本协商、连接注册表(node_id → conn)。
- [ ] 心跳收发与 offline 判定;连接生命周期管理(重复连接、断开清理)。

**验证**:`go test ./pkg/panel/...`;集成测试:模拟节点证书完成握手 + 心跳。
**风险点**:mTLS 校验与吊销判定必须在应用逻辑前完成;测试覆盖"吊销证书被拒""过期证书被拒""未知指纹被拒"。
**回滚点**:新增监听,不影响现有服务。

---

## 阶段 3:节点 Agent(节点侧)`pkg/nodeagent` + `cmd/node`

- [ ] 首启流程:本机生成私钥(不上传)+ CSR;服务器 TLS 校验面板;提交令牌+CSR;保存签发证书。
- [ ] mTLS 拨号 + `hello`(上报 applied_version + 内容哈希);指数退避重连(上限+抖动)。
- [ ] 期望状态执行器:接收 `desired_state` → 应用到节点本地 DB(复用现有 credentials/buckets/quota 写逻辑)→ 回 `ack`(synced/failed)。
- [ ] 持久化 `applied_version`;重连对账;漂移检测(本地内容哈希 vs 面板期望哈希)。
- [ ] 节点 DB 迁移严格增量(安全网 C):只增表/增列(applied_version、drift、task records),不改不删现有 credentials/buckets/request_stats;沿用 `AutoMigrate`+`validateSchema` 并把新表加入校验清单。
- [ ] `cmd/node`:只启动 S3 数据面 + Agent 客户端,**不启动 WebAdmin 监听**;未注册初始态直接用本地 DB 提供 S3(安全网 A),对旧 config 业务字段忽略而非报错(安全网 B)。

**验证**:`go build ./cmd/node`;`go test ./pkg/nodeagent/...`;端到端:面板下发期望状态 → 节点应用 → S3 校验新凭据可用。
**风险点**:期望状态应用必须幂等且事务化,应用失败不得破坏节点既有可用配置(应用前后 S3 数据面不中断)。
**回滚点**:`cmd/node` 是新入口。硬切换下回退 = 把 node 换回上一版已发布的单体二进制(见 design §8.4);安全网 C 的增量迁移保证旧二进制忽略新表即可继续跑,回退前须在面板侧 disable 该节点。

---

## 阶段 4:一次性任务(日志查询 / 存储扫描 / 校正)

- [ ] 面板任务编排:`task` 下发(仅在线节点)、`task_id` 幂等、超时、在途上限(背压)。
- [ ] 节点任务执行器:复用 `pkg/webadmin` 的 logs 查询与 `pkg/storage` 的 reconcile(preview/apply 两段式已存在)。
- [ ] 结果回执 `task_result`:受限结果集(日志条数/时间/字节上限);扫描区分 preview 与 apply。
- [ ] 中断处理:发出后断连 → 标记 failed/unknown,要求管理员重新确认,高风险(校正 apply)不静默重试。

**验证**:`go test ./pkg/panel/... ./pkg/nodeagent/...`;集成:日志查询有上限、reconcile preview 不改数据、apply 改数据并回执。
**风险点**:reconcile apply 是高风险写操作,幂等键 + 审计 + 不自动重试必须齐备。
**回滚点**:任务通道独立于期望状态,可单独禁用。

---

## 阶段 5:面板管理面(UI + REST)与审计

- [ ] `cmd/panel`:内嵌 WebAdmin UI(迁移 `pkg/webadmin/ui`),沿用登录/Session/失败锁定/TOTP/验证码。
- [ ] 面板 REST:节点列表/详情/生命周期(active/disabled/retired)、注册令牌、证书管理、期望状态编辑与发布、任务发起与结果、状态漂移展示。
- [ ] 按节点视图:仪表盘、运行状态、有限日志、配置版本、同步状态(waiting/synced/failed/drift)。
- [ ] 批量操作:显式选目标节点,分节点记录结果,不报全局成功。
- [ ] 审计中间件:所有管理操作写 `audit_logs`,脱敏(无私钥/完整 Secret Key/令牌/Session)。
- [ ] Secret Key 只创建时返回一次;列表/详情/日志/审计不返回明文;轮换流程。

**验证**:`go test ./pkg/panel/...`;前端构建;手工走查登录→建节点→发令牌→注册→下发→任务。
**风险点**:Secret Key 泄漏面(任何返回明文的接口都是缺陷);审计脱敏必须有测试断言。
**回滚点**:UI/REST 层,回滚不影响已注册节点的数据面。

---

## 阶段 6:单节点原地迁移(硬切换,三条安全网收口)

- [ ] 安全网 A 验证:换 node 镜像后、注册完成前,用现有 `natives3.db` 断言 S3 数据面全程可读写(未注册初始态直接消费本地 DB,不阻塞)。
- [ ] 安全网 B 验证:用含业务字段的旧 `config.yaml` 启动 node,断言不报错、只消费基础设施字段。
- [ ] 节点只读上报现有 Bucket/凭据/配额/状态(明文 Secret Key 经 mTLS 上报,面板加密存)。
- [ ] 面板导入摘要 + 管理员确认;确认前不覆盖节点业务配置;确认后生成期望状态基线 version=1。
- [ ] 迁移失败保持节点原 S3 服务与本地状态可用,支持重试或放弃接管。

**验证**:端到端:用现有 `natives3.db` 样本节点走一遍导入;失败注入测试(确认前中断不改节点);安全网 A/B 各一条断言测试。
**风险点**:导入前绝不能向节点写业务配置——需测试断言"未确认导入 → 节点配置零变更";换镜像到注册完成期间 S3 不得中断(安全网 A)。
**回滚点**:放弃接管 → 节点回到"未注册但正常服务 S3"的初始态,等待重新发起注册。

---

## 阶段 7:镜像、配置、文档、备份/恢复

- [ ] 拆分/参数化 `Dockerfile` 为面板镜像与节点镜像两个构建目标。
- [ ] 节点 `config.yaml` 精简为基础设施项;面板配置新增 PKI 路径、主密钥路径、接入端口。
- [ ] `docker-compose.example.yml` 增加多节点示例。
- [ ] 文档:注册流程、安全事件处置(吊销只切控制面,还需停宿主机服务/轮换 S3 凭据)、完整恢复集与"DB 与主密钥分别备份"。
- [ ] 备份/恢复验证:仅 DB 备份不能恢复明文 Secret Key;有效证书节点恢复后免重注册。

**验证**:两镜像分别 `docker build`;`--check-config` 通过;恢复演练脚本。
**风险点**:恢复集不完整会导致灾难不可恢复;文档必须列全 §7.3 六项。
**回滚点**:镜像/配置层,可回退旧 Dockerfile。

---

## Task Split(建议)

批准实现时,建议建父任务 `multi-node-mtls-control-plane`,拆以下可独立验收的子任务:

1. `controlproto-and-pki` — 阶段 0+1(协议包 + 面板 PKI/加密/模型)
2. `panel-agent-transport` — 阶段 2+3(接入端点 + 节点 Agent + 期望状态对账)
3. `oneshot-tasks` — 阶段 4(日志/扫描/校正任务)
4. `panel-admin-ui` — 阶段 5(管理面 + 审计)
5. `migration-and-images` — 阶段 6+7(原地迁移 + 双镜像 + 备份恢复)

依赖顺序:1 → 2 → {3,4} → 5。依赖写入各子任务 `prd.md`/`implement.md`,不靠树位置隐含。

---

## 全局验证命令

```bash
go build ./...
go test ./...
go run ./cmd/panel -check-config    # 面板配置校验（实现后）
go run ./cmd/node  -check-config    # 节点配置校验（实现后）
```

## 全局回滚保证

- **硬切换**:不再维护单体入口。回滚 = 把 node 换回升级前已发布的单体二进制;安全网 C(增量迁移,只增不改)保证旧二进制忽略 node 新增表、直接用未改动的 credentials/buckets 继续跑。回退前须在面板侧先 disable 该节点,停止期望状态下发。
- 实现过程中每个阶段仍是独立可回滚的代码增量(见各阶段回滚点),失败可退回上一阶段而不影响已上线的数据面。
- 面板 DB 与节点 DB 物理隔离;面板侧改动不触碰节点既有 schema。
- `pkg/controlproto` 版本约束保证面板/节点镜像可独立回滚且协议可协商或明确拒绝。
