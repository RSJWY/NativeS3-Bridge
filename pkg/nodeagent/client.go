package nodeagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// AgentVersion identifies this agent build in the hello handshake.
const AgentVersion = "1.0.0"

// Client tuning defaults. These bound reconnection and heartbeat behavior so a
// panel outage produces steady, jittered retries rather than a tight loop.
const (
	DefaultHeartbeatInterval = 15 * time.Second
	DefaultDialTimeout       = 15 * time.Second
	DefaultMinBackoff        = 1 * time.Second
	DefaultMaxBackoff        = 60 * time.Second
	clientMaxMessageBytes    = 1 << 20 // 1 MiB, matches panel DefaultMaxMessageBytes
)

// ClientConfig configures the node agent's control-plane connection. The S3 data
// plane runs independently of this client: if the panel is unreachable, the node
// keeps serving S3 from its last-applied local DB (design §7 / safety net A).
type ClientConfig struct {
	// AgentURL is the panel's mTLS WebSocket endpoint, e.g. wss://panel:PORT/agent.
	AgentURL string
	NodeID   int64
	Identity Identity

	HeartbeatInterval time.Duration
	DialTimeout       time.Duration
	MinBackoff        time.Duration
	MaxBackoff        time.Duration
}

func (c *ClientConfig) applyDefaults() {
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.MinBackoff <= 0 {
		c.MinBackoff = DefaultMinBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultMaxBackoff
	}
}

// Client is the node-side control-plane agent. It dials the panel over mTLS,
// performs the hello handshake, applies desired state, runs one-shot tasks, and
// maintains heartbeats. All control-plane failures are non-fatal to the S3 data
// plane; the client simply reconnects with backoff.
type Client struct {
	cfg      ClientConfig
	db       *gorm.DB
	executor *Executor
	runner   TaskRunner

	writeMu sync.Mutex
	ws      *websocket.Conn
}

// TaskRunner executes the predefined one-shot tasks. It is an interface so the
// node binary can wire in the concrete log/storage implementations without this
// package depending on webadmin/storage internals.
type TaskRunner interface {
	Run(ctx context.Context, task controlproto.TaskPayload) controlproto.TaskResultPayload
}

// NewClient builds a node agent client. runner may be nil, in which case task
// messages are rejected with a failed result.
func NewClient(cfg ClientConfig, gdb *gorm.DB, executor *Executor, runner TaskRunner) *Client {
	cfg.applyDefaults()
	return &Client{cfg: cfg, db: gdb, executor: executor, runner: runner}
}

// Run connects and services the control plane until ctx is cancelled, retrying
// with exponential backoff (capped, jittered) across disconnects. Run never
// returns an error for transient connection failures; it returns only when ctx
// is done.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.MinBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Warn("control-plane connection ended", "error", err, "retry_in", backoff)
		}
		// Sleep with jitter, then grow backoff toward the cap.
		if !sleepWithContext(ctx, jitter(backoff)) {
			return ctx.Err()
		}
		backoff = nextBackoff(backoff, c.cfg.MaxBackoff)
		if err == nil {
			// A clean disconnect resets backoff so the next reconnect is prompt.
			backoff = c.cfg.MinBackoff
		}
	}
}

