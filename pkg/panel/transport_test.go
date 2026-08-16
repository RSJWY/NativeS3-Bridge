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
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
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
	t.Cleanup(func() { closeTestNode(t, gdb, hub, node.ID, ws) })

	// Send hello advertising an older applied version (1) than desired (2).
	sendEnv(t, ctx, ws, controlproto.TypeHello, "h1", controlproto.HelloPayload{
		ProtocolVersion: controlproto.ProtocolVersion,
		NodeID:          "1",
		AppliedVersion:  1,
		Capabilities:    []string{controlproto.CapabilityAuthoritativeConfigV1},
		Region:          "ap-southeast-2",
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

	// hello 里自报的区域随握手落库,供 Panel 只读展示。
	var st NodeState
	if err := gdb.Where("node_id = ?", node.ID).First(&st).Error; err != nil {
		t.Fatalf("load node state: %v", err)
	}
	if st.Region != "ap-southeast-2" {
		t.Fatalf("reported region = %q, want ap-southeast-2", st.Region)
	}
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

// --- /renew endpoint tests ---

// renewTestCert issues a node client cert, persists the NodeCert row, and
// returns the tls.Certificate + fingerprint for use in /renew tests.
func renewTestCert(t *testing.T, gdb *gorm.DB, ca *CA, nodeID uint) (tls.Certificate, string) {
	t.Helper()
	clientCert, fingerprint := issueNodeClientCert(t, ca, nodeID)
	if err := gdb.Create(&NodeCert{
		NodeID: nodeID, Fingerprint: fingerprint, Serial: "1",
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}).Error; err != nil {
		t.Fatalf("store cert: %v", err)
	}
	return clientCert, fingerprint
}
func TestRenewIssuesCert(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: NewHub()})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	clientCert, oldFingerprint := renewTestCert(t, gdb, ca, node.ID)

	// Build a CSR with the correct CN for this node.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", node.ID)},
	}, key)
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	body := mustJSON(t, renewRequest{CSRPEM: string(csrPEM)})
	srv := startTestServer(t, ts, ca)
	defer srv.Close()

	clientTLS := &tls.Config{Certificates: []tls.Certificate{clientCert}, InsecureSkipVerify: true}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}

	resp, err := httpClient.Post(srv.URL+"/renew", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("renew request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renew status = %d, want 200", resp.StatusCode)
	}
	var result registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode renew response: %v", err)
	}
	if result.CertPEM == "" || result.CACertPEM == "" || result.NotAfter == "" {
		t.Fatalf("renew response missing fields: %+v", result)
	}

	// A new NodeCert row must exist.
	var count int64
	gdb.Model(&NodeCert{}).Where("node_id = ?", node.ID).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 cert rows after renew, got %d", count)
	}

	// The old cert must NOT be revoked (D1: activation happens on /agent reconnect).
	var oldCert NodeCert
	if err := gdb.Where("fingerprint = ?", oldFingerprint).First(&oldCert).Error; err != nil {
		t.Fatalf("query old cert: %v", err)
	}
	if oldCert.Revoked {
		t.Fatal("old cert must not be revoked at renew time (D1)")
	}

	// Audit entry must exist.
	var auditCount int64
	gdb.Model(&AuditLog{}).Where("action = ? AND target_node = ? AND result = ?", "node_cert_renew", node.ID, "issued").Count(&auditCount)
	if auditCount != 1 {
		t.Fatalf("expected 1 audit entry, got %d", auditCount)
	}
}

func TestRenewRejectsExpiredCert(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: NewHub()})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	clientCert, fingerprint := issueNodeClientCert(t, ca, node.ID)
	// Persist as already expired.
	if err := gdb.Create(&NodeCert{
		NodeID: node.ID, Fingerprint: fingerprint, Serial: "1",
		NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: time.Now().Add(-time.Hour),
	}).Error; err != nil {
		t.Fatalf("store cert: %v", err)
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", node.ID)},
	}, key)
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	body := mustJSON(t, renewRequest{CSRPEM: string(csrPEM)})

	srv := startTestServer(t, ts, ca)
	defer srv.Close()
	clientTLS := &tls.Config{Certificates: []tls.Certificate{clientCert}, InsecureSkipVerify: true}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}

	resp, err := httpClient.Post(srv.URL+"/renew", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("renew request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("renew with expired cert: status = %d, want 401", resp.StatusCode)
	}
}

