# S1: Reject weak session_secret at config load

## Goal

Validate() rejects example/short session_secret like metrics_token does. Update all example configs and tests.

## Requirements

- `Config.Validate` must reject an empty session secret, any known example/placeholder value, and any secret shorter than 32 bytes.
- Validation errors must identify `webadmin.session_secret` and explain that a random secret of at least 32 bytes is required without echoing the configured value.
- Example and checked-in runtime configs must not use a known weak session secret and must remain loadable where they are used by automated tests.
- Tests must cover short, known-weak, and valid session secrets, while unrelated validation tests use a valid test-only secret.
- Documentation examples must not instruct operators to use the rejected example value.

## Acceptance Criteria

- [x] Loading or validating `change-me-32bytes-random` fails.
- [x] Loading or validating a secret shorter than 32 bytes fails.
- [x] A non-placeholder secret of at least 32 bytes passes session-secret validation.
- [x] All config package tests pass.
- [x] Repository search finds no active config/documentation example using `change-me-32bytes-random`.

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
