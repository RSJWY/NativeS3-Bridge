# Panel Authoritative Configuration Guidelines

> Executable contracts for Panel-owned node configuration drafts, exact published snapshots, control-plane delivery, node apply, managed S3 behavior, and in-place import.

---

## Scenario: Panel Authoritative Configuration Lifecycle

### 1. Scope / Trigger

- Trigger: any change to node-scoped configuration routes under `/api/admin/nodes/{id}`, `pkg/panel` desired-state/import/transport code, `pkg/controlproto` desired-state payloads, `pkg/managedconfig`, `pkg/nodeagent` apply state, managed router wiring in `pkg/server`, or the Panel node-detail UI.
- Goal: keep the Panel draft, the last explicitly published snapshot, and the node's last successfully applied state as three separate and testable layers.
- This scenario is cross-layer and security-sensitive because it spans two databases, encrypted credentials, mTLS WebSocket messages, live runtime caches/controllers, and native object directories.

### 2. Signatures

Panel admin routes, all behind the existing admin-session middleware:

```text
GET|POST   /api/admin/nodes/{id}/buckets
PUT        /api/admin/nodes/{id}/buckets/{name}/acl
DELETE     /api/admin/nodes/{id}/buckets/{name}

GET|POST   /api/admin/nodes/{id}/credentials
PATCH      /api/admin/nodes/{id}/credentials/{accessKey}
DELETE     /api/admin/nodes/{id}/credentials/{accessKey}
POST       /api/admin/nodes/{id}/credentials/{accessKey}/rotate

GET|POST   /api/admin/nodes/{id}/webhooks
PATCH      /api/admin/nodes/{id}/webhooks/{webhookID}
DELETE     /api/admin/nodes/{id}/webhooks/{webhookID}

GET|PUT|DELETE /api/admin/nodes/{id}/rate-limit
POST       /api/admin/nodes/{id}/desired-state
POST       /api/admin/nodes/{id}/desired-state/push

GET|POST   /api/admin/nodes/{id}/import
POST       /api/admin/nodes/{id}/import/confirm
POST       /api/admin/nodes/{id}/import/abort
```

Core backend signatures:

```go
func (a *DesiredStateAuthority) Publish(nodeID uint, updatedBy string) (version int64, hash string, err error)
func (a *DesiredStateAuthority) PublishTx(tx *gorm.DB, nodeID uint, updatedBy string) (version int64, hash string, err error)
func (a *DesiredStateAuthority) BuildPushable(nodeID uint) (controlproto.DesiredStatePayload, error)
func (a *DesiredStateAuthority) DraftStatus(nodeID uint) (dirty bool, publishRequired bool, err error)

func (m *MigrationCoordinator) RequestImport(ctx context.Context, hub *Hub, nodeID uint) (ImportSummary, error)
func (m *MigrationCoordinator) PendingSummary(nodeID uint) (ImportSummary, bool)
func (m *MigrationCoordinator) Confirm(nodeID uint, adminIdentity string) (version int64, hash string, err error)
func (m *MigrationCoordinator) Abort(nodeID uint, adminIdentity string) error

func (e *Executor) ApplyDesiredState(payload controlproto.DesiredStatePayload) (appliedHash string, err error)
func NewManagedRouterWithQuotaManager(..., rateLimit *RateLimitController) http.Handler
func (c *RateLimitController) Update(config.RateLimitConfig)
func (m *hooks.Manager) ReplaceConfigs(configs []db.HookConfig)
```

Wire and persistence signatures:

```go
const controlproto.CapabilityAuthoritativeConfigV1 = "authoritative_config_v1"

type HelloPayload struct {
    AppliedVersion int64    `json:"applied_version"`
    ContentHash    string   `json:"content_hash"`
    Capabilities   []string `json:"capabilities,omitempty"`
}

type DesiredStatePayload struct {
    Version     int64        `json:"version"`
    ContentHash string       `json:"content_hash"`
    Content     DesiredState `json:"content"`
}

type AckPayload struct {
    Version     int64     `json:"version"`
    State       SyncState `json:"state"`
    ContentHash string    `json:"content_hash"`
    Error       string    `json:"error,omitempty"`
}
```

