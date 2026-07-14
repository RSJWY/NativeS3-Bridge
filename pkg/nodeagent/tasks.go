package nodeagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/logging"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
	"gorm.io/gorm"
)

// Result-size ceilings for one-shot tasks. The control channel is not a bulk
// data path (design §4.5 backpressure): log queries are bounded in count and
// scan results are compact summaries, never raw object listings.
const (
	maxLogQueryLimit     = 500
	defaultLogQueryLimit = 200
)

// LocalTaskRunner executes the predefined one-shot tasks on the node using
// node-local resources only: the in-memory log ring for log queries and the
// storage reconcile logic for scan/apply. It performs NO arbitrary command
// execution — only the three predefined task types (design §5.1).
type LocalTaskRunner struct {
	db             *gorm.DB
	logRing        *logging.Ring
	dataRoot       string
	metadataSuffix string
	invalidator    CredentialInvalidator
}

// NewLocalTaskRunner builds a task runner over node-local resources.
func NewLocalTaskRunner(gdb *gorm.DB, logRing *logging.Ring, dataRoot, metadataSuffix string, invalidator CredentialInvalidator) *LocalTaskRunner {
	return &LocalTaskRunner{db: gdb, logRing: logRing, dataRoot: dataRoot, metadataSuffix: metadataSuffix, invalidator: invalidator}
}

// Run dispatches a task to its predefined handler. Unknown task types are
// rejected (no generic command channel).
func (r *LocalTaskRunner) Run(ctx context.Context, task controlproto.TaskPayload) controlproto.TaskResultPayload {
	base := controlproto.TaskResultPayload{TaskID: task.TaskID, Type: task.Type}
	switch task.Type {
	case controlproto.TaskLogQuery:
		return r.runLogQuery(task, base)
	case controlproto.TaskStorageScan:
		return r.runStorageScan(task, base, false)
	case controlproto.TaskStorageReconcileApply:
		return r.runStorageScan(task, base, true)
	default:
		base.State = controlproto.TaskStateFailed
		base.Error = fmt.Sprintf("unsupported task type %q", task.Type)
		return base
	}
}

// runLogQuery returns a bounded slice of recent log lines from the in-memory
// ring, filtered by keyword. The count is capped so the control connection is
// never used as an unbounded log stream.
func (r *LocalTaskRunner) runLogQuery(task controlproto.TaskPayload, base controlproto.TaskResultPayload) controlproto.TaskResultPayload {
	if r.logRing == nil {
		base.State = controlproto.TaskStateFailed
		base.Error = "log ring is not configured"
		return base
	}
	limit := task.Params.Limit
	if limit <= 0 {
		limit = defaultLogQueryLimit
	}
	if limit > maxLogQueryLimit {
		limit = maxLogQueryLimit
	}
	// Fetch one extra to detect truncation without leaking more than the cap.
	entries := r.logRing.Snapshot(limit+1, "", task.Params.Keyword)
	truncated := false
	if len(entries) > limit {
		entries = entries[:limit]
		truncated = true
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, formatLogEntry(e))
	}
	base.State = controlproto.TaskStateSuccess
	base.Result = controlproto.TaskResult{LogLines: lines, LogTruncated: truncated}
	return base
}

// runStorageScan runs a bucket reconcile in preview (apply=false) or apply mode.
// Preview never mutates data; apply deletes orphan sidecars and updates bound
// credential UsedBytes, exactly matching the webadmin reconcile semantics. Apply
// is a high-risk write; idempotency is enforced by the caller via task_id.
func (r *LocalTaskRunner) runStorageScan(task controlproto.TaskPayload, base controlproto.TaskResultPayload, apply bool) controlproto.TaskResultPayload {
	bucket := strings.TrimSpace(task.Params.Bucket)
	if bucket == "" {
		base.State = controlproto.TaskStateFailed
		base.Error = "bucket is required"
		return base
	}
	if r.dataRoot == "" {
		base.State = controlproto.TaskStateFailed
		base.Error = "storage reconcile is not configured"
		return base
	}
	report, err := storage.ReconcileBucket(r.dataRoot, bucket, r.metadataSuffix)
	if err != nil {
		base.State = controlproto.TaskStateFailed
		base.Error = err.Error()
		return base
	}
	result := controlproto.TaskResult{
		Bucket:             bucket,
		Applied:            apply,
		ObjectCount:        report.ObjectCount,
		ScannedBytes:       report.ScannedBytes,
		OrphanSidecarCount: report.OrphanSidecarCount(),
	}

	if apply {
		deleted, delErr := report.DeleteOrphanSidecars()
		if delErr != nil {
			base.State = controlproto.TaskStateFailed
			base.Error = fmt.Sprintf("delete orphan sidecars: %v", delErr)
			return base
		}
		result.OrphansDeleted = deleted

		var credentials []dbpkg.Credential
		if err := r.db.Where("bucket = ? AND bucket <> ''", bucket).Order("id ASC").Find(&credentials).Error; err != nil {
			base.State = controlproto.TaskStateFailed
			base.Error = fmt.Sprintf("query bound credentials: %v", err)
			return base
		}
		if err := r.db.Transaction(func(tx *gorm.DB) error {
			for i := range credentials {
				if err := tx.Model(&credentials[i]).Update("used_bytes", report.ScannedBytes).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			base.State = controlproto.TaskStateFailed
			base.Error = fmt.Sprintf("update credentials: %v", err)
			return base
		}
		result.CredentialsUpdated = len(credentials)
		if r.invalidator != nil {
			for _, cred := range credentials {
				r.invalidator.Invalidate(cred.AccessKey)
			}
		}
	}

	base.State = controlproto.TaskStateSuccess
	base.Result = result
	return base
}

func formatLogEntry(e logging.Entry) string {
	var b strings.Builder
	if !e.Time.IsZero() {
		b.WriteString(e.Time.Format("2006-01-02T15:04:05Z07:00"))
		b.WriteByte(' ')
	}
	if e.Level != "" {
		b.WriteString(e.Level)
		b.WriteByte(' ')
	}
	b.WriteString(e.Message)
	return b.String()
}
