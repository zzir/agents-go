package server

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// rateLimiterMaxKeys caps the per-IP bucket map. At the cap, stale entries are
// pruned; if every entry is fresh the map stops growing and new clients share
// fate with a full table (429) rather than growing memory without bound.
const rateLimiterMaxKeys = 10000

// Per-IP budgets. The auth surface is a credential oracle, so it gets a tight
// budget; webhooks fire from machines and legitimately burst.
const (
	authRatePerMinute = 10
	authRateBurst     = 10
	hookRatePerMinute = 60
	hookRateBurst     = 30
)

// ipLimiter is a per-key (client IP) token-bucket rate limiter.
type ipLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
	limit   rate.Limit
	burst   int
}

type ipBucket struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// newIPLimiter allows perMinute sustained requests per key with the given burst.
func newIPLimiter(perMinute, burst int) *ipLimiter {
	return &ipLimiter{
		buckets: make(map[string]*ipBucket),
		limit:   rate.Limit(float64(perMinute) / 60.0),
		burst:   burst,
	}
}

// Allow reports whether one request for key may proceed now.
func (l *ipLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= rateLimiterMaxKeys {
			l.pruneLocked(now)
			if len(l.buckets) >= rateLimiterMaxKeys {
				return false
			}
		}
		b = &ipBucket{lim: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[key] = b
	}
	b.lastSeen = now
	return b.lim.Allow()
}

// pruneLocked drops entries idle long enough to have refilled their burst —
// forgetting them changes nothing about what they would be allowed.
func (l *ipLimiter) pruneLocked(now time.Time) {
	idle := time.Duration(float64(l.burst)/float64(l.limit)) * time.Second
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > idle {
			delete(l.buckets, k)
		}
	}
}

// RateLimit is a gin middleware enforcing a per-client-IP request rate. The
// client IP honors X-Forwarded-For only from proxies named in
// SetTrustedProxies; everyone else is keyed by their direct address.
func RateLimit(perMinute, burst int) gin.HandlerFunc {
	l := newIPLimiter(perMinute, burst)
	return func(c *gin.Context) {
		if !l.Allow(c.ClientIP()) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests,
				protocol.NewErrorResponse(protocol.CodeRateLimited, "too many requests; slow down"))
			return
		}
		c.Next()
	}
}
