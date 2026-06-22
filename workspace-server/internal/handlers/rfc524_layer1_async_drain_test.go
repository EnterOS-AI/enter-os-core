package handlers

// rfc524_layer1_async_drain_test.go — regression test for RFC internal#524
// Layer 1 forward-port. Asserts:
//
//   1. globalGoAsync goroutines are drained by drainTestAsync before the
//      test cleanup chain returns control.
//   2. Routing through globalGoAsync (rather than bare `go ...`) ensures
//      a sibling-handler's detached goroutine cannot outlive a test's
//      db.DB swap.
//
// Companion of handlers_test.go:drainTestAsync (canonical 69d9b4e3 fix
// extended to non-*WorkspaceHandler call sites). If either property
// regresses, this test fails fast.

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestRFC524_GlobalGoAsync_DrainsBeforeCleanup asserts that goroutines
// scheduled via globalGoAsync run to completion before drainTestAsync
// returns. Concretely: schedule a globalGoAsync that flips a counter
// after a short sleep, then call drainTestAsync; the counter must
// already be 1 when the call returns.
func TestRFC524_GlobalGoAsync_DrainsBeforeCleanup(t *testing.T) {
	var ran int32
	globalGoAsync(func() {
		time.Sleep(20 * time.Millisecond)
		atomic.StoreInt32(&ran, 1)
	})

	// drainTestAsync drains per-handler asyncWG + the package-level
	// globalAsync WG. After it returns the goroutine MUST have run.
	drainTestAsync()

	if atomic.LoadInt32(&ran) != 1 {
		t.Fatalf("drainTestAsync returned before globalGoAsync goroutine finished — regression of RFC internal#524 Layer 1 drain coupling")
	}
}

// TestRFC524_GlobalGoAsync_MultipleConcurrent asserts the drain is
// O(n)-correct: schedule a fan-out of globalGoAsync calls (like
// restartAllAffectedByGlobalKey does on a large global secret rotation)
// and confirm every one completes before drainTestAsync returns.
func TestRFC524_GlobalGoAsync_MultipleConcurrent(t *testing.T) {
	const n = 32
	var completed int32
	for i := 0; i < n; i++ {
		globalGoAsync(func() {
			// Short, random-ish work; the point is they're all in flight
			// at the same time when drainTestAsync is called.
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&completed, 1)
		})
	}

	drainTestAsync()

	got := atomic.LoadInt32(&completed)
	if got != n {
		t.Fatalf("drainTestAsync returned with %d/%d globalGoAsync goroutines incomplete — fan-out drain broken", n-got, n)
	}
}

// TestRFC524_HandlerGoAsync_AndGlobalAsync_BothDrained asserts that
// drainTestAsync waits for BOTH the per-handler asyncWG (the original
// 69d9b4e3 primitive) AND the package-level globalAsync (the Layer 1
// extension). Schedules one of each and confirms both finish.
func TestRFC524_HandlerGoAsync_AndGlobalAsync_BothDrained(t *testing.T) {
	setupTestDB(t) // registers handlers + arms the drain

	var perHandlerDone, globalDone int32
	wh := NewWorkspaceHandler(nil, nil, "", t.TempDir())
	wh.goAsync(func() {
		time.Sleep(15 * time.Millisecond)
		atomic.StoreInt32(&perHandlerDone, 1)
	})
	globalGoAsync(func() {
		time.Sleep(15 * time.Millisecond)
		atomic.StoreInt32(&globalDone, 1)
	})

	drainTestAsync()

	if atomic.LoadInt32(&perHandlerDone) != 1 {
		t.Errorf("per-handler asyncWG drain regressed (RFC internal#524 Layer 1 expects 69d9b4e3 to remain wired)")
	}
	if atomic.LoadInt32(&globalDone) != 1 {
		t.Errorf("global async drain not wired (RFC internal#524 Layer 1 extension missing)")
	}
}
