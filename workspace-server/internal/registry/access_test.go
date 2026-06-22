package registry

import (
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

func setupMockDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { mockDB.Close(); db.DB = prevDB })
	return mock
}

func ptr(s string) *string { return &s }

func expectLookup(mock sqlmock.Sqlmock, id string, parentID *string) {
	row := mock.NewRows([]string{"id", "parent_id"})
	if parentID != nil {
		row.AddRow(id, *parentID)
	} else {
		row.AddRow(id, nil)
	}
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id").
		WithArgs(id).
		WillReturnRows(row)
}

func expectNotFound(mock sqlmock.Sqlmock, id string) {
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id").
		WithArgs(id).
		WillReturnError(sql.ErrNoRows)
}

// ---------- Tests ----------

func TestCanCommunicate_SameWorkspace(t *testing.T) {
	mock := setupMockDB(t)
	expectLookup(mock, "ws-1", nil)
	expectLookup(mock, "ws-1", nil)

	if !CanCommunicate("ws-1", "ws-1") {
		t.Error("same workspace should always communicate")
	}
}

func TestCanCommunicate_Siblings(t *testing.T) {
	mock := setupMockDB(t)
	// ws-a and ws-b are siblings (same parent ws-parent)
	expectLookup(mock, "ws-a", ptr("ws-parent"))
	expectLookup(mock, "ws-b", ptr("ws-parent"))

	if !CanCommunicate("ws-a", "ws-b") {
		t.Error("siblings should communicate")
	}
}

func TestCanCommunicate_Denied_RootSiblings(t *testing.T) {
	mock := setupMockDB(t)
	// Two unrelated org roots (both parent_id = NULL) must NOT communicate.
	// This is the regression test for #1955: without an org_id column, two
	// root workspaces have no shared ancestor, so they must be denied.
	expectLookup(mock, "ws-a", nil)
	expectLookup(mock, "ws-b", nil)

	if CanCommunicate("ws-a", "ws-b") {
		t.Error("unrelated root-level workspaces should NOT communicate")
	}
}

func TestCanCommunicate_ParentToChild(t *testing.T) {
	mock := setupMockDB(t)
	// ws-parent talks to ws-child (whose parent is ws-parent)
	expectLookup(mock, "ws-parent", nil)
	expectLookup(mock, "ws-child", ptr("ws-parent"))

	if !CanCommunicate("ws-parent", "ws-child") {
		t.Error("parent should communicate with child")
	}
}

func TestCanCommunicate_ChildToParent(t *testing.T) {
	mock := setupMockDB(t)
	// ws-child talks up to ws-parent
	expectLookup(mock, "ws-child", ptr("ws-parent"))
	expectLookup(mock, "ws-parent", nil)

	if !CanCommunicate("ws-child", "ws-parent") {
		t.Error("child should communicate with parent")
	}
}

func TestCanCommunicate_Denied_DifferentParents(t *testing.T) {
	mock := setupMockDB(t)
	// ws-a (parent: p1) and ws-b (parent: p2) — not siblings, no shared ancestor.
	expectLookup(mock, "ws-a", ptr("p1"))
	expectLookup(mock, "ws-b", ptr("p2"))
	// Walk #1: isAncestorOf(ws-a, p2) → p2 is parentless, false.
	expectLookup(mock, "p2", nil)
	// Walk #2: isAncestorOf(ws-b, p1) → p1 is parentless, false.
	expectLookup(mock, "p1", nil)

	if CanCommunicate("ws-a", "ws-b") {
		t.Error("workspaces with different parents should NOT communicate")
	}
}

func TestCanCommunicate_Denied_CousinToRoot(t *testing.T) {
	mock := setupMockDB(t)
	// ws-child (parent: ws-mid, which has its own root ws-other-root) and
	// ws-root (a different parentless workspace).
	// The ancestor walk from ws-child should reach ws-other-root but never
	// ws-root, so communication is denied.
	expectLookup(mock, "ws-child", ptr("ws-mid"))
	expectLookup(mock, "ws-root", nil)
	// Ancestor walk: starts at *caller.ParentID = ws-mid. Walks ws-mid → ws-other-root → nil.
	expectLookup(mock, "ws-mid", ptr("ws-other-root"))
	expectLookup(mock, "ws-other-root", nil)

	if CanCommunicate("ws-child", "ws-root") {
		t.Error("child should NOT communicate with unrelated root workspace")
	}
}

