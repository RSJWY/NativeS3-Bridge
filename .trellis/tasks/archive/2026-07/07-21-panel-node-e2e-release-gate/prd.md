# Panel Node 端到端发布门

## Goal

把发布验证从“源码构建和包级测试通过”提升为可重复的真实运行验收：在隔离环境中启动 Panel 与 Node，完成注册、mTLS、权威配置下发和 S3 数据面操作，并在发布前由 GitHub Actions 自动阻止回归。

## Background / Confirmed Facts

- `.github/workflows/release.yml` 当前已经构建前端、运行 Go vet/test/race、检查分发契约，并发布两个最终镜像和十个归档；但没有启动 Panel/Node 做跨进程验证。
- `scripts/test-release-integrity.sh` 已有临时 PKI、配置生成和 Panel 启动逻辑，但没有完成 Node 注册、配置同步或 S3 CRUD。
- `scripts/smoke-test-expanded.sh` 已覆盖单独 Node 的对象、metadata、tagging、multipart、presigned URL 和 webhook 冒烟，可复用其 curl/AWS 签名边界。
- Panel Admin API 已提供登录、节点/令牌、credential、bucket、desired-state 和状态接口；Node 配置支持首次注册、mTLS 重连和本地 S3 安全网。
- 历史 Docker 手工验证已证明双镜像可构建、Panel 可启动、Node 可接入；该流程曾暴露首次注册 CA 信任问题，现有代码已修复，但需要自动回归保护。
- GitHub-hosted Ubuntu runner 的官方镜像包含 Google Chrome 和 ChromeDriver；不新增 Playwright/npm 浏览器依赖。

## Requirements

### R1. 隔离运行环境

- 每次运行使用独立临时目录、随机 loopback 端口、临时 SQLite 数据库、对象目录、PKI、Docker network/name。
- 测试结束无论成功失败都停止进程/容器、删除 network 和临时敏感材料；日志不得包含 token、secret、私钥、cookie 或完整 presigned URL。

### R2. Panel → Node 控制面

- 启动 Panel，登录 Admin API，创建逻辑节点、一次性注册令牌、bucket 和 credential，并发布权威期望状态。
- 启动 Node，验证首次 server-TLS 注册、客户端证书持久化、mTLS WebSocket、在线状态、heartbeat 和 desired-state `synced`。
- 验证 Node 只暴露 S3 数据面；Panel 与 Node 的端口边界符合 Compose/镜像契约。

### R3. S3 数据面

- 使用下发的 credential 对 Node 执行 bucket 创建/确认、对象 PUT、GET、HEAD 和 DELETE；断言磁盘对象内容与 HTTP 结果一致。
- 使用 curl SigV4 完成验证，避免把 AWS CLI 作为唯一 CI 依赖；已有扩展 smoke 继续覆盖更深的对象特性。

### R4. 故障与恢复

- Panel 延迟启动或短暂停止时，Node 进程保持 S3 服务；Panel 恢复后 Node 自动注册/重连并重新同步。
- Node 重启后使用持久化身份重新建立 mTLS，不依赖重复消费注册令牌。
- 错误 CA/证书必须 fail-closed，不能伪装成注册成功；Node 的 S3 数据面仍可独立存活。
- 注册响应丢失/重试的幂等契约由现有 `pkg/nodeagent` 回归测试覆盖；发布门至少验证一次真实传输失败后的重连路径，并在报告中区分两类证据。

### R5. 浏览器管理面

- 使用 runner 的 Chrome/ChromeDriver 登录临时 Panel，访问 panel 节点页，断言服务模式路由到 `/nodes`、节点名称可见、页面请求使用 Panel API，且不会请求 standalone dashboard/credentials 路径。
- 浏览器步骤失败时输出脱敏 URL、HTTP 状态和页面文本摘要，不保存 profile、cookie 或密码。

### R6. CI 接入

- 新增独立 `e2e` quality job，依赖 `prepare` 和现有 `quality`，在 `artifacts`、`images`、`release` 前完成。
- 无 Docker 时脚本使用本地二进制；GitHub release runner 中 Docker 可用时额外构建并运行 `panel`/`node` 最终 target。
- 只有 E2E 成功才允许归档、镜像推送和 GitHub Release；支持 `workflow_dispatch`/本地直接运行以便诊断。

## Acceptance Criteria

- [x] `scripts/test-panel-node-e2e.sh` 在无 Docker 的 Linux 环境中完成 R1–R5，并返回非零错误码且输出脱敏证据。
- [x] Docker 模式真实构建两个最终 target，启动隔离 Panel/Node 容器并完成 R2–R4；Compose 静态契约仍通过。
- [x] GitHub Release workflow 的 `e2e` job 在 artifacts/images/release 之前运行，任一场景失败都会阻止发布。
- [x] CI 日志不含注册令牌、S3 secret、私钥、session cookie 或完整签名 URL；失败时保留可定位的非敏感日志摘要。
- [x] 连续运行至少两次通过；`go vet ./...`、`go test ./...`、`go test -race ./...`、前端 build、distribution contract 全部保持绿色。

## Out of Scope

- 不引入 Playwright、Selenium 或新的前端运行时依赖。
- 不做多节点压力/性能测试、跨地域网络模拟或 MySQL/PostgreSQL 全矩阵 CI；这些仍由现有包测试和后续专项覆盖。
- 不改变 Panel/Node 业务协议或数据模型；只有测试暴露真实产品缺陷时，才另开修复提交并更新对应任务范围。

## Key Decisions

- 发布门以现有 tag-triggered Release workflow 为强制入口；本地脚本和手动 workflow_dispatch 用于复现，不额外把完整 Docker E2E 绑定到每次普通提交。
- 运行流程采用“公共断言 + local/docker runtime adapter”，避免维护两套业务场景。
- 浏览器验证使用 ChromeDriver WebDriver HTTP API 与 Python 标准库，runner 自带浏览器，避免 npm 下载和 Playwright 版本漂移。
