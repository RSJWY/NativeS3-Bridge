package panel

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// DefaultMaxMessageBytes bounds a single control-plane frame so a misbehaving or
// compromised peer cannot force unbounded memory use. The control channel is not
// a bulk data path; desired-state and task-result payloads are small.
const DefaultMaxMessageBytes = 1 << 20 // 1 MiB

// DefaultMaxInFlightTasks bounds the number of dispatched-but-unacknowledged
// tasks on a single connection (backpressure). When reached the panel must defer
// dispatching new tasks rather than queueing without bound.
const DefaultMaxInFlightTasks = 16

// AgentConn wraps a single node's WebSocket connection. Writes are serialized
// through a ctx-aware semaphore because concurrent writes are unsafe; reads are
// driven by the single serve loop, so no read lock is needed. AgentConn carries
// the negotiated protocol version and the authenticated node identity resolved
// from the mTLS certificate.
type AgentConn struct {
	NodeID          uint
	ProtocolVersion int
	Fingerprint     string
	Capabilities    []string
	AppliedVersion  int64
	ContentHash     string
	NeedsSync       bool
	// HeartbeatInterval 是节点在 hello 中自报的心跳间隔(已按 R1.3 钳制/回落),
	// 用于计算离线阈值与读超时。零值表示未协商到有效值(同步升级后不应出现)。
	HeartbeatInterval time.Duration

	ws *websocket.Conn

	// importReassembler 重组本连接上 v2 import_report_chunk。
	importReassembler *importReassembler

	// writeSem 串行化写操作。用容量 1 的 channel 而不是 sync.Mutex:互斥锁的等待
	// 不可中断,一个"不读"的对端会让后续写方永久卡在 Lock() 上(连 ctx 超时也救不
	// 回来,因为超时只覆盖拿到锁之后的 Write)。channel 信号量让锁等待本身也能被
	// ctx 取消。
	writeSem chan struct{}

	// inFlight guards the set of dispatched-but-unacknowledged task IDs for
	// backpressure accounting.
	inFlightMu  sync.Mutex
	inFlight    map[string]struct{}
	maxInFlight int

	// lastSeen is the last time any frame was received from the node; the serve
	// loop updates it and the offline sweeper reads it.
	lastSeenMu sync.RWMutex
	lastSeen   time.Time

	// heartbeatMu 保护心跳落库节流与连续存储失败计数。两者都只由本连接的 serve
	// 循环读写,加锁是为了对遥测/诊断类的旁路读取保持 race-free。
	heartbeatMu sync.Mutex
	// lastPersistedBeat 是本连接上次把心跳落库的时间(零值表示还没落过)。
	lastPersistedBeat time.Time
	// storageFailures 是连续存储失败次数;任何一次成功落库都会清零。
	storageFailures int
}

// shouldPersistHeartbeat 判断本帧心跳是否应该落库。正常 cadence 下每帧都会返回
// true;狂发帧被压到每 minInterval 一次。它只做判断,不记录时间戳——落库成功后由
// markHeartbeatPersisted 记录。若在这里就推进时间戳,一次失败的落库会同时吃掉本次
// 写入和下一个 minInterval 内的重试机会,DB 短暂故障期间反而更难恢复。
func (c *AgentConn) shouldPersistHeartbeat(now time.Time, minInterval time.Duration) bool {
	c.heartbeatMu.Lock()
	defer c.heartbeatMu.Unlock()
	return c.lastPersistedBeat.IsZero() || now.Sub(c.lastPersistedBeat) >= minInterval
}

// markHeartbeatPersisted 记下这一帧确实落库成功的时间。
func (c *AgentConn) markHeartbeatPersisted(now time.Time) {
	c.heartbeatMu.Lock()
	c.lastPersistedBeat = now
	c.heartbeatMu.Unlock()
}

// noteStorageFailure 记一次存储失败并返回累计的连续失败次数。
func (c *AgentConn) noteStorageFailure() int {
	c.heartbeatMu.Lock()
	defer c.heartbeatMu.Unlock()
	c.storageFailures++
	return c.storageFailures
}

// noteStorageSuccess 清零连续失败计数(阈值只针对"连续"失败)。
func (c *AgentConn) noteStorageSuccess() {
	c.heartbeatMu.Lock()
	c.storageFailures = 0
	c.heartbeatMu.Unlock()
}

func (c *AgentConn) Supports(capability string) bool {
	return controlproto.HasCapability(c.Capabilities, capability)
}