- Panel draft tables: `node_credentials`, `node_buckets`, `node_webhooks`, `node_rate_limits`.
- Published table: one `desired_configs` row per node with monotonic `version`, schema-versioned `content_json`, and plaintext-derived `content_hash`.
- Node applied-state tables: existing business tables plus additive `agent_meta` and `managed_rate_limits`.

### 3. Contracts

#### Draft API and concurrency

- Every resource lookup and mutation is scoped by `node_id`; the node must exist before a credential, bucket, webhook, rate-limit, or import subroute operates.
- CRUD changes only draft tables. It must not increment `desired_configs.version`, push automatically, or mutate node state.
- Credential `secret_key` is returned only by create/rotate. List, update, delete, audit, node response, and persisted snapshot JSON never expose plaintext.
- A non-empty credential `bucket` must reference a bucket in the same node draft. Deleting a bucket with a bound credential is a conflict.
- Webhook events are an admin-facing `string[]`, canonicalized to a sorted comma-separated persistence value. Supported values are exactly `ObjectCreated` and `ObjectDeleted`.
- `enabled: false` must be written explicitly; do not let a GORM `default:true` tag turn a disabled webhook back on.
- All per-node draft read-check-write sequences, explicit publish, and import confirmation use the shared in-process `lockNodeDraft(nodeID)` boundary. Keep the database transaction inside that lock. A bare `COUNT` followed by `INSERT`/`DELETE` is not a sufficient invariant boundary.

#### Exact published snapshot

- `DesiredConfig.ContentJSON` is a Panel-internal schema-versioned snapshot. Credential entries contain `secret_key_cipher`, never plaintext and never a masked empty secret.
- Explicit publish reads the canonical draft inside a transaction, decrypts only in memory, validates the full state, computes `DesiredState.ContentHash()`, and atomically creates/increments the published row.
- `BuildPushable` reads only `desired_configs`; it must never consult current draft rows. It decrypts the persisted ciphertext, validates the state, recomputes the hash, and refuses delivery when the hash differs.
- A legacy snapshot without the supported `schema_version` is unrecoverable because the original secrets were masked. Return `ErrDesiredSnapshotRepublishRequired` and fail closed; never rebuild an old version from current draft rows.
- `DraftStatus` compares canonical encrypted draft JSON with the published snapshot. No published row means `draft_dirty=true`, even for an empty draft, so an administrator can explicitly publish an empty authoritative baseline.

#### Delivery, reconnect, and observed state

- New nodes advertise optional `authoritative_config_v1`. New Panels may keep an old node connected for health/observation but must reject authoritative pushes and record an actionable upgrade error when the capability is absent.
- After hello and Hub registration, a version/hash mismatch automatically pushes the exact published snapshot. Manual push has identical snapshot/capability checks.
- API responses derive `waiting` whenever the published version/hash differs from the last observed apply, except that stored `failed`/`drift` evidence remains authoritative. A missing published target cannot be displayed as `synced`; a legacy/unpushable target is displayed as a republish failure.
- Replacing a connection is atomic from the observed-state perspective: an obsolete connection's deferred disconnect must not mark the replacement offline. Recheck Hub ownership after any offline write so a replacement that registered during the write wins the final observed state.
- A failed apply ACK preserves the previously observed applied version/hash and records only a sanitized error.
- A `synced` ACK is trusted only when both version and hash match the current published row. A mismatch is recorded as `drift`, not `synced`.
- NodeState create/update uses one portable `INSERT ... ON CONFLICT(node_id) DO UPDATE` operation. SQLite `BUSY`/`LOCKED` errors use typed primary result codes with bounded, context-aware retry; never classify retryability from an error-message substring.

#### Node apply and runtime state

- `ApplyDesiredState` validates version, payload hash, all resource fields, uniqueness, and credential-to-bucket references before database mutation.
- A payload version lower than `AgentMeta.AppliedVersion` is rejected. Duplicate/same-version delivery may be reapplied only when it is not a regression and the content hash remains valid.
- Bucket-directory preflight may create missing empty directories. If a later preflight or transaction step fails, remove only empty directories created by that attempt; never remove retained data.
- Credentials, buckets, webhooks, managed rate-limit state, and `AgentMeta(version, hash)` commit in one transaction.
- Before saving `AgentMeta`, read the persisted business state back through the same transaction and compare its canonical hash with the payload. A trigger/default/coercion mismatch rolls back the transaction and must not ACK `synced`.
- After commit, credential cache invalidation, bucket ACL/existence invalidation, webhook replacement, and rate-limit controller update are in-memory operations with no error return. The node ACKs only after the database contract is proven.
- Absence of `managed_rate_limits` means the built-in anonymous defaults. Restart loads the persisted singleton before constructing the managed router.

