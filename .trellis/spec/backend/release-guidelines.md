# Release Guidelines

## Scenario: Panel/Node GitHub Release

### 1. Scope / Trigger

- Applies whenever `.github/workflows/release.yml`, Docker image publication, or downloadable release archives change.
- The supported deployment boundary is the hard-cutover `panel` + `node` pair. `cmd/natives3bridge` and `ghcr.io/<owner>/natives3-bridge` are legacy-only and must not be republished.

### 2. Signatures

- Workflow inputs: required `tag: string`; optional `source_ref: string`.
- Programs: `./cmd/panel` and `./cmd/node`.
- Docker targets: `panel` and `node`.
- Archive targets per component: `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`.

### 3. Contracts

- Archive name: `natives3-<component>-<version>-<os>-<arch>.tar.gz`.
- Every archive contains the component binary, `README.md`, `configs/<component>.example.yaml`, and `docs/multi-node-operations.md`.
- One `checksums.txt` covers all ten archives.
- Images are `ghcr.io/<lowercase-owner>/natives3-panel` and `.../natives3-node`, each with the release tag and `latest`.
- Images use `linux/amd64,linux/arm64`, component-specific build target and GHA cache scope, `provenance: mode=min`, and explicit `sbom: false`.
- Default workflow permission is `contents: read`; only the image job receives `packages: write`, and only the release job receives `contents: write`.
- The `e2e` job depends on `prepare` and `quality`. Archives and images depend on
  `e2e`, and the release job depends on `e2e`, both archives, and both matrix
  image builds.

### 4. Validation & Error Matrix

| Condition | Required behavior |
| --- | --- |
| Empty or invalid tag | Stop in `prepare`; publish nothing |
| Existing tag resolves to another SHA | Stop in `prepare`; publish nothing |
| UI, vet, test, race, distribution contract, or Panel/Node E2E fails | Do not run archive/image/release publication |
| Either image matrix entry fails | Do not create the GitHub Release |
| Archive upload/download has no matching files | Fail instead of creating a partial Release |

### 5. Good/Base/Bad Cases

- Good: a tag builds ten archives and two multi-architecture image indexes from one fixed commit SHA.
- Base: manual dispatch omits `source_ref`, so the triggering SHA is fixed and used by every downstream checkout.
- Bad: a single job builds `cmd/natives3bridge`, pushes one package, or grants workflow-wide write permissions.

### 6. Tests Required

- `actionlint .github/workflows/release.yml` must pass.
- `bash scripts/test-distribution-contract.sh` must assert component targets, image names, cache scopes, provenance/SBOM, release dependencies, and absence of `cmd/natives3bridge`.
- `bash scripts/test-panel-node-e2e.sh --mode local` must pass with the browser
  gate, and Docker-capable release runners must also pass `--mode docker` before
  archives or images are built.
- Build all ten component/OS/architecture combinations with `CGO_ENABLED=0`; inspect each tar listing for its binary, component config, and operations document; assert ten checksum lines.
- Run the same UI build and Go 1.21 vet/test/race commands used by the quality job.

### 7. Wrong vs Correct

#### Wrong

```yaml
permissions:
  contents: write
  packages: write
jobs:
  build:
    steps:
      - run: go build ./cmd/natives3bridge
```

#### Correct

```yaml
permissions:
  contents: read
jobs:
  images:
    permissions:
      contents: read
      packages: write
    strategy:
      matrix:
        component: [panel, node]
```

## Scenario: Panel/Node E2E Release Gate

### 1. Scope / Trigger

- Trigger: changes to Panel/Node registration, mTLS transport, authoritative
  configuration, S3 authentication/data paths, Panel service-mode routing,
  Docker final targets, or `.github/workflows/release.yml`.
- Goal: prove the shipped processes work together before any archive, image, or
  GitHub Release is published.

### 2. Signatures

- Main command:
  `bash scripts/test-panel-node-e2e.sh --mode local|docker|auto [--timeout N] [--skip-build] [--skip-browser] [--report PATH]`.
- Main environment keys: `E2E_MODE`, `E2E_TIMEOUT`, `E2E_SKIP_BUILD`,
  `E2E_SKIP_BROWSER`, `E2E_REPORT`, `E2E_PANEL_BIN`, `E2E_NODE_BIN`, and
  `E2E_ADMIN_PASSWORD`.
- `E2E_TIMEOUT`/`--timeout` must be a positive integer in the shell harness;
  the Python browser helper must reject a non-positive per-step timeout before
  starting ChromeDriver.
- Browser command:
  `python3 scripts/internal/e2e-browser.py --panel-url URL --expected-node-name NAME [--chromedriver PATH] [--chrome PATH] [--report PATH] [--timeout N]`.
