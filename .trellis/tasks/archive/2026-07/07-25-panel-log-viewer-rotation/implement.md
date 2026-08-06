# Implementation plan: Panel local log viewer and rotation

## Preconditions

- [x] Parent final planning summary is approved.
- [x] Curated context manifests validate.
- [x] Start this child before editing product code.

## Checklist

1. [x] Load `trellis-before-dev` and the logging/webadmin/UI/release specs.
2. [x] Search every duplicated setup value/call site before extraction.
3. [x] Add the shared logging setup/runtime and migrate three command wrappers.
4. [x] Add/adjust setup and rotation tests, including async cleanup polling.
5. [x] Export the reusable logs handler/viewer and keep standalone behavior.
6. [x] Inject Panel ring/file, register authenticated `/api/admin/logs`, and add
       Panel server tests.
7. [x] Allow `/logs` in Panel mode, add navigation/mode-specific copy, and keep
       styles consistent with the existing page.
8. [x] Update README/config/deployment docs and executable specs.
9. [x] Run focused/full validation and review the diff. Commit/archive are the
       next Phase 3.4 steps in the main session.

## Validation

```bash
gofmt -w <changed-go-files>
go test ./pkg/config ./pkg/logging ./pkg/webadmin ./pkg/panel ./cmd/panel ./cmd/node ./cmd/natives3bridge -count=1
npm --prefix pkg/webadmin/ui run build
go vet ./...
go test ./...
go build ./...
bash scripts/test-distribution-contract.sh
```

## Rollback point

Revert the shared setup migration and Panel handler/route together. Existing
log files/configs need no migration or deletion.

## Verification result

- Focused and full Go tests, `go vet ./...`, `go test -race ./...`, and
  `go build ./...` passed.
- The Vue production build, Python compile check, shell syntax checks, and
  distribution contract passed.
- Local Panel/Node E2E passed with `--skip-browser`; the environment has no
  `chromedriver`, so the real browser run remains part of the parent/CI gate.
