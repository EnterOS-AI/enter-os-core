package middleware

import (
	"context"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// newTestLimiterForKeyFor — same shape as newTestLimiter in ratelimit_test.go
// but exposes the *gin.Engine and lets the caller inject headers per-request.
func newTestLimiterForKeyFor(t *testing.T, rate int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rl := NewRateLimiter(rate, 5*time.Second, ctx)
	r := gin.New()
	if err := r.SetTrustedProxies(nil); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	r.Use(rl.Middleware())
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r
}

// TestKeyFor_OrgIdHeaderTrumpsBearerAndIP — when X-Molecule-Org-Id is set
// the bucket is keyed on it regardless of bearer token or IP. This is the
// load-bearing case for the production SaaS plane: every tenant routed
// through the same upstream proxy IP gets its own bucket because the
// CP attaches the org-id header.
func TestKeyFor_OrgIdHeaderTrumpsBearerAndIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rl := NewRateLimiter(2, 5*time.Second, ctx)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	c.Request.RemoteAddr = "10.0.0.1:1234"
	c.Request.Header.Set("X-Molecule-Org-Id", "org-aaa")
	c.Request.Header.Set("Authorization", "Bearer ignored-token-value")

	got := rl.keyFor(c)
	if got != "org:org-aaa" {
		t.Errorf("keyFor with org-id header: got %q, want %q", got, "org:org-aaa")
	}
}

// TestKeyFor_BearerTokenWhenNoOrgId — the per-tenant Caddy box path:
// no org-id header (canvas same-origin), but Authorization Bearer is
// always set by WorkspaceAuth-protected routes. Bucket keyed on the
// SHA-256 hex of the token so distinct sessions on the same egress IP
// get distinct buckets — and so the in-memory map can never become a
// token dump if the process is inspected.
func TestKeyFor_BearerTokenWhenNoOrgId(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rl := NewRateLimiter(2, 5*time.Second, ctx)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	c.Request.RemoteAddr = "10.0.0.1:1234"
	c.Request.Header.Set("Authorization", "Bearer secret-token-abc")

	got := rl.keyFor(c)
	expectedHash := fmt.Sprintf("%x", sha256.Sum256([]byte("secret-token-abc")))
	if got != "tok:"+expectedHash {
		t.Errorf("keyFor with bearer-only: got %q, want %q", got, "tok:"+expectedHash)
	}
	// Critical security pin: raw token must never appear in the key.
	if strings.Contains(got, "secret-token-abc") {
		t.Errorf("keyFor leaked raw bearer token in bucket key: %q", got)
	}
}

// TestKeyFor_IPFallbackWhenNoOrgIdNoBearer — anonymous probes (no auth,
// no tenant header) fall through to ClientIP keying. This is the only
// path that depended on the pre-#179 trust-XFF behaviour and is fine
// to keep IP-keyed because the surface is just /health, /buildinfo,
// and the registry-boot endpoints.
func TestKeyFor_IPFallbackWhenNoOrgIdNoBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rl := NewRateLimiter(2, 5*time.Second, ctx)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	c.Request.RemoteAddr = "203.0.113.1:1234"

	got := rl.keyFor(c)
	// gin.ClientIP() strips the port — we just need to confirm the prefix
	// and that the IP appears.
	if !strings.HasPrefix(got, "ip:") {
		t.Errorf("keyFor without auth/org headers: got %q, want prefix %q", got, "ip:")
	}
	if !strings.Contains(got, "203.0.113.1") {
		t.Errorf("keyFor IP fallback: got %q, want to contain %q", got, "203.0.113.1")
	}
}

// TestRateLimit_TwoOrgsSameIP_IndependentBuckets — the load-bearing
// regression test for issue #59. Two tenants behind the same upstream
// proxy must NOT share a bucket; the production SaaS-plane outage was
// every tenant collapsing to the proxy IP and saturating one bucket.
//
// Mutation invariant: removing the org-id branch from keyFor — say,
// returning "ip:" + c.ClientIP() unconditionally — collapses both
// tenants back into one bucket and this test fails on the 3rd
// request because it would 429 instead of 200.
func TestRateLimit_TwoOrgsSameIP_IndependentBuckets(t *testing.T) {
	r := newTestLimiterForKeyFor(t, 2)

	exhaust := func(orgID string) {
		t.Helper()
		for i := 0; i < 2; i++ {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = "10.0.0.1:1234" // SAME upstream proxy IP
			req.Header.Set("X-Molecule-Org-Id", orgID)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("setup orgID=%s req %d: want 200, got %d", orgID, i+1, w.Code)
			}
		}
	}

	exhaust("org-aaa")
	// org-aaa is now at 0 tokens. org-bbb's bucket must be FRESH.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Molecule-Org-Id", "org-bbb")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("org-bbb on same IP must have its own bucket: got %d, want 200 (issue #59 regression)", w.Code)
	}

	// Confirm org-aaa is still throttled — proves we're not just opening
	// the gate to everyone.
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Molecule-Org-Id", "org-aaa")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("org-aaa exhausted bucket: want 429, got %d", w.Code)
	}
}

