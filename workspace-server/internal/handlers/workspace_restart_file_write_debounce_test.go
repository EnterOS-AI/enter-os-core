package handlers

// Tests for internal#624 — file-write → RestartByID tight-loop fix.
//
// Empirical chain (Loki 2026-05-20 22:00-22:11Z on workspace
// 3fe84b89-eb65-42fc-ad1f-5c93582ca3e7, claude-code SEO Agent):
//
//   1. Canvas Save writes 10-17 files in a 30-60s window.
//   2. Each successful PUT /files at templates.go:575 / 591 / 607 / 662 /
//      682 / 697 (and template_import.go:239 / 275 / 297) fires
//      `goAsync(func() { wh.RestartByID(wsID) })`.
//   3. RestartByID's existing 60s self-fire debounce catches calls 1-60s
//      after the cycle starts. But writes at T+65s+ pass the debounce,
//      set pending=true on the still-running coalesceRestart cycle, and
//      drain IMMEDIATELY into cycle 2 — no re-debounce because the
//      original drain loop re-uses the same restartStartedAt.
//   4. Cycle 2 DELETEs+recreates EC2 mid-burst → user sees
//      EC2InstanceStateInvalidException 500 on the in-flight PUTs.
//
// Fix: two layers (both shipped in the same PR).
//
//   Path A (call-site debounce): every file-write trigger goes through
//   maybeRestartAfterFileWrite, which silently drops re-fires within 15s
//   of the last fire for the same workspace.
//
//   Path B (drain-loop re-stamp): coalesceRestart now re-stamps
//   restartStartedAt at the top of each drained iteration, so any
//   RestartByID arriving during a drained cycle hits a fresh 60s window
//   and is dropped by shouldDebounceRestart instead of chaining further.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetFileWriteDebounceState wipes the package-level sync.Map + drop
// counter for the given workspace ID. Tests must call this between
// scenarios because fileWriteRestartLastFireAt is shared.
func resetFileWriteDebounceState(workspaceID string) {
	fileWriteRestartLastFireAt.Delete(workspaceID)
	fileWriteRestartDropCounter.Store(0)
}

// newFileWriteDebounceHandler constructs a minimal *WorkspaceHandler with
// no provisioner so RestartByID short-circuits at HasProvisioner()=false
// — we only care that maybeRestartAfterFileWrite reaches goAsync at all.
// The asyncWG inside goAsync lets us wait for the goroutine to finish so
// we can deterministically observe whether RestartByID was scheduled.
func newFileWriteDebounceHandler(t *testing.T) *WorkspaceHandler {
	t.Helper()
	return NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
}

// TestMaybeRestartAfterFileWrite_FirstWriteRestarts — the baseline case:
// the very first call for a workspace must actually fire goAsync (i.e.
// no debounce-drop on the first PUT). Without this the helper would
// silently swallow every legitimate single-file save.
func TestMaybeRestartAfterFileWrite_FirstWriteRestarts(t *testing.T) {
	const wsID = "fw-debounce-first"
	resetFileWriteDebounceState(wsID)

	h := newFileWriteDebounceHandler(t)
	h.maybeRestartAfterFileWrite(wsID)

	// Drop counter must NOT have incremented — the call fired.
	if got := fileWriteRestartDropCounter.Load(); got != 0 {
		t.Errorf("first call to maybeRestartAfterFileWrite must fire (drop counter must stay 0), got %d", got)
	}

	// Last-fire timestamp must be populated (non-zero) so the next call
	// will compare against it.
	sv, ok := fileWriteRestartLastFireAt.Load(wsID)
	if !ok {
		t.Fatal("first call must register the workspace in fileWriteRestartLastFireAt")
	}
	stamp := sv.(*atomic.Int64).Load()
	if stamp == 0 {
		t.Error("first call must record a non-zero last-fire timestamp")
	}

	// Wait for the spawned goroutine to finish so it doesn't leak into
	// the next test (RestartByID will short-circuit on no-provisioner).
	h.waitAsyncForTest()
}

