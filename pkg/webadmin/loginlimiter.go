package webadmin

import (
	"sync"
	"time"
)

type loginLimiter struct {
	mu          sync.Mutex
	maxFailures int
	window      time.Duration
	now         func() time.Time
	entries     map[string]*loginEntry
}

type loginEntry struct {
	failures    int
	lockedUntil time.Time
	lastSeen    time.Time
}

func newLoginLimiter(maxFailures int, window time.Duration, now func() time.Time) *loginLimiter {
	if maxFailures <= 0 {
		maxFailures = 5
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	if now == nil {
		now = time.Now
	}
	return &loginLimiter{maxFailures: maxFailures, window: window, now: now, entries: map[string]*loginEntry{}}
}

func (l *loginLimiter) locked(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now().UTC()
	l.cleanup(now)
	entry := l.entries[ip]
	if entry == nil {
		return false, 0
	}
	entry.lastSeen = now
	if entry.lockedUntil.IsZero() {
		return false, 0
	}
	if now.Before(entry.lockedUntil) {
		return true, entry.lockedUntil.Sub(now)
	}
	delete(l.entries, ip)
	return false, 0
}

func (l *loginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now().UTC()
	l.cleanup(now)
	entry := l.entries[ip]
	if entry == nil {
		entry = &loginEntry{}
		l.entries[ip] = entry
	}
	entry.lastSeen = now
	entry.failures++
	if entry.failures >= l.maxFailures {
		entry.failures = 0
		entry.lockedUntil = now.Add(l.window)
	}
}

func (l *loginLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

func (l *loginLimiter) cleanup(now time.Time) {
	expireAfter := 2 * l.window
	for ip, entry := range l.entries {
		if entry.lockedUntil.IsZero() {
			if now.Sub(entry.lastSeen) > expireAfter {
				delete(l.entries, ip)
			}
			continue
		}
		if !now.Before(entry.lockedUntil) && now.Sub(entry.lockedUntil) > l.window {
			delete(l.entries, ip)
		}
	}
}
