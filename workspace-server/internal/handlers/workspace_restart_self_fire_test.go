package handlers

// Tests for the 2026-05-19 ws-server self-fire restart feedback loop fix.
//
// Empirical chain reproduced (prod-Reviewer/Researcher 4x reprov thrash
// 2026-05-19 ~00:05-00:09Z, root-caused via Loki):
//
//  1. POST /secrets → go h.restartFunc(workspaceID) (secrets.go:264).
//  2. runRestartCycle sets url='' synchronously, then async provisions EC2
//     (workspace_restart.go).
//  3. During 20-30s window while EC2 is `pending` (codex first heartbeat
//     not yet landed): workspaces.url='' AND IsRunning=false.
//  4. Any ProxyA2A (canvas /delegations poll OR the restart-context probe
//     at the end of runRestartCycle) → maybeMarkContainerDead sees the
//     container-dead state → calls RestartByID → loop.
//  5. coalesceRestart sets pending=true, drains by running ANOTHER full
//     cycle → provision.ec2_stopped of the just-booted instance →
//     re-provision.
//
// Fix: three interdependent layers.
//
//  L1) isRestarting() gate in maybeMarkContainerDead +
//      preflightContainerHealth — early-return false/nil so the probe
//      can't trigger a fresh RestartByID while a restart is in flight.
//  L2) sendRestartContext requires url != '' AND last_heartbeat_at >
//      restart_start_ts before firing the trailing ProxyA2A probe.
//  L3) RestartByID silently drops successive calls within
//      restartDebounceWindow of restartStartedAt, with a counter for
//      observability.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// resetSelfFireState wipes all the per-workspace mutation state these
// tests touch, plus the package-level drop counter, so the test is
// hermetic regardless of ordering.
func resetSelfFireState(workspaceID string) {
	restartStates.Delete(workspaceID)
	restartByIDDropCounter.Store(0)
}

// markRestarting forces restartStates into "cycle in flight" without
// running an actual cycle, so the tests can isolate the gate behaviour
// without the full provision pipeline. Returns a finish() that flips
// running=false (mimicking coalesceRestart's deferred state-clear).
func markRestarting(workspaceID string) (finish func()) {
	sv, _ := restartStates.LoadOrStore(workspaceID, &restartState{})
	state := sv.(*restartState)
	state.mu.Lock()
	state.running = true
	state.restartStartedAt = time.Now()
	state.mu.Unlock()
	return func() {
		state.mu.Lock()
		state.running = false
		state.mu.Unlock()
	}
}

// TestIsRestarting_FalseWhenNoStateEntry — baseline: a workspace that
// has never been restarted reports !isRestarting. Pinning this so a
// future LoadOrStore refactor can't silently start returning true for
// unknown workspaces.
func TestIsRestarting_FalseWhenNoStateEntry(t *testing.T) {
	const wsID = "self-fire-ws-never"
	resetSelfFireState(wsID)
	if isRestarting(wsID) {
		t.Fatal("isRestarting must return false for a workspace with no state entry")
	}
}

// TestIsRestarting_TrueWhileCycleRunning — the load-bearing invariant
// that Layer 1 depends on. While running=true, isRestarting must report
// true; the moment it flips to false, isRestarting must report false.
func TestIsRestarting_TrueWhileCycleRunning(t *testing.T) {
	const wsID = "self-fire-ws-in-flight"
	resetSelfFireState(wsID)

	finish := markRestarting(wsID)
	if !isRestarting(wsID) {
		t.Fatal("isRestarting must return true while running=true")
	}
	finish()
	if isRestarting(wsID) {
		t.Fatal("isRestarting must return false after running flips back to false")
	}
}

