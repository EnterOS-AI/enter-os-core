package imagewatch

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/handlers"
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
