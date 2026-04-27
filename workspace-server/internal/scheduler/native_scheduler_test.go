package scheduler

import (
	"testing"
)

// TestSetNativeSchedulerCheck pins the wiring contract: New() leaves
// providesNativeScheduler nil (= today's behavior, never skip);
// SetNativeSchedulerCheck installs the override. The actual skip
// behavior in tick() needs a DB and is exercised by the integration
// tests in tests/e2e/.
func TestSetNativeSchedulerCheck(t *testing.T) {
	s := New(nil, nil)
	if s.providesNativeScheduler != nil {
		t.Fatal("New() must leave providesNativeScheduler nil so untouched callers preserve today's behavior")
	}

	called := false
	checker := NativeSchedulerCheck(func(workspaceID string) bool {
		called = true
		return workspaceID == "ws-native"
	})
	s.SetNativeSchedulerCheck(checker)
	if s.providesNativeScheduler == nil {
		t.Fatal("SetNativeSchedulerCheck did not install the function")
	}
	if !s.providesNativeScheduler("ws-native") {
		t.Fatal("installed checker not invoked / wrong return")
	}
	if !called {
		t.Fatal("installed checker not called")
	}
	if s.providesNativeScheduler("ws-other") {
		t.Fatal("checker should return false for non-native workspace")
	}
}

// TestNativeSchedulerCheck_NilSafeInTick documents the contract used
// by tick(): a nil providesNativeScheduler must mean "always fire" so
// existing callers (test fixtures, prior to capability primitives)
// preserve today's behavior unchanged. The conditional in tick reads
// `s.providesNativeScheduler != nil && s.providesNativeScheduler(id)`
// — neither branch can panic on a nil-checker scheduler.
func TestNativeSchedulerCheck_NilSafeInTick(t *testing.T) {
	s := New(nil, nil)
	// We don't actually call tick() — that requires a live DB. We just
	// pin that the field is nil after New, which is the load-bearing
	// invariant tick() relies on.
	if s.providesNativeScheduler != nil {
		t.Fatal("nil-safety contract violated: providesNativeScheduler must be nil from New()")
	}
}
