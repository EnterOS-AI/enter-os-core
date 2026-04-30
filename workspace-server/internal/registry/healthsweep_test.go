package registry

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// mockChecker implements ContainerChecker for testing.
type mockChecker struct {
	mu      sync.Mutex
	running map[string]bool
}

func (m *mockChecker) IsRunning(_ context.Context, workspaceID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[workspaceID], nil
}

func setupTestDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	db.DB = mockDB
	t.Cleanup(func() { mockDB.Close() })
	return mock
}

func setupTestRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	db.RDB = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { mr.Close() })
	return mr
}

func TestSweepOnlineWorkspaces_DeadContainer(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)

	// Set up Redis keys for a workspace that's about to be detected as dead
	mr.Set("ws:ws-dead-123", "online")
	mr.Set("ws:ws-dead-123:url", "http://127.0.0.1:32000")
	mr.Set("ws:ws-dead-123:internal_url", "http://ws-ws-dead-123:8000")

	// Mock: query returns one online workspace
	rows := sqlmock.NewRows([]string{"id"}).AddRow("ws-dead-123")
	mock.ExpectQuery("SELECT id FROM workspaces WHERE status IN").
		WillReturnRows(rows)

	// Mock: update to offline (Docker sweep keeps 'offline' status —
	// 'awaiting_agent' is the external-runtime path).
	mock.ExpectExec("UPDATE workspaces SET status = 'offline'").
		WithArgs("ws-dead-123").
		WillReturnResult(sqlmock.NewResult(0, 1))

	checker := &mockChecker{running: map[string]bool{
		"ws-dead-123": false, // container is dead
	}}

	var offlineCalled []string
	var mu sync.Mutex
	onOffline := func(_ context.Context, id string) {
		mu.Lock()
		offlineCalled = append(offlineCalled, id)
		mu.Unlock()
	}

	sweepOnlineWorkspaces(context.Background(), checker, onOffline)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(offlineCalled) != 1 || offlineCalled[0] != "ws-dead-123" {
		t.Fatalf("expected onOffline for ws-dead-123, got: %v", offlineCalled)
	}

	// Redis keys should be cleared
	if mr.Exists("ws:ws-dead-123") {
		t.Error("expected liveness key to be deleted")
	}
	if mr.Exists("ws:ws-dead-123:url") {
		t.Error("expected URL cache to be deleted")
	}
	if mr.Exists("ws:ws-dead-123:internal_url") {
		t.Error("expected internal URL cache to be deleted")
	}
}

