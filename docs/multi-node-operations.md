# Multi-Node mTLS Control Plane — Operations Guide

This guide covers deploying and operating the panel (国内 management plane) plus
one or more nodes (海外 S3 data plane). It is the hard-cutover replacement for the
single `natives3bridge` binary: existing deployments migrate to the panel + node
pair (design §8.1/§8.3). There is no supported standalone mode after the cutover.

---

## 1. Topology

```
                 ┌───────────────────────────┐
   admin ───────▶│  panel (cmd/panel)         │
   HTTPS 9001    │  - WebAdmin UI + REST      │
                 │  - node control listener   │◀──── nodes dial in (mTLS)
                 │    9443 (mTLS WebSocket)   │      wss://panel:9443/agent
                 └───────────────────────────┘
                                                   ┌────────────────────────┐
   S3 clients ────────────────────────────────────▶│ node (cmd/node)        │
   direct to node egress, never via panel          │ - S3 data plane 9000   │
                                                    │ - agent client (dials) │
                                                    └────────────────────────┘
```

- Nodes **only dial the panel**; the panel never connects back to a node.
- S3 traffic goes **directly** to the node's own network egress, never through
  the panel.
- The node exposes **no** management/admin port. Only 9000 (S3) is listened on.

---

## 2. First-boot registration flow

1. Admin creates a logical node in the panel UI → panel issues a **single-use,
   10-minute** registration token.
2. The node's first boot generates a private key locally (**never uploaded**) and
   a CSR.
3. The node POSTs `{node_id, token, csr}` to `https://panel:9443/register` over
   **server TLS** (it verifies the panel via the configured CA file).
4. The panel validates the token (unused, unexpired, matching the node), signs a
   client certificate with the online intermediate CA, and **immediately burns
   the token**.
5. The node saves the issued certificate and thereafter connects with **mTLS**;
   the registration token is never used again.

If registration is not yet done, the node still serves S3 from its local DB
(safety net A) and retries in the background.

---

## 3. Node lifecycle

| State | Meaning | Effect |
|---|---|---|
| `active` | Normal | Receives desired state and tasks |
| `disabled` | Paused (reversible) | Live connection dropped; no desired state/tasks until re-enabled |
| `retired` | Permanent (UI "delete") | All certs + tokens revoked; node row retained for audit |

Retiring a node revokes its certificates: it can no longer connect to the control
plane. This does **not** stop the node's S3 data plane (see §5).

---

## 4. In-place migration (adopting an existing single node)

The flow is strictly **read-then-confirm** — the panel writes no business config
to the node before the admin confirms (design §8.3):

1. Upgrade the host to the `node` image, keeping the same `data_root` and DB. The
   node serves S3 from its existing local DB immediately (safety net A).
2. Register the node (§2).
3. Admin triggers an import; the node reports its current buckets/credentials/
   quotas **read-only** (plaintext secret keys travel only over the established
   mTLS channel and are encrypted by the panel on receipt).
4. Admin reviews the import summary and **confirms**. Only then does the panel
   adopt the config into its own tables and publish the `version=1` baseline.
5. Aborting discards the pending import; the node keeps serving S3 unchanged.

A node that already has panel-managed config cannot be re-adopted (guards against
clobbering).

---

## 5. Security incident handling

Revoking a node's certificate **only severs the control plane**. It does **not**
stop the node's S3 data plane. On suspected node/key compromise you must ALSO:

- Stop the node's host service / container (to stop serving S3), and/or
- **Rotate the affected S3 credentials** (rotate in the panel → publish → the new
  secret propagates; the old secret stops working once the node applies it).

Certificate revocation, host shutdown, and credential rotation are independent
levers; a real incident usually needs all three.

---

## 6. Backup and recovery set (design §7.3)

### 6.1 The five components

A complete, restorable backup MUST cover all five:

1. Panel database (`<install_dir>/data/panel.db`).
2. Secret-key encryption **master key** (`<install_dir>/data/secrets/master.key`,
   the external `master_key_file`).
