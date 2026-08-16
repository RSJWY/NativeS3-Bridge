package nodeagent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// AgentVersion identifies this agent build in the hello handshake.
const AgentVersion = "1.0.0"

// certErrLogEvery 与 certErrorReporter.log 配合实现降频:证书类永久错误的
// 首次失败立即报 Error,之后每 N 次重连重复一次(60s 上限退避下约 10 分钟
// 一条),避免每次重连刷屏,但绝不完全静默(R4.3)。
const certErrLogEvery = 10

// certError 是证书类永久错误的分类结果。两条路径失败位置完全不同,必须
// 分别识别并分别报错(design §3.3):
//
//   - 路径 A(local):本地证书已过期或损坏,在 TLS 握手阶段就被本机拒绝,
//     请求根本没到 panel。确定性判定,不依赖错误字符串匹配。
//   - 路径 B(rejected):panel 返回 401 -- 证书被吊销、节点被停用/退役、
//     或指纹不在证书表内。失败发生在 HTTP 层。
type certError struct {
	kind string // "local" | "rejected"
	err  error
}

func (e *certError) Error() string {
	if e.kind == "local" {
		return fmt.Sprintf("node client certificate unusable: %v", e.err)
	}
	return fmt.Sprintf("panel rejected node certificate: %v", e.err)
}

func (e *certError) Unwrap() error { return e.err }

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

	// Region 是节点本地配置的 S3 签名区域,随每次 hello 上报给 Panel 供展示。
	// 它不参与任何控制面决策:Panel 不会据此下发配置,区域仍由节点 yaml 决定。
	Region string

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
	cfg       ClientConfig
	db        *gorm.DB
	executor  *Executor
	runner    TaskRunner
	telemetry *StorageTelemetryRecorder
	// certErrs 对证书类永久错误做「首次立即报、之后降频重复」的日志节流,
	// 连接成功时重置计数,恢复后再次失败仍会立即报(R4.3)。
	certErrs certErrorReporter

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

