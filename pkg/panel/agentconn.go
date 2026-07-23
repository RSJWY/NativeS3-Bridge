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
// through a mutex because gorilla-style concurrent writes are unsafe; reads are
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

	writeMu sync.Mutex

	// inFlight guards the set of dispatched-but-unacknowledged task IDs for
	// backpressure accounting.
	inFlightMu  sync.Mutex
	inFlight    map[string]struct{}
	maxInFlight int

	// lastSeen is the last time any frame was received from the node; the serve
	// loop updates it and the offline sweeper reads it.
	lastSeenMu sync.RWMutex
	lastSeen   time.Time
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
		inFlight:    make(map[string]struct{}),
		maxInFlight: DefaultMaxInFlightTasks,
		lastSeen:    nowUTC(),
	}
}

// send marshals and writes an envelope. Writes are serialized; the context
// bounds how long a single write may block so a stuck node cannot wedge the
// panel's goroutine forever.
func (c *AgentConn) send(ctx context.Context, env controlproto.Envelope) error {
	data, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode %s: %w", env.Type, err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("write %s: %w", env.Type, err)
	}
	return nil
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
