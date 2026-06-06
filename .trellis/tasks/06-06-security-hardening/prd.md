# 安全加固：admin 端口 TLS 支持、匿名下载 per-IP 限流、登录暴力破解节流

## Goal

admin 端口可配置 TLS（或明确反代部署文档）；public-read 桶匿名 GET 加 per-IP 限流防滥用；登录接口加失败节流/锁定防暴力破解。

## Requirements

- TBD

## Acceptance Criteria

- [ ] TBD

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
