package router

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

// setupRouterTestDB initialises db.DB with a sqlmock connection and returns
// the mock controller. Restores db.DB on test cleanup.
func setupRouterTestDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	prev := db.DB
	db.DB = mockDB
	t.Cleanup(func() {
		db.DB = prev
		mockDB.Close()
	})
	return mock
}
