# 可观测性：健康检查端点 /healthz /readyz 与 Prometheus /metrics

## Goal

新增 /healthz、/readyz 健康检查端点（容器部署必备）；基于现有 DB 请求/字节统计导出 Prometheus /metrics 指标；访问日志补请求 ID 串联。

## Requirements

- TBD

## Acceptance Criteria

- [ ] TBD

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
