package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- test doubles -----------------------------------------------------------

// fakeStore is an in-memory SecretStore. nil entry = absent key.
type fakeStore struct {
	mu     sync.Mutex
	values map[string]string
	getErr error
	putErr error
	puts   int32 // count of successful Put calls
}

func newFakeStore() *fakeStore { return &fakeStore{values: map[string]string{}} }

func (f *fakeStore) Get(_ context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return "", false, f.getErr
	}
	v, ok := f.values[key]
	return v, ok, nil
}

func (f *fakeStore) Put(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.values[key] = value
	atomic.AddInt32(&f.puts, 1)
	return nil
}

func (f *fakeStore) get(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.values[key]
}

// fakeTransport records every request and returns a scripted response. It is
// the network seam — tests NEVER make a real request.
type fakeTransport struct {
	mu        sync.Mutex
	calls     int32
	urls      []string
	methods   []string
	bodies    []string
	status    int
	respBody  string
	transport func(*http.Request) (*http.Response, error) // optional override
}

func (t *fakeTransport) Do(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&t.calls, 1)
	t.mu.Lock()
	t.urls = append(t.urls, req.URL.String())
	t.methods = append(t.methods, req.Method)
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		t.bodies = append(t.bodies, string(b))
	} else {
		t.bodies = append(t.bodies, "")
	}
	t.mu.Unlock()

	if t.transport != nil {
		return t.transport(req)
	}
	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(t.respBody)),
		Header:     make(http.Header),
	}, nil
}

func (t *fakeTransport) callCount() int { return int(atomic.LoadInt32(&t.calls)) }

// --- helpers ----------------------------------------------------------------

// makeJWT builds an unsigned-but-parseable JWT whose payload carries exp.
func makeJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		fmt.Sprintf(`{"exp":%d,"sub":"codex"}`, exp.Unix())))
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + payload + "." + sig
}

// authBlob builds a nested codex auth.json blob with the given tokens.
func authBlob(access, refresh string) string {
	b, _ := json.Marshal(map[string]any{
		"tokens": map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"id_token":      "id-original",
		},
		"OPENAI_API_KEY": nil,
		"last_refresh":   "2026-01-01T00:00:00Z",
	})
	return string(b)
}

func newTestRefresher(store SecretStore, client httpDoer, now time.Time) *refresher {
	return &refresher{
		store:  store,
		client: client,
		now:    func() time.Time { return now },
	}
}

func okRefreshResponse(access, refresh string) string {
	b, _ := json.Marshal(oauthTokens{AccessToken: access, RefreshToken: refresh, IDToken: "id-new"})
	return string(b)
}

// --- tests ------------------------------------------------------------------

// TestJWTExpParse covers the exp decode (valid, malformed, missing).
func TestJWTExpParse(t *testing.T) {
	want := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	got, ok := jwtExp(makeJWT(want))
	if !ok {
		t.Fatalf("jwtExp(valid) ok=false, want true")
	}
	if !got.Equal(want) {
		t.Errorf("jwtExp = %v, want %v", got, want)
	}

	if _, ok := jwtExp("not-a-jwt"); ok {
		t.Errorf("jwtExp(non-jwt) ok=true, want false")
	}
	if _, ok := jwtExp("a.b.c"); ok {
		t.Errorf("jwtExp(garbage parts) ok=true, want false")
	}
	// 3 parts but payload has no exp.
	noExp := base64.RawURLEncoding.EncodeToString([]byte("{}"))
	if _, ok := jwtExp("h." + noExp + ".s"); ok {
		t.Errorf("jwtExp(no exp claim) ok=true, want false")
	}
}

