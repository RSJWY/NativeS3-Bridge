# Security Hardening Round 2

**Status**: planning
**Priority**: P1
**Assignee**: rsjwy
**Date**: 2026-07-11

## Context

Full security audit completed 2026-07-11 covering S3 data plane (SigV4, anonymous access, path traversal, rate limiting, quota), webadmin control plane (login, session, CSRF, TLS, AuthZ, UI), and secrets/logging/config/storage. The codebase is fundamentally sound — SigV4 verification is spec-correct with constant-time comparison, path traversal is blocked, anonymous access is tightly scoped, bcrypt password storage, parameterized SQL, no secret leakage in logs. This task addresses the remaining hardening items.

## Design decision: per-bucket credential scoping (implemented)

NativeS3-Bridge now supports per-bucket credential scoping. The `Credential` model has a `Bucket` column: when non-empty, the credential can only access that single bucket; when empty, it retains full access to all buckets (backward compatible). The Auth middleware in `pkg/server/router.go` enforces the bucket boundary, rejecting requests to other buckets and service-level operations with 403 AccessDenied. The webadmin API and UI allow setting/changing the bucket on each credential. A `--seed-bucket` CLI flag scopes the seed credential. Implemented in commit `ccdc97d` (2026-07-11).

## Medium-severity subtasks

Each gets its own subtask. All require coordinated changes to example configs and tests — none are single-line fixes.

### S1: Reject weak session_secret at config load

**Problem**: `session_secret` set to the example value `change-me-32bytes-random` only produces a warning, not a validation error (`pkg/config/config.go:203,255`). An attacker who knows the example secret can forge admin session tokens offline, bypassing password, TOTP, captcha, and login lockout entirely.

**Fix**: `Validate()` should reject example/known-weak session secrets the same way `metrics_token` is hard-rejected (`config.go:238-240`). Add a minimum-length check (≥32 bytes).

**Breaking change**: All 5 example configs (`configs/config*.yaml`) and `config_test.go` (4 usages) use the example value. Tests that load example configs will fail. Resolution: change example configs to a placeholder that triggers a clear error message instructing the operator to generate a real secret, OR use a non-example test-only secret in test configs. `TestLoadParsesMultipartDurations` currently loads `config.example.yaml` — it must either use a test-specific config or the example config must pass validation.

### S2: Remove hardcoded bootstrap password from Docker example

**Problem**: `configs/config.docker.example.yaml` ships `admin_bootstrap_password: "ChangeMeNow!"` (or similar). Operators who copy the example without changing it get a known admin password.

**Fix**: Remove the bootstrap password from the Docker example. Add a comment explaining how to generate a bcrypt hash and set `password_hash` directly, or pass the bootstrap password via environment variable / docker secret. The example should fail to start (login disabled) until the operator sets a real password, rather than starting with a known one.

### S3: Enforce admin TLS / restrict default admin bind

**Problem**: The admin port defaults to `0.0.0.0:9001` (`config.go:130`) and silently serves plaintext HTTP when TLS is not configured (`pkg/webadmin/server.go:70-75`). The one-time secret key creation response (`POST /api/admin/credentials`) rides this plaintext transport. Only a warning is emitted (`config.go:264-266`).

**Fix options** (pick one or combine):
- Default `admin_addr` to `127.0.0.1:9001` so admin is loopback-only unless explicitly opened.
- Refuse to start when admin addr is public AND admin TLS is off AND no trusted-proxy TLS termination is configured (hard error, not warning).

Either way, the one-time secret key response must never transit plaintext.

### S4: Fix XFF-spoofable rate limiting / login lockout

**Problem**: When `trust_forwarded: true`, `clientIP()` takes the **first** (leftmost) entry of `X-Forwarded-For` (`pkg/server/ratelimit.go:64-72`, `pkg/webadmin/net.go:9-17`). The leftmost XFF value is client-controlled. An attacker rotates XFF values to get a fresh rate-limit/login-lockout bucket for every request, fully bypassing both throttles.

