package handlers

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// stall_watchdog_test.go — coverage for Agent-Liveness RFC Layer 3 (A2)
// stall-watchdog. Validates the probe → restart state machine:
//
//  1. stale + busy + online, no prior state → PROBE (enqueue + 'probed' row).
//  2. probed + still-silent past grace → SOFT-RESTART (restartFunc fired +
//     'restarted'/last_action_at row).
//  3. probed + activity resumed (last_activity_at advanced past snapshot) →
//     CLEAR (state row deleted, NO restart).
//  4. probed + past grace BUT within cooldown of a prior restart → held, no
//     restart.
//  5. paused/hibernated/offline/idle never appear in the sweep (gated in SQL):
//     empty result set is a clean no-op.
//  6. env-override interval parses + falls back to default.
//
// The stale/busy/online/idle gating lives entirely in the sweep SELECT WHERE,
// so at the sqlmock level those cases reduce to "the query returns the row"
// vs "returns no row" — sqlmock can't evaluate a WHERE clause. The real-DB
// integration harness asserts the predicates; here we pin that a returned
// candidate drives exactly the right state-machine action.

const stallRows = `SELECT w.id`

func newTestStallWatchdog(t *testing.T) (*StallWatchdog, *recordedStallEnqueue, *recordedRestart) {
	t.Helper()
	enq := &recordedStallEnqueue{}
	rst := &recordedRestart{}
	sw := NewStallWatchdog(nil, rst.fire) // binds to sqlmock db.DB from setupTestDB
	sw.enqueue = func(ctx context.Context, workspaceID, callerID string, priority int,
		body []byte, method, idempotencyKey string, expiresAt *time.Time) (string, int, error) {
		enq.calls++
		enq.workspaceID = workspaceID
		enq.priority = priority
		enq.method = method
		enq.idemKey = idempotencyKey
		enq.body = body
		return "q-1", 1, nil
	}
	return sw, enq, rst
}

type recordedStallEnqueue struct {
	workspaceID string
	idemKey     string
	method      string
	priority    int
	body        []byte
	calls       int
}

type recordedRestart struct {
	calls  int
	lastID string
}

func (r *recordedRestart) fire(workspaceID string) {
	r.calls++
	r.lastID = workspaceID
}

func stallCols() []string {
	return []string{"id", "last_activity_at", "state", "probed_at", "probed_activity_at", "last_action_at"}
}

// --- 1. first detection → probe ---