// newAgentConn wraps an accepted websocket connection for a node.
func newAgentConn(nodeID uint, fingerprint string, ws *websocket.Conn) *AgentConn {
	return &AgentConn{
		NodeID:            nodeID,
		Fingerprint:       fingerprint,
		ws:                ws,
		writeSem:          make(chan struct{}, 1),
		inFlight:          make(map[string]struct{}),
		maxInFlight:       DefaultMaxInFlightTasks,
		lastSeen:          nowUTC(),
		HeartbeatInterval: DefaultHeartbeatInterval,
		importReassembler: newImportReassembler(),
	}
}

// heartbeatInterval 返回本连接实际使用的心跳间隔(已钳制/回落);零值时取 panel 默认。
func (c *AgentConn) heartbeatInterval() time.Duration {
	if c.HeartbeatInterval > 0 {
		return c.HeartbeatInterval
	}
	return DefaultHeartbeatInterval
}

// send marshals and writes an envelope. Writes are serialized; ctx bounds both
// the wait for the write slot and the write itself, so a stuck node cannot wedge
// the panel's goroutine forever.
func (c *AgentConn) send(ctx context.Context, env controlproto.Envelope) error {
	data, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode %s: %w", env.Type, err)
	}
	if err := c.acquireWrite(ctx); err != nil {
		return fmt.Errorf("write %s: %w", env.Type, err)
	}
	defer c.releaseWrite()
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("write %s: %w", env.Type, err)
	}
	return nil
}

// acquireWrite 取得写序列化槽位。ctx 取消/超时时立即返回,不再无限等待前一个写完成。
// writeSem 为 nil 时(测试里直写字面量构造的 AgentConn)退化为不加锁,与旧行为一致。
func (c *AgentConn) acquireWrite(ctx context.Context) error {
	if c.writeSem == nil {
		return nil
	}
	select {
	case c.writeSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *AgentConn) releaseWrite() {
	if c.writeSem == nil {
		return
	}
	<-c.writeSem
}

// sendMessage builds an envelope for msgType/payload and sends it.
func (c *AgentConn) sendMessage(ctx context.Context, msgType controlproto.MessageType, id string, payload any) error {
	env, err := controlproto.NewEnvelope(msgType, id, payload)
	if err != nil {
		return err
	}
	return c.send(ctx, env)
}

// readEnvelope reads one frame and decodes the envelope. It enforces the message
// size limit via the underscore-configured read limit on the underlying socket.
func (c *AgentConn) readEnvelope(ctx context.Context) (controlproto.Envelope, error) {
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return controlproto.Envelope{}, err
	}
	env, err := controlproto.DecodeEnvelope(data)
	if err != nil {
		return controlproto.Envelope{}, err
	}
	c.markSeen()
	return env, nil
}

func (c *AgentConn) markSeen() {
	c.lastSeenMu.Lock()
	c.lastSeen = nowUTC()
	c.lastSeenMu.Unlock()
}

// LastSeen returns the time of the last received frame.
func (c *AgentConn) LastSeen() time.Time {
	c.lastSeenMu.RLock()
	defer c.lastSeenMu.RUnlock()
	return c.lastSeen
}

// reserveTask records taskID as in-flight for backpressure. It returns false if
// the in-flight limit is already reached, in which case the caller must not
// dispatch the task.
func (c *AgentConn) reserveTask(taskID string) bool {
	c.inFlightMu.Lock()
	defer c.inFlightMu.Unlock()
	if len(c.inFlight) >= c.maxInFlight {
		return false
	}
	c.inFlight[taskID] = struct{}{}
	return true
}

// releaseTask clears taskID from the in-flight set (on result or failure).
func (c *AgentConn) releaseTask(taskID string) {
	c.inFlightMu.Lock()
	delete(c.inFlight, taskID)
	c.inFlightMu.Unlock()
}

// inFlightCount returns the current number of unacknowledged tasks.
func (c *AgentConn) inFlightCount() int {
	c.inFlightMu.Lock()
	defer c.inFlightMu.Unlock()
	return len(c.inFlight)
}

// inFlightTasks returns a snapshot of the currently-unacknowledged task IDs.
// Used when a connection drops to mark those tasks as interrupted (the panel
// cannot know whether the node executed them).
func (c *AgentConn) inFlightTasks() []string {
	c.inFlightMu.Lock()
	defer c.inFlightMu.Unlock()
	ids := make([]string, 0, len(c.inFlight))
	for id := range c.inFlight {
		ids = append(ids, id)
	}
	return ids
}

// close terminates the websocket with a normal-closure status.
func (c *AgentConn) close(reason string) {
	_ = c.ws.Close(websocket.StatusNormalClosure, reason)
}

// closeError terminates the websocket with a protocol-error status.
func (c *AgentConn) closeError(reason string) {
	_ = c.ws.Close(websocket.StatusProtocolError, reason)
}

// --- v2 import_report_chunk 重组器 ---

