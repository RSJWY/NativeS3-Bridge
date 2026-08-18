package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	// ErrInvalidTaskParams is returned before persistence/dispatch when a
	// predefined task's typed parameters violate its bounded contract.
	ErrInvalidTaskParams = errors.New("invalid task params")
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
	if taskType == controlproto.TaskLogQuery {
		if _, err := controlproto.ParseLogQuery(params); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidTaskParams, err)
		}
	}
	conn, ok := o.hub.Get(nodeID)
	if !ok {
		return "", ErrNodeOffline
	}

	taskID := uuid.NewString()
	paramsJSON, err := json.Marshal(persistedTaskParams(taskType, params))
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
	go o.expireTask(conn, taskID, nodeID)
	return taskID, nil
}

func persistedTaskParams(taskType controlproto.TaskType, params controlproto.TaskParams) any {
	if taskType != controlproto.TaskLogQuery {
		return params
	}
	return struct {
		Since           string `json:"since,omitempty"`
		Until           string `json:"until,omitempty"`
		Level           string `json:"level,omitempty"`
		Limit           int    `json:"limit,omitempty"`
		KeywordProvided bool   `json:"keyword_provided,omitempty"`
	}{
		Since: strings.TrimSpace(params.Since), Until: strings.TrimSpace(params.Until),
		Level: strings.TrimSpace(params.Level), Limit: params.Limit,
		KeywordProvided: strings.TrimSpace(params.Keyword) != "",
	}
}

func (o *TaskOrchestrator) expireTask(conn *AgentConn, taskID string, nodeID uint) {
	timer := time.NewTimer(o.timeout)
	defer timer.Stop()
	<-timer.C
	conn.releaseTask(taskID)
	res := o.db.Model(&Task{}).
		Where("task_id = ? AND node_id = ? AND state IN ?", taskID, nodeID, []string{
			string(controlproto.TaskStatePending), string(controlproto.TaskStateRunning),
		}).
		Updates(map[string]any{
			"state": string(controlproto.TaskStateFailed), "error": "task timed out waiting for node result", "updated_at": nowUTC(),
		})
	if res.Error == nil && res.RowsAffected > 0 {
		o.audit("task_timeout", nodeID, taskID, string(controlproto.TaskStateFailed), "control-plane")
	}
}

// GetTask returns the current persisted state of a task by ID.
func (o *TaskOrchestrator) GetTask(taskID string) (Task, error) {
	var task Task
	if err := o.db.Where("task_id = ?", taskID).First(&task).Error; err != nil {
		return Task{}, err
	}
	return task, nil
}

// GetTaskForNode scopes task lookup to the node encoded in the REST path.
func (o *TaskOrchestrator) GetTaskForNode(nodeID uint, taskID string) (Task, error) {
	var task Task
	if err := o.db.Where("node_id = ? AND task_id = ?", nodeID, taskID).First(&task).Error; err != nil {
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

// markState updates a task's transitional/terminal state. It only ever moves a
// task forward out of `pending`: 节点的执行结果可能先于本地 markState 落库
// (handleTaskResult 与 Dispatch 是两条并发路径),没有守卫时一次迟到的
// markState(running) 会把已经写好的 success/failed 终态连同 result 一起冲掉,
// 并把 error 列清空。三个调用点(Dispatch 的两处失败回滚与成功后的 running)
// 触发时任务都必然还是 pending,所以这个前置条件不会挡住任何正常迁移。
// 超时/断连路径不走这里:expireTask 与 FailInFlightForConn 各自带
// `state IN (pending, running)` 守卫的独立 UPDATE。
func (o *TaskOrchestrator) markState(taskID string, state controlproto.TaskState, resultJSON, errMsg string) {
	updates := map[string]any{
		"state":      string(state),
		"updated_at": nowUTC(),
	}
	if resultJSON != "" {
		updates["result_json"] = resultJSON
	}
	// 与 result_json 同口径:空错误信息表示"本次迁移没有错误要记",而不是"把之前
	// 记录的错误抹掉"——无条件清空会擦掉节点已上报的失败原因。
	if errMsg != "" {
		updates["error"] = errMsg
	}
	res := o.db.Model(&Task{}).
		Where("task_id = ? AND state = ?", taskID, string(controlproto.TaskStatePending)).
		Updates(updates)
	if res.Error != nil {
		slog.Error("task state update failed", "task", taskID, "state", state, "error", res.Error)
		return
	}
	if res.RowsAffected == 0 {
		// 竞态的正常结局:结果已经先落库了。保留既有终态,不是错误。
		slog.Info("task state update skipped; task already left pending",
			"task", taskID, "attempted_state", state)
	}
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