// TestMaybeMarkContainerDead_SkippedWhileRestarting — Layer 1 for the
// reactive path. With isRestarting=true the function must early-return
// false WITHOUT invoking IsRunning, hitting the DB UPDATE, or kicking
// a RestartByID goroutine. If any of those side-effects fire we'd
// re-arm the self-fire loop the gate exists to close.
func TestMaybeMarkContainerDead_SkippedWhileRestarting(t *testing.T) {
	const wsID = "self-fire-ws-mmcd"
	resetSelfFireState(wsID)
	mock := setupTestDB(t) // sqlmock with strict expectation matching

	// Workspace row read inside maybeMarkContainerDead — this happens
	// BEFORE the isRestarting gate in the current implementation, so
	// allow exactly one SELECT runtime row.
	mock.ExpectQuery(`SELECT COALESCE\(runtime, 'claude-code'\) FROM workspaces WHERE id =`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))

	// Gate flipped: must early-return without doing anything else.
	finish := markRestarting(wsID)
	defer finish()

	stub := &preflightLocalProv{running: false, err: nil}
	h := newSelfFireHandler(t)
	h.provisioner = stub

	dead, _, _ := h.maybeMarkContainerDead(context.Background(), wsID, "", []byte("{}"), "message/send", 0, false)
	if dead != false {
		t.Errorf("maybeMarkContainerDead must return false while restarting, got %v", dead)
	}
	if stub.calls != 0 {
		t.Errorf("IsRunning must not be called while restarting (Layer 1 gate broken); got %d calls", stub.calls)
	}
}

// TestPreflightContainerHealth_SkippedWhileRestarting — Layer 1 for the
// proactive path. Same shape as above: with restart in flight, return
// nil (let the optimistic forward proceed) and DO NOT call IsRunning.
// The forward will fail with a connect error; the post-restart reactive
// path can decide what to do then, by which point the EC2 has either
// come up (no more failures) or markProvisionFailed has fired.
func TestPreflightContainerHealth_SkippedWhileRestarting(t *testing.T) {
	const wsID = "self-fire-ws-preflight"
	resetSelfFireState(wsID)
	_ = setupTestDB(t)

	finish := markRestarting(wsID)
	defer finish()

	stub := &preflightLocalProv{running: false, err: nil}
	h := newSelfFireHandler(t)
	h.provisioner = stub

	if err := h.preflightContainerHealth(context.Background(), wsID); err != nil {
		t.Errorf("preflightContainerHealth must return nil while restarting, got %+v", err)
	}
	if stub.calls != 0 {
		t.Errorf("IsRunning must not be called while restarting (Layer 1 gate broken); got %d calls", stub.calls)
	}
}

// TestRestartByID_DebounceSilentDrop — Layer 3. After a cycle starts,
// any RestartByID arriving within restartDebounceWindow MUST be dropped
// silently — not coalesced (which would still drain to another cycle).
// The drop counter must increment by exactly one per dropped call so
// ops can see how often the self-fire would have fired pre-fix.
func TestRestartByID_DebounceSilentDrop(t *testing.T) {
	const wsID = "self-fire-ws-debounce"
	resetSelfFireState(wsID)

	// Stamp restartStartedAt = now, running=false (simulates the "just
	// finished" window where the loop would re-fire pre-fix).
	sv, _ := restartStates.LoadOrStore(wsID, &restartState{})
	state := sv.(*restartState)
	state.mu.Lock()
	state.restartStartedAt = time.Now()
	state.running = false
	state.mu.Unlock()

	// Counter baseline.
	if got := restartByIDDropCounter.Load(); got != 0 {
		t.Fatalf("expected drop counter 0 at start, got %d", got)
	}

	// Five rapid-fire RestartByID calls should all drop (the maximum
	// observed pre-fix was 4x — pinning >=4 here keeps the regression
	// shape true to the prod incident).
	h := newSelfFireHandler(t)
	stub := &preflightLocalProv{running: true, err: nil}
	h.provisioner = stub
	for i := 0; i < 5; i++ {
		h.RestartByID(wsID)
	}

	if got := restartByIDDropCounter.Load(); got != 5 {
		t.Errorf("expected 5 drops within debounce window, got %d", got)
	}

	// shouldDebounceRestart itself must report true for the same window.
	if !shouldDebounceRestart(wsID) {
		t.Error("shouldDebounceRestart must return true within window")
	}
}

