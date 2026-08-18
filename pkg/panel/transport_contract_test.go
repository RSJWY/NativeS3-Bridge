package panel

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// dialTestAgent 用给定的 clientCert 返回一个已 mTLS 拨号到测试服务器的 WebSocket 连接。
func dialTestAgent(t *testing.T, srv *httptest.Server, clientCert tls.Certificate) *websocket.Conn {
	t.Helper()
	clientTLS := &tls.Config{
		Certificates:       []tls.Certificate{clientCert},
		InsecureSkipVerify: true,
	}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "wss" + strings.TrimPrefix(srv.URL, "https") + "/agent"
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	return ws
}

func TestHandshakeHeartbeatInterval(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: hub, HeartbeatInterval: 15 * time.Second})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
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
	clientTLS := &tls.Config{Certificates: []tls.Certificate{clientCert}, InsecureSkipVerify: true}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "wss" + strings.TrimPrefix(srv.URL, "https") + "/agent"
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "test done")

	// 上报 60s 心跳,应在 hello_ack 中协商出 v2。
	sendEnv(t, ctx, ws, controlproto.TypeHello, "h1", controlproto.HelloPayload{
		ProtocolVersion:     controlproto.ProtocolVersion,
		NodeID:              "1",
		AppliedVersion:      0,
		Capabilities:        []string{controlproto.CapabilityAuthoritativeConfigV1},
		HeartbeatIntervalMS: 60000,
	})
	ack := readEnv(t, ctx, ws)
	if ack.Type != controlproto.TypeHelloAck {
		t.Fatalf("expected hello_ack, got %s", ack.Type)
	}
	var ackPayload controlproto.HelloAckPayload
	if err := ack.DecodePayload(&ackPayload); err != nil {
		t.Fatalf("decode hello_ack: %v", err)
	}
	if ackPayload.ProtocolVersion != 2 {
		t.Fatalf("protocol version = %d, want 2", ackPayload.ProtocolVersion)
	}
	waitFor(t, func() bool { return hub.IsOnline(node.ID) })

	conn, ok := hub.Get(node.ID)
	if !ok {
		t.Fatal("node not online after handshake")
	}
	if conn.heartbeatInterval() != 60*time.Second {
		t.Fatalf("heartbeat interval = %v, want 60s", conn.heartbeatInterval())
	}
}

func TestHandshakeClampsInvalidHeartbeatInterval(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: hub, HeartbeatInterval: 15 * time.Second})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
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
	ws := dialTestAgent(t, srv, clientCert)
	defer ws.Close(websocket.StatusNormalClosure, "test done")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 上报 0ms(非法),应回落到 panel 默认 15s。
	sendEnv(t, ctx, ws, controlproto.TypeHello, "h1", controlproto.HelloPayload{
		ProtocolVersion:     controlproto.ProtocolVersion,
		NodeID:              "1",
		AppliedVersion:      0,
		Capabilities:        []string{controlproto.CapabilityAuthoritativeConfigV1},
		HeartbeatIntervalMS: 0,
	})
	ack := readEnv(t, ctx, ws)
	if ack.Type != controlproto.TypeHelloAck {
		t.Fatalf("expected hello_ack, got %s", ack.Type)
	}
	waitFor(t, func() bool { return hub.IsOnline(node.ID) })

	conn, ok := hub.Get(node.ID)
	if !ok {
		t.Fatal("node not online after handshake")
	}
	if conn.heartbeatInterval() != 15*time.Second {
		t.Fatalf("clamped interval = %v, want 15s", conn.heartbeatInterval())
	}
}

func TestV1PeerHandshakeFails(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: hub})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
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
	ws := dialTestAgent(t, srv, clientCert)
	defer ws.Close(websocket.StatusNormalClosure, "test done")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 模拟 v1 节点:panel 应回复 fatal error 并断连。
	sendEnv(t, ctx, ws, controlproto.TypeHello, "h1", controlproto.HelloPayload{
		ProtocolVersion: 1,
		NodeID:          "1",
		AppliedVersion:  0,
		Capabilities:    []string{controlproto.CapabilityAuthoritativeConfigV1},
	})
	env := readEnv(t, ctx, ws)
	if env.Type != controlproto.TypeError {
		t.Fatalf("expected error, got %s", env.Type)
	}
	var perr controlproto.ErrorPayload
	if err := env.DecodePayload(&perr); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if perr.Code != controlproto.ErrCodeVersionIncompatible {
		t.Fatalf("error code = %s, want version_incompatible", perr.Code)
	}
	if !strings.Contains(perr.Message, "synchronous upgrade required") {
		t.Fatalf("error message missing upgrade hint: %s", perr.Message)
	}
	if !perr.Fatal {
		t.Fatal("version error should be fatal")
	}
}

