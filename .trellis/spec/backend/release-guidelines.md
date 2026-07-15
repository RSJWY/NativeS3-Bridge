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
```
