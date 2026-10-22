package panel

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"github.com/RSJWY/NativeS3-Bridge/pkg/logging"
)

func TestAdminEventBusFanout(t *testing.T) {
	bus := NewAdminEventBus()
	a := NewAdminAPI(nil, nil, nil, nil, nil, nil, nil, nil)
	a.SetAdminEventBus(bus)
	if a.events != bus {
		t.Fatal("SetAdminEventBus did not wire the bus")
	}

	first := bus.subscribe()
	second := bus.subscribe()
	defer bus.unsubscribe(second)

	bus.Publish(AdminEvent{Type: AdminEventNodeOnline, NodeID: 7})
	bus.Publish(AdminEvent{Type: AdminEventTaskUpdated, NodeID: 7, TaskID: "t1", State: "running"})

	for i := 0; i < 2; i++ {
		select {
		case ev := <-first.events:
			if ev.NodeID != 7 {
				t.Fatalf("event node = %d, want 7", ev.NodeID)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber missed event %d", i)
		}
		select {
		case <-second.events:
		case <-time.After(time.Second):
			t.Fatalf("second subscriber missed event %d", i)
		}
	}
}

func TestAdminEventBusUnsubscribeStopsDelivery(t *testing.T) {
	bus := NewAdminEventBus()
	sub := bus.subscribe()
	bus.unsubscribe(sub)
	bus.unsubscribe(sub) // 幂等:流式 handler 的 defer 清理会再调一次

	bus.Publish(AdminEvent{Type: AdminEventNodeOffline, NodeID: 1})

	select {
	case ev := <-sub.events:
		t.Fatalf("unsubscribed subscriber received %+v", ev)
	case <-sub.done:
	case <-time.After(time.Second):
		t.Fatal("unsubscribe did not close the done channel")
	}
}

func TestAdminEventBusEvictsSlowSubscriber(t *testing.T) {
	bus := NewAdminEventBus()
	sub := bus.subscribe()

	for i := 0; i < sseSubscriberBuffer+1; i++ {
		bus.Publish(AdminEvent{Type: AdminEventNodeOnline, NodeID: uint(i)})
	}

	select {
	case <-sub.done:
	default:
		t.Fatal("slow subscriber was not evicted after overflowing its buffer")
	}
	// 摘除后总线仍可继续服务新订阅者,发布不再触达旧订阅者。
	fresh := bus.subscribe()
	defer bus.unsubscribe(fresh)
	bus.Publish(AdminEvent{Type: AdminEventNodeOnline, NodeID: 99})
	select {
	case ev := <-fresh.events:
		if ev.NodeID != 99 {
			t.Fatalf("fresh subscriber node = %d, want 99", ev.NodeID)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh subscriber missed event after eviction")
	}
}

// readSSEFrame 读取一个完整 SSE 帧(到空行为止)。
func readSSEFrame(t *testing.T, reader *bufio.Reader) (event string, data string) {
	t.Helper()
	event = ""
	data = ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE frame: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if event != "" || data != "" {
				return event, data
			}
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestAdminEventsHandlerStreamsEvents(t *testing.T) {
	bus := NewAdminEventBus()
	prevHeartbeat := sseHeartbeatInterval
	sseHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { sseHeartbeatInterval = prevHeartbeat })

	a := NewAdminAPI(nil, nil, nil, nil, nil, nil, nil, nil)
	a.SetAdminEventBus(bus)
	srv := httptest.NewServer(http.HandlerFunc(a.AdminEvents))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("x-accel-buffering = %q, want no", resp.Header.Get("X-Accel-Buffering"))
	}

	reader := bufio.NewReader(resp.Body)
	// 首帧 retry 提示代表订阅已生效,此后发布保证可被本连接收到。
	retry, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(retry, "retry: ") {
		t.Fatalf("first line = %q, err=%v, want retry hint", retry, err)
	}

	heartbeatSeen := false
	go func() {
		// 等订阅生效后发布;retry 帧已被上面同步读走,这里直接发布即可。
		bus.Publish(AdminEvent{Type: AdminEventNodeOnline, NodeID: 3})
		bus.Publish(AdminEvent{Type: AdminEventTaskUpdated, NodeID: 3, TaskID: "task-1", State: "success"})
	}()

	deadline := time.Now().Add(3 * time.Second)
	gotOnline := false
	var gotTask AdminEvent
	for time.Now().Before(deadline) && (!gotOnline || !heartbeatSeen) {
		event, data := readSSEFrame(t, reader)
		switch event {
		case "heartbeat":
			heartbeatSeen = true
		case AdminEventNodeOnline:
			gotOnline = true
			var ev AdminEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("decode online event: %v", err)
			}
			if ev.Type != AdminEventNodeOnline || ev.NodeID != 3 {
				t.Fatalf("online event = %+v", ev)
			}
		case AdminEventTaskUpdated:
			if err := json.Unmarshal([]byte(data), &gotTask); err != nil {
				t.Fatalf("decode task event: %v", err)
			}
			if gotTask.TaskID != "task-1" || gotTask.State != "success" {
				t.Fatalf("task event = %+v", gotTask)
			}
		}
	}
	if !gotOnline {
		t.Fatal("did not receive node_online event")
	}
	if gotTask.TaskID != "task-1" {
		t.Fatal("did not receive task_updated event")
	}
	if !heartbeatSeen {
		t.Fatal("did not receive heartbeat event")
	}
}

