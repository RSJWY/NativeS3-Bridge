package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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
	maxLogFieldBytes    = 16 << 10
	maxLogAttrKeyBytes  = 256
	maxLogAttrsPerEntry = 64
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
		return r.runLogQuery(ctx, task, base)
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
func (r *LocalTaskRunner) runLogQuery(ctx context.Context, task controlproto.TaskPayload, base controlproto.TaskResultPayload) controlproto.TaskResultPayload {
	if r.logRing == nil {
		return failedTask(base, "log ring is not configured")
	}
	spec, err := controlproto.ParseLogQuery(task.Params)
	if err != nil {
		return failedTask(base, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return failedTask(base, "log query cancelled")
	}

	// Fetch one extra to detect truncation without leaking more than the cap.
	entries := r.logRing.Query(logging.QueryOptions{
		Limit: spec.Limit + 1, Level: spec.Level, Query: spec.Keyword, Since: spec.Since, Until: spec.Until,
	})
	truncated := len(entries) > spec.Limit
	if len(entries) > spec.Limit {
		entries = entries[:spec.Limit]
	}
	result := controlproto.TaskResult{LogSource: "ring"}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return failedTask(base, "log query cancelled")
		}
		projected, entryTruncated := projectLogEntry(e)
		candidate := result
		candidate.LogEntries = append(append([]controlproto.TaskLogEntry(nil), result.LogEntries...), projected)
		candidate.LogLines = append(append([]string(nil), result.LogLines...), formatTaskLogEntry(projected))
		candidate.LogTruncated = truncated || entryTruncated
		encoded, err := json.Marshal(candidate)
		if err != nil || len(encoded) > controlproto.MaxLogQueryResultBytes {
			truncated = true
			break
		}
		result = candidate
		if entryTruncated {
			truncated = true
		}
	}
	result.LogTruncated = truncated
	// Setting the final truncation flag adds a few bytes. Remove the oldest
	// included entries until the complete result remains within the wire budget.
	for len(result.LogEntries) > 0 {
		encoded, err := json.Marshal(result)
		if err == nil && len(encoded) <= controlproto.MaxLogQueryResultBytes {
			break
		}
		result.LogEntries = result.LogEntries[:len(result.LogEntries)-1]
		result.LogLines = result.LogLines[:len(result.LogLines)-1]
		result.LogTruncated = true
	}
	base.State = controlproto.TaskStateSuccess
	base.Result = result
	return base
}

func projectLogEntry(entry logging.Entry) (controlproto.TaskLogEntry, bool) {
	message, truncated := truncateLogString(entry.Message, maxLogFieldBytes)
	level, levelTruncated := truncateLogString(strings.ToUpper(entry.Level), controlproto.MaxLogQueryLevelBytes)
	truncated = truncated || levelTruncated
	projected := controlproto.TaskLogEntry{Level: level, Msg: message}
	if !entry.Time.IsZero() {
		projected.Time = entry.Time.UTC().Format(time.RFC3339Nano)
	}
	if len(entry.Attrs) > 0 {
		projected.Attrs = make(map[string]string)
		keys := make([]string, 0, len(entry.Attrs))
		for key := range entry.Attrs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		count := 0
		for _, key := range keys {
			if logging.IsSensitiveKey(key) {
				continue
			}
			if count >= maxLogAttrsPerEntry {
				truncated = true
				break
			}
			cleanKey, keyTruncated := truncateLogString(key, maxLogAttrKeyBytes)
			cleanValue, valueTruncated := truncateLogString(fmt.Sprint(entry.Attrs[key]), maxLogFieldBytes)
			if cleanKey == "" {
				continue
			}
			projected.Attrs[cleanKey] = cleanValue
			truncated = truncated || keyTruncated || valueTruncated
			count++
		}
		if len(projected.Attrs) == 0 {
			projected.Attrs = nil
		}
	}
	return projected, truncated
}

func formatTaskLogEntry(entry controlproto.TaskLogEntry) string {
	var b strings.Builder
	if entry.Time != "" {
		b.WriteString(entry.Time)
		b.WriteByte(' ')
	}
	if entry.Level != "" {
		b.WriteString(entry.Level)
		b.WriteByte(' ')
	}
	b.WriteString(entry.Msg)
	return b.String()
}

func truncateLogString(value string, maxBytes int) (string, bool) {
	if len(value) <= maxBytes {
		return value, false
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end], true
}

func failedTask(base controlproto.TaskResultPayload, message string) controlproto.TaskResultPayload {
	base.State = controlproto.TaskStateFailed
	base.Error = message
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