func TestRenewRejectsRevokedCert(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: NewHub()})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	clientCert, _ := renewTestCert(t, gdb, ca, node.ID)
	// Revoke the cert.
	if _, err := RevokeNodeCerts(gdb, node.ID, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", node.ID)},
	}, key)
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	body := mustJSON(t, renewRequest{CSRPEM: string(csrPEM)})

	srv := startTestServer(t, ts, ca)
	defer srv.Close()
	clientTLS := &tls.Config{Certificates: []tls.Certificate{clientCert}, InsecureSkipVerify: true}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}

	resp, err := httpClient.Post(srv.URL+"/renew", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("renew request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("renew with revoked cert: status = %d, want 401", resp.StatusCode)
	}
}

func TestRenewRejectsRetiredNode(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: NewHub()})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	clientCert, _ := renewTestCert(t, gdb, ca, node.ID)
	// Retire the node.
	if err := gdb.Model(&node).Update("status", NodeStatusRetired).Error; err != nil {
		t.Fatalf("retire node: %v", err)
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", node.ID)},
	}, key)
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	body := mustJSON(t, renewRequest{CSRPEM: string(csrPEM)})

	srv := startTestServer(t, ts, ca)
	defer srv.Close()
	clientTLS := &tls.Config{Certificates: []tls.Certificate{clientCert}, InsecureSkipVerify: true}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}

	resp, err := httpClient.Post(srv.URL+"/renew", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("renew request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("renew with retired node: status = %d, want 401", resp.StatusCode)
	}
}

func TestRenewRejectsNoClientCert(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: NewHub()})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", node.ID)},
	}, key)
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	body := mustJSON(t, renewRequest{CSRPEM: string(csrPEM)})

	srv := startTestServer(t, ts, ca)
	defer srv.Close()
	// No client cert.
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	resp, err := httpClient.Post(srv.URL+"/renew", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("renew request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("renew without client cert: status = %d, want 401", resp.StatusCode)
	}
}

func TestRenewRejectsCSRCNMismatch(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: NewHub()})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	clientCert, _ := renewTestCert(t, gdb, ca, node.ID)

	// CSR with wrong CN (node-999 instead of node-1).
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-999"},
	}, key)
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	body := mustJSON(t, renewRequest{CSRPEM: string(csrPEM)})

	srv := startTestServer(t, ts, ca)
	defer srv.Close()
	clientTLS := &tls.Config{Certificates: []tls.Certificate{clientCert}, InsecureSkipVerify: true}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}

	resp, err := httpClient.Post(srv.URL+"/renew", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("renew request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("renew with mismatched CSR CN: status = %d, want 400", resp.StatusCode)
	}
}

// TestActivateCertFailureDoesNotBlockConnection verifies AC9: when the
// ActivateCert DB transaction fails, authenticateMTLS still returns ok=true
// so the connection proceeds. Availability takes priority (R2.3).
func TestActivateCertFailureDoesNotBlockConnection(t *testing.T) {
	// Use a writable DB to set up schema and data, then reopen read-only so
	// IsCertValid (SELECT) succeeds but ActivateCert (UPDATE) fails.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "panel.db")
	writableDB, err := db.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open writable db: %v", err)
	}
	if err := Migrate(writableDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ca := newTestIntermediateCA(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := writableDB.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	clientCert, fingerprint := issueNodeClientCert(t, ca, node.ID)
	if err := writableDB.Create(&NodeCert{
		NodeID: node.ID, Fingerprint: fingerprint, Serial: "1",
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}).Error; err != nil {
		t.Fatalf("store cert: %v", err)
	}
	sqlDB, _ := writableDB.DB()
	_ = sqlDB.Close()

	// Reopen read-only: SELECTs work, UPDATEs/INSERTs fail.
	roDB, err := gorm.Open(sqlite.Open("file:"+dbPath+"?mode=ro"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open read-only db: %v", err)
	}
	t.Cleanup(func() {
		s, _ := roDB.DB()
		_ = s.Close()
	})

	hub := NewHub()
	ts := NewTransportServer(TransportDeps{DB: roDB, CA: ca, Hub: hub})

	// Build a request with r.TLS set to carry the client cert.
	leaf, _ := x509.ParseCertificate(clientCert.Certificate[0])
	r := &http.Request{
		TLS: &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{leaf},
		},
	}
	fp, nodeID, ok := ts.authenticateMTLS(r)
	if !ok {
		t.Fatal("authenticateMTLS must return ok=true even when ActivateCert fails (R2.3)")
	}
	if fp == "" || nodeID != node.ID {
		t.Fatalf("authenticateMTLS returned wrong identity: fp=%q nodeID=%d", fp, nodeID)
	}
}
