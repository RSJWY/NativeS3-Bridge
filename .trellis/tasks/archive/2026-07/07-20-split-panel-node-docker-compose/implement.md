# Implementation Plan

1. Add standalone panel and node Compose templates using GHCR images and independent bind-mounted config/data directories.
2. Add a panel installer that validates dependencies/arguments, generates secrets and PKI, writes config/Compose, sets permissions, pulls the image, validates Compose, and starts panel.
3. Add a node installer that validates dependencies/arguments and the supplied CA, writes config/Compose, sets permissions, pulls the image, validates Compose, and starts node.
4. Add `docs/docker-deployment.md` with no-clone quick deployment, panel-to-node registration handoff, complete manual deployment, upgrades, logs, removal, external DB notes, and security boundaries.
5. Replace README's stale release warning and long Docker walkthrough with a short link-driven deployment section.
6. Remove the old combined Compose example and update distribution-contract checks for the split templates/installers.
7. Validate shell syntax, Compose rendering for repository templates and installer-generated files, distribution contracts, and relevant tests. Run an isolated installer generation/container startup smoke test where environment permits.
8. Review only task-owned changes, avoiding the pre-existing uncommitted dashboard files. Commit and push the current branch after verification.

## Validation Commands

```bash
bash -n scripts/install-panel.sh scripts/install-node.sh
NATIVES3_TAG=latest docker compose -f docker-compose.panel.yml config
NATIVES3_TAG=latest docker compose -f docker-compose.node.yml config
bash scripts/test-distribution-contract.sh
go test ./pkg/config ./pkg/nodeagent ./pkg/panel
```

For installer output, run each installer in a temporary directory with a non-conflicting Compose project/ports or generation-only mode, then run `docker compose config` against generated files. If actual published-image startup is available, verify panel health and node configuration startup without modifying repository data.

## Rollback Points

- Before deleting `docker-compose.example.yml`, ensure both standalone templates contain the equivalent service healthchecks, mounts, and port boundaries.
- Before replacing README content, preserve advanced operations via `docs/docker-deployment.md` and `docs/multi-node-operations.md` links.
- Never stage or commit the pre-existing `pkg/webadmin/ui` working-tree changes.
