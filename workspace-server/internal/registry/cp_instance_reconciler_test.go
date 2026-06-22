package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

// fakeRunningChecker implements InstanceRunningChecker for the
// instance-reconciler tests. Records every IsRunning call so tests can
// assert which workspace IDs were probed, and returns a per-id
// (running, err) pair so we can model CP's three answers:
//
//	(true,  nil) — instance is running.
//	(false, nil) — CLEAN "not running" (terminated/stopped/absent).
//	(true,  err) — transient DB/transport error (FAIL-SAFE path).
type fakeRunningChecker struct {
	mu      sync.Mutex
	running map[string]bool
	errs    map[string]error
	calls   []string
}

func (f *fakeRunningChecker) IsRunning(_ context.Context, wsID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, wsID)
	if err, ok := f.errs[wsID]; ok {
		// Mirror CPProvisioner.IsRunning: (true, err) on transient errors
		// so callers stay on the alive path.
		return true, err
	}
	return f.running[wsID], nil
}

// recordingOffline is an OfflineHandler that records the workspace IDs
// it was invoked with.
type recordingOffline struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingOffline) handler() OfflineHandler {
	return func(_ context.Context, wsID string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, wsID)
	}
}

func (r *recordingOffline) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// expectReconcileQuery registers the reconciler's SELECT, pinning the
// scope-critical predicates: status IN ('online','degraded'), instance_id
// present (captured as a column for the TOCTOU re-confirm), and runtime
// <> 'external'. A future widening that drops any of these (e.g. sweeping
// paused rows, or external rows the heartbeat pass owns), or that drops
// the instance_id column the re-confirm depends on, fails every test that
// uses this helper.
func expectReconcileQuery(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`(?s)^\s*SELECT id::text, instance_id\s+FROM workspaces\s+WHERE status IN \('online', 'degraded'\)\s+AND instance_id IS NOT NULL\s+AND instance_id != ''\s+AND COALESCE\(runtime, ''\) <> 'external'\s+ORDER BY updated_at DESC\s+LIMIT \$1`).
		WithArgs(CPInstanceReconcileLimit).
		WillReturnRows(rows)
}

// reconcileRows builds the two-column (id, instance_id) result the
// reconciler's SELECT now returns. Pass id/instance_id pairs.
func reconcileRows(pairs ...[2]string) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"id", "instance_id"})
	for _, p := range pairs {
		r.AddRow(p[0], p[1])
	}
	return r
}

// expectReconfirm registers the TOCTOU re-confirm primary-key lookup for
// workspace id `wsID`, returning the row's CURRENT (status, instance_id).
// This is what the reconciler re-reads after IsRunning returns (false,
// nil), before it flips: it only flips when the SAME non-empty instance
// is still recorded AND status is still online/degraded.
func expectReconfirm(mock sqlmock.Sqlmock, wsID, curStatus, curInstanceID string) {
	mock.ExpectQuery(`(?s)^\s*SELECT status, COALESCE\(instance_id, ''\)\s+FROM workspaces\s+WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "instance_id"}).AddRow(curStatus, curInstanceID))
}

// expectReconfirmNoRows registers a re-confirm lookup that finds the row
// gone (deleted between SELECT and re-confirm) — the reconciler must
// treat this as "not a dead EC2" and skip the flip.
func expectReconfirmNoRows(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectQuery(`(?s)^\s*SELECT status, COALESCE\(instance_id, ''\)\s+FROM workspaces\s+WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "instance_id"}))
}

