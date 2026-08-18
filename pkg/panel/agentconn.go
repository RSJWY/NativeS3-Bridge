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

	ws *websocket.Conn

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

// shouldPersistHeartbeat 判断本帧心跳是否应该落库,并在返回 true 时记下落库时间。
// 正常 cadence 下每帧都会返回 true;狂发帧被压到每 minInterval 一次。
func (c *AgentConn) shouldPersistHeartbeat(now time.Time, minInterval time.Duration) bool {
	c.heartbeatMu.Lock()
	defer c.heartbeatMu.Unlock()
	if !c.lastPersistedBeat.IsZero() && now.Sub(c.lastPersistedBeat) < minInterval {
		return false
	}
	c.lastPersistedBeat = now
	return true
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
		NodeID:      nodeID,
		Fingerprint: fingerprint,
		ws:          ws,
		writeSem:    make(chan struct{}, 1),
		inFlight:    make(map[string]struct{}),
		maxInFlight: DefaultMaxInFlightTasks,
		lastSeen:    nowUTC(),
	}
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
