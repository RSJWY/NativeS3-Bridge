# Docker 启动验证结果

## 执行环境

- Docker Desktop 4.50.0 / Engine 28.5.1
- Docker Compose v2.40.3-desktop.1
- 分支：`07-13-multi-node-mtls-control-plane`
- 测试目录：`/tmp/natives3-docker-smoke-codex`（已删除）

## 通过项

- `docker build --target panel ...` 成功；前端 `npm ci` 和 `npm run build` 成功。
- `docker build --target node ...` 成功。
- panel 容器 Docker health 为 `healthy`，panel UI HTTP 200，登录、节点创建和注册令牌签发均成功。
- node 容器 Docker health 为 `healthy`，S3 `:9000` 可连接并返回预期 `AccessDenied`。
- panel 仅发布 9001/9443；node 仅发布 9000；交叉端口连接失败。
- 临时 workaround 下 node 首次注册成功，panel 节点列表显示 `online=true`，mTLS 心跳持续更新。
- Compose 文件静态解析通过。
- 测试容器、网络、镜像和临时数据均已清理。

## 失败项与缺陷

### P1：node 首次注册没有使用 `panel.ca_file`

使用 node 示例配置直接启动时，node 日志为：

```text
node registration failed; continuing to serve S3 from local DB
error="submit registration: Post \"https://natives3-smoke-panel:9443/register\": tls: failed to verify certificate: x509: certificate signed by unknown authority"
```

原因是 `pkg/nodeagent/register.go` 的首次注册 HTTP client 使用系统 CA，没有加载配置中的 `panel.ca_file`。`ca_file` 只在注册成功后的 mTLS WebSocket 阶段使用。临时设置 `SSL_CERT_FILE=/data/pki/panel-ca.crt` 后注册成功，证明证书、SAN、网络和 panel 注册接口本身可用。

### P2：panel `/healthz` 不是健康端点

`GET /healthz` 返回的是嵌入 SPA HTML（HTTP 200），不是 `ok` 健康响应。当前 Compose healthcheck 使用 `panel -check-config`，因此容器仍会显示 healthy；但外部反向代理或编排系统若按 `/healthz` 探测，会得到误导性成功结果。

## 代码与工作树保护

- 本次未修改业务代码、Dockerfile、Compose 或 README。
- 工作树中的 `README.md` 修改在测试前已存在/由用户另行修改，本任务未触碰；应保留并单独审阅。
