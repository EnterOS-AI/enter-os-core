package registry

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

func setupHibernationMock(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { mockDB.Close(); db.DB = prevDB })
	return mock
}

// TestHibernateIdleWorkspaces_CallsHandlerForEachCandidate verifies that
// hibernateIdleWorkspaces calls onHibernate once for each workspace row
// returned by the DB query.
func TestHibernateIdleWorkspaces_CallsHandlerForEachCandidate(t *testing.T) {
	mock := setupHibernationMock(t)

	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("ws-idle-1").
			AddRow("ws-idle-2"))

	var called []string
	hibernateIdleWorkspaces(context.Background(), func(ctx context.Context, id string) {
		called = append(called, id)
	})

	if len(called) != 2 {
		t.Fatalf("expected 2 hibernations, got %d: %v", len(called), called)
	}
	if called[0] != "ws-idle-1" || called[1] != "ws-idle-2" {
		t.Errorf("unexpected IDs: %v", called)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestHibernateIdleWorkspaces_NoRowsNoHandler verifies that no handler is
// called when the query returns zero rows (no idle workspaces).
func TestHibernateIdleWorkspaces_NoRowsNoHandler(t *testing.T) {
	mock := setupHibernationMock(t)

	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // empty

	var called int
	hibernateIdleWorkspaces(context.Background(), func(_ context.Context, _ string) {
		called++
	})

	if called != 0 {
		t.Errorf("expected 0 hibernations, got %d", called)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestHibernateIdleWorkspaces_DBErrorDoesNotPanic verifies that a DB error
// from the query is logged but does not crash the monitor loop.
func TestHibernateIdleWorkspaces_DBErrorDoesNotPanic(t *testing.T) {
	mock := setupHibernationMock(t)

	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WillReturnError(sql.ErrConnDone)

	// Should not panic
	hibernateIdleWorkspaces(context.Background(), func(_ context.Context, _ string) {
		t.Error("handler should not be called on DB error")
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestStartHibernationMonitor_TicksAndCallsHandler verifies the monitor loop
// ticks at the configured interval and calls the handler.
func TestStartHibernationMonitor_TicksAndCallsHandler(t *testing.T) {
	mock := setupHibernationMock(t)

	// Expect at least one DB query (the first tick)
	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-hibernate-me"))

	var callCount int32
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		StartHibernationMonitorWithInterval(ctx, 50*time.Millisecond, func(_ context.Context, id string) {
			if id == "ws-hibernate-me" {
				atomic.AddInt32(&callCount, 1)
				cancel() // stop after first hit
			}
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitor did not stop within timeout")
	}

	if atomic.LoadInt32(&callCount) == 0 {
		t.Error("expected handler to be called at least once")
	}
}

// TestStartHibernationMonitor_StopsOnContextCancel verifies clean shutdown
// when the context is cancelled before any tick fires.
func TestStartHibernationMonitor_StopsOnContextCancel(t *testing.T) {
	_ = setupHibernationMock(t) // no DB calls expected

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	done := make(chan struct{})
	go func() {
		// Very long interval — only context cancel should stop it
		StartHibernationMonitorWithInterval(ctx, 10*time.Minute, func(_ context.Context, _ string) {
			// should never be called
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not stop on context cancel")
	}
}
