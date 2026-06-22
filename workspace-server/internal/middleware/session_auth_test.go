package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resetSessionCache clears global cache state between tests.
func resetSessionCache() {
	sessionCache.Lock()
	defer sessionCache.Unlock()
	sessionCache.entries = make(map[string]sessionCacheEntry)
}

// resetIdentityCache clears global identity cache state between tests.
func resetIdentityCache() {
	identityCache.Lock()
	defer identityCache.Unlock()
	identityCache.entries = make(map[string]identityCacheEntry)
}

// mockCPMeServer builds an httptest server that returns the given
// status/body for /cp/auth/me. Also tracks hit count via the returned atomic.
func mockCPMeServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/cp/auth/me") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s, hits
}

func TestVerifiedCPIdentity_ResolvesAndCaches(t *testing.T) {
	resetIdentityCache()
	srv, hits := mockCPMeServer(t, 200, `{"user_id":"u_123","email":"kim@example.com"}`)
	t.Setenv("CP_UPSTREAM_URL", srv.URL)
	t.Setenv("MOLECULE_ORG_SLUG", "acme")

	uid, email, ok := VerifiedCPIdentity("session=valid")
	if !ok || uid != "u_123" || email != "kim@example.com" {
		t.Errorf("expected authenticated u_123/kim@example.com, got %q/%q/%v", uid, email, ok)
	}
	if hits.Load() != 1 {
		t.Errorf("expected 1 upstream hit; got %d", hits.Load())
	}

	uid, email, ok = VerifiedCPIdentity("session=valid")
	if !ok || uid != "u_123" || email != "kim@example.com" {
		t.Errorf("cached call should resolve; got %q/%q/%v", uid, email, ok)
	}
	if hits.Load() != 1 {
		t.Errorf("identity cache should prevent second upstream hit; got %d", hits.Load())
	}
}

func TestVerifiedCPIdentity_MissingFields(t *testing.T) {
	resetIdentityCache()
	srv, _ := mockCPMeServer(t, 200, `{"user_id":"u_123"}`)
	t.Setenv("CP_UPSTREAM_URL", srv.URL)
	t.Setenv("MOLECULE_ORG_SLUG", "acme")

	uid, email, ok := VerifiedCPIdentity("session=partial")
	if ok {
		t.Errorf("missing email should be unauthenticated; got %q/%q/%v", uid, email, ok)
	}
}

func TestVerifiedCPIdentity_UpstreamError(t *testing.T) {
	resetIdentityCache()
	srv, _ := mockCPMeServer(t, 502, `{}`)
	t.Setenv("CP_UPSTREAM_URL", srv.URL)
	t.Setenv("MOLECULE_ORG_SLUG", "acme")

	uid, email, ok := VerifiedCPIdentity("session=bad")
	if ok {
		t.Errorf("502 should be unauthenticated; got %q/%q/%v", uid, email, ok)
	}
}

func TestVerifiedCPIdentity_NoCookie(t *testing.T) {
	resetIdentityCache()
	uid, email, ok := VerifiedCPIdentity("")
	if ok || uid != "" || email != "" {
		t.Errorf("empty cookie should be unauthenticated; got %q/%q/%v", uid, email, ok)
	}
}

// mockCPServer builds an httptest server that returns the given
// status/body for /cp/auth/tenant-member. Also tracks hit count via
// the returned atomic so tests can verify cache behavior.
func mockCPServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/cp/auth/tenant-member") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s, hits
}

func TestVerifiedCPSession_EmptyCookie(t *testing.T) {
	resetSessionCache()
	ok, presented, _ := VerifiedCPSession("")
	if ok || presented {
		t.Errorf("empty cookie should be (false, false); got (%v, %v)", ok, presented)
	}
}

func TestVerifiedCPSession_NoSlugConfigured(t *testing.T) {
	resetSessionCache()
	t.Setenv("CP_UPSTREAM_URL", "https://cp.test")
	t.Setenv("MOLECULE_ORG_SLUG", "")
	ok, presented, _ := VerifiedCPSession("session=foo")
	// Without a slug we can't ask about tenant membership. Must
	// refuse (false, false) — caller falls through to bearer tier.
	if ok || presented {
		t.Errorf("no slug should be (false, false); got (%v, %v)", ok, presented)
	}
}

