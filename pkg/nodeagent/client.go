package nodeagent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// errRenewedReconnect 标记续期成功后的计划内断连,Run 识别后应清零退避并
// 立即用新证书重连(R4)。
var errRenewedReconnect = errors.New("certificate renewed, reconnecting with new certificate")

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
	DefaultHandshakeTimeout  = 30 * time.Second
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
	HandshakeTimeout  time.Duration // 握手独立超时,默认 30s
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
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = DefaultHandshakeTimeout
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

	// negotiatedVersion 是本连接握手协商出的协议版本,用于 R6 单帧 version 校验。
	negotiatedVersion int
	// lastRecvAt 是最后一次成功收到帧的 UnixNano,serve 看门狗用它检测静默连接。
	lastRecvAt atomic.Int64
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
		if errors.Is(err, errRenewedReconnect) {
			// R4:续期成功后的计划内断连,立即用新证书重连,不清退避就是最小值。
			slog.Info("certificate renewed, reconnecting with new certificate immediately")
			backoff = c.cfg.MinBackoff
			continue
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

	hsCtx, hsCancel := context.WithTimeout(ctx, c.cfg.HandshakeTimeout)
	if err := c.handshake(hsCtx, ws); err != nil {
		hsCancel()
		return fmt.Errorf("handshake: %w", err)
	}
	hsCancel()
	// 连接成功:证书错误节流计数归零,恢复后再次失败仍会立即报 Error。
	c.certErrs.Reset()

	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()

	// Heartbeat goroutine runs until the serve loop exits.
	renewedCh := make(chan struct{}, 1)
	go c.heartbeatLoop(serveCtx, ws, func() {
		select {
		case renewedCh <- struct{}{}:
		default:
		}
	})

	err = c.serveLoop(serveCtx, ws)
	select {
	case <-renewedCh:
		// R4:续期成功后的计划内断连,返回哨兵错误让 Run 清零退避立即重连。
		return errRenewedReconnect
	default:
		return err
	}
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

	// R1:落盘前校验续期证书。续期场景 CA 池用本地 CAFile。
	caPEM, err := os.ReadFile(c.cfg.Identity.CAFile)
	if err != nil {
		return fmt.Errorf("read local CA for validation: %w", err)
	}
	key, err = c.cfg.Identity.ensureKey()
	if err != nil {
		return fmt.Errorf("load node key for validation: %w", err)
	}
	cert, err := validateIssuedCert(result.CertPEM, key, caPEM, time.Now())
	if err != nil {
		return fmt.Errorf("validate renewed certificate: %w", err)
	}
	// R1.2:续期场景同样做 not_after 交叉核对。
	if result.NotAfter != "" {
		notAfter, parseErr := time.Parse(time.RFC3339, result.NotAfter)
		if parseErr != nil {
			slog.Warn("panel returned unparsable not_after, using certificate value", "not_after", result.NotAfter, "error", parseErr)
		} else if !notAfter.Equal(cert.NotAfter) {
			slog.Warn("panel not_after disagrees with renewed certificate, trusting certificate value",
				"panel_not_after", notAfter, "cert_not_after", cert.NotAfter)
		}
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
		ProtocolVersion:     controlproto.ProtocolVersion,
		NodeID:              fmt.Sprintf("%d", c.cfg.NodeID),
		AgentVersion:        AgentVersion,
		AppliedVersion:      meta.AppliedVersion,
		ContentHash:         localHash,
		Capabilities:        []string{controlproto.CapabilityAuthoritativeConfigV1},
		Region:              c.cfg.Region,
		HeartbeatIntervalMS: int(c.cfg.HeartbeatInterval.Milliseconds()),
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
		if perr.Code == controlproto.ErrCodeVersionIncompatible {
			slog.Warn("protocol version mismatch with panel, synchronous upgrade required",
				"local_version", controlproto.ProtocolVersion,
				"local_min_version", controlproto.MinCompatibleVersion,
				"panel_message", perr.Message)
		}
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
	c.negotiatedVersion = ack.ProtocolVersion
	c.lastRecvAt.Store(time.Now().UnixNano())
	// If the panel says we need to sync, it will push desired_state right after;
	// no action needed here beyond logging.
	if ack.NeedsSync {
		slog.Info("panel requests desired-state sync", "target_version", ack.DesiredVersion)
	}
	return nil
}

// serveLoop reads and dispatches control-plane frames until the connection ends.
func (c *Client) serveLoop(ctx context.Context, ws *websocket.Conn) error {
	// R3:serve 看门狗,检测 panel 静默/半开连接。
	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()
	go c.watchdog(serveCtx, serveCancel, ws)

	for {
		env, err := c.readEnvelope(serveCtx, ws)
		if err != nil {
			return err
		}

		// R6:单帧 version 防御性校验。高于协商版本的帧丢弃不断连;缺失 version
		// 的帧按 v1 容忍(同版本部署不会触发,只是防御旧帧)。
		if env.Version > 0 && env.Version > c.negotiatedVersion {
			slog.Warn("dropping frame with version higher than negotiated", "type", env.Type, "version", env.Version, "negotiated", c.negotiatedVersion)
			continue
		}

		switch env.Type {
		case controlproto.TypeDesiredState:
			c.handleDesiredState(serveCtx, ws, env)
		case controlproto.TypeTask:
			c.handleTask(serveCtx, ws, env)
		case controlproto.TypeImportRequest:
			c.handleImportRequest(serveCtx, ws, env)
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

// watchdog 每隔 heartbeat_interval 检查一次最近一次收帧时间。
// 若超过 max(3×heartbeat_interval, 60s) 未收到任何帧,则 cancel serveCtx 触发重连。
func (c *Client) watchdog(ctx context.Context, cancel context.CancelFunc, ws *websocket.Conn) {
	interval := c.cfg.HeartbeatInterval
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	timeout := 3 * interval
	if timeout < 60*time.Second {
		timeout = 60 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.Unix(0, c.lastRecvAt.Load())
			if time.Since(last) > timeout {
				slog.Warn("control-plane watchdog timeout, reconnecting",
					"last_recv", last, "timeout", timeout)
				if ws != nil {
					_ = ws.Close(websocket.StatusPolicyViolation, "watchdog timeout")
				}
				cancel()
				return
			}
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

// importReportChunkBytes 是单个 import_report_chunk 序列化后的上限。
const importReportChunkBytes = 512 << 10

// handleImportRequest replies with a read-only snapshot of the node's current
// local business config (credentials with plaintext secrets, buckets, webhooks)
// so the panel can present an import summary for admin confirmation. It is
// strictly read-only: the node's config is never modified by an import request
// (the migration red line — the panel must not write business config to the
// node before the admin confirms, design §8.3).
//
// v2 起改为分页传输,避免单帧超过 panel 1 MiB 读上限导致导入死循环。
func (c *Client) handleImportRequest(ctx context.Context, ws *websocket.Conn, env controlproto.Envelope) {
	state, err := c.executor.LocalState()
	if err != nil {
		slog.Error("build local state for import failed", "error", err)
		_ = c.sendMessage(ctx, ws, controlproto.TypeError, env.ID, controlproto.ErrorPayload{
			Code: controlproto.ErrCodeInternal, Message: "build local state: " + err.Error(),
		})
		return
	}

	chunks, err := buildImportReportChunks(env.ID, state)
	if err != nil {
		slog.Error("build import report chunks failed", "error", err)
		_ = c.sendMessage(ctx, ws, controlproto.TypeError, env.ID, controlproto.ErrorPayload{
			Code: controlproto.ErrCodeInternal, Message: "build import report chunks: " + err.Error(),
		})
		return
	}

	for _, chunk := range chunks {
		if err := c.sendMessage(ctx, ws, controlproto.TypeImportReportChunk, env.ID, chunk); err != nil {
			slog.Error("send import report chunk failed", "seq", chunk.Seq, "error", err)
			return
		}
	}
}

// buildImportReportChunks 把节点本地状态拆成若干分页块。每块只带一类资源的一段,
// 且序列化后不超过 importReportChunkBytes。拆分的最细粒度是单条资源,因此单条资源
// 本身超限时整组 import 会失败(意味着该节点当前状态不适合通过控制面迁移)。
func buildImportReportChunks(requestID string, state controlproto.DesiredState) ([]controlproto.ImportReportChunkPayload, error) {
	var chunks []controlproto.ImportReportChunkPayload
	addChunk := func(creds []controlproto.DesiredCredential, buckets []controlproto.DesiredBucket, webhooks []controlproto.DesiredWebhook) {
		chunks = append(chunks, controlproto.ImportReportChunkPayload{
			RequestID: requestID, Credentials: creds, Buckets: buckets, Webhooks: webhooks,
		})
	}

	// credentials 每条一块:包含明文 secret,不与其他条目混装,避免一块里多个 secret。
	for _, cred := range state.Credentials {
		addChunk([]controlproto.DesiredCredential{cred}, nil, nil)
	}
	// buckets / webhooks 各自按条目成块
	for _, b := range state.Buckets {
		addChunk(nil, []controlproto.DesiredBucket{b}, nil)
	}
	for _, h := range state.Webhooks {
		addChunk(nil, nil, []controlproto.DesiredWebhook{h})
	}

	// 设置 Seq/Total;同时校验单块大小。
	total := len(chunks)
	for i := range chunks {
		chunks[i].Seq = i
		chunks[i].Total = total
		data, err := json.Marshal(chunks[i])
		if err != nil {
			return nil, fmt.Errorf("marshal chunk %d: %w", i, err)
		}
		if len(data) > importReportChunkBytes {
			return nil, fmt.Errorf("chunk %d exceeds %d bytes (got %d); reduce import size", i, importReportChunkBytes, len(data))
		}
	}
	return chunks, nil
}

// maxTaskTimeout 是 node 侧对 panel 下发 timeout_ms 的硬编码上界。
// 防止恶意/故障 panel 让任务无限占住 serve 循环。
const maxTaskTimeout = 10 * time.Minute

// defaultTaskTimeout 是 timeout_ms 缺失/非法时的安全默认值,与 panel 默认一致。
const defaultTaskTimeout = 60 * time.Second

// taskTimeout 计算任务实际执行时长。<=0 使用默认值;超出上界按上界执行并 Warn。
func taskTimeout(timeoutMS int64) time.Duration {
	if timeoutMS <= 0 {
		slog.Warn("panel sent invalid task timeout_ms, using default", "timeout_ms", timeoutMS, "default", defaultTaskTimeout)
		return defaultTaskTimeout
	}
	t := time.Duration(timeoutMS) * time.Millisecond
	if t > maxTaskTimeout {
		slog.Warn("panel sent task timeout_ms above node limit, clamping", "timeout_ms", timeoutMS, "limit", maxTaskTimeout)
		return maxTaskTimeout
	}
	return t
}

// isKnownTaskType 判断 task type 是否在预定义枚举内。
func isKnownTaskType(t controlproto.TaskType) bool {
	switch t {
	case controlproto.TaskLogQuery, controlproto.TaskStorageScan, controlproto.TaskStorageReconcileApply:
		return true
	}
	return false
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

	// R5:task_id 为空或 type 不在已知枚举内 → 不执行、不写台账,回 failed 结果。
	if strings.TrimSpace(task.TaskID) == "" {
		slog.Warn("panel sent task with empty task_id, ignoring")
		return
	}
	if !isKnownTaskType(task.Type) {
		slog.Warn("panel sent task with unknown type", "task_id", task.TaskID, "type", task.Type)
		_ = c.sendMessage(ctx, ws, controlproto.TypeTaskResult, env.ID, controlproto.TaskResultPayload{
			TaskID: task.TaskID,
			Type:   task.Type,
			State:  controlproto.TaskStateFailed,
			Error:  fmt.Sprintf("unsupported task type %q", task.Type),
		})
		return
	}

	// Idempotency: if we already have a result for this task ID, resend it.
	// 命中时校验缓存条目的 type 与本次一致,不一致视为新任务执行。
	if cached, ok := c.cachedTaskResult(task.TaskID); ok {
		if cached.Type != task.Type {
			slog.Warn("task id collision with different type, executing as new task", "task_id", task.TaskID, "cached_type", cached.Type, "new_type", task.Type)
		} else {
			_ = c.sendMessage(ctx, ws, controlproto.TypeTaskResult, env.ID, cached)
			return
		}
	}

	// R3:node 真正执行 panel 下发的 timeout_ms。
	timeout := taskTimeout(task.TimeoutMS)
	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var result controlproto.TaskResultPayload
	if c.runner == nil {
		result = controlproto.TaskResultPayload{
			TaskID: task.TaskID,
			Type:   task.Type,
			State:  controlproto.TaskStateFailed,
			Error:  "node does not support tasks",
		}
	} else {
		result = c.runner.Run(taskCtx, task)
	}
	result.TaskID = task.TaskID
	result.Type = task.Type

	// 超时失败不落幂等台账(它不是成功执行),panel 会按 task_timeout 判终态。
	if result.State == controlproto.TaskStateSuccess {
		if err := c.recordTaskResult(task, result); err != nil {
			slog.Error("record task result failed", "task", task.TaskID, "error", err)
		}
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
// onRenewed 在成功续期并主动断连时被调用一次(允许 nil)。
func (c *Client) heartbeatLoop(ctx context.Context, ws *websocket.Conn, onRenewed func()) {
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
				// R3.3:心跳发送失败必须关闭连接并 cancel serveCtx,让 Run 立即重连。
				slog.Warn("heartbeat send failed, closing connection", "error", err)
				_ = ws.Close(websocket.StatusInternalError, "heartbeat send failed")
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
						if onRenewed != nil {
							onRenewed()
						}
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
	c.lastRecvAt.Store(time.Now().UnixNano())
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
