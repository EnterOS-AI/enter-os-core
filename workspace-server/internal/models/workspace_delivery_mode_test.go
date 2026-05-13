package models

import "testing"

// ==================== IsValidDeliveryMode ====================

func TestIsValidDeliveryMode_Valid(t *testing.T) {
	for _, mode := range []string{DeliveryModePush, DeliveryModePoll} {
		if !IsValidDeliveryMode(mode) {
			t.Errorf("IsValidDeliveryMode(%q) = false, want true", mode)
		}
	}
}

func TestIsValidDeliveryMode_Invalid(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},         // empty string is not valid — callers must resolve the default
		{"pushx", false},   // typo
		{"pollx", false},    // typo
		{"PUSH", false},     // case-sensitive
		{"PUSH ", false},    // trailing space
		{"push ", false},    // trailing space
		{"hybrid", false},   // non-existent mode
		{"poll ", false},    // trailing space
	}
	for _, tc := range cases {
		got := IsValidDeliveryMode(tc.val)
		if got != tc.want {
			t.Errorf("IsValidDeliveryMode(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
}

// ==================== WorkspaceStatus ====================

func TestWorkspaceStatus_String(t *testing.T) {
	statuses := []WorkspaceStatus{
		StatusProvisioning,
		StatusOnline,
		StatusOffline,
		StatusDegraded,
		StatusFailed,
		StatusRemoved,
		StatusPaused,
		StatusHibernated,
		StatusHibernating,
		StatusAwaitingAgent,
	}
	for _, s := range statuses {
		if got := s.String(); got != string(s) {
			t.Errorf("WorkspaceStatus(%q).String() = %q, want %q", s, got, string(s))
		}
	}
}

func TestAllWorkspaceStatuses_Length(t *testing.T) {
	// The const block has 10 statuses; AllWorkspaceStatuses must match.
	if got := len(AllWorkspaceStatuses); got != 10 {
		t.Errorf("len(AllWorkspaceStatuses) = %d, want 10", got)
	}
}

func TestAllWorkspaceStatuses_ContainsAllNamed(t *testing.T) {
	// Verify every named const appears in AllWorkspaceStatuses exactly once.
	named := []WorkspaceStatus{
		StatusProvisioning,
		StatusOnline,
		StatusOffline,
		StatusDegraded,
		StatusFailed,
		StatusRemoved,
		StatusPaused,
		StatusHibernated,
		StatusHibernating,
		StatusAwaitingAgent,
	}
	set := make(map[WorkspaceStatus]bool, len(AllWorkspaceStatuses))
	for _, s := range AllWorkspaceStatuses {
		set[s] = true
	}
	for _, s := range named {
		if !set[s] {
			t.Errorf("named status %q missing from AllWorkspaceStatuses", s)
		}
	}
	if len(set) != len(named) {
		t.Errorf("AllWorkspaceStatuses has %d unique entries, want %d", len(set), len(named))
	}
}

func TestAllWorkspaceStatuses_NoEmpty(t *testing.T) {
	for _, s := range AllWorkspaceStatuses {
		if s == "" {
			t.Errorf("AllWorkspaceStatuses contains empty string")
		}
	}
}
