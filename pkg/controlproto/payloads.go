package controlproto

// This file defines the type-specific payload structures carried in
// Envelope.Payload. Every payload is plain JSON with exported fields. New
// fields must be optional (omitempty where reasonable) so that older peers that
// do not understand them continue to decode successfully.

// HelloPayload is sent by the node as the first frame after the mTLS WebSocket
// handshake. It advertises the node identity and the state the node has already
// applied so the panel can decide whether reconciliation is required.
type HelloPayload struct {
	ProtocolVersion int    `json:"protocol_version"`
	NodeID          string `json:"node_id"`
	AgentVersion    string `json:"agent_version"`
	AppliedVersion  int64  `json:"applied_version"`
	ContentHash     string `json:"content_hash"`
}

// HelloAckPayload is the panel's response to hello. It reports the negotiated
// protocol version, the panel's clock (for skew detection), and whether the
// node must reconcile against a newer desired state.
type HelloAckPayload struct {
	ProtocolVersion int    `json:"protocol_version"`
	ServerTime      string `json:"server_time"`
	NeedsSync       bool   `json:"needs_sync"`
	DesiredVersion  int64  `json:"desired_version"`
}

// HeartbeatPayload is sent periodically by the node to keep the connection
// alive and report a lightweight observed-state summary.
type HeartbeatPayload struct {
	AppliedVersion int64 `json:"applied_version"`
	UsedBytesTotal int64 `json:"used_bytes_total,omitempty"`
	ObjectCount    int64 `json:"object_count,omitempty"`
}

// HeartbeatAckPayload carries the panel clock so the node can detect drift.
type HeartbeatAckPayload struct {
	ServerTime string `json:"server_time"`
}

// DesiredStatePayload delivers the full latest desired state to a node. The
// panel is the sole authority; the node persists AppliedVersion after a
// successful apply. Content is the whole schema (not a delta) so that a single
// message fully reconciles the node.
type DesiredStatePayload struct {
	Version     int64        `json:"version"`
	ContentHash string       `json:"content_hash"`
	Content     DesiredState `json:"content"`
}

// SyncState enumerates the reconciliation outcomes reported back to the panel.
type SyncState string

const (
	SyncStateSynced  SyncState = "synced"
	SyncStateWaiting SyncState = "waiting"
	SyncStateFailed  SyncState = "failed"
	SyncStateDrift   SyncState = "drift"
)

// AckPayload reports the result of applying a desired-state version. Error is
// populated only when State is failed.
type AckPayload struct {
	Version     int64     `json:"version"`
	State       SyncState `json:"state"`
	ContentHash string    `json:"content_hash"`
	Error       string    `json:"error,omitempty"`
}

// TaskType enumerates the predefined one-shot operations. There is deliberately
// no generic shell/command channel: only these fixed operations are allowed.
type TaskType string

const (
	TaskLogQuery              TaskType = "log_query"
	TaskStorageScan           TaskType = "storage_scan"
	TaskStorageReconcileApply TaskType = "storage_reconcile_apply"
)

// TaskPayload is a one-shot task dispatched panel->node. TaskID is the
// idempotency key: a node that receives a duplicate TaskID must execute once
// and re-send the cached result. Params is task-type specific.
type TaskPayload struct {
	TaskID    string     `json:"task_id"`
	Type      TaskType   `json:"type"`
	Params    TaskParams `json:"params"`
	TimeoutMS int64      `json:"timeout_ms,omitempty"`
	CreatedBy string     `json:"created_by,omitempty"`
}

// TaskParams holds the union of parameters across task types. Only the fields
// relevant to Type are populated. Keeping a single struct (rather than raw JSON)
// keeps the wire contract explicit and type-checked on both ends.
type TaskParams struct {
	// log_query
	Since   string `json:"since,omitempty"`
	Until   string `json:"until,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Keyword string `json:"keyword,omitempty"`

	// storage_scan / storage_reconcile_apply
	Bucket string `json:"bucket,omitempty"`
	Apply  bool   `json:"apply,omitempty"`
}

// TaskState enumerates the terminal states of a task from the panel's view.
type TaskState string

const (
	TaskStatePending TaskState = "pending"
	TaskStateRunning TaskState = "running"
	TaskStateSuccess TaskState = "success"
	TaskStateFailed  TaskState = "failed"
	TaskStateUnknown TaskState = "unknown"
)

// TaskResultPayload is the node's response to a task. Result carries the
// bounded result set; Error is set when State is failed.
type TaskResultPayload struct {
	TaskID string     `json:"task_id"`
	Type   TaskType   `json:"type"`
	State  TaskState  `json:"state"`
	Result TaskResult `json:"result"`
	Error  string     `json:"error,omitempty"`
}

// TaskResult holds the bounded outputs of a task. Fields are populated per Type.
type TaskResult struct {
	// log_query: bounded set of log lines plus whether results were truncated.
	LogLines     []string `json:"log_lines,omitempty"`
	LogTruncated bool     `json:"log_truncated,omitempty"`

	// storage_scan / storage_reconcile_apply
	Bucket             string `json:"bucket,omitempty"`
	Applied            bool   `json:"applied,omitempty"`
	ObjectCount        int64  `json:"object_count,omitempty"`
	ScannedBytes       int64  `json:"scanned_bytes,omitempty"`
	OrphanSidecarCount int    `json:"orphan_sidecar_count,omitempty"`
	OrphansDeleted     int    `json:"orphans_deleted,omitempty"`
	CredentialsUpdated int    `json:"credentials_updated,omitempty"`
}

// ImportReportPayload is the node's read-only report of its existing local
// business config during in-place migration (design §8.3). It carries the full
// local state (including plaintext secret keys, which travel only over the
// established mTLS channel) so the panel can build an import summary and, only
// after admin confirmation, adopt it as the version=1 baseline. Sending this
// report never mutates the node.
type ImportReportPayload struct {
	State            DesiredState `json:"state"`
	CredentialCount  int          `json:"credential_count"`
	BucketCount      int          `json:"bucket_count"`
	WebhookCount     int          `json:"webhook_count"`
	LocalContentHash string       `json:"local_content_hash"`
}

// ErrorCode enumerates protocol-level error codes exchanged in ErrorPayload.
type ErrorCode string

const (
	ErrCodeVersionIncompatible ErrorCode = "version_incompatible"
	ErrCodeUnauthorized        ErrorCode = "unauthorized"
	ErrCodeMalformed           ErrorCode = "malformed"
	ErrCodeInternal            ErrorCode = "internal"
)

// ErrorPayload is a protocol-level error. Fatal signals the receiver that the
// connection will be / should be closed (e.g. version incompatibility).
type ErrorPayload struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Fatal   bool      `json:"fatal,omitempty"`
}
