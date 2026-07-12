# Webadmin Backend Guidelines

> Single-password admin API, credential management, dashboard data, and embedded Vue SPA serving contracts.

---

## Scenario: Webadmin API And Embedded SPA

### 1. Scope / Trigger

- Trigger: any change to `pkg/webadmin`, admin routes in `cmd/natives3bridge`, `config.WebAdminConfig`, `server.admin_addr`, frontend embed wiring, or admin-facing credential/dashboard JSON payloads.
- Goal: keep the admin UI isolated on the admin listener, protect API routes with the single-password session, and preserve the one-time credential secret contract.

### 2. Signatures

- `func BootstrapPasswordHash(cfg *config.WebAdminConfig) error`
- `func NewAuth(cfg config.WebAdminConfig, secureCookie ...bool) *Auth`
- `func (a *Auth) Login(w http.ResponseWriter, r *http.Request)`
- `func (a *Auth) Logout(w http.ResponseWriter, r *http.Request)`
- `func (a *Auth) Middleware(next http.Handler) http.Handler`
- `func NewAPI(gdb *gorm.DB, invalidator interface{ Invalidate(string) }, buckets *storage.BucketStore) *API`
- `func NewServer(serverCfg config.ServerConfig, webCfg config.WebAdminConfig, gdb *gorm.DB, credentialStore *auth.CredentialStore, bucketStore *storage.BucketStore) (*Server, error)`
- `func (c *Config) Validate() error`
- `func (c *Config) ProductionWarnings() []string`
- Embedded static bundle: `pkg/webadmin/ui.DistFS` from `//go:embed all:dist`.

### 3. Contracts

- Admin API routes are served only by the admin server on `server.admin_addr`, not by the S3 router.
- `POST /api/admin/login` accepts `{"password": string}` and sets `natives3_admin_session` as an HTTP-only signed cookie on success.
- All `/api/admin/*` routes except login must be wrapped by `Auth.Middleware` and return JSON `401` on missing, expired, or invalid sessions.
- `POST /api/admin/logout` clears the session cookie.
- `GET /api/admin/credentials` returns `id`, `access_key`, `name`, `status`, `quota_bytes`, `used_bytes`, and `created_at`; it must never return `secret_key`.
- `POST /api/admin/credentials` accepts `name` and `quota_bytes`, creates an enabled credential, and returns `secret_key` exactly in that create response.
- `PATCH /api/admin/credentials/{id}` accepts optional `name`, `status`, and `quota_bytes`; update must invalidate the affected `CredentialStore` access key.
- `DELETE /api/admin/credentials/{id}` deletes the credential and invalidates the affected access key.
- Dashboard APIs return real database data from `credentials` and `request_stats`:
  - `/api/admin/dashboard/summary`: `total_credentials`, `total_quota_bytes`, `total_used_bytes`.
  - `/api/admin/dashboard/usage-ranking`: access key/name usage rows ordered by `used_bytes`.
  - `/api/admin/dashboard/request-trend?days=N`: UTC day buckets with put/get/delete and byte counters.
- Bucket admin APIs are session-protected under `/api/admin/buckets*` and must use the shared `storage.BucketStore`, not S3 ACL routes:
  - `GET /api/admin/buckets` returns `[{name, acl, created_at}]` from `BucketStore.List`.
  - `POST /api/admin/buckets` accepts `{"name":"<bucket>"}`, calls `BucketStore.Create`, and returns the created bucket with default ACL `private`.
  - `DELETE /api/admin/buckets/{name}` only deletes buckets that contain no objects and have no credentials bound to their name.
- Non-empty credential `bucket` values on create/update must reference an existing bucket; historical dangling bindings remain readable but cannot be newly written.
  - `PUT /api/admin/buckets/{name}/acl` accepts `{"acl":"private"|"public-read"}`, calls `BucketStore.SetACL`, and returns the updated bucket.
- If effective admin TLS is disabled, startup must log `admin UI served over plain HTTP; enable TLS for production`, and README must warn that the admin UI is plain HTTP.
- A public `server.admin_addr` without effective admin TLS is allowed for container port publishing and trusted reverse-proxy deployments, but `ProductionWarnings` must emit `server.admin_addr listens publicly without admin TLS`; deployment controls must restrict or encrypt the host-side entry point.
- SPA fallback must serve `index.html` for non-API deep links and return JSON errors for unknown `/api/*` paths.

