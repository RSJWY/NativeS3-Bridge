package panel

import (
	"sync"
	"time"
)

// Hub tracks live agent connections keyed by node ID. It is the panel's
// in-memory registry of which nodes are currently connected on the control
// plane. A node has at most one live connection: a newer connection for the
// same node evicts the older one (the node reconnected, e.g. after a network
// blip) so the panel never fans a message out to a stale socket.
type Hub struct {
	mu    sync.RWMutex
	conns map[uint]*AgentConn
}

// NewHub creates an empty connection registry.
func NewHub() *Hub {
	return &Hub{conns: make(map[uint]*AgentConn)}
}

// Register adds conn for nodeID and returns any previous connection that was
// displaced (nil if none). The caller is responsible for closing a displaced
// connection; Register itself never blocks on I/O.
func (h *Hub) Register(nodeID uint, conn *AgentConn) (previous *AgentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	previous = h.conns[nodeID]
	h.conns[nodeID] = conn
	return previous
}

// Unregister removes conn for nodeID only if the currently-registered
// connection is still conn. This prevents a slow-closing old connection from
// evicting the newer one that already replaced it.
func (h *Hub) Unregister(nodeID uint, conn *AgentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if current, ok := h.conns[nodeID]; ok && current == conn {
		delete(h.conns, nodeID)
	}
}

// Get returns the live connection for nodeID, or (nil, false) if the node is
// not currently connected.
func (h *Hub) Get(nodeID uint) (*AgentConn, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	conn, ok := h.conns[nodeID]
	return conn, ok
}

// OnlineNodes returns the set of node IDs with a live connection.
func (h *Hub) OnlineNodes() []uint {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]uint, 0, len(h.conns))
	for id := range h.conns {
		ids = append(ids, id)
	}
	return ids
}

// IsOnline reports whether nodeID currently has a live connection.
func (h *Hub) IsOnline(nodeID uint) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[nodeID]
	return ok
}

// Count returns the number of live connections.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// nowUTC is a tiny indirection so tests can reason about timestamps without a
// clock injection framework. Kept unexported and trivial.
func nowUTC() time.Time { return time.Now().UTC() }
