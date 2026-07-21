# Panel Node 端到端发布门

## Goal

建立真实 Panel→Node→S3 发布门，覆盖浏览器、TLS 注册、mTLS 重连、期望状态、S3 CRUD、故障恢复和升级/重启场景，替换“只构建/只检查配置/只看首页 200”的弱验证。

## Requirements

- 使用隔离临时目录、端口、PKI、数据库和对象目录。
- 无 Docker 时可运行本地二进制门；Docker 可用时额外验证两个最终镜像和 Compose。
- 失败证据脱敏且自动清理进程/容器。

## Acceptance Criteria

- [ ] CI 中真实启动 Panel 与 Node，完成注册、上线、配置发布和 S3 bucket/object CRUD。
- [ ] 覆盖 Panel 晚启动、短暂中断、Node 重启、证书/CA 错误和响应丢失。
- [ ] 浏览器断言 Panel 不请求错误 API，发布脚本不再把 SPA 200 当功能通过。

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
