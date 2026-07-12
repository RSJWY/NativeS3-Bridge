# S2: Remove hardcoded bootstrap password from Docker example

## Goal

Docker example must not ship a known admin password. Operator sets real hash before login works.

## Requirements

- The Docker example must not contain a usable or known `admin_bootstrap_password`.
- The example must leave admin login disabled until the operator supplies a bcrypt `password_hash`.
- Docker deployment documentation must show how to generate a bcrypt hash without placing a plaintext password in the checked-in example.
- Documentation must explain that an empty `password_hash` disables admin login.

## Acceptance Criteria

- [x] `configs/config.docker.example.yaml` has an empty `admin_bootstrap_password`.
- [x] The Docker example comments direct operators to configure `password_hash`.
- [x] The Docker README flow no longer instructs users to set a known bootstrap password.
- [x] Repository tests continue to pass with the safer example.

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
