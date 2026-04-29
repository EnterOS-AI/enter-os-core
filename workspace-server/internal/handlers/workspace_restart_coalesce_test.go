package handlers

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetRestartStatesFor clears any state for the given workspaceID. Tests
// must call this between scenarios because restartStates is a package-level
// sync.Map shared across all RestartByID callers (incl. other tests in the
// suite).
func resetRestartStatesFor(workspaceID string) {
	restartStates.Delete(workspaceID)
}

// TestCoalesceRestart_SingleCallRunsOneCycle is the baseline:
// no concurrency, one cycle. If this fails the gate logic is broken at
// its simplest path.
func TestCoalesceRestart_SingleCallRunsOneCycle(t *testing.T) {
	const wsID = "test-coalesce-single"
	resetRestartStatesFor(wsID)

	var calls atomic.Int32
	coalesceRestart(wsID, func() { calls.Add(1) })

	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 cycle, got %d", got)
	}
}

// TestCoalesceRestart_ConcurrentCallsCoalesce verifies the bug fix: N
// concurrent requests during a slow cycle collapse to at most 2 cycles
// (the in-flight one + one more that picks up everyone who arrived
// during it). Reproduces the SetSecret + SetModel race that was
// silently dropping the second restart.
func TestCoalesceRestart_ConcurrentCallsCoalesce(t *testing.T) {
	const wsID = "test-coalesce-concurrent"
	resetRestartStatesFor(wsID)

	var calls atomic.Int32
	cycleStarted := make(chan struct{}, 1)
	cycleProceed := make(chan struct{})
	cycle := func() {
		n := calls.Add(1)
		if n == 1 {
			// First cycle blocks on cycleProceed, so we can fire the
			// "concurrent" requests during it. Subsequent cycles run
			// to completion immediately.
			cycleStarted <- struct{}{}
			<-cycleProceed
		}
	}

	// Kick off the first request in a goroutine so we can observe its
	// in-flight state.
	done := make(chan struct{})
	go func() {
		coalesceRestart(wsID, cycle)
		close(done)
	}()

	// Wait for the first cycle to actually be running before firing
	// the concurrent batch — otherwise we might call coalesceRestart
	// before running=true is set, defeating the test.
	<-cycleStarted

	// Fire 5 concurrent requests during the in-flight cycle. Each
	// should set pending=true and return immediately (no cycle run
	// from the goroutine's POV — the loop in goroutine #1 will pick
	// up the pending flag).
	const concurrentCount = 5
	var wg sync.WaitGroup
	for i := 0; i < concurrentCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			coalesceRestart(wsID, cycle)
		}()
	}
	wg.Wait()

	// At this point, exactly 1 cycle is running (the first one). The 5
	// concurrent requests all set pending=true and returned without
	// running their own cycle.
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 in-flight cycle so far, got %d (concurrent calls did not coalesce)", got)
	}

	// Release the first cycle. The goroutine's loop should pick up
	// pending=true and run exactly ONE more cycle, then exit.
	close(cycleProceed)
	<-done

	// Total cycles: 2 (the original + one coalesced follow-up for
	// all 5 concurrent requests). Anything more = wasted restarts;
	// anything less = lost requests.
	if got := calls.Load(); got != 2 {
		t.Errorf("expected exactly 2 cycles total (1 original + 1 coalesced follow-up for 5 concurrent calls), got %d", got)
	}
}

// TestCoalesceRestart_SequentialCallsRunSeparately verifies the gate
// doesn't over-coalesce: when calls don't overlap, each gets its own
// cycle. Important — the bug was dropping calls; the fix shouldn't
// over-correct by collapsing distinct sequential restarts.
func TestCoalesceRestart_SequentialCallsRunSeparately(t *testing.T) {
	const wsID = "test-coalesce-sequential"
	resetRestartStatesFor(wsID)

	var calls atomic.Int32
	for i := 0; i < 3; i++ {
		coalesceRestart(wsID, func() { calls.Add(1) })
	}

	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 cycles for 3 sequential calls, got %d", got)
	}
}

// TestCoalesceRestart_RequestDuringCyclePickedUp is the targeted
// reproduction of the original bug. One request is in flight; one more
// arrives during it. The fix must run a follow-up cycle so the second
// request's effects (e.g. a model write that committed after the first
// restart's secrets-load) are guaranteed to land in the next container.
func TestCoalesceRestart_RequestDuringCyclePickedUp(t *testing.T) {
	const wsID = "test-coalesce-pickup"
	resetRestartStatesFor(wsID)

	var calls atomic.Int32
	cycleStarted := make(chan struct{}, 1)
	cycleProceed := make(chan struct{})

	cycle := func() {
		n := calls.Add(1)
		if n == 1 {
			cycleStarted <- struct{}{}
			<-cycleProceed
		}
	}

	done := make(chan struct{})
	go func() {
		coalesceRestart(wsID, cycle)
		close(done)
	}()

	<-cycleStarted
	// One concurrent request — mimics SetModel arriving during
	// SetSecret's restart.
	coalesceRestart(wsID, cycle)
	close(cycleProceed)
	<-done

	if got := calls.Load(); got != 2 {
		t.Errorf("expected 2 cycles (1 original + 1 picked-up pending), got %d — the pending request was dropped, reverting the fix", got)
	}
}