// TestRestartByID_DebounceExpiresAfterWindow — outside the window, the
// debounce must release: a legitimate later restart (e.g. user clicked
// Restart again after waiting) must proceed to coalesceRestart. We
// shrink restartDebounceWindow to 1ms for the duration of this test so
// we don't sleep a full 60s in CI.
func TestRestartByID_DebounceExpiresAfterWindow(t *testing.T) {
	const wsID = "self-fire-ws-debounce-release"
	resetSelfFireState(wsID)

	orig := RestartDebounceWindow
	RestartDebounceWindow = 5 * time.Millisecond
	defer func() { RestartDebounceWindow = orig }()

	// Stamp inside the window.
	sv, _ := restartStates.LoadOrStore(wsID, &restartState{})
	state := sv.(*restartState)
	state.mu.Lock()
	state.restartStartedAt = time.Now()
	state.running = false
	state.mu.Unlock()

	if !shouldDebounceRestart(wsID) {
		t.Fatal("within 5ms window must debounce")
	}

	// Sleep past the window. Use a small margin to avoid clock-skew
	// flakes on slow CI hosts.
	time.Sleep(20 * time.Millisecond)

	if shouldDebounceRestart(wsID) {
		t.Fatal("after 20ms (4x window) must no longer debounce")
	}
}

// TestRestartByID_SingleProvisionPerRestart — the regression test for
// the prod incident: a SINGLE secrets PUT (which is the trigger shape)
// must produce exactly ONE coalesceRestart cycle, not four. Models the
// full chain: secrets handler → RestartByID → coalesceRestart → cycle
// runs → during the cycle window, simulated probes call RestartByID
// again. With all three layers in place, the probes are dropped and the
// total cycle count stays at 1.
func TestRestartByID_SingleProvisionPerRestart(t *testing.T) {
	const wsID = "self-fire-ws-single-provision"
	resetSelfFireState(wsID)

	// In-flight gate that mimics the EC2-pending window. The cycle
	// blocks on cycleProceed so we can fire the simulated probes while
	// running=true.
	var cycleCount atomic.Int32
	cycleStarted := make(chan struct{}, 1)
	cycleProceed := make(chan struct{})

	cycle := func() {
		n := cycleCount.Add(1)
		if n == 1 {
			cycleStarted <- struct{}{}
			<-cycleProceed
		}
	}

	// Kick the first cycle via coalesceRestart (this is what RestartByID
	// would do post-debounce-check).
	done := make(chan struct{})
	go func() {
		coalesceRestart(wsID, cycle)
		close(done)
	}()
	<-cycleStarted

	// Simulate the 4 probe-driven RestartByID calls observed in prod.
	// Each must drop because we're within the debounce window AND a
	// cycle is in flight.
	h := newSelfFireHandler(t)
	stub := &preflightLocalProv{running: true, err: nil}
	h.provisioner = stub
	for i := 0; i < 4; i++ {
		h.RestartByID(wsID)
	}

	// Release the cycle.
	close(cycleProceed)
	<-done

	if got := cycleCount.Load(); got != 1 {
		t.Errorf("expected exactly 1 provision cycle for a single trigger "+
			"(self-fire fix), got %d — regression of the prod 4x reprov thrash class",
			got)
	}
	if got := restartByIDDropCounter.Load(); got != 4 {
		t.Errorf("expected 4 self-fire probes dropped, got %d "+
			"(observability counter must record the saved cycles)", got)
	}
}

// newSelfFireHandler constructs a minimal *WorkspaceHandler suitable for
// the Layer-1 gate tests. Wraps the boilerplate so the per-test setup
// stays focused on the assertion.
func newSelfFireHandler(t *testing.T) *WorkspaceHandler {
	t.Helper()
	return NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
}
