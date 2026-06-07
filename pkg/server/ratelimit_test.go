package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestIPRateLimiterAllowsBurstThenRejects(t *testing.T) {
	limiter := newIPRateLimiter(1, 2)

	if !limiter.allow("127.0.0.1") || !limiter.allow("127.0.0.1") {
		t.Fatal("burst requests should be allowed")
	}
	if limiter.allow("127.0.0.1") {
		t.Fatal("third immediate request should be rejected")
	}
	if !limiter.allow("127.0.0.2") {
		t.Fatal("different IP should have independent limiter")
	}
}

func TestClientIPTrustForwarded(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.10, 10.0.0.2")
	req.Header.Set("X-Real-IP", "203.0.113.20")

	if got := clientIP(req, false); got != "10.0.0.1" {
		t.Fatalf("clientIP trust off = %q, want remote host", got)
	}
	if got := clientIP(req, true); got != "203.0.113.10" {
		t.Fatalf("clientIP trust on = %q, want first XFF", got)
	}

	req.Header.Del("X-Forwarded-For")
	if got := clientIP(req, true); got != "203.0.113.20" {
		t.Fatalf("clientIP X-Real-IP = %q, want 203.0.113.20", got)
	}
}

func TestIPRateLimiterLazyCleanupAndConcurrent(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	limiter := newIPRateLimiter(100, 100)
	limiter.now = func() time.Time { return now }
	limiter.staleAfter = time.Minute
	_ = limiter.allow("127.0.0.1")
	now = now.Add(time.Minute + time.Nanosecond)
	_ = limiter.allow("127.0.0.2")

	limiter.mu.Lock()
	if _, ok := limiter.limiters["127.0.0.1"]; ok {
		limiter.mu.Unlock()
		t.Fatal("stale limiter was not cleaned")
	}
	limiter.mu.Unlock()

	limiter = newIPRateLimiter(1000, 1000)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = limiter.allow(fmt.Sprintf("127.0.0.%d", i%5))
			}
		}(i)
	}
	wg.Wait()
}
