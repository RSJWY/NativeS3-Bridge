package panel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// DefaultHeartbeatInterval is how often nodes are expected to send heartbeats.
// The panel marks a node offline after DefaultOfflineMultiplier missed intervals.
const (
	DefaultHeartbeatInterval = 15 * time.Second
	DefaultOfflineMultiplier = 3
	registrationBodyLimit    = 1 << 16 // 64 KiB: token + CSR only
	handshakeReadTimeout     = 10 * time.Second
	writeTimeout             = 10 * time.Second
)

// TransportDeps are the collaborators the transport server needs. Keeping them
// as an interface-free struct of concrete dependencies avoids premature
// abstraction; the fields are all owned by the panel process.
type TransportDeps struct {
	DB          *gorm.DB
	CA          *CA
	Hub         *Hub
	Cipher      *SecretCipher
	ClientCTTL  time.Duration
	OnConnected func(ctx context.Context, conn *AgentConn) // optional reconcile hook
	// OnDisconnected fires when a serve loop ends (connection closed). It is used
	// to fail any tasks still in flight on the dropped connection (design §5.3).
	OnDisconnected func(conn *AgentConn)
	// MigrationSink receives a node's read-only import report during in-place
	// migration. Optional; nil disables the import path.
	MigrationSink MigrationSink
}

// MigrationSink receives node import reports. *MigrationCoordinator implements
// it; kept as an interface so the transport layer does not depend on the
// migration lifecycle beyond ingesting a report.
type MigrationSink interface {
	ingestReport(nodeID uint, report controlproto.ImportReportPayload) error
}

// TransportServer terminates node control-plane connections. It exposes two
// HTTP surfaces that a caller wires onto (typically) a single mTLS listener:
//   - POST /register : one-shot registration (server TLS only; see note below)
//   - GET  /agent    : the mTLS WebSocket control channel
//
// Registration and the agent channel have different client-auth requirements
// (registration has no client cert yet; the agent channel requires one), so the
// design runs registration behind tls.RequestClientCert / VerifyClientCertIfGiven
// and enforces the mTLS requirement per-route in the handler rather than at the
// listener. See cmd/panel for how the listener is configured.
type TransportServer struct {
	deps TransportDeps
}

// NewTransportServer builds the transport server from its dependencies.
func NewTransportServer(deps TransportDeps) *TransportServer {
	if deps.ClientCTTL <= 0 {
		deps.ClientCTTL = DefaultClientCertTTL
	}
	return &TransportServer{deps: deps}
}

// Handler returns the HTTP handler exposing /register and /agent.
func (s *TransportServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/register", s.handleRegister)
	mux.HandleFunc("/agent", s.handleAgent)
	return mux
}

// registerRequest is the one-shot registration body: a token proving the node
// was created by an admin plus a CSR whose private key never leaves the node.
type registerRequest struct {
	NodeID int64  `json:"node_id"`
	Token  string `json:"token"`
	CSRPEM string `json:"csr_pem"`
}

type registerResponse struct {
	CertPEM   string `json:"cert_pem"`
	CACertPEM string `json:"ca_cert_pem"`
	NotAfter  string `json:"not_after"`
}

// handleRegister validates a single-use token and issues a client certificate
// for the CSR. It is intentionally NOT mTLS-authenticated (the node has no cert
// yet); the token is the bearer credential and is consumed atomically so it
// cannot be replayed. The endpoint is served over server TLS so the node can
// verify the panel identity before presenting its token.
func (s *TransportServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req registerRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, registrationBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.NodeID <= 0 || req.Token == "" || req.CSRPEM == "" {
		writeTransportError(w, http.StatusBadRequest, "node_id, token and csr_pem are required")
		return
	}
	nodeID := uint(req.NodeID)
	now := nowUTC()

	outcome, err := s.issueOrReplayRegistration(nodeID, req.Token, []byte(req.CSRPEM), now)
	if err != nil {
		if errors.Is(err, errRegistrationCSR) {
			writeTransportError(w, http.StatusBadRequest, "invalid CSR")
			return
		}
		if !errors.Is(err, errRegistrationDenied) {
			slog.Error("registration transaction failed", "node", nodeID, "error", err)
			writeTransportError(w, http.StatusInternalServerError, "registration failed")
			return
		}
		s.audit("node_register", nodeID, "", "denied")
		writeTransportError(w, http.StatusUnauthorized, "registration denied")
		return
	}
	result := "issued"
	if outcome.replayed {
		result = "replayed"
	}
	s.audit("node_register", nodeID, outcome.fingerprint, result)
	writeTransportJSON(w, http.StatusOK, outcome.response)
}