// TestMaybeRestartAfterFileWrite_SecondWriteWithin15sSkipped — the core
// fix: a second call within fileWriteRestartDebounceWindow of the first
// MUST NOT fire RestartByID. The drop counter must increment by exactly
// one and the last-fire timestamp must remain the FIRST call's stamp
// (proof that the second call did not overwrite it).
func TestMaybeRestartAfterFileWrite_SecondWriteWithin15sSkipped(t *testing.T) {
	const wsID = "fw-debounce-second-within"
	resetFileWriteDebounceState(wsID)

	h := newFileWriteDebounceHandler(t)

	// First call — fires.
	h.maybeRestartAfterFileWrite(wsID)
	h.waitAsyncForTest()

	sv, _ := fileWriteRestartLastFireAt.Load(wsID)
	firstStamp := sv.(*atomic.Int64).Load()

	// Second call immediately — must be dropped.
	h.maybeRestartAfterFileWrite(wsID)

	if got := fileWriteRestartDropCounter.Load(); got != 1 {
		t.Errorf("second call within 15s must increment drop counter by exactly 1, got %d", got)
	}

	// The CAS-loop must NOT have overwritten the first-call stamp — the
	// debounce branch short-circuits before the CompareAndSwap.
	stampAfter := sv.(*atomic.Int64).Load()
	if stampAfter != firstStamp {
		t.Errorf("dropped call must NOT update last-fire stamp (preserves debounce window); "+
			"first=%d after=%d", firstStamp, stampAfter)
	}
}

// TestMaybeRestartAfterFileWrite_ManyWritesInBurstCoalesceToOne — the
// "bonus" regression test called out in the issue: 10 simulated PUTs
// over 60s (compressed to a tight loop, all within 15s) must produce
// exactly 1 RestartByID schedule and 9 drops. Models the canvas Save
// burst shape that triggered the prod incident.
func TestMaybeRestartAfterFileWrite_ManyWritesInBurstCoalesceToOne(t *testing.T) {
	const wsID = "fw-debounce-burst"
	resetFileWriteDebounceState(wsID)

	h := newFileWriteDebounceHandler(t)

	// 10 rapid-fire calls — simulates 10 PUTs landing inside the canvas
	// Save burst window.
	const burstSize = 10
	for i := 0; i < burstSize; i++ {
		h.maybeRestartAfterFileWrite(wsID)
	}
	h.waitAsyncForTest()

	// One fired (call #1) + 9 dropped.
	if got := fileWriteRestartDropCounter.Load(); got != burstSize-1 {
		t.Errorf("expected %d drops for a %d-call burst (only call #1 fires), got %d",
			burstSize-1, burstSize, got)
	}
}

// TestMaybeRestartAfterFileWrite_AfterWindowExpiresFiresAgain — outside
// the debounce window, the helper must release and fire again. Shrinks
// fileWriteRestartDebounceWindow to 5ms so we don't sleep 15s in CI.
// Important: without this, a legitimate "user edited, walked away for
// a minute, edited again" would never restart and config changes would
// never reach the agent.
func TestMaybeRestartAfterFileWrite_AfterWindowExpiresFiresAgain(t *testing.T) {
	const wsID = "fw-debounce-window-expires"
	resetFileWriteDebounceState(wsID)

	orig := fileWriteRestartDebounceWindow
	fileWriteRestartDebounceWindow = 5 * time.Millisecond
	defer func() { fileWriteRestartDebounceWindow = orig }()

	h := newFileWriteDebounceHandler(t)

	h.maybeRestartAfterFileWrite(wsID) // fires
	h.waitAsyncForTest()

	// Wait past the window.
	time.Sleep(20 * time.Millisecond)

	h.maybeRestartAfterFileWrite(wsID) // must fire again
	h.waitAsyncForTest()

	// Drop counter must still be 0 — both calls fired.
	if got := fileWriteRestartDropCounter.Load(); got != 0 {
		t.Errorf("second call after window expiry must fire (not drop), got %d drops", got)
	}
}

// TestMaybeRestartAfterFileWrite_DifferentWorkspacesIndependent — the
// per-workspace state map must isolate: a burst on workspace A must not
// affect workspace B's debounce. Pinning so a future "use a single
// global atomic" refactor breaks loudly.
func TestMaybeRestartAfterFileWrite_DifferentWorkspacesIndependent(t *testing.T) {
	const wsA = "fw-debounce-ws-a"
	const wsB = "fw-debounce-ws-b"
	resetFileWriteDebounceState(wsA)
	resetFileWriteDebounceState(wsB)

	h := newFileWriteDebounceHandler(t)

	// 5 calls on A, all but one drop.
	for i := 0; i < 5; i++ {
		h.maybeRestartAfterFileWrite(wsA)
	}
	h.waitAsyncForTest()

	dropsAfterA := fileWriteRestartDropCounter.Load()

	// First call on B — must fire (its own independent window).
	h.maybeRestartAfterFileWrite(wsB)
	h.waitAsyncForTest()

	// B's call must not have incremented the drop counter — it fired.
	if got := fileWriteRestartDropCounter.Load(); got != dropsAfterA {
		t.Errorf("workspace B's first call must fire (not share workspace A's debounce); "+
			"drops after A=%d, drops after B=%d", dropsAfterA, got)
	}

	// Both workspaces must have their own last-fire entries.
	if _, ok := fileWriteRestartLastFireAt.Load(wsA); !ok {
		t.Error("workspace A missing from fileWriteRestartLastFireAt")
	}
	if _, ok := fileWriteRestartLastFireAt.Load(wsB); !ok {
		t.Error("workspace B missing from fileWriteRestartLastFireAt")
	}
}

