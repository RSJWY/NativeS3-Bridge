package webadmin

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestLoginLimiterLocksAndExpires(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	limiter := newLoginLimiter(2, time.Minute, func() time.Time { return now })

	if locked, _ := limiter.locked("127.0.0.1"); locked {
		t.Fatal("fresh IP is locked")
	}
	limiter.recordFailure("127.0.0.1")
	if locked, _ := limiter.locked("127.0.0.1"); locked {
		t.Fatal("IP locked before threshold")
	}
	limiter.recordFailure("127.0.0.1")
	if locked, retryAfter := limiter.locked("127.0.0.1"); !locked || retryAfter != time.Minute {
		t.Fatalf("locked = %v retryAfter = %v, want locked for 1m", locked, retryAfter)
	}

	now = now.Add(time.Minute + time.Nanosecond)
	if locked, _ := limiter.locked("127.0.0.1"); locked {
		t.Fatal("IP still locked after window expired")
	}
}

func TestLoginLimiterSuccessClearsAndIPsAreIsolated(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	limiter := newLoginLimiter(2, time.Minute, func() time.Time { return now })

	limiter.recordFailure("127.0.0.1")
	limiter.recordFailure("127.0.0.2")
	limiter.recordFailure("127.0.0.2")
	limiter.recordSuccess("127.0.0.1")
	limiter.recordFailure("127.0.0.1")

	if locked, _ := limiter.locked("127.0.0.1"); locked {
		t.Fatal("successful login did not clear first IP failures")
	}
	if locked, _ := limiter.locked("127.0.0.2"); !locked {
		t.Fatal("second IP should remain locked")
	}
}

func TestLoginLimiterLazyCleanup(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	limiter := newLoginLimiter(3, time.Minute, func() time.Time { return now })
	limiter.recordFailure("127.0.0.1")

	now = now.Add(3*time.Minute + time.Nanosecond)
	limiter.recordFailure("127.0.0.2")

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if _, ok := limiter.entries["127.0.0.1"]; ok {
		t.Fatal("stale entry was not cleaned")
	}
}

func TestLoginLimiterConcurrent(t *testing.T) {
	limiter := newLoginLimiter(1000, time.Minute, time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := fmt.Sprintf("127.0.0.%d", i%5)
			for j := 0; j < 100; j++ {
				limiter.recordFailure(ip)
				_, _ = limiter.locked(ip)
				limiter.recordSuccess(ip)
			}
		}(i)
	}
	wg.Wait()
}
