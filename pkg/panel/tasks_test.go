package panel

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

func TestTaskDispatchOnlyToOnlineNode(t *testing.T) {
	gdb := openTestDB(t)
	hub := NewHub()
	orch := NewTaskOrchestrator(gdb, hub, 10*time.Second)

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}

	// Node is not online (not in hub); dispatch must fail with ErrNodeOffline.
	_, err := orch.Dispatch(context.Background(), node.ID, controlproto.TaskLogQuery, controlproto.TaskParams{}, "admin")
	if err != ErrNodeOffline {
		t.Fatalf("expected ErrNodeOffline, got %v", err)
	}
}

func TestTaskDispatchAndResult(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	orch := NewTaskOrchestrator(gdb, hub, 10*time.Second)

	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: hub})
	node, ws := dialTestNode(t, gdb, ca, hub, ts)

	// Dispatch a log query task.
	ctx := context.Background()
	keyword := "private-search-sentinel"
	taskID, err := orch.Dispatch(ctx, node.ID, controlproto.TaskLogQuery, controlproto.TaskParams{Limit: 10, Keyword: keyword}, "admin")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Node receives the task frame.
	taskEnv := readEnv(t, ctx, ws)
	if taskEnv.Type != controlproto.TypeTask {
		t.Fatalf("expected task, got %s", taskEnv.Type)
	}
	var taskPayload controlproto.TaskPayload
	if err := taskEnv.DecodePayload(&taskPayload); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if taskPayload.TaskID != taskID {
		t.Fatalf("task_id = %s, want %s", taskPayload.TaskID, taskID)
	}
	var persisted Task
	if err := gdb.Where("task_id = ?", taskID).First(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted.Params, keyword) || !strings.Contains(persisted.Params, `"keyword_provided":true`) {
		t.Fatalf("persisted params were not redacted: %s", persisted.Params)
	}

	// Node reports success result.
	sendEnv(t, ctx, ws, controlproto.TypeTaskResult, taskID, controlproto.TaskResultPayload{
		TaskID: taskID,
		Type:   controlproto.TaskLogQuery,
		State:  controlproto.TaskStateSuccess,
		Result: controlproto.TaskResult{
			LogEntries: []controlproto.TaskLogEntry{{Time: "2026-07-25T10:00:00Z", Level: "INFO", Msg: "line1"}},
			LogLines:   []string{"line1", "line2"}, LogSource: "ring",
		},
	})

	// Panel persists the result.
	waitFor(t, func() bool {
		task, err := orch.GetTask(taskID)
		return err == nil && task.State == string(controlproto.TaskStateSuccess)
	})
}

func TestTaskTimeoutReleasesSlotAndIgnoresLateResult(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	orch := NewTaskOrchestrator(gdb, hub, 40*time.Millisecond)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: hub})
	node, ws := dialTestNode(t, gdb, ca, hub, ts)

	taskID, err := orch.Dispatch(context.Background(), node.ID, controlproto.TaskLogQuery, controlproto.TaskParams{}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	readEnv(t, context.Background(), ws)
	waitFor(t, func() bool {
		task, err := orch.GetTask(taskID)
		return err == nil && task.State == string(controlproto.TaskStateFailed) && strings.Contains(task.Error, "timed out")
	})
	conn, ok := hub.Get(node.ID)
	if !ok {
		t.Fatal("node disconnected during timeout test")
	}
	if conn.inFlightCount() != 0 {
		t.Fatalf("in-flight count after timeout = %d", conn.inFlightCount())
	}

	sendEnv(t, context.Background(), ws, controlproto.TypeTaskResult, taskID, controlproto.TaskResultPayload{
		TaskID: taskID, Type: controlproto.TaskLogQuery, State: controlproto.TaskStateSuccess,
		Result: controlproto.TaskResult{LogLines: []string{"late"}},
	})
	time.Sleep(50 * time.Millisecond)
	task, err := orch.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != string(controlproto.TaskStateFailed) || !strings.Contains(task.Error, "timed out") || strings.Contains(task.ResultJSON, "late") {
		t.Fatalf("late result overwrote timeout: %+v", task)
	}
}