func TestVerifiedCPSession_NoCPConfigured(t *testing.T) {
	resetSessionCache()
	t.Setenv("CP_UPSTREAM_URL", "")
	t.Setenv("MOLECULE_ORG_SLUG", "acme")
	ok, presented, _ := VerifiedCPSession("session=foo")
	// Self-hosted path: CP not configured, but cookie WAS presented.
	// Presented=true lets the caller know not to fall through to
	// bearer as if no credential arrived.
	if ok || !presented {
		t.Errorf("no CP should be (false, true); got (%v, %v)", ok, presented)
	}
}

func TestVerifiedCPSession_MemberTrue(t *testing.T) {
	resetSessionCache()
	srv, hits := mockCPServer(t, 200, `{"member":true,"user_id":"u_1","role":"owner","org_id":"org_1"}`)
	t.Setenv("CP_UPSTREAM_URL", srv.URL)
	t.Setenv("MOLECULE_ORG_SLUG", "acme")

	ok, presented, _ := VerifiedCPSession("session=valid")
	if !ok || !presented {
		t.Errorf("valid member should be (true, true); got (%v, %v)", ok, presented)
	}
	if hits.Load() != 1 {
		t.Errorf("expected 1 upstream hit; got %d", hits.Load())
	}

	// Second call must be served from cache.
	ok, _, _ = VerifiedCPSession("session=valid")
	if !ok {
		t.Errorf("cached call should still be true")
	}
	if hits.Load() != 1 {
		t.Errorf("cache miss: expected still 1 upstream hit; got %d", hits.Load())
	}
}

func TestVerifiedCPSession_MemberFalse(t *testing.T) {
	resetSessionCache()
	// CP returns 200 but member=false — user is authed but not in this org
	srv, hits := mockCPServer(t, 200, `{"member":false}`)
	t.Setenv("CP_UPSTREAM_URL", srv.URL)
	t.Setenv("MOLECULE_ORG_SLUG", "acme")

	ok, presented, _ := VerifiedCPSession("session=wrong-tenant")
	if ok || !presented {
		t.Errorf("non-member should be (false, true); got (%v, %v)", ok, presented)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 upstream hit")
	}
	// Cached negatively.
	_, _, _ = VerifiedCPSession("session=wrong-tenant")
	if hits.Load() != 1 {
		t.Errorf("negative result should cache too; got %d hits", hits.Load())
	}
}

func TestVerifiedCPSession_Upstream401(t *testing.T) {
	resetSessionCache()
	srv, _ := mockCPServer(t, 401, ``)
	t.Setenv("CP_UPSTREAM_URL", srv.URL)
	t.Setenv("MOLECULE_ORG_SLUG", "acme")

	ok, presented, _ := VerifiedCPSession("session=expired")
	if ok || !presented {
		t.Errorf("401 upstream should be (false, true); got (%v, %v)", ok, presented)
	}
}

func TestVerifiedCPSession_MalformedJSON(t *testing.T) {
	resetSessionCache()
	srv, _ := mockCPServer(t, 200, `not-json`)
	t.Setenv("CP_UPSTREAM_URL", srv.URL)
	t.Setenv("MOLECULE_ORG_SLUG", "acme")

	ok, presented, _ := VerifiedCPSession("session=broken")
	if ok || !presented {
		t.Errorf("malformed body should be (false, true); got (%v, %v)", ok, presented)
	}
}

func TestVerifiedCPSession_TransportErrorNotCached(t *testing.T) {
	resetSessionCache()
	// Point at a port that's definitely refused.
	t.Setenv("CP_UPSTREAM_URL", "http://127.0.0.1:1")
	t.Setenv("MOLECULE_ORG_SLUG", "acme")

	ok, presented, _ := VerifiedCPSession("session=whatever")
	if ok || !presented {
		t.Errorf("transport error should be (false, true); got (%v, %v)", ok, presented)
	}
	// Transport errors must NOT be cached — otherwise a 3s CP blip
	// locks every user out for the negative-TTL window.
	sessionCache.Lock()
	n := len(sessionCache.entries)
	sessionCache.Unlock()
	if n != 0 {
		t.Errorf("transport error cached %d entries; want 0", n)
	}
}

