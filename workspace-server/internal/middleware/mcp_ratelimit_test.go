package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newMCPTestRouter creates a minimal gin.Engine with the MCPRateLimiter applied
// and a single POST /mcp endpoint for test requests.
func newMCPTestRouter(t *testing.T, rate int, interval time.Duration) *gin.Engine {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rl := NewMCPRateLimiter(rate, interval, ctx)
	r := gin.New()
	r.POST("/mcp", rl.Middleware(), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	return r
}

// mcpReq builds a POST /mcp request with an Authorization: Bearer header.
func mcpReq(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// ─────────────────────────────────────────────────────────────────────────────

func TestMCPRateLimiter_AllowsUnderLimit(t *testing.T) {
	r := newMCPTestRouter(t, 5, time.Minute)
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, mcpReq("token-abc"))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}
}

func TestMCPRateLimiter_Blocks429OnExceed(t *testing.T) {
	r := newMCPTestRouter(t, 2, time.Minute)
	token := "token-xyz"

	// Drain the bucket.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, mcpReq(token))
		if w.Code != http.StatusOK {
			t.Fatalf("setup request %d: expected 200, got %d", i+1, w.Code)
		}
	}

	// Next request must be blocked.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, mcpReq(token))
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after exceeding limit, got %d", w.Code)
	}
}

func TestMCPRateLimiter_IndependentBucketsPerToken(t *testing.T) {
	r := newMCPTestRouter(t, 1, time.Minute)
	// Each unique token gets its own fresh bucket.
	for _, tok := range []string{"token-a", "token-b", "token-c"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, mcpReq(tok))
		if w.Code == http.StatusTooManyRequests {
			t.Errorf("token %q: expected separate bucket, got 429", tok)
		}
	}
}

func TestMCPRateLimiter_NoToken_Returns401(t *testing.T) {
	r := newMCPTestRouter(t, 10, time.Minute)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, mcpReq("")) // no Authorization header
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing token, got %d", w.Code)
	}
}

func TestMCPRateLimiter_SetsRateLimitHeaders(t *testing.T) {
	r := newMCPTestRouter(t, 10, time.Minute)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, mcpReq("header-fixture-token"))

	if w.Header().Get("X-RateLimit-Limit") != "10" {
		t.Errorf("X-RateLimit-Limit: got %q, want 10", w.Header().Get("X-RateLimit-Limit"))
	}
	if w.Header().Get("X-RateLimit-Remaining") == "" {
		t.Error("X-RateLimit-Remaining header missing")
	}
	if w.Header().Get("X-RateLimit-Reset") == "" {
		t.Error("X-RateLimit-Reset header missing")
	}
}

func TestMCPRateLimiter_ResetsAfterInterval(t *testing.T) {
	r := newMCPTestRouter(t, 1, 50*time.Millisecond)
	token := "reset-fixture-token"

	// Exhaust the bucket.
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, mcpReq(token))
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w1.Code)
	}

	// Verify blocked.
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, mcpReq(token))
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request (before reset): expected 429, got %d", w2.Code)
	}

	// Wait for the interval to expire.
	time.Sleep(60 * time.Millisecond)

	// Should be allowed again after the reset.
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, mcpReq(token))
	if w3.Code == http.StatusTooManyRequests {
		t.Errorf("expected bucket to reset after interval, still got 429")
	}
}

func TestMCPRateLimiter_RetryAfterOn429(t *testing.T) {
	r := newMCPTestRouter(t, 1, time.Minute)
	token := "retry-after-token"

	// Drain bucket.
	r.ServeHTTP(httptest.NewRecorder(), mcpReq(token))

	// Throttled request must carry Retry-After.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, mcpReq(token))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header on 429")
	}
	if w.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Errorf("X-RateLimit-Remaining: got %q, want 0", w.Header().Get("X-RateLimit-Remaining"))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

func TestTokenKey_IsDeterministic(t *testing.T) {
	k1 := tokenKey("my-secret-token")
	k2 := tokenKey("my-secret-token")
	if k1 != k2 {
		t.Error("tokenKey should be deterministic for same input")
	}
	k3 := tokenKey("different-token")
	if k1 == k3 {
		t.Error("tokenKey should produce different output for different tokens")
	}
}

func TestBearerFromHeader_Parsing(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"Bearer abc123", "abc123"},
		{"bearer abc123", "abc123"},
		{"BEARER abc123", "abc123"},
		{"", ""},
		{"Basic xyz", ""},
		{"Bearer", ""},
	}
	for _, tt := range tests {
		got := bearerFromHeader(tt.header)
		if got != tt.want {
			t.Errorf("bearerFromHeader(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}