// connectAndServe dials the panel, runs the handshake, and serves the connection
// until it closes or ctx is cancelled.
func (c *Client) connectAndServe(ctx context.Context) error {
	tlsConfig, err := c.clientTLS()
	if err != nil {
		return fmt.Errorf("build client TLS: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()
	ws, _, err := websocket.Dial(dialCtx, c.cfg.AgentURL, &websocket.DialOptions{
		HTTPClient: tlsHTTPClient(tlsConfig),
	})
	if err != nil {
		return fmt.Errorf("dial panel: %w", err)
	}
	ws.SetReadLimit(clientMaxMessageBytes)
	c.setWS(ws)
	defer func() {
		_ = ws.Close(websocket.StatusNormalClosure, "agent shutting down")
		c.setWS(nil)
	}()

	if err := c.handshake(ctx, ws); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()

	// Heartbeat goroutine runs until the serve loop exits.
	go c.heartbeatLoop(serveCtx, ws)

	return c.serveLoop(serveCtx, ws)
}

// handshake sends hello (with the locally-applied version + hash) and processes
// hello_ack. If the panel signals a version incompatibility it returns an error
// so the caller stops retrying tightly.
func (c *Client) handshake(ctx context.Context, ws *websocket.Conn) error {
	meta, err := LoadMeta(c.db)
	if err != nil {
		return fmt.Errorf("load agent meta: %w", err)
	}
	localHash, err := c.executor.LocalContentHash()
	if err != nil {
		return fmt.Errorf("compute local hash: %w", err)
	}
	hello := controlproto.HelloPayload{
		ProtocolVersion: controlproto.ProtocolVersion,
		NodeID:          fmt.Sprintf("%d", c.cfg.NodeID),
		AgentVersion:    AgentVersion,
		AppliedVersion:  meta.AppliedVersion,
		ContentHash:     localHash,
	}
	if err := c.sendMessage(ctx, ws, controlproto.TypeHello, "", hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	env, err := c.readEnvelope(ctx, ws)
	if err != nil {
		return fmt.Errorf("read hello_ack: %w", err)
	}
	if env.Type == controlproto.TypeError {
		var perr controlproto.ErrorPayload
		_ = env.DecodePayload(&perr)
		return fmt.Errorf("panel rejected connection: %s (%s)", perr.Message, perr.Code)
	}
	if env.Type != controlproto.TypeHelloAck {
		return fmt.Errorf("expected hello_ack, got %s", env.Type)
	}
	var ack controlproto.HelloAckPayload
	if err := env.DecodePayload(&ack); err != nil {
		return fmt.Errorf("decode hello_ack: %w", err)
	}
	if !controlproto.IsCompatible(ack.ProtocolVersion) {
		return fmt.Errorf("negotiated protocol version %d is not supported", ack.ProtocolVersion)
	}
	// If the panel says we need to sync, it will push desired_state right after;
	// no action needed here beyond logging.
	if ack.NeedsSync {
		slog.Info("panel requests desired-state sync", "target_version", ack.DesiredVersion)
	}
	return nil
}

// serveLoop reads and dispatches control-plane frames until the connection ends.
func (c *Client) serveLoop(ctx context.Context, ws *websocket.Conn) error {
	for {
		env, err := c.readEnvelope(ctx, ws)
		if err != nil {
			return err
		}
		switch env.Type {
		case controlproto.TypeDesiredState:
			c.handleDesiredState(ctx, ws, env)
		case controlproto.TypeTask:
			c.handleTask(ctx, ws, env)
		case controlproto.TypeImportRequest:
			c.handleImportRequest(ctx, ws, env)
		case controlproto.TypeHeartbeatAck:
			// clock info only; ignored for now.
		case controlproto.TypeError:
			var perr controlproto.ErrorPayload
			_ = env.DecodePayload(&perr)
			slog.Warn("panel reported error", "code", perr.Code, "msg", perr.Message)
			if perr.Fatal {
				return fmt.Errorf("panel fatal error: %s", perr.Message)
			}
		default:
			slog.Warn("ignoring unexpected message", "type", env.Type)
		}
	}
}

// handleDesiredState applies a pushed desired state transactionally and replies
// with an ack carrying the resulting sync state. A failed apply leaves the node's
// prior config intact and reports state=failed.
func (c *Client) handleDesiredState(ctx context.Context, ws *websocket.Conn, env controlproto.Envelope) {
	var payload controlproto.DesiredStatePayload
	if err := env.DecodePayload(&payload); err != nil {
		slog.Error("decode desired_state failed", "error", err)
		return
	}

	appliedHash, err := c.executor.Apply(payload.Content)
	if err != nil {
		slog.Error("apply desired_state failed", "version", payload.Version, "error", err)
		_ = c.sendMessage(ctx, ws, controlproto.TypeAck, env.ID, controlproto.AckPayload{
			Version: payload.Version,
			State:   controlproto.SyncStateFailed,
			Error:   err.Error(),
		})
		return
	}

	state := controlproto.SyncStateSynced
	// If the panel included a content hash, verify our applied state matches it.
	// A mismatch indicates the panel and node disagree on the schema/content and
	// is surfaced as drift rather than a clean sync.
	if payload.ContentHash != "" && payload.ContentHash != appliedHash {
		state = controlproto.SyncStateDrift
	}

	if err := SaveMeta(c.db, payload.Version, appliedHash); err != nil {
		slog.Error("persist applied version failed", "error", err)
		_ = c.sendMessage(ctx, ws, controlproto.TypeAck, env.ID, controlproto.AckPayload{
			Version: payload.Version,
			State:   controlproto.SyncStateFailed,
			Error:   fmt.Sprintf("persist applied version: %v", err),
		})
		return
	}

	slog.Info("applied desired_state", "version", payload.Version, "state", state)
	_ = c.sendMessage(ctx, ws, controlproto.TypeAck, env.ID, controlproto.AckPayload{
		Version:     payload.Version,
		State:       state,
		ContentHash: appliedHash,
	})
}

// handleImportRequest replies with a read-only snapshot of the node's current
// local business config (credentials with plaintext secrets, buckets, webhooks)
// so the panel can present an import summary for admin confirmation. It is
// strictly read-only: the node's config is never modified by an import request
// (the migration red line — the panel must not write business config to the
// node before the admin confirms, design §8.3).
func (c *Client) handleImportRequest(ctx context.Context, ws *websocket.Conn, env controlproto.Envelope) {
	state, err := c.executor.LocalState()
	if err != nil {
		slog.Error("build local state for import failed", "error", err)
		_ = c.sendMessage(ctx, ws, controlproto.TypeError, env.ID, controlproto.ErrorPayload{
			Code: controlproto.ErrCodeInternal, Message: "build local state: " + err.Error(),
		})
		return
	}
	report := controlproto.ImportReportPayload{
		State:            state,
		CredentialCount:  len(state.Credentials),
		BucketCount:      len(state.Buckets),
		WebhookCount:     len(state.Webhooks),
		LocalContentHash: state.ContentHash(),
	}
	if err := c.sendMessage(ctx, ws, controlproto.TypeImportReport, env.ID, report); err != nil {
		slog.Error("send import report failed", "error", err)
	}
}

// handleTask executes a one-shot task with idempotency. A duplicate task ID
// returns the previously-cached result without re-executing (critical for
// high-risk reconcile-apply). Results are recorded before being sent.
func (c *Client) handleTask(ctx context.Context, ws *websocket.Conn, env controlproto.Envelope) {
	var task controlproto.TaskPayload
	if err := env.DecodePayload(&task); err != nil {
		slog.Error("decode task failed", "error", err)
		return
	}

	// Idempotency: if we already have a result for this task ID, resend it.
	if cached, ok := c.cachedTaskResult(task.TaskID); ok {
		_ = c.sendMessage(ctx, ws, controlproto.TypeTaskResult, env.ID, cached)
		return
	}

	var result controlproto.TaskResultPayload
	if c.runner == nil {
		result = controlproto.TaskResultPayload{
			TaskID: task.TaskID,
			Type:   task.Type,
			State:  controlproto.TaskStateFailed,
			Error:  "node does not support tasks",
		}
	} else {
		result = c.runner.Run(ctx, task)
	}
	result.TaskID = task.TaskID
	result.Type = task.Type

	// Record before sending so a crash after send still leaves an idempotency
	// record; a duplicate delivery then resends rather than re-executing.
	if err := c.recordTaskResult(task, result); err != nil {
		slog.Error("record task result failed", "task", task.TaskID, "error", err)
	}
	_ = c.sendMessage(ctx, ws, controlproto.TypeTaskResult, env.ID, result)
}

// cachedTaskResult returns a previously-recorded result for taskID, if any.
func (c *Client) cachedTaskResult(taskID string) (controlproto.TaskResultPayload, bool) {
	var rec AppliedTask
	if err := c.db.Where("task_id = ?", taskID).First(&rec).Error; err != nil {
		return controlproto.TaskResultPayload{}, false
	}
	var result controlproto.TaskResult
	if rec.ResultJSON != "" {
		_ = json.Unmarshal([]byte(rec.ResultJSON), &result)
	}
	return controlproto.TaskResultPayload{
		TaskID: rec.TaskID,
		Type:   controlproto.TaskType(rec.Type),
		State:  controlproto.TaskState(rec.State),
		Result: result,
	}, true
}

// recordTaskResult persists the task idempotency record.
func (c *Client) recordTaskResult(task controlproto.TaskPayload, result controlproto.TaskResultPayload) error {
	resultJSON, _ := json.Marshal(result.Result)
	rec := AppliedTask{
		TaskID:     task.TaskID,
		Type:       string(task.Type),
		State:      string(result.State),
		ResultJSON: string(resultJSON),
		CreatedAt:  time.Now().UTC(),
	}
	return c.db.Create(&rec).Error
}

// heartbeatLoop sends periodic heartbeats until ctx is cancelled.
func (c *Client) heartbeatLoop(ctx context.Context, ws *websocket.Conn) {
	ticker := time.NewTicker(c.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			meta, err := LoadMeta(c.db)
			if err != nil {
				slog.Warn("heartbeat: load meta failed", "error", err)
				continue
			}
			if err := c.sendMessage(ctx, ws, controlproto.TypeHeartbeat, "", controlproto.HeartbeatPayload{
				AppliedVersion: meta.AppliedVersion,
			}); err != nil {
				slog.Debug("heartbeat send failed", "error", err)
				return
			}
		}
	}
}

// --- wire helpers ---

func (c *Client) setWS(ws *websocket.Conn) {
	c.writeMu.Lock()
	c.ws = ws
	c.writeMu.Unlock()
}

func (c *Client) sendMessage(ctx context.Context, ws *websocket.Conn, msgType controlproto.MessageType, id string, payload any) error {
	env, err := controlproto.NewEnvelope(msgType, id, payload)
	if err != nil {
		return err
	}
	data, err := env.Encode()
	if err != nil {
		return err
	}
	// Serialize writes: heartbeat and serve-loop replies can race otherwise.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return ws.Write(ctx, websocket.MessageText, data)
}

func (c *Client) readEnvelope(ctx context.Context, ws *websocket.Conn) (controlproto.Envelope, error) {
	_, data, err := ws.Read(ctx)
	if err != nil {
		return controlproto.Envelope{}, err
	}
	return controlproto.DecodeEnvelope(data)
}

// clientTLS builds the mTLS client config: node client cert + panel CA trust for
// verifying the panel's server certificate.
func (c *Client) clientTLS() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.cfg.Identity.CertFile, c.cfg.Identity.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert/key: %w", err)
	}
	pool := x509.NewCertPool()
	if c.cfg.Identity.CAFile != "" {
		caPEM, err := os.ReadFile(c.cfg.Identity.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read panel CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("panel CA file contains no certificates")
		}
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// tlsHTTPClient builds an *http.Client whose transport uses the given TLS
// config. coder/websocket dials over this client so mTLS applies to the
// WebSocket upgrade request.
func tlsHTTPClient(tlsConfig *tls.Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   tlsConfig,
			ForceAttemptHTTP2: false,
		},
	}
}

// --- backoff helpers ---

func nextBackoff(current, max time.Duration) time.Duration {
	next := time.Duration(math.Min(float64(current)*2, float64(max)))
	if next < current {
		next = max
	}
	return next
}

// jitter returns d scaled by a random factor in [0.5, 1.5) to decorrelate
// reconnect storms across nodes.
func jitter(d time.Duration) time.Duration {
	factor := 0.5 + rand.Float64()
	return time.Duration(float64(d) * factor)
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
