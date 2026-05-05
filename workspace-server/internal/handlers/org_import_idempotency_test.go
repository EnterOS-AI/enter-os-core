package handlers

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Tests for the idempotency helper added in #2859 (RFC #2857 Phase 3).
//
// Background: org_import.createWorkspaceTree was non-idempotent —
// every call INSERTed a fresh row for every workspace in the tree,
// regardless of whether matching workspaces already existed. Calling
// /org/import twice with the same template duplicated the entire tree;
// in a 4-day window tenant-hongming accumulated 72 stale child
// workspaces this way.
//
// The fix routes through lookupExistingChild before INSERT. These
// tests pin the helper's three observable behaviors plus an AST gate
// that catches future re-introductions of the un-checked INSERT.

func TestLookupExistingChild_NotFound_ReturnsFalseNoError(t *testing.T) {
	mock := setupTestDB(t)
	// 0-row result → driver returns sql.ErrNoRows on Scan.
	parent := "parent-1"
	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WithArgs("Alpha", &parent).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	h := &OrgHandler{}
	id, found, err := h.lookupExistingChild(context.Background(), "Alpha", &parent)

	if err != nil {
		t.Fatalf("expected nil error on no-rows, got: %v", err)
	}
	if found {
		t.Errorf("expected found=false on no-rows, got found=true")
	}
	if id != "" {
		t.Errorf("expected empty id on no-rows, got %q", id)
	}
}

func TestLookupExistingChild_Found_ReturnsIDAndTrue(t *testing.T) {
	mock := setupTestDB(t)
	parent := "parent-1"
	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WithArgs("Alpha", &parent).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-existing-uuid"))

	h := &OrgHandler{}
	id, found, err := h.lookupExistingChild(context.Background(), "Alpha", &parent)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Errorf("expected found=true when row exists")
	}
	if id != "ws-existing-uuid" {
		t.Errorf("expected id=ws-existing-uuid, got %q", id)
	}
}

func TestLookupExistingChild_NilParent_MatchesRoot(t *testing.T) {
	// `parent_id IS NOT DISTINCT FROM NULL` is the load-bearing trick —
	// a plain `=` would never match a NULL row. Pin that roots
	// (parent_id=NULL) are still found by the lookup.
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WithArgs("RootAgent", (*string)(nil)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-root-uuid"))

	h := &OrgHandler{}
	id, found, err := h.lookupExistingChild(context.Background(), "RootAgent", nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found || id != "ws-root-uuid" {
		t.Errorf("expected found=true id=ws-root-uuid, got found=%v id=%q", found, id)
	}
}

func TestLookupExistingChild_DBError_Propagates(t *testing.T) {
	// A real DB error must NOT be silently swallowed. If the SELECT
	// can't run, the caller fails fast — never falls back to creating
	// a duplicate. That fallback is the failure mode the helper exists
	// to prevent.
	mock := setupTestDB(t)
	parent := "parent-1"
	connFail := errors.New("simulated postgres unavailable")
	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WithArgs("Alpha", &parent).
		WillReturnError(connFail)

	h := &OrgHandler{}
	id, found, err := h.lookupExistingChild(context.Background(), "Alpha", &parent)

	if err == nil {
		t.Fatalf("expected DB error to propagate, got nil")
	}
	// Helper returns the raw error, not a wrap — match by string for
	// portability across error wrapping conventions.
	if !strings.Contains(err.Error(), "simulated postgres unavailable") {
		t.Errorf("expected the original DB error to surface, got: %v", err)
	}
	if found {
		t.Errorf("expected found=false on DB error, got found=true")
	}
	if id != "" {
		t.Errorf("expected empty id on DB error, got %q", id)
	}
}

// Source-level guard — pins that org_import.go calls
// h.lookupExistingChild BEFORE its INSERT INTO workspaces.
//
// Per memory feedback_behavior_based_ast_gates.md: pin the behavior
// (idempotency check before INSERT), not just function names. If a
// future refactor reintroduces the un-checked INSERT (the original
// bug shape that leaked 72 workspaces in 4 days), this test fails.
func TestCreateWorkspaceTree_CallsLookupBeforeInsert(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(wd, "org_import.go"))
	if err != nil {
		t.Fatalf("read org_import.go: %v", err)
	}

	lookupAt := bytes.Index(src, []byte("h.lookupExistingChild("))
	insertAt := bytes.Index(src, []byte("INSERT INTO workspaces"))

	if lookupAt < 0 {
		t.Fatalf("org_import.go missing call to h.lookupExistingChild — idempotency check removed?")
	}
	if insertAt < 0 {
		t.Fatalf("org_import.go missing INSERT INTO workspaces — schema change?")
	}
	if lookupAt > insertAt {
		t.Errorf("h.lookupExistingChild must come BEFORE INSERT INTO workspaces in org_import.go (lookup@%d, insert@%d) — non-idempotent ordering would re-leak under repeat /org/import calls", lookupAt, insertAt)
	}
}
