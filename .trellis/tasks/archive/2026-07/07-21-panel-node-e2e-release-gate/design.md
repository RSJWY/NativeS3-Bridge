# Technical Design

## 1. Boundaries

This task is a release-test and CI change. Product runtime code remains unchanged unless the new gate demonstrates a real defect in an already-claimed contract. Test artifacts live under `scripts/` and are safe to run against temporary resources.

## 2. Runtime Adapter

`scripts/test-panel-node-e2e.sh` owns the scenario and assertions. It has two adapters:

- `local`: builds `cmd/panel` and `cmd/node` with `GOWORK=off`, starts them as child processes, and talks through loopback ports. This is the no-Docker fallback.
- `docker`: builds `Dockerfile --target panel` and `--target node`, creates an isolated network, starts the final images with generated `/data` mounts, and maps only test ports. The same host-side assertions consume the mapped admin, agent and S3 endpoints.

The adapter exports `start_panel`, `start_node`, `stop_panel`, `stop_node`, `panel_admin_url`, `node_s3_url`, and `wait_ready`; scenario logic does not branch on container internals except for diagnostics.

## 3. Test Data and PKI

The script creates a temporary CA and server certificate with SANs for both `localhost`/`127.0.0.1` (local mode) and `panel` (Docker mode), plus a 32-byte panel master key. It writes separate panel/node YAML files and separate panel/node data roots. Credentials and registration tokens are held in shell variables or mode-600 temporary files and are never echoed.

## 4. Scenario Flow

1. Validate prerequisites and build UI/binaries or images.
2. Start Panel and wait for admin readiness.
3. Login through `/api/admin/login`; create node, registration token, bucket and credential; publish desired state while Node is offline.
4. Start Node with the configured CA and token. Wait for S3 listener, then poll `/api/admin/nodes/{id}` until `online=true`, `sync_state=synced`, and the desired/applied versions match.
5. Execute SigV4 bucket/object PUT, HEAD, GET and DELETE against Node; compare downloaded bytes and native file bytes.
6. Stop Panel, assert Node S3 remains reachable, restart Panel using the same data/PKI, and wait for mTLS reconnect/sync.
7. Restart Node with persisted data and an empty registration token; assert it reconnects without a new token and preserves S3 access.
8. Start a negative node attempt with an unrelated CA; assert registration fails closed while its S3 listener remains alive.
9. Run the ChromeDriver browser check: login, navigate to `/dashboard`, assert the Panel service redirects to `/nodes`, assert node text, and inspect performance logs for forbidden standalone API paths.
10. Emit a redacted report and clean all resources in a trap.

Registration response-loss replay remains covered by the existing `pkg/nodeagent` tests; the script records that evidence separately instead of pretending a proxy-based fault injection occurred.

## 5. Browser Driver

`scripts/internal/e2e-browser.py` uses only Python standard library plus the ChromeDriver WebDriver HTTP API. It creates an ephemeral Chrome profile, sets headless flags, executes same-origin JavaScript for login/navigation, waits for the Panel node view, and reads performance logs. It returns non-zero on unexpected route/API requests and never prints cookies or form values.

## 6. CI Topology

Add an `e2e` job after `quality`:

```text
prepare → quality → e2e → {artifacts, images} → release
```

The job checks out `needs.prepare.outputs.sha`, sets up Node 18 and Go 1.21, builds the embedded UI, runs the local adapter, then runs Docker mode. Artifacts/images/release add `e2e` to `needs`. An `if: failure()` upload step may publish only the redacted report and selected logs with a short retention period.

## 7. Compatibility, Security, and Rollback

- No database migrations or runtime API changes are introduced.
- Local mode remains usable on developer machines without Docker; Docker mode is required on the hosted release runner.
- If a test fails, cleanup runs before reporting and no generated credential/PKI file is retained.
- Rollback is removing the test script/helper and the `e2e` workflow dependency; existing build/test release gates remain intact.
