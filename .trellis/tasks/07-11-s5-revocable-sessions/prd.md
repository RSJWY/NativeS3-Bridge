# S5: Revocable admin sessions

## Goal

Logout/password-change must invalidate existing session tokens. Add session epoch or per-session records.

## Requirements

- Every successful login must create a cryptographically random session identifier and register it as active until its expiry.
- Middleware must require both a valid HMAC/expiry and an active session record.
- Logout must revoke the current session before clearing the cookie; replaying the old cookie must fail immediately.
- Creating a new `Auth` instance (including service restart after password configuration changes) must invalidate tokens issued by the previous instance.
- Session registry access must be concurrency-safe and expired entries must be cleaned without an unbounded background goroutine.

## Acceptance Criteria

- [x] A freshly issued session authorizes protected routes.
- [x] The same cookie returns 401 after logout.
- [x] A token from a prior `Auth` instance returns 401 even with the same signing key.
- [x] Tampered and expired session behavior remains unchanged.
- [x] Webadmin tests pass, including concurrent-safe session paths.

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