// TestRateLimit_TwoTokensSameIP_IndependentBuckets — analog of the
// org-id case for the per-tenant Caddy box: two distinct user
// sessions on the same egress IP, distinguished only by their bearer
// tokens, must get independent buckets. This was the path Hongming
// hit on hongming.moleculesai.app — a single user with multiple
// browser tabs against one workspace-server box.
func TestRateLimit_TwoTokensSameIP_IndependentBuckets(t *testing.T) {
	r := newTestLimiterForKeyFor(t, 2)

	exhaust := func(token string) {
		t.Helper()
		for i := 0; i < 2; i++ {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = "127.0.0.1:1234" // local Caddy proxy — same for both
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("setup token=%s req %d: want 200, got %d", token, i+1, w.Code)
			}
		}
	}

	exhaust("user-a-token")
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer user-b-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("user-b token on same proxy IP must have its own bucket: got %d, want 200", w.Code)
	}
}

// TestRateLimit_SameOrgDifferentTokens_SharedBucket — counter-pin:
// ensure org-id keying really does collapse all tokens within one
// org into one bucket. This is the desired behaviour: a tenant that
// mints multiple tokens shouldn't be able to circumvent its quota
// by rotating tokens between requests. (The same-IP-different-org
// test above proves we don't collapse ACROSS orgs; this one proves
// we DO collapse WITHIN one org.)
func TestRateLimit_SameOrgDifferentTokens_SharedBucket(t *testing.T) {
	r := newTestLimiterForKeyFor(t, 2)

	for _, tok := range []string{"token-1", "token-2"} {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("X-Molecule-Org-Id", "org-shared")
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("setup tok=%s: want 200, got %d", tok, w.Code)
		}
	}
	// Bucket should be exhausted now — third request, even with a fresh
	// token, must 429 because the org-id is keying it.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Molecule-Org-Id", "org-shared")
	req.Header.Set("Authorization", "Bearer token-3")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("rotating tokens within one org should NOT bypass the quota: got %d, want 429", w.Code)
	}
}

// TestRateLimit_Middleware_RoutesThroughKeyFor is the AST gate (mirror
// of #36/#10/#12's gates). Pins the SSOT routing invariant:
// (*RateLimiter).Middleware MUST call rl.keyFor and MUST NOT carry a
// direct c.ClientIP() call (= the parallel-impl drift this PR fixes).
//
// Mutation invariant: a future PR that re-introduces direct IP keying
// in Middleware (`ip := c.ClientIP()`) makes this test fail. That's
// the signal to either (a) extend keyFor's contract to cover the new
// case OR (b) update this gate with an explicit reason. Either way the
// drift gets a reviewer's attention before shipping.
func TestRateLimit_Middleware_RoutesThroughKeyFor(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "ratelimit.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse ratelimit.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		f, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		// Match `func (rl *RateLimiter) Middleware() ...`
		if f.Name.Name != "Middleware" {
			return true
		}
		if f.Recv == nil || len(f.Recv.List) != 1 {
			return true
		}
		star, ok := f.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			return true
		}
		if id, ok := star.X.(*ast.Ident); !ok || id.Name != "RateLimiter" {
			return true
		}
		fn = f
		return false
	})
	if fn == nil {
		t.Fatal("(*RateLimiter).Middleware not found — was it renamed? update this gate or the SSOT routing assumption")
	}

	var (
		callsKeyFor   bool
		callsClientIP bool
	)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "keyFor":
			callsKeyFor = true
		case "ClientIP":
			callsClientIP = true
		}
		return true
	})

	if !callsKeyFor {
		t.Error("(*RateLimiter).Middleware must call rl.keyFor for SSOT bucket-key derivation — see issue #59. Found no keyFor call.")
	}
	if callsClientIP {
		t.Error("(*RateLimiter).Middleware carries a direct c.ClientIP() call. This is the parallel-impl drift issue #59 fixed. " +
			"Either route through rl.keyFor OR — if a new use case truly needs direct IP — extend keyFor's contract first and update this gate to allow the specific delta.")
	}
}
