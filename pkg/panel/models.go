// Package panel implements the control-plane panel: node registration, PKI,
// desired-state authority, task orchestration, and audit. Its persistence layer
// is a GORM schema that is completely independent of the node-level schema in
// pkg/db — the two databases are physically separate and migrate independently
// (see migrate.go). The panel never reuses node table structures.
package panel

import "time"

// NodeStatus lifecycle values. A node is created active; disabled pauses
// desired-state delivery and tasks but is reversible; retired is permanent and
// revokes all certificates and registration tokens.
const (
	NodeStatusActive   = "active"
	NodeStatusDisabled = "disabled"
	NodeStatusRetired  = "retired"
)

// SyncState mirrors controlproto.SyncState for the observed node_status row.
const (
	SyncStateSynced  = "synced"
	SyncStateWaiting = "waiting"
	SyncStateFailed  = "failed"
	SyncStateDrift   = "drift"
)

// Node is a logical node. ID is the internal unique identifier and is never
// reused (reinstall/replace keeps the same logical node but issues a new
// certificate). DisplayName is mutable. UI "delete" is implemented as retire,
// preserving audit relationships rather than physically deleting the row.
type Node struct {
	ID          uint   `gorm:"primaryKey"`
	DisplayName string `gorm:"size:128;not null"`
	Status      string `gorm:"size:16;not null;default:active"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	RetiredAt   *time.Time
}

// NodeCert tracks a client certificate issued to a node for mTLS. Fingerprint
// is the SHA-256 of the DER certificate (hex). The panel stores only the
// certificate and its metadata, never the private key. Revoked certificates are
// rejected at mTLS handshake time.
type NodeCert struct {
	ID          uint   `gorm:"primaryKey"`
	NodeID      uint   `gorm:"index;not null"`
	Fingerprint string `gorm:"uniqueIndex;size:64;not null"`
	Serial      string `gorm:"size:64;not null"`
	NotBefore   time.Time
	NotAfter    time.Time
	Revoked     bool `gorm:"not null;default:false"`
	RevokedAt   *time.Time
	CreatedAt   time.Time
}

// RegistrationToken is a single-use, short-lived token that authorizes a node's
// first registration. Only the hash is stored (never the plaintext). UsedAt is
// set the moment the token is consumed, after which it is invalid.
type RegistrationToken struct {
	ID        uint   `gorm:"primaryKey"`
	NodeID    uint   `gorm:"index;not null"`
	TokenHash string `gorm:"uniqueIndex;size:64;not null"`
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// DesiredConfig holds the single latest desired state for a node. Version is
// monotonic; only the newest version is retained (no per-command backlog). The
// content is the JSON of controlproto.DesiredState and ContentHash is its
// canonical hash.
type DesiredConfig struct {
	ID          uint   `gorm:"primaryKey"`
	NodeID      uint   `gorm:"uniqueIndex;not null"`
	Version     int64  `gorm:"not null;default:0"`
	ContentJSON string `gorm:"type:text;not null"`
	ContentHash string `gorm:"size:64;not null"`
	UpdatedBy   string `gorm:"size:128"`
	UpdatedAt   time.Time
}

// NodeState is the observed (reported) state of a node. It is updated from
// heartbeats and acks and is the panel's read-only view of the node.
type NodeState struct {
	ID             uint   `gorm:"primaryKey"`
	NodeID         uint   `gorm:"uniqueIndex;not null"`
	Online         bool   `gorm:"not null;default:false"`
	AppliedVersion int64  `gorm:"not null;default:0"`
	SyncState      string `gorm:"size:16;not null;default:waiting"`
	ContentHash    string `gorm:"size:64"`
	LastError      string `gorm:"size:512"`
	LastHeartbeat  *time.Time
	UpdatedAt      time.Time
}

// NodeCredential stores an S3 credential owned by the panel. The secret key is
// stored only as ciphertext (AEAD, master key held outside the DB). The
// plaintext secret is returned to the admin exactly once at creation and is
// never returned by list/detail/log/audit endpoints. AccessKey is unique per
// node (credentials are node-level resources with independent namespaces).
type NodeCredential struct {
	ID              uint   `gorm:"primaryKey"`
	NodeID          uint   `gorm:"not null;uniqueIndex:idx_node_access"`
	AccessKey       string `gorm:"size:128;not null;uniqueIndex:idx_node_access"`
	SecretKeyCipher string `gorm:"type:text;not null"`
	Name            string `gorm:"size:128"`
	Bucket          string `gorm:"size:63"`
	Status          string `gorm:"size:16;not null;default:enabled"`
	QuotaBytes      int64  `gorm:"not null;default:0"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Task records a one-shot task and its result. TaskID is the idempotency key.
// Params and Result are JSON blobs. State follows controlproto.TaskState.
type Task struct {
	ID         uint   `gorm:"primaryKey"`
	TaskID     string `gorm:"uniqueIndex;size:64;not null"`
	NodeID     uint   `gorm:"index;not null"`
	Type       string `gorm:"size:32;not null"`
	Params     string `gorm:"type:text"`
	State      string `gorm:"size:16;not null;default:pending"`
	ResultJSON string `gorm:"type:text"`
	Error      string `gorm:"size:512"`
	CreatedBy  string `gorm:"size:128"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// AuditLog is an append-only record of every management action. It must never
// contain private keys, full S3 secret keys, registration token plaintext, or
// session credentials.
type AuditLog struct {
	ID             uint      `gorm:"primaryKey"`
	TS             time.Time `gorm:"index"`
	Action         string    `gorm:"size:64;not null"`
	TargetNode     uint      `gorm:"index"`
	TargetResource string    `gorm:"size:256"`
	Result         string    `gorm:"size:32"`
	Source         string    `gorm:"size:128"`
}

// NodeBucket is a panel-authoritative bucket declaration for a node. It mirrors
// the node-level bucket (name + ACL) but lives in the panel schema so the panel
// is the sole authority (design §2.1). Unique per (node, name).
type NodeBucket struct {
	ID        uint   `gorm:"primaryKey"`
	NodeID    uint   `gorm:"not null;uniqueIndex:idx_node_bucket"`
	Name      string `gorm:"size:63;not null;uniqueIndex:idx_node_bucket"`
	ACL       string `gorm:"size:16;not null;default:private"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NodeWebhook is a panel-authoritative webhook declaration for a node. Mirrors
// the node HookConfig (url + events + enabled).
type NodeWebhook struct {
	ID        uint   `gorm:"primaryKey"`
	NodeID    uint   `gorm:"index;not null"`
	URL       string `gorm:"size:512;not null"`
	Events    string `gorm:"size:256;not null"`
	Enabled   bool   `gorm:"not null;default:true"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NodeRateLimit is the panel-authoritative anonymous rate-limit policy for a
// node. At most one row per node.
type NodeRateLimit struct {
	ID             uint    `gorm:"primaryKey"`
	NodeID         uint    `gorm:"not null;uniqueIndex:idx_node_ratelimit"`
	AnonymousRPS   float64 `gorm:"not null;default:0"`
	AnonymousBurst int     `gorm:"not null;default:0"`
	TrustForwarded bool    `gorm:"not null;default:false"`
	UpdatedAt      time.Time
}
