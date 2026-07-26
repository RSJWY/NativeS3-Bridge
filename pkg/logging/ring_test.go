package logging

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func testTime() time.Time { return time.Unix(1, 0) }

func TestRingSnapshotFiltersAndWraps(t *testing.T) {
	ring := NewRing(2)
	ring.Append(Entry{Level: "INFO", Message: "first"})
	ring.Append(Entry{Level: "WARN", Message: "second", Attrs: map[string]any{"bucket": "media"}})
	ring.Append(Entry{Level: "ERROR", Message: "third"})

	entries := ring.Snapshot(10, "warn", "media")
	if len(entries) != 1 || entries[0].Message != "second" {
		t.Fatalf("entries = %+v, want second", entries)
	}
	all := ring.Snapshot(10, "", "")
	if len(all) != 2 || all[0].Message != "third" || all[1].Message != "second" {
		t.Fatalf("entries = %+v, want newest wrapped entries", all)
	}
}

func TestRingConcurrentAppend(t *testing.T) {
	ring := NewRing(100)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := 0; index < 100; index++ {
				ring.Append(Entry{Level: "INFO", Message: "entry"})
			}
		}()
	}
	wg.Wait()
	if got := len(ring.Snapshot(1000, "", "")); got != 100 {
		t.Fatalf("snapshot length = %d, want 100", got)
	}
}

func TestRingQueryAppliesInclusiveTimeFiltersBeforeLimit(t *testing.T) {
	ring := NewRing(10)
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	ring.Append(Entry{Time: base, Level: "INFO", Message: "old"})
	ring.Append(Entry{Time: base.Add(time.Minute), Level: "ERROR", Message: "first match", Attrs: map[string]any{"component": "media"}})
	ring.Append(Entry{Time: base.Add(2 * time.Minute), Level: "ERROR", Message: "newest match", Attrs: map[string]any{"component": "media"}})
	ring.Append(Entry{Time: base.Add(3 * time.Minute), Level: "ERROR", Message: "too new", Attrs: map[string]any{"component": "media"}})
	since := base.Add(time.Minute)
	until := base.Add(2 * time.Minute)

	entries := ring.Query(QueryOptions{Limit: 1, Level: "error", Query: "MEDIA", Since: &since, Until: &until})
	if len(entries) != 1 || entries[0].Message != "newest match" {
		t.Fatalf("entries = %+v, want newest inclusive match", entries)
	}
	entries[0].Attrs["component"] = "changed"
	if got := ring.Query(QueryOptions{Limit: 1, Level: "error", Query: "media", Since: &since, Until: &until})[0].Attrs["component"]; got != "media" {
		t.Fatalf("query returned mutable attrs, got %v", got)
	}
}

func TestRingHandlerFiltersSensitiveAttrs(t *testing.T) {
	ring := NewRing(10)
	handler := NewRingHandler(slog.NewTextHandler(io.Discard, nil), ring)
	record := slog.NewRecord(testTime(), slog.LevelInfo, "request", 0)
	record.Add("access_key", "visible", "secret_key", "hidden", "password_hash", "hidden")
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	entry := ring.Snapshot(1, "", "")[0]
	if entry.Attrs["access_key"] != "visible" {
		t.Fatalf("access_key = %v", entry.Attrs["access_key"])
	}
	if _, ok := entry.Attrs["secret_key"]; ok {
		t.Fatal("secret_key must be filtered")
	}
}