- Browser discovery keys: `CHROMEDRIVER`, runner-provided
  `CHROMEWEBDRIVER` (directory containing `chromedriver`), `CHROME_BIN`, and
  `GOOGLE_CHROME_BIN`.
- Workflow topology: `prepare -> quality -> e2e -> {artifacts, images} -> release`.

### 3. Contracts

- Every run uses `umask 077`, a mode-700 temporary root, random loopback ports,
  separate Panel/Node data roots and SQLite files, temporary PKI, and unique
  Docker names/network. Cleanup removes processes, containers, images, network,
  cookies, configs, databases, and private material on success or failure.
- Panel readiness is `GET /api/admin/auth-settings` with
  `service_mode=panel`; a SPA `200` is not readiness evidence.
- While Node is offline, the test creates a logical node, registration token,
  bucket, credential, and published desired state. The fresh Node must persist
  its key/certificate, establish mTLS, report heartbeat, and reach matching
  non-zero desired/applied versions with `sync_state=synced`.
- S3 requests use curl SigV4, not AWS CLI as the sole dependency. The managed
  bucket rejects direct creation, accepts signed HEAD, and object
  PUT/HEAD/GET/DELETE must match the native file bytes.
- During Panel outage and restart, Node keeps serving the last applied S3
  state. The test logs in again after Panel restart because admin sessions are
  process-memory state, then waits for automatic mTLS reconnect/sync.
- Node restart clears the registration token and must reuse the same private
  key/certificate while preserving S3 bytes.
- An unrelated CA must leave the negative Node without a client certificate,
  produce TLS verification evidence on the Node or Panel side, and leave its S3
  listener alive.
- The browser gate logs in, uses SPA history navigation for the
  `/dashboard -> /nodes` guard, requires a same-origin `/api/admin/nodes`
  request, rejects standalone API paths/non-Panel origins/HTTP `>=400`, and
  removes its profile. Do not combine ChromeDriver `--silent` with
  `--log-level`; current drivers reject that combination.
- Docker mode validates both Compose templates, builds the final `panel` and
  `node` targets, gives the Panel container network alias `panel`, runs with the
  invoking UID/GID so mode-600 bind-mounted configs remain readable, and maps
  only Panel `9001/9443` and Node `9000`. The Docker context excludes local AI
  tool directories and every `node_modules` tree; container lifecycle probes use
  `docker container inspect` so an identically named image cannot be mistaken
  for a running container.
- Persistent CI evidence is only the redacted text report. Browser failure JSON
  (safe URL, page summary, network/status findings) is folded into that report;
  raw cookies, configs, PKI, databases, object roots, and signed URLs are never
  uploaded. The structured `browser-report.json` is the single browser
  diagnostic; `browser.stderr` is a fallback only when that JSON was not
  produced.
- Redaction replaces every generated credential/token value even when a test
  override is short, then scans the finished report and deletes it if any
  known value remains. This prevents a useful-looking failure artifact from
  becoming a secret leak.

### 4. Validation & Error Matrix

| Condition | Required behavior |
| --- | --- |
| Local prerequisite missing | Fail with the missing command; clean temporary state |
| Docker daemon or Compose v2 unavailable in Docker mode | Fail before starting the scenario |
| `CHROMEWEBDRIVER` names a runner directory | Resolve `<dir>/chromedriver` |
| ChromeDriver cannot start or create a session | Return non-zero and retain one redacted browser diagnostic in `E2E_REPORT` |
| Browser helper receives a non-positive timeout | Reject arguments before launching ChromeDriver |
| Panel restarts | Discard the old cookie jar, log in again, then poll reconnect/sync |
| Wrong CA | Persist no client certificate; S3 remains reachable; record TLS verification evidence |
| Any E2E adapter fails in GitHub Actions | Do not run artifacts, images, or release publication |
| Report contains a generated token/password/S3 secret | Delete the report and fail the run |

### 5. Good/Base/Bad Cases

- Good: both adapters complete registration, sync, SigV4 CRUD, Panel outage,
  tokenless Node restart, wrong-CA isolation, and browser API/routing evidence.
- Base: a developer without Docker runs the full local adapter with compatible
  Chrome/ChromeDriver; the release runner additionally runs Docker mode.
- Bad: treating `/healthz` SPA fallback as Panel readiness, reusing a pre-restart
  admin cookie, full-page navigating the SPA route check, omitting Docker's
  `panel` alias, weakening private config modes for container readability,
  allowing tool `node_modules` symlinks into the Docker context, using generic
  `docker inspect` when image and container names collide, or uploading the
  temporary runtime directory.

### 6. Tests Required

- `bash -n scripts/test-panel-node-e2e.sh scripts/test-distribution-contract.sh`.
- `python3 -m py_compile scripts/internal/e2e-browser.py`.
- Two consecutive full local runs, including ChromeDriver and the positive
  same-origin `/api/admin/nodes` assertion.
