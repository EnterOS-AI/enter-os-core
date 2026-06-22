package middleware

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// MCPRateLimiter implements a per-bearer-token rate limiter for the MCP bridge.
// Unlike the IP-based RateLimiter, this one keys on the bearer token so that
// a single long-lived opencode SSE connection cannot issue more than `rate`
// tool-call requests per `interval`.
//
// The token is stored as a SHA-256 hash (hex), never as plaintext, so the
// in-memory table does not become a token dump if the process is inspected.
type MCPRateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*mcpBucket
	rate     int
	interval time.Duration
}

type mcpBucket struct {
	tokens    int
	lastReset time.Time
}

// NewMCPRateLimiter creates a rate limiter with the given rate per interval.
// Pass a context to stop the background cleanup goroutine on shutdown.
func NewMCPRateLimiter(rate int, interval time.Duration, ctx context.Context) *MCPRateLimiter {
	rl := &MCPRateLimiter{
		buckets:  make(map[string]*mcpBucket),
		rate:     rate,
		interval: interval,
	}
	go func() {
		defer recoverPanic("mcp_ratelimit: bucket cleanup")
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

// Middleware returns a Gin middleware that rate limits MCP requests by bearer token.
// Requests without a bearer token are rejected with 401 (WorkspaceAuth should
// have already handled this, but we guard defensively).
func (rl *MCPRateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tok := bearerFromHeader(c.GetHeader("Authorization"))
		if tok == "" {
			// WorkspaceAuth already rejected missing tokens; this path should
			// be unreachable in production. Return 401 defensively.
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}

		// Hash the token so raw values are never stored in the bucket map.
		key := tokenKey(tok)

		rl.mu.Lock()
		b, exists := rl.buckets[key]
		if !exists {
			b = &mcpBucket{tokens: rl.rate, lastReset: time.Now()}
			rl.buckets[key] = b
		}
		if time.Since(b.lastReset) >= rl.interval {
			b.tokens = rl.rate
			b.lastReset = time.Now()
		}

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
			c.Header("Retry-After", strconv.Itoa(resetSeconds))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":       "MCP rate limit exceeded",
				"retry_after": resetSeconds,
			})
			return
		}
		b.tokens--
		rl.mu.Unlock()

		c.Next()
	}
}

// tokenKey returns the hex SHA-256 of a bearer token for use as a bucket key.
func tokenKey(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return fmt.Sprintf("%x", sum)
}

// bearerFromHeader extracts the token from an "Authorization: Bearer <tok>"
// header value. Returns "" when the header is absent or malformed.
func bearerFromHeader(authHeader string) string {
	const prefix = "Bearer "
	if len(authHeader) > len(prefix) && strings.EqualFold(authHeader[:len(prefix)], prefix) {
		return authHeader[len(prefix):]
	}
	return ""
}
