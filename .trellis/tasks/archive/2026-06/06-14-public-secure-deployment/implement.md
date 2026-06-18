# Public secure deployment implementation plan

## Scope rule

Do not start code implementation until this planning package is reviewed and the task is explicitly moved from `planning` to `in_progress`.

The work should be delivered in small phases. Each phase must keep `go test ./...`, `go vet ./...`, and `npm run build` green before moving to the next phase.

## Phase 0 - Planning review

- [ ] Review `prd.md`, `design.md`, and this `implement.md`.
- [ ] Confirm the human-verification provider. Recommended default is Cloudflare Turnstile-compatible verification.
- [ ] Confirm whether ops endpoint protection should be implemented in the app, reverse proxy docs only, or both. Recommended default is both: app config gates plus docs.
- [ ] Confirm whether TOTP may add a small dependency or should be implemented with standard library crypto only.
- [ ] After review, run `python3 ./.trellis/scripts/task.py start .trellis/tasks/06-14-public-secure-deployment` or the platform equivalent.

## Phase 1 - Configuration and production checks

Files likely touched:

- `pkg/config/config.go`
- `pkg/config/config_test.go`
- `cmd/natives3bridge/main.go`
- `configs/config.example.yaml`
- `configs/config.docker.example.yaml`
- `README.md`

Implementation checklist:

- [ ] Add config structs for webadmin TOTP and captcha.
- [ ] Add config struct for ops endpoint exposure if app-level gates are chosen.
- [ ] Preserve default local behavior: TOTP disabled, captcha disabled, health behavior compatible.
- [ ] Add validation for impossible states:
  - [ ] captcha enabled requires provider, site key, secret key, and verify URL.
  - [ ] TOTP enabled requires a non-empty valid secret.
  - [ ] metrics token, if configured, must not be the example value.
- [ ] Add production-hardening warnings or `-check-config`.
- [ ] Ensure warnings do not print secrets or full URLs containing tokens.
- [ ] Update config examples with safe comments, not real secrets.

Validation:

```bash
gofmt -w pkg/config cmd/natives3bridge
go test ./pkg/config ./cmd/natives3bridge
go vet ./...
go test ./...
```

Rollback point:

- Revert config struct/check changes before touching authentication if compatibility problems appear.

## Phase 2 - Ops endpoint boundary

Files likely touched:

- `pkg/webadmin/server.go`
- `pkg/webadmin/ops.go`
- `pkg/webadmin/ops_test.go`
- `pkg/webadmin/api_test.go` or route integration tests
- `README.md`

Implementation checklist:

- [ ] Introduce ops exposure settings in `webadmin.NewServer` construction.
- [ ] Keep `/healthz` unauthenticated by default if it remains a pure liveness response.
- [ ] Gate `/readyz` and `/metrics` according to config.
- [ ] If metrics token is chosen:
  - [ ] Accept `Authorization: Bearer <token>`.
  - [ ] Return `401` or `404` consistently when missing/invalid, without leaking expected token.
  - [ ] Keep Prometheus scrape compatibility documented.
- [ ] Preserve existing webadmin guideline behavior for internal deployments through config.
- [ ] Add tests proving public-disabled `/metrics` and `/readyz` are blocked while `/api/admin/*` session behavior is unchanged.
- [ ] Add tests proving enabled ops endpoints still return the existing content type/body contracts.

Validation:

```bash
gofmt -w pkg/webadmin
go test ./pkg/webadmin
go vet ./...
go test ./...
```

Rollback point:

- Restore previous route registration if probes break in local validation.

## Phase 3 - TOTP authentication

Files likely touched:

- `pkg/webadmin/auth.go`
- `pkg/webadmin/auth_test.go`
- `pkg/webadmin/loginlimiter.go`
- `pkg/config/config.go`
- `pkg/webadmin/ui/src/views/Login.vue`
- `pkg/webadmin/ui/src/api/client.ts`
- `README.md`

Implementation checklist:

- [ ] Extend login request with optional `totp_code`.
- [ ] Add TOTP verifier abstraction to keep `Auth.Login` testable.
- [ ] Validate TOTP only after captcha and password succeed, but record login failure for any TOTP failure.
- [ ] Use a small time window, preferably current 30-second step plus +/-1 step.
- [ ] Keep error responses generic.
- [ ] Ensure successful login clears the per-IP limiter exactly once after all required factors pass.
- [ ] Update login UI to show the TOTP input only when configured or when the server indicates it is required.
- [ ] Document config-based reset/recovery path.

Tests:

- [ ] TOTP disabled: password-only login works as before.
- [ ] TOTP enabled and code missing: login fails and records failure.
- [ ] TOTP enabled and code wrong: login fails and records failure.
- [ ] TOTP enabled and code valid: session cookie is issued.
- [ ] Lockout prevents bcrypt/TOTP verification.
- [ ] Success clears failure count after both password and TOTP pass.