func TestAdminEventsHandlerRejectsNonGET(t *testing.T) {
	a := NewAdminAPI(nil, nil, nil, nil, nil, nil, nil, nil)
	a.SetAdminEventBus(NewAdminEventBus())
	rec := httptest.NewRecorder()
	a.AdminEvents(rec, httptest.NewRequest(http.MethodPost, "/api/admin/events", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestAdminEventsHandlerWithoutBus(t *testing.T) {
	a := NewAdminAPI(nil, nil, nil, nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	a.AdminEvents(rec, httptest.NewRequest(http.MethodGet, "/api/admin/events", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestAdminEventsRouteRequiresSession(t *testing.T) {
	gdb := openTestDB(t)
	key := make([]byte, masterKeyLen)
	for i := range key {
		key[i] = byte(i + 21)
	}
	cipher, err := NewSecretCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	server := newSSETestAdminServer(t, gdb, cipher)

	unauthorized := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/admin/events", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated events status = %d, want 401", unauthorized.Code)
	}
	if ct := unauthorized.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("unauthenticated request must not establish a stream, got content-type %q", ct)
	}
}

func TestTaskStateTransitionsPublishEvents(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	bus := NewAdminEventBus()
	orch := NewTaskOrchestrator(gdb, hub, 10*time.Second)
	orch.SetAdminEventBus(bus)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: hub, Events: bus})

	// 先订阅再拨号/派发,连接建立与任务流转的事件都不会漏。
	sub := bus.subscribe()
	defer bus.unsubscribe(sub)

	node, ws := dialTestNode(t, gdb, ca, hub, ts)

	ctx := context.Background()
	taskID, err := orch.Dispatch(ctx, node.ID, controlproto.TaskLogQuery, controlproto.TaskParams{Limit: 5}, "admin")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	readEnv(t, ctx, ws) // 消费任务帧
	sendEnv(t, ctx, ws, controlproto.TypeTaskResult, taskID, controlproto.TaskResultPayload{
		TaskID: taskID, Type: controlproto.TaskLogQuery, State: controlproto.TaskStateSuccess,
		Result: controlproto.TaskResult{LogLines: []string{"line1"}},
	})
	waitFor(t, func() bool {
		task, err := orch.GetTask(taskID)
		return err == nil && task.State == string(controlproto.TaskStateSuccess)
	})

	// 收集到 success 为止,校验 pending → running → success 全部按序发布。
	var states []string
	deadline := time.After(3 * time.Second)
	for {
		if len(states) > 0 && states[len(states)-1] == "success" {
			break
		}
		select {
		case ev := <-sub.events:
			if ev.Type == AdminEventTaskUpdated && ev.TaskID == taskID {
				states = append(states, ev.State)
			}
		case <-deadline:
			t.Fatalf("did not observe success event; got states %v", states)
		}
	}
	want := []string{"pending", "running", "success"}
	if len(states) != len(want) {
		t.Fatalf("task states = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("task states = %v, want %v", states, want)
		}
	}
}

// newSSETestAdminServer 构造挂好事件总线的完整 admin 服务器,依赖组合与
// adminserver_test.go 一致(鉴权走真实 webadmin 中间件)。
func newSSETestAdminServer(t *testing.T, gdb *gorm.DB, cipher *SecretCipher) *AdminServer {
	t.Helper()
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("panel-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("password hash: %v", err)
	}
	server, err := NewAdminServer(AdminServerDeps{
		Config: &config.PanelConfig{
			AdminAddr: "127.0.0.1:0",
			WebAdmin: config.WebAdminConfig{
				PasswordHash:      string(passwordHash),
				SessionSecret:     "test-sse-session-secret",
				SessionTTLMinutes: 10,
			},
		},
		DB:        gdb,
		Hub:       NewHub(),
		Creds:     NewPanelCredentialStore(gdb, cipher),
		Desired:   NewDesiredStateAuthority(gdb, cipher),
		Tasks:     NewTaskOrchestrator(gdb, NewHub(), 0),
		Transport: NewTransportServer(TransportDeps{DB: gdb, Hub: NewHub(), Cipher: cipher, Events: NewAdminEventBus()}),
		Migration: NewMigrationCoordinator(gdb, cipher, NewDesiredStateAuthority(gdb, cipher), NewAuditor(gdb)),
		Audit:     NewAuditor(gdb),
		LogRing:   logging.NewRing(10),
		LogFile:   filepath.Join(t.TempDir(), "panel.log"),
		Events:    NewAdminEventBus(),
	})
	if err != nil {
		t.Fatalf("new admin server: %v", err)
	}
	return server
}
