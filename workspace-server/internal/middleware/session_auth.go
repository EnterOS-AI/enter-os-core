package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// sessionCache holds short-lived verification results for upstream
// session-cookie checks. Entries are scoped BY TENANT SLUG so one
// tenant's cache can't satisfy another tenant's check even when the
// same cookie is presented.
//
// Keyed by a sha256 of (slug + cookie) rather than raw cookie bytes:
//   - Avoids storing raw session tokens in memory for longer than
//     needed to look them up.
//   - Makes the cache lookup deterministic regardless of cookie
//     ordering / whitespace that browsers sometimes introduce.
//
// Bounded: we evict random entries when size breaches sessionCacheMax.
// Periodic sweeper GCs expired entries even when they aren't re-hit.
var sessionCache = struct {
	sync.Mutex
	entries map[string]sessionCacheEntry
}{entries: make(map[string]sessionCacheEntry)}

// identityCache holds short-lived /cp/auth/me responses for verified canvas
// sessions. It is separate from sessionCache because a session may be valid
// while the identity endpoint is temporarily unavailable, and we want the
// identity lookup to retry without poisoning the session-validity cache.
var identityCache = struct {
	sync.Mutex
	entries map[string]identityCacheEntry
}{entries: make(map[string]identityCacheEntry)}

type identityCacheEntry struct {
	expiresAt time.Time
	userID    string
	email     string
	ok        bool
}

const (
	// Positive TTL: on the higher end because a valid session is
	// stable until logout. 30s means logout or role change takes at
	// most 30s to propagate.
	sessionCacheTTLOK = 30 * time.Second

	// Negative TTL: shorter, because a transient CP 502 (see
	// controlplane issue #157 — terms-status flake) must heal
	// quickly. 5s still absorbs a burst of retries from a single
	// page render without fanning out to CP.
	sessionCacheTTLFail = 5 * time.Second

	// Cap on cached entries. 10k × ~100 bytes = ~1 MB — enough
	// headroom for realistic tenant traffic without a slow leak.
	sessionCacheMax = 10_000

	// Sweeper runs opportunistically; cost is O(N) per sweep.
	sessionCacheSweepEvery = 2 * time.Minute

	// identityCache* mirror sessionCache but for /cp/auth/me results.
	identityCacheTTLOK     = 30 * time.Second
	identityCacheTTLFail   = 5 * time.Second
	identityCacheMax       = 10_000
	identityCacheSweepEvery = 2 * time.Minute
)

type sessionCacheEntry struct {
	expiresAt time.Time
	ok        bool
	userID    string
}

// cacheKey derives the lookup key. Using sha256 here isn't about
// cryptographic secrecy — it's about keying by (tenant, cookie) in a
// fixed-size string and not sprinkling raw tokens around the map.
func cacheKey(slug, cookie string) string {
	h := sha256.New()
	h.Write([]byte(slug))
	h.Write([]byte{0}) // separator so ("a","bc") ≠ ("ab","c")
	h.Write([]byte(cookie))
	return hex.EncodeToString(h.Sum(nil))
}

// sessionCacheGet returns (ok, userID, hit). hit=false means expired or absent.
func sessionCacheGet(key string) (ok bool, userID string, hit bool) {
	sessionCache.Lock()
	defer sessionCache.Unlock()
	e, present := sessionCache.entries[key]
	if !present {
		return false, "", false
	}
	if time.Now().After(e.expiresAt) {
		delete(sessionCache.entries, key)
		return false, "", false
	}
	return e.ok, e.userID, true
}

// sessionCachePut stores the result with the appropriate TTL. On
// overflow it evicts a pseudo-random entry so the cache stays
// bounded. This isn't LRU — we don't need precise recency, just
// ceiling behaviour. Random eviction is O(1) expected and avoids
// the bookkeeping of a doubly-linked list.
func sessionCachePut(key string, ok bool, userID string) {
	ttl := sessionCacheTTLFail
	if ok {
		ttl = sessionCacheTTLOK
	}
	sessionCache.Lock()
	defer sessionCache.Unlock()
	if len(sessionCache.entries) >= sessionCacheMax {
		// Evict N random entries to amortize the sweep cost. Pick
		// the first N in map-iteration order (Go randomizes this).
		const evictBatch = 128
		i := 0
		for k := range sessionCache.entries {
			delete(sessionCache.entries, k)
			i++
			if i >= evictBatch {
				break
			}
		}
	}
	sessionCache.entries[key] = sessionCacheEntry{
		expiresAt: time.Now().Add(ttl),
		ok:        ok,
		userID:    userID,
	}
}

