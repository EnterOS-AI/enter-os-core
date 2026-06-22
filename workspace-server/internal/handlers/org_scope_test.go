package handlers

// Sqlmock-backed coverage for org_scope.go (orgRootID + sameOrg).
// Security-critical path — cross-tenant isolation (#1953).

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

// ---------- orgRootID ----------

func TestOrgRootID_HappyPath_NonRoot(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	// CTE walks: ws-child → ws-parent → org-root (parent_id IS NULL)
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(wsUUID3))

	root, err := orgRootID(context.Background(), db.DB, wsUUID1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root != wsUUID3 {
		t.Errorf("root=%q, want %q", root, wsUUID3)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestOrgRootID_WorkspaceIsRoot(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	// One-row chain: the workspace itself is the org root.
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(wsUUID1))

	root, err := orgRootID(context.Background(), db.DB, wsUUID1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root != wsUUID1 {
		t.Errorf("root=%q, want %q", root, wsUUID1)
	}
}

func TestOrgRootID_NoRows(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}))

	_, err := orgRootID(context.Background(), db.DB, wsUUID1)
	if !errors.Is(err, errNoOrgRoot) {
		t.Fatalf("expected errNoOrgRoot, got %v", err)
	}
}

func TestOrgRootID_DBError(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(wsUUID1).
		WillReturnError(errors.New("conn lost"))

	_, err := orgRootID(context.Background(), db.DB, wsUUID1)
	if err == nil || errors.Is(err, errNoOrgRoot) {
		t.Fatalf("expected DB error, got %v", err)
	}
}

func TestOrgRootID_EmptyRoot(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	// Row present but root is empty string → treated as not-found.
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(""))

	_, err := orgRootID(context.Background(), db.DB, wsUUID1)
	if !errors.Is(err, errNoOrgRoot) {
		t.Fatalf("expected errNoOrgRoot for empty root, got %v", err)
	}
}

// ---------- sameOrg ----------

func TestSameOrg_SameWorkspace(t *testing.T) {
	// Fast path: identical IDs are same-org without touching DB.
	mock, cleanup := withMockDB(t)
	defer cleanup()

	ok, err := sameOrg(context.Background(), db.DB, wsUUID1, wsUUID1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("same workspace must be same-org")
	}
	// No DB expectations → proves short-circuit.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB was touched despite short-circuit: %v", err)
	}
}

func TestSameOrg_SameOrg(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(wsUUID3))
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(wsUUID2).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(wsUUID3))

	ok, err := sameOrg(context.Background(), db.DB, wsUUID1, wsUUID2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected same-org")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSameOrg_DifferentOrg(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(wsUUID3))
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(wsUUID2).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow("org-b"))

	ok, err := sameOrg(context.Background(), db.DB, wsUUID1, wsUUID2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected different-org")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSameOrg_OrgRootFails(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(wsUUID1).
		WillReturnError(errors.New("conn lost"))

	_, err := sameOrg(context.Background(), db.DB, wsUUID1, wsUUID2)
	if err == nil {
		t.Fatal("expected error when orgRootID fails")
	}
}

func TestSameOrg_OrgRootNotFound(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}))

	_, err := sameOrg(context.Background(), db.DB, wsUUID1, wsUUID2)
	if !errors.Is(err, errNoOrgRoot) {
		t.Fatalf("expected errNoOrgRoot, got %v", err)
	}
}