// TestReconcileOnce_NotRunning_FlipsOffline — the core bug (core#2247):
// an online SaaS workspace whose EC2 is terminated. CP reports a CLEAN
// (false, nil); onOffline MUST be called with that id so the existing
// auto-heal (status flip + RestartByID reprovision) kicks in.
func TestReconcileOnce_NotRunning_FlipsOffline(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{running: map[string]bool{"ws-dead": false}}
	off := &recordingOffline{}

	expectReconcileQuery(mock, reconcileRows([2]string{"ws-dead", "i-dead"}))
	// (false,nil) → re-confirm: row still online with the SAME instance →
	// confirmed-dead → flip.
	expectReconfirm(mock, "ws-dead", "online", "i-dead")

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 1 || got[0] != "ws-dead" {
		t.Fatalf("expected onOffline(ws-dead), got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_Running_DoesNotFlip — healthy steady state. CP
// reports (true, nil); the workspace stays online, onOffline is NOT
// called.
func TestReconcileOnce_Running_DoesNotFlip(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{running: map[string]bool{"ws-alive": true}}
	off := &recordingOffline{}

	// Running → no re-confirm, no flip.
	expectReconcileQuery(mock, reconcileRows([2]string{"ws-alive", "i-alive"}))

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 0 {
		t.Fatalf("running workspace must NOT be flipped offline, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_TransientError_DoesNotFlip — FAIL-SAFE contract.
// IsRunning returns (true, err) on a transient DB/transport blip; the
// reconciler MUST NOT flip the workspace offline. This is the guardrail
// that stops a CP outage from cascading every healthy workspace through
// reprovision.
func TestReconcileOnce_TransientError_DoesNotFlip(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{
		errs: map[string]error{"ws-blip": errors.New("cp provisioner: status: connection reset")},
	}
	off := &recordingOffline{}

	// (true,err) short-circuits BEFORE the re-confirm — no re-confirm query
	// is registered, so a stray re-confirm would fail ExpectationsWereMet.
	expectReconcileQuery(mock, reconcileRows([2]string{"ws-blip", "i-blip"}))

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 0 {
		t.Fatalf("fail-safe violated: transient IsRunning error must NOT flip offline, got %v", got)
	}
	if calls := checker.calls; len(calls) != 1 || calls[0] != "ws-blip" {
		t.Fatalf("expected IsRunning(ws-blip), got %v", checker.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_QueryScopeExcludesExternalAndNonOnline — pins the
// SELECT predicate. The regex in expectReconcileQuery requires
// status IN ('online','degraded') AND runtime <> 'external'; if a future
// edit widens the scope to include paused/hibernated/removed rows or
// external rows (owned by the heartbeat pass), or narrows it back to drop
// 'degraded', this query no longer matches and sqlmock fails the test.
// With the predicate intact, a DB that has only out-of-scope rows returns
// empty → no IsRunning, no flip.
func TestReconcileOnce_QueryScopeExcludesExternalAndNonOnline(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{}
	off := &recordingOffline{}

	// The predicate filters out external + out-of-scope-status rows
	// server-side, modelled as the empty result those filters produce.
	expectReconcileQuery(mock, reconcileRows())

	reconcileOnce(context.Background(), checker, off.handler())

	if len(checker.calls) != 0 {
		t.Fatalf("out-of-scope rows must never reach IsRunning, got %v", checker.calls)
	}
	if got := off.got(); len(got) != 0 {
		t.Fatalf("expected no offline flips for out-of-scope rows, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_MixedBatch — each row is judged independently: the
// dead one flips, the alive one and the transient-error one don't.
func TestReconcileOnce_MixedBatch(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{
		running: map[string]bool{"ws-dead": false, "ws-alive": true},
		errs:    map[string]error{"ws-blip": errors.New("503")},
	}
	off := &recordingOffline{}

	expectReconcileQuery(mock, reconcileRows(
		[2]string{"ws-dead", "i-dead"},
		[2]string{"ws-alive", "i-alive"},
		[2]string{"ws-blip", "i-blip"},
	))
	// Only ws-dead reaches the re-confirm ((false,nil)); it confirms.
	expectReconfirm(mock, "ws-dead", "online", "i-dead")

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 1 || got[0] != "ws-dead" {
		t.Fatalf("expected only ws-dead flipped, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_TOCTOU_InstanceChanged_DoesNotFlip — the HIGH-1
// regression guard. IsRunning returns a CLEAN (false, nil), but between
// the reconciler's SELECT and the probe the row's instance_id changed
// (reprovision attached a fresh EC2). IsRunning's independent
// resolveInstanceID is the reason the (false,nil) is stale: it may have
// resolved an empty/old instance. The re-confirm sees a DIFFERENT
// instance_id and MUST skip — flipping here would knock out a workspace
// whose NEW EC2 is not proven dead and fire RestartByID on a just-
// reprovisioned row.
func TestReconcileOnce_TOCTOU_InstanceChanged_DoesNotFlip(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{running: map[string]bool{"ws-race": false}}
	off := &recordingOffline{}

	expectReconcileQuery(mock, reconcileRows([2]string{"ws-race", "i-old"}))
	// Re-confirm: row is still online but now points at a DIFFERENT
	// instance (reprovisioned) → the (false,nil) was about i-old which is
	// no longer attached → skip.
	expectReconfirm(mock, "ws-race", "online", "i-new")

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 0 {
		t.Fatalf("TOCTOU guard violated: instance_id changed since SELECT must NOT flip, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_TOCTOU_InstanceCleared_DoesNotFlip — same HIGH-1
// guard, the instance_id-NULLed variant (CP-orphan-sweeper or a delete
// cleared it). Re-confirm sees an empty instance_id → skip.
func TestReconcileOnce_TOCTOU_InstanceCleared_DoesNotFlip(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{running: map[string]bool{"ws-cleared": false}}
	off := &recordingOffline{}

	expectReconcileQuery(mock, reconcileRows([2]string{"ws-cleared", "i-gone"}))
	expectReconfirm(mock, "ws-cleared", "online", "") // instance_id cleared

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 0 {
		t.Fatalf("TOCTOU guard violated: cleared instance_id must NOT flip, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_TOCTOU_StatusMoved_DoesNotFlip — same HIGH-1 guard,
// the status-moved variant. The row left online/degraded (e.g. paused or
// removed) between SELECT and re-confirm → skip.
func TestReconcileOnce_TOCTOU_StatusMoved_DoesNotFlip(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{running: map[string]bool{"ws-paused": false}}
	off := &recordingOffline{}

	expectReconcileQuery(mock, reconcileRows([2]string{"ws-paused", "i-keep"}))
	expectReconfirm(mock, "ws-paused", "paused", "i-keep") // status moved out of scope

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 0 {
		t.Fatalf("TOCTOU guard violated: row no longer online/degraded must NOT flip, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_TOCTOU_RowGone_DoesNotFlip — same HIGH-1 guard, the
// row-deleted variant. The re-confirm finds no row (concurrent delete) →
// skip; a stale (false,nil) about a just-deleted row must never fire
// onOffline/RestartByID.
func TestReconcileOnce_TOCTOU_RowGone_DoesNotFlip(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{running: map[string]bool{"ws-deleted": false}}
	off := &recordingOffline{}

	expectReconcileQuery(mock, reconcileRows([2]string{"ws-deleted", "i-x"}))
	expectReconfirmNoRows(mock, "ws-deleted") // row gone

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 0 {
		t.Fatalf("TOCTOU guard violated: deleted row must NOT flip, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_Degraded_FlipsOffline — MED-3 scope. A `degraded`
// SaaS workspace whose EC2 is gone is otherwise covered by NO sweep. It's
// in scope (the SELECT regex requires status IN ('online','degraded')),
// CP reports (false,nil), the re-confirm shows it STILL degraded with the
// SAME instance → flip.
func TestReconcileOnce_Degraded_FlipsOffline(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{running: map[string]bool{"ws-degraded": false}}
	off := &recordingOffline{}

	expectReconcileQuery(mock, reconcileRows([2]string{"ws-degraded", "i-deg"}))
	expectReconfirm(mock, "ws-degraded", "degraded", "i-deg")

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 1 || got[0] != "ws-degraded" {
		t.Fatalf("expected onOffline(ws-degraded), got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconfirm_DBError_DoesNotFlip — re-confirm fail-safe. If the
// re-confirm read itself errors (transient DB blip), we treat it as "not
// confirmed" and skip the flip rather than acting on an unverifiable
// (false,nil).
func TestReconcileOnce_ReconfirmDBError_DoesNotFlip(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{running: map[string]bool{"ws-x": false}}
	off := &recordingOffline{}

	expectReconcileQuery(mock, reconcileRows([2]string{"ws-x", "i-x"}))
	mock.ExpectQuery(`(?s)^\s*SELECT status, COALESCE\(instance_id, ''\)\s+FROM workspaces\s+WHERE id = \$1`).
		WithArgs("ws-x").
		WillReturnError(errors.New("connection reset"))

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 0 {
		t.Fatalf("re-confirm DB error must fail-safe (no flip), got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_QueryError — DB transient failure. Reconcile returns
// without panicking and never probes IsRunning or flips anything.
func TestReconcileOnce_QueryError(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{}
	off := &recordingOffline{}

	mock.ExpectQuery(`(?s)^\s*SELECT id::text, instance_id\s+FROM workspaces`).
		WithArgs(CPInstanceReconcileLimit).
		WillReturnError(errors.New("connection refused"))

	reconcileOnce(context.Background(), checker, off.handler())

	if len(checker.calls) != 0 || len(off.got()) != 0 {
		t.Fatalf("query error must short-circuit; calls=%v offline=%v", checker.calls, off.got())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_NilDB — defensive against db.DB being nil. Must not
// panic, must not probe, must not flip.
func TestReconcileOnce_NilDB(t *testing.T) {
	saved := db.DB
	db.DB = nil
	t.Cleanup(func() { db.DB = saved })

	checker := &fakeRunningChecker{}
	off := &recordingOffline{}
	reconcileOnce(context.Background(), checker, off.handler())

	if len(checker.calls) != 0 || len(off.got()) != 0 {
		t.Fatalf("nil db.DB must short-circuit; calls=%v offline=%v", checker.calls, off.got())
	}
}

// TestStartCPInstanceReconciler_NilCheckerDisabled — boot-safety: a SaaS
// CP without cpProv configured must not start the loop (immediate return,
// no goroutine leak).
func TestStartCPInstanceReconciler_NilCheckerDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		StartCPInstanceReconciler(ctx, nil, nil, 60*time.Second)
		close(done)
	}()
	select {
	case <-done:
		// expected — nil checker short-circuits.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StartCPInstanceReconciler(nil) did not return immediately")
	}
}

// TestStartCPInstanceReconciler_RunsOnceImmediatelyAndExitsOnCancel —
// cadence contract: one sweep at boot (so a restart starts healing
// immediately), and the loop terminates on ctx cancel.
func TestStartCPInstanceReconciler_RunsOnceImmediatelyAndExitsOnCancel(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{}
	off := &recordingOffline{}

	// Boot sweep query. The 60s ticker won't fire inside the test window;
	// register a second optional expectation so a stray tick can't fail.
	expectReconcileQuery(mock, sqlmock.NewRows([]string{"id"}))
	expectReconcileQuery(mock, sqlmock.NewRows([]string{"id"}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		StartCPInstanceReconciler(ctx, checker, off.handler(), 60*time.Second)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("StartCPInstanceReconciler did not exit on ctx cancel")
	}
}