// handleAgent upgrades an mTLS-authenticated request to a WebSocket and runs the
// control-plane serve loop. The peer certificate MUST already be verified by the
// TLS layer; this handler performs the application-layer revocation/lifecycle
// check (IsCertValid) before accepting any protocol frame.
func (s *TransportServer) handleAgent(w http.ResponseWriter, r *http.Request) {
	fingerprint, nodeID, ok := s.authenticateMTLS(r)
	if !ok {
		writeTransportError(w, http.StatusUnauthorized, "client certificate required")
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The control channel is same-origin machine-to-machine; there is no
		// browser origin to check. Compression is left default-off (small JSON).
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Warn("agent websocket accept failed", "node", nodeID, "error", err)
		return
	}
	ws.SetReadLimit(DefaultMaxMessageBytes)

	conn := newAgentConn(nodeID, fingerprint, ws)
	// The serve loop owns the connection lifecycle from here.
	s.serve(r.Context(), conn)
}

// authenticateMTLS extracts and validates the verified client certificate.
// Returns the fingerprint, resolved node ID, and whether the cert is currently
// accepted (exists, not revoked, node not retired).
func (s *TransportServer) authenticateMTLS(r *http.Request) (fingerprint string, nodeID uint, ok bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", 0, false
	}
	leaf := r.TLS.PeerCertificates[0]
	fp := FingerprintDER(leaf.Raw)
	id, valid, err := IsCertValid(s.deps.DB, fp, nowUTC())
	if err != nil {
		slog.Error("cert validity lookup failed", "error", err)
		return "", 0, false
	}
	if !valid {
		return "", 0, false
	}
	return fp, id, true
}

// serve runs the per-connection read loop: handshake, then dispatch until the
// connection closes. It registers the connection in the hub for the connection's
// lifetime and updates the observed node_status row.
func (s *TransportServer) serve(ctx context.Context, conn *AgentConn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := s.handshake(ctx, conn); err != nil {
		slog.Warn("agent handshake failed", "node", conn.NodeID, "error", err)
		conn.closeError("handshake failed")
		return
	}

	previous := s.deps.Hub.Register(conn.NodeID, conn)
	if previous != nil {
		previous.close("replaced by newer connection")
	}
	defer s.deps.Hub.Unregister(conn.NodeID, conn)

	s.setOnline(conn.NodeID, true)
	defer s.setOnline(conn.NodeID, false)

	// On disconnect, fail any tasks still in flight on this connection. Deferred
	// before OnConnected so it always runs once the connection is registered.
	if s.deps.OnDisconnected != nil {
		defer s.deps.OnDisconnected(conn)
	}

	if s.deps.OnConnected != nil {
		s.deps.OnConnected(ctx, conn)
	}

	for {
		env, err := conn.readEnvelope(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Info("agent connection closed", "node", conn.NodeID, "reason", err)
			}
			return
		}
		if err := s.dispatch(ctx, conn, env); err != nil {
			slog.Warn("dispatch failed", "node", conn.NodeID, "type", env.Type, "error", err)
			return
		}
	}
}

// handshake reads the node's hello frame, negotiates the protocol version, and
// replies with hello_ack (including whether the node must reconcile).
func (s *TransportServer) handshake(ctx context.Context, conn *AgentConn) error {
	hsCtx, cancel := context.WithTimeout(ctx, handshakeReadTimeout)
	defer cancel()

	env, err := conn.readEnvelope(hsCtx)
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	if env.Type != controlproto.TypeHello {
		return fmt.Errorf("expected hello, got %s", env.Type)
	}
	var hello controlproto.HelloPayload
	if err := env.DecodePayload(&hello); err != nil {
		return fmt.Errorf("decode hello: %w", err)
	}
	negotiated, err := controlproto.NegotiateVersion(hello.ProtocolVersion)
	if err != nil {
		_ = conn.sendMessage(hsCtx, controlproto.TypeError, "", controlproto.ErrorPayload{
			Code: controlproto.ErrCodeVersionIncompatible, Message: err.Error(), Fatal: true,
		})
		return fmt.Errorf("version negotiation: %w", err)
	}
	conn.ProtocolVersion = negotiated

	// Decide whether the node needs to sync against the latest desired config.
	needsSync, desiredVersion := s.reconcileDecision(conn.NodeID, hello.AppliedVersion, hello.ContentHash)
	if err := conn.sendMessage(hsCtx, controlproto.TypeHelloAck, env.ID, controlproto.HelloAckPayload{
		ProtocolVersion: negotiated,
		ServerTime:      nowUTC().Format(time.RFC3339),
		NeedsSync:       needsSync,
		DesiredVersion:  desiredVersion,
	}); err != nil {
		return fmt.Errorf("send hello_ack: %w", err)
	}

	// Record the applied version the node reported.
	s.updateAppliedVersion(conn.NodeID, hello.AppliedVersion, hello.ContentHash)
	return nil
}