### 4. Validation & Error Matrix

- Missing or invalid session on protected route -> HTTP `401` JSON `{"error":"unauthorized"}`.
- Wrong login password -> fixed delay, HTTP `401` JSON `{"error":"invalid password"}`.
- Unsupported HTTP method -> HTTP `405` JSON error.
- Malformed JSON or unknown request fields -> HTTP `400` JSON error.
- Negative `quota_bytes` -> HTTP `400` JSON error.
- Credential `name` longer than 128 characters -> HTTP `400` JSON error.
- Invalid status value outside `enabled` / `disabled` -> HTTP `400` JSON error.
- Unknown credential id/access key -> HTTP `404` JSON error.
- Invalid bucket name -> HTTP `400` JSON `{"error":"invalid bucket name"}`.
- Invalid bucket ACL outside `private` / `public-read` -> HTTP `400` JSON error.
- Missing bucket on delete or ACL update -> HTTP `404` JSON `{"error":"bucket not found"}`.
- Non-empty bucket delete -> HTTP `409` JSON `{"error":"bucket not empty"}`.
- Bucket with bound credentials delete -> HTTP `409` JSON `{"error":"bucket has bound credentials"}`.
- Credential references a missing bucket -> HTTP `400` JSON `{"error":"bucket does not exist"}`.
- Public `server.admin_addr` with admin TLS disabled -> configuration remains valid and emits a production warning rather than preventing startup.
- Enabled effective admin TLS without both certificate and key paths -> configuration error naming `server.admin_tls`.
- DB failures -> HTTP `500` JSON error without leaking SQL/internal details.

### 5. Good/Base/Bad Cases

- Good: login with the configured password, create a credential, use the returned secret with real `aws-cli`, disable the credential via admin API, and observe S3 requests rejected immediately due to cache invalidation.
- Good: list credentials after create and verify the response includes `access_key` but not `secret_key`.
- Base: moving the compiled binary to a directory without source still serves the embedded Vue app and admin JSON APIs.
- Base: creating a bucket through `/api/admin/buckets` creates the native bucket directory and DB row with ACL `private`; switching to `public-read` is immediately visible in a subsequent admin list response.
- Base: Docker listens on `0.0.0.0:9001` inside the container while Compose publishes `127.0.0.1:9001:9001` on the host; config validation passes and reports the plaintext-listener warning.
- Bad: exposing admin routes on the S3 listener, serving `/api/admin/credentials` without a session, or returning `secret_key` from list/update/delete.
- Bad: updating credential status/quota/name without invalidating `CredentialStore`, because S3 auth can keep stale status/quota.
- Bad: implementing bucket ACL changes as S3 `PutBucketAcl` or any unauthenticated admin endpoint; bucket ACL management belongs only to the session-protected webadmin API.
- Bad: rejecting every plaintext `0.0.0.0` admin listener at config load, because container networking requires a non-loopback listener even when the host publish or trusted proxy is restricted.

### 6. Tests Required

- `npm ci && npm run build` in `pkg/webadmin/ui` before Go build so `dist/` exists for embed.
- `go build -o natives3bridge ./cmd/natives3bridge`, `go vet ./...`, and `go test ./...` after admin backend changes.
- Curl smoke: unauthenticated `/api/admin/credentials` returns `401`; wrong password returns `401`; correct password sets cookie; create returns `secret_key`; list does not return `secret_key`.
- Bucket API tests must cover authenticated list/create, invalid bucket name `400`, ACL update and list reflection, invalid ACL `400`, empty delete `200`, non-empty delete `409`, missing bucket `404`, and unauthenticated `GET`/`POST`/`DELETE`/`PUT /acl` all returning `401`.
- Bucket API smoke: login, create bucket, set `public-read`, confirm `GET /api/admin/buckets` shows `public-read`, confirm no-cookie admin request returns `401`, upload a signed object, verify anonymous GET succeeds for `public-read`, switch back to `private`, and verify anonymous GET returns `403`.
- Real `aws-cli` smoke: UI-created credential can upload/download; after admin disable, the same credential is rejected.
- Browser smoke: login page renders; successful login reaches dashboard; three ECharts canvases render; credentials page renders.
- Single-binary smoke: copy `natives3bridge` to a no-source temp directory and verify the embedded admin UI is still served.
- Config regression: `0.0.0.0:9001` without admin TLS passes `Validate` and emits the public-listener warning; loopback plaintext and public TLS do not emit that warning.

