# Implementation Plan

1. Extend registration-token persistence for public-key binding and replayable issued certificate response.
2. Refactor Panel registration into one transactional authorize/sign/persist/replay operation; add concurrency, response-loss replay, and changed-key rejection tests.
3. Add typed node registration errors and a cancellable exponential-backoff registration loop; wire `cmd/node` to it and test transient recovery/permanent rejection.
4. Register real Panel `/healthz` and `/readyz` before SPA fallback with route tests.
5. Tighten Node config validation for `panel.ca_file` and add a Node live-health CLI probe; update Compose/install healthchecks and distribution contract tests.
6. Update backend/release specs with idempotent registration and health semantics.
7. Run formatting, focused tests, full tests/vet/build, and a local real-TLS Panel/Node registration recovery smoke.

## Validation Commands

```bash
go test ./pkg/panel ./pkg/nodeagent ./pkg/config ./cmd/node
go test ./...
go vet ./...
go build ./cmd/panel ./cmd/node
bash scripts/test-distribution-contract.sh
```

## Risk Points

- Registration token transactions must not leak whether a token is invalid, expired, used, or bound to another node/key.
- Retry loops must stop on context cancellation and avoid log storms.
- Health probes must not make Node depend on Panel availability.