3. Deployment CA certificate and private key
   (`<install_dir>/data/pki/intermediate-ca.crt` + `.key`). Despite the file
   name this is the self-signed root (see §10.1): it is simultaneously the
   client-cert issuer, the server-cert issuer, and the trust anchor. **Losing
   it means full cluster reinstall** - there is no recovery path (§10.6).
4. Panel configuration (`<install_dir>/panel.yaml`).
5. Necessary audit data - it lives **inside the panel database** (the
   `audit_logs` table), not in a separate file; item 1 covers it.

Earlier versions of this checklist listed an "offline root CA" as a separate
item. No offline root has ever existed in the implementation; if you backed
up per the old six-item list, the deployment CA above is the item that
covered it. See §10.6 for the registered limitation.

The panel server certificate (`panel-server.crt`/`.key`) and the public
`panel-ca.crt` copy are deliberately **not** backup items: both can be
re-derived from the deployment CA (re-sign via §10.5 / re-copy the public
cert), so backing them up is redundant though harmless.

### 6.2 The two red lines

- **DB and master key are backed up separately.** Possession of the database
  backup alone MUST NOT yield plaintext S3 secret keys — the DB stores only
  ciphertext; the master key lives outside the DB. Store them in different trust
  domains.
- **Valid-cert nodes need no re-registration after restore.** Restoring the panel
  DB restores the certificate fingerprint table, so nodes whose certificates are
  still valid and unrevoked reconnect automatically. Re-registration is required
  only for nodes whose certs were revoked or expired - the expired-cert
  recovery sequence is §10.4.

Fail-closed: if the CA or the master key is truly lost, recovery is by
re-registering nodes and/or rotating S3 credentials. There is **no** backdoor to
bypass mTLS or export plaintext secrets.

---

## 7. Upgrade and rollback (hard cutover)

- The panel and node images build/upgrade/roll back independently but share the
  version-constrained `pkg/controlproto`; incompatible versions are rejected at
  the hello handshake rather than mis-parsed.
- **Rolling back the multi-node change entirely** = replacing the node image with
  the pre-multinode single binary. Safety net C (strictly additive node-DB
  migration) makes this safe: the old binary ignores the agent's added tables and
  keeps using the unchanged `credentials`/`buckets`/`request_stats`. Before
  rolling back, **disable the node in the panel** first so no desired state is
  pushed to a node about to run old code.
- Desired state is versioned; a bad publish is corrected by publishing a new
  version (no automatic rollback, to avoid fighting drift detection).

---

## 8. Recovery drill (checklist)

1. Restore the panel DB, master key, CA materials, and panel config to a fresh
   panel host.
2. Start the panel; confirm `-check-config` passes (fails closed if the master
   key or CA is missing).
3. Confirm existing nodes with valid certs reconnect **without** re-registration.
   (Nodes whose certs have already **expired** are not covered by this step -
   they follow the §10.4 recovery sequence instead.)
4. Verify a DB-only restore (without the master key) cannot decrypt secrets —
   this proves the two backups are correctly separated.
5. Verify audit history is present.

---

## 9. Process logs and local rotation

- Panel, Node, and the legacy rollback binary share one logging setup contract:
  stdout and the newest 2000 in-memory records are always enabled; an optional
  lumberjack file adds size/backup/age rotation and gzip compression.
- Prefer `log.dir` for new Panel/Node deployments. It resolves to
  `<dir>/natives3bridge.log`. Legacy `log.file` remains accepted as a complete
  path, but the two settings are mutually exclusive.
- The authenticated Panel `/logs` page reads only the Panel process's ring,
  active file, and exact lumberjack backup names. It supports level, keyword,
  limit, plain-history, and gzip-history filtering without accepting arbitrary
  filesystem paths.
- Node rotation files remain on each Node host. Raw Node history is not a
  control-plane file-transfer feature, and the UI does not offer download or
  delete actions.
- The Node detail page may dispatch the predefined `log_query` task over mTLS.
  It reads only the current in-memory ring, applies level/keyword/inclusive
  RFC3339 time filters before a 500-entry limit, and keeps the serialized result
  below 256 KiB. Timeout, disconnect, failure, empty, and truncation states are
  explicit; no shell, live stream, or raw rotated file is exposed.

---

## 10. Certificate lifecycle operations