### 7. Wrong vs Correct

Wrong:

```go
mux.HandleFunc("/api/admin/credentials", api.Credentials) // no session middleware
```

Correct:

```go
mux.Handle("/api/admin/credentials", auth.Middleware(http.HandlerFunc(api.Credentials)))
```

Wrong:

```go
return fmt.Errorf("server.admin_addr must not listen publicly without server.admin_tls enabled")
```

Correct:

```go
warnings = append(warnings, "server.admin_addr listens publicly without admin TLS; use a trusted HTTPS reverse proxy or enable server.admin_tls")
```

Wrong:

```go
mux.HandleFunc("/api/admin/buckets", api.Buckets) // no session middleware
```

Correct:

```go
mux.Handle("/api/admin/buckets", auth.Middleware(http.HandlerFunc(api.Buckets)))
mux.Handle("/api/admin/buckets/", auth.Middleware(http.HandlerFunc(api.BucketByName)))
```

Wrong:

```go
writeJSON(w, http.StatusOK, cred) // includes SecretKey from db.Credential
```

Correct:

```go
writeJSON(w, http.StatusOK, credentialToResponse(cred)) // no secret_key field
```

---

## Scenario: Operational Health And Metrics Endpoints

### 1. Scope / Trigger


- Trigger: any change to `pkg/webadmin/ops.go`, admin listener route registration, health/readiness endpoints, or Prometheus metrics output.
- Goal: expose container/probe and Prometheus endpoints on the admin listener without weakening `/api/admin/*` session protection or leaking sensitive data.

### 2. Signatures


- `func NewOpsHandler(gdb *gorm.DB) *OpsHandler`
- `func (o *OpsHandler) Healthz(w http.ResponseWriter, r *http.Request)`
- `func (o *OpsHandler) Readyz(w http.ResponseWriter, r *http.Request)`
- `func (o *OpsHandler) Metrics(w http.ResponseWriter, r *http.Request)`
- Admin listener routes: `GET /healthz`, `GET /readyz`, `GET /metrics`.

### 3. Contracts


- Ops endpoints are registered on the admin listener before the SPA fallback and outside `Auth.Middleware`; they must not be mounted on the S3 listener.
- `/healthz` is a liveness probe only: it returns HTTP `200`, `Content-Type: text/plain; charset=utf-8`, and body `ok`; it must not ping or query the database.
- `/readyz` is a readiness probe: it pings the underlying `gorm.DB` SQL handle and returns `200 ready` when reachable or `503 database unavailable` when the handle is nil, closed, or cannot ping.
- `/metrics` returns HTTP `200`, `Content-Type: text/plain; version=0.0.4; charset=utf-8`, and Prometheus text exposition format.
- Metrics are derived from persistent aggregate data only: `request_stats` for S3 operation/byte counters, `credentials` for credential count and quota/used totals, `buckets` for bucket count, and DB reachability for `natives3_database_up`.
- Metrics must not include credential secrets, admin session values, request payload data, object names, bucket names, or access keys.
- Current metric names are:
  - `natives3_requests_total{op="put|get|delete"}`
  - `natives3_bytes_in_total`
  - `natives3_bytes_out_total`
  - `natives3_credentials`
  - `natives3_buckets`
  - `natives3_quota_bytes_total`
  - `natives3_used_bytes_total`
  - `natives3_database_up`

### 4. Validation & Error Matrix


- Non-`GET` request to an ops endpoint -> HTTP `405`, `Allow: GET`, `Content-Type: text/plain; charset=utf-8`, body `method not allowed`.
- Nil `gorm.DB`, unavailable SQL handle, or failed `Ping` on `/readyz` -> HTTP `503` with body `database unavailable`.
- Nil/closed DB or aggregate query failure on `/metrics` -> HTTP `200` and `natives3_database_up 0`; do not panic or return SQL/internal error details.
- Missing admin session on `/metrics`, `/healthz`, or `/readyz` -> still served normally; missing admin session on `/api/admin/*` -> HTTP `401` JSON `{"error":"unauthorized"}`.

### 5. Good/Base/Bad Cases


