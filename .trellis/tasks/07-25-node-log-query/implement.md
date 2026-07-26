# Implementation plan: Node remote log query

## Preconditions

- [x] Parent final planning summary is approved.
- [x] Curated context manifests validate.
- [x] Start this child before editing product code.
- [x] Reconcile any shared setup signature from the Panel viewer child before
      final integration.

## Checklist

1. [x] Load `trellis-before-dev` and control/logging/webadmin/UI specs.
2. [x] Add optional protocol fields/types and compatibility tests.
3. [x] Extend ring querying so time/level/keyword filters run before limits.
4. [x] Return structured sanitized entries with count/byte truncation and a
       legacy text fallback.
5. [x] Add safe param validation and redacted task persistence.
6. [x] Enforce task timeout/late-result/node-scope behavior with race-focused
       tests that also protect existing storage tasks.
7. [x] Add typed frontend task methods and the focused Node log component.
8. [x] Extend the Panel/Node E2E scenario with a deterministic safe query.
9. [ ] Update docs/specs, run validation, review diff, commit, and archive child.

## Validation

```bash
gofmt -w <changed-go-files>
go test ./pkg/logging ./pkg/controlproto ./pkg/nodeagent ./pkg/panel ./cmd/node -count=1
npm --prefix pkg/webadmin/ui run build
go vet ./...
go test ./...
go test -race ./...
go build ./...
bash scripts/test-panel-node-e2e.sh --mode local
```

Run Docker E2E before parent completion on a Docker-capable host.

## Rollback point

Remove the optional structured fields/UI while retaining the legacy task type.
No database schema rollback is required.
