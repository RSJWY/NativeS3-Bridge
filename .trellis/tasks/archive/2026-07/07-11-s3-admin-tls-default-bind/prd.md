# S3: Enforce admin TLS / restrict default admin bind

## Goal

Admin port should not expose plaintext login/secret-key on public interfaces by default.

## Requirements

- The default admin listener must bind to `127.0.0.1:9001`.
- Configuration validation must reject a public admin listener unless effective admin TLS is enabled.
- Loopback plaintext remains supported for local administration and trusted local reverse proxies.
- Checked-in examples and documentation must default to loopback and explain that public exposure requires admin TLS.
- S3 listener behavior must remain unchanged.

## Acceptance Criteria

- [x] An omitted `server.admin_addr` defaults to `127.0.0.1:9001`.
- [x] `0.0.0.0:9001` with admin TLS disabled fails validation.
- [x] Loopback without TLS and public bind with valid TLS settings pass validation.
- [x] Example configs do not expose plaintext admin traffic publicly.
- [x] Config and webadmin tests pass.

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
