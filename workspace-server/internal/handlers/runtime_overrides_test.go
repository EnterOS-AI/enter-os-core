package handlers

import (
	"sync"
	"testing"
	"time"
)

func TestRuntimeOverrideCache_SetAndGet(t *testing.T) {
	c := &runtimeOverrideCache{}

	if _, ok := c.IdleTimeout("ws-a"); ok {
		t.Fatal("empty cache should not return any override")
	}

	c.SetIdleTimeout("ws-a", 10*time.Minute)
	got, ok := c.IdleTimeout("ws-a")
	if !ok || got != 10*time.Minute {
		t.Fatalf("expected 10m override; got=%v ok=%v", got, ok)
	}

	// Sibling workspace unaffected — pin against the trap where a
	// shared map without proper keying would leak overrides across
	// workspaces (a hard-to-debug "claude-code's longer timeout
	// somehow applied to langgraph too").
	if _, ok := c.IdleTimeout("ws-b"); ok {
		t.Fatal("override for ws-a leaked to ws-b")
	}
}

func TestRuntimeOverrideCache_ZeroOrNegativeClears(t *testing.T) {
	// Adapter dropping the override (returning None / 0 from
	// idle_timeout_override) must restore platform-default behavior.
	// If the cache held the previous value indefinitely, an adapter
	// downgrade would silently keep the longer timeout active.
	c := &runtimeOverrideCache{}
	c.SetIdleTimeout("ws-a", 10*time.Minute)
	if _, ok := c.IdleTimeout("ws-a"); !ok {
		t.Fatal("setup: override should be set")
	}

	c.SetIdleTimeout("ws-a", 0)
	if _, ok := c.IdleTimeout("ws-a"); ok {
		t.Fatal("zero duration should clear override")
	}

	c.SetIdleTimeout("ws-a", 5*time.Minute)
	c.SetIdleTimeout("ws-a", -1*time.Second)
	if _, ok := c.IdleTimeout("ws-a"); ok {
		t.Fatal("negative duration should clear override")
	}
}

func TestRuntimeOverrideCache_EmptyWorkspaceIDIgnored(t *testing.T) {
	// Defensive: a misrouted heartbeat with empty workspace_id
	// should NOT pollute the cache with a "" key. workspaceID == ""
	// is also the value dispatchA2A passes when the workspace is
	// indeterminate, and that path must not surface a stored value.
	c := &runtimeOverrideCache{}
	c.SetIdleTimeout("", 10*time.Minute)
	if _, ok := c.IdleTimeout(""); ok {
		t.Fatal("empty workspace_id must not store overrides")
	}
}

func TestRuntimeOverrideCache_SetReplaces(t *testing.T) {
	// A heartbeat with a new override value replaces, doesn't append.
	c := &runtimeOverrideCache{}
	c.SetIdleTimeout("ws-a", 10*time.Minute)
	c.SetIdleTimeout("ws-a", 20*time.Minute)
	got, _ := c.IdleTimeout("ws-a")
	if got != 20*time.Minute {
		t.Fatalf("expected 20m after replacement; got %v", got)
	}
}

func TestRuntimeOverrideCache_Reset(t *testing.T) {
	c := &runtimeOverrideCache{}
	c.SetIdleTimeout("ws-a", 10*time.Minute)
	c.SetIdleTimeout("ws-b", 20*time.Minute)
	c.Reset()
	if _, ok := c.IdleTimeout("ws-a"); ok {
		t.Fatal("reset should clear ws-a")
	}
	if _, ok := c.IdleTimeout("ws-b"); ok {
		t.Fatal("reset should clear ws-b")
	}
}

func TestRuntimeOverrideCache_SetCapabilitiesAndHas(t *testing.T) {
	c := &runtimeOverrideCache{}
	if c.HasCapability("ws-a", "scheduler") {
		t.Fatal("empty cache must not return any capability")
	}

	c.SetCapabilities("ws-a", map[string]bool{"scheduler": true, "session": false})
	if !c.HasCapability("ws-a", "scheduler") {
		t.Fatal("scheduler capability not stored")
	}
	if c.HasCapability("ws-a", "session") {
		t.Fatal("session=false should report as absent (False)")
	}
	if c.HasCapability("ws-a", "heartbeat") {
		t.Fatal("missing key must report as absent")
	}
}

func TestRuntimeOverrideCache_CapabilitiesIsolatedPerWorkspace(t *testing.T) {
	// Critical: ws-a declaring native scheduler must NOT make ws-b
	// also skip its schedules. The cache's per-key isolation is the
	// only thing standing between "claude-code adapter declares this"
	// and "every workspace silently inherits the declaration."
	c := &runtimeOverrideCache{}
	c.SetCapabilities("ws-a", map[string]bool{"scheduler": true})
	if c.HasCapability("ws-b", "scheduler") {
		t.Fatal("ws-a's scheduler capability leaked to ws-b")
	}
}

