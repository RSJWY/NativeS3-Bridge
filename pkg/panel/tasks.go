package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// DefaultTaskTimeout bounds how long the panel waits for a task_result before
// giving up on a dispatched task. One-shot tasks are interactive admin actions;
// they must not hang the orchestrator indefinitely.
const DefaultTaskTimeout = 60 * time.Second

// Task orchestration errors surfaced to admin callers.
var (
	// ErrNodeOffline is returned when a task is dispatched to a node that has no
	// live control-plane connection. One-shot tasks are NEVER queued for a future
	// reconnect (design §5.3): the admin must retry when the node is back online.
	ErrNodeOffline = errors.New("node is offline")
	// ErrTooManyInFlight is returned when the node already has the maximum number
	// of unacknowledged tasks (backpressure). The admin must wait and retry.
	ErrTooManyInFlight = errors.New("node has too many tasks in flight")
	// ErrUnsupportedTaskType guards against dispatching anything outside the
	// predefined operation set (no generic command channel).
	ErrUnsupportedTaskType = errors.New("unsupported task type")
)

// TaskOrchestrator dispatches one-shot tasks to online nodes and records their
// lifecycle in the tasks table. It enforces the design's task rules: online-only
// dispatch, per-task-id idempotency, in-flight backpressure, timeout, and
// interruption handling (a task whose connection drops is marked failed/unknown
// and never silently retried).
type TaskOrchestrator struct {
	db      *gorm.DB
	hub     *Hub
	timeout time.Duration
}

// NewTaskOrchestrator builds an orchestrator over the panel DB and connection
// hub. A non-positive timeout falls back to DefaultTaskTimeout.
func NewTaskOrchestrator(db *gorm.DB, hub *Hub, timeout time.Duration) *TaskOrchestrator {
	if timeout <= 0 {
		timeout = DefaultTaskTimeout
	}
	return &TaskOrchestrator{db: db, hub: hub, timeout: timeout}
}

// isSupportedTaskType reports whether t is one of the predefined operations.
func isSupportedTaskType(t controlproto.TaskType) bool {
	switch t {
	case controlproto.TaskLogQuery, controlproto.TaskStorageScan, controlproto.TaskStorageReconcileApply:
		return true
	default:
		return false
	}
}

// Dispatch sends a one-shot task to an online node. It persists the task row,
// reserves an in-flight slot for backpressure, and sends the task frame. The
// returned taskID is the idempotency key the node dedupes on. Dispatch does NOT
// wait for the result; the node reports it asynchronously via task_result, which
// the transport server records (see handleTaskResult).
//
// Errors: ErrUnsupportedTaskType, ErrNodeOffline, ErrTooManyInFlight, or a DB /
// transport error. On any error no in-flight slot is leaked.
func (o *TaskOrchestrator) Dispatch(ctx context.Context, nodeID uint, taskType controlproto.TaskType, params controlproto.TaskParams, createdBy string) (string, error) {
	if !isSupportedTaskType(taskType) {
		return "", ErrUnsupportedTaskType
	}
	conn, ok := o.hub.Get(nodeID)
	if !ok {
		return "", ErrNodeOffline
	}

	taskID := uuid.NewString()
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("marshal task params: %w", err)
	}

	// Persist the task as pending before sending so a crash between send and
	// result still leaves an auditable record.
	record := Task{
		TaskID:    taskID,
		NodeID:    nodeID,
		Type:      string(taskType),
		Params:    string(paramsJSON),
		State:     string(controlproto.TaskStatePending),
		CreatedBy: createdBy,
		CreatedAt: nowUTC(),
		UpdatedAt: nowUTC(),
	}
	if err := o.db.Create(&record).Error; err != nil {
		return "", fmt.Errorf("persist task: %w", err)
	}

	// Reserve an in-flight slot (backpressure). If the node is saturated, roll the
	// task back to a terminal failed state so it is not left dangling as pending.
	if !conn.reserveTask(taskID) {
		o.markState(taskID, controlproto.TaskStateFailed, "", "node has too many tasks in flight")
		return "", ErrTooManyInFlight
	}

	timeoutMS := int64(o.timeout / time.Millisecond)
	payload := controlproto.TaskPayload{
		TaskID:    taskID,
		Type:      taskType,
		Params:    params,
		TimeoutMS: timeoutMS,
		CreatedBy: createdBy,
	}
	sendCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := conn.sendMessage(sendCtx, controlproto.TypeTask, taskID, payload); err != nil {
		// Send failed: release the slot and mark the task failed. The connection is
		// likely dead; the node never received the task, so failed (not unknown).
		conn.releaseTask(taskID)
		o.markState(taskID, controlproto.TaskStateFailed, "", fmt.Sprintf("send task: %v", err))
		return "", fmt.Errorf("send task: %w", err)
	}

	o.markState(taskID, controlproto.TaskStateRunning, "", "")
	o.audit("task_dispatch", nodeID, taskID, string(taskType), createdBy)
	return taskID, nil
}

// GetTask returns the current persisted state of a task by ID.
func (o *TaskOrchestrator) GetTask(taskID string) (Task, error) {
	var task Task
	if err := o.db.Where("task_id = ?", taskID).First(&task).Error; err != nil {
		return Task{}, err
	}
	return task, nil
}

// FailInFlightForConn marks every task still in flight on a dropped connection
// as "unknown": the panel dispatched them but the connection closed before a
// result arrived, so it cannot know whether the node executed them. High-risk
// operations (reconcile apply) must NOT be silently retried — the admin has to
// re-confirm (design §5.3). This is called from the serve loop on disconnect.
func (o *TaskOrchestrator) FailInFlightForConn(conn *AgentConn) {
	for _, taskID := range conn.inFlightTasks() {
		conn.releaseTask(taskID)
		// Only transition tasks that are still non-terminal. A result that raced in
		// just before close has already set a terminal state; don't clobber it.
		res := o.db.Model(&Task{}).
			Where("task_id = ? AND state IN ?", taskID, []string{
				string(controlproto.TaskStatePending),
				string(controlproto.TaskStateRunning),
			}).
			Updates(map[string]any{
				"state":      string(controlproto.TaskStateUnknown),
				"error":      "connection closed before result; result unknown, re-confirm before retry",
				"updated_at": nowUTC(),
			})
		if res.Error == nil && res.RowsAffected > 0 {
			o.audit("task_interrupted", conn.NodeID, taskID, "", "control-plane")
		}
	}
}

// markState updates a task's terminal/transitional state. result and errMsg are
// optional; empty strings leave the respective columns unchanged only for the
// error (result is written as-is). Kept small and side-effect-local.
func (o *TaskOrchestrator) markState(taskID string, state controlproto.TaskState, resultJSON, errMsg string) {
	updates := map[string]any{
		"state":      string(state),
		"updated_at": nowUTC(),
	}
	if resultJSON != "" {
		updates["result_json"] = resultJSON
	}
	updates["error"] = errMsg
	_ = o.db.Model(&Task{}).Where("task_id = ?", taskID).Updates(updates).Error
}

func (o *TaskOrchestrator) audit(action string, nodeID uint, resource, detail, source string) {
	if source == "" {
		source = "admin"
	}
	entry := AuditLog{
		TS:             nowUTC(),
		Action:         action,
		TargetNode:     nodeID,
		TargetResource: resource,
		Result:         detail,
		Source:         source,
	}
	_ = o.db.Create(&entry).Error
}