// TestMaybeRestartAfterFileWrite_ConcurrentCallsSafelyDebounced — the
// CAS-loop contract: many goroutines hitting the helper concurrently
// must still produce at most one fired call (drops = N-1). Pinning the
// "thousands of writes, one restart" performance shape called out in
// the helper's comment. Uses sync.WaitGroup to release all goroutines
// in a tight burst so the CAS is genuinely contended.
func TestMaybeRestartAfterFileWrite_ConcurrentCallsSafelyDebounced(t *testing.T) {
	const wsID = "fw-debounce-concurrent"
	resetFileWriteDebounceState(wsID)

	h := newFileWriteDebounceHandler(t)

	const goroutines = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // hold every goroutine at the gate
			h.maybeRestartAfterFileWrite(wsID)
		}()
	}
	close(start) // release the herd
	wg.Wait()
	h.waitAsyncForTest()

	// Exactly N-1 drops: one goroutine wins the CAS and fires, all
	// other N-1 see a fresh stamp and drop into the debounce branch.
	if got := fileWriteRestartDropCounter.Load(); got != goroutines-1 {
		t.Errorf("expected %d drops for %d concurrent callers (exactly one fires), got %d",
			goroutines-1, goroutines, got)
	}
}

// TestCoalesceRestart_DrainRespectsRestartedAtBetweenIterations —
// Path B regression: when coalesceRestart drains a pending request into
// a follow-up cycle, the restartStartedAt timestamp must be re-stamped
// for that follow-up iteration. Without this, a RestartByID arriving
// during cycle 2 would hit a stale 60s window (computed from cycle 1's
// start) and could pass the debounce just because cycle 1 + cycle 2's
// runtime exceeded 60s combined.
//
// The test fires cycle 1 → completes → sets pending=true to trigger
// cycle 2 → asserts that restartStartedAt was advanced for the drained
// iteration. The cycle function itself just records the wall-clock at
// which it observed restartStartedAt, so the test can compare cycle 1's
// stamp vs cycle 2's stamp.
func TestCoalesceRestart_DrainRespectsRestartedAtBetweenIterations(t *testing.T) {
	const wsID = "fw-debounce-drain-restamp"
	resetRestartStatesFor(wsID)

	// Capture the restartStartedAt observed at the top of each cycle
	// iteration. The cycle reads it directly from the state map so we
	// see what coalesceRestart wrote.
	var stamps []time.Time
	var stampsMu sync.Mutex
	cycleCount := 0
	cycle := func() {
		sv, _ := restartStates.Load(wsID)
		state := sv.(*restartState)
		state.mu.Lock()
		stampsMu.Lock()
		stamps = append(stamps, state.restartStartedAt)
		stampsMu.Unlock()
		state.mu.Unlock()

		cycleCount++
		if cycleCount == 1 {
			// While inside cycle 1, set pending=true so the drain loop
			// runs cycle 2 next iteration. Mirrors the prod shape: a
			// PUT lands during cycle 1, sets pending=true via
			// RestartByID → coalesceRestart's pending branch.
			state.mu.Lock()
			state.pending = true
			state.mu.Unlock()

			// Sleep briefly so cycle 2's stamp is observably later
			// than cycle 1's. Without a real wall-clock gap the
			// assertion can't tell re-stamp from no-op.
			time.Sleep(20 * time.Millisecond)
		}
	}

	coalesceRestart(wsID, cycle)

	stampsMu.Lock()
	defer stampsMu.Unlock()
	if len(stamps) != 2 {
		t.Fatalf("expected 2 cycle iterations (original + drained pending), got %d", len(stamps))
	}
	if !stamps[1].After(stamps[0]) {
		t.Errorf("Path B regression: cycle 2's restartStartedAt (%v) must be AFTER "+
			"cycle 1's (%v) — drained iterations must re-stamp so the self-fire "+
			"debounce window resets per cycle. Without this, a RestartByID arriving "+
			"during cycle 2 sees a stale window and can chain into cycle 3.",
			stamps[1], stamps[0])
	}
}