func TestCanCommunicate_Denied_CallerNotFound(t *testing.T) {
	mock := setupMockDB(t)
	expectNotFound(mock, "ws-missing")

	if CanCommunicate("ws-missing", "ws-target") {
		t.Error("nonexistent caller should be denied")
	}
}

func TestCanCommunicate_Denied_TargetNotFound(t *testing.T) {
	mock := setupMockDB(t)
	expectLookup(mock, "ws-caller", nil)
	expectNotFound(mock, "ws-missing")

	if CanCommunicate("ws-caller", "ws-missing") {
		t.Error("nonexistent target should be denied")
	}
}

func TestCanCommunicate_Allowed_GrandparentToGrandchild(t *testing.T) {
	mock := setupMockDB(t)
	// PM (no parent) → Backend Engineer (parent: Dev Lead, parent: PM).
	// Originally rejected ("grandparent should NOT communicate with grandchild
	// directly") — that broke audit_summary routing because Security Auditor
	// could not delegate up to PM. The hierarchy is now ancestor↔descendant.
	expectLookup(mock, "ws-pm", nil)
	expectLookup(mock, "ws-be", ptr("ws-dl"))
	// Ancestor walk: target.ParentID = ws-dl. isAncestorOf(ws-pm, ws-dl).
	// Walks ws-dl → ws-pm → match. (Walk lookup #1: ws-dl.)
	expectLookup(mock, "ws-dl", ptr("ws-pm"))

	if !CanCommunicate("ws-pm", "ws-be") {
		t.Error("PM should be able to communicate with Backend Engineer (descendant)")
	}
}

func TestCanCommunicate_Allowed_GrandchildToGrandparent(t *testing.T) {
	mock := setupMockDB(t)
	// Security Auditor (parent: Dev Lead) → PM (parent of Dev Lead).
	// This is the Security Auditor → PM audit_summary delivery path.
	expectLookup(mock, "ws-sec", ptr("ws-dl"))
	expectLookup(mock, "ws-pm", nil)
	// Direct parent → child fast path: target.ParentID = nil, skip.
	// Direct child → parent: caller.ParentID = ws-dl, target.ID = ws-pm,
	//   ws-dl != ws-pm, skip.
	// Distant ancestor → descendant: target.ParentID = nil, skip.
	// Distant descendant → ancestor: caller.ParentID = ws-dl. Walks
	//   isAncestorOf(ws-pm, ws-dl) → looks up ws-dl → returns ws-pm → match.
	expectLookup(mock, "ws-dl", ptr("ws-pm"))

	if !CanCommunicate("ws-sec", "ws-pm") {
		t.Error("Security Auditor should be able to send audit_summary up to PM")
	}
}

func TestCanCommunicate_Allowed_DeepAncestor(t *testing.T) {
	mock := setupMockDB(t)
	// Four-level chain: ws-leaf (parent: ws-l3, parent: ws-l2, parent: ws-l1).
	// ws-leaf → ws-l1 should be allowed.
	expectLookup(mock, "ws-leaf", ptr("ws-l3"))
	expectLookup(mock, "ws-l1", nil)
	// Distant descendant → ancestor walk: starts at ws-l3.
	//   ws-l3 → ws-l2: not ws-l1, continue.
	//   ws-l2 → ws-l1: match!
	expectLookup(mock, "ws-l3", ptr("ws-l2"))
	expectLookup(mock, "ws-l2", ptr("ws-l1"))

	if !CanCommunicate("ws-leaf", "ws-l1") {
		t.Error("4-level descendant should reach root ancestor")
	}
}

func TestCanCommunicate_Denied_UnrelatedAncestors(t *testing.T) {
	mock := setupMockDB(t)
	// Two separate org subtrees:
	//   tree A: ws-a-leaf → ws-a-mid → ws-a-root
	//   tree B: ws-b-leaf → ws-b-mid → ws-b-root
	// ws-a-leaf → ws-b-root must be denied even though both have parents
	// (no shared ancestor).
	expectLookup(mock, "ws-a-leaf", ptr("ws-a-mid"))
	expectLookup(mock, "ws-b-root", nil)
	// Walk: isAncestorOf(ws-b-root, ws-a-mid).
	//   ws-a-mid → ws-a-root: not ws-b-root, continue.
	//   ws-a-root has no parent → false.
	expectLookup(mock, "ws-a-mid", ptr("ws-a-root"))
	expectLookup(mock, "ws-a-root", nil)

	if CanCommunicate("ws-a-leaf", "ws-b-root") {
		t.Error("workspaces in different subtrees should NOT communicate via the walk")
	}
}