func TestStallWatchdog_StaleBusyOnline_IsProbed(t *testing.T) {
	mock := setupTestDB(t)
	sw, enq, rst := newTestStallWatchdog(t)
	const ws = "11111111-1111-1111-1111-111111111111"
	staleAct := time.Now().Add(-20 * time.Minute)

	mock.ExpectQuery(stallRows).
		WillReturnRows(sqlmock.NewRows(stallCols()).
			AddRow(ws, staleAct, nil, nil, nil, nil))
	// Probe upserts the 'probed' state row.
	mock.ExpectExec(`INSERT INTO workspace_stall_state`).
		WithArgs(ws, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Best-effort audit row.
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res := sw.Sweep(context.Background())
	if res.Probed != 1 || res.Restarted != 0 || res.Cleared != 0 || res.Errors != 0 {
		t.Fatalf("expected exactly one probe; got %+v", res)
	}
	if enq.calls != 1 || enq.workspaceID != ws {
		t.Errorf("expected one probe enqueue to %s; got calls=%d ws=%s", ws, enq.calls, enq.workspaceID)
	}
	if enq.method != "message/send" {
		t.Errorf("probe method: got %q want message/send", enq.method)
	}
	if enq.idemKey == "" {
		t.Errorf("expected a non-empty hourly idempotency key")
	}
	if bs := string(enq.body); !strings.Contains(bs, "no recorded activity") || !strings.Contains(bs, "restarted") {
		t.Errorf("probe body missing liveness copy: %s", bs)
	}
	if rst.calls != 0 {
		t.Errorf("first detection must NOT restart; got %d", rst.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// --- 2. probed + still-silent past grace → restart ---

func TestStallWatchdog_ProbedStillSilentPastGrace_IsRestarted(t *testing.T) {
	mock := setupTestDB(t)
	sw, enq, rst := newTestStallWatchdog(t)
	const ws = "22222222-2222-2222-2222-222222222222"

	// last_activity_at unchanged since the probe snapshot; probed_at older than
	// the 5min grace.
	act := time.Now().Add(-30 * time.Minute)
	probedAt := time.Now().Add(-10 * time.Minute)

	mock.ExpectQuery(stallRows).
		WillReturnRows(sqlmock.NewRows(stallCols()).
			AddRow(ws, act, "probed", probedAt, act, nil))
	// softRestart records the 'restarted'/last_action_at row...
	mock.ExpectExec(`INSERT INTO workspace_stall_state`).
		WithArgs(ws, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// ...then the audit row.
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res := sw.Sweep(context.Background())
	waitGlobalAsyncForTest() // drain the globalGoAsync restart dispatch

	if res.Restarted != 1 || res.Probed != 0 || res.Cleared != 0 || res.Errors != 0 {
		t.Fatalf("expected exactly one restart; got %+v", res)
	}
	if rst.calls != 1 || rst.lastID != ws {
		t.Errorf("expected soft-restart of %s; got calls=%d id=%s", ws, rst.calls, rst.lastID)
	}
	if enq.calls != 0 {
		t.Errorf("restart stage must not re-probe; got %d enqueues", enq.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// --- 3. activity resumed → clear, no restart ---

func TestStallWatchdog_ActivityResumed_IsClearedNoRestart(t *testing.T) {
	mock := setupTestDB(t)
	sw, enq, rst := newTestStallWatchdog(t)
	const ws = "33333333-3333-3333-3333-333333333333"

	// probed_activity_at is the OLD snapshot; live last_activity_at advanced
	// PAST it → the agent acted. (Still old enough to be in the sweep set, e.g.
	// active_tasks>0 with the most recent activity 13min ago, but newer than the
	// snapshot taken at probe time.)
	snapshot := time.Now().Add(-25 * time.Minute)
	resumedAct := time.Now().Add(-13 * time.Minute)
	probedAt := time.Now().Add(-10 * time.Minute)

	mock.ExpectQuery(stallRows).
		WillReturnRows(sqlmock.NewRows(stallCols()).
			AddRow(ws, resumedAct, "probed", probedAt, snapshot, nil))
	// Clear deletes the state row...
	mock.ExpectExec(`DELETE FROM workspace_stall_state`).
		WithArgs(ws).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// ...then audits the clear.
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res := sw.Sweep(context.Background())
	waitGlobalAsyncForTest()

	if res.Cleared != 1 || res.Restarted != 0 || res.Probed != 0 || res.Errors != 0 {
		t.Fatalf("expected exactly one clear; got %+v", res)
	}
	if rst.calls != 0 {
		t.Errorf("resumed activity must NOT restart; got %d", rst.calls)
	}
	if enq.calls != 0 {
		t.Errorf("clear must not enqueue; got %d", enq.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// --- 4. past grace but within cooldown → held, no restart ---

func TestStallWatchdog_WithinCooldown_NotRestarted(t *testing.T) {
	mock := setupTestDB(t)
	sw, enq, rst := newTestStallWatchdog(t)
	const ws = "44444444-4444-4444-4444-444444444444"

	act := time.Now().Add(-40 * time.Minute)
	probedAt := time.Now().Add(-10 * time.Minute)  // past the 5min grace
	lastAction := time.Now().Add(-5 * time.Minute) // within the 30min cooldown

	mock.ExpectQuery(stallRows).
		WillReturnRows(sqlmock.NewRows(stallCols()).
			AddRow(ws, act, "probed", probedAt, act, lastAction))
	// No further Exec expected — held within cooldown.

	res := sw.Sweep(context.Background())
	waitGlobalAsyncForTest()

	if res.Restarted != 0 || res.Probed != 0 || res.Cleared != 0 || res.Errors != 0 {
		t.Fatalf("within cooldown must be a no-op; got %+v", res)
	}
	if rst.calls != 0 {
		t.Errorf("cooldown must suppress restart; got %d", rst.calls)
	}
	if enq.calls != 0 {
		t.Errorf("no enqueue expected while held; got %d", enq.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (an Exec fired despite cooldown?): %v", err)
	}
}

// --- 5. empty set (paused/hibernated/offline/idle gated by SQL) → no-op ---

func TestStallWatchdog_EmptyResultIsCleanNoOp(t *testing.T) {
	mock := setupTestDB(t)
	sw, enq, rst := newTestStallWatchdog(t)

	mock.ExpectQuery(stallRows).
		WillReturnRows(sqlmock.NewRows(stallCols()))

	res := sw.Sweep(context.Background())
	if res.Probed != 0 || res.Restarted != 0 || res.Cleared != 0 || res.Errors != 0 {
		t.Errorf("empty set must produce zero changes; got %+v", res)
	}
	if enq.calls != 0 || rst.calls != 0 {
		t.Errorf("ineligible candidates must not be probed/restarted; enq=%d rst=%d", enq.calls, rst.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// --- 6. env override parsing ---

func TestStallWatchdogConstructor_PicksUpEnvOverride(t *testing.T) {
	t.Setenv("STALL_WATCHDOG_INTERVAL_S", "120")
	_ = setupTestDB(t)
	sw := NewStallWatchdog(nil, nil)
	if sw.Interval() != 120*time.Second {
		t.Errorf("interval override not picked up: got %v", sw.Interval())
	}
}

func TestStallWatchdogConstructor_DefaultWhenEnvUnset(t *testing.T) {
	t.Setenv("STALL_WATCHDOG_INTERVAL_S", "")
	_ = setupTestDB(t)
	sw := NewStallWatchdog(nil, nil)
	if sw.Interval() != defaultStallWatchdogInterval {
		t.Errorf("default interval not used: got %v", sw.Interval())
	}
}

// --- restartFunc nil → probe-only (past grace does not panic / restarts 0) ---

func TestStallWatchdog_NilRestartFunc_ProbeOnlyNoRestart(t *testing.T) {
	mock := setupTestDB(t)
	sw, _, _ := newTestStallWatchdog(t)
	sw.restart = nil // probe-only mode
	const ws = "55555555-5555-5555-5555-555555555555"

	act := time.Now().Add(-40 * time.Minute)
	probedAt := time.Now().Add(-10 * time.Minute)

	mock.ExpectQuery(stallRows).
		WillReturnRows(sqlmock.NewRows(stallCols()).
			AddRow(ws, act, "probed", probedAt, act, nil))
	// No Exec: softRestart returns early (nil restartFunc) before any DB write.

	res := sw.Sweep(context.Background())
	if res.Restarted != 0 || res.Errors != 0 {
		t.Fatalf("nil restartFunc must not restart or error; got %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// guard against an accidental unused import of sql.
var _ = sql.ErrNoRows
