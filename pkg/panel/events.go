package panel

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// AdminEvent is a notification-only payload pushed to admin browsers over SSE.
// It carries identities (node/task) and state names, never full business data:
// on receipt the frontend re-pulls the relevant REST endpoint, which stays the
// single source of truth (design decision: no duplicate data plane over SSE).
type AdminEvent struct {
	Type   string `json:"type"`             // node_online | node_offline | task_updated
	NodeID uint   `json:"node_id"`          // owning node of the event
	TaskID string `json:"task_id,omitempty"`
	State  string `json:"state,omitempty"`  // task_updated only: pending/running/success/failed/unknown
}

const (
	AdminEventNodeOnline  = "node_online"
	AdminEventNodeOffline = "node_offline"
	AdminEventTaskUpdated = "task_updated"

	// sseSubscriberBuffer 是单个 SSE 订阅者的事件缓冲。正常场景下事件频率极低
	// (节点上下线、管理员触发的任务),远撑不满。
	sseSubscriberBuffer = 32
	// sseRetryHint 提示浏览器 EventSource 重连前等待的毫秒数,避免对端异常时
	// 紧密循环重连。
	sseRetryHint = "retry: 5000\n\n"
)

// sseHeartbeatInterval 是服务端判活心跳的周期。前端以显著更大的阈值(3 倍)判定
// SSE 不可用并降级轮询,所以心跳周期不能调大到与降级阈值同量级。var 而非 const:
// 测试把它调小以验证心跳帧。
var sseHeartbeatInterval = 15 * time.Second

// AdminEventBus fans admin events out to in-process SSE subscribers. Publishing
// never blocks the control-plane or task paths: each subscriber has a bounded
// buffer, and a subscriber that cannot keep up is evicted (its stream ends, the
// browser's EventSource reconnects and re-pulls REST state) rather than being
// backpressured. The bus is process-local: the panel is deployed as a single
// instance (docker-compose.panel.yml), so no cross-instance fanout exists.
type AdminEventBus struct {
	mu     sync.Mutex
	nextID uint64
	subs   map[uint64]*adminEventSubscriber
}

type adminEventSubscriber struct {
	events chan AdminEvent
	done   chan struct{} // closed on removal; wakes the stream handler
}

// NewAdminEventBus creates an empty bus.
func NewAdminEventBus() *AdminEventBus {
	return &AdminEventBus{subs: make(map[uint64]*adminEventSubscriber)}
}

// Publish delivers ev to every current subscriber. Slow subscribers are evicted
// instead of blocking the publisher: dropping their backlog silently would leave
// a stale page that never learns what it missed, while eviction ends the stream
// and the browser recovers via reconnect + full REST re-pull.
func (b *AdminEventBus) Publish(ev AdminEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, sub := range b.subs {
		select {
		case sub.events <- ev:
		default:
			slog.Debug("admin event subscriber evicted (slow consumer)", "event", ev.Type, "node", ev.NodeID)
			b.removeLocked(id, sub)
		}
	}
}

// subscribe registers a new subscriber and returns it.
func (b *AdminEventBus) subscribe() *adminEventSubscriber {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	sub := &adminEventSubscriber{
		events: make(chan AdminEvent, sseSubscriberBuffer),
		done:   make(chan struct{}),
	}
	b.subs[b.nextID] = sub
	return sub
}

// unsubscribe removes a subscriber. It is idempotent and also called by Publish
// when evicting a slow consumer, so the stream handler's deferred cleanup must
// not panic or double-close on an already-evicted subscription.
func (b *AdminEventBus) unsubscribe(sub *adminEventSubscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, s := range b.subs {
		if s == sub {
			b.removeLocked(id, sub)
			return
		}
	}
}

func (b *AdminEventBus) removeLocked(id uint64, sub *adminEventSubscriber) {
	delete(b.subs, id)
	// closing done wakes the stream handler; the events channel is garbage after
	// that and never written again (Publish only touches live map entries).
	close(sub.done)
}

// SetAdminEventBus wires the SSE event bus into the admin API. Optional: nil
// leaves the endpoint answering 503 (used by tests that don't exercise events).
func (a *AdminAPI) SetAdminEventBus(bus *AdminEventBus) {
	a.events = bus
}

// AdminEvents serves GET /api/admin/events as a Server-Sent Events stream of
// AdminEvents. It is mounted behind the same webadmin session middleware as the
// REST surface: an unauthenticated request is rejected before reaching here and
// never establishes a stream.
//
// 流式响应依赖 admin http.Server 不设 WriteTimeout(一旦设了,长连接会被写截止
// 时间掐断);ReadTimeout 只约束请求读取,EventSource 的 GET 没有请求体,不影响
// 响应流。adminserver.go 的超时注释与此约定配套维护。
func (a *AdminAPI) AdminEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.events == nil {
		writeTransportError(w, http.StatusServiceUnavailable, "event stream unavailable")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeTransportError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	sub := a.events.subscribe()
	defer a.events.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	// 事件流绝不能被任何一层缓存。
	w.Header().Set("Cache-Control", "no-store")
	// nginx 类反代默认缓冲响应体,X-Accel-Buffering: no 在应用层请求其直通;
	// docs/public-deployment.md 的代理配置(proxy_buffering off)与此互为兜底。
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// retry 提示浏览器断线后的重连间隔;首帧同时起到「订阅已生效」的边界作用,
	// 测试据此判断可以安全发布事件。
	if _, err := fmt.Fprint(w, sseRetryHint); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	// 心跳必须是具名事件而非 SSE 注释行:注释帧不会触发浏览器 EventSource 的任何
	// 回调,前端判活(超时降级轮询)感知不到它。
	seq := uint64(0)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev := <-sub.events:
			data, err := json.Marshal(ev)
			if err != nil {
				slog.Warn("admin event marshal failed", "event", ev.Type, "error", err)
				continue
			}
			seq++
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", seq, ev.Type, data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