// TestCoalesceRestart_StateClearedAfterDrain ensures the running flag
// is reset to false when the loop exits with pending=false, so a
// later restart request starts a fresh cycle instead of being
// permanently coalesced into nothing. Defends against a future edit
// that forgets to clear running.
func TestCoalesceRestart_StateClearedAfterDrain(t *testing.T) {
	const wsID = "test-coalesce-state-clear"
	resetRestartStatesFor(wsID)

	// First call: runs one cycle, drains, sets running=false.
	var calls1 atomic.Int32
	coalesceRestart(wsID, func() { calls1.Add(1) })

	// Second call (later, no overlap): must run its own cycle.
	var calls2 atomic.Int32
	coalesceRestart(wsID, func() { calls2.Add(1) })

	if got := calls1.Load(); got != 1 {
		t.Errorf("first call: expected 1 cycle, got %d", got)
	}
	if got := calls2.Load(); got != 1 {
		t.Errorf("second call: expected 1 cycle (state should reset between drains), got %d", got)
	}
}

// TestCoalesceRestart_PanicInCycleClearsState defends against a
// regression of the sticky-running deadlock: if cycle() panics, the
// running flag MUST be cleared so a follow-up RestartByID for the
// same workspace can still acquire the gate. Without the deferred
// state-clear + recover, a single panic would permanently lock a
// workspace out of all future restarts until process restart.
//
// Also asserts the panic is RECOVERED (not re-raised): callers are
// `go h.RestartByID(...)` from HTTP handlers, and an unrecovered
// goroutine panic in Go takes down the whole process. Crashing the
// platform for every tenant because one workspace's cycle panicked
// is the wrong availability tradeoff. The panic message + stack
// trace are still logged for debuggability.
func TestCoalesceRestart_PanicInCycleClearsState(t *testing.T) {
	const wsID = "test-coalesce-panic-recovery"
	resetRestartStatesFor(wsID)

	// First call's cycle panics. coalesceRestart's defer must swallow
	// the panic so this test caller doesn't see it propagate up — that
	// matches what the real production caller (`go h.RestartByID(...)`)
	// gets: the goroutine survives, no process crash.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panic should NOT propagate out of coalesceRestart (would crash the platform process from a goroutine), got: %v", r)
		}
	}()
	coalesceRestart(wsID, func() { panic("simulated cycle failure") })

	// Second call must run a fresh cycle. If running stayed true after
	// the panic, this call would early-return without invoking cycle.
	var ran atomic.Bool
	coalesceRestart(wsID, func() { ran.Store(true) })
	if !ran.Load() {
		t.Error("post-panic restart was blocked — running flag leaked, workspace permanently locked out")
	}
}

// TestCoalesceRestart_DifferentWorkspacesDoNotSerialize verifies the
// per-workspace state map: an in-flight restart for ws A must not
// block restarts for ws B. Important for performance — without this,
// unrelated workspaces would queue behind each other.
func TestCoalesceRestart_DifferentWorkspacesDoNotSerialize(t *testing.T) {
	const wsA = "test-coalesce-ws-a"
	const wsB = "test-coalesce-ws-b"
	resetRestartStatesFor(wsA)
	resetRestartStatesFor(wsB)

	aStarted := make(chan struct{}, 1)
	aProceed := make(chan struct{})
	var aCycles atomic.Int32
	var bCycles atomic.Int32

	doneA := make(chan struct{})
	go func() {
		coalesceRestart(wsA, func() {
			aCycles.Add(1)
			aStarted <- struct{}{}
			<-aProceed
		})
		close(doneA)
	}()

	<-aStarted
	// While A is in flight, B's restart must be free to run.
	bDone := make(chan struct{})
	go func() {
		coalesceRestart(wsB, func() { bCycles.Add(1) })
		close(bDone)
	}()

	select {
	case <-bDone:
		// Correct — B did not block on A.
	case <-time.After(2 * time.Second):
		t.Fatal("B's restart blocked on A's — per-workspace state isolation is broken")
	}

	close(aProceed)
	<-doneA

	if got := aCycles.Load(); got != 1 {
		t.Errorf("A: expected 1 cycle, got %d", got)
	}
	if got := bCycles.Load(); got != 1 {
		t.Errorf("B: expected 1 cycle, got %d", got)
	}
}
