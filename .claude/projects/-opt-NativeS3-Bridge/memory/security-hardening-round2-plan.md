---
name: security-hardening-round2-plan
description: 07-11 安全审查结论与 round2 加固任务树（6 中危 + 7 低危，单租户确认）
metadata:
  type: project
---

2026-07-11 完成全量安全审查（S3 数据面 + webadmin 控制面 + 密钥/日志/配置）。代码基本面扎实：SigV4 常量时间验签、路径穿越已堵、匿名访问紧缩、bcrypt 存储口令、SQL 参数化、日志不泄露密钥。

**单租户确认**：任一 enabled 凭证可读写删所有桶，无按桶隔离。这是刻意的单租户网关设计，不列入加固范围。

**6 个中危子任务**（Trellis `07-11-security-hardening-round2` 下）：
- S1 弱 session_secret 只警告不拒绝（`config.go:203,255`）-> 可离线伪造管理会话
- S2 Docker 示例内置已知 bootstrap 口令（`config.docker.example.yaml`）
- S3 管理端口默认 `0.0.0.0:9001` + 无 TLS 静默明文降级（`server.go:70-75`）
- S4 trust_forwarded 取 XFF 左跳（客户端可控）-> 限流/锁定全绕过（`ratelimit.go:64`、`net.go:9`）
- S5 无状态会话不可撤销（`auth.go:173-215`）
- S6 配额三处绕过：TOCTOU 竞态、信任客户端声明的 size、分片临时数据不限量（`router.go:311,324`）

**7 个低危**（L1-L7，作为清单附在父 PRD，不单独立项）：CSRF 仅靠 SameSite、Cookie Secure 跟随本地 TLS、bootstrap bcrypt hash 进日志、public_healthz 静默覆盖、webhook 无签名、SQLite 文件 0644、备份无上限累积。

关联：[[natives3-bridge-startup]] [[security-hardening-task-plan]]
