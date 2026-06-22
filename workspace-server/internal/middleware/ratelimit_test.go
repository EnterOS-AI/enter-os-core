package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// newTestLimiter spins up a tiny limiter with a 2-token/5s budget so tests can
// exhaust + recover without real-time delays.
func newTestLimiter(t *testing.T) (*RateLimiter, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rl := NewRateLimiter(2, 5*time.Second, ctx)
	r := gin.New()
	r.Use(rl.Middleware())
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return rl, r
}

// TestRateLimit_HeadersPresentOnAllowedRequest covers issue #105 — every
// response (not just 429s) must carry the X-RateLimit-* triplet so clients
// can back off proactively.
func TestRateLimit_HeadersPresentOnAllowedRequest(t *testing.T) {
	_, r := newTestLimiter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := w.Header().Get("X-RateLimit-Limit"); got != "2" {
		t.Errorf("X-RateLimit-Limit = %q, want 2", got)
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "1" {
		t.Errorf("X-RateLimit-Remaining = %q, want 1", got)
	}
	reset, err := strconv.Atoi(w.Header().Get("X-RateLimit-Reset"))
	if err != nil || reset < 0 || reset > 5 {
		t.Errorf("X-RateLimit-Reset = %q, want 0-5", w.Header().Get("X-RateLimit-Reset"))
	}
}

// TestRateLimit_XFF_BypassDocumented shows that when the test router is
// configured to trust the source proxy, a spoofed X-Forwarded-For header can
// rotate an attacker's effective IP and bypass per-IP rate limiting
// (documents the issue #179 vulnerability). Modern Gin defaults to trusting
// NO proxies, so we explicitly trust the test RemoteAddr to reproduce the
// pre-fix behavior; without this trust the bypass is correctly blocked.
func TestRateLimit_XFF_BypassDocumented(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rl := NewRateLimiter(2, 5*time.Second, ctx)

	r := gin.New()
	// Replicate the pre-fix trust model: the request source (10.0.0.1) is
	// treated as a trusted proxy, so c.ClientIP() reads X-Forwarded-For.
	if err := r.SetTrustedProxies([]string{"10.0.0.1/32"}); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	r.Use(rl.Middleware())
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// Exhaust both tokens for the real IP 10.0.0.1.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("setup request %d: want 200, got %d", i+1, w.Code)
		}
	}
	// Third request without XFF must be rate-limited.
	{
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("3rd request (no XFF): want 429, got %d", w.Code)
		}
	}
	// With trusted proxy config, spoofing X-Forwarded-For rotates the
	// effective IP → new bucket → bypass succeeds (returns 200).
	{
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("X-Forwarded-For", "20.0.0.1")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("XFF bypass did not work under trusted-proxy config: want 200, got %d", w.Code)
		}
	}
}

// TestRateLimit_XFF_NoBypassWithTrustedProxiesNil is the regression test for
// issue #179: after r.SetTrustedProxies(nil) is added to router.Setup(), a
// spoofed X-Forwarded-For header is ignored and the real RemoteAddr is used,
// so the bypass no longer works.
func TestRateLimit_XFF_NoBypassWithTrustedProxiesNil(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rl := NewRateLimiter(2, 5*time.Second, ctx)

	r := gin.New()
	// Fix for issue #179 — mirror what router.Setup() now does.
	if err := r.SetTrustedProxies(nil); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	r.Use(rl.Middleware())
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// Exhaust both tokens for RemoteAddr 10.0.0.2.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.RemoteAddr = "10.0.0.2:9999"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("setup request %d: want 200, got %d", i+1, w.Code)
		}
	}
	// Third plain request must be rate-limited.
	{
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.RemoteAddr = "10.0.0.2:9999"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("3rd plain request: want 429, got %d", w.Code)
		}
	}
	// Spoofed XFF must NOT rotate the bucket — still 429 because
	// SetTrustedProxies(nil) forces c.ClientIP() to return RemoteAddr.
	{
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.RemoteAddr = "10.0.0.2:9999"
		req.Header.Set("X-Forwarded-For", "99.99.99.99")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("XFF bypass still works after fix: want 429, got %d — SetTrustedProxies(nil) not effective", w.Code)
		}
	}
}

// TestRateLimit_RetryAfterOn429 — throttled responses must carry Retry-After
// per RFC 6585, so curl/fetch clients back off the exact required window.
func TestRateLimit_RetryAfterOn429(t *testing.T) {
	_, r := newTestLimiter(t)
	// Burn through both tokens.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d", i+1, w.Code)
		}
	}
	// Third should 429.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request: want 429, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Error("missing Retry-After header on 429")
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q on 429, want 0", got)
	}
}
