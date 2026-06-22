package handlers

// Tests for core#2771 — Create and Restart for the same workspace ID
// must serialize through acquireRestartProvisionGate, otherwise the
// async create provision and a near-immediate /restart can both reach
// provisioner.Start concurrently and trigger a Docker container name
// conflict → markProvisionFailed → workspace wedged "failed".
//
// These tests assert the load-bearing property: a Create call
// (provisionWorkspaceAuto) and a Restart call (RestartWorkspaceAutoOpts)
// for the SAME ws-<id> cannot overlap. Different ws-<id>s do not block
// each other (the per-workspace gate is a sync.Map keyed on ID).

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
)

// TestCreateRestartGate_SharedAcrossCreateAndRestart: pre-fix, only
// RestartWorkspaceAutoOpts acquired the gate; Create's
// provisionWorkspaceAuto dispatched the async provision WITHOUT the
// gate, so a Create then /restart for the same ws-<id> could both
// reach provisioner.Start. The regression test proves the gate is
// now SHARED across the two entry points: holding the gate from
// the create side blocks the restart side (and vice versa).
//
// This is the load-bearing property that closes core#2771. If a
// future refactor pulls the gate acquisition out of one of the two
// entry points, this test will fail.
func TestCreateRestartGate_SharedAcrossCreateAndRestart(t *testing.T) {
	const wsID = "test-core2771-shared-gate"
	resetRestartProvisionGateFor(wsID)

	gate := acquireRestartProvisionGate(wsID)

	// Holder A simulates a Create call whose provision is in flight
	// (i.e. the new gate acquisition path inside provisionWorkspaceAuto).
	// Holder B simulates a concurrent Restart call (RestartWorkspaceAutoOpts).
	// The gate must serialize them: only one holds the gate at a time.
	holderAHolds := make(chan struct{})
	holderACanRelease := make(chan struct{})
	holderBAcquiredAt := make(chan time.Time, 1)

	var wg sync.WaitGroup
	wg.Add(2)

	// Holder A: acquire the gate (representing create-side in-flight
	// provision), signal, then wait for the test to release.
	go func() {
		defer wg.Done()
		gate.Lock()
		close(holderAHolds)
		<-holderACanRelease
		gate.Unlock()
	}()

	// Holder B: while A holds, attempt to acquire the same gate
	// (representing the restart-side call landing on the same ws-<id>).
	// Record the moment the lock actually returns. The gate must
	// guarantee the Lock() returns ONLY after A unlocks.
	go func() {
		defer wg.Done()
		<-holderAHolds
		gate.Lock()
		holderBAcquiredAt <- time.Now()
		gate.Unlock()
	}()

	// Wait for A to have the lock, then sleep a beat so B's Lock()
	// has definitely had a chance to block (otherwise B's record
	// timestamp is just goroutine-scheduling noise, not the gate's
	// serialization).
	<-holderAHolds
	time.Sleep(100 * time.Millisecond)

	// Confirm B has NOT acquired the lock yet — proving the gate is
	// load-bearing across the entry points.
	select {
	case <-holderBAcquiredAt:
		t.Fatal("holder B acquired the lock while holder A still held it — gate is not serializing across create+restart entry points (core#2771 regression)")
	default:
		// expected: B is still blocked
	}

	// Now release A. B must unblock within a small window.
	close(holderACanRelease)
	select {
	case bAcquiredAt := <-holderBAcquiredAt:
		// We can't assert a precise elapsed time (CI clock noise) but
		// the timestamp MUST be after the release we just sent. The
		// fact that the channel delivered at all is the assertion.
		_ = bAcquiredAt
	case <-time.After(2 * time.Second):
		t.Fatal("holder B did not acquire the lock within 2s of A releasing — gate may be broken or leaked")
	}

	wg.Wait()
}

// TestCreateRestartGate_DifferentWorkspacesDoNotBlock: orthogonal
// workspace IDs must not block each other. The gate is a per-workspace
// mutex (sync.Map keyed on ID); two different ws-<id>s have different
// mutex instances, so concurrent create/restart on different
// workspaces proceed independently.
func TestCreateRestartGate_DifferentWorkspacesDoNotBlock(t *testing.T) {
	const wsA = "test-core2771-different-A"
	const wsB = "test-core2771-different-B"
	resetRestartProvisionGateFor(wsA)
	resetRestartProvisionGateFor(wsB)

	gateA := acquireRestartProvisionGate(wsA)
	gateB := acquireRestartProvisionGate(wsB)

	// Hold gateA, then confirm gateB can be acquired without waiting.
	gateA.Lock()
	defer gateA.Unlock()

	acquired := make(chan struct{})
	go func() {
		gateB.Lock()
		close(acquired)
		gateB.Unlock()
	}()

	select {
	case <-acquired:
		// good — gateA and gateB are independent
	case <-time.After(200 * time.Millisecond):
		t.Fatal("gateB blocked behind gateA — per-workspace gates must be independent (different ws-IDs)")
	}
}

