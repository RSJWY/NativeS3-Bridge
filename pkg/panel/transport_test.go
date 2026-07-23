package panel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// issueNodeClientCert signs a node client cert from the test CA and returns the
// tls.Certificate the node presents plus its fingerprint (as stored by panel).
func issueNodeClientCert(t *testing.T, ca *CA, nodeID uint) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen node key: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-request"},
	}, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	signed, err := ca.SignNodeCSR(csrPEM, nodeID, 0, time.Now())
	if err != nil {
		t.Fatalf("sign csr: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(signed.CertPEM, keyPEM)
	if err != nil {
		t.Fatalf("x509 key pair: %v", err)
	}
	return cert, signed.Fingerprint
}

// startTestServer wires the transport server behind an httptest TLS server that
// requests client certs (mTLS). Returns the server and its base URL.
func startTestServer(t *testing.T, ts *TransportServer, ca *CA) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(ts.Handler())
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())
	srv.TLS = &tls.Config{
		ClientCAs:  pool,
		ClientAuth: tls.VerifyClientCertIfGiven,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestAgentHandshakeAndDesiredStatePush(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	key := make([]byte, masterKeyLen)
	cipher, err := NewSecretCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: hub, Cipher: cipher})

	// Create node + issue its client cert, persist the cert row.
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

	secretCipher, err := cipher.Encrypt("sk1")
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}
	if err := gdb.Create(&NodeCredential{NodeID: node.ID, AccessKey: "AK1", SecretKeyCipher: secretCipher, Status: "enabled"}).Error; err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	authority := NewDesiredStateAuthority(gdb, cipher)
	if _, _, err := authority.Publish(node.ID, "test"); err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	if _, _, err := authority.Publish(node.ID, "test"); err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	srv := startTestServer(t, ts, ca)
	wsURL := "wss" + strings.TrimPrefix(srv.URL, "https") + "/agent"

	// Dial with the node client cert over mTLS.
	clientTLS := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      srv.TLS.RootCAs, // not set; use InsecureSkipVerify for the test server cert
	}
	clientTLS.InsecureSkipVerify = true // httptest server cert is self-signed
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "done")

	// Send hello advertising an older applied version (1) than desired (2).
	sendEnv(t, ctx, ws, controlproto.TypeHello, "h1", controlproto.HelloPayload{
		ProtocolVersion: controlproto.ProtocolVersion,
		NodeID:          "1",
		AppliedVersion:  1,
		Capabilities:    []string{controlproto.CapabilityAuthoritativeConfigV1},
	})

	// Expect hello_ack with needs_sync=true, desired_version=2.
	ack := readEnv(t, ctx, ws)
	if ack.Type != controlproto.TypeHelloAck {
		t.Fatalf("expected hello_ack, got %s", ack.Type)
	}
	var ackPayload controlproto.HelloAckPayload
	if err := ack.DecodePayload(&ackPayload); err != nil {
		t.Fatalf("decode hello_ack: %v", err)
	}
	if !ackPayload.NeedsSync || ackPayload.DesiredVersion != 2 {
		t.Fatalf("expected needs_sync=true version=2, got %+v", ackPayload)
	}

	// The node should now be registered online in the hub.
	waitFor(t, func() bool { return hub.IsOnline(node.ID) })

	// Panel automatically pushes desired state after the connection is registered;
	// node acks synced.
	ds := readEnv(t, ctx, ws)
	if ds.Type != controlproto.TypeDesiredState {
		t.Fatalf("expected desired_state, got %s", ds.Type)
	}
	var dsPayload controlproto.DesiredStatePayload
	if err := ds.DecodePayload(&dsPayload); err != nil {
		t.Fatalf("decode desired_state: %v", err)
	}
	if dsPayload.Version != 2 {
		t.Fatalf("desired version = %d, want 2", dsPayload.Version)
	}

	// Node acks synced; panel records it in node_status.
	sendEnv(t, ctx, ws, controlproto.TypeAck, ds.ID, controlproto.AckPayload{
		Version: 2, State: controlproto.SyncStateSynced, ContentHash: dsPayload.ContentHash,
	})
	waitFor(t, func() bool {
		var st NodeState
		if err := gdb.Where("node_id = ?", node.ID).First(&st).Error; err != nil {
			return false
		}
		return st.AppliedVersion == 2 && st.SyncState == SyncStateSynced
	})
}

