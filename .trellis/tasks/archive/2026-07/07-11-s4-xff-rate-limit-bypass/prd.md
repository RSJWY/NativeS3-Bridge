# S4: Fix XFF-spoofable rate limiting and login lockout

## Goal

Use rightmost untrusted hop instead of leftmost XFF when trust_forwarded is on. Update tests.

## Requirements

- When `trust_forwarded` is disabled, both S3 rate limiting and admin login lockout must use `RemoteAddr` and ignore forwarded headers.
- When enabled, `X-Forwarded-For` parsing must use the rightmost non-empty hop so attacker-controlled leftmost prefixes cannot create new limiter identities.
- `X-Real-IP` remains the fallback only when no usable XFF hop exists.
- S3 anonymous rate limiting and webadmin login lockout must implement identical selection semantics.

## Acceptance Criteria

- [x] `client, proxy` resolves to `proxy`, not `client`, when forwarded headers are trusted.
- [x] Changing only a prepended XFF value cannot bypass anonymous rate limiting.
- [x] Changing only a prepended XFF value cannot bypass admin login lockout.
- [x] Existing `trust_forwarded=false` and `X-Real-IP` behavior remains covered.
- [x] Server and webadmin tests pass.

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