func init() {
	go func() {
		defer recoverPanic("session_auth: cache sweeper")
		// Jitter startup so restarts don't align sweeps.
		time.Sleep(time.Duration(rand.Int64N(int64(sessionCacheSweepEvery))))
		t := time.NewTicker(sessionCacheSweepEvery)
		defer t.Stop()
		for range t.C {
			sweepExpired()
			sweepExpiredIdentity()
		}
	}()
}

// recoverPanic is the canonical panic-recovery wrapper for the
// long-lived background goroutines the workspace-server middleware
// package runs (cache sweepers, rate-limit cleanup, session cache,
// A2A SSE idle watcher, terminal bridges, importer provision
// goroutine — see core#2125). It is invoked via `defer` at the
// TOP of any goroutine that must not let a panic crash the process
// (the package has no global recover; a panic that escapes a
// goroutine takes the whole workspace-server down).
//
// The wrapper logs the recovered value with a caller-supplied
// prefix so the operator can grep for the specific goroutine that
// tripped, then returns. The deferred caller continues to its
// function epilogue (typically `close(done)`).
//
// Refactored from inline `defer func() { if r := recover(); ... }()`
// blocks (which were duplicated across 6 call sites in #2125) so
// the panic-recovery contract is testable in one place — see
// TestRecoverPanic_RecoversFromPanicInGoroutine in panic_recovery_test.go.
func recoverPanic(prefix string) {
	if r := recover(); r != nil {
		log.Printf("%s: PANIC: %v", prefix, r)
	}
}

// sweepExpired removes expired entries so a low-hit-rate cache still
// releases memory. Cheap — we hold the lock briefly per entry.
func sweepExpired() {
	now := time.Now()
	sessionCache.Lock()
	defer sessionCache.Unlock()
	for k, e := range sessionCache.entries {
		if now.After(e.expiresAt) {
			delete(sessionCache.entries, k)
		}
	}
}

func sweepExpiredIdentity() {
	now := time.Now()
	identityCache.Lock()
	defer identityCache.Unlock()
	for k, e := range identityCache.entries {
		if now.After(e.expiresAt) {
			delete(identityCache.entries, k)
		}
	}
}

// cpSessionVerifyURL builds the upstream /cp/auth/tenant-member URL
// with the tenant slug attached. Returns "" when the tenant isn't
// configured for CP verification (CP_UPSTREAM_URL unset).
func cpSessionVerifyURL(slug string) string {
	base := strings.TrimRight(os.Getenv("CP_UPSTREAM_URL"), "/")
	if base == "" {
		return ""
	}
	return base + "/cp/auth/tenant-member?slug=" + url.QueryEscape(slug)
}

// tenantSlug returns the slug this platform represents. Pulled from
// the MOLECULE_ORG_SLUG env at provision time; falls back to empty
// when unset (self-hosted / dev).
func tenantSlug() string {
	return strings.TrimSpace(os.Getenv("MOLECULE_ORG_SLUG"))
}

