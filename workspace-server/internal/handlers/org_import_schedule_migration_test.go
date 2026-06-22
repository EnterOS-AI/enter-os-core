package handlers

import (
	"context"
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// TestMigrateRuntimeSchedulesFromRemovedPredecessor verifies the happy path:
// a removed predecessor exists for the agent (matched by role), and its
// runtime-created schedules are re-pointed onto the freshly-created workspace.
// internal#2006 (recreate-orphans-schedules regression).
func TestMigrateRuntimeSchedulesFromRemovedPredecessor(t *testing.T) {
	mock := setupTestDB(t)
	h := &OrgHandler{}

	// Predecessor lookup (role branch) returns the removed prior workspace.
	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("old-removed-ws"))
	// Re-point UPDATE migrates 2 runtime schedules.
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WillReturnResult(sqlmock.NewResult(0, 2))

	parent := "parent-1"
	h.migrateRuntimeSchedulesFromRemovedPredecessor(
		context.Background(), "new-ws", interface{}("code-reviewer"), "Code Reviewer (2)", &parent,
	)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestMigrateRuntimeSchedules_NoPredecessor verifies the first-time-create path:
// no removed predecessor → the function returns after the lookup and MUST NOT
// run the re-point UPDATE (sqlmock errors on an unexpected query if it does).
func TestMigrateRuntimeSchedules_NoPredecessor(t *testing.T) {
	mock := setupTestDB(t)
	h := &OrgHandler{}

	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WillReturnError(sql.ErrNoRows)
	// No ExpectExec — an UPDATE here would be an unexpected query → test fails.

	h.migrateRuntimeSchedulesFromRemovedPredecessor(
		context.Background(), "new-ws", interface{}("researcher"), "Root-Cause Researcher", nil,
	)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestMigrateRuntimeSchedules_NameFallback verifies the name-branch lookup is
// used when the agent has no stable role (role == nil), still followed by the
// re-point UPDATE.
func TestMigrateRuntimeSchedules_NameFallback(t *testing.T) {
	mock := setupTestDB(t)
	h := &OrgHandler{}

	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("old-removed-ws"))
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.migrateRuntimeSchedulesFromRemovedPredecessor(
		context.Background(), "new-ws", nil, "Some Agent", nil,
	)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