This section is the runbook for the full certificate lifecycle: what exists,
what renews itself, what to check routinely, and how to recover when a
certificate has already expired. It reflects the shipped behavior (automatic
client-cert renewal, server-cert re-sign subcommand, expiry observability);
it does not describe planned features.

### 10.1 What certificates exist

Three certificate kinds exist in a deployment. There is **no** offline root
CA - see §10.6 for that known limitation.

| Certificate | File (host path) | Validity | Issued by | Expiry consequence | Auto-renewal |
|---|---|---|---|---|---|
| Deployment CA | `<panel_install_dir>/data/pki/intermediate-ca.crt` + `.key` | 3650 days | itself (`req -x509`, `pathlen:0`) | full cluster reinstall (§10.6) | no |
| Panel server cert | `<panel_install_dir>/data/pki/panel-server.crt` + `.key` | 825 days | deployment CA | nodes cannot connect; re-sign it (§10.5) | no |
| Node client cert | `<node_install_dir>/data/pki/node.crt` + `.key` (inside the node container: `/data/pki/node.crt`, `/data/pki/node.key`) | 90 days by default (`pki.client_cert_ttl`) | deployment CA | token re-registration required (§10.4) | **yes** (§10.2) |

Despite the file name, `intermediate-ca.crt` is **not** an intermediate: it is
a self-signed, `pathlen:0` root. The name is historical. It is simultaneously
the client-cert issuer, the server-cert issuer, and the only trust anchor on
both sides (nodes verify the panel server cert against it; the panel verifies
node client certs against it). Losing the CA key means losing everything:
every node cert, the server cert, and the trust anchor - recovery is a full
reinstall, see §10.6.

The public half is distributed to nodes as `panel-ca.crt` (copied to
`<node_install_dir>/data/pki/panel-ca.crt` during install). Only the public
certificate may be copied to nodes; the CA **key must never** leave the panel
host.

### 10.2 Automatic client-cert renewal

Under normal operation nothing needs to be done for node client certificates:

- Threshold: a node renews when the remaining validity drops below **one third
  of the certificate's total TTL** (`RenewalThreshold` = TTL/3). With the
  default 90-day TTL that is 30 days before expiry.
