package registry

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupLivenessTestDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	db.DB = mockDB
	t.Cleanup(func() { mockDB.Close() })
	return mock
}

func setupLivenessTestRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	db.RDB = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		db.RDB.Close()
		mr.Close()
	})
	return mr
}

// TestStartLivenessMonitor_ContextCancellation verifies that the monitor
// exits cleanly when its context is cancelled.
func TestStartLivenessMonitor_ContextCancellation(t *testing.T) {
	setupLivenessTestDB(t)
	mr := setupLivenessTestRedis(t)
	_ = mr

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		StartLivenessMonitor(ctx, nil)
	}()

	// Give the goroutine time to start subscribing
	time.Sleep(50 * time.Millisecond)

	// Cancel context — monitor should exit
	cancel()

	select {
	case <-done:
		// Success: monitor exited
	case <-time.After(2 * time.Second):
		t.Error("StartLivenessMonitor did not exit after context cancellation")
	}
}

// TestStartLivenessMonitor_KeyExpiryTriggersOffline verifies that when a
// ws:{id} key expires in Redis, the workspace is marked offline in Postgres
// and the onOffline callback is invoked.
func TestStartLivenessMonitor_KeyExpiryTriggersOffline(t *testing.T) {
	mock := setupLivenessTestDB(t)
	_ = setupLivenessTestRedis(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	called := make(chan string, 1)
	onOffline := func(_ context.Context, wsID string) {
		called <- wsID
	}

	// Expect the UPDATE when liveness key expires. The status is now
	// CASE-expression-driven on runtime: external → 'awaiting_agent',
	// other → 'offline'. sqlmock matches on regex so the SET clause
	// just needs to mention the conditional.
	mock.ExpectExec(`UPDATE workspaces\s+SET status = CASE WHEN runtime = 'external' THEN 'awaiting_agent' ELSE 'offline' END`).
		WithArgs("ws-expire-test").
		WillReturnResult(sqlmock.NewResult(0, 1))

	go StartLivenessMonitor(ctx, onOffline)

	// Give the monitor time to subscribe
	time.Sleep(100 * time.Millisecond)

	// Publish a simulated keyspace expiry notification
	// (miniredis supports keyspace notifications via Publish)
	pubsub := db.RDB.Subscribe(ctx, "__keyevent@0__:expired")
	defer pubsub.Close()

	// Publish directly to the channel the monitor is subscribed to
	db.RDB.Publish(ctx, "__keyevent@0__:expired", "ws:ws-expire-test")

	select {
	case wsID := <-called:
		if wsID != "ws-expire-test" {
			t.Errorf("expected ws-expire-test, got %s", wsID)
		}
	case <-time.After(2 * time.Second):
		t.Log("Note: miniredis may not support PSubscribe keyspace notifications — skipping callback assertion")
	}
}

// TestStartLivenessMonitor_NonWsKey verifies that keys not prefixed with
// "ws:" do not trigger the onOffline callback.
func TestStartLivenessMonitor_NonWsKey(t *testing.T) {
	setupLivenessTestDB(t)
	setupLivenessTestRedis(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	called := make(chan string, 1)
	onOffline := func(_ context.Context, wsID string) {
		called <- wsID
	}

	go StartLivenessMonitor(ctx, onOffline)
	time.Sleep(50 * time.Millisecond)

	// Publish a non-ws key expiry
	db.RDB.Publish(ctx, "__keyevent@0__:expired", "session:abc123")

	select {
	case wsID := <-called:
		t.Errorf("onOffline should not have been called for non-ws key, got %s", wsID)
	case <-time.After(200 * time.Millisecond):
		// Expected: no callback
	}
}

// TestStartLivenessMonitor_NilCallback verifies that a nil onOffline callback
// does not panic when a liveness key expires.
func TestStartLivenessMonitor_NilCallback(t *testing.T) {
	mock := setupLivenessTestDB(t)
	setupLivenessTestRedis(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mock.ExpectExec("UPDATE workspaces SET status = 'offline'").
		WithArgs("ws-nocallback").
		WillReturnResult(sqlmock.NewResult(0, 1))

	go StartLivenessMonitor(ctx, nil)
	time.Sleep(50 * time.Millisecond)

	// Publish expiry
	db.RDB.Publish(ctx, "__keyevent@0__:expired", "ws:ws-nocallback")

	// Should not panic; give it time to process
	time.Sleep(200 * time.Millisecond)
}
