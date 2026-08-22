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

// Per-IP budgets. A credential guess (a failed bearer, a token login, a code
// exchange) gets a tight budget; the OAuth flow steps and webhooks are hit by
// legitimate clients in bursts.
const (
	authRatePerMinute = 10
	authRateBurst     = 10
	flowRatePerMinute = 60
	flowRateBurst     = 30
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

// Exhausted reports whether key has no budget left, without consuming any.
func (l *ipLimiter) Exhausted(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	return b != nil && b.lim.Tokens() < 1
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

// AuthRateLimit is the budget for the routes where every request is a
// credential guess (token login, code exchange). Mounted by the route
// registration in handler.
func AuthRateLimit() gin.HandlerFunc {
	return RateLimit(authRatePerMinute, authRateBurst)
}

// FlowRateLimit is the looser budget for the OAuth flow steps (start,
// callback), which allocate server state per call but guess nothing.
func FlowRateLimit() gin.HandlerFunc {
	return RateLimit(flowRatePerMinute, flowRateBurst)
}

// AuthGuard is the per-IP budget of FAILED credential checks shared by every
// place a bearer is resolved (REST, the WS auth frame): a failure consumes
// one, an exhausted IP is refused before the check runs, and a credential
// that authenticates costs nothing — so a valid client is never limited.
type AuthGuard struct{ fails *ipLimiter }

// NewAuthGuard returns a guard with the credential-guess budget.
func NewAuthGuard() *AuthGuard {
	return &AuthGuard{fails: newIPLimiter(authRatePerMinute, authRateBurst)}
}

// Exhausted reports whether ip has failed too often to be checked again now.
// A nil guard never refuses.
func (g *AuthGuard) Exhausted(ip string) bool { return g != nil && g.fails.Exhausted(ip) }

// Failed records one failed check for ip.
func (g *AuthGuard) Failed(ip string) {
	if g != nil {
		g.fails.Allow(ip)
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
