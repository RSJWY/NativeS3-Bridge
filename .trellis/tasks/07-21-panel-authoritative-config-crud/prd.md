# Panel 权威配置管理闭环

## Goal

补齐 Panel 作为 credentials、buckets/ACL、webhooks 和匿名限流唯一权威所需的 API、UI、校验和下发语义，消除全量期望状态删除本地配置但 Panel 无法管理这些配置的断层。

## Requirements

- 增加 node 作用域 bucket、credential 更新/停用/删除、webhook 和 rate-limit API/UI。
- 所有写操作审计、鉴权，并在显式发布后进入版本化 desired state。
- 防止 credential 绑定不存在 bucket；删除 bucket 时处理绑定 credential 与磁盘对象安全边界。

## Acceptance Criteria

- [ ] Panel UI 可完成上述配置的完整生命周期并安全发布到在线/离线 node。
- [ ] 全量下发不会意外删除管理员无法在 Panel 中表达的有效业务配置。
- [ ] secret 仍只在创建/轮换时返回一次。

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
