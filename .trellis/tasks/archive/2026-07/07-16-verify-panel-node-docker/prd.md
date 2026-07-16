# 验证 panel node Docker 启动

## Goal

验证当前分支的 panel 与 node Docker 镜像能够独立构建，并按面板/节点分离架构完成一次真实容器启动和控制面接入，给出可复现的通过项与失败项。

## Confirmed Facts

- Docker Desktop 已通过 WSL 集成可用；Docker Engine 28.5.1、Compose 2.40.3。
- `Dockerfile` 提供 `panel`、`node` 两个 target，Compose 示例也声明了两个服务及配置健康检查。
- panel 启动依赖主密钥、在线中间 CA 和 agent 服务端证书；node 首次接入依赖面板创建的逻辑节点与单次注册令牌。
- 仓库已有发布完整性脚本，可复用其临时 PKI 生成方式。
- 本次验证不应使用或覆盖仓库中的真实配置、数据和已有 Docker 资源。

## Requirements

- 使用专用临时目录保存配置、PKI、SQLite 数据库和对象数据。
- 使用唯一的镜像、容器和网络名称，避免影响用户已有容器。
- 分别构建 Dockerfile 的 `panel` 和 `node` target。
- panel 必须使用有效临时 PKI 和 32 字节主密钥启动，并通过容器健康检查。
- 通过 panel 管理 API 创建逻辑节点并取得单次注册令牌，然后启动 node 完成首次注册和 mTLS 控制连接。
- 验证 panel 只承载管理面/控制面端口，node 只承载 S3 数据面端口。
- 收集容器状态、健康检查、关键日志和必要的 HTTP 探测作为证据。
- 无论验证成功或失败，都清理本次创建的容器和网络；测试镜像与临时证据可在汇总后清理。
- 不修改业务代码、Dockerfile、Compose 或 README；发现问题只记录，不在本任务内修复。

## Acceptance Criteria

- [x] `panel` 与 `node` 两个 Docker target 均构建成功。
- [x] panel 容器保持运行并报告 Docker health `healthy`，管理 UI 可访问；发现 `/healthz` 实际回退到 SPA HTML。
- [x] 管理 API 能创建节点并签发注册令牌。
- [ ] node 容器保持运行并报告 Docker health `healthy`，但默认配置首次注册失败；设置临时 `SSL_CERT_FILE` 后注册成功且 panel 记录节点在线。
- [x] node 的 S3 端口可访问，且未发布 panel 管理端口；panel 未发布 S3 端口。
- [ ] 默认路径下容器无错误：node 默认首次注册出现 `x509: certificate signed by unknown authority`；workaround 后无退出/重启并保持 mTLS 心跳。
- [x] 临时容器、网络、测试镜像和运行目录已清理；未修改业务代码、Dockerfile、Compose 或 README。
- [x] 已输出通过项、失败项、限制以及建议修复方向。

## Out of Scope

- 修改实现或部署文档。
- 发布 GHCR 镜像或创建 GitHub Release。
- 压力测试、跨主机网络测试和生产级证书生命周期演练。
- 完整 S3 CRUD、升级回滚和灾难恢复验证。

## Notes

- 这是 PRD-only 的轻量验证任务；执行过程只产生隔离的临时运行资源。