**Fix**: When `trust_forwarded` is on, use the **last** (rightmost) untrusted hop — i.e., the IP of the trusted proxy that set the header — or require a configurable `trusted_proxy_count` and take the hop at that position from the right. The first/leftmost entry is only correct when there is exactly one proxy and the client doesn't spoof XFF, which is the untrusted case.

**Tests**: `ratelimit_test.go:29-41` currently asserts that `trust_forwarded=true` returns the first XFF value — these expectations must be updated to match the new logic.

### S5: Revocable admin sessions

**Problem**: Session tokens are stateless `{exp}+HMAC` (`pkg/webadmin/auth.go:46-48,206-215`). Logout only clears the client cookie (`auth.go:173-189`); a stolen token remains valid for the full TTL (default 12h). Password change and credential rotation do not invalidate existing sessions. There is no per-session ID — every login in the same second yields an identical token.

**Fix**: Add a session token version / nonce to the token payload. Store active sessions or a global invalidation epoch in the DB. On password change, bump the epoch. Optionally add per-session records for individual revocation. This is the largest subtask — consider whether a simpler mitigation (shorter TTL + session_secret rotation procedure) is acceptable for the threat model first.

### S6: Close quota bypass vectors

Three vectors found in `pkg/quota` and middleware:

1. **TOCTOU race**: `Quota` middleware checks `quota.Check(id, size)` (`pkg/server/router.go:311`) before the handler runs, then `commitUsage` runs after (`pkg/handlers/object.go:78`). Two concurrent PUTs each pass the check, both write, both commit — over-quota.
2. **Client-declared size**: `contentLengthForQuota` trusts `x-amz-decoded-content-length` (`router.go:324-333`). A client underreports size to pass the check, then streams a larger body.
3. **Multipart temp data**: Individual parts don't count against quota (by design per spec). But `CompleteMultipartUpload` checks quota once for the total, and if it passes, the merge happens. Between parts upload and complete, temp data can fill the disk with no limit — a disk-exhaustion DoS.

**Fix**:
- Race: use a per-credential lock around check+commit, or an atomic `UPDATE ... WHERE used_bytes + ? <= quota_bytes` in `Commit` and handle the zero-rows-affected case as quota exceeded.
- Client size: after the write completes, commit the **actual** bytes written (already the case for GET via `io.Copy` count) — for PUT, compare actual size against the declared size and reject/rollback if actual > declared.
- Multipart: enforce a configurable cap on total pending multipart bytes per credential, or a global cap on `.multipart/` directory size. At minimum, the multipart GC interval + TTL should be short enough to bound the risk.

## Low-severity checklist (no subtask — fix opportunistically)

| # | Issue | Location |
|---|---|---|
| L1 | CSRF relies solely on SameSite=Lax, no token/Origin check | `pkg/webadmin/auth.go:149` |
| L2 | Cookie `Secure` flag false behind TLS-terminating proxy, no override | `pkg/webadmin/server.go:26-27` |
| L3 | bcrypt hash emitted to logs at bootstrap | `pkg/webadmin/auth.go:60` |
| L4 | `public_healthz: false` silently overridden to true by `applyDefaults` | `pkg/config/config.go:187` |
| L5 | Webhook payloads not signed (recipients can't verify authenticity) | `pkg/hooks/` |
| L6 | SQLite DB file default perms 0644 (world-readable, contains plaintext S3 secret keys) | `pkg/db/` |
| L7 | Pre-upgrade SQLite backups inherit source perms and accumulate without a retention cap | `pkg/db/migrate.go` |

## Acceptance criteria

- [ ] S1: `Validate()` rejects example/short session secrets; all example configs and tests updated.
- [ ] S2: Docker example does not ship a known bootstrap password.
- [ ] S3: Admin port does not expose plaintext login/secret-key on public interfaces by default.
- [ ] S4: `trust_forwarded` uses the correct (rightmost) untrusted hop; tests updated.
- [ ] S5: Admin sessions can be revoked (logout invalidates token, or password change invalidates all sessions).
- [ ] S6: Quota race closed, client-size underreport blocked, multipart disk usage bounded.
- [ ] All existing tests pass; new tests cover each fix.
- [ ] `go build ./...` and `go test ./...` green.