func TestImportReportChunkReassembly(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	key := make([]byte, masterKeyLen)
	cipher, err := NewSecretCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	coordinator := NewMigrationCoordinator(gdb, cipher, NewDesiredStateAuthority(gdb, cipher), NewAuditor(gdb))
	ts := NewTransportServer(TransportDeps{
		DB: gdb, CA: ca, Hub: hub, Cipher: cipher,
		MigrationSink: coordinator,
	})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
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
	ws := dialTestAgent(t, srv, clientCert)
	defer ws.Close(websocket.StatusNormalClosure, "test done")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sendEnv(t, ctx, ws, controlproto.TypeHello, "h1", controlproto.HelloPayload{
		ProtocolVersion:     controlproto.ProtocolVersion,
		NodeID:              "1",
		AppliedVersion:      0,
		Capabilities:        []string{controlproto.CapabilityAuthoritativeConfigV1},
		HeartbeatIntervalMS: 15000,
	})
	if ack := readEnv(t, ctx, ws); ack.Type != controlproto.TypeHelloAck {
		t.Fatalf("expected hello_ack, got %s", ack.Type)
	}
	waitFor(t, func() bool { return hub.IsOnline(node.ID) })

	// 发送 import_request 触发节点上报(这里直接模拟节点分块回复)。
	sendEnv(t, ctx, ws, controlproto.TypeImportReportChunk, "req-1", controlproto.ImportReportChunkPayload{
		RequestID: "req-1", Seq: 1, Total: 3,
		Buckets: []controlproto.DesiredBucket{{Name: "bucket-a", ACL: "private"}},
	})
	sendEnv(t, ctx, ws, controlproto.TypeImportReportChunk, "req-1", controlproto.ImportReportChunkPayload{
		RequestID: "req-1", Seq: 0, Total: 3,
		Credentials: []controlproto.DesiredCredential{{AccessKey: "AK1", SecretKey: "sk1", Status: "enabled"}},
	})
	sendEnv(t, ctx, ws, controlproto.TypeImportReportChunk, "req-1", controlproto.ImportReportChunkPayload{
		RequestID: "req-1", Seq: 2, Total: 3,
		Webhooks: []controlproto.DesiredWebhook{{URL: "http://example.com", Events: "ObjectCreated", Enabled: true}},
	})

	waitFor(t, func() bool {
		_, ok := coordinator.PendingSummary(node.ID)
		return ok
	})

	summary, ok := coordinator.PendingSummary(node.ID)
	if !ok {
		t.Fatal("pending import not created")
	}
	if summary.CredentialCount != 1 || summary.BucketCount != 1 || summary.WebhookCount != 1 {
		t.Fatalf("unexpected summary counts: %+v", summary)
	}
}

func TestImportReportChunkLimitDisconnects(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	key := make([]byte, masterKeyLen)
	cipher, err := NewSecretCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	coordinator := NewMigrationCoordinator(gdb, cipher, NewDesiredStateAuthority(gdb, cipher), NewAuditor(gdb))
	ts := NewTransportServer(TransportDeps{
		DB: gdb, CA: ca, Hub: hub, Cipher: cipher,
		MigrationSink: coordinator,
	})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
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
	ws := dialTestAgent(t, srv, clientCert)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sendEnv(t, ctx, ws, controlproto.TypeHello, "h1", controlproto.HelloPayload{
		ProtocolVersion:     controlproto.ProtocolVersion,
		NodeID:              "1",
		AppliedVersion:      0,
		Capabilities:        []string{controlproto.CapabilityAuthoritativeConfigV1},
		HeartbeatIntervalMS: 15000,
	})
	if ack := readEnv(t, ctx, ws); ack.Type != controlproto.TypeHelloAck {
		t.Fatalf("expected hello_ack, got %s", ack.Type)
	}
	waitFor(t, func() bool { return hub.IsOnline(node.ID) })

	// 发送 Total 远大于实际块数的分块,触发重组超时逻辑:这里用 seq >= total 直接非法。
	sendEnv(t, ctx, ws, controlproto.TypeImportReportChunk, "req-bad", controlproto.ImportReportChunkPayload{
		RequestID: "req-bad", Seq: 5, Total: 2,
	})

	// 非法 chunk 应触发断连,读取应失败。
	_, _, rerr := ws.Read(ctx)
	if rerr == nil {
		t.Fatal("expected disconnect after invalid chunk")
	}
}
