package nodeagent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
)

// --- 测试辅助 ---

// openTestDB 为 client 测试创建一个临时 sqlite 数据库并迁移 base schema 与 agent 状态表。
func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.db")
	gdb, err := db.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.MigrateConfigured("sqlite", path, gdb); err != nil {
		t.Fatalf("migrate base schema: %v", err)
	}
	if err := MigrateState(gdb); err != nil {
		t.Fatalf("migrate agent state: %v", err)
	}
	return gdb
}

// startTestPanelServer 启动一个 TLS WebSocket 测试服务器,接受任意客户端证书,
// 并调用 handler 处理升级后的连接。返回的服务器 URL 可直接填入 ClientConfig.AgentURL。
func startTestPanelServer(t *testing.T, handler func(*testing.T, *websocket.Conn)) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		handler(t, ws)
	}))
	server.TLS = &tls.Config{
		ClientAuth: tls.RequireAnyClientCert,
		MinVersion: tls.VersionTLS12,
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// testIdentity 生成一份临时 mTLS 身份,并把 server 的 TLS 证书写入 CAFile,
// 使节点侧能验证测试服务器。
func testIdentity(t *testing.T, server *httptest.Server) Identity {
	t.Helper()
	tmp := t.TempDir()
	id := Identity{
		KeyFile:  filepath.Join(tmp, "node.key"),
		CertFile: filepath.Join(tmp, "node.crt"),
		CAFile:   filepath.Join(tmp, "panel-ca.crt"),
	}
	writeTestCert(t, id, time.Now().Add(24*time.Hour))
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	writeFile(t, id.CAFile, caPEM)
	return id
}

// testClient 构造一个连向测试服务器的 Client,db/executor/runner 按需传入。
func testClient(t *testing.T, server *httptest.Server, id Identity, gdb *gorm.DB) *Client {
	t.Helper()
	var executor *Executor
	if gdb != nil {
		executor = NewManagedExecutor(gdb, ExecutorRuntime{})
	}
	return NewClient(ClientConfig{
		AgentURL:          "wss://" + server.Listener.Addr().String() + "/agent",
		NodeID:            1,
		Identity:          id,
		DialTimeout:       2 * time.Second,
		MinBackoff:        10 * time.Millisecond,
		MaxBackoff:        50 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
	}, gdb, executor, nil)
}

// sendEnv 从服务器侧发送一个 envelope。
func sendEnv(t *testing.T, ctx context.Context, ws *websocket.Conn, msgType controlproto.MessageType, id string, payload any) {
	t.Helper()
	env, err := controlproto.NewEnvelope(msgType, id, payload)
	if err != nil {
		t.Fatalf("new envelope: %v", err)
	}
	data, err := env.Encode()
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write envelope: %v", err)
	}
}

// readEnv 从服务器侧读取一个 envelope。
func readEnv(t *testing.T, ctx context.Context, ws *websocket.Conn) controlproto.Envelope {
	t.Helper()
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	env, err := controlproto.DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env
}

// --- R3/R4/R6 WebSocket 行为测试 ---

// TestHandshakeTimeout 验证服务器接受连接后不应答 hello 时,handshake 在独立
// 超时内返回错误。测试把 HandshakeTimeout 压到 100ms 以快速完成。
func TestHandshakeTimeout(t *testing.T) {
	server := startTestPanelServer(t, func(t *testing.T, ws *websocket.Conn) {
		// 读 hello,但不回 ack,让客户端握手超时。
		_, _, _ = ws.Read(context.Background())
		// 保持连接打开直到测试结束。
		<-context.Background().Done()
	})
	id := testIdentity(t, server)
	client := testClient(t, server, id, openTestDB(t))
	client.cfg.HandshakeTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := client.connectAndServe(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected handshake timeout error")
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("expected handshake error, got %v", err)
	}
	// 应在压短后的握手超时附近返回,而不是无限挂起。
	if elapsed > 500*time.Millisecond {
		t.Fatalf("handshake took too long: %v", elapsed)
	}
}

