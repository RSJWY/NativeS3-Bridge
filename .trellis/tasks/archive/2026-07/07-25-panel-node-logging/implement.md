# Implementation plan: Panel/Node logging

## Phase gate

- [x] User reviews and explicitly approves the final planning summary.
- [x] Validate parent and child artifacts/manifests.
- [x] Do not start the parent as an implementation target. Start
      `07-25-panel-log-viewer-rotation` first; start
      `07-25-node-log-query` after its own artifacts are reviewed (it may be
      implemented after or alongside the first child, but final integration
      waits for both).

## Child 1: Panel local viewer and rotation

- [x] Load the relevant backend/frontend specs before editing.
- [x] Consolidate duplicated slog/lumberjack/ring setup behind one shared
      contract while preserving thin command wrappers where useful.
- [x] Pass Panel ring/effective file into `AdminServer`.
- [x] Export/reuse the existing safe webadmin log viewer and mount authenticated
      `/api/admin/logs` in Panel mode.
- [x] Allow Panel `/logs`, add navigation/copy, and preserve standalone behavior.
- [x] Add setup/rotation/history/security tests for Panel and Node command paths,
      including async pruning and existing-file startup behavior.
- [x] Update config/operations docs and logging/webadmin/UI specs.
- [x] Run focused tests, frontend build, full Go verification, and review the
      child diff before commit/archive.

## Child 2: Node remote log query

- [x] Load control-plane, logging, webadmin, UI, and cross-layer specs.
- [x] Add backward-compatible structured log query fields and protocol tests.
- [x] Add ring time/level/query filtering and serialized-byte truncation.
- [x] Harden Node runner redaction, validation, and legacy text fallback.
- [x] Return typed, node-scoped Panel task responses; redact persisted log-query
      keywords and enforce timeout/late-result behavior.
- [x] Add typed API client methods and `PanelNodeLogsSection.vue` with bounded
      polling and complete visible states.
- [x] Add protocol/runner/orchestrator/admin API/frontend tests.
- [x] Update control-plane/logging/webadmin/UI specs and review the child diff
      before commit/archive.

## Parent integration

- [x] Extend `scripts/test-panel-node-e2e.sh` with a deterministic safe remote
      log query and Panel local-log assertion in local and Docker modes where
      applicable.
- [x] Ensure reports contain no generated token/password/S3 secret/private key/
      cookie/full signed URL and preserve existing cleanup guarantees.
- [x] Verify standalone is not reintroduced into Docker/release artifacts;
      compile/test it only as compatibility coverage.
- [x] Run the full validation matrix below.
- [ ] Perform final spec consistency review, commit task-owned changes, archive
      both children and then the parent, and record the session journal.

## Validation commands

Focused commands will be selected from the changed files; the final gate is:

```bash
gofmt -w <changed-go-files>
npm --prefix pkg/webadmin/ui run build
go test ./pkg/config ./pkg/logging ./pkg/webadmin ./pkg/panel ./pkg/nodeagent ./pkg/controlproto ./cmd/panel ./cmd/node ./cmd/natives3bridge -count=1
go vet ./...
go test ./...
go test -race ./...
go build ./...
bash -n scripts/test-panel-node-e2e.sh scripts/test-distribution-contract.sh
python3 -m py_compile scripts/internal/e2e-browser.py
bash scripts/test-distribution-contract.sh
bash scripts/test-panel-node-e2e.sh --mode local
```

Docker mode is required before final completion on a Docker-capable environment:

```bash
bash scripts/test-panel-node-e2e.sh --mode docker
```

## Risk and rollback points

- Shared logging setup touches all command entry points: preserve command-level
  wrappers/tests and stop if standalone compatibility changes unexpectedly.
- Panel route matching affects the shared SPA: verify both service modes before
  accepting the frontend diff.
- Task timeout changes affect storage tasks too: use conditional state updates
  and regression-test success/disconnect/timeout/late-result races.
- Protocol changes must be additive and keep legacy `log_lines` decoding.
- Do not expand the control channel into file transfer or live streaming during
  implementation; any such need returns to planning.

## Completion definition

- [x] Parent and child acceptance criteria are satisfied.
- [x] No unresolved blocking question or known secret leakage remains.
- [x] All required local/Docker gates pass or an environmental limitation is
      documented with equivalent evidence.
- [ ] Specs, docs, task artifacts, commits, archives, and journal are complete.