// reconcileDecision compares the node's reported applied version and hash to the
// panel's desired config, returning whether a fresh desired_state must be sent
// and the target version.
func (s *TransportServer) reconcileDecision(nodeID uint, appliedVersion int64, appliedHash string) (needsSync bool, desiredVersion int64) {
	var desired DesiredConfig
	if err := s.deps.DB.Where("node_id = ?", nodeID).First(&desired).Error; err != nil {
		// No desired config yet (e.g. un-imported node): nothing to sync.
		return false, 0
	}
	if desired.Version == 0 {
		return false, 0
	}
	if appliedVersion != desired.Version || appliedHash != desired.ContentHash {
		return true, desired.Version
	}
	return false, desired.Version
}

// dispatch routes a received envelope to its handler.
func (s *TransportServer) dispatch(ctx context.Context, conn *AgentConn, env controlproto.Envelope) error {
	switch env.Type {
	case controlproto.TypeHeartbeat:
		return s.handleHeartbeat(ctx, conn, env)
	case controlproto.TypeAck:
		return s.handleAck(conn, env)
	case controlproto.TypeTaskResult:
		return s.handleTaskResult(conn, env)
	case controlproto.TypeImportReport:
		return s.handleImportReport(conn, env)
	case controlproto.TypeError:
		var payload controlproto.ErrorPayload
		_ = env.DecodePayload(&payload)
		slog.Warn("node reported protocol error", "node", conn.NodeID, "code", payload.Code, "msg", payload.Message)
		return nil
	default:
		// Unknown/unsupported message type on this direction is a protocol error.
		return fmt.Errorf("unexpected message type %s", env.Type)
	}
}

func (s *TransportServer) handleHeartbeat(ctx context.Context, conn *AgentConn, env controlproto.Envelope) error {
	var hb controlproto.HeartbeatPayload
	if err := env.DecodePayload(&hb); err != nil {
		return err
	}
	s.touchHeartbeat(conn.NodeID, hb.AppliedVersion)
	return conn.sendMessage(ctx, controlproto.TypeHeartbeatAck, env.ID, controlproto.HeartbeatAckPayload{
		ServerTime: nowUTC().Format(time.RFC3339),
	})
}

func (s *TransportServer) handleAck(conn *AgentConn, env controlproto.Envelope) error {
	var ack controlproto.AckPayload
	if err := env.DecodePayload(&ack); err != nil {
		return err
	}
	updates := map[string]any{
		"applied_version": ack.Version,
		"sync_state":      string(ack.State),
		"content_hash":    ack.ContentHash,
		"last_error":      ack.Error,
		"updated_at":      nowUTC(),
	}
	if err := s.upsertNodeState(conn.NodeID, updates); err != nil {
		return err
	}
	return nil
}

func (s *TransportServer) handleTaskResult(conn *AgentConn, env controlproto.Envelope) error {
	var result controlproto.TaskResultPayload
	if err := env.DecodePayload(&result); err != nil {
		return err
	}
	conn.releaseTask(result.TaskID)
	resultJSON, _ := json.Marshal(result.Result)
	updates := map[string]any{
		"state":       string(result.State),
		"result_json": string(resultJSON),
		"error":       result.Error,
		"updated_at":  nowUTC(),
	}
	if err := s.deps.DB.Model(&Task{}).Where("task_id = ?", result.TaskID).Updates(updates).Error; err != nil {
		return err
	}
	s.audit("task_result", conn.NodeID, result.TaskID, string(result.State))
	return nil
}

// handleImportReport forwards a node's read-only import report to the migration
// sink (if configured). The node is never mutated by this path; the sink only
// records a PENDING import for later admin confirmation.
func (s *TransportServer) handleImportReport(conn *AgentConn, env controlproto.Envelope) error {
	if s.deps.MigrationSink == nil {
		return nil
	}
	var report controlproto.ImportReportPayload
	if err := env.DecodePayload(&report); err != nil {
		return err
	}
	if err := s.deps.MigrationSink.ingestReport(conn.NodeID, report); err != nil {
		slog.Error("ingest import report failed", "node", conn.NodeID, "error", err)
		return nil // non-fatal to the connection
	}
	return nil
}

// PushDesiredState sends the latest desired config to a connected node. It is
// safe to call from admin request handlers; it returns an error if the node is
// offline (desired state is not queued as a "task" — it is reconciled on
// reconnect via the hello handshake).
//
// The desired_configs row stores a MASKED copy (no plaintext secrets), so the
// push path rebuilds the state with decrypted secrets via the desired-state
// authority. The stored content hash (computed over the unmasked state) is sent
// verbatim so the node's applied hash matches.
func (s *TransportServer) PushDesiredState(ctx context.Context, nodeID uint) error {
	conn, ok := s.deps.Hub.Get(nodeID)
	if !ok {
		return fmt.Errorf("node %d is offline", nodeID)
	}
	authority := NewDesiredStateAuthority(s.deps.DB, s.deps.Cipher)
	payload, err := authority.BuildPushable(nodeID)
	if err != nil {
		return fmt.Errorf("build pushable desired state: %w", err)
	}
	return conn.sendMessage(ctx, controlproto.TypeDesiredState, "", payload)
}