#### Managed S3 and native object safety

- Only `cmd/node` opts into `NewManagedRouterWithQuotaManager`; standalone constructors retain filesystem bucket CRUD behavior.
- Managed `ListBuckets`, `HeadBucket`, and object routing use bucket metadata rows as authority. A retained directory without a metadata row is hidden and inaccessible.
- Managed S3 `CreateBucket` and `DeleteBucket` return `AccessDenied`; lifecycle changes enter only through Panel draft -> publish -> apply.
- Removing a bucket declaration deletes metadata only. Native object bytes and directories remain on disk.
- Declaring a previously unmanaged non-empty directory fails apply with the bucket name but without enumerating object keys.
- Server-side COPY validates both sides: the source bucket must be declared, and a bucket-scoped credential may read only from its bound source bucket as well as write to its allowed destination.

#### Import adoption

- Import request is read-only and online-only. The report is held in memory as a pending summary; secrets are encrypted immediately and never returned in the summary.
- Already-managed detection covers any node credential, bucket, webhook, rate-limit, or desired-config row.
- Confirm holds the node draft lock and uses one GORM transaction to insert all draft resources and publish version 1. Pending state is removed only after commit.
- Confirm failure rolls back every Panel business row. Confirm does not push or mutate the node.
- Request, report ingestion, confirm, and abort reject overlapping lifecycle operations rather than replacing or deleting state underneath an in-progress confirm.

### 4. Validation & Error Matrix

- Missing node on any node-scoped subroute -> HTTP `404` with a sanitized JSON error.
- Malformed JSON, unknown fields, invalid bucket name/ACL, invalid credential status/quota/name, invalid webhook URL/events, or non-positive rate-limit values -> HTTP `400`.
- Duplicate bucket or canonical webhook -> HTTP `409`.
- Credential references a missing same-node bucket -> HTTP `400`; deleting a bucket with bound credentials -> HTTP `409`.
- Missing credential/bucket/webhook/pending import -> HTTP `404`.
- Import on offline node, already-managed node, duplicate pending request, or overlapping import operation -> HTTP `409`; report timeout -> HTTP `504`.
- Missing capability, legacy snapshot, invalid published hash, offline push, or send failure -> no wire apply; `NodeState.sync_state` becomes `failed` with a sanitized actionable error.
- Published version/hash differs from the observed apply without stored failure evidence -> API reports `waiting`, including an offline publish that has not yet reconnected.
- SQLite NodeState write remains busy through the retry bound, or its context is canceled -> return the write/context error; do not silently report success.
- Invalid payload hash, invalid full-state references, version regression, retained non-empty directory, database failure, or readback hash mismatch -> node apply fails with prior DB/runtime/meta intact.
- Credentialed managed routing to an undeclared destination, or a declared-destination COPY whose source is undeclared -> S3 `404 NoSuchBucket`.
- Anonymous object access to a bucket without a public managed ACL row -> S3 `403 AccessDenied`, including retained directories whose declaration was removed.
- Managed direct bucket create/delete or cross-scope COPY with a bucket-bound credential -> S3 `403 AccessDenied`.
- ACK says `synced` but version/hash differs from published state -> Panel records `drift` and does not present the node as synced.

### 5. Good/Base/Bad Cases

- Good: publish version 4, edit/rotate/delete draft rows, then push current; the node still receives the exact version-4 content and secret until version 5 is explicitly published.
- Good: publish an empty draft on a fresh node; version 1 exists and later reconnect reconciliation can intentionally clear node-managed business config.
- Good: a full apply commits rows, reads them back to the same hash, updates `AgentMeta`, then immediately swaps credential/bucket/webhook/rate-limit runtime views.
- Good: deleting a Panel bucket, publishing, and applying hides it from S3 while all native object bytes remain in the retained directory.
- Base: an offline node accepts draft edits and a new published version; it receives that exact snapshot after reconnect if it advertises the capability.
- Base: resetting rate limit removes the managed singleton and restores built-in defaults after apply/restart.
- Bad: reconstructing a push from current draft tables under an old version/hash.
- Bad: recording the attempted version/hash from a failed ACK, or trusting a mismatched `synced` ACK.
- Bad: authorizing COPY only against the destination URL; the source is a separate read authorization boundary.
- Bad: checking webhook duplicates or bucket bindings without holding the per-node draft lock through the subsequent write.
- Bad: deleting or exposing a retained native directory merely because its Panel declaration changed.