const (
	// importReassemblyMaxChunks 单 request 最多接受的 chunk 数,防恶意节点用大量
	// 不收尾分块占内存。
	importReassemblyMaxChunks = 32
	// importReassemblyMaxBytes 单 request 重组后总字节上限。
	importReassemblyMaxBytes = 16 << 20
	// importReassemblyTimeout 重组最长等待时间,超时丢弃。
	importReassemblyTimeout = 5 * time.Minute
)

// importReassembler 按 request_id 重组 v2 import_report_chunk。
type importReassembler struct {
	mu     sync.Mutex
	states map[string]*importReassemblyState
}

type importReassemblyState struct {
	requestID   string
	total       int
	received    map[int]controlproto.ImportReportChunkPayload
	bytes       int
	deadline    time.Time
	delivered   bool
}

func newImportReassembler() *importReassembler {
	return &importReassembler{states: make(map[string]*importReassemblyState)}
}

// ingest 喂入一块,返回 (收齐?, 完整 report, error)。
// 错误表示应断连(超限)。
func (r *importReassembler) ingest(chunk controlproto.ImportReportChunkPayload, now time.Time) (bool, controlproto.ImportReportPayload, error) {
	if chunk.RequestID == "" || chunk.Total <= 0 || chunk.Seq < 0 || chunk.Seq >= chunk.Total {
		return false, controlproto.ImportReportPayload{}, fmt.Errorf("invalid import report chunk: request_id=%q seq=%d total=%d", chunk.RequestID, chunk.Seq, chunk.Total)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.states[chunk.RequestID]
	if !ok {
		state = &importReassemblyState{
			requestID: chunk.RequestID,
			total:     chunk.Total,
			received:  make(map[int]controlproto.ImportReportChunkPayload),
			deadline:  now.Add(importReassemblyTimeout),
		}
		r.states[chunk.RequestID] = state
	}

	if state.delivered {
		return true, controlproto.ImportReportPayload{}, nil
	}
	if chunk.Total != state.total {
		return false, controlproto.ImportReportPayload{}, fmt.Errorf("import chunk total mismatch: expected %d, got %d", state.total, chunk.Total)
	}
	if now.After(state.deadline) {
		delete(r.states, chunk.RequestID)
		return false, controlproto.ImportReportPayload{}, fmt.Errorf("import reassembly timed out for request %s", chunk.RequestID)
	}
	if len(state.received) >= importReassemblyMaxChunks || len(state.received)+1 > importReassemblyMaxChunks {
		delete(r.states, chunk.RequestID)
		return false, controlproto.ImportReportPayload{}, fmt.Errorf("import reassembly chunk limit exceeded for request %s", chunk.RequestID)
	}

	chunkBytes := estimateChunkBytes(chunk)
	if state.bytes+chunkBytes > importReassemblyMaxBytes {
		delete(r.states, chunk.RequestID)
		return false, controlproto.ImportReportPayload{}, fmt.Errorf("import reassembly byte limit exceeded for request %s", chunk.RequestID)
	}

	if _, exists := state.received[chunk.Seq]; exists {
		// 重复块忽略,不算错误。
		return false, controlproto.ImportReportPayload{}, nil
	}
	state.received[chunk.Seq] = chunk
	state.bytes += chunkBytes

	if len(state.received) < state.total {
		return false, controlproto.ImportReportPayload{}, nil
	}

	// 收齐,组装成完整 ImportReportPayload。
	report := controlproto.ImportReportPayload{}
	for i := 0; i < state.total; i++ {
		c := state.received[i]
		report.State.Credentials = append(report.State.Credentials, c.Credentials...)
		report.State.Buckets = append(report.State.Buckets, c.Buckets...)
		report.State.Webhooks = append(report.State.Webhooks, c.Webhooks...)
	}
	report.CredentialCount = len(report.State.Credentials)
	report.BucketCount = len(report.State.Buckets)
	report.WebhookCount = len(report.State.Webhooks)
	report.LocalContentHash = report.State.ContentHash()
	state.delivered = true
	delete(r.states, chunk.RequestID)
	return true, report, nil
}

// estimateChunkBytes 估算 chunk 的内存占用(近似 JSON 字节数)。
func estimateChunkBytes(chunk controlproto.ImportReportChunkPayload) int {
	n := len(chunk.RequestID) + 32 // 元数据开销近似
	for _, c := range chunk.Credentials {
		n += len(c.AccessKey) + len(c.SecretKey) + len(c.Name) + len(c.Bucket) + len(c.Status) + 64
	}
	for _, b := range chunk.Buckets {
		n += len(b.Name) + len(b.ACL) + 32
	}
	for _, h := range chunk.Webhooks {
		n += len(h.URL) + len(h.Events) + 32
	}
	return n
}