- Docker mode on a Docker-capable runner, including `docker compose config`,
  both final targets from the repository context, port boundaries,
  reconnect/restart, and wrong-CA checks.
- Failure injection for ChromeDriver startup/session creation: the outer report
  contains exactly one redacted browser JSON diagnostic and no known secret.
- `actionlint .github/workflows/release.yml`, distribution contract, UI build,
  `go vet ./...`, `go test ./...`, and `go test -race ./...`.

### 7. Wrong vs Correct

#### Wrong

```yaml
artifacts:
  needs: [prepare, quality]
```

```python
driver_args = ["--log-level=SEVERE", "--silent"]
```

#### Correct

```yaml
e2e:
  needs: [prepare, quality]
artifacts:
  needs: [prepare, quality, e2e]
```

```python
driver_args = ["--log-level=SEVERE"]
```

```text
browser-report.json + browser.stderr
```

```text
browser-report.json (browser.stderr only when JSON is absent)
```

## Scenario: Docker First-Registration TLS Smoke

### 1. Scope / Trigger

- Trigger: Docker/Compose smoke tests or changes to node first-boot registration.
- Goal: verify that a fresh node can validate the panel server certificate before it has a client certificate, then switch to mTLS for the agent WebSocket.

### 2. Signatures

- Node config: `panel.register_url`, `panel.agent_url`, `panel.ca_file`, `panel.cert_file`, and `panel.key_file`.
- Registration request: `POST panel.register_url` with `{node_id, token, csr}` over server TLS.

### 3. Contracts

- The first-registration HTTP client MUST load the CA certificate from `panel.ca_file`; relying only on the container system trust store is incorrect for a private panel CA.
- After registration, the node MUST persist the issued certificate and CA, then use the same CA for mTLS WebSocket server verification.
- A Docker smoke test MUST run once with the normal config (no hidden trust-store workaround) and fail if registration reports `x509: certificate signed by unknown authority`.

### 4. Validation & Error Matrix

| Condition | Required behavior |
| --- | --- |
| Valid private CA at `panel.ca_file` | Registration succeeds and panel marks node online |
| CA omitted/unreadable | Registration fails closed with a certificate verification error; node continues S3 only |
| Registration succeeds | Client cert/key and CA are persisted under the node data volume |
| Agent cert invalid/revoked | mTLS connection is rejected and node retries without stopping S3 |

### 5. Good/Base/Bad Cases

- Good: a fresh container registers against a panel signed by the test intermediate CA using only `panel.ca_file`.
- Base: setting `SSL_CERT_FILE` can diagnose the problem, but is not an acceptable replacement for loading the configured CA in application code.
- Bad: disabling TLS verification or permanently adding the panel CA through an undocumented image-specific trust-store mutation.

### 6. Tests Required

- Build both Docker targets.
- Start panel and node with an isolated network and generated private CA.
- Assert normal-config first registration succeeds without `SSL_CERT_FILE` or `InsecureSkipVerify`.
- Assert panel node status becomes `online=true` and heartbeat updates.
- Assert node exposes only S3 port 9000 and panel exposes only admin/control ports 9001/9443.

### 7. Wrong vs Correct

#### Wrong

```go
client := &http.Client{Timeout: timeout}
// The private panel CA in panel.ca_file is never loaded.
```

#### Correct

```go
pool := x509.NewCertPool()
pool.AppendCertsFromPEM(os.ReadFile(cfg.CAFile))
client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
```

## Scenario: Recoverable Node Registration And Data-Plane Health

### 1. Scope / Trigger

- Trigger: changes to Panel registration persistence/handlers, Node first-boot
  registration, node lifecycle certificate checks, or Node container healthchecks.

### 2. Signatures

- `POST /register` with `{node_id, token, csr_pem}` and
  `{cert_pem, ca_cert_pem, not_after}` response.
- `node -health -config <node.yaml>` probes `server.s3_addr`.
- Registration token replay columns: `public_key_fingerprint`,
  `issued_cert_pem`, `issued_ca_pem`, and `issued_not_after`.

### 3. Contracts

- Registration identity is `(token hash, node ID, PKIX public-key SHA-256)`.
- Token consumption, replay material, and the single `NodeCert` insert commit in
  one database transaction. Same-token/same-key retries return the stored
  response; changed-key retries receive the same coarse denial as invalid tokens.
- Node retries transport/TLS failures, HTTP 429, and HTTP 5xx with cancellable,
  jittered exponential backoff. Other HTTP 4xx responses stop the current token.
- Only `active` nodes may register or authenticate an agent connection.
  `disabled` preserves unrevoked certificates so reactivation can reconnect;
  `retired` remains rejected.
- `panel.ca_file` is required by Node config validation.
- Docker health runs `node -health`, normalizes wildcard binds to loopback, and
  accepts any syntactically valid HTTP response from the S3 listener, including
  the expected unauthenticated S3 `403`. Panel reachability is not part of Node
  container health.

