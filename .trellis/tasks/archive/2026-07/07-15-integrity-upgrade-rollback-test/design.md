# 完整性与版本升级回滚专项测试 — 技术设计

## 1. 测试分层

本任务采用四层验证，失败时优先在最靠近根因的层修复：

1. 源码层：格式、vet、构建、单元/集成测试、race。
2. 配置与协议层：panel/node 真实配置检查，mTLS/版本协商/迁移红线测试。
3. 进程级升级层：实际运行旧单体、当前 node、旧单体三个阶段，以 aws-cli 对同一数据集做读写验证。
4. 交付层：前端生产构建、Dockerfile/Compose 静态契约及可用时的真实镜像构建。

## 2. 升级基线与数据流

固定升级基线为多节点提交的直接父提交 `5f0be5c`，避免使用会随分支移动的名称。

```text
5f0be5c standalone
  ├─ 创建 SQLite 业务表、凭据、Bucket
  └─ S3 PUT legacy-object
          │ 保留同一 DB + data_root
          ▼
current cmd/node
  ├─ MigrateConfigured: integrity → backup → base migration → integrity
  ├─ MigrateState: 只增 agent_meta/applied_tasks
  ├─ 无面板/未注册仍提供 S3
  └─ GET legacy-object + PUT node-object
          │ 保留同一 DB + data_root
          ▼
5f0be5c standalone rollback
  ├─ 忽略 Agent 新表
  └─ GET legacy-object + GET node-object + 新写入验证
```

进程级脚本使用临时工作树构建旧二进制，所有数据库、配置、日志、对象与密钥材料放在临时目录，退出时清理，不污染用户工作区。

## 3. 关键断言

### 3.1 数据保真

- 旧凭据和 Bucket 在 node/rollback 阶段均可用于 SigV4 请求。
- 旧对象与 node 阶段新对象做字节比较，不只检查 HTTP 状态。
- SQLite 业务行、索引和对象 sidecar 不丢失。

### 3.2 数据库兼容

- node 启动产生至少一个可打开的升级前备份。
- 当前 DB 出现 Agent 新表，但原表行值不变。
- 回滚旧二进制启动成功；它不需要理解 Agent 表，也不得删除它们。

### 3.3 可用性与失败关闭

- 控制面不可用不影响 node S3 监听和请求处理。
- panel 配置检查必须实际加载 32 字节主密钥和中间 CA。
- 删除主密钥或 CA 后 panel 检查失败，防止半启动。

### 3.4 交付契约

- panel 镜像只暴露管理/Agent 端口；node 镜像只暴露 S3 端口。
- Compose healthcheck 必须调用镜像内真实存在的二进制路径。
- 双目标构建共享协议代码，但独立生成 panel/node 可执行文件。

## 4. 自动化形态

- 新增 `scripts/test-upgrade-rollback.sh` 承载真实版本切换演练。
- 复用现有 Go 测试覆盖协议/mTLS/迁移/加密；缺口优先补为包级回归测试。
- 静态交付检查使用 shell 断言，避免必须依赖 Docker 引擎；真实镜像构建作为环境允许时的附加强验证。

## 5. 风险与回滚

- 旧二进制构建依赖 Git 中仍存在 `5f0be5c`；脚本支持 `LEGACY_REF` 覆盖，默认固定该提交。
- aws-cli 测试使用临时端口和临时凭据，避免读取用户的 AWS 配置。
- 任何代码修复都限制在测试暴露的问题范围；若修复导致回归，可回退对应小提交/补丁，不改用户数据。
- Docker 引擎不可用属于外部环境限制，不能用静态检查伪装成真实镜像构建成功。