func TestTaskResultTypeMismatchDoesNotReleaseSlot(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	orch := NewTaskOrchestrator(gdb, hub, 10*time.Second)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: hub})
	node, ws := dialTestNode(t, gdb, ca, hub, ts)

	taskID, err := orch.Dispatch(context.Background(), node.ID, controlproto.TaskLogQuery, controlproto.TaskParams{}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	readEnv(t, context.Background(), ws)
	conn, ok := hub.Get(node.ID)
	if !ok {
		t.Fatal("node disconnected during task result validation test")
	}
	if conn.inFlightCount() != 1 {
		t.Fatalf("in-flight count before result = %d", conn.inFlightCount())
	}

	sendEnv(t, context.Background(), ws, controlproto.TypeTaskResult, taskID, controlproto.TaskResultPayload{
		TaskID: taskID, Type: controlproto.TaskStorageScan, State: controlproto.TaskStateSuccess,
	})
	time.Sleep(50 * time.Millisecond)
	task, err := orch.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != string(controlproto.TaskStateRunning) {
		t.Fatalf("mismatched result changed task state to %s", task.State)
	}
	if conn.inFlightCount() != 1 {
		t.Fatalf("mismatched result released in-flight slot, count=%d", conn.inFlightCount())
	}

	sendEnv(t, context.Background(), ws, controlproto.TypeTaskResult, taskID, controlproto.TaskResultPayload{
		TaskID: taskID, Type: controlproto.TaskLogQuery, State: controlproto.TaskStateSuccess,
	})
	waitFor(t, func() bool {
		task, err := orch.GetTask(taskID)
		return err == nil && task.State == string(controlproto.TaskStateSuccess)
	})
	if conn.inFlightCount() != 0 {
		t.Fatalf("valid result did not release in-flight slot, count=%d", conn.inFlightCount())
	}
}

func TestTaskBackpressureRejectsTooManyInFlight(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	orch := NewTaskOrchestrator(gdb, hub, 10*time.Second)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: hub})
	node, ws := dialTestNode(t, gdb, ca, hub, ts)

	ctx := context.Background()
	const maxInFlight = DefaultMaxInFlightTasks

	// Saturate the in-flight window.
	taskIDs := make([]string, maxInFlight)
	for i := 0; i < maxInFlight; i++ {
		tid, err := orch.Dispatch(ctx, node.ID, controlproto.TaskLogQuery, controlproto.TaskParams{}, "admin")
		if err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
		taskIDs[i] = tid
		// Drain the task frame so the send doesn't block.
		readEnv(t, ctx, ws)
	}

	// The next dispatch must be rejected with backpressure.
	_, err := orch.Dispatch(ctx, node.ID, controlproto.TaskLogQuery, controlproto.TaskParams{}, "admin")
	if err != ErrTooManyInFlight {
		t.Fatalf("expected ErrTooManyInFlight, got %v", err)
	}

	// Ack one task to free a slot.
	sendEnv(t, ctx, ws, controlproto.TypeTaskResult, taskIDs[0], controlproto.TaskResultPayload{
		TaskID: taskIDs[0], Type: controlproto.TaskLogQuery, State: controlproto.TaskStateSuccess,
	})
	waitFor(t, func() bool {
		task, _ := orch.GetTask(taskIDs[0])
		return task.State == string(controlproto.TaskStateSuccess)
	})

	// Now a new dispatch must succeed.
	_, err = orch.Dispatch(ctx, node.ID, controlproto.TaskLogQuery, controlproto.TaskParams{}, "admin")
	if err != nil {
		t.Fatalf("dispatch after ack: %v", err)
	}
}

