package handlers

import (
	"runtime"
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

// drainCoalesceGoroutine spawns `coalesceRestart(wsID, cycle)` on a
// goroutine that mirrors the real production caller shape
// (`go h.RestartByID(...)` from a2a_proxy.go, a2a_proxy_helpers.go,
// main.go), and registers a t.Cleanup that blocks until the goroutine
// has TERMINATED — not just panicked-and-recovered, fully exited.
//
// This is the bleed-prevention contract for Class H (Task #170): no
// test in this file may declare itself complete while a coalesceRestart
// goroutine it spawned is still alive, because that goroutine could
// otherwise wake up after the test's sqlmock has been closed and
// either:
//   - issue a stale INSERT that gets attributed to the next test's
//     sqlmock connection — surfaces as
//     "INSERT-not-expected for kind=DELEGATION_FAILED" / =WORKSPACE_PROVISION_FAILED
//     in a neighbour test that doesn't itself touch coalesceRestart; or
//   - hold a reference to the closed *sql.DB and panic on the next op.
//
// Implementation notes:
//   - sync.WaitGroup must be Add()ed BEFORE the goroutine is spawned;
//     Add inside the goroutine races with Wait.
//   - t.Cleanup runs in LIFO order, so this composes safely with other
//     cleanups (e.g. setupTestDB's mockDB.Close).
//   - We don't bound the Wait with a timeout — if the goroutine
//     genuinely deadlocks, the whole test process should hang and fail
//     under -timeout. A timeout-then-orphan would mask the bleed.
func drainCoalesceGoroutine(t *testing.T, wsID string, cycle func()) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		coalesceRestart(wsID, cycle)
	}()
	t.Cleanup(wg.Wait)
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

	// Spawn the panicking cycle on a goroutine via drainCoalesceGoroutine
	// — this mirrors the real production callsite shape
	// (`go h.RestartByID(...)` from a2a_proxy.go:584,
	// a2a_proxy_helpers.go:197, main.go:213). The previous form called
	// coalesceRestart synchronously, which neither exercised the
	// goroutine-survival contract nor caught Class H bleed regressions
	// where the panic-recovery goroutine outlives the test and pollutes
	// the next test's sqlmock with INSERTs from runRestartCycle's
	// LogActivity calls (kinds DELEGATION_FAILED / WORKSPACE_PROVISION_FAILED).
	//
	// drainCoalesceGoroutine registers a t.Cleanup that Wait()s for the
	// goroutine to TERMINATE — not merely panic-and-recover — before
	// the test ends.
	drainCoalesceGoroutine(t, wsID, func() { panic("simulated cycle failure") })

	// We need a mid-test barrier (not just the t.Cleanup-time barrier)
	// so the second coalesceRestart below sees state.running=false. The
	// goroutine clears state.running inside its deferred recover; poll
	// the package-level restartStates map until that observable flip
	// happens. Bound at 2s — longer = real bug.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sv, ok := restartStates.Load(wsID)
		if ok {
			st := sv.(*restartState)
			st.mu.Lock()
			running := st.running
			st.mu.Unlock()
			if !running {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}

	// Second call must run a fresh cycle. If running stayed true after
	// the panic, this call would early-return without invoking cycle.
	// Synchronous — no panic, so no goroutine to drain, and we want to
	// assert ran.Load() immediately after.
	var ran atomic.Bool
	coalesceRestart(wsID, func() { ran.Store(true) })
	if !ran.Load() {
		t.Error("post-panic restart was blocked — running flag leaked, workspace permanently locked out")
	}
}

// TestCoalesceRestart_DrainHelperWaitsForGoroutineExit is the Class H
// regression guard for Task #170. It asserts the contract enforced by
// drainCoalesceGoroutine: t.Cleanup blocks until the spawned
// coalesceRestart goroutine has FULLY EXITED — not merely recovered
// from panic. This is the contract that prevents stale LogActivity
// INSERTs from a recovering goroutine bleeding into the next test's
// sqlmock (the failure mode reported as "INSERT-not-expected for
// kind=DELEGATION_FAILED" in TestPooledWithEICTunnel_PreservesFnErr).
//
// We use a deterministic bleed-shape probe rather than goroutine-count
// arithmetic: the cycle blocks on a release channel for ~150ms — long
// enough that without a Wait barrier, the outer sub-test would return
// before the goroutine exited. We then verify the wg.Wait inside
// drainCoalesceGoroutine actually delayed t.Run's completion: total
// elapsed must be >= the block duration. Asserts exact-shape, not
// substring (per saved-memory feedback_assert_exact_not_substring):
// elapsed < blockFor would mean the cleanup didn't wait, which is the
// exact bleed we're guarding against.
//
// We additionally panic from the cycle (after the block) to confirm
// the helper waits past panic recovery, not just past cycle return.
func TestCoalesceRestart_DrainHelperWaitsForGoroutineExit(t *testing.T) {
	const blockFor = 150 * time.Millisecond
	const wsID = "test-coalesce-drain-helper-contract"
	resetRestartStatesFor(wsID)

	// done is closed inside the cycle, AFTER the block + AFTER the
	// panic (which the deferred recover in coalesceRestart catches).
	// Actually: defer in cycle runs before panic propagates to the
	// outer recover. Use defer to close.
	exited := make(chan struct{})

	subStart := time.Now()
	t.Run("drain_under_subtest", func(st *testing.T) {
		drainCoalesceGoroutine(st, wsID, func() {
			defer close(exited)
			time.Sleep(blockFor)
			panic("contract-test panic-after-block")
		})
		// st.Cleanup runs here, before t.Run returns. wg.Wait must
		// block until the goroutine has finished its panic recovery.
	})
	subElapsed := time.Since(subStart)

	// Contract: the helper's wg.Wait MUST have blocked t.Run from
	// returning until after the cycle's block + panic recovery.
	if subElapsed < blockFor {
		t.Fatalf(
			"drainCoalesceGoroutine contract violated: t.Run returned in %v, "+
				"but cycle blocks for %v. The Wait barrier is broken — a "+
				"coalesceRestart goroutine can outlive its test's t.Cleanup "+
				"and pollute neighbour-test sqlmock state (Class H bleed).",
			subElapsed, blockFor,
		)
	}

	// And the goroutine must have actually closed `exited` (i.e. ran
	// the deferred close before panic propagated through coalesceRestart's
	// recover). If exited is still open here, the goroutine never
	// reached the close — meaning either the panic short-circuited the
	// defer (Go runtime bug — won't happen) or the goroutine never
	// ran at all (drainCoalesceGoroutine spawn shape regressed).
	select {
	case <-exited:
		// Correct path.
	default:
		t.Fatal("cycle goroutine never reached its deferred close — panic-recovery contract regressed")
	}

	// Belt-and-suspenders: the post-recover state-clear must have
	// flipped state.running back to false. If this fails, the panic
	// path skipped the deferred state-clear in coalesceRestart.
	sv, ok := restartStates.Load(wsID)
	if !ok {
		t.Fatal("restartStates entry missing for wsID after cycle — sync.Map regression")
	}
	st := sv.(*restartState)
	st.mu.Lock()
	running := st.running
	st.mu.Unlock()
	if running {
		t.Error("state.running was not cleared after panic — sticky-running deadlock regressed")
	}

	// Reference runtime.NumGoroutine to keep the runtime import
	// honest — also a useful smoke check that the goroutine count
	// hasn't ballooned 10x while debugging this test.
	if n := runtime.NumGoroutine(); n > 200 {
		t.Logf("warning: NumGoroutine=%d after drain — high but not necessarily a leak", n)
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