- Mechanism: the node POSTs a fresh CSR to `POST /renew` over the existing
  mTLS connection (identity comes from the current client cert; the CSR
  carries the node's public key).
- Old-cert revocation (activation semantics): the new certificate is **not**
  active until the node first connects with it. The old certificate is
  revoked only at that moment. If the renewed cert never connects, the old
  one keeps working - renewal cannot lock a node out.
- Failure behavior: if `/renew` fails (panel down, transient error), the node
  keeps its current certificate, retries with backoff alongside its normal
  reconnect loop, and logs an Error-level entry. The S3 data plane is never
  affected by renewal failures (safety net A).

Note: renewal is only possible **before** expiry. Once the NotAfter moment has
passed, the certificate can no longer be renewed or used - recovery is
re-registration (§10.4). There is deliberately no grace period (§10.4).

### 10.3 Checking expiry (routine)

Two supported ways; there is no Prometheus/metrics exporter for certificate
expiry.

- Admin UI: the node detail page shows a per-certificate table with status and
  days remaining. The dashboard aggregates nodes needing attention, including
  separate counts for cert-expiring and cert-expired nodes
  (`GET /api/admin/dashboard/summary` fields `certs.expiring_nodes` and
  `certs.expired_nodes`).
- Admin API: `GET /api/admin/nodes/{id}/certs` lists each
  certificate with `status` and `days_until_expiry`.

Certificate status is a four-state derived value (priority: revoked >
expired > expiring > active):

| Status | Meaning |
|---|---|
| `revoked` | revoked in the panel DB (takes precedence even if also expired) |
| `expired` | `now >= NotAfter`; `days_until_expiry` is negative |
| `expiring` | remaining validity is below TTL/3 (strictly; exactly at the threshold counts as active) |
| `active` | everything fine |

The panel logs a warning at startup if the deployment CA itself has less than
90 days of validity left. CA near-expiry has no remedy short of reinstall
(§10.6) - start planning immediately.

### 10.4 Recovering an expired node cert

When a node client certificate has passed its NotAfter, the node cannot
connect to the control plane. Its S3 data plane keeps serving its local DB
(safety net A), but it stops receiving desired state.

**There is no grace period, by design.** Accepting an expired client
certificate would require relaxing TLS client-certificate verification on the
control plane - opening the door to "expired certs can still connect" as a
class of attack. A one-time manual re-registration is preferred over any such
weakening.

Recovery steps:

1. Confirm the certificate status: in the admin UI node detail (or
   `GET /api/admin/nodes/{id}/certs`) the node's certificate shows
   `status: "expired"` with negative `days_until_expiry`. The node's log also
   reports an Error-level entry ("node certificate problem prevents
   control-plane connection ... client certificate expired at ...") with the
   recovery hint pointing back to these steps.
2. Issue a one-time registration token for the node:
   `POST /api/admin/nodes/{id}/tokens` (201; the plaintext token is
   returned exactly once; a retired node returns 409 - re-create the logical
   node instead).
3. Stop the node container/service.
4. Delete the expired certificate material on the node host:
   `<node_install_dir>/data/pki/node.crt` and `node.key`.
5. Put the new token into `node.yaml` (`panel.registration_token`).
6. Start the node; it generates a fresh key, registers over server TLS, and
   receives a new client certificate.
7. After the node is back online and synced, clear `registration_token` from
   `node.yaml` again (tokens are single-use; this is hygiene).

The node then reconnects over mTLS with its new certificate; no other nodes
are affected.

### 10.5 Re-signing the panel server cert

The panel's own server certificate (825 days by default) is used by nodes to
verify the panel. When it nears expiry - or when the hostname/IP nodes use to
reach the panel changes - re-sign it with the install script subcommand:

```bash
sudo ./install-panel.sh renew-server-cert \
  --install-dir /opt/natives3-panel \
  --panel-host panel.example.com,10.0.0.5
```

- `--panel-host` accepts a comma-separated list. Every hostname and IP that
  nodes actually use to connect to the panel MUST be in the certificate SAN -
  a node connecting via a name not present in the SAN fails TLS verification.
  That is the multi-SAN reason for the list form.
- The command is non-destructive: it writes only under
  `<install_dir>/data/pki/`, backs up the previous `panel-server.crt/.key`
  (`.bak.<timestamp>` files, with rollback instructions in its output), and
  never touches the panel DB, `master.key`, the CA, or node certificates.
- **Already-registered nodes are NOT affected**: the CA and their client
  certificates are unchanged; nodes reconnect without re-registration after
  the panel restarts.
- The panel loads the server certificate at process startup - it **must be
  restarted** for the new certificate to take effect. Pass `--restart` to let
  the subcommand restart the container automatically, or restart manually:
  `docker compose --project-directory /opt/natives3-panel restart panel`.

> **WARNING - never use `--force` to "renew" certificates.** Re-running the
> installer with `--force` deletes the entire installation directory
> (`rm -rf`), destroying the panel DB, the CA, and `master.key`. That is a
> full reinstall, not a renewal. The only certificate operations you need are
> this subcommand (server cert) and §10.2/§10.4 (node certs).

### 10.6 CA expiry - known limitation L1

The deployment CA `intermediate-ca.crt` is a self-signed, `pathlen:0` root
(see §10.1). There is no offline root CA above it and no mechanism to rotate
it while it remains the trust anchor - every node's `panel-ca.crt` and every
issued certificate chains directly to this one key pair. This is registered
as **known limitation L1** (CA hierarchy is not a true hierarchy and is not
rotatable); the "intermediate" file name is historical, not descriptive.

Consequences, stated plainly:

- When the CA expires (3650 days after install), **the only recovery is a
  full cluster reinstall**: new CA, re-install of the panel, re-registration
  of every node.
- If the CA private key is compromised or lost, same story - there is no
  in-place rotation path.

Planning advice: calendar the CA expiry date at install time
(`openssl x509 -noout -enddate -in <install_dir>/data/pki/intermediate-ca.crt`)
and schedule the reinstall window **6-12 months before it expires**. The
panel also warns in its log at startup once the CA has less than 90 days
left; by then the reinstall should already be scheduled, not starting.
