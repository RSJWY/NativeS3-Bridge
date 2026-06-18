# Public secure deployment design

## 1. Recommended deployment architecture

NativeS3-Bridge should treat the S3 API and the webadmin UI as separate public security boundaries.

Recommended public topology:

```text
Internet
  |
  | HTTPS
  v
Reverse proxy / CDN / WAF
  |-- s3.example.com    -> NativeS3 S3 listener, usually :9000
  |-- admin.example.com -> NativeS3 admin listener, preferably private bind or private network, usually :9001
  |-- ops internal only -> /healthz, /readyz, /metrics, not exposed through public admin.example.com
```

Deployment defaults:

- S3 public access should use `private` buckets and short-lived presigned URLs.
- `public-read` buckets are only for resources that are intended to be anonymously downloadable by anyone with the object URL.
- Webadmin public access must use HTTPS and application-level enhanced authentication. Reverse proxy HTTPS termination is the preferred public deployment shape; direct app TLS remains supported for simpler deployments.
- S3 API and webadmin should use separate hostnames so cookies, rate limits, WAF rules, access logs, and ops endpoints can be managed independently.
- `rate_limit.trust_forwarded` must remain `false` unless the app is only reachable through a trusted proxy that overwrites forwarded headers.

## 2. Current system boundaries

### S3 listener

Code boundary:

- `cmd/natives3bridge/main.go` creates the S3 server.
- `pkg/server/router.go` owns middleware ordering and S3 dispatch.
- `pkg/auth` verifies header SigV4 and query presigned requests.
- `pkg/server/ratelimit.go` applies anonymous `public-read` object rate limiting.
- `pkg/storage` preserves the 1:1 native file mapping.

Public contract:

- Signed requests keep the existing SigV4 behavior.
- Presigned URLs are regular credentialed requests and must reuse the same quota and object handlers.
- Anonymous access is limited to `GET` / `HEAD` object requests for buckets with ACL `public-read`.
- Anonymous list/write/tagging/multipart/subresource requests remain denied.

### Webadmin listener

Code boundary:

- `pkg/webadmin/server.go` registers admin UI, admin APIs, and ops endpoints.
- `pkg/webadmin/auth.go` owns password login, session signing, cookie settings, and login failure lockout.
- `pkg/webadmin/loginlimiter.go` owns in-memory per-IP login failure state.
- `pkg/webadmin/ops.go` owns `/healthz`, `/readyz`, and `/metrics`.
- `pkg/webadmin/ui/src/views/Login.vue` and `pkg/webadmin/ui/src/api/client.ts` own login UI and API payloads.

Current public gap:

- Login only accepts `{"password": string}`.
- Existing per-IP lockout covers password failures but not future TOTP or human-verification failures until those paths are added.
- Ops endpoints are intentionally unauthenticated for probes and Prometheus; this is correct for internal deployment but unsafe to expose casually on a public admin hostname.

## 3. S3 direct-link strategy

### Private bucket direct links

Default user-facing direct links should be presigned URLs:

1. A trusted business service stores objects with a normal enabled S3 credential.
2. The same trusted service generates a query-presigned `GET` URL for a specific object key and a short TTL.
3. The end user receives only the presigned URL.
4. NativeS3 verifies query SigV4 with `auth.HasPresignQuery` / `LocalSigV4Authenticator.Verify`.
5. The object handler streams native bytes without changing the storage layout.

Recommended TTL policy:

- Default: minutes, not days.
- Longer TTL only for low-risk immutable content.
- Never log or persist full presigned URLs with query strings. Existing S3 access logging already logs `r.URL.Path`, which avoids persisting `X-Amz-Signature`.

### Public-read buckets

Use `public-read` only when all objects in that bucket are meant to be anonymously retrievable if the key is known.

Preserve the existing frozen matrix:

- Anonymous `GET` / `HEAD` object: allowed only for `public-read`.
- Anonymous list bucket: denied.
- Anonymous write/delete/tagging/multipart: denied.
- Signed requests: unchanged.

