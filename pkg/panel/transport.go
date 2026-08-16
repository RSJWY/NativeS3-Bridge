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
	"strings"
	"time"

	"github.com/coder/websocket"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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
	nodeStateMaxBusyRetries  = 5
	nodeStateBusyRetryDelay  = 5 * time.Millisecond
	// maxReportedRegionBytes 与 NodeState.Region 的列宽一致。
	maxReportedRegionBytes = 64
)

var ErrAuthoritativeConfigCapabilityRequired = errors.New("agent upgrade required for authoritative config")

// TransportDeps are the collaborators the transport server needs. Keeping them
// as an interface-free struct of concrete dependencies avoids premature
// abstraction; the fields are all owned by the panel process.
type TransportDeps struct {
	DB          *gorm.DB
	CA          *CA
	Hub         *Hub
	Cipher      *SecretCipher
	ClientCTTL  time.Duration
	OnConnected func(ctx context.Context, conn *AgentConn) // optional connection observer
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

// Handler returns the HTTP handler exposing /register, /agent, and /renew.
func (s *TransportServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/register", s.handleRegister)
	mux.HandleFunc("/agent", s.handleAgent)
	mux.HandleFunc("/renew", s.handleRenew)
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

// handleRenew issues a renewed client certificate to an already-authenticated
// node. Identity is taken solely from the mTLS client certificate (not the
// request body) to prevent cross-node certificate substitution. The CSR's CN
// must match the node identity bound to the presenting certificate. The old
// certificate is NOT revoked here — that happens when the new cert first
// successfully connects via /agent (D1).
func (s *TransportServer) handleRenew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	fingerprint, nodeID, ok := s.authenticateMTLS(r)
	if !ok {
		s.audit("node_cert_renew", 0, "", "denied")
		writeTransportError(w, http.StatusUnauthorized, "valid client certificate required")
		return
	}
	_ = fingerprint
	var req renewRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, registrationBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.audit("node_cert_renew", nodeID, "", "denied")
		writeTransportError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CSRPEM == "" {
		s.audit("node_cert_renew", nodeID, "", "denied")
		writeTransportError(w, http.StatusBadRequest, "csr_pem is required")
		return
	}

	csr, err := parseCSRPEM([]byte(req.CSRPEM))
	if err != nil {
		s.audit("node_cert_renew", nodeID, "", "denied")
		writeTransportError(w, http.StatusBadRequest, "invalid CSR")
		return
	}
	if err := csr.CheckSignature(); err != nil {
		s.audit("node_cert_renew", nodeID, "", "denied")
		writeTransportError(w, http.StatusBadRequest, "CSR signature invalid")
		return
	}
	if csr.Subject.CommonName != nodeSubject(nodeID).CommonName {
		s.audit("node_cert_renew", nodeID, "", "denied")
		writeTransportError(w, http.StatusBadRequest, "CSR subject does not match node identity")
		return
	}

	now := nowUTC()
	signed, err := s.deps.CA.SignNodeCSR([]byte(req.CSRPEM), nodeID, s.deps.ClientCTTL, now)
	if err != nil {
		slog.Error("renew: sign CSR failed", "node", nodeID, "error", err)
		s.audit("node_cert_renew", nodeID, "", "denied")
		writeTransportError(w, http.StatusInternalServerError, "certificate signing failed")
		return
	}
	cert := NodeCert{
		NodeID:      nodeID,
		Fingerprint: signed.Fingerprint,
		Serial:      signed.Serial,
		NotBefore:   signed.NotBefore,
		NotAfter:    signed.NotAfter,
	}
	if err := s.deps.DB.Create(&cert).Error; err != nil {
		slog.Error("renew: persist cert failed", "node", nodeID, "error", err)
		s.audit("node_cert_renew", nodeID, "", "denied")
		writeTransportError(w, http.StatusInternalServerError, "certificate persistence failed")
		return
	}
	s.audit("node_cert_renew", nodeID, signed.Fingerprint, "issued")
	writeTransportJSON(w, http.StatusOK, registerResponse{
		CertPEM:   string(signed.CertPEM),
		CACertPEM: string(s.deps.CA.CertificatePEM()),
		NotAfter:  signed.NotAfter.Format(time.RFC3339),
	})
}

// renewRequest is the body for POST /renew: only the CSR, identity comes from
// the mTLS client certificate.
type renewRequest struct {
	CSRPEM string `json:"csr_pem"`
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
	// Activate the cert (D1): first connection with this fingerprint marks it
	// activated and revokes all other unrevoked certs for the same node.
	// Failure is non-fatal — the connection proceeds (R2.3).
	if err := ActivateCert(s.deps.DB, fp, id, nowUTC()); err != nil {
		slog.Error("activate node certificate failed", "node", id, "fingerprint", fp, "error", err)
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

	s.setOnline(conn.NodeID, true)
	defer s.disconnect(conn)

	// On disconnect, fail any tasks still in flight on this connection. Deferred
	// before OnConnected so it always runs once the connection is registered.
	if s.deps.OnDisconnected != nil {
		defer s.deps.OnDisconnected(conn)
	}

	if s.deps.OnConnected != nil {
		s.deps.OnConnected(ctx, conn)
	}
	if conn.NeedsSync {
		if err := s.PushDesiredState(ctx, conn.NodeID); err != nil {
			slog.Warn("automatic desired-state reconcile failed", "node", conn.NodeID, "error", err)
		}
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

func (s *TransportServer) disconnect(conn *AgentConn) {
	s.deps.Hub.Unregister(conn.NodeID, conn)
	if !s.deps.Hub.IsOnline(conn.NodeID) {
		s.setOnline(conn.NodeID, false)
	}
	// A replacement can register after the offline check but before the write
	// above commits. Recheck so the obsolete disconnect cannot win the final DB
	// write and leave a live replacement displayed as offline.
	if s.deps.Hub.IsOnline(conn.NodeID) {
		s.setOnline(conn.NodeID, true)
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
	conn.Capabilities = append([]string(nil), hello.Capabilities...)
	conn.AppliedVersion = hello.AppliedVersion
	conn.ContentHash = hello.ContentHash

	// Decide whether the node needs to sync against the latest desired config.
	needsSync, desiredVersion := s.reconcileDecision(conn.NodeID, hello.AppliedVersion, hello.ContentHash)
	conn.NeedsSync = needsSync
	if err := conn.sendMessage(hsCtx, controlproto.TypeHelloAck, env.ID, controlproto.HelloAckPayload{
		ProtocolVersion: negotiated,
		ServerTime:      nowUTC().Format(time.RFC3339),
		NeedsSync:       needsSync,
		DesiredVersion:  desiredVersion,
	}); err != nil {
		return fmt.Errorf("send hello_ack: %w", err)
	}

	var snapshotErr error
	if desiredVersion > 0 {
		_, snapshotErr = NewDesiredStateAuthority(s.deps.DB, s.deps.Cipher).BuildPushable(conn.NodeID)
	}

	// Record the applied version the node reported.
	s.recordHelloObservation(conn.NodeID, hello.AppliedVersion, hello.ContentHash, hello.Region)
	if desiredVersion > 0 && !conn.Supports(controlproto.CapabilityAuthoritativeConfigV1) {
		s.recordSyncFailure(conn.NodeID, "agent upgrade required: authoritative config capability is missing")
	} else if snapshotErr != nil {
		s.recordSyncFailure(conn.NodeID, desiredSnapshotFailureMessage(snapshotErr))
	} else if desiredVersion > 0 && !needsSync {
		_ = s.upsertNodeState(conn.NodeID, map[string]any{
			"sync_state": SyncStateSynced, "last_error": "", "updated_at": nowUTC(),
		})
	}
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
	s.touchHeartbeat(conn.NodeID, hb)
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
		"sync_state": string(ack.State),
		"last_error": "",
		"updated_at": nowUTC(),
	}
	switch ack.State {
	case controlproto.SyncStateFailed:
		// A failed apply leaves the node on its previously applied version/hash.
		// Preserve those observed fields instead of recording the attempted target
		// version as though it had partially succeeded.
		updates["last_error"] = safeReportedApplyError(ack.Error)
	case controlproto.SyncStateSynced, controlproto.SyncStateDrift:
		updates["applied_version"] = ack.Version
		updates["content_hash"] = ack.ContentHash
		if ack.State == controlproto.SyncStateSynced {
			var desired DesiredConfig
			if err := s.deps.DB.Where("node_id = ?", conn.NodeID).First(&desired).Error; err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				updates["sync_state"] = SyncStateDrift
				updates["last_error"] = "node reported a version with no published desired state"
			} else if ack.Version != desired.Version || ack.ContentHash != desired.ContentHash {
				updates["sync_state"] = SyncStateDrift
				updates["last_error"] = "node reported content that does not match the published desired state"
			}
		}
	default:
		return fmt.Errorf("invalid desired-state ack status %q", ack.State)
	}
	if err := s.upsertNodeState(conn.NodeID, updates); err != nil {
		return err
	}
	return nil
}

func safeReportedApplyError(message string) string {
	message = strings.TrimSpace(message)
	for _, fragment := range []string{
		"desired content hash mismatch",
		"validate desired state:",
		"retained data prevents declaring bucket",
	} {
		if strings.Contains(message, fragment) {
			runes := []rune(message)
			if len(runes) > 500 {
				return string(runes[:500])
			}
			return message
		}
	}
	if message == "" {
		return "node failed to apply desired state"
	}
	return "node failed to apply desired state"
}

func (s *TransportServer) handleTaskResult(conn *AgentConn, env controlproto.Envelope) error {
	var result controlproto.TaskResultPayload
	if err := env.DecodePayload(&result); err != nil {
		return err
	}
	var task Task
	if err := s.deps.DB.Where("task_id = ? AND node_id = ?", result.TaskID, conn.NodeID).First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	taskType := controlproto.TaskType(task.Type)
	if result.Type != "" && result.Type != taskType {
		return nil
	}
	// Only a result that resolves to this connection's persisted task and agrees
	// with its type acknowledges the in-flight dispatch. A mismatched frame must
	// not free backpressure capacity while the real task is still running.
	conn.releaseTask(result.TaskID)
	state := result.State
	if state != controlproto.TaskStateSuccess && state != controlproto.TaskStateFailed {
		state = controlproto.TaskStateFailed
		result.Error = "node returned a non-terminal task state"
	}
	cleanResult := sanitizeTaskResult(taskType, result.Result)
	resultJSON, _ := json.Marshal(cleanResult)
	updates := map[string]any{
		"state":       string(state),
		"result_json": string(resultJSON),
		"error":       sanitizeTaskError(taskType, result.Error),
		"updated_at":  nowUTC(),
	}
	updated := s.deps.DB.Model(&Task{}).
		Where("id = ? AND state IN ?", task.ID, []string{
			string(controlproto.TaskStatePending), string(controlproto.TaskStateRunning),
		}).Updates(updates)
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected == 0 {
		return nil
	}
	s.audit("task_result", conn.NodeID, result.TaskID, string(state))
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
// The desired_configs row stores an exact schema-versioned snapshot with
// credential ciphertext (never plaintext). The push path decrypts only that
// immutable snapshot, recomputes its plaintext-derived hash, and refuses to
// send when the persisted content/hash pair is not self-consistent.
func (s *TransportServer) PushDesiredState(ctx context.Context, nodeID uint) error {
	conn, ok := s.deps.Hub.Get(nodeID)
	if !ok {
		return fmt.Errorf("node %d is offline", nodeID)
	}
	if !conn.Supports(controlproto.CapabilityAuthoritativeConfigV1) {
		message := "agent upgrade required: authoritative config capability is missing"
		s.recordSyncFailure(nodeID, message)
		return fmt.Errorf("%w: %s", ErrAuthoritativeConfigCapabilityRequired, message)
	}
	authority := NewDesiredStateAuthority(s.deps.DB, s.deps.Cipher)
	payload, err := authority.BuildPushable(nodeID)
	if err != nil {
		message := desiredSnapshotFailureMessage(err)
		s.recordSyncFailure(nodeID, message)
		return fmt.Errorf("%s: %w", message, err)
	}
	_ = s.upsertNodeState(nodeID, map[string]any{
		"sync_state": SyncStateWaiting, "last_error": "", "updated_at": nowUTC(),
	})
	if err := conn.sendMessage(ctx, controlproto.TypeDesiredState, "", payload); err != nil {
		s.recordSyncFailure(nodeID, "send desired state failed")
		return fmt.Errorf("send desired state: %w", err)
	}
	return nil
}

func desiredSnapshotFailureMessage(err error) string {
	if errors.Is(err, ErrDesiredSnapshotRepublishRequired) {
		return "published snapshot is legacy and must be republished"
	}
	if errors.Is(err, ErrDesiredSnapshotHashMismatch) {
		return "published snapshot failed integrity verification"
	}
	return "published desired snapshot is unavailable"
}

func (s *TransportServer) recordSyncFailure(nodeID uint, message string) {
	_ = s.upsertNodeState(nodeID, map[string]any{
		"sync_state": SyncStateFailed, "last_error": message, "updated_at": nowUTC(),
	})
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

func (s *TransportServer) touchHeartbeat(nodeID uint, hb controlproto.HeartbeatPayload) {
	now := nowUTC()
	updates := map[string]any{
		"online":          true,
		"applied_version": hb.AppliedVersion,
		"last_heartbeat":  &now,
		"updated_at":      now,
	}
	if telemetry, ok := hb.Telemetry(); ok {
		// 完整有效快照:持久化实际值与节点观测时间(不是 Panel 收包时间)。
		updates["used_bytes_total"] = &telemetry.UsedBytesTotal
		updates["object_count"] = &telemetry.ObjectCount
		observed := telemetry.ObservedAt
		updates["telemetry_observed_at"] = &observed
		updates["telemetry_valid"] = true
	} else {
		// 旧版/字段不完整的心跳:当前观测不可用,不能把旧值伪装成当前观测。
		// 保留旧列值供排查,但 Valid=false 使聚合口径把它排除。
		updates["telemetry_valid"] = false
	}
	_ = s.upsertNodeState(nodeID, updates)
}

// recordHelloObservation 落库节点在 hello 里自报的观测量。region 一并写入(包括
// 空串):节点降级到不上报 region 的旧版本时,展示为"未上报"比留着上一次连接的
// 旧值更诚实——Panel 不知道当前 agent 实际在用哪个区域。
func (s *TransportServer) recordHelloObservation(nodeID uint, version int64, hash, region string) {
	_ = s.upsertNodeState(nodeID, map[string]any{
		"applied_version": version,
		"content_hash":    hash,
		"region":          sanitizeReportedRegion(region),
		"updated_at":      nowUTC(),
	})
}

// sanitizeReportedRegion 归一化节点自报的 region。值来自节点本地 yaml,虽然经过
// mTLS 通道但仍是"节点说了算"的自由文本,且会直接进管理端页面:去掉首尾空白与
// 控制字符,并截断到列宽 64,避免脏值撑坏展示或写库失败。
func sanitizeReportedRegion(region string) string {
	region = strings.TrimSpace(region)
	region = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, region)
	if len(region) > maxReportedRegionBytes {
		region = region[:maxReportedRegionBytes]
	}
	return region
}

// upsertNodeState creates or updates the single node_status row for nodeID.
func (s *TransportServer) upsertNodeState(nodeID uint, updates map[string]any) error {
	ctx := context.Background()
	if s.deps.DB != nil && s.deps.DB.Statement != nil && s.deps.DB.Statement.Context != nil {
		ctx = s.deps.DB.Statement.Context
	}
	var err error
	for attempt := 0; attempt <= nodeStateMaxBusyRetries; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		err = s.upsertNodeStateOnce(nodeID, updates)
		if err == nil {
			return nil
		}
		// Context 取消优先:ctx 取消时底层驱动可能抛出难以归类的错误——GORM 用
		// fmt.Errorf("%v; %w") 把 busy 错误与事务清理错误合并,原始类型从错误链
		// 丢失,isSQLiteBusyError 无法识别。只要 ctx 已取消就返回干净的 context
		// 错误,保证调用方 errors.Is(err, context.Canceled) 成立,并停止重试。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if s.deps.DB.Dialector.Name() != "sqlite" || !isSQLiteBusyError(err) {
			return err
		}
		if attempt == nodeStateMaxBusyRetries {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * nodeStateBusyRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			// Context 取消后立即返回干净的 context 错误,不合并之前的 busy 错误。
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func (s *TransportServer) upsertNodeStateOnce(nodeID uint, updates map[string]any) error {
	row := NodeState{NodeID: nodeID, SyncState: SyncStateWaiting}
	applyStateUpdates(&row, updates)
	return s.deps.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "node_id"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&row).Error
}

func isSQLiteBusyError(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	// SQLite extended result codes retain the primary result in the low byte.
	// SQLITE_BUSY is 5 and SQLITE_LOCKED is 6.
	switch coded.Code() & 0xff {
	case 5, 6:
		return true
	default:
		return false
	}
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
	if v, ok := updates["region"].(string); ok {
		row.Region = v
	}
	if v, ok := updates["last_error"].(string); ok {
		row.LastError = v
	}
	if v, ok := updates["last_heartbeat"].(*time.Time); ok {
		row.LastHeartbeat = v
	}
	if v, ok := updates["used_bytes_total"].(*int64); ok {
		row.UsedBytesTotal = v
	}
	if v, ok := updates["object_count"].(*int64); ok {
		row.ObjectCount = v
	}
	if v, ok := updates["telemetry_observed_at"].(*time.Time); ok {
		row.TelemetryObservedAt = v
	}
	if v, ok := updates["telemetry_valid"].(bool); ok {
		row.TelemetryValid = v
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