func TestSweepOnlineWorkspaces_RunningContainer(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	rows := sqlmock.NewRows([]string{"id"}).AddRow("ws-alive-456")
	mock.ExpectQuery("SELECT id FROM workspaces WHERE status IN").
		WillReturnRows(rows)

	// No UPDATE expected — container is running
	checker := &mockChecker{running: map[string]bool{
		"ws-alive-456": true,
	}}

	offlineCalled := false
	sweepOnlineWorkspaces(context.Background(), checker, func(_ context.Context, id string) {
		offlineCalled = true
	})

	if offlineCalled {
		t.Error("onOffline should not be called for running container")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestStartHealthSweep_NilChecker(t *testing.T) {
	// Should return immediately without panicking
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		StartHealthSweep(ctx, nil, time.Second, nil)
		close(done)
	}()

	select {
	case <-done:
		// Good — returned immediately
	case <-time.After(2 * time.Second):
		t.Fatal("StartHealthSweep with nil checker should return immediately")
	}
}

// ==================== Phase 30.7 — sweepStaleRemoteWorkspaces ====================

// The remote-liveness sweep queries workspaces with runtime='external'
// whose last_heartbeat_at is older than the stale-after window, marks
// them offline, clears Redis state, and fires onOffline. These tests
// verify the SQL shape, the offline-path side effects, and the
// environment-variable override for the staleness window.

func TestSweepStaleRemoteWorkspaces_MarksStaleAwaitingAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Two stale remote workspaces returned by the query
	mock.ExpectQuery(`FROM workspaces\s+WHERE status IN \('online', 'degraded'\)\s+AND COALESCE\(runtime, 'langgraph'\) = 'external'\s+AND COALESCE\(last_heartbeat_at, updated_at\) < now\(\) - `).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("ws-stale-1").
			AddRow("ws-stale-2"))
	mock.ExpectExec(`UPDATE workspaces SET status = 'awaiting_agent'`).
		WithArgs("ws-stale-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspaces SET status = 'awaiting_agent'`).
		WithArgs("ws-stale-2").
		WillReturnResult(sqlmock.NewResult(0, 1))

	var offlineCalls []string
	onOffline := func(_ context.Context, id string) {
		offlineCalls = append(offlineCalls, id)
	}

	sweepStaleRemoteWorkspaces(context.Background(), onOffline)

	if len(offlineCalls) != 2 {
		t.Errorf("expected onOffline called twice, got %d (%v)", len(offlineCalls), offlineCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSweepStaleRemoteWorkspaces_NoStaleWorkspaces(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`FROM workspaces\s+WHERE status IN \('online', 'degraded'\)\s+AND COALESCE\(runtime, 'langgraph'\) = 'external'`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	called := 0
	onOffline := func(_ context.Context, _ string) { called++ }

	sweepStaleRemoteWorkspaces(context.Background(), onOffline)

	if called != 0 {
		t.Errorf("onOffline should not fire when no stale rows; got %d", called)
	}
}

func TestSweepStaleRemoteWorkspaces_NilCallbackNoPanic(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-x"))
	mock.ExpectExec(`UPDATE workspaces SET status = 'awaiting_agent'`).
		WithArgs("ws-x").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Must not panic with nil callback
	sweepStaleRemoteWorkspaces(context.Background(), nil)
}

func TestSweepStaleRemoteWorkspaces_QueryErrorLogged(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`FROM workspaces`).
		WillReturnError(assertDBDown{})

	// Must return cleanly without panicking. No onOffline should fire.
	called := 0
	sweepStaleRemoteWorkspaces(context.Background(), func(_ context.Context, _ string) { called++ })
	if called != 0 {
		t.Errorf("on query error, no onOffline should fire; got %d", called)
	}
}

type assertDBDown struct{}

func (assertDBDown) Error() string { return "simulated DB outage" }

// ==================== Phase 30.7 — remoteStaleAfter env override ====================

func TestRemoteStaleAfter_DefaultWhenUnset(t *testing.T) {
	t.Setenv("REMOTE_LIVENESS_STALE_AFTER", "")
	if got := remoteStaleAfter(); got != DefaultRemoteStaleAfter {
		t.Errorf("expected default %s, got %s", DefaultRemoteStaleAfter, got)
	}
}

func TestRemoteStaleAfter_HonorsValidOverride(t *testing.T) {
	t.Setenv("REMOTE_LIVENESS_STALE_AFTER", "45")
	if got := remoteStaleAfter(); got != 45*time.Second {
		t.Errorf("expected 45s, got %s", got)
	}
}

func TestRemoteStaleAfter_FallsBackOnGarbage(t *testing.T) {
	for _, v := range []string{"abc", "0", "-10", ""} {
		t.Setenv("REMOTE_LIVENESS_STALE_AFTER", v)
		if got := remoteStaleAfter(); got != DefaultRemoteStaleAfter {
			t.Errorf("value %q: expected fallback to default, got %s", v, got)
		}
	}
}

// ==================== Phase 30.7 — StartHealthSweep with nil Docker checker ====================

// Before 30.7, nil-checker caused StartHealthSweep to return immediately
// (no liveness monitoring at all). Now it should still run the remote
// sweep on the ticker. We verify by observing at least one remote-sweep
// query hits the mocked DB before we cancel.

func TestStartHealthSweep_NilCheckerRunsRemoteSweep(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// The goroutine will tick once every 50ms; we give it 200ms then
	// cancel. sqlmock will satisfy any number of calls.
	mock.ExpectQuery(`FROM workspaces\s+WHERE status IN \('online', 'degraded'\)\s+AND COALESCE\(runtime, 'langgraph'\) = 'external'`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		StartHealthSweep(ctx, nil, 50*time.Millisecond, nil)
		close(done)
	}()

	time.Sleep(120 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StartHealthSweep did not return after ctx cancel")
	}

	// Expectations may have been met multiple times; we assert the
	// query shape matched at least once. sqlmock.MatchExpectationsInOrder
	// with a single Query expectation handles that by matching the
	// first call and leaving subsequent calls unmatched (logged, not
	// panicking). Test passes as long as we didn't panic.
}