func TestRuntimeOverrideCache_NilOrEmptyCapabilitiesClears(t *testing.T) {
	// An adapter that previously declared native scheduler then
	// dropped the flag (e.g. SDK update) must restore platform
	// fallback. nil + empty-map both mean "clear".
	c := &runtimeOverrideCache{}
	c.SetCapabilities("ws-a", map[string]bool{"scheduler": true})
	if !c.HasCapability("ws-a", "scheduler") {
		t.Fatal("setup: scheduler should be set")
	}

	c.SetCapabilities("ws-a", nil)
	if c.HasCapability("ws-a", "scheduler") {
		t.Fatal("nil should clear capabilities")
	}

	c.SetCapabilities("ws-a", map[string]bool{"scheduler": true})
	c.SetCapabilities("ws-a", map[string]bool{})
	if c.HasCapability("ws-a", "scheduler") {
		t.Fatal("empty map should clear capabilities")
	}
}

func TestRuntimeOverrideCache_SetCapabilitiesIsDefensiveCopy(t *testing.T) {
	// The caller's map MUST NOT alias the cached one. A future careless
	// caller mutating the original map after the call should not
	// retroactively change cached capability declarations.
	c := &runtimeOverrideCache{}
	original := map[string]bool{"scheduler": true}
	c.SetCapabilities("ws-a", original)
	original["scheduler"] = false
	if !c.HasCapability("ws-a", "scheduler") {
		t.Fatal("cache aliased the caller's map; capability flipped via outside mutation")
	}
}

func TestRuntimeOverrideCache_SetIdleTimeoutPreservesCapabilities(t *testing.T) {
	// The two heartbeat fields are independent — updating one must
	// not stomp the other. Pre-fix, each Set replaced the entire
	// entry, which meant the second-arriving Set in the heartbeat
	// handler effectively erased the first.
	c := &runtimeOverrideCache{}
	c.SetCapabilities("ws-a", map[string]bool{"scheduler": true})
	c.SetIdleTimeout("ws-a", 600*time.Second)

	if !c.HasCapability("ws-a", "scheduler") {
		t.Fatal("SetIdleTimeout erased prior capabilities")
	}
	got, ok := c.IdleTimeout("ws-a")
	if !ok || got != 600*time.Second {
		t.Fatalf("idle timeout lost; got=%v ok=%v", got, ok)
	}

	// And the inverse: SetCapabilities must not erase IdleTimeout.
	c.SetCapabilities("ws-a", map[string]bool{"scheduler": true, "session": true})
	if got, ok := c.IdleTimeout("ws-a"); !ok || got != 600*time.Second {
		t.Fatal("SetCapabilities erased prior idle timeout")
	}
}

func TestRuntimeOverrideCache_EmptyEntryDeleted(t *testing.T) {
	// When both fields are cleared, the entry should drop out of the
	// cache entirely so a stale workspace doesn't accumulate empty
	// husks indefinitely.
	c := &runtimeOverrideCache{}
	c.SetIdleTimeout("ws-a", 60*time.Second)
	c.SetCapabilities("ws-a", map[string]bool{"scheduler": true})

	c.SetIdleTimeout("ws-a", 0)
	c.SetCapabilities("ws-a", nil)

	if _, ok := c.m.Load("ws-a"); ok {
		t.Fatal("entry should be deleted when both fields cleared")
	}
}

func TestProvidesNativeScheduler_PackageLevel(t *testing.T) {
	// The package-level function the scheduler imports — pin that it
	// reads the same singleton the heartbeat handler writes to.
	runtimeOverrides.Reset()
	defer runtimeOverrides.Reset()

	if ProvidesNativeScheduler("ws-a") {
		t.Fatal("empty cache should not declare native scheduler")
	}
	runtimeOverrides.SetCapabilities("ws-a", map[string]bool{"scheduler": true})
	if !ProvidesNativeScheduler("ws-a") {
		t.Fatal("ProvidesNativeScheduler did not see the declaration")
	}
	if ProvidesNativeScheduler("") {
		t.Fatal("empty workspace ID should never declare native scheduler")
	}
}

func TestRuntimeOverrideCache_ConcurrentSafe(t *testing.T) {
	// dispatchA2A reads the cache on every request; heartbeat handlers
	// write on every 30s. Different workspaces will be hot in different
	// goroutines. The sync.Map underlying the cache promises this; the
	// test pins it so a future "let me just use a regular map with a
	// mutex" change can't silently regress under load.
	c := &runtimeOverrideCache{}
	var wg sync.WaitGroup
	const N = 100

	for i := 0; i < N; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			c.SetIdleTimeout("ws", time.Duration(i+1)*time.Second)
		}(i)
		go func() {
			defer wg.Done()
			_, _ = c.IdleTimeout("ws")
		}()
	}
	wg.Wait()
	// Final value must be SOME positive duration written by one of the
	// goroutines — not corrupted, not zero.
	got, ok := c.IdleTimeout("ws")
	if !ok || got <= 0 || got > time.Duration(N)*time.Second {
		t.Fatalf("expected a valid override after concurrent writes; got %v ok=%v", got, ok)
	}
}
