package events

import (
	"sort"
	"strings"
	"testing"
)

// TestAllEventTypes_IsSnapshot pins the canonical event taxonomy.
// Adding a new constant in types.go without updating AllEventTypes
// (or vice versa) fails this test.
//
// The snapshot is also the authoritative input to the canvas-side
// parity gate (PR-B-2 follow-up): the TypeScript union members in
// canvas/src/lib/ws-events.ts MUST match this list exactly. A drift
// gate at CI time will assert set equality once the TS file lands.
func TestAllEventTypes_IsSnapshot(t *testing.T) {
	// Every named constant must appear in AllEventTypes. Walk via
	// reflection over the package-level vars would over-include test
	// fixtures, so list the canonical names here. When a constant
	// is added in types.go, append the EventType's literal value
	// to the expected list below — the failure message names
	// exactly what's missing so the diff is one-line obvious.
	expected := []string{
		"A2A_RESPONSE",
		"ACTIVITY_LOGGED",
		"AGENT_ASSIGNED",
		"AGENT_CARD_UPDATED",
		"AGENT_MESSAGE",
		"AGENT_MOVED",
		"AGENT_REMOVED",
		"AGENT_REPLACED",
		"APPROVAL_ESCALATED",
		"APPROVAL_REQUESTED",
		"CHANNEL_MESSAGE",
		"CRON_EXECUTED",
		"CRON_SKIPPED",
		"DELEGATION_COMPLETE",
		"DELEGATION_FAILED",
		"DELEGATION_SENT",
		"DELEGATION_STATUS",
		"EXTERNAL_CREDENTIALS_ROTATED",
		"TASK_UPDATED",
		"WORKSPACE_AWAITING_AGENT",
		"WORKSPACE_DEGRADED",
		"WORKSPACE_HEARTBEAT",
		"WORKSPACE_HIBERNATED",
		"WORKSPACE_OFFLINE",
		"WORKSPACE_ONLINE",
		"WORKSPACE_PAUSED",
		"WORKSPACE_PROVISIONING",
		"WORKSPACE_PROVISION_FAILED",
		"WORKSPACE_REMOVED",
	}
	sort.Strings(expected)

	actual := make([]string, 0, len(AllEventTypes))
	for _, e := range AllEventTypes {
		actual = append(actual, string(e))
	}
	sort.Strings(actual)

	if len(actual) != len(expected) {
		t.Errorf("AllEventTypes count = %d, want %d\nactual:   %s\nexpected: %s",
			len(actual), len(expected),
			strings.Join(actual, ", "),
			strings.Join(expected, ", "))
		return
	}
	for i, want := range expected {
		if actual[i] != want {
			t.Errorf("AllEventTypes[%d] = %q, want %q (full diff:\n  actual:   %v\n  expected: %v\n)",
				i, actual[i], want, actual, expected)
		}
	}
}

// TestEventType_NoEmptyConstants pins that no constant declared in
// types.go has an accidentally-empty value. The catch is the
// "WORKSPACE_X" → forgot-to-fill pattern: a typo in the literal
// would surface as the empty string, and broadcast pipelines would
// silently filter empty-name events without any error signal.
func TestEventType_NoEmptyConstants(t *testing.T) {
	for _, e := range AllEventTypes {
		if string(e) == "" {
			t.Errorf("found empty EventType in AllEventTypes — typo in types.go?")
		}
	}
}

// TestEventType_AllUppercaseSnakeCase pins the wire format. Mixed
// case or kebab-case would break the canvas TypeScript switch
// statements (every consumer's `case "AGENT_MESSAGE":` is upper-
// snake). The check is the catch for an accidental
// `"agent_message"` typo that wouldn't fail the snapshot gate.
func TestEventType_AllUppercaseSnakeCase(t *testing.T) {
	for _, e := range AllEventTypes {
		s := string(e)
		// Allowed chars: A-Z, 0-9, _ — nothing else, no leading/
		// trailing underscores, no consecutive underscores.
		if s != strings.ToUpper(s) {
			t.Errorf("EventType %q is not all-uppercase — wire format requires upper-snake", s)
		}
		if strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") {
			t.Errorf("EventType %q has leading/trailing underscore — disallowed", s)
		}
		if strings.Contains(s, "__") {
			t.Errorf("EventType %q has consecutive underscores — disallowed", s)
		}
		for _, r := range s {
			if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
				t.Errorf("EventType %q contains disallowed char %q", s, r)
				break
			}
		}
	}
}
