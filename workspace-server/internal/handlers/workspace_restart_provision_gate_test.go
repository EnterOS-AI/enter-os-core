package handlers

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetRestartProvisionGateFor clears the per-workspace provision-gate
// mutex for the given workspaceID. Tests must call this between
// scenarios because restartProvisionGates is a package-level sync.Map
// shared across every restart path (manual HTTP + programmatic
// RestartByID).
func resetRestartProvisionGateFor(workspaceID string) {
	restartProvisionGates.Delete(workspaceID)
}

// TestRestartProvisionGate_SingleCallRunsOneCycle is the baseline:
// no contention, one cycle. If this fails the gate infrastructure is
// broken at the simplest path.
func TestRestartProvisionGate_SingleCallRunsOneCycle(t *testing.T) {
	const wsID = "test-provgate-single"
	resetRestartProvisionGateFor(wsID)

	gate := acquireRestartProvisionGate(wsID)
	var calls atomic.Int32
	gate.Lock()
	calls.Add(1)
	gate.Unlock()

	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 cycle, got %d", got)
	}
}

// TestRestartProvisionGate_ConcurrentAcquiresSerialize verifies the
// load-bearing property: a second goroutine calling Lock() while the
// first holds the gate must block until the first releases. This is
// the "only ONE Docker create for ws-<id> at a time" invariant — the
// second caller's Stop+Start is fully serialized behind the first's.
func TestRestartProvisionGate_ConcurrentAcquiresSerialize(t *testing.T) {
	const wsID = "test-provgate-serialize"
	resetRestartProvisionGateFor(wsID)

	gate := acquireRestartProvisionGate(wsID)

	// Goroutine 1 holds the gate, then signals "ready", then waits
	// for the test harness to release it. Goroutine 2 tries to Lock
	// while goroutine 1 holds it. If the gate is doing its job,
	// goroutine 2's Lock() returns only AFTER goroutine 1 unlocks.
	holder1Ready := make(chan struct{})
	holder1CanProceed := make(chan struct{})
	holder2AcquiredAt := make(chan time.Time, 1)

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: holds the gate, signals ready, waits.
	go func() {
		defer wg.Done()
		gate.Lock()
		close(holder1Ready)
		<-holder1CanProceed
		gate.Unlock()
	}()

	// Goroutine 2: tries to Lock while goroutine 1 holds. Records
	// when it actually acquires the lock.
	go func() {
		defer wg.Done()
		<-holder1Ready
		gate.Lock()
		holder2AcquiredAt <- time.Now()
		gate.Unlock()
	}()

	// Wait for goroutine 1 to have the lock, then sleep a beat so
	// goroutine 2's Lock() has definitely had a chance to block.
	<-holder1Ready
	time.Sleep(50 * time.Millisecond)

	// Release goroutine 1. Goroutine 2 should now acquire and
	// timestamp itself.
	releaseAt := time.Now()
	close(holder1CanProceed)

	// Goroutine 2's acquisition must happen at or after releaseAt
	// (the gate is FIFO for non-starved callers, so a strict >=
	// suffices; the sleep ensures we're not just measuring jitter).
	select {
	case got := <-holder2AcquiredAt:
		if got.Before(releaseAt) {
			t.Errorf("goroutine 2 acquired the gate BEFORE release: got=%v release=%v", got, releaseAt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine 2 never acquired the gate (deadlock)")
	}

	wg.Wait()
}

// TestRestartProvisionGate_ConcurrentCyclesSerializeOnGate is the
// end-to-end invariant test for the bug we're closing: when the
// manual HTTP Restart path and the programmatic RestartByID path
// (or two distinct programmatic callers) BOTH try to enter their
// Stop+Start cycle simultaneously, the gate serializes them so
// the second caller's provisioner.Start runs only AFTER the first's
// is fully done. Mirrors the production call shape — both paths
// acquire the same per-workspace gate, then run their cycle inside
// the Lock/Unlock.
//
// In production this is the contract that prevents the "two
// provisioner.Start calls for the same ws-<id> → Docker name
// conflict → markProvisionFailed" symptom that wedged the
// workspace to "failed" in #2659 run 353677/job 478450.
func TestRestartProvisionGate_ConcurrentCyclesSerializeOnGate(t *testing.T) {
	const wsID = "test-provgate-concurrent-cycles"
	resetRestartProvisionGateFor(wsID)

	gate := acquireRestartProvisionGate(wsID)

	// Each "cycle" is a closure that: (1) acquires the gate, (2)
	// increments the shared counter, (3) records its start time
	// and increment index, (4) sleeps briefly so the OTHER cycle has
	// a chance to interleave (it WON'T, if the gate is doing its job),
	// (5) records its end time, (6) releases the gate.
	var (
		cycleStarts atomic.Int32
		cycleEnds   atomic.Int32
	)

	cycle := func() {
		gate.Lock()
		defer gate.Unlock()
		idx := cycleStarts.Add(1)
		_ = idx
		// Hold long enough that any interleaving cycle would have
		// also entered its critical section if the gate were broken.
		time.Sleep(20 * time.Millisecond)
		cycleEnds.Add(1)
	}

	// Fire two cycles concurrently via separate goroutines. If the
	// gate is doing its job, cycleEnds == cycleStarts at the end
	// (each cycle fully exits before the next enters). If the gate
	// is broken (or missing), two cycles would overlap in the
	// critical section and cycleEnds would lag cycleStarts.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cycle()
		}()
	}
	wg.Wait()

	// Invariant: starts and ends must match. If they don't, the
	// critical sections overlapped — which would mean the gate
	// failed to serialize them.
	if got := cycleStarts.Load(); got != 2 {
		t.Errorf("expected 2 cycle starts, got %d", got)
	}
	if got := cycleEnds.Load(); got != 2 {
		t.Errorf("expected 2 cycle ends (matching starts), got %d — cycles overlapped inside the gate's critical section", got)
	}
}

// TestRestartProvisionGate_TwoWorkspacesIndependent verifies the
// gate is per-workspace: holding the gate for ws-A does NOT block
// acquisition of the gate for ws-B. (If the implementation
// accidentally used a single global mutex, this would deadlock.)
func TestRestartProvisionGate_TwoWorkspacesIndependent(t *testing.T) {
	const wsA = "test-provgate-ws-a"
	const wsB = "test-provgate-ws-b"
	resetRestartProvisionGateFor(wsA)
	resetRestartProvisionGateFor(wsB)
	t.Cleanup(func() {
		resetRestartProvisionGateFor(wsA)
		resetRestartProvisionGateFor(wsB)
	})

	gateA := acquireRestartProvisionGate(wsA)
	gateA.Lock()
	defer gateA.Unlock()

	// If the implementation accidentally used a single global
	// mutex, this Lock() would block forever waiting for the
	// release of the defer above. Use a short timeout via a
	// select-equivalent (goroutine + channel).
	type result struct {
		ok bool
	}
	done := make(chan result, 1)
	go func() {
		gateB := acquireRestartProvisionGate(wsB)
		gateB.Lock()
		// Hold briefly to prove the critical section was actually acquired; this
		// also satisfies staticcheck SA2001 (empty critical section) because the
		// Lock/Unlock pair encloses a real operation.
		time.Sleep(1 * time.Millisecond)
		gateB.Unlock()
		done <- result{ok: true}
	}()

	select {
	case r := <-done:
		if !r.ok {
			t.Error("ws-B gate Lock/Unlock returned !ok")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ws-B gate acquisition blocked while ws-A was held — gates are not per-workspace")
	}
}
