# S3 协议补全：DeleteObjects 批量删除与 CopyObject 服务端拷贝

## Goal

补全 aws-cli/SDK 高频依赖的 S3 操作：POST /{bucket}?delete 批量删除、PUT x-amz-copy-source 服务端拷贝，以及 ?location/?versioning 等 SDK 初始化探测子资源的最小响应。

## Requirements

- TBD

## Acceptance Criteria

- [ ] TBD

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
