package hooks

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
)

const (
	DefaultQueueSize = 1024
	DefaultWorkers   = 4
	DefaultMaxRetry  = 3
)

type Config struct {
	QueueSize int
	Workers   int
	MaxRetry  int
	Timeout   time.Duration
}

type Manager struct {
	db             *gorm.DB
	queue          chan Event
	workers        int
	maxRetry       int
	timeout        time.Duration
	retryBaseDelay time.Duration

	mu     sync.RWMutex
	hooks  []Hook
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewManager(gdb *gorm.DB, cfg Config) *Manager {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.MaxRetry <= 0 {
		cfg.MaxRetry = DefaultMaxRetry
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultWebhookTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{db: gdb, queue: make(chan Event, cfg.QueueSize), workers: cfg.Workers, maxRetry: cfg.MaxRetry, timeout: cfg.Timeout, retryBaseDelay: time.Second, ctx: ctx, cancel: cancel}
}

func (m *Manager) Start() {
	if err := m.Reload(); err != nil {
		slog.Warn("load webhook hooks", "error", err)
	}
	for i := 0; i < m.workers; i++ {
		m.wg.Add(1)
		go m.worker()
	}
}

func (m *Manager) Emit(e Event) {
	if e.Timestamp == "" {
		e.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	select {
	case m.queue <- e:
	default:
		slog.Warn("drop hook event because queue is full", "type", e.Type, "bucket", e.Bucket, "key", e.Key)
	}
}

func (m *Manager) Reload() error {
	var configs []db.HookConfig
	if err := m.db.Where("enabled = ?", true).Find(&configs).Error; err != nil {
		return err
	}
	hooks := make([]Hook, 0, len(configs))
	for _, cfg := range configs {
		events := parseEvents(cfg.Events)
		if cfg.URL == "" || len(events) == 0 {
			continue
		}
		hooks = append(hooks, NewWebhookHook(cfg.URL, events, m.timeout))
	}
	m.mu.Lock()
	m.hooks = hooks
	m.mu.Unlock()
	return nil
}

func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case event := <-m.queue:
			m.deliver(event)
		}
	}
}

func (m *Manager) deliver(e Event) {
	m.mu.RLock()
	hooks := append([]Hook(nil), m.hooks...)
	m.mu.RUnlock()
	for _, hook := range hooks {
		if !hook.Match(e.Type) {
			continue
		}
		if err := m.deliverWithRetry(hook, e); err != nil {
			slog.Warn("deliver hook event failed", "type", e.Type, "bucket", e.Bucket, "key", e.Key, "error", err)
		}
	}
}

func (m *Manager) deliverWithRetry(hook Hook, e Event) error {
	var err error
	for attempt := 0; attempt <= m.maxRetry; attempt++ {
		err = hook.Deliver(e)
		if err == nil {
			return nil
		}
		if attempt == m.maxRetry {
			break
		}
		delay := m.retryBaseDelay << attempt
		select {
		case <-m.ctx.Done():
			return err
		case <-time.After(delay):
		}
	}
	return err
}

func parseEvents(raw string) []EventType {
	parts := strings.Split(raw, ",")
	events := make([]EventType, 0, len(parts))
	for _, part := range parts {
		switch event := EventType(strings.TrimSpace(part)); event {
		case ObjectCreated, ObjectDeleted:
			events = append(events, event)
		}
	}
	return events
}