Existing anonymous per-IP rate limiting remains useful but should not be treated as abuse protection by itself. Public deployments should also use reverse proxy or CDN rate limits for public-read traffic.

## 4. Webadmin authentication enhancement

The MVP keeps the single-user admin model and adds defense in depth:

- Existing password: bcrypt hash from `webadmin.password_hash`.
- New TOTP: second factor required when configured/enabled.
- New human verification: public login challenge checked server-side before accepting the login.
- Existing login limiter: all failed login paths record failure for the same source IP.

### Login flow

Target `POST /api/admin/login` request shape:

```json
{
  "password": "admin-password",
  "totp_code": "123456",
  "captcha_token": "provider-token"
}
```

Server-side order:

1. Resolve client IP with the same trusted-forwarded policy already used by login lockout.
2. Reject immediately with `429` when locked.
3. Decode JSON with unknown-field rejection preserved.
4. If human verification is enabled, validate `captcha_token` server-side.
5. Validate bcrypt password.
6. If TOTP is enabled, validate `totp_code`.
7. On any failure in steps 4-6, record login failure and return a generic unauthorized or challenge error without revealing which factor was wrong.
8. On success, record login success, issue the existing signed session cookie, and return the current success payload plus any non-sensitive auth state needed by the UI.

Response guidance:

- Keep `401` for invalid credentials or invalid TOTP.
- Keep `429` with `Retry-After` for lockout.
- Use `400` for malformed JSON or missing required fields.
- Avoid separate messages like "password correct but TOTP wrong"; public UI may show a generic login failure.

### TOTP state and operations

TOTP should be application configuration backed, not multi-user database state, because the admin model remains single-user.

Recommended config additions under `webadmin`:

```yaml
totp:
  enabled: false
  issuer: "NativeS3-Bridge"
  account: "admin"
  secret: ""
  recovery_codes_hash: []
```

Operational paths:

- Initialization: admin generates a TOTP secret out of band or via a one-time CLI/admin endpoint, stores it in config, scans the otpauth URI, then restarts or reloads config depending on final implementation.
- Normal login: login requires password + TOTP code when `totp.enabled=true`.
- Disable/reset: operator edits config to set `enabled=false` or replaces `secret`; this is acceptable for the MVP because there is one admin and config is already a deployment artifact.
- Recovery: optional recovery codes can be added if implementation scope allows; otherwise document the config-based reset path clearly.

Implementation options:

- Prefer a small, well-known TOTP library only if the dependency is accepted during implementation planning.
- If no new dependency is desired, implement RFC 6238 validation narrowly with HMAC-SHA1/SHA256, 30-second step, 6 digits, and a +/-1 step window, with focused tests against known vectors.

### Human verification

Recommended MVP provider: Cloudflare Turnstile-compatible server-side verification.

Rationale:

- It keeps bot defense outside the password/TOTP code path.
- Server-side verification is a simple HTTP POST from Go.
- It avoids storing additional user state.
- It can be disabled for private LAN deployments.

Recommended config additions:

```yaml
captcha:
  enabled: false
  provider: "turnstile"
  site_key: ""
  secret_key: ""
  verify_url: "https://challenges.cloudflare.com/turnstile/v0/siteverify"
  timeout: "3s"
```

Contracts:

- When disabled, login works with password/TOTP only.
- When enabled, the frontend renders the configured provider widget and sends `captcha_token`.
- The backend verifies the token before password comparison.
- Failed, missing, timed-out, or malformed captcha verification records a login failure.
- Provider network failure should fail closed for public mode. Private deployments can disable captcha explicitly.

The design intentionally keeps the provider behind config and a small verifier interface so hCaptcha/reCAPTCHA or an internal challenge can be added later without changing the login handler contract.

## 5. Ops endpoint exposure strategy

The existing webadmin guideline requires unauthenticated ops endpoints for probes:

- `GET /healthz`
- `GET /readyz`
- `GET /metrics`

For public deployment, do not wrap these endpoints in the admin session by default because that would break Kubernetes probes and Prometheus scrapers. Instead add an explicit exposure boundary.

