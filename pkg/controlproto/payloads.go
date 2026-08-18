package controlproto

import (
	"fmt"
	"strings"
	"time"
)

// This file defines the type-specific payload structures carried in
// Envelope.Payload. Every payload is plain JSON with exported fields. New
// fields must be optional (omitempty where reasonable) so that older peers that
// do not understand them continue to decode successfully.

// HelloPayload is sent by the node as the first frame after the mTLS WebSocket
// handshake. It advertises the node identity and the state the node has already
// applied so the panel can decide whether reconciliation is required.
// Region 是节点本地 yaml 里的 S3 签名区域(node 侧 cfg.Region),纯观测字段:
// Panel 只展示节点自报的值,不做权威下发——区域仍归节点配置所有。omitempty
// 保证旧节点(不发送该字段)照常握手,Panel 侧表现为"未上报"。
type HelloPayload struct {
	ProtocolVersion int      `json:"protocol_version"`
	NodeID          string   `json:"node_id"`
	AgentVersion    string   `json:"agent_version"`
	AppliedVersion  int64    `json:"applied_version"`
	ContentHash     string   `json:"content_hash"`
	Capabilities    []string `json:"capabilities,omitempty"`
	Region          string   `json:"region,omitempty"`
	// HeartbeatIntervalMS 是节点自报的心跳间隔(毫秒)。同步升级后必填;
	// panel 据此计算离线阈值与读超时。非法值走 panel 配置回落并 Warn。
	HeartbeatIntervalMS int `json:"heartbeat_interval_ms"`
}

const CapabilityAuthoritativeConfigV1 = "authoritative_config_v1"

