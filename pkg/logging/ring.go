package logging

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

const DefaultRingCapacity = 2000

type Entry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"msg"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

type Ring struct {
	mu       sync.RWMutex
	entries  []Entry
	next     int
	capacity int
	full     bool
}

func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = DefaultRingCapacity
	}
	return &Ring{entries: make([]Entry, capacity), capacity: capacity}
}

func (r *Ring) Append(entry Entry) {
	r.mu.Lock()
	r.entries[r.next] = cloneEntry(entry)
	r.next = (r.next + 1) % r.capacity
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

func (r *Ring) Snapshot(limit int, level, query string) []Entry {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	level = strings.ToUpper(strings.TrimSpace(level))
	query = strings.ToLower(strings.TrimSpace(query))

	r.mu.RLock()
	count := r.next
	if r.full {
		count = r.capacity
	}
	result := make([]Entry, 0, min(limit, count))
	for offset := 0; offset < count && len(result) < limit; offset++ {
		index := (r.next - 1 - offset + r.capacity) % r.capacity
		entry := r.entries[index]
		if level != "" && strings.ToUpper(entry.Level) != level {
			continue
		}
		if query != "" && !entryMatches(entry, query) {
			continue
		}
		result = append(result, cloneEntry(entry))
	}
	r.mu.RUnlock()
	return result
}

func entryMatches(entry Entry, query string) bool {
	if strings.Contains(strings.ToLower(entry.Message), query) {
		return true
	}
	for key, value := range entry.Attrs {
		if strings.Contains(strings.ToLower(key), query) || strings.Contains(strings.ToLower(fmt.Sprint(value)), query) {
			return true
		}
	}
	return false
}

func cloneEntry(entry Entry) Entry {
	if entry.Attrs == nil {
		return entry
	}
	attrs := make(map[string]any, len(entry.Attrs))
	for key, value := range entry.Attrs {
		attrs[key] = value
	}
	entry.Attrs = attrs
	return entry
}