// TestRefreshOnce_SkipWhenFresh: a token well outside the safety margin is NOT
// refreshed — no POST, no write-back.
func TestRefreshOnce_SkipWhenFresh(t *testing.T) {
	now := time.Now()
	store := newFakeStore()
	store.values[CodexAuthSecretKey] = authBlob(makeJWT(now.Add(2*time.Hour)), "rt-1")
	tr := &fakeTransport{status: http.StatusOK, respBody: okRefreshResponse("new-at", "rt-2")}
	r := newTestRefresher(store, tr, now)

	if perm := r.refreshOnce(context.Background()); perm {
		t.Fatalf("fresh token: permanentFailure=true, want false")
	}
	if tr.callCount() != 0 {
		t.Errorf("fresh token: %d OAuth POSTs, want 0", tr.callCount())
	}
	if atomic.LoadInt32(&store.puts) != 0 {
		t.Errorf("fresh token: %d write-backs, want 0", store.puts)
	}
}

// TestRefreshOnce_RotateThenReskip: a token inside the margin is refreshed once
// (POST + write-back of the rotated blob); a subsequent call on the now-fresh
// rotated token skips (no second POST). Proves rotate→write-back→re-skip.
func TestRefreshOnce_RotateThenReskip(t *testing.T) {
	now := time.Now()
	store := newFakeStore()
	// Expires in 5m — inside the 15m safety margin → DUE.
	store.values[CodexAuthSecretKey] = authBlob(makeJWT(now.Add(5*time.Minute)), "rt-1")
	// Rotated access token is fresh (2h out); rotated refresh is rt-2.
	tr := &fakeTransport{status: http.StatusOK, respBody: okRefreshResponse(makeJWT(now.Add(2*time.Hour)), "rt-2")}
	r := newTestRefresher(store, tr, now)

	if perm := r.refreshOnce(context.Background()); perm {
		t.Fatalf("due token: permanentFailure=true, want false")
	}
	if tr.callCount() != 1 {
		t.Fatalf("due token: %d OAuth POSTs, want exactly 1", tr.callCount())
	}
	if atomic.LoadInt32(&store.puts) != 1 {
		t.Fatalf("due token: %d write-backs, want exactly 1", store.puts)
	}

	// The written blob must carry the rotated refresh_token and preserve the
	// non-token field.
	rotated := store.get(CodexAuthSecretKey)
	tokens, err := parseTokens(rotated)
	if err != nil {
		t.Fatalf("parse rotated blob: %v", err)
	}
	if tokens.RefreshToken != "rt-2" {
		t.Errorf("rotated refresh_token = %q, want rt-2", tokens.RefreshToken)
	}
	if !strings.Contains(rotated, "last_refresh") {
		t.Errorf("rotated blob dropped the preserved last_refresh field: %s", rotated)
	}

	// Second call: the rotated access token is fresh → skip, no new POST.
	if perm := r.refreshOnce(context.Background()); perm {
		t.Fatalf("re-skip: permanentFailure=true, want false")
	}
	if tr.callCount() != 1 {
		t.Errorf("re-skip: %d total OAuth POSTs, want still 1", tr.callCount())
	}
	if atomic.LoadInt32(&store.puts) != 1 {
		t.Errorf("re-skip: %d total write-backs, want still 1", store.puts)
	}
}

// TestRefreshOnce_NoSecretInert: absent CODEX_AUTH_JSON → inert (no POST, no
// write-back, no error/permanent).
func TestRefreshOnce_NoSecretInert(t *testing.T) {
	store := newFakeStore() // empty
	tr := &fakeTransport{}
	r := newTestRefresher(store, tr, time.Now())

	if perm := r.refreshOnce(context.Background()); perm {
		t.Fatalf("no secret: permanentFailure=true, want false")
	}
	if tr.callCount() != 0 {
		t.Errorf("no secret: %d POSTs, want 0", tr.callCount())
	}
	if atomic.LoadInt32(&store.puts) != 0 {
		t.Errorf("no secret: %d write-backs, want 0", store.puts)
	}
}