- Good: `/metrics` can be scraped without a cookie and `/api/admin/credentials` still returns `401` without a valid admin session.
- Good: closing the test SQL handle makes `/readyz` return `503` and `/metrics` include `natives3_database_up 0` without panicking.
- Base: an empty database returns zero-valued counters and `natives3_database_up 1` when the DB can ping.
- Bad: wrapping ops endpoints with admin auth, because Kubernetes probes and Prometheus scrapers may not have an admin cookie.
- Bad: registering ops endpoints after the SPA fallback, because `/metrics` or `/readyz` could incorrectly serve `index.html`.
- Bad: emitting labels containing bucket names, access keys, object keys, or other high-cardinality/sensitive values.

### 6. Tests Required


- Unit tests for `/healthz` response code, content type, body, and no DB dependency.
- Unit tests for `/readyz` success and nil/closed DB failure behavior.
- Unit tests for `/metrics` content type, metric names, operation labels, non-zero aggregate values from `credentials`, `request_stats`, and `buckets`, plus DB-down degradation.
- Route integration test proving unauthenticated ops endpoints are reachable while `/api/admin/*` remains session-protected.
- Run `gofmt`, `go build ./...`, `go vet ./...`, and `go test ./...` after changes.

### 7. Wrong vs Correct

Wrong:

```go
mux.Handle("/metrics", auth.Middleware(http.HandlerFunc(ops.Metrics))) // probe/scraper needs admin cookie
```

Correct:

```go
mux.HandleFunc("/metrics", ops.Metrics)
mux.Handle("/api/admin/credentials", auth.Middleware(http.HandlerFunc(api.Credentials)))
```

Wrong:

```go
mux.HandleFunc("/", spa.Serve)
mux.HandleFunc("/readyz", ops.Readyz) // unreachable because fallback was registered first
```

Correct:

```go
mux.HandleFunc("/readyz", ops.Readyz)
mux.HandleFunc("/", spa.Serve)
```

Wrong:

```go
fmt.Fprintf(w, "natives3_bucket_used_bytes{bucket=%q} %d\n", bucket.Name, used)
```

Correct:

```go
fmt.Fprintf(w, "natives3_buckets %d\n", bucketCount)
```

---

## Common Mistakes

- Do not rely on `configs/config.sqlite.yaml` for admin login smoke unless it has a password configured. Use a temporary local config with `admin_bootstrap_password` for validation.
- Do not commit built `dist/assets/*`; keep only `.gitkeep` tracked. Build artifacts are regenerated before embedding and are ignored by `.gitignore`.
- Do not add sensitive or high-cardinality labels to `/metrics`; keep labels limited to bounded operational dimensions such as S3 operation names.

## Scenario: Logs And Storage Reconcile Admin APIs

### 1. Scope / Trigger
- Trigger: changes to `/api/admin/logs` or `/api/admin/buckets/{name}/reconcile` and their UI clients.

### 2. Signatures
- `GET /api/admin/logs?limit=&level=&q=`.
- `POST /api/admin/buckets/{name}/reconcile`, body `{"apply": false|true}`.

### 3. Contracts
- Both routes exist only behind admin session middleware, never public ops or S3 routers.
- Logs return `source`, `file_enabled`, `limit`, `entries`, and optional `warning`.
- Reconcile returns disk counts, at most 50 orphan samples, bound credential identifiers/diffs, and actual apply counts; never `secret_key`.
- UI performs dry-run first and confirms apply; apply always rescans server-side.

### 4. Validation & Error Matrix
- Missing session -> 401; wrong method -> 405; invalid body/name -> 400; missing bucket -> 404; internal scan/delete/DB error -> sanitized 500.

### 5. Good/Base/Bad Cases
- Good: file-read failure visibly falls back to ring; reconcile confirmation states sidecar and used-byte effects.
- Base: empty logs and no-difference reports render useful empty states.
- Bad: client log paths, applying stale browser totals, returning secrets, or mounting routes publicly.

### 6. Tests Required
- Backend: auth, methods, filters, fallback, dry-run immutability, apply side effects, invalidation, and sanitized errors.
- Frontend: type-check/Vite build and browser checks for `/logs`, report, and confirmation.

### 7. Wrong vs Correct
- Wrong: trust dry-run bytes sent back by the browser.
- Correct: accept only `apply`, rescan configured storage, and derive mutations server-side.