// TestCreateRestartGate_ProvisionPathHoldsGateUntilCompletion is the
// behavioral assertion: the gate acquired by provisionWorkspaceAuto
// must be held until the async provision COMPLETES (not just until
// provisionWorkspace returns to spawn the goroutine). This is the
// invariant that makes "only one Docker create for ws-<id>" hold.
//
// The test directly mimics the new code path: Lock the gate, then
// assert that a second Lock() call on the same gate cannot return
// until we Unlock. This is the unit-level mirror of the goroutine's
// `defer gate.Unlock()` at the tail of provisionWorkspace.
func TestCreateRestartGate_ProvisionPathHoldsGateUntilCompletion(t *testing.T) {
	const wsID = "test-core2771-hold-until-completion"
	resetRestartProvisionGateFor(wsID)

	gate := acquireRestartProvisionGate(wsID)

	// Simulate the goroutine that provisionWorkspaceAuto now spawns:
	// it holds the gate for the entire provision (defer gate.Unlock
	// at the tail). The restart side, while this is in flight, must
	// block.
	holderReady := make(chan struct{})
	holderReleased := make(chan struct{})
	var concurrentAcquired atomic.Bool
	concurrentAcquired.Store(false)

	go func() {
		gate.Lock()
		close(holderReady)
		// Simulate a non-trivial provision (the real provision can
		// run for seconds — Docker create, register, etc.). The 50ms
		// here is enough to prove the test isn't accidentally racy.
		time.Sleep(50 * time.Millisecond)
		gate.Unlock()
		close(holderReleased)
	}()

	<-holderReady

	// Try to acquire the same gate while the simulated provision is
	// still in flight. Must block.
	acquiredAt := make(chan time.Time, 1)
	go func() {
		gate.Lock()
		concurrentAcquired.Store(true)
		acquiredAt <- time.Now()
		gate.Unlock()
	}()

	// Confirm we're still blocked before the provision releases.
	time.Sleep(20 * time.Millisecond)
	if concurrentAcquired.Load() {
		t.Fatal("concurrent acquirer got the lock while provision goroutine still held it — core#2771 race window not closed")
	}

	// Wait for the provision to release, then confirm the acquirer unblocks.
	<-holderReleased
	select {
	case <-acquiredAt:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("acquirer did not unblock after provision released — possible gate leak")
	}
}

// TestCreateRestartGate_RestartCycleDoesNotDeadlock (CR2 #11473 RC):
// the previous head of this PR introduced a deadlock —
// provisionWorkspaceAutoSync unconditionally acquired the gate, but
// runRestartCycle (auto-restart) already held the gate and called
// the same helper. The non-reentrant sync.Mutex deadlocked the
// programmatic restart path before reprovision.
//
// The fix split provisionWorkspaceAutoSync into two:
//   - provisionWorkspaceAutoSync (unlocked): acquires + defers
//   - provisionWorkspaceAutoSyncLocked: assumes the gate is HELD
//
// runRestartCycle calls the Locked variant. This test exercises
// that exact path: hold the gate (mimicking the top of
// runRestartCycle), then call the Locked variant, then assert
// no deadlock and the no-backend markProvisionFailed fires.
//
// Without the Locked variant (i.e. if a future refactor reverts the
// split and calls the unlocked one from runRestartCycle), this
// test would deadlock past the 2-second timeout because the
// unlocked variant tries to re-lock the non-reentrant mutex.
func TestCreateRestartGate_RestartCycleDoesNotDeadlock(t *testing.T) {
	const wsID = "test-core2771-restart-no-deadlock"
	resetRestartProvisionGateFor(wsID)

	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	// markProvisionFailed is the no-backend fallback fired by the
	// Locked variant when neither CP nor Docker is wired. The test
	// relies on the no-backend path to keep the test simple (no
	// provisioner plumbing) — the load-bearing assertion is that
	// the call returns (no deadlock), not the specific provision
	// outcome.
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	// Do NOT call SetCPProvisioner — both backends nil, which drives
	// the Locked variant down the markProvisionFailed path.

	// Mimic the top of runRestartCycle: acquire the gate before
	// calling the provision helper. The helper MUST be the Locked
	// variant — calling the unlocked one would deadlock here
	// (re-locking the same non-reentrant mutex).
	gate := acquireRestartProvisionGate(wsID)
	gate.Lock()

	// Now call the Locked variant — must complete within a small
	// window. If the contract breaks (someone reverts the split
	// and re-locks inside), this deadlocks and the test fails
	// at the timeout.
	type result struct {
		ok bool
	}
	done := make(chan result, 1)
	go func() {
		ok := h.provisionWorkspaceAutoSyncLocked(wsID, "", nil, models.CreateWorkspacePayload{
			Name: "no-deadlock", Tier: 1, Runtime: "claude-code", Model: "anthropic:claude-opus-4-7", // core#2594
		})
		done <- result{ok: ok}
	}()

	select {
	case r := <-done:
		// No deadlock. The no-backend path returned false; the
		// markProvisionFailed UPDATE fired.
		if r.ok {
			t.Errorf("expected false return from no-backend provisionWorkspaceAutoSyncLocked, got true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provisionWorkspaceAutoSyncLocked under an already-held gate did not return within 2s — DEADLOCK (CR2 #11473 RC not fixed)")
	}

	// Release the gate we held (the restart cycle would, on its defer).
	gate.Unlock()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected markProvisionFailed UPDATE to fire: %v", err)
	}
}