func TestVerifiedCPSession_CrossTenantIsolation(t *testing.T) {
	resetSessionCache()
	// Even if we have a valid session for tenant A, asking for
	// tenant B's membership must hit the CP separately. Same cookie
	// with different tenant slug → different cache key.
	reqs := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.URL.RawQuery)
		// Return member=true for slug=acme, member=false for slug=bob
		if strings.Contains(r.URL.RawQuery, "slug=acme") {
			_, _ = w.Write([]byte(`{"member":true,"user_id":"u_1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"member":false}`))
	}))
	defer srv.Close()
	t.Setenv("CP_UPSTREAM_URL", srv.URL)

	cookie := "session=shared-auth"

	t.Setenv("MOLECULE_ORG_SLUG", "acme")
	if ok, _, _ := VerifiedCPSession(cookie); !ok {
		t.Errorf("acme should say member=true")
	}

	t.Setenv("MOLECULE_ORG_SLUG", "bob")
	if ok, _, _ := VerifiedCPSession(cookie); ok {
		t.Errorf("bob tenant must NOT accept acme cookie despite same session bytes")
	}
	if len(reqs) != 2 {
		t.Errorf("cross-tenant should issue 2 upstream calls; got %d", len(reqs))
	}
}

func TestSessionCache_BoundedEviction(t *testing.T) {
	resetSessionCache()
	// Fill beyond cap and verify size stays roughly bounded.
	// Not testing exact eviction policy (random) — just that we
	// don't grow unbounded.
	for i := 0; i < sessionCacheMax+500; i++ {
		sessionCachePut(fmt.Sprintf("k%d", i), true, "")
	}
	sessionCache.Lock()
	n := len(sessionCache.entries)
	sessionCache.Unlock()
	if n > sessionCacheMax {
		t.Errorf("cache grew to %d, exceeds cap %d", n, sessionCacheMax)
	}
}

func TestSessionCache_ExpiredEntryIgnored(t *testing.T) {
	resetSessionCache()
	key := "k-expired"
	sessionCache.Lock()
	sessionCache.entries[key] = sessionCacheEntry{
		expiresAt: time.Now().Add(-1 * time.Second),
		ok:        true,
	}
	sessionCache.Unlock()
	if ok, _, hit := sessionCacheGet(key); ok || hit {
		t.Errorf("expired entry must not hit; got ok=%v hit=%v", ok, hit)
	}
}

func TestCacheKey_SlugSeparator(t *testing.T) {
	// ("a","bc") and ("ab","c") must not collide.
	if cacheKey("a", "bc") == cacheKey("ab", "c") {
		t.Errorf("cacheKey collides on ambiguous splits")
	}
}

func TestTenantSlug(t *testing.T) {
	t.Setenv("MOLECULE_ORG_SLUG", "acme-corp")
	if got := tenantSlug(); got != "acme-corp" {
		t.Errorf("tenantSlug() = %q, want %q", got, "acme-corp")
	}
}

func TestTenantSlug_TrimSpace(t *testing.T) {
	t.Setenv("MOLECULE_ORG_SLUG", "  spaced-slug  ")
	if got := tenantSlug(); got != "spaced-slug" {
		t.Errorf("tenantSlug() = %q, want %q", got, "spaced-slug")
	}
}

func TestTenantSlug_Empty(t *testing.T) {
	t.Setenv("MOLECULE_ORG_SLUG", "")
	if got := tenantSlug(); got != "" {
		t.Errorf("tenantSlug() = %q, want empty", got)
	}
}

func TestCPSessionVerifyURL(t *testing.T) {
	t.Setenv("CP_UPSTREAM_URL", "https://cp.test")
	got := cpSessionVerifyURL("acme")
	want := "https://cp.test/cp/auth/tenant-member?slug=acme"
	if got != want {
		t.Errorf("cpSessionVerifyURL() = %q, want %q", got, want)
	}
}

func TestCPSessionVerifyURL_TrailingSlash(t *testing.T) {
	t.Setenv("CP_UPSTREAM_URL", "https://cp.test/")
	got := cpSessionVerifyURL("acme")
	want := "https://cp.test/cp/auth/tenant-member?slug=acme"
	if got != want {
		t.Errorf("cpSessionVerifyURL() = %q, want %q", got, want)
	}
}

func TestCPSessionVerifyURL_EscapeSlug(t *testing.T) {
	t.Setenv("CP_UPSTREAM_URL", "https://cp.test")
	got := cpSessionVerifyURL("acme corp")
	want := "https://cp.test/cp/auth/tenant-member?slug=acme+corp"
	if got != want {
		t.Errorf("cpSessionVerifyURL() = %q, want %q", got, want)
	}
}

func TestCPSessionVerifyURL_NoCPConfigured(t *testing.T) {
	t.Setenv("CP_UPSTREAM_URL", "")
	if got := cpSessionVerifyURL("acme"); got != "" {
		t.Errorf("cpSessionVerifyURL() = %q, want empty when CP_UPSTREAM_URL unset", got)
	}
}
