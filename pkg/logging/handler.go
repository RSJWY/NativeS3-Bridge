package logging

import (
	"context"
	"log/slog"
	"strings"
)

type RingHandler struct {
	next   slog.Handler
	ring   *Ring
	attrs  []slog.Attr
	groups []string
}

func NewRingHandler(next slog.Handler, ring *Ring) *RingHandler {
	return &RingHandler{next: next, ring: ring}
}

func (h *RingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *RingHandler) Handle(ctx context.Context, record slog.Record) error {
	attrs := make(map[string]any)
	for _, attr := range h.attrs {
		appendAttr(attrs, h.groups, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		appendAttr(attrs, h.groups, attr)
		return true
	})
	h.ring.Append(Entry{Time: record.Time.UTC(), Level: record.Level.String(), Message: record.Message, Attrs: attrs})
	return h.next.Handle(ctx, record)
}

func (h *RingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.next = h.next.WithAttrs(attrs)
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *RingHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.next = h.next.WithGroup(name)
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

func appendAttr(target map[string]any, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) || sensitiveKey(attr.Key) {
		return
	}
	if attr.Value.Kind() == slog.KindGroup {
		for _, child := range attr.Value.Group() {
			appendAttr(target, append(groups, attr.Key), child)
		}
		return
	}
	key := strings.Join(append(groups, attr.Key), ".")
	target[key] = attr.Value.Any()
}

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), ".", "_"))
	for _, part := range []string{"secret", "password", "authorization", "cookie", "signature", "token"} {
		if strings.Contains(normalized, part) {
			return true
		}
	}
	return false
}
