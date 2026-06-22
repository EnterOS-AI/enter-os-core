package db

// redis_test.go — regression coverage for the workspace online-status and
// URL-resolution Redis layer (redis.go), which previously had NO test.
//
// Issue #2150 (SOP rule internal#765). redis.go drives two fleet-wide
// behaviours that break silently if a key name or TTL drifts:
//
//   - online detection: SetOnline / RefreshTTL / IsOnline on `ws:<id>`.
//     A wrong key prefix or a TTL shorter than the heartbeat interval makes
//     live workspaces flap to "unreachable — restart" (the exact failure
//     LivenessTTL=180s was tuned to avoid). A TTL too long hides real
//     crashes.
//   - proxy URL resolution: CacheURL / GetCachedURL / CacheInternalURL /
//     GetCachedInternalURL on `ws:<id>:url` and `ws:<id>:internal_url`.
//     A2A forwarding resolves the target workspace through these keys; a
//     prefix collision (e.g. the liveness key overlapping the URL key)
//     would serve the wrong URL or a literal "online" string as a URL.
//
// These tests run against miniredis — an in-process Redis that speaks the
// real RESP protocol and enforces real TTL/expiry semantics — so they
// exercise the actual go-redis client calls and key/TTL behaviour, not a
// mock that rubber-stamps them. miniredis is already a module dependency.
//
// Watch-fail intent: change any `ws:%s...` format string in redis.go, or
// regress LivenessTTL below the heartbeat window, and a test here fails.

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// withMiniRedis spins up an in-process Redis, points the package-global RDB
// at it, and registers Cleanup. Returns the server handle so tests can drive
// the clock (FastForward) to exercise TTL expiry deterministically.
func withMiniRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	RDB = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		RDB.Close()
		mr.Close()
	})
	return mr
}

// TestLivenessTTL_ExceedsHeartbeatWindow pins the tuned TTL. The heartbeat
// loop fires every 30s; LivenessTTL must allow several missed beats (the
// comment in redis.go targets ~5) so a busy leader starved for 60-120s is
// not falsely declared dead. 180s = 6×30s. Regressing this toward the old
// 60s value reintroduces the false-positive restart cycle.
func TestLivenessTTL_ExceedsHeartbeatWindow(t *testing.T) {
	const heartbeatInterval = 30 * time.Second
	const minMissedBeats = 5
	if LivenessTTL < heartbeatInterval*minMissedBeats {
		t.Errorf("LivenessTTL=%s is too short: must tolerate >=%d missed %s heartbeats (>= %s) to avoid false-positive restarts",
			LivenessTTL, minMissedBeats, heartbeatInterval, heartbeatInterval*minMissedBeats)
	}
}

// TestSetOnline_KeyAndTTL verifies SetOnline writes the canonical `ws:<id>`
// key with the value "online" and the LivenessTTL — the exact contract
// IsOnline and the a2a_proxy reactive check rely on.
func TestSetOnline_KeyAndTTL(t *testing.T) {
	mr := withMiniRedis(t)
	ctx := context.Background()
	const ws = "ws-abc-123"

	if err := SetOnline(ctx, ws); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}

	// Key name must be exactly ws:<id> — not, say, ws:<id>:online.
	if !mr.Exists("ws:" + ws) {
		t.Fatalf("expected key %q to exist; keys present: %v", "ws:"+ws, mr.Keys())
	}
	got, err := mr.Get("ws:" + ws)
	if err != nil {
		t.Fatalf("mr.Get: %v", err)
	}
	if got != "online" {
		t.Errorf("liveness value = %q, want %q", got, "online")
	}

	// TTL must be the tuned LivenessTTL (allow miniredis's whole-second
	// granularity).
	ttl := mr.TTL("ws:" + ws)
	if ttl != LivenessTTL {
		t.Errorf("TTL = %s, want %s", ttl, LivenessTTL)
	}
}

// TestIsOnline_TrueThenExpires drives the real TTL clock: a freshly-set
// workspace is online; after the TTL elapses it is offline. This is the
// behaviour online-detection depends on — proven against real expiry, not
// asserted from a mock.
func TestIsOnline_TrueThenExpires(t *testing.T) {
	mr := withMiniRedis(t)
	ctx := context.Background()
	const ws = "ws-expiry"

	if err := SetOnline(ctx, ws); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	online, err := IsOnline(ctx, ws)
	if err != nil {
		t.Fatalf("IsOnline: %v", err)
	}
	if !online {
		t.Fatal("expected workspace online immediately after SetOnline")
	}

	// Fast-forward just past the TTL; the liveness key must expire.
	mr.FastForward(LivenessTTL + time.Second)

	online, err = IsOnline(ctx, ws)
	if err != nil {
		t.Fatalf("IsOnline after expiry: %v", err)
	}
	if online {
		t.Error("expected workspace offline after TTL elapsed")
	}
}

// TestRefreshTTL_ExtendsLiveness proves a heartbeat (RefreshTTL) keeps a
// workspace alive across what would otherwise be an expiry. Without the
// refresh the key expires; with it, IsOnline stays true. Watch-fail: if
// RefreshTTL targets the wrong key, the refresh is a no-op and this fails.
func TestRefreshTTL_ExtendsLiveness(t *testing.T) {
	mr := withMiniRedis(t)
	ctx := context.Background()
	const ws = "ws-refresh"

	if err := SetOnline(ctx, ws); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	// Advance most of the way to expiry, then heartbeat.
	mr.FastForward(LivenessTTL - 5*time.Second)
	if err := RefreshTTL(ctx, ws); err != nil {
		t.Fatalf("RefreshTTL: %v", err)
	}
	// Advance past where the ORIGINAL TTL would have expired. Still online.
	mr.FastForward(10 * time.Second)
	online, err := IsOnline(ctx, ws)
	if err != nil {
		t.Fatalf("IsOnline: %v", err)
	}
	if !online {
		t.Error("expected workspace still online after RefreshTTL heartbeat")
	}
}

