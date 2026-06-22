package handlers

import "testing"

// #2989 rollout-safety (2026-06-18): nil (runtime predating #147, field absent)
// must be treated as ALLOW, not fail-closed — else the gate takes every
// pre-#147 concierge offline. Only an explicit false fail-closes.
func TestPlatformAgentMCPServerPresent_NilTolerance(t *testing.T) {
	h := &RegistryHandler{}
	tt := true
	ff := false
	if !h.platformAgentMCPServerPresent(nil) {
		t.Error("nil (old runtime, field absent) must be ALLOW, got block")
	}
	if !h.platformAgentMCPServerPresent(&tt) {
		t.Error("&true must be allow")
	}
	if h.platformAgentMCPServerPresent(&ff) {
		t.Error("&false (runtime reports MCP absent) must fail-closed")
	}
}