func TestAgentRejectsUnknownCert(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	hub := NewHub()
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: hub})

	// Issue a cert but DO NOT persist the NodeCert row: the fingerprint is unknown.
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	clientCert, _ := issueNodeClientCert(t, ca, node.ID)

	srv := startTestServer(t, ts, ca)
	wsURL := "wss" + strings.TrimPrefix(srv.URL, "https") + "/agent"

	clientTLS := &tls.Config{Certificates: []tls.Certificate{clientCert}, InsecureSkipVerify: true}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The handshake should be rejected (401) because the fingerprint is unknown.
	_, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: httpClient})
	if err == nil {
		t.Fatal("expected dial rejection for unknown cert")
	}
}

func TestRegisterEndpointIssuesCert(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: NewHub()})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()
	token, err := GenerateRegistrationToken(gdb, node.ID, 0, now)
	if err != nil {
		t.Fatalf("gen token: %v", err)
	}

	// Build a node CSR.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-request"},
	}, key)
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	rr := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(mustJSON(t, registerRequest{
		NodeID: int64(node.ID), Token: token, CSRPEM: string(csrPEM),
	})))
	rw := httptest.NewRecorder()
	ts.handleRegister(rw, rr)

	if rw.Code != http.StatusOK {
		t.Fatalf("register status = %d, body = %s", rw.Code, rw.Body.String())
	}
	// Cert row must be persisted and token consumed.
	var certCount int64
	gdb.Model(&NodeCert{}).Where("node_id = ?", node.ID).Count(&certCount)
	if certCount != 1 {
		t.Fatalf("expected 1 persisted cert, got %d", certCount)
	}
	// A response-loss retry with the same token and private key must replay the
	// exact issued response without inserting another certificate row.
	rr2 := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(mustJSON(t, registerRequest{
		NodeID: int64(node.ID), Token: token, CSRPEM: string(csrPEM),
	})))
	rw2 := httptest.NewRecorder()
	ts.handleRegister(rw2, rr2)
	if rw2.Code != http.StatusOK || rw2.Body.String() != rw.Body.String() {
		t.Fatalf("same-key replay status/body = %d %q, want 200 %q", rw2.Code, rw2.Body.String(), rw.Body.String())
	}
	gdb.Model(&NodeCert{}).Where("node_id = ?", node.ID).Count(&certCount)
	if certCount != 1 {
		t.Fatalf("same-key replay inserted certificates: got %d", certCount)
	}

	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otherCSRDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-request"},
	}, otherKey)
	otherCSRPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: otherCSRDER})
	rr3 := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(mustJSON(t, registerRequest{
		NodeID: int64(node.ID), Token: token, CSRPEM: string(otherCSRPEM),
	})))
	rw3 := httptest.NewRecorder()
	ts.handleRegister(rw3, rr3)
	if rw3.Code != http.StatusUnauthorized {
		t.Fatalf("changed-key replay status = %d, want 401", rw3.Code)
	}
}

// --- helpers ---

func sendEnv(t *testing.T, ctx context.Context, ws *websocket.Conn, msgType controlproto.MessageType, id string, payload any) {
	t.Helper()
	env, err := controlproto.NewEnvelope(msgType, id, payload)
	if err != nil {
		t.Fatalf("new envelope: %v", err)
	}
	data, err := env.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write %s: %v", msgType, err)
	}
}

func readEnv(t *testing.T, ctx context.Context, ws *websocket.Conn) controlproto.Envelope {
	t.Helper()
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	env, err := controlproto.DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}
