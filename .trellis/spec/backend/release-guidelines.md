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
- The release job depends on both archives and both matrix image builds. Archives and images depend on the quality gate.

### 4. Validation & Error Matrix

| Condition | Required behavior |
| --- | --- |
| Empty or invalid tag | Stop in `prepare`; publish nothing |
| Existing tag resolves to another SHA | Stop in `prepare`; publish nothing |
| UI, vet, test, race, or distribution contract fails | Do not run archive/image/release publication |
| Either image matrix entry fails | Do not create the GitHub Release |
| Archive upload/download has no matching files | Fail instead of creating a partial Release |

### 5. Good/Base/Bad Cases

- Good: a tag builds ten archives and two multi-architecture image indexes from one fixed commit SHA.
- Base: manual dispatch omits `source_ref`, so the triggering SHA is fixed and used by every downstream checkout.
- Bad: a single job builds `cmd/natives3bridge`, pushes one package, or grants workflow-wide write permissions.

### 6. Tests Required

- `actionlint .github/workflows/release.yml` must pass.
- `bash scripts/test-distribution-contract.sh` must assert component targets, image names, cache scopes, provenance/SBOM, release dependencies, and absence of `cmd/natives3bridge`.
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
