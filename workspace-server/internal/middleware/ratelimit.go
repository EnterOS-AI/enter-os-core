// Package middleware provides HTTP middleware for the platform API.
package middleware

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimiter implements a token bucket rate limiter keyed by tenant
// identity (org id, then bearer token, then client IP — see keyFor).
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     int // tokens per interval
	interval time.Duration
}

type bucket struct {
	tokens    int
	lastReset time.Time
}

// NewRateLimiter creates a rate limiter with the given rate per interval.
// Pass a context to stop the cleanup goroutine on shutdown.
func NewRateLimiter(rate int, interval time.Duration, ctx context.Context) *RateLimiter {
	rl := &RateLimiter{
		buckets:  make(map[string]*bucket),
		rate:     rate,
		interval: interval,
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rl.mu.Lock()
				cutoff := time.Now().Add(-10 * time.Minute)
				for k, b := range rl.buckets {
					if b.lastReset.Before(cutoff) {
						delete(rl.buckets, k)
					}
				}
				rl.mu.Unlock()
			}
		}
	}()
	return rl
}

// keyFor returns the bucket identifier for this request. Priority:
//
//  1. X-Molecule-Org-Id header — when present (CP-routed SaaS traffic),
//     isolates tenants from each other regardless of the upstream proxy IP
//     they all share.
//  2. SHA-256 of Authorization Bearer token — when present (per-workspace
//     bearer, ADMIN_TOKEN, org-scoped API token). On a per-tenant Caddy
//     box where the org-id header isn't attached, this still distinguishes
//     distinct user sessions on the same egress IP.
//  3. ClientIP() — anonymous probes, /health scrapes, registry boot
//     signals (when SetTrustedProxies(nil) is in effect, this is the
//     direct TCP RemoteAddr — fine for the probe surface, not fine as a
//     primary key behind a proxy, hence the priority order above).
//
// Mixing these namespaces is fine because they never collide: org ids
// are UUIDs ("org:..."), token hashes are 64-char hex ("tok:..."), IPs
// contain dots/colons ("ip:...").
//
// Security note on X-Molecule-Org-Id spoofing: the rate limiter runs
// BEFORE TenantGuard, so the org-id value here is unvalidated. A caller
// reaching workspace-server directly could spoof the header to drain
// another org's bucket. In production this surface is closed by the
// CP/Caddy front: tenant SGs reject :8080 from the public internet, and
// CP rewrites the header to the verified org. If a future deployment
// exposes :8080 directly, validate the org-id (e.g. against
// MOLECULE_ORG_ID) before keying on it, or move this middleware after
// TenantGuard. The token-hash and IP fallbacks are unspoofable.
//
// Issue #59 — replaces the previous IP-only keying that silently
// collapsed all canvas traffic into one bucket once #179 disabled
// proxy-header trust. See the issue for the deployment-shape analysis.
func (rl *RateLimiter) keyFor(c *gin.Context) string {
	if orgID := strings.TrimSpace(c.GetHeader("X-Molecule-Org-Id")); orgID != "" {
		return "org:" + orgID
	}
	if tok := bearerFromHeader(c.GetHeader("Authorization")); tok != "" {
		return "tok:" + tokenKey(tok)
	}
	return "ip:" + c.ClientIP()
}

// Middleware returns a Gin middleware that rate limits per caller. The
// caller-key derivation lives in keyFor — see that function's doc for
// the priority list and rationale.
func (rl *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Tier-1b dev-mode hatch — same gate as AdminAuth / WorkspaceAuth /
		// discovery. On a local single-user Docker setup the 600-req/min
		// bucket fills fast: a 15-workspace canvas + activity polling +
		// approvals polling + A2A overlay + initial hydration all land in
		// one bucket (whichever keyFor returns — typically the dev user's
		// IP or shared admin token), so a minute of active use can trip
		// 429 and blank the page. Gated by MOLECULE_ENV=development +
		// empty ADMIN_TOKEN so SaaS production keeps the bucket.
		if isDevModeFailOpen() {
			c.Header("X-RateLimit-Limit", "unlimited")
			c.Next()
			return
		}

		key := rl.keyFor(c)

		rl.mu.Lock()
		b, exists := rl.buckets[key]
		if !exists {
			b = &bucket{tokens: rl.rate, lastReset: time.Now()}
			rl.buckets[key] = b
		}

		// Reset tokens if interval has passed
		if time.Since(b.lastReset) >= rl.interval {
			b.tokens = rl.rate
			b.lastReset = time.Now()
		}

		// Issue #105 — advertise the current bucket state so clients and
		// monitoring tools can back off proactively. Headers are set on every
		// response (both allowed and throttled) so they're observable against
		// any endpoint — /health, /metrics, and every /workspaces/* route.
		//
		// The `reset` value is seconds until the current bucket refills,
		// matching the RFC 6585 Retry-After spec for 429 responses and the
		// de-facto X-RateLimit-Reset convention (GitHub, Stripe, etc.).
		remaining := b.tokens - 1
		if remaining < 0 {
			remaining = 0
		}
		resetSeconds := int(time.Until(b.lastReset.Add(rl.interval)).Seconds())
		if resetSeconds < 0 {
			resetSeconds = 0
		}
		c.Header("X-RateLimit-Limit", strconv.Itoa(rl.rate))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
		c.Header("X-RateLimit-Reset", strconv.Itoa(resetSeconds))

		if b.tokens <= 0 {
			rl.mu.Unlock()
			// Retry-After is the canonical 429 signal per RFC 6585.
			c.Header("Retry-After", strconv.Itoa(resetSeconds))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":       "rate limit exceeded",
				"retry_after": resetSeconds,
			})
			c.Abort()
			return
		}

		b.tokens--
		rl.mu.Unlock()

		c.Next()
	}
}