Recommended MVP:

```yaml
ops:
  public_healthz: true
  public_readyz: false
  public_metrics: false
  metrics_token: ""
```

Route behavior:

- `/healthz` can remain unauthenticated because it only returns `ok`.
- `/readyz` should be internal-only by default because it reveals service readiness.
- `/metrics` should be internal-only by default because aggregate usage metrics are operational data.
- If `metrics_token` is set, allow `/metrics` only with `Authorization: Bearer <token>` or another explicit header. Keep Prometheus compatibility documented.

Alternative deployment-only control:

- Keep app routes unchanged and require reverse proxy rules that block `/readyz` and `/metrics` on the public admin hostname.
- This is acceptable only if the production checklist makes it explicit and testable.

Preferred implementation direction:

- Add app-level config gates so the binary has safe defaults even when the reverse proxy is misconfigured.
- Preserve the existing internal-probe behavior by allowing deployments to enable unauthenticated ops endpoints intentionally.

## 6. Production configuration checks

Add a startup warning path and a standalone check mode if scope allows.

Recommended command:

```bash
natives3bridge -check-config -config configs/config.yaml
```

Checks:

- `webadmin.session_secret` is not empty and is not `change-me-32bytes-random`.
- `webadmin.admin_bootstrap_password` is empty after first boot.
- `webadmin.password_hash` is configured for production.
- Public admin exposure has HTTPS through `admin_tls` or documented reverse proxy.
- `server.admin_addr` is not public unless protected by TLS/reverse proxy.
- `rate_limit.trust_forwarded=true` is only used behind a trusted proxy.
- Captcha enabled has `site_key` and `secret_key`.
- TOTP enabled has a valid secret.
- `/metrics` and `/readyz` are not public unless intentionally configured.

Startup behavior:

- Config validation should still reject impossible states such as TLS enabled without cert/key.
- Production-hardening checks can start as warnings to avoid breaking local deployments.
- A later `--production` or `deployment.mode: production` switch can make warnings fatal.

## 7. Documentation additions

Add a "公网安全部署" section to README with:

- Recommended domain split: `s3.example.com` and `admin.example.com`.
- Reverse proxy example for HTTPS termination.
- Warning not to expose the raw admin listener directly.
- Presigned URL flow for private buckets.
- `public-read` boundaries and anonymous limit behavior.
- `trust_forwarded` rule.
- Ops endpoint exposure rules.
- Production checklist.

Production checklist:

- HTTPS terminates at app or trusted reverse proxy.
- Admin password is strong and stored only as bcrypt hash.
- `admin_bootstrap_password` is empty.
- `session_secret` is random and not the example value.
- TOTP enabled for public admin.
- Captcha enabled for public admin, or an explicit reason is documented.
- `/readyz` and `/metrics` are internal-only or token-protected.
- `trust_forwarded` is false unless the proxy overwrites forwarding headers.
- Logs do not include full presigned URLs, auth headers, cookies, or object payloads.
- Public-read buckets contain only intentionally public objects.

## 8. Compatibility and rollback

Compatibility:

- Existing local/LAN deployments continue to work with captcha and TOTP disabled.
- Existing session cookie format can remain unchanged unless TOTP state must be embedded; prefer not to embed auth method details in the cookie.
- Existing admin API sessions remain protected by HMAC session cookies.
- Existing public-read behavior and SigV4 behavior must not change.

Rollback:

- Disable captcha with `webadmin.captcha.enabled=false`.
- Disable TOTP with `webadmin.totp.enabled=false` if admin access is blocked.
- Reopen internal probes by restoring ops config or reverse proxy rules.
- Revert config warnings/check mode without touching storage data.

## 9. Explicit non-goals

This task does not implement:

- Multi-user admin accounts.
- RBAC, OIDC, SSO, or IAM-style policies.
- Per-object authorization rules beyond existing bucket ACL and presigned URLs.
- CDN cache invalidation or anti-hotlinking.
- Distributed login limiter or distributed S3 rate limiting.
- Object versioning, replication, or clustered storage.