func HasCapability(capabilities []string, capability string) bool {
	for _, candidate := range capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
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
//
// 遥测字段(used_bytes_total / object_count / observed_at)是可选的节点存储
// 观测:指针类型让"字段缺失"与"合法的 0 字节 / 0 对象"可区分。旧节点不发
// 这三个字段,Panel 必须把缺失解释为"未上报",绝不能当成 0。observed_at 是
// 节点统计计数器的时间(RFC3339 UTC),不是 Panel 收到心跳的时间;三个字段
// 只要有任何一处不完整,整组遥测都按"不可用"处理(见 Telemetry)。
type HeartbeatPayload struct {
	AppliedVersion int64  `json:"applied_version"`
	UsedBytesTotal *int64 `json:"used_bytes_total,omitempty"`
	ObjectCount    *int64 `json:"object_count,omitempty"`
	ObservedAt     string `json:"observed_at,omitempty"`
}

// HeartbeatTelemetry is the validated telemetry view of a heartbeat: the
// three optional fields are only meaningful together.
type HeartbeatTelemetry struct {
	UsedBytesTotal int64
	ObjectCount    int64
	ObservedAt     time.Time
}

// Telemetry returns the node's storage observation when the payload carries a
// complete, well-formed telemetry group. Missing fields, a missing/unparsable
// observed_at, or partial data all yield ok=false ("未上报/不可用"), never a
// fabricated zero snapshot.
func (h HeartbeatPayload) Telemetry() (HeartbeatTelemetry, bool) {
	if h.UsedBytesTotal == nil || h.ObjectCount == nil {
		return HeartbeatTelemetry{}, false
	}
	if *h.UsedBytesTotal < 0 || *h.ObjectCount < 0 {
		return HeartbeatTelemetry{}, false
	}
	observed := strings.TrimSpace(h.ObservedAt)
	if observed == "" {
		return HeartbeatTelemetry{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, observed)
	if err != nil {
		return HeartbeatTelemetry{}, false
	}
	return HeartbeatTelemetry{
		UsedBytesTotal: *h.UsedBytesTotal,
		ObjectCount:    *h.ObjectCount,
		ObservedAt:     parsed.UTC(),
	}, true
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
	Level   string `json:"level,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Keyword string `json:"keyword,omitempty"`

	// storage_scan / storage_reconcile_apply
	Bucket string `json:"bucket,omitempty"`
	Apply  bool   `json:"apply,omitempty"`
}

const (
	DefaultLogQueryLimit    = 200
	MaxLogQueryLimit        = 500
	MaxLogQueryKeywordBytes = 256
	MaxLogQueryResultBytes  = 256 << 10
	MaxLogQueryLevelBytes   = 32
)

// LogQuerySpec is the validated, normalized interpretation of log_query
// parameters. It is owned by the wire package so Panel and Node cannot drift on
// limits or time semantics.
type LogQuerySpec struct {
	Since   *time.Time
	Until   *time.Time
	Level   string
	Keyword string
	Limit   int
}

// ParseLogQuery validates task parameters without echoing user-provided values
// into errors. RFC3339 boundaries are inclusive when consumed by the ring.
func ParseLogQuery(params TaskParams) (LogQuerySpec, error) {
	if params.Limit < 0 {
		return LogQuerySpec{}, fmt.Errorf("log limit must not be negative")
	}
	keyword := strings.TrimSpace(params.Keyword)
	if len(keyword) > MaxLogQueryKeywordBytes {
		return LogQuerySpec{}, fmt.Errorf("log keyword is too long")
	}
	level := strings.TrimSpace(params.Level)
	if len(level) > MaxLogQueryLevelBytes {
		return LogQuerySpec{}, fmt.Errorf("log level is too long")
	}
	since, err := parseOptionalRFC3339("since", params.Since)
	if err != nil {
		return LogQuerySpec{}, err
	}
	until, err := parseOptionalRFC3339("until", params.Until)
	if err != nil {
		return LogQuerySpec{}, err
	}
	if since != nil && until != nil && since.After(*until) {
		return LogQuerySpec{}, fmt.Errorf("log since must not be after until")
	}
	limit := params.Limit
	if limit == 0 {
		limit = DefaultLogQueryLimit
	}
	if limit > MaxLogQueryLimit {
		limit = MaxLogQueryLimit
	}
	return LogQuerySpec{Since: since, Until: until, Level: level, Keyword: keyword, Limit: limit}, nil
}

func parseOptionalRFC3339(name, value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, fmt.Errorf("log %s must be RFC3339", name)
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

// TaskLogEntry is the structured, redacted log representation carried over
// the control channel. Attr values are strings so peers never have to decode
// arbitrary process-local values.
type TaskLogEntry struct {
	Time  string            `json:"time,omitempty"`
	Level string            `json:"level,omitempty"`
	Msg   string            `json:"msg"`
	Attrs map[string]string `json:"attrs,omitempty"`
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
	// log_query: new peers prefer structured entries. LogLines remains additive
	// compatibility for older peers and is bounded by the same limits.
	LogEntries   []TaskLogEntry `json:"log_entries,omitempty"`
	LogLines     []string       `json:"log_lines,omitempty"`
	LogTruncated bool           `json:"log_truncated,omitempty"`
	LogSource    string         `json:"log_source,omitempty"`

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
//
// v2 起 node 不再发送完整单帧,而是拆成 ImportReportChunkPayload 分页传输;
// 该类型继续保留,用于 panel 侧重组收齐后统一落库。
type ImportReportPayload struct {
	State            DesiredState `json:"state"`
	CredentialCount  int          `json:"credential_count"`
	BucketCount      int          `json:"bucket_count"`
	WebhookCount     int          `json:"webhook_count"`
	LocalContentHash string       `json:"local_content_hash"`
}

// ImportReportChunkPayload 是 v2 引入的 import 报告分页单元。
// node 把本地业务配置按资源类型切成多块,每块序列化后 ≤ 512 KiB;
// panel 按 request_id 重组,收齐 total 块后走既有导入落库路径。
type ImportReportChunkPayload struct {
	RequestID string `json:"request_id"`
	Seq       int    `json:"seq"`
	Total     int    `json:"total"`
	// 每块只携带一类资源的一段;三类资源共用 Seq/Total 编号空间
	Credentials []DesiredCredential `json:"credentials,omitempty"`
	Buckets     []DesiredBucket     `json:"buckets,omitempty"`
	Webhooks    []DesiredWebhook    `json:"webhooks,omitempty"`
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