### 6. Tests Required

- Panel snapshot tests assert draft edits after publish do not change `BuildPushable`, DB JSON contains ciphertext but no plaintext secret, legacy rows fail closed, stored hash is recomputed, and an empty first draft is publishable.
- Draft API tests cover auth, existing-node requirement, node isolation, strict JSON, validation, duplicate conflicts, credential/bucket binding, secret redaction, disabled webhook persistence, and audit redaction.
- Concurrency tests race duplicate webhook creates and bucket-delete against credential-create; final rows must satisfy the same invariants as serialized requests.
- Import tests cover read-only request, pending summary redaction, already-managed detection for every resource class, overlapping request/confirm/abort behavior, and transaction rollback.
- Transport tests cover optional capability decoding, new/new reconnect auto-push, old-node gate, connection replacement, failed ACK preservation, and mismatched synced ACK -> drift.
- Node response tests cover offline publish `synced -> waiting`, exact matching state remaining synced, failed/drift evidence preservation, no desired target, and legacy/unpushable target behavior.
- NodeState persistence tests hold a real SQLite write lock to prove typed busy retries occur, prove cancellation stops backoff, and stress replacement disconnect plus failed ACK races repeatedly.
- Executor tests cover invalid hash/reference rejection, version regression, preflight directory cleanup, retained-data guard, full transaction rollback, readback hash mismatch rollback, `AgentMeta`, cache invalidation, webhook replacement, and rate-limit persistence/runtime update.
- Managed router tests cover metadata-authoritative list/access, direct create/delete denial, retained bytes after logical delete, undeclared COPY source, and bucket-scoped cross-source COPY denial.
- Required automated gates: focused package tests, `go test -count=1 ./...`, `go vet ./...`, `go build ./...`, UI `npm ci && npm run build`, and `git diff --check`.
- Required live gate when the environment is available: Panel login -> draft CRUD -> publish; mTLS reconnect auto-sync; S3 ACL/credential/webhook/rate-limit behavior; retained-object hiding; import request -> abort -> request -> confirm.

### 7. Wrong vs Correct

Wrong:

```go
// Version/hash came from the last publish, but content came from mutable draft rows.
state, _ := authority.Build(nodeID)
return controlproto.DesiredStatePayload{Version: row.Version, ContentHash: row.ContentHash, Content: state}
```

Correct:

```go
// Version, encrypted content, and plaintext-derived hash are one immutable unit.
payload, err := authority.BuildPushable(nodeID)
```

Wrong:

```go
if ack.State == controlproto.SyncStateSynced {
    updateAppliedVersion(nodeID, ack.Version, ack.ContentHash)
}
```

Correct:

```go
if ack.State == controlproto.SyncStateSynced &&
    ack.Version == desired.Version && ack.ContentHash == desired.ContentHash {
    updateAppliedVersion(nodeID, ack.Version, ack.ContentHash)
} else {
    recordDrift(nodeID)
}
```

Wrong:

```go
// Destination auth alone does not authorize reading the copy source.
objectHandler.Copy(w, r, destinationBucket, key)
```

Correct:

```go
sourceBucket, err := handlers.CopySourceBucket(r)
requireDeclaredBucket(sourceBucket)
requireIdentityBucketScope(identity, sourceBucket)
objectHandler.Copy(w, r, destinationBucket, key)
```

---

## Common Mistakes

- Do not describe a draft delete as immediately changing S3 behavior; hiding occurs only after publish and successful node apply.
- Do not use `gorm.Create` with a `default:true` boolean when explicit `false` is meaningful unless the insert path explicitly selects/writes that field.
- Do not detect SQLite contention with `strings.Contains(err.Error(), "database is locked")`; application errors can contain the same text, and canceled contexts must stop retry promptly.
- Do not move node-agent additive tables into `pkg/db.Migrate`; only `cmd/node` owns `MigrateState`, so standalone remains compatible.
- Do not add a second validation implementation at the HTTP or executor boundary. Shared resource and full-state rules belong in `pkg/managedconfig`.
