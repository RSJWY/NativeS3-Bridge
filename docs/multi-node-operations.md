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

### 6.1 The six components

A complete, restorable backup MUST cover all six:

1. Panel database.
2. Secret-key encryption **master key** (the external `master_key_file`).
3. Root CA certificate and **encrypted** root private key (offline).
4. Online intermediate CA certificate and private key.
5. Panel configuration.
6. Necessary audit data.

### 6.2 The two red lines

- **DB and master key are backed up separately.** Possession of the database
  backup alone MUST NOT yield plaintext S3 secret keys — the DB stores only
  ciphertext; the master key lives outside the DB. Store them in different trust
  domains.
- **Valid-cert nodes need no re-registration after restore.** Restoring the panel
  DB restores the certificate fingerprint table, so nodes whose certificates are
  still valid and unrevoked reconnect automatically. Re-registration is required
  only for nodes whose certs were revoked or expired.

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
4. Verify a DB-only restore (without the master key) cannot decrypt secrets —
   this proves the two backups are correctly separated.
5. Verify audit history is present.