// VerifiedCPSession returns true when the request carries a cookie
// that the CP confirms belongs to a MEMBER of THIS tenant's org (not
// just "someone is logged in"). The difference is the authz boundary:
// any WorkOS-authed user could hit /cp/auth/me successfully; only
// actual org members pass /cp/auth/tenant-member?slug=<us>.
//
// The returned userID is the WorkOS user_id when valid is true; otherwise
// it is empty. Callers that need the user_id for audit (e.g. org-token
// mint provenance) should use this value rather than making a second
// /cp/auth/me round-trip.
//
// Returns (false, "", false) when no cookie at all, so callers can
// distinguish "no credential presented" (fall through to bearer)
// from "credential presented but invalid" (abort with 401).
//
// Also returns (false, "", false) when MOLECULE_ORG_SLUG isn't configured
// — fail-safe: better to refuse session auth than to accept it
// without knowing which tenant we ARE. Deployments that want session
// auth MUST set both CP_UPSTREAM_URL and MOLECULE_ORG_SLUG.
func VerifiedCPSession(cookieHeader string) (valid bool, presented bool, userID string) {
	if cookieHeader == "" {
		return false, false, ""
	}
	slug := tenantSlug()
	if slug == "" {
		return false, false, ""
	}
	verifyURL := cpSessionVerifyURL(slug)
	if verifyURL == "" {
		return false, true, ""
	}

	key := cacheKey(slug, cookieHeader)
	if ok, uid, hit := sessionCacheGet(key); hit {
		return ok, true, uid
	}

	// Short timeout — a slow CP mustn't gate every canvas render.
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest("GET", verifyURL, nil)
	if err != nil {
		log.Printf("VerifiedCPSession: build req: %v", err)
		return false, true, ""
	}
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("User-Agent", "molecule-tenant-platform/session-verifier")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("VerifiedCPSession: upstream: %v", err)
		// NOTE: we deliberately do NOT cache transport failures.
		// Caching them would mean a 3s CP blip locks out all users
		// for the negative-TTL window. Next request retries.
		return false, true, ""
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		sessionCachePut(key, false, "")
		return false, true, ""
	}

	var body struct {
		Member bool   `json:"member"`
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		sessionCachePut(key, false, "")
		return false, true, ""
	}
	if !body.Member || body.UserID == "" {
		sessionCachePut(key, false, "")
		return false, true, ""
	}

	sessionCachePut(key, true, body.UserID)
	return true, true, body.UserID
}

// VerifiedCPIdentity resolves the authenticated user's identity from the
// control plane's /cp/auth/me endpoint. It returns (userID, email, ok).
// ok=false means the identity could not be resolved (no cookie, CP not
// configured, non-200 response, or missing fields). The caller is expected
// to label the message UNAUTHENTICATED in that case.
//
// Results are short-cached in identityCache so a burst of canvas messages
// from the same session does not hammer the CP.
func VerifiedCPIdentity(cookieHeader string) (userID, email string, ok bool) {
	if cookieHeader == "" {
		return "", "", false
	}
	slug := tenantSlug()
	if slug == "" {
		return "", "", false
	}
	base := strings.TrimRight(os.Getenv("CP_UPSTREAM_URL"), "/")
	if base == "" {
		return "", "", false
	}

	key := cacheKey(slug, cookieHeader)
	if e, hit := identityCacheGet(key); hit {
		return e.userID, e.email, e.ok
	}

	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest("GET", base+"/cp/auth/me", nil)
	if err != nil {
		log.Printf("VerifiedCPIdentity: build req: %v", err)
		identityCachePut(key, "", "", false)
		return "", "", false
	}
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("User-Agent", "molecule-tenant-platform/session-identity")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("VerifiedCPIdentity: upstream: %v", err)
		identityCachePut(key, "", "", false)
		return "", "", false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		identityCachePut(key, "", "", false)
		return "", "", false
	}

	var body struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("VerifiedCPIdentity: decode: %v", err)
		identityCachePut(key, "", "", false)
		return "", "", false
	}
	if body.UserID == "" || body.Email == "" {
		identityCachePut(key, "", "", false)
		return "", "", false
	}

	identityCachePut(key, body.UserID, body.Email, true)
	return body.UserID, body.Email, true
}

// identityCacheGet returns the cached identity entry and a hit flag.
func identityCacheGet(key string) (identityCacheEntry, bool) {
	identityCache.Lock()
	defer identityCache.Unlock()
	e, present := identityCache.entries[key]
	if !present {
		return identityCacheEntry{}, false
	}
	if time.Now().After(e.expiresAt) {
		delete(identityCache.entries, key)
		return identityCacheEntry{}, false
	}
	return e, true
}

// identityCachePut stores the identity result with the appropriate TTL and
// keeps the cache bounded.
func identityCachePut(key, userID, email string, ok bool) {
	ttl := identityCacheTTLFail
	if ok {
		ttl = identityCacheTTLOK
	}
	identityCache.Lock()
	defer identityCache.Unlock()
	if len(identityCache.entries) >= identityCacheMax {
		const evictBatch = 128
		i := 0
		for k := range identityCache.entries {
			delete(identityCache.entries, k)
			i++
			if i >= evictBatch {
				break
			}
		}
	}
	identityCache.entries[key] = identityCacheEntry{
		expiresAt: time.Now().Add(ttl),
		userID:    userID,
		email:     email,
		ok:        ok,
	}
}
