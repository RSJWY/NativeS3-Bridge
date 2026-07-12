# Implement: Revocable admin sessions

1. Add session IDs and an active-session registry to `Auth`.
2. Issue and register sessions during login.
3. Verify registry membership in middleware and pass payload via context.
4. Revoke the current ID during logout.
5. Update API test helpers and add logout/restart regression tests.
6. Run `go test ./pkg/webadmin -count=1` and the final repository-wide regression suite.