// SetTelemetryRecorder wires the recorder shared with the S3 handlers and task
// runner, including its in-memory fail-closed latch.
func (c *Client) SetTelemetryRecorder(recorder *StorageTelemetryRecorder) {
	c.telemetry = recorder
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

// certErrorReporter 节流证书类永久错误的 Error 日志:同一类错误首次立即
// 报,之后每 certErrLogEvery 次重复一次;不同类错误各自独立计数;Reset
// 在连接成功时清零,恢复后再次失败立即重新报。日志级别提升不改变控制流,
// node 继续退避重连并服务本地 DB(安全网 A,R4.4)。
type certErrorReporter struct {
	failures map[string]int
}

func (r *certErrorReporter) log(kind, action, detail string, err error) {
	if r.failures == nil {
		r.failures = make(map[string]int)
	}
	r.failures[kind]++
	n := r.failures[kind]
	if n == 1 || n%certErrLogEvery == 0 {
		slog.Error("node certificate problem prevents control-plane connection",
			"kind", kind, "failures", n, "action", action, "detail", detail, "error", err)
	}
}

func (r *certErrorReporter) Reset() {
	r.failures = nil
}

// checkLocalCert 在拨号前做一次本地证书检查(路径 A):证书缺失、损坏或已过
// NotAfter 时给出确定性的、不依赖错误字符串匹配的判定。复用兄弟任务
// register.go 的 LoadCertificate(R4.5,AC15),到期口径与签发/续期完全一致。
func (c *Client) checkLocalCert() *certError {
	cert, err := c.cfg.Identity.LoadCertificate()
	if err != nil {
		return &certError{kind: "local", err: err}
	}
	if !time.Now().Before(cert.NotAfter) {
		return &certError{kind: "local", err: fmt.Errorf(
			"client certificate expired at %s", cert.NotAfter.Format(time.RFC3339))}
	}
	return nil
}

// connectAndServe dials the panel, runs the handshake, and serves the connection
// until it closes or ctx is cancelled.
func (c *Client) connectAndServe(ctx context.Context) error {
	// 路径 A:拨号前主动检查本地证书,过期/损坏在 TLS 握手阶段就会失败,
	// 提前在这里给出确定的证书语义,而不是混进通用的 dial 错误。
	if certErr := c.checkLocalCert(); certErr != nil {
		c.certErrs.log("local",
			"管理员在 panel 上签发一次性注册令牌,填入 node.yaml 后重启节点(无宽限期)",
			certErr.Error(), certErr)
		return certErr
	}

	tlsConfig, err := c.clientTLS()
	if err != nil {
		certLoadErr := &certError{kind: "local", err: err}
		if strings.Contains(err.Error(), "load client cert") {
			// LoadX509KeyPair 失败 = 证书/私钥文件本身有问题(路径 A),
			// 不是网络瞬时错误。
			c.certErrs.log("local",
				"管理员在 panel 上签发一次性注册令牌,填入 node.yaml 后重启节点(无宽限期)",
				certLoadErr.Error(), certLoadErr)
			return certLoadErr
		}
		return fmt.Errorf("build client TLS: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()
	ws, resp, err := websocket.Dial(dialCtx, c.cfg.AgentURL, &websocket.DialOptions{
		HTTPClient: tlsHTTPClient(tlsConfig),
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			// 路径 B:panel 对「证书被吊销 / 节点停用或退役 / 指纹不在表内」
			// 一律返回 401。证书本身能完成 TLS 握手,问题在 panel 侧的
			// 吊销/停用/指纹表,与路径 A(本地证书不可用)的文案分开。
			rejected := &certError{kind: "rejected", err: fmt.Errorf(
				"panel returned HTTP %d (client certificate required)", resp.StatusCode)}
			c.certErrs.log("rejected",
				"请管理员在管理面检查该节点状态与证书吊销记录;若被吊销需重新注册",
				rejected.Error(), rejected)
			return rejected
		}
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
	// 连接成功:证书错误节流计数归零,恢复后再次失败仍会立即报 Error。
	c.certErrs.Reset()

	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()

	// Heartbeat goroutine runs until the serve loop exits.
	go c.heartbeatLoop(serveCtx, ws)

	return c.serveLoop(serveCtx, ws)
}

// renewCertificate sends a POST /renew request to the panel with a fresh CSR
// and persists the issued certificate to disk. It reuses the existing mTLS
// client configuration (clientTLS) and the node's private key.
func (c *Client) renewCertificate(ctx context.Context) error {
	renewURL, err := renewURLFromAgentURL(c.cfg.AgentURL)
	if err != nil {
		return fmt.Errorf("derive renew URL: %w", err)
	}
	key, err := c.cfg.Identity.ensureKey()
	if err != nil {
		return fmt.Errorf("load node key: %w", err)
	}
	csrPEM, err := buildCSR(key, c.cfg.NodeID)
	if err != nil {
		return fmt.Errorf("build CSR: %w", err)
	}
	tlsConfig, err := c.clientTLS()
	if err != nil {
		return fmt.Errorf("build client TLS: %w", err)
	}
	httpClient := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}
	body, err := json.Marshal(map[string]string{"csr_pem": string(csrPEM)})
	if err != nil {
		return fmt.Errorf("marshal renew request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, renewURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build renew request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("renew request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("renew rejected with status %d", resp.StatusCode)
	}
	var result registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode renew response: %w", err)
	}
	if result.CertPEM == "" {
		return fmt.Errorf("panel returned an empty certificate")
	}
	if err := persistPEM(c.cfg.Identity.CertFile, []byte(result.CertPEM), 0o644); err != nil {
		return fmt.Errorf("write renewed cert: %w", err)
	}
	return nil
}

// renewURLFromAgentURL derives the /renew HTTPS endpoint URL from the agent
// WebSocket URL. It replaces the scheme (wss→https, ws→http) and the last path
// segment (/agent → /renew), preserving host, port, and any path prefix.
func renewURLFromAgentURL(agentURL string) (string, error) {
	u, err := url.Parse(agentURL)
	if err != nil {
		return "", fmt.Errorf("parse agent URL: %w", err)
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	default:
		return "", fmt.Errorf("unexpected agent URL scheme %q", u.Scheme)
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) > 0 && parts[len(parts)-1] == "agent" {
		parts[len(parts)-1] = "renew"
	} else {
		parts = append(parts, "renew")
	}
	u.Path = strings.Join(parts, "/")
	return u.String(), nil
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
		Capabilities:    []string{controlproto.CapabilityAuthoritativeConfigV1},
		Region:          c.cfg.Region,
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

	appliedHash, err := c.executor.ApplyDesiredState(payload)
	if err != nil {
		slog.Error("apply desired_state failed", "version", payload.Version, "error", err)
		_ = c.sendMessage(ctx, ws, controlproto.TypeAck, env.ID, controlproto.AckPayload{
			Version: payload.Version,
			State:   controlproto.SyncStateFailed,
			Error:   safeApplyError(err),
		})
		return
	}

	slog.Info("applied desired_state", "version", payload.Version, "state", controlproto.SyncStateSynced)
	_ = c.sendMessage(ctx, ws, controlproto.TypeAck, env.ID, controlproto.AckPayload{
		Version:     payload.Version,
		State:       controlproto.SyncStateSynced,
		ContentHash: appliedHash,
	})
}

func safeApplyError(err error) string {
	message := err.Error()
	for _, safeFragment := range []string{
		"desired content hash mismatch",
		"validate desired state:",
		"retained data prevents declaring bucket",
	} {
		if strings.Contains(message, safeFragment) {
			return message
		}
	}
	return "desired state apply failed"
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

// heartbeatLoop sends periodic heartbeats until ctx is cancelled. It also
// checks whether the client certificate needs renewal on each tick, so a
// long-lived connection will still renew before expiry (R3.3).
func (c *Client) heartbeatLoop(ctx context.Context, ws *websocket.Conn) {
	ticker := time.NewTicker(c.cfg.HeartbeatInterval)
	defer ticker.Stop()
	renewed := false
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
			payload := HeartbeatTelemetrySnapshot(c.db)
			if c.telemetry != nil {
				payload = c.telemetry.HeartbeatTelemetrySnapshot()
			}
			payload.AppliedVersion = meta.AppliedVersion
			if err := c.sendMessage(ctx, ws, controlproto.TypeHeartbeat, "", payload); err != nil {
				slog.Debug("heartbeat send failed", "error", err)
				return
			}
			// Check for certificate renewal on each heartbeat tick. Once
			// renewal succeeds, don't retry (R3.5). The ws close will cause
			// the serve loop to exit and Run to reconnect with the new cert.
			if !renewed {
				cert, err := c.cfg.Identity.LoadCertificate()
				if err == nil && NeedsRenewal(cert, time.Now()) {
					slog.Info("client certificate approaching expiry, requesting renewal")
					if err := c.renewCertificate(ctx); err != nil {
						slog.Warn("certificate renewal failed; will retry on next heartbeat", "error", err)
					} else {
						renewed = true
						slog.Info("certificate renewed successfully, reconnecting with new certificate")
						_ = ws.Close(websocket.StatusNormalClosure, "renewed certificate, reconnecting")
						return
					}
				}
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
