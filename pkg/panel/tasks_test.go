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
	taskID, err := orch.Dispatch(ctx, node.ID, controlproto.TaskLogQuery, controlproto.TaskParams{Limit: 10}, "admin")
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

	// Node reports success result.
	sendEnv(t, ctx, ws, controlproto.TypeTaskResult, taskID, controlproto.TaskResultPayload{
		TaskID: taskID,
		Type:   controlproto.TaskLogQuery,
		State:  controlproto.TaskStateSuccess,
		Result: controlproto.TaskResult{LogLines: []string{"line1", "line2"}},
	})

	// Panel persists the result.
	waitFor(t, func() bool {
		task, err := orch.GetTask(taskID)
		return err == nil && task.State == string(controlproto.TaskStateSuccess)
	})
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
		_ = ws.Close(websocket.StatusNormalClosure, "test cleanup")
		// Closing the client only initiates server-side teardown. Wait until the
		// serve goroutine has persisted online=false and unregistered the
		// connection before t.TempDir removes the SQLite directory. Without this
		// barrier Go 1.21 can intermittently report a non-empty temp directory or
		// a read-only database during cleanup.
		deadline := time.Now().Add(3 * time.Second)
		for hub.IsOnline(node.ID) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if hub.IsOnline(node.ID) {
			t.Errorf("node %d remained online during test cleanup", node.ID)
		}
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
