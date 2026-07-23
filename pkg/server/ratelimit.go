package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"golang.org/x/time/rate"
)

type rateLimitRuntime struct {
	config  config.RateLimitConfig
	limiter *ipRateLimiter
}

// RateLimitController is the managed node's hot-swappable anonymous rate-limit
// policy. Each update replaces both the policy and its per-IP limiter set as one
// atomic snapshot.
type RateLimitController struct {
	current atomic.Value // rateLimitRuntime
}

func NewRateLimitController(cfg config.RateLimitConfig) *RateLimitController {
	controller := &RateLimitController{}
	controller.Update(cfg)
	return controller
}

func normalizeRateLimitConfig(cfg config.RateLimitConfig) config.RateLimitConfig {
	if cfg.AnonymousRPS <= 0 {
		cfg.AnonymousRPS = config.DefaultAnonymousRPS
	}
	if cfg.AnonymousBurst <= 0 {
		cfg.AnonymousBurst = config.DefaultAnonymousBurst
	}
	return cfg
}

func (c *RateLimitController) Update(cfg config.RateLimitConfig) {
	cfg = normalizeRateLimitConfig(cfg)
	c.current.Store(rateLimitRuntime{config: cfg, limiter: newIPRateLimiter(cfg.AnonymousRPS, cfg.AnonymousBurst)})
}

func (c *RateLimitController) Config() config.RateLimitConfig {
	return c.current.Load().(rateLimitRuntime).config
}

func (c *RateLimitController) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bucket, key := parseS3Path(r.URL.Path)
			if hasCredentials(r) || !isAnonymousObjectRead(r, bucket, key) {
				next.ServeHTTP(w, r)
				return
			}
			runtime := c.current.Load().(rateLimitRuntime)
			if !runtime.limiter.allow(clientIP(r, runtime.config.TrustForwarded)) {
				writeSlowDown(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

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
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				if ip := strings.TrimSpace(parts[i]); ip != "" {
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