### 4. Validation & Error Matrix

| Condition | Required behavior |
| --- | --- |
| Network/TLS error, 429, or 5xx | Retry in the same process with the same on-disk private key |
| 400/401/403 or other non-429 4xx | Stop retrying and return a token-free error |
| Used token, same node and public key | Return the stored certificate response; do not insert a second cert |
| Used token, different public key | Return coarse HTTP 401 |
| Node disabled or retired | Registration and mTLS certificate validation fail |
| Disabled node becomes active | Existing unrevoked, unexpired certificate is valid again |
| S3 listener responds 403 | `node -health` exits successfully |
| S3 port is closed | `node -health` exits non-zero |

### 5. Good/Base/Bad Cases

- Good: Panel commits issuance but the response is lost; the Node retries using
  its persisted private key and receives the exact original response.
- Base: Panel is temporarily unavailable while the Node S3 listener remains
  healthy and continues serving its last applied configuration.
- Bad: regenerating the Node key on every attempt, consuming the token before
  certificate persistence, retrying a permanent 401 forever, or using
  `-check-config` as a runtime healthcheck.

### 6. Tests Required

- Panel regression: same-key replay returns byte-equivalent JSON and exactly one
  certificate row; changed-key replay returns 401.
- Node regression: 5xx recovery reaches success in one retry loop; 401 performs
  exactly one attempt; cancellation stops backoff.
- Lifecycle regression: disabled rejects a valid cert and active accepts it again.
- Health regression: wildcard bind probes loopback; HTTP 403 passes; closed port fails.
- Release validation: `go test ./...`, `go vet ./...`, panel/node builds,
  `scripts/test-distribution-contract.sh`, and release integrity smoke.

### 7. Wrong vs Correct

#### Wrong

```go
if err := Register(identity, params); err != nil {
    return // a transient first-boot outage now requires a container restart
}
```

#### Correct

```go
if err := RegisterWithRetry(ctx, identity, params, RegisterRetryOptions{}); err != nil {
    // only cancellation or a permanent rejection reaches this branch
}
```

## Scenario: No-Clone Docker Deployment

### 1. Scope / Trigger

- Applies when Docker Compose templates, panel/node installers, or Docker deployment documentation change.

### 2. Signatures

- Panel: `install-panel.sh --panel-host HOST [--install-dir PATH] [--tag TAG] [--force] [--no-start]`.
- Node: `install-node.sh --panel-url URL --node-id ID --registration-token TOKEN --ca-file PATH [--install-dir PATH] [--tag TAG] [--force] [--no-start]`.

### 3. Contracts

- Templates are `docker-compose.panel.yml` and `docker-compose.node.yml`; the combined Compose file is not a supported deployment entry point.
- Images are pulled from `ghcr.io/rsjwy/natives3-panel` and `ghcr.io/rsjwy/natives3-node`; no local `build:` is present.
- Panel publishes `127.0.0.1:9001` and `9443`; node publishes only `9000`.
- Panel installation generates its SQLite config, 32-byte master key, deployment CA, matching server certificate, bootstrap password, and session secret.
- Node installation requires the public panel CA, node ID, and one-time token; it never receives the CA private key.
- `--no-start` generates and validates files without pulling or starting images; `--force` is the only overwrite path.

### 4. Validation & Error Matrix

| Condition | Required behavior |
| --- | --- |
| Missing required argument in non-interactive mode | Exit with a clear error |
| Invalid host, URL, tag, node ID, CA, or unsafe install path | Exit before creating the deployment |
| Existing install directory without `--force` | Refuse to overwrite |
| Compose v2 unavailable | Exit with a dependency error |
| Valid `--no-start` invocation | Generate config, PKI/data layout, and Compose without pulling images |

### 5. Good/Base/Bad Cases

- Good: download one installer from GitHub Raw, generate an independent host deployment, and pull a pinned release tag.
- Base: use `latest` for quick evaluation, then pin a release tag for repeatable production deployment.
- Bad: clone the repository only to deploy, share one SQLite file between panel and node, disable TLS verification, or expose panel admin directly on all interfaces.

### 6. Tests Required

- `bash -n` both installers and the distribution-contract test.
- Assert both Compose templates contain the correct image, mounts, healthcheck, and only their allowed ports.
- Generate panel and node deployments with `--no-start`; assert required files exist and verify the panel server certificate against the generated public CA.
- Run `docker compose config` when Docker Compose is available.

### 7. Wrong vs Correct

#### Wrong

```yaml
services:
  panel:
    build: .
  node:
    build: .
```

#### Correct

```yaml
services:
  panel:
    image: ghcr.io/rsjwy/natives3-panel:${NATIVES3_TAG:-latest}
```
