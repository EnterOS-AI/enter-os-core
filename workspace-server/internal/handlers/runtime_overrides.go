package handlers

import (
	"sync"
	"time"
)

// runtimeOverrides is the in-memory cache of per-workspace, adapter-
// declared overrides for cross-cutting capabilities. Populated by the
// heartbeat handler from HeartbeatPayload.RuntimeMetadata; consumed by
// dispatch paths (a2a_proxy.dispatchA2A reads IdleTimeout) before
// applying their own platform-default behavior.
//
// Why an in-memory cache and not a DB column:
//   - Heartbeats arrive every ~30s, so a fresh override propagates
//     within a heartbeat cycle of any change in adapter declarations.
//   - On platform restart the cache resets to empty until each
//     workspace's next heartbeat repopulates it. Worst-case window =
//     30s of platform-default behavior. Acceptable; nothing about
//     these overrides is correctness-critical (they tune timeouts +
//     enable native ownership of fallback features, not state).
//   - DB-roundtripping every dispatch would add latency to a hot
//     path (a2a_proxy is on every agent → agent call). The cache is
//     a sync.Map — atomic ptr load per dispatch, zero lock contention
//     under steady load.
//
// Stale entries: a workspace that goes offline never sends another
// heartbeat, but the cache entry persists until the platform restarts.
// Acceptable because dispatchA2A only consults the cache when actually
// dispatching to that workspace — a stale entry for an offline
// workspace just means "use the override that was active when it was
// last alive" (correct behavior; the workspace will get the same
// timeouts when it comes back).
//
// See workspace/adapter_base.py:idle_timeout_override and project
// memory `project_runtime_native_pluggable.md`.
var runtimeOverrides runtimeOverrideCache

type runtimeOverrideEntry struct {
	idleTimeout time.Duration // 0 means "no override; use global default"
	// capabilities maps wire-name keys from RuntimeCapabilities.to_dict()
	// — "heartbeat", "scheduler", "session", "status_mgmt", "retry",
	// "activity_decoration", "channel_dispatch" — to whether the adapter
	// claims native ownership. Consumers (e.g. scheduler.tick) read this
	// to decide whether to fire their platform-fallback behavior for this
	// workspace.
	//
	// nil map means "no capability declarations received yet" → consumers
	// fall back to the platform default (today's behavior).
	capabilities map[string]bool
}

type runtimeOverrideCache struct {
	m sync.Map // key: workspaceID (string), value: runtimeOverrideEntry
}

// loadEntry returns the entry for workspaceID (or a zero-value entry).
// Internal helper for the partial-update Set methods; sync.Map's
// Load doesn't support "read or default" in one shot.
func (c *runtimeOverrideCache) loadEntry(workspaceID string) runtimeOverrideEntry {
	if v, ok := c.m.Load(workspaceID); ok {
		if e, ok := v.(runtimeOverrideEntry); ok {
			return e
		}
	}
	return runtimeOverrideEntry{}
}

// deleteIfEmpty drops the workspace's entry from the cache when both
// idleTimeout and capabilities are absent. Keeps the cache from
// retaining empty husks forever after a runtime stops sending overrides.
func (c *runtimeOverrideCache) deleteIfEmpty(workspaceID string, e runtimeOverrideEntry) {
	if e.idleTimeout <= 0 && len(e.capabilities) == 0 {
		c.m.Delete(workspaceID)
		return
	}
	c.m.Store(workspaceID, e)
}

// SetIdleTimeout records the per-workspace idle-timeout override sent
// in the most recent heartbeat. d == 0 clears the override (falling
// back to the global default), so a runtime that previously declared
// an override and then dropped it cleanly returns to platform behavior.
// Capability flags on the same workspace are preserved.
func (c *runtimeOverrideCache) SetIdleTimeout(workspaceID string, d time.Duration) {
	if workspaceID == "" {
		return
	}
	e := c.loadEntry(workspaceID)
	if d <= 0 {
		e.idleTimeout = 0
	} else {
		e.idleTimeout = d
	}
	c.deleteIfEmpty(workspaceID, e)
}

// IdleTimeout returns the per-workspace override and ok=true when one
// is in effect; ok=false means dispatchA2A should fall back to the
// global idleTimeoutDuration.
func (c *runtimeOverrideCache) IdleTimeout(workspaceID string) (time.Duration, bool) {
	e := c.loadEntry(workspaceID)
	if e.idleTimeout <= 0 {
		return 0, false
	}
	return e.idleTimeout, true
}

// SetCapabilities records the per-workspace capability declaration map
// (e.g. {"scheduler": true, "heartbeat": false, ...}) sent in the most
// recent heartbeat. Replaces any prior map; pass nil to clear.
// IdleTimeout on the same workspace is preserved.
//
// The wire-name keys (heartbeat, scheduler, session, status_mgmt, retry,
// activity_decoration, channel_dispatch) match RuntimeCapabilities.to_dict()
// in workspace/adapter_base.py — keep in sync there.
func (c *runtimeOverrideCache) SetCapabilities(workspaceID string, caps map[string]bool) {
	if workspaceID == "" {
		return
	}
	e := c.loadEntry(workspaceID)
	if len(caps) == 0 {
		e.capabilities = nil
	} else {
		// Defensive copy: caller may reuse / mutate the map after the
		// call; the cache holds long-lived refs.
		dup := make(map[string]bool, len(caps))
		for k, v := range caps {
			dup[k] = v
		}
		e.capabilities = dup
	}
	c.deleteIfEmpty(workspaceID, e)
}

// HasCapability returns true when the workspace's adapter has declared
// native ownership of the named capability. False when no entry exists,
// no capability map was ever sent, or the named capability is absent /
// false. Consumers (scheduler.tick, etc.) call this before firing their
// platform-fallback behavior.
func (c *runtimeOverrideCache) HasCapability(workspaceID, name string) bool {
	if workspaceID == "" || name == "" {
		return false
	}
	e := c.loadEntry(workspaceID)
	return e.capabilities[name]
}

// Reset clears the entire cache. Test-only; production code never
// needs this since heartbeats refresh entries naturally.
func (c *runtimeOverrideCache) Reset() {
	c.m.Range(func(k, _ any) bool {
		c.m.Delete(k)
		return true
	})
}

// ProvidesNativeScheduler is the public adapter exposed to the scheduler
// package — wraps HasCapability("scheduler") with the package-level
// runtimeOverrides instance. Wired into Scheduler.New() at router setup
// to keep scheduler/scheduler.go free of a handlers/ import.
func ProvidesNativeScheduler(workspaceID string) bool {
	return runtimeOverrides.HasCapability(workspaceID, "scheduler")
}