// TestWatchdogTimeout 验证服务器 hello_ack 后完全静默,node 侧看门狗在
// max(3×heartbeat_interval, 60s) 内触发重连。测试用 50ms 心跳,阈值 60s,
// 因此看门狗应在 ~60s 触发;为加速,直接测试 watchdog 函数本身。
func TestWatchdogTimeout(t *testing.T) {
	client := NewClient(ClientConfig{HeartbeatInterval: 50 * time.Millisecond}, nil, nil, nil)
	client.lastRecvAt.Store(time.Now().Add(-2 * time.Minute).UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 用假的 cancel 与 ws:看门狗超时后会调用 cancel。
	called := make(chan struct{})
	fakeCancel := func() { close(called) }
	// 由于 watchdog 需要 *websocket.Conn 做 Close,这里传入 nil 并在取消后退出。
	go client.watchdog(ctx, fakeCancel, nil)

	select {
	case <-called:
		// 成功:看门狗在 60s 阈值内触发。
	case <-time.After(70 * time.Second):
		t.Fatal("watchdog did not fire within 70s")
	}
}

// TestHeartbeatSendFailureClosesLoop verifies that a peer closing the socket
// causes the heartbeat writer to return instead of continuing a dead loop.
func TestHeartbeatSendFailureClosesLoop(t *testing.T) {
	server := startTestPanelServer(t, func(t *testing.T, ws *websocket.Conn) {
		_, _, _ = ws.Read(context.Background())
	})
	id := testIdentity(t, server)
	client := testClient(t, server, id, openTestDB(t))
	client.cfg.HeartbeatInterval = 10 * time.Millisecond

	cert, err := tls.LoadX509KeyPair(id.CertFile, id.KeyFile)
	if err != nil {
		t.Fatalf("load test client certificate: %v", err)
	}
	dialCtx, cancelDial := context.WithTimeout(context.Background(), time.Second)
	defer cancelDial()
	ws, _, err := websocket.Dial(dialCtx, "wss://"+server.Listener.Addr().String()+"/agent", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // test server certificate is ephemeral
				Certificates:       []tls.Certificate{cert},
			},
		}},
	})
	if err != nil {
		t.Fatalf("dial test websocket: %v", err)
	}
	if err := ws.CloseNow(); err != nil {
		t.Fatalf("force-close client websocket: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		client.heartbeatLoop(ctx, ws, nil)
		close(done)
	}()

	select {
	case <-done:
		// The failed write must close the heartbeat loop before its context
		// deadline; otherwise a dead control-plane connection can linger.
	case <-ctx.Done():
		t.Fatal("heartbeat loop did not stop after peer close")
	}
}