func TestTaskInterruptedOnDisconnect(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	orch := NewTaskOrchestrator(gdb, hub, 10*time.Second)

	// Wire the OnDisconnected hook to fail in-flight tasks.
	ts := NewTransportServer(TransportDeps{
		DB:             gdb,
		CA:             ca,
		Hub:            hub,
		OnDisconnected: orch.FailInFlightForConn,
	})
	node, ws := dialTestNode(t, gdb, ca, hub, ts)

	ctx := context.Background()
	taskID, err := orch.Dispatch(ctx, node.ID, controlproto.TaskLogQuery, controlproto.TaskParams{}, "admin")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Node receives the task but does NOT send a result yet.
	readEnv(t, ctx, ws)

	// Close the connection while the task is in flight.
	ws.Close(websocket.StatusNormalClosure, "test disconnect")
	time.Sleep(100 * time.Millisecond) // let the serve loop unregister

	// The task must be marked as "unknown" (not failed, not success).
	task, err := orch.GetTask(taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.State != string(controlproto.TaskStateUnknown) {
		t.Fatalf("task state = %s, want unknown", task.State)
	}
	if !strings.Contains(task.Error, "connection closed") {
		t.Fatalf("task error should mention connection closed, got %q", task.Error)
	}
}

func TestTaskUnsupportedTypeRejected(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	orch := NewTaskOrchestrator(gdb, hub, 10*time.Second)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: hub})
	node, _ := dialTestNode(t, gdb, ca, hub, ts)

	_, err := orch.Dispatch(context.Background(), node.ID, controlproto.TaskType("unsupported"), controlproto.TaskParams{}, "admin")
	if err != ErrUnsupportedTaskType {
		t.Fatalf("expected ErrUnsupportedTaskType, got %v", err)
	}
}

// dialTestNode stands up a live control-plane connection for a test node. It
// returns the node record and the client-side websocket. The connection is
// registered in the hub so task dispatch can reach it.
func dialTestNode(t *testing.T, gdb *gorm.DB, ca *CA, hub *Hub, ts *TransportServer) (Node, *websocket.Conn) {
	t.Helper()
	node := Node{DisplayName: "test-node", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	clientCert, fingerprint := issueNodeClientCert(t, ca, node.ID)
	if err := gdb.Create(&NodeCert{
		NodeID: node.ID, Fingerprint: fingerprint, Serial: "1",
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}).Error; err != nil {
		t.Fatalf("store cert: %v", err)
	}

	srv := startTestServer(t, ts, ca)
	wsURL := "wss" + strings.TrimPrefix(srv.URL, "https") + "/agent"

	clientTLS := &tls.Config{Certificates: []tls.Certificate{clientCert}, InsecureSkipVerify: true}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		closeTestNode(t, gdb, hub, node.ID, ws)
	})

	// Complete handshake: node sends hello, panel replies hello_ack.
	sendEnv(t, ctx, ws, controlproto.TypeHello, "h1", controlproto.HelloPayload{
		ProtocolVersion: controlproto.ProtocolVersion,
		NodeID:          fmt.Sprintf("%d", node.ID),
		AppliedVersion:  0,
	})
	readEnv(t, ctx, ws) // consume hello_ack

	// Node is now online and registered.
	waitFor(t, func() bool { return hub.IsOnline(node.ID) })
	return node, ws
}

// closeTestNode waits for the complete server-side disconnect sequence. The
// Hub unregisters before the final NodeState write, so waiting only for Hub
// status can race SQLite WAL cleanup under Go 1.21.
func closeTestNode(t *testing.T, gdb *gorm.DB, hub *Hub, nodeID uint, ws *websocket.Conn) {
	t.Helper()
	_ = ws.Close(websocket.StatusNormalClosure, "test cleanup")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hub.IsOnline(nodeID) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		var state NodeState
		if err := gdb.Where("node_id = ?", nodeID).First(&state).Error; err == nil && !state.Online {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if hub.IsOnline(nodeID) {
		t.Errorf("node %d remained online during test cleanup", nodeID)
		return
	}
	t.Errorf("node %d state write did not settle during test cleanup", nodeID)
}