// TestRefreshOnce_PermanentFailNoWriteNoStorm: a 400 invalid_grant must (a) not
// write back, (b) return permanentFailure=true, and (c) NOT re-POST on the next
// cycle for the same (burned) seed — the anti-storm latch.
func TestRefreshOnce_PermanentFailNoWriteNoStorm(t *testing.T) {
	now := time.Now()
	store := newFakeStore()
	store.values[CodexAuthSecretKey] = authBlob(makeJWT(now.Add(1*time.Minute)), "rt-burned")
	tr := &fakeTransport{
		status:   http.StatusBadRequest,
		respBody: `{"error":"invalid_grant","error_description":"refresh token already used"}`,
	}
	r := newTestRefresher(store, tr, now)

	perm := r.refreshOnce(context.Background())
	if !perm {
		t.Fatalf("invalid_grant: permanentFailure=false, want true")
	}
	if tr.callCount() != 1 {
		t.Fatalf("invalid_grant: %d POSTs, want exactly 1", tr.callCount())
	}
	if atomic.LoadInt32(&store.puts) != 0 {
		t.Fatalf("invalid_grant: %d write-backs, want 0 (must NOT persist a failed refresh)", store.puts)
	}

	// Next cycle, SAME burned seed: must NOT re-POST (anti-storm latch).
	perm2 := r.refreshOnce(context.Background())
	if tr.callCount() != 1 {
		t.Errorf("anti-storm: re-POSTed a burned refresh_token (%d total POSTs, want still 1)", tr.callCount())
	}
	_ = perm2 // latched cycle returns false (already-known failure, nothing new)

	// A RE-SEED (blob changes) clears the latch and allows a fresh attempt.
	store.mu.Lock()
	store.values[CodexAuthSecretKey] = authBlob(makeJWT(now.Add(1*time.Minute)), "rt-freshly-seeded")
	store.mu.Unlock()
	tr.status = http.StatusOK
	tr.respBody = okRefreshResponse(makeJWT(now.Add(2*time.Hour)), "rt-rotated")
	if perm := r.refreshOnce(context.Background()); perm {
		t.Fatalf("post-reseed: permanentFailure=true, want false")
	}
	if tr.callCount() != 2 {
		t.Errorf("post-reseed: %d total POSTs, want 2 (latch should clear on re-seed)", tr.callCount())
	}
}

// TestRefreshOnce_TransientNoWriteNoLatch: a 5xx is transient — no write-back,
// returns false (no hard backoff latch), and a later cycle retries.
func TestRefreshOnce_TransientNoWriteNoLatch(t *testing.T) {
	now := time.Now()
	store := newFakeStore()
	store.values[CodexAuthSecretKey] = authBlob(makeJWT(now.Add(1*time.Minute)), "rt-1")
	tr := &fakeTransport{status: http.StatusServiceUnavailable, respBody: "upstream down"}
	r := newTestRefresher(store, tr, now)

	if perm := r.refreshOnce(context.Background()); perm {
		t.Fatalf("503: permanentFailure=true, want false (transient)")
	}
	if atomic.LoadInt32(&store.puts) != 0 {
		t.Errorf("503: %d write-backs, want 0", store.puts)
	}
	// Retry next cycle succeeds (no latch on transient).
	tr.status = http.StatusOK
	tr.respBody = okRefreshResponse(makeJWT(now.Add(2*time.Hour)), "rt-2")
	if perm := r.refreshOnce(context.Background()); perm {
		t.Fatalf("retry after 503: permanentFailure=true, want false")
	}
	if tr.callCount() != 2 {
		t.Errorf("transient retry: %d total POSTs, want 2", tr.callCount())
	}
	if atomic.LoadInt32(&store.puts) != 1 {
		t.Errorf("transient retry: %d write-backs, want 1", store.puts)
	}
}