// --- node_status persistence helpers ---

func (s *TransportServer) setOnline(nodeID uint, online bool) {
	updates := map[string]any{"online": online, "updated_at": nowUTC()}
	if online {
		now := nowUTC()
		updates["last_heartbeat"] = &now
	}
	_ = s.upsertNodeState(nodeID, updates)
}

func (s *TransportServer) touchHeartbeat(nodeID uint, appliedVersion int64) {
	now := nowUTC()
	_ = s.upsertNodeState(nodeID, map[string]any{
		"online":          true,
		"applied_version": appliedVersion,
		"last_heartbeat":  &now,
		"updated_at":      now,
	})
}

func (s *TransportServer) updateAppliedVersion(nodeID uint, version int64, hash string) {
	_ = s.upsertNodeState(nodeID, map[string]any{
		"applied_version": version,
		"content_hash":    hash,
		"updated_at":      nowUTC(),
	})
}

// upsertNodeState creates or updates the single node_status row for nodeID.
func (s *TransportServer) upsertNodeState(nodeID uint, updates map[string]any) error {
	return s.deps.DB.Transaction(func(tx *gorm.DB) error {
		var existing NodeState
		err := tx.Where("node_id = ?", nodeID).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			row := NodeState{NodeID: nodeID, SyncState: SyncStateWaiting}
			applyStateUpdates(&row, updates)
			return tx.Create(&row).Error
		}
		if err != nil {
			return err
		}
		return tx.Model(&NodeState{}).Where("node_id = ?", nodeID).Updates(updates).Error
	})
}

// applyStateUpdates copies known update keys onto a NodeState for the create path
// (GORM's Updates map does not apply to a fresh struct create).
func applyStateUpdates(row *NodeState, updates map[string]any) {
	if v, ok := updates["online"].(bool); ok {
		row.Online = v
	}
	if v, ok := updates["applied_version"].(int64); ok {
		row.AppliedVersion = v
	}
	if v, ok := updates["sync_state"].(string); ok && v != "" {
		row.SyncState = v
	}
	if v, ok := updates["content_hash"].(string); ok {
		row.ContentHash = v
	}
	if v, ok := updates["last_error"].(string); ok {
		row.LastError = v
	}
	if v, ok := updates["last_heartbeat"].(*time.Time); ok {
		row.LastHeartbeat = v
	}
}

func (s *TransportServer) audit(action string, nodeID uint, resource, result string) {
	entry := AuditLog{
		TS:             nowUTC(),
		Action:         action,
		TargetNode:     nodeID,
		TargetResource: resource,
		Result:         result,
		Source:         "control-plane",
	}
	if err := s.deps.DB.Create(&entry).Error; err != nil {
		slog.Error("write audit log failed", "action", action, "error", err)
	}
}

// SweepOffline marks nodes offline whose last heartbeat is older than the
// offline threshold. Intended to be called periodically by the panel. It only
// updates the observed state; it never touches the node's data plane.
func (s *TransportServer) SweepOffline(interval time.Duration, multiplier int) error {
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	if multiplier <= 0 {
		multiplier = DefaultOfflineMultiplier
	}
	threshold := nowUTC().Add(-time.Duration(multiplier) * interval)
	return s.deps.DB.Model(&NodeState{}).
		Where("online = ? AND (last_heartbeat IS NULL OR last_heartbeat < ?)", true, threshold).
		Updates(map[string]any{"online": false, "updated_at": nowUTC()}).Error
}

// TLSConfig builds a tls.Config that verifies node client certificates against
// the intermediate CA. Registration and agent routes share the listener; client
// certs are requested but verification of presence is enforced per-route
// (handleAgent requires one, handleRegister does not). Exported for cmd/panel to
// configure the node接入 listener.
func (s *TransportServer) ListenerTLSConfig(serverCert tls.Certificate) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(s.deps.CA.Certificate())
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		// Request a client cert but allow its absence so the registration route
		// (which has no cert yet) still works; handleAgent rejects missing certs.
		ClientAuth: tls.VerifyClientCertIfGiven,
		MinVersion: tls.VersionTLS12,
	}
}

func writeTransportJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeTransportError(w http.ResponseWriter, status int, message string) {
	writeTransportJSON(w, status, map[string]string{"error": message})
}