// TestIsOnline_UnknownWorkspace returns false (and no error) for a workspace
// that was never set — the default for a never-registered / long-dead agent.
func TestIsOnline_UnknownWorkspace(t *testing.T) {
	withMiniRedis(t)
	ctx := context.Background()
	online, err := IsOnline(ctx, "never-seen")
	if err != nil {
		t.Fatalf("IsOnline: %v", err)
	}
	if online {
		t.Error("expected unknown workspace to be offline")
	}
}

// TestURLCache_RoundTrip pins the `ws:<id>:url` key and its 5-minute TTL,
// and proves the value round-trips. A2A push resolves the target through
// this key.
func TestURLCache_RoundTrip(t *testing.T) {
	mr := withMiniRedis(t)
	ctx := context.Background()
	const ws = "ws-url"
	const url = "https://ws-url.workspaces.moleculesai.app"

	if err := CacheURL(ctx, ws, url); err != nil {
		t.Fatalf("CacheURL: %v", err)
	}
	got, err := GetCachedURL(ctx, ws)
	if err != nil {
		t.Fatalf("GetCachedURL: %v", err)
	}
	if got != url {
		t.Errorf("GetCachedURL = %q, want %q", got, url)
	}
	if !mr.Exists("ws:" + ws + ":url") {
		t.Errorf("expected key %q; present: %v", "ws:"+ws+":url", mr.Keys())
	}
	if ttl := mr.TTL("ws:" + ws + ":url"); ttl != 5*time.Minute {
		t.Errorf("url cache TTL = %s, want 5m", ttl)
	}
}

// TestInternalURLCache_RoundTrip pins the `ws:<id>:internal_url` key (the
// Docker-internal address used for workspace-to-workspace discovery) and its
// 5-minute TTL.
func TestInternalURLCache_RoundTrip(t *testing.T) {
	mr := withMiniRedis(t)
	ctx := context.Background()
	const ws = "ws-int"
	const url = "http://ws-int:8080"

	if err := CacheInternalURL(ctx, ws, url); err != nil {
		t.Fatalf("CacheInternalURL: %v", err)
	}
	got, err := GetCachedInternalURL(ctx, ws)
	if err != nil {
		t.Fatalf("GetCachedInternalURL: %v", err)
	}
	if got != url {
		t.Errorf("GetCachedInternalURL = %q, want %q", got, url)
	}
	if ttl := mr.TTL("ws:" + ws + ":internal_url"); ttl != 5*time.Minute {
		t.Errorf("internal url cache TTL = %s, want 5m", ttl)
	}
}

// TestKeyNamespacesDoNotCollide is the prefix-collision regression: the
// liveness key (ws:<id>), the URL key (ws:<id>:url), and the internal-URL
// key (ws:<id>:internal_url) must be three DISTINCT keys for the same
// workspace. If a future edit collapses the format strings, IsOnline would
// read a URL as liveness (or vice versa) and online-detection / proxy
// resolution would corrupt each other fleet-wide.
func TestKeyNamespacesDoNotCollide(t *testing.T) {
	mr := withMiniRedis(t)
	ctx := context.Background()
	const ws = "ws-collide"

	if err := SetOnline(ctx, ws); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	if err := CacheURL(ctx, ws, "https://public"); err != nil {
		t.Fatalf("CacheURL: %v", err)
	}
	if err := CacheInternalURL(ctx, ws, "http://internal:8080"); err != nil {
		t.Fatalf("CacheInternalURL: %v", err)
	}

	// Liveness value must still be "online", NOT a URL.
	if v, _ := mr.Get("ws:" + ws); v != "online" {
		t.Errorf("liveness key clobbered by a URL write: got %q", v)
	}
	if v, _ := mr.Get("ws:" + ws + ":url"); v != "https://public" {
		t.Errorf("url key = %q, want https://public", v)
	}
	if v, _ := mr.Get("ws:" + ws + ":internal_url"); v != "http://internal:8080" {
		t.Errorf("internal_url key = %q, want http://internal:8080", v)
	}
}

// TestClearWorkspaceKeys_RemovesAllThree proves teardown removes the
// liveness, URL, and internal-URL keys together — a leaked liveness key
// after deletion would keep a dead workspace looking online; a leaked URL
// key would let the proxy forward to a recycled address.
func TestClearWorkspaceKeys_RemovesAllThree(t *testing.T) {
	mr := withMiniRedis(t)
	ctx := context.Background()
	const ws = "ws-clear"

	if err := SetOnline(ctx, ws); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	if err := CacheURL(ctx, ws, "https://x"); err != nil {
		t.Fatalf("CacheURL: %v", err)
	}
	if err := CacheInternalURL(ctx, ws, "http://x:8080"); err != nil {
		t.Fatalf("CacheInternalURL: %v", err)
	}

	ClearWorkspaceKeys(ctx, ws)

	for _, k := range []string{"ws:" + ws, "ws:" + ws + ":url", "ws:" + ws + ":internal_url"} {
		if mr.Exists(k) {
			t.Errorf("key %q survived ClearWorkspaceKeys", k)
		}
	}
	online, err := IsOnline(ctx, ws)
	if err != nil {
		t.Fatalf("IsOnline: %v", err)
	}
	if online {
		t.Error("workspace still online after ClearWorkspaceKeys")
	}
}
