package panel

import (
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// insertTask 直接落一条指定状态的任务,用于构造 markState 的交错场景。
func insertTask(t *testing.T, o *TaskOrchestrator, taskID, state, result, errMsg string) {
	t.Helper()
	record := Task{
		TaskID:     taskID,
		NodeID:     1,
		Type:       string(controlproto.TaskStorageScan),
		State:      state,
		ResultJSON: result,
		Error:      errMsg,
		CreatedAt:  nowUTC(),
		UpdatedAt:  nowUTC(),
	}
	if err := o.db.Create(&record).Error; err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

func loadTask(t *testing.T, o *TaskOrchestrator, taskID string) Task {
	t.Helper()
	var task Task
	if err := o.db.Where("task_id = ?", taskID).First(&task).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	return task
}

// R4(AC3):节点的执行结果可能先于 Dispatch 的 markState(running) 落库。迟到的
// markState 必须被状态守卫挡住,不能把已经写好的终态、result 和 error 冲掉。
func TestMarkStateDoesNotOverwriteTerminalStateOrClearError(t *testing.T) {
	gdb := openTestDB(t)
	orchestrator := NewTaskOrchestrator(gdb, NewHub(), 0)

	// 结果已经先落库:success + result,以及节点上报的错误说明。
	insertTask(t, orchestrator, "task-raced", string(controlproto.TaskStateSuccess),
		`{"scanned":42}`, "partial: 1 object skipped")

	// 迟到的 running 迁移。
	orchestrator.markState("task-raced", controlproto.TaskStateRunning, "", "")

	task := loadTask(t, orchestrator, "task-raced")
	if task.State != string(controlproto.TaskStateSuccess) {
		t.Fatalf("state = %q, want success preserved (late markState must not move a terminal task back)", task.State)
	}
	if task.ResultJSON != `{"scanned":42}` {
		t.Fatalf("result_json = %q, want the already-persisted result preserved", task.ResultJSON)
	}
	if task.Error != "partial: 1 object skipped" {
		t.Fatalf("error = %q, want the reported error preserved (markState must not blank it)", task.Error)
	}
}

// R4:守卫不能挡住正常迁移——pending -> running 以及 pending -> failed(下发失败回滚)
// 都必须照常生效,否则任务会永远停在 pending。
func TestMarkStateStillAdvancesPendingTasks(t *testing.T) {
	gdb := openTestDB(t)
	orchestrator := NewTaskOrchestrator(gdb, NewHub(), 0)

	insertTask(t, orchestrator, "task-running", string(controlproto.TaskStatePending), "", "")
	orchestrator.markState("task-running", controlproto.TaskStateRunning, "", "")
	if got := loadTask(t, orchestrator, "task-running").State; got != string(controlproto.TaskStateRunning) {
		t.Fatalf("state = %q, want running", got)
	}

	insertTask(t, orchestrator, "task-failed", string(controlproto.TaskStatePending), "", "")
	orchestrator.markState("task-failed", controlproto.TaskStateFailed, "", "send task: connection closed")
	failed := loadTask(t, orchestrator, "task-failed")
	if failed.State != string(controlproto.TaskStateFailed) {
		t.Fatalf("state = %q, want failed", failed.State)
	}
	if failed.Error != "send task: connection closed" {
		t.Fatalf("error = %q, want the dispatch failure reason recorded", failed.Error)
	}
}