Validation:

```bash
gofmt -w pkg/webadmin pkg/config
go test ./pkg/webadmin ./pkg/config
npm run build --prefix pkg/webadmin/ui
go vet ./...
go test ./...
```

Rollback point:

- TOTP can be disabled in config to restore password-only behavior while retaining code.

## Phase 4 - Human verification

Files likely touched:

- `pkg/webadmin/auth.go`
- `pkg/webadmin/auth_test.go`
- `pkg/webadmin/captcha.go` or similar new file
- `pkg/config/config.go`
- `pkg/webadmin/ui/src/views/Login.vue`
- `pkg/webadmin/ui/src/api/client.ts`
- `pkg/webadmin/ui/src/env.d.ts`
- `README.md`

Implementation checklist:

- [ ] Add captcha verifier interface.
- [ ] Implement Turnstile-compatible server-side verification with timeout and context.
- [ ] Do not log provider token, secret key, or provider response body if it may contain sensitive fields.
- [ ] Treat missing token, provider rejection, timeout, or malformed response as login failure when captcha is enabled.
- [ ] Fail closed when enabled.
- [ ] Keep captcha disabled by default for local/LAN deployments.
- [ ] Add frontend widget integration without loading provider script when captcha is disabled.
- [ ] Add a lightweight admin auth settings endpoint if the frontend needs to know whether captcha/TOTP inputs should render.

Tests:

- [ ] Captcha disabled: existing login behavior unchanged.
- [ ] Captcha enabled and token missing: login fails and limiter records failure.
- [ ] Captcha enabled and provider rejects: login fails and limiter records failure.
- [ ] Captcha enabled and provider accepts: password/TOTP path proceeds.
- [ ] Provider timeout fails closed.
- [ ] Provider token and secret never appear in logs or responses.

Validation:

```bash
gofmt -w pkg/webadmin pkg/config
go test ./pkg/webadmin ./pkg/config
npm run build --prefix pkg/webadmin/ui
go vet ./...
go test ./...
```

Rollback point:

- Set captcha disabled in config to restore the previous login flow.

## Phase 5 - S3 direct-link documentation and safety checks

Files likely touched:

- `README.md`
- `scripts/smoke-test.sh` if extending smoke coverage
- possibly `pkg/auth` or `pkg/server` tests only if a real behavior gap is discovered

Implementation checklist:

- [ ] Document private bucket presigned URL flow.
- [ ] Document `public-read` boundaries and when not to use it.
- [ ] Document recommended short TTLs.
- [ ] Document that full presigned URLs should not be logged.
- [ ] Add or update smoke steps for:
  - [ ] signed upload
  - [ ] presigned GET
  - [ ] anonymous public-read GET
  - [ ] anonymous list/write denial
  - [ ] private bucket anonymous denial

Validation:

```bash
go test ./pkg/auth ./pkg/server ./pkg/handlers
go vet ./...
go test ./...
```

Rollback point:

- Documentation-only changes can be reverted independently.

## Phase 6 - Release readiness checks

Run full validation:

```bash
npm run build --prefix pkg/webadmin/ui
go build ./...
go vet ./...
go test ./...
```

Optional manual smoke:

```bash
go build -o natives3bridge ./cmd/natives3bridge
./natives3bridge -config configs/config.yaml
```

Manual checks:

- [ ] Login page renders with only password when TOTP/captcha disabled.
- [ ] Login page renders required TOTP/captcha controls when enabled.
- [ ] Wrong password, wrong TOTP, and failed captcha all count toward lockout.
- [ ] Successful login sets session cookie and reaches dashboard.
- [ ] `/metrics` and `/readyz` are not publicly reachable under the chosen config.
- [ ] S3 signed operations still pass.
- [ ] Presigned GET still passes.
- [ ] Anonymous public-read behavior is unchanged except for documented rate limits.

## Risky areas

- `pkg/webadmin/auth.go`: easy to accidentally clear login failures before all factors pass.
- `pkg/webadmin/server.go`: route ordering matters; SPA fallback must remain last.
- `pkg/server/ratelimit.go`: forwarded header trust is a security boundary.
- `pkg/webadmin/ui/src/views/Login.vue`: avoid loading third-party challenge scripts when disabled.
- `configs/config.docker.example.yaml`: must not keep example production secrets that look deployable.

## Review checklist before completion

- [ ] No secret, session cookie, auth header, captcha token, or presigned query signature is logged.
- [ ] All new security settings have safe local defaults and clear production guidance.
- [ ] Public admin cannot rely on password-only auth when production checklist is followed.
- [ ] Ops endpoints have an explicit boundary.
- [ ] S3 storage still preserves native 1:1 file mapping.
- [ ] No multi-user/RBAC/IAM scope was introduced accidentally.
