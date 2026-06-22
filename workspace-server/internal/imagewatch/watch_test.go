package imagewatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/handlers"
)

// fakeRefresher records every Refresh call and lets tests inject errors.
type fakeRefresher struct {
	mu    sync.Mutex
	calls [][]string
	err   error
}

func (f *fakeRefresher) Refresh(_ context.Context, runtimes []string, _ bool) (handlers.RefreshResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), runtimes...))
	if f.err != nil {
		return handlers.RefreshResult{}, f.err
	}
	return handlers.RefreshResult{Pulled: runtimes}, nil
}

func (f *fakeRefresher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newTestWatcher(svc Refresher, runtimes ...string) *Watcher {
	return &Watcher{
		svc:      svc,
		runtimes: runtimes,
		seen:     make(map[string]string),
	}
}

// staticFetcher returns a fixed digest for every call. mutableFetcher lets
// tests change the returned digest between ticks.
func staticFetcher(digest string) digestFetcher {
	return func(_ context.Context, _ string) (string, error) {
		return digest, nil
	}
}

func TestTick_FirstObservationSeedsWithoutRefresh(t *testing.T) {
	svc := &fakeRefresher{}
	w := newTestWatcher(svc, "claude-code")

	w.tick(context.Background(), staticFetcher("sha256:aaaa"))

	if svc.callCount() != 0 {
		t.Errorf("first tick must seed only, got %d Refresh calls", svc.callCount())
	}
	if w.seen["claude-code"] != "sha256:aaaa" {
		t.Errorf("seen digest not recorded: got %q", w.seen["claude-code"])
	}
}

func TestTick_NoRefreshWhenDigestUnchanged(t *testing.T) {
	svc := &fakeRefresher{}
	w := newTestWatcher(svc, "claude-code")

	fetch := staticFetcher("sha256:steady")
	w.tick(context.Background(), fetch) // seed
	w.tick(context.Background(), fetch) // unchanged
	w.tick(context.Background(), fetch) // unchanged

	if svc.callCount() != 0 {
		t.Errorf("steady-state ticks must not refresh, got %d calls", svc.callCount())
	}
}

func TestTick_RefreshFiresWhenDigestChanges(t *testing.T) {
	svc := &fakeRefresher{}
	w := newTestWatcher(svc, "claude-code", "hermes")

	w.tick(context.Background(), staticFetcher("sha256:v1")) // seed both
	if svc.callCount() != 0 {
		t.Fatalf("seed tick should not refresh; got %d", svc.callCount())
	}

	// Only claude-code's digest moves. hermes stays.
	moveOne := func(_ context.Context, rt string) (string, error) {
		if rt == "claude-code" {
			return "sha256:v2", nil
		}
		return "sha256:v1", nil
	}
	w.tick(context.Background(), moveOne)

	if svc.callCount() != 1 {
		t.Fatalf("expected exactly 1 Refresh call (only claude-code moved), got %d", svc.callCount())
	}
	if got := svc.calls[0]; len(got) != 1 || got[0] != "claude-code" {
		t.Errorf("Refresh called with wrong runtime: got %v, want [claude-code]", got)
	}
	if w.seen["claude-code"] != "sha256:v2" {
		t.Errorf("post-refresh seen digest should advance: got %q", w.seen["claude-code"])
	}
}

func TestTick_RollsBackSeenDigestOnRefreshError(t *testing.T) {
	// Critical safety property: a transient Docker glitch during Refresh
	// must not convince the watcher the work is done. Next tick should
	// retry against the same upstream digest.
	svc := &fakeRefresher{err: errors.New("docker daemon unreachable")}
	w := newTestWatcher(svc, "claude-code")

	w.tick(context.Background(), staticFetcher("sha256:old")) // seed
	w.tick(context.Background(), staticFetcher("sha256:new")) // change → fails

	if got := w.seen["claude-code"]; got != "sha256:old" {
		t.Errorf("after Refresh error, seen must roll back to %q (so next tick retries), got %q", "sha256:old", got)
	}
	if svc.callCount() != 1 {
		t.Fatalf("expected 1 Refresh attempt (the failed one), got %d", svc.callCount())
	}

	// Recovery: clear the error, run again with same upstream digest.
	// Watcher should retry because seen was rolled back.
	svc.err = nil
	w.tick(context.Background(), staticFetcher("sha256:new"))
	if svc.callCount() != 2 {
		t.Errorf("after rollback, next tick should retry refresh; got %d total calls", svc.callCount())
	}
	if got := w.seen["claude-code"]; got != "sha256:new" {
		t.Errorf("after successful retry, seen should advance: got %q", got)
	}
}

func TestTick_DigestFetchErrorSkipsRuntime(t *testing.T) {
	// One runtime's GHCR call failing must not block other runtimes from
	// being checked (e.g. one template repo briefly 500s).
	svc := &fakeRefresher{}
	w := newTestWatcher(svc, "claude-code", "hermes")
	w.seen["claude-code"] = "sha256:old"
	w.seen["hermes"] = "sha256:old"

	flaky := func(_ context.Context, rt string) (string, error) {
		if rt == "claude-code" {
			return "", errors.New("registry hiccup")
		}
		return "sha256:new", nil
	}
	w.tick(context.Background(), flaky)

	// hermes moved → 1 refresh fired.
	if svc.callCount() != 1 || svc.calls[0][0] != "hermes" {
		t.Errorf("expected hermes-only refresh after claude-code fetch error, got calls=%v", svc.calls)
	}
	// claude-code's seen digest must not be touched (no remote observed).
	if got := w.seen["claude-code"]; got != "sha256:old" {
		t.Errorf("fetch error must leave seen digest untouched, got %q", got)
	}
}

// TestRemoteDigest_RegistryHostFollowsEnv pins the RFC #229 fix: with
// MOLECULE_IMAGE_REGISTRY pointed at a private mirror, the watcher's HTTP
// calls (token endpoint + manifest HEAD) must hit that mirror's host, not
// the hardcoded ghcr.io of the pre-fix code path. We stand up an httptest
// server, point MOLECULE_IMAGE_REGISTRY at its host, and assert both
// endpoints get hit on it.
//
// Without this test, a future refactor could revert the helper indirection
// and the watcher would silently go back to talking to ghcr.io even when
// the platform is configured for ECR — exactly the bug RFC #229 is closing.
func TestRemoteDigest_RegistryHostFollowsEnv(t *testing.T) {
	var (
		mu              sync.Mutex
		tokenHits       int
		manifestHits    int
		lastTokenURL    string
		lastManifestURL string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			tokenHits++
			lastTokenURL = r.URL.String()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"fake-bearer"}`))
		case strings.HasPrefix(r.URL.Path, "/v2/") && strings.Contains(r.URL.Path, "/manifests/latest"):
			manifestHits++
			lastManifestURL = r.URL.Path
			w.Header().Set("Docker-Content-Digest", "sha256:cafef00d")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// httptest.Server.URL is "http://127.0.0.1:NNNN". RegistryHost() works
	// over the host:port portion (provisioner.RegistryPrefix takes the env
	// verbatim), so we strip the scheme and append "/molecule-ai" to mimic
	// the prefix shape MOLECULE_IMAGE_REGISTRY actually uses in production.
	host := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv("MOLECULE_IMAGE_REGISTRY", host+"/molecule-ai")

	w := newTestWatcher(&fakeRefresher{}, "claude-code")
	// Use the test-server URL scheme by overriding the http client only —
	// remoteDigest constructs https://<host>/... internally. We need the
	// watcher to hit our http server, so swap the URL scheme by injecting
	// a transport that rewrites https→http for this test.
	w.http = &http.Client{Transport: rewriteToHTTP{}}

	digest, err := w.remoteDigest(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("remoteDigest failed: %v", err)
	}
	if digest != "sha256:cafef00d" {
		t.Errorf("digest: got %q, want sha256:cafef00d", digest)
	}

	mu.Lock()
	defer mu.Unlock()
	if tokenHits != 1 {
		t.Errorf("token endpoint hits: got %d, want 1 (watcher must hit configured registry, not ghcr.io)", tokenHits)
	}
	if manifestHits != 1 {
		t.Errorf("manifest HEAD hits: got %d, want 1 (watcher must hit configured registry, not ghcr.io)", manifestHits)
	}
	// service= query param must reflect the configured host so registries
	// that validate the param (GHCR-style spec) accept the request.
	if !strings.Contains(lastTokenURL, "service="+host) && !strings.Contains(lastTokenURL, "service=127.0.0.1") {
		t.Errorf("token URL service param not host-derived: got %q", lastTokenURL)
	}
	wantManifestPath := "/v2/molecule-ai/workspace-template-claude-code/manifests/latest"
	if lastManifestURL != wantManifestPath {
		t.Errorf("manifest path: got %q, want %q", lastManifestURL, wantManifestPath)
	}
}

// rewriteToHTTP is a tiny RoundTripper that flips https→http so the watcher
// (which builds https URLs from the configured registry host) can target an
// httptest.Server that only speaks http. Production code paths still go
// over https; this is a unit-test seam only.
type rewriteToHTTP struct{}

func (rewriteToHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = "http"
		req = clone
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestShortDigest(t *testing.T) {
	cases := map[string]string{
		"sha256:abcdef0123456789":     "sha256:abcdef012345",
		"sha256:short":                "sha256:short",
		"":                            "",
		"no-colon-format":             "no-colon-format",
		"sha256:0000000000000000abcd": "sha256:000000000000",
	}
	for in, want := range cases {
		if got := shortDigest(in); got != want {
			t.Errorf("shortDigest(%q): got %q, want %q", in, got, want)
		}
	}
}