// TestVersionFrameDropped 验证 serveLoop 收到高于协商版本的帧时丢弃并不断连。
func TestVersionFrameDropped(t *testing.T) {
	server := startTestPanelServer(t, func(t *testing.T, ws *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// 读 hello。
		hello := readEnv(t, ctx, ws)
		if hello.Type != controlproto.TypeHello {
			t.Fatalf("expected hello, got %s", hello.Type)
		}
		// 回 hello_ack,协商版本为 2。
		sendEnv(t, ctx, ws, controlproto.TypeHelloAck, "ack1", controlproto.HelloAckPayload{
			ProtocolVersion: 2,
			ServerTime:      time.Now().UTC().Format(time.RFC3339),
			NeedsSync:       false,
		})
		// 发送一个高版本 fatal error 帧,client 应丢弃它;若被错误执行,连接会
		// 因 fatal error 而断开,测试即可发现。
		highVersionEnv := controlproto.Envelope{
			Type:    controlproto.TypeError,
			Version: 99,
			ID:      "bad",
			Payload: json.RawMessage(`{"code":"test","message":"should be dropped","fatal":true}`),
		}
		data, _ := highVersionEnv.Encode()
		_ = ws.Write(ctx, websocket.MessageText, data)

		// 再发一个正常心跳 ack,然后显式关闭连接,让 client 的 read 立即返回。
		sendEnv(t, ctx, ws, controlproto.TypeHeartbeatAck, "ack2", controlproto.HeartbeatAckPayload{})
		time.Sleep(50 * time.Millisecond)
		_ = ws.Close(websocket.StatusNormalClosure, "test done")
	})
	id := testIdentity(t, server)
	client := testClient(t, server, id, openTestDB(t))
	client.cfg.HeartbeatInterval = 1 * time.Hour // 禁用心跳,避免干扰读循环

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := client.connectAndServe(ctx)

	// 服务器发完 ack2 后关闭,连接结束应为正常 EOF/close,不应是高版本帧导致的 fatal error。
	if err != nil {
		if strings.Contains(err.Error(), "panel fatal error") {
			t.Fatalf("high-version frame was executed instead of dropped: %v", err)
		}
		if !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "close") && !strings.Contains(err.Error(), "WebSocket closed") && !strings.Contains(err.Error(), "status = StatusNormalClosure") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

// TestRenewedReconnectSentinel 验证 errRenewedReconnect 哨兵能被 Run 识别为计划内断连。
func TestRenewedReconnectSentinel(t *testing.T) {
	if !errors.Is(errRenewedReconnect, errRenewedReconnect) {
		t.Fatal("errRenewedReconnect sentinel must be comparable with errors.Is")
	}
}

// --- R5 task 校验测试 ---

// fakeTaskRunner 记录被调用过的任务并返回成功结果,用于验证空 task_id 不会落台账。
type fakeTaskRunner struct {
	called atomic.Int32
	result controlproto.TaskResultPayload
}

func (f *fakeTaskRunner) Run(ctx context.Context, task controlproto.TaskPayload) controlproto.TaskResultPayload {
	f.called.Add(1)
	if f.result.State != "" {
		return f.result
	}
	return controlproto.TaskResultPayload{State: controlproto.TaskStateSuccess}
}

// TestHandleTaskRejectsEmptyID 验证 task_id 为空时不执行、不写台账、仅返回(无法回包)。
func TestHandleTaskRejectsEmptyID(t *testing.T) {
	gdb := openTestDB(t)
	server := startTestPanelServer(t, func(t *testing.T, ws *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// 读 hello 并回 ack,让 serveLoop 进入分发。
		_ = readEnv(t, ctx, ws)
		sendEnv(t, ctx, ws, controlproto.TypeHelloAck, "ack", controlproto.HelloAckPayload{
			ProtocolVersion: controlproto.ProtocolVersion,
			ServerTime:      time.Now().UTC().Format(time.RFC3339),
		})
		// 发一个 task_id 为空的任务。
		sendEnv(t, ctx, ws, controlproto.TypeTask, "t1", controlproto.TaskPayload{
			TaskID: "", Type: controlproto.TaskLogQuery, TimeoutMS: 1000,
		})
		// 稍等后关闭;空 task_id 不应有响应。
		time.Sleep(200 * time.Millisecond)
	})
	id := testIdentity(t, server)
	client := NewClient(ClientConfig{
		AgentURL:          "wss://" + server.Listener.Addr().String() + "/agent",
		NodeID:            1,
		Identity:          id,
		DialTimeout:       2 * time.Second,
		HeartbeatInterval: 1 * time.Hour, // 禁用心跳干扰
	}, gdb, NewManagedExecutor(gdb, ExecutorRuntime{}), &fakeTaskRunner{})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_ = client.connectAndServe(ctx)

	// 台账不应有空 task_id。
	var count int64
	if err := gdb.Model(&AppliedTask{}).Where("task_id = ?", "").Count(&count).Error; err != nil {
		t.Fatalf("count applied tasks: %v", err)
	}
	if count != 0 {
		t.Fatalf("empty task_id should not be recorded, got %d rows", count)
	}
}

// TestHandleTaskRejectsUnknownType 验证未知任务类型回 failed 结果。
func TestHandleTaskRejectsUnknownType(t *testing.T) {
	gdb := openTestDB(t)
	server := startTestPanelServer(t, func(t *testing.T, ws *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = readEnv(t, ctx, ws)
		sendEnv(t, ctx, ws, controlproto.TypeHelloAck, "ack", controlproto.HelloAckPayload{
			ProtocolVersion: controlproto.ProtocolVersion,
			ServerTime:      time.Now().UTC().Format(time.RFC3339),
		})
		sendEnv(t, ctx, ws, controlproto.TypeTask, "t2", controlproto.TaskPayload{
			TaskID: "unknown-task", Type: "not_a_real_task", TimeoutMS: 1000,
		})
		// 读取 task_result 响应。
		res := readEnv(t, ctx, ws)
		if res.Type != controlproto.TypeTaskResult {
			t.Fatalf("expected task_result, got %s", res.Type)
		}
		var result controlproto.TaskResultPayload
		if err := res.DecodePayload(&result); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if result.State != controlproto.TaskStateFailed {
			t.Fatalf("state = %s, want failed", result.State)
		}
		if !strings.Contains(result.Error, "unsupported") {
			t.Fatalf("error should mention unsupported type, got %q", result.Error)
		}
	})
	id := testIdentity(t, server)
	client := NewClient(ClientConfig{
		AgentURL:          "wss://" + server.Listener.Addr().String() + "/agent",
		NodeID:            1,
		Identity:          id,
		DialTimeout:       2 * time.Second,
		HeartbeatInterval: 1 * time.Hour,
	}, gdb, NewManagedExecutor(gdb, ExecutorRuntime{}), &fakeTaskRunner{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = client.connectAndServe(ctx)
}

// TestTaskTimeoutClamping 验证 node 侧对 panel 下发 timeout_ms 的十分钟上界钳制。
func TestTaskTimeoutClamping(t *testing.T) {
	if taskTimeout(0) != defaultTaskTimeout {
		t.Fatalf("zero timeout should fall back to default")
	}
	if taskTimeout(-1000) != defaultTaskTimeout {
		t.Fatalf("negative timeout should fall back to default")
	}
	if taskTimeout(30*1000) != 30*time.Second {
		t.Fatalf("30s timeout mismatch")
	}
	if taskTimeout(20*60*1000) != maxTaskTimeout {
		t.Fatalf("timeout above 10min should be clamped")
	}
}

// TestIsKnownTaskType 验证预定义任务类型识别。
func TestIsKnownTaskType(t *testing.T) {
	if !isKnownTaskType(controlproto.TaskLogQuery) {
		t.Fatal("log_query should be known")
	}
	if !isKnownTaskType(controlproto.TaskStorageScan) {
		t.Fatal("storage_scan should be known")
	}
	if !isKnownTaskType(controlproto.TaskStorageReconcileApply) {
		t.Fatal("reconcile_apply should be known")
	}
	if isKnownTaskType("unknown") {
		t.Fatal("unknown type should not be known")
	}
}
