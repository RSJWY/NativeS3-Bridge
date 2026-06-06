# 对象写入原子性与完整性校验（临时文件+rename，Content-MD5/ETag 校验）

## Goal

本地文件后端 PUT 落盘改为写临时文件再 rename，避免并发/中断产生半截对象；PUT 支持 Content-MD5 校验并在完成后比对 ETag，静默损坏可见。

## Requirements

- TBD

## Acceptance Criteria

- [ ] TBD

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
