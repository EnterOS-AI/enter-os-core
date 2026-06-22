package handlers

import (
	"context"
	"sort"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Tests for the reconcile-mode + audit-event additions to OrgHandler.Import.
//
// Background: /org/import was purely additive — re-running with a tree that
// renamed/reparented a role left the prior workspace online (different
// parent_id from the new one, so lookupExistingChild's parent-scoped dedupe
// missed it). The 2026-05-08 dev-tree case left 8 orphans surviving a
// re-import. mode="reconcile" closes the gap; emitOrgEvent makes "what
// happened at 20:13?" queryable instead of stdout-grep archaeology.

func TestWalkOrgWorkspaceNames_FlatTree(t *testing.T) {
	tree := []OrgWorkspace{
		{Name: "Dev Lead"},
		{Name: "Release Manager"},
	}
	var names []string
	walkOrgWorkspaceNames(tree, &names)
	sort.Strings(names)
	want := []string{"Dev Lead", "Release Manager"}
	if !equalStrings(names, want) {
		t.Errorf("flat tree: got %v, want %v", names, want)
	}
}

func TestWalkOrgWorkspaceNames_NestedTree(t *testing.T) {
	tree := []OrgWorkspace{
		{
			Name: "Dev Lead",
			Children: []OrgWorkspace{
				{Name: "Core Platform Lead", Children: []OrgWorkspace{{Name: "Core-BE"}}},
				{Name: "SDK Lead"},
			},
		},
	}
	var names []string
	walkOrgWorkspaceNames(tree, &names)
	sort.Strings(names)
	want := []string{"Core Platform Lead", "Core-BE", "Dev Lead", "SDK Lead"}
	if !equalStrings(names, want) {
		t.Errorf("nested tree: got %v, want %v", names, want)
	}
}

// Pins the contract that spawning:false subtrees still contribute their names
// to the reconcile working set. If the walker started skipping them, a
// re-import that toggled spawning would orphan whichever workspaces had been
// previously imported with spawning:true — the inverse of the bug being
// fixed. Spawning gates *provisioning*, not *reconcile membership*.
func TestWalkOrgWorkspaceNames_SpawningFalseStillCounted(t *testing.T) {
	f := false
	tree := []OrgWorkspace{
		{Name: "Dev Lead", Children: []OrgWorkspace{
			{Name: "Skipped Lead", Spawning: &f, Children: []OrgWorkspace{
				{Name: "Skipped Child"},
			}},
		}},
	}
	var names []string
	walkOrgWorkspaceNames(tree, &names)
	sort.Strings(names)
	want := []string{"Dev Lead", "Skipped Child", "Skipped Lead"}
	if !equalStrings(names, want) {
		t.Errorf("spawning:false subtree: got %v, want %v", names, want)
	}
}

func TestWalkOrgWorkspaceNames_EmptyNamesSkipped(t *testing.T) {
	tree := []OrgWorkspace{
		{Name: "Dev Lead"},
		{Name: ""}, // YAML default / placeholder
		{Name: "Release Manager"},
	}
	var names []string
	walkOrgWorkspaceNames(tree, &names)
	sort.Strings(names)
	want := []string{"Dev Lead", "Release Manager"}
	if !equalStrings(names, want) {
		t.Errorf("empty-name skip: got %v, want %v", names, want)
	}
}

// emitOrgEvent must INSERT into structure_events with event_type + JSON
// payload. Verifies the SQL shape pinning so a future schema rename
// (e.g., switching to audit_events) breaks the test loudly instead of
// silently dropping telemetry.
func TestEmitOrgEvent_InsertsToStructureEvents(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectExec(`INSERT INTO structure_events`).
		WithArgs("org.import.started", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	emitOrgEvent(context.Background(), "org.import.started", map[string]any{
		"name": "test-org",
		"mode": "reconcile",
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// Insert failures are log-and-swallow — telemetry MUST NOT block the
// caller path. If this regresses (e.g., a future patch returns the err),
// org-import requests would fail with HTTP 500 every time a structure_events
// INSERT hiccups, which is strictly worse than losing the row.
func TestEmitOrgEvent_DBErrorIsSwallowed(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectExec(`INSERT INTO structure_events`).
		WithArgs("org.import.failed", sqlmock.AnyArg()).
		WillReturnError(errSentinelTest)

	// Must not panic; must not propagate. The function returns nothing,
	// so the contract is "doesn't crash."
	emitOrgEvent(context.Background(), "org.import.failed", map[string]any{
		"err": "preflight failed",
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestErrString(t *testing.T) {
	if got := errString(nil); got != "" {
		t.Errorf("nil error: got %q, want empty", got)
	}
	if got := errString(errSentinelTest); got != "sentinel" {
		t.Errorf("sentinel error: got %q, want \"sentinel\"", got)
	}
}

// errSentinelTest is a marker error used for swallow-error assertions.
var errSentinelTest = sentinelErrTest{}

type sentinelErrTest struct{}

func (sentinelErrTest) Error() string { return "sentinel" }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