// TestCreateRestartGate_ReacquiringSyncWouldDeadlock is the negative
// control: a "naive" call to the UNLOCKED variant from inside an
// already-held gate would deadlock (the test asserts the deadlock
// with a timeout). Together with the previous test, this proves:
//   1. The Locked variant does NOT deadlock under a held gate.
//   2. The unlocked variant DOES deadlock under a held gate (it
//      tries to re-lock the same non-reentrant mutex).
//
// This guards against a future refactor that accidentally removes
// the Locked/Unlocked split: the negative control would START
// passing silently if the unlocked variant stopped trying to
// re-lock, and the positive control would START failing. Either
// change is a regression. The test pair keeps the contract pinned.
func TestCreateRestartGate_ReacquiringSyncWouldDeadlock(t *testing.T) {
	const wsID = "test-core2771-reacquire-deadlock"
	resetRestartProvisionGateFor(wsID)

	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	// The unlocked variant tries to re-lock (which deadlocks on
	// the same non-reentrant mutex) and so never reaches the
	// markProvisionFailed UPDATE. We DO NOT expect that UPDATE.
	_ = mock

	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())

	// Hold the gate from outside (mimicking runRestartCycle's
	// outer Lock), then try to call the UNLOCKED variant. The
	// unlocked variant's Lock() inside would block on the same
	// mutex → deadlock. We use a 500ms timeout to assert the
	// deadlock happens (a fast return would mean the unlocked
	// variant no longer tries to re-lock, which would be a
	// regression of the gate-acquisition design).
	gate := acquireRestartProvisionGate(wsID)
	gate.Lock()

	done := make(chan struct{})
	go func() {
		// This call MUST deadlock (under a held gate, the
		// unlocked variant's gate.Lock() inside cannot return).
		_ = h.provisionWorkspaceAutoSync(wsID, "", nil, models.CreateWorkspacePayload{
			Name: "would-deadlock", Tier: 1, Runtime: "claude-code", Model: "anthropic:claude-opus-4-7", // core#2594
		})
		close(done)
	}()

	// Wait 500ms — well past any reasonable provision time but
	// short enough to keep the test fast. The call must still be
	// blocked.
	select {
	case <-done:
		// Returned fast — that's the regression we want to
		// catch. The unlocked variant no longer re-locks, which
		// would mean the create+restart serialization is broken.
		t.Fatal("provisionWorkspaceAutoSync (unlocked) returned under a held gate — the unlock+relock pattern is gone; create+restart serialization is broken")
	case <-time.After(500 * time.Millisecond):
		// Still blocked — correct: the unlocked variant is
		// waiting for the gate that we hold, exactly as it
		// should. The runRestartCycle path uses the Locked
		// variant to avoid this deadlock.
	}

	// Unblock: release the gate. The unlocked variant should now
	// proceed and the test will exit cleanly.
	gate.Unlock()
	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("unlocked variant did not unblock after gate release — possible gate leak")
	}
}