// TestRefreshOnce_SingleFlight: concurrent refreshOnce calls on a DUE token must
// POST exactly once total — the package mutex serializes them and the second
// sees the freshly-rotated (now-fresh) token and skips. Structural single-flight.
func TestRefreshOnce_SingleFlight(t *testing.T) {
	now := time.Now()
	store := newFakeStore()
	store.values[CodexAuthSecretKey] = authBlob(makeJWT(now.Add(1*time.Minute)), "rt-1")
	// Every successful rotation yields a FRESH (2h) access token, so once one
	// caller rotates, the other sees fresh and skips.
	tr := &fakeTransport{status: http.StatusOK, respBody: okRefreshResponse(makeJWT(now.Add(2*time.Hour)), "rt-2")}
	r := newTestRefresher(store, tr, now)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			r.refreshOnce(context.Background())
		}()
	}
	wg.Wait()

	if tr.callCount() != 1 {
		t.Errorf("single-flight: %d OAuth POSTs across %d concurrent calls, want exactly 1", tr.callCount(), n)
	}
	if atomic.LoadInt32(&store.puts) != 1 {
		t.Errorf("single-flight: %d write-backs, want exactly 1", store.puts)
	}
}

// TestRefreshOnce_PostsExactlyOnceToOAuthEndpoint: when it DOES refresh, the
// single POST goes to the OAuth token URL with the refresh_token grant body.
func TestRefreshOnce_PostsExactlyOnceToOAuthEndpoint(t *testing.T) {
	now := time.Now()
	store := newFakeStore()
	store.values[CodexAuthSecretKey] = authBlob(makeJWT(now.Add(1*time.Minute)), "rt-secret")
	tr := &fakeTransport{status: http.StatusOK, respBody: okRefreshResponse(makeJWT(now.Add(2*time.Hour)), "rt-2")}
	r := newTestRefresher(store, tr, now)

	r.refreshOnce(context.Background())

	if tr.callCount() != 1 {
		t.Fatalf("%d POSTs, want exactly 1", tr.callCount())
	}
	if tr.urls[0] != oauthTokenURL {
		t.Errorf("POST URL = %q, want %q", tr.urls[0], oauthTokenURL)
	}
	if tr.methods[0] != http.MethodPost {
		t.Errorf("method = %q, want POST", tr.methods[0])
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(tr.bodies[0]), &body); err != nil {
		t.Fatalf("request body not json: %v (%s)", err, tr.bodies[0])
	}
	if body["grant_type"] != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", body["grant_type"])
	}
	if body["refresh_token"] != "rt-secret" {
		t.Errorf("refresh_token = %q, want rt-secret", body["refresh_token"])
	}
	if body["client_id"] != codexOAuthClientID {
		t.Errorf("client_id = %q, want %q", body["client_id"], codexOAuthClientID)
	}
}

// TestRefreshOnce_ReadErrorSkips: a store read error is a transient skip (no
// POST, no permanent latch).
func TestRefreshOnce_ReadErrorSkips(t *testing.T) {
	store := newFakeStore()
	store.getErr = fmt.Errorf("db down")
	tr := &fakeTransport{}
	r := newTestRefresher(store, tr, time.Now())
	if perm := r.refreshOnce(context.Background()); perm {
		t.Errorf("read error: permanentFailure=true, want false")
	}
	if tr.callCount() != 0 {
		t.Errorf("read error: %d POSTs, want 0", tr.callCount())
	}
}

// TestMergeTokens_PreservesOtherFields proves the rotated write-back keeps every
// non-token field and does not clobber id_token with an empty rotated value.
func TestMergeTokens_PreservesOtherFields(t *testing.T) {
	blob := authBlob("old-at", "old-rt")
	out, err := mergeTokens(blob, oauthTokens{AccessToken: "new-at", RefreshToken: "new-rt"}) // no id_token
	if err != nil {
		t.Fatalf("mergeTokens: %v", err)
	}
	tokens, err := parseTokens(out)
	if err != nil {
		t.Fatalf("parse merged: %v", err)
	}
	if tokens.AccessToken != "new-at" || tokens.RefreshToken != "new-rt" {
		t.Errorf("merged tokens = %+v, want new-at/new-rt", tokens)
	}
	if tokens.IDToken != "id-original" {
		t.Errorf("empty rotated id_token clobbered the original: got %q, want id-original", tokens.IDToken)
	}
	if !strings.Contains(out, "last_refresh") {
		t.Errorf("merge dropped preserved field: %s", out)
	}
}
