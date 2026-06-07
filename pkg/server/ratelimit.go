package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type ipRateLimiter struct {
	mu         sync.Mutex
	limiters   map[string]*rate.Limiter
	lastSeen   map[string]time.Time
	r          rate.Limit
	burst      int
	now        func() time.Time
	staleAfter time.Duration
}

func newIPRateLimiter(rps float64, burst int) *ipRateLimiter {
	if rps <= 0 {
		rps = 10
	}
	if burst <= 0 {
		burst = 20
	}
	return &ipRateLimiter{
		limiters:   map[string]*rate.Limiter{},
		lastSeen:   map[string]time.Time{},
		r:          rate.Limit(rps),
		burst:      burst,
		now:        time.Now,
		staleAfter: 10 * time.Minute,
	}
}

func (l *ipRateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now().UTC()
	l.cleanup(now)
	limiter := l.limiters[ip]
	if limiter == nil {
		limiter = rate.NewLimiter(l.r, l.burst)
		l.limiters[ip] = limiter
	}
	l.lastSeen[ip] = now
	return limiter.Allow()
}

func (l *ipRateLimiter) cleanup(now time.Time) {
	for ip, seen := range l.lastSeen {
		if now.Sub(seen) > l.staleAfter {
			delete(l.lastSeen, ip)
			delete(l.limiters, ip)
		}
	}
}

func clientIP(r *http.Request, trustForwarded bool) string {
	if trustForwarded {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			for _, part := range strings.Split(xff, ",") {
				if ip := strings.TrimSpace(part); ip != "" {
					return ip
				}
			}
		}
		if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
			return realIP
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
