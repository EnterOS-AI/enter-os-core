package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
	"unicode/utf8"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/textutil"
)

// errDBDown is a sentinel error used by tests to simulate a DB connection failure.
var errDBDown = sql.ErrConnDone

// setupTestDB replaces the global db.DB with a sqlmock and returns the mock
// handle. The real DB is restored (by closing the mock conn) via t.Cleanup.
func setupTestDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { mockDB.Close(); db.DB = prevDB })
	return mock
}

// panicProxy is a test double whose ProxyA2ARequest always panics.
// Used by TestPanicRecovery to verify the scheduler's goroutine-level
// panic recovery in fireSchedule.
type panicProxy struct{}

func (p *panicProxy) ProxyA2ARequest(
	_ context.Context, _ string, _ []byte, _ string, _ bool,
) (int, []byte, error) {
	panic("simulated A2A proxy panic")
}

// EnqueueA2A satisfies the extended A2AProxy interface; panics like the fire path.
func (p *panicProxy) EnqueueA2A(
	_ context.Context, _ string, _ string, _ int, _ []byte, _ string, _ string, _ *time.Time,
) (string, int, error) {
	panic("simulated A2A enqueue panic")
}

// ── TestLastTickAt_zero ───────────────────────────────────────────────────────

// TestLastTickAt_zero confirms that LastTickAt returns a zero time.Time on a
// freshly-created scheduler that has never been started or ticked.
func TestLastTickAt_zero(t *testing.T) {
	s := New(nil, nil)
	if got := s.LastTickAt(); !got.IsZero() {
		t.Errorf("LastTickAt() before any tick = %v, want zero time.Time", got)
	}
}

// ── TestHealthy_beforeStart ───────────────────────────────────────────────────

// TestHealthy_beforeStart confirms that Healthy returns false when lastTickAt
// is zero (scheduler created but never ticked).
func TestHealthy_beforeStart(t *testing.T) {
	s := New(nil, nil)
	if s.Healthy() {
		t.Error("Healthy() = true on a scheduler that has never ticked, want false")
	}
}

// ── TestHealthy_freshTick ─────────────────────────────────────────────────────

// TestHealthy_freshTick sets lastTickAt to the current time — mirroring what
// tick() does after a completed poll cycle — and confirms Healthy returns true.
func TestHealthy_freshTick(t *testing.T) {
	s := New(nil, nil)

	// Simulate what tick() writes after wg.Wait() returns.
	s.mu.Lock()
	s.lastTickAt = time.Now()
	s.mu.Unlock()

	if !s.Healthy() {
		t.Error("Healthy() = false immediately after a fresh tick timestamp, want true")
	}
}

// ── TestHealthy_stale ─────────────────────────────────────────────────────────

// TestHealthy_stale backdates lastTickAt by 3×pollInterval (well beyond the
// 2×pollInterval liveness window) and confirms Healthy returns false.
func TestHealthy_stale(t *testing.T) {
	s := New(nil, nil)

	s.mu.Lock()
	s.lastTickAt = time.Now().Add(-3 * pollInterval) // 90 s ago; threshold is 60 s
	s.mu.Unlock()

	if s.Healthy() {
		t.Errorf("Healthy() = true when lastTickAt is 3×pollInterval (%s) ago, want false",
			3*pollInterval)
	}
}

// ── TestComputeNextRun_valid ──────────────────────────────────────────────────

// TestComputeNextRun_valid checks that "0 * * * *" (top-of-hour) returns a
// future time whose Minute() == 0 when the reference is mid-hour.
func TestComputeNextRun_valid(t *testing.T) {
	// 2025-01-01 12:30 UTC — clearly mid-hour so "next" top-of-hour is 13:00.
	ref := time.Date(2025, 1, 1, 12, 30, 0, 0, time.UTC)

	next, err := ComputeNextRun("0 * * * *", "UTC", ref)
	if err != nil {
		t.Fatalf("ComputeNextRun(valid expr) returned unexpected error: %v", err)
	}
	if !next.After(ref) {
		t.Errorf("ComputeNextRun() = %v, want a time strictly after ref %v", next, ref)
	}
	if next.Minute() != 0 {
		t.Errorf("ComputeNextRun() minute = %d, want 0 (top of hour)", next.Minute())
	}
}

// ── TestComputeNextRun_invalid ────────────────────────────────────────────────

// TestComputeNextRun_invalid confirms that an unparseable cron expression
// returns a non-nil error.
func TestComputeNextRun_invalid(t *testing.T) {
	_, err := ComputeNextRun("not-a-cron", "UTC", time.Now())
	if err == nil {
		t.Error("ComputeNextRun(invalid cron expr) = nil, want non-nil error")
	}
}

// ── TestComputeNextRun_invalidTimezone ────────────────────────────────────────

// TestComputeNextRun_invalidTimezone confirms that an unrecognised IANA
// timezone name returns a non-nil error (rather than silently falling back
// to UTC, which could mask misconfigured schedules).
func TestComputeNextRun_invalidTimezone(t *testing.T) {
	_, err := ComputeNextRun("0 * * * *", "Not/AZone", time.Now())
	if err == nil {
		t.Error("ComputeNextRun(invalid tz) = nil, want non-nil error")
	}
}

// ── TestPanicRecovery ─────────────────────────────────────────────────────────

// TestPanicRecovery verifies that a panic inside a fireSchedule goroutine does
// NOT crash the scheduler.
//
// The test calls tick() directly with a sqlmock that surfaces one due schedule.
// panicProxy causes ProxyA2ARequest to panic; the deferred recover() in
// fireSchedule catches it.  After tick() returns, lastTickAt must be set and
// Healthy() must return true — proving the scheduler survived.
//
// Without panic recovery an unrecovered goroutine panic terminates the entire
// test binary, so the test completing is itself evidence that recovery worked.
func TestPanicRecovery(t *testing.T) {
	mock := setupTestDB(t)

	// WorkspaceID must be ≥12 chars: fireSchedule slices it with [:12] for logging.
	schedRows := sqlmock.NewRows(
		[]string{"id", "workspace_id", "name", "cron_expr", "timezone", "prompt"},
	).AddRow(
		"sched-panic-01",       // id
		"ws-panic-workspace-1", // workspace_id (21 chars > 12)
		"panic-job",            // name
		"* * * * *",            // cron_expr
		"UTC",                  // timezone
		"fire and panic",       // prompt
	)
	mock.ExpectQuery(`SELECT id, workspace_id`).WillReturnRows(schedRows)

	s := New(&panicProxy{}, nil)

	// tick() launches fireSchedule in a goroutine that will panic.
	// If there is no recovery, the goroutine crash terminates the process here.
	s.tick(context.Background())

	// tick() returned normally → the panic was caught, wg.Wait() completed,
	// and lastTickAt was updated.
	if !s.Healthy() {
		t.Error("Healthy() = false after panic-recovery tick, want true " +
			"— scheduler must survive a panicking A2A proxy")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// ── TestShort_helper ──────────────────────────────────────────────────────────
// Regression guard for the short() helper that replaced unsafe [:N] slices
// after code review. Panicked when IDs were shorter than the slice bound.

func TestShort_helper(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abcdef1234567890", 8, "abcdef12"},
		{"abc", 8, "abc"}, // shorter than n — no panic, no truncation
		{"", 8, ""},
		{"12345678", 8, "12345678"}, // exactly n
	}
	for _, tc := range cases {
		if got := short(tc.in, tc.n); got != tc.want {
			t.Errorf("short(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// ── TestRecordSkipped_writesSkippedStatus ────────────────────────────────────
// ── busyEnqueueProxy + TestFireSchedule_BusyEnqueuesInsteadOfSkipping ──────────
//
// Replaces the old "busy → skip after 2 min" assertion. When the workspace is
// at capacity, fireSchedule must ENQUEUE the tick into the durable a2a_queue
// (keyed by schedule_id) and record last_status='queued' — NOT fire and NOT
// recordSkipped. Proves the scheduled-tick-starvation fix.

type busyEnqueueProxy struct {
	fired       int
	enqueued    int
	enqKey      string
	enqMethod   string
	enqPriority int
}

func (p *busyEnqueueProxy) ProxyA2ARequest(
	_ context.Context, _ string, _ []byte, _ string, _ bool,
) (int, []byte, error) {
	p.fired++
	return 200, []byte(`{"ok":true}`), nil
}

func (p *busyEnqueueProxy) EnqueueA2A(
	_ context.Context, _ string, _ string, priority int, _ []byte, method, idempotencyKey string, _ *time.Time,
) (string, int, error) {
	p.enqueued++
	p.enqKey = idempotencyKey
	p.enqMethod = method
	p.enqPriority = priority
	return "q-busy-1", 1, nil
}

func TestFireSchedule_BusyEnqueuesInsteadOfSkipping(t *testing.T) {
	mock := setupTestDB(t)

	sched := scheduleRow{
		ID:          "77777777-dead-beef-0000-000000000007",
		WorkspaceID: "88888888-dead-beef-0000-000000000008",
		Name:        "busy-enqueue-job",
		CronExpr:    "*/5 * * * *",
		Timezone:    "UTC",
		Prompt:      "tick while busy",
	}

	// Capacity check → active_tasks(2) >= max_concurrent(1): workspace is busy.
	mock.ExpectQuery(`SELECT COALESCE`).
		WillReturnRows(sqlmock.NewRows([]string{"active_tasks", "max"}).AddRow(2, 1))

	// recordQueued UPDATE — binds ($1=sched.ID, $2=nextRunPtr, $3=reason);
	// last_status='queued' is a SQL literal, not a bound arg.
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// recordQueued activity_logs INSERT — binds 4 args (workspace_id, summary,
	// request_body, error_detail); status='queued' is a SQL literal.
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(sched.WorkspaceID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	proxy := &busyEnqueueProxy{}
	s := New(proxy, nil)
	s.fireSchedule(context.Background(), sched)

	if proxy.fired != 0 {
		t.Errorf("busy workspace: ProxyA2ARequest must NOT fire, got %d fires", proxy.fired)
	}
	if proxy.enqueued != 1 {
		t.Fatalf("busy workspace: expected exactly 1 EnqueueA2A, got %d", proxy.enqueued)
	}
	if proxy.enqKey != sched.ID {
		t.Errorf("idempotency key must be schedule_id %q (buffer-latest dedup), got %q", sched.ID, proxy.enqKey)
	}
	if proxy.enqMethod != "message/send" {
		t.Errorf("enqueued method = %q, want \"message/send\"", proxy.enqMethod)
	}
	if proxy.enqPriority != priorityTask {
		t.Errorf("enqueued priority = %d, want priorityTask(%d)", proxy.enqPriority, priorityTask)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations — busy tick not recorded as queued: %v", err)
	}
}

// #115 coverage gap: the recordSkipped path wasn't tested at all when it
// first landed. Exercises the UPDATE workspace_schedules + INSERT into
// activity_logs via sqlmock. Broadcaster is nil so we don't need to stub
// RecordAndBroadcast (the nil-check in recordSkipped handles that).

func TestRecordSkipped_writesSkippedStatus(t *testing.T) {
	mock := setupTestDB(t)
	s := New(nil, nil)

	sched := scheduleRow{
		ID:          "11111111-1111-1111-1111-111111111111",
		WorkspaceID: "22222222-2222-2222-2222-222222222222",
		Name:        "Hourly security audit",
		CronExpr:    "17 * * * *",
		Timezone:    "UTC",
		Prompt:      "audit",
	}

	// Expect the schedule-row UPDATE with last_status='skipped' and the
	// cron_run activity_logs INSERT with status='skipped' + error_detail
	// carrying the active_tasks reason.
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(sched.WorkspaceID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	s.recordSkipped(context.Background(), sched, 3)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ── successProxy ─────────────────────────────────────────────────────────────

// successProxy is a test double whose ProxyA2ARequest always returns HTTP 200
// with no error, simulating a healthy A2A round-trip.
type successProxy struct{}

func (p *successProxy) ProxyA2ARequest(
	_ context.Context, _ string, _ []byte, _ string, _ bool,
) (int, []byte, error) {
	return 200, []byte(`{"ok":true}`), nil
}

// EnqueueA2A satisfies the extended A2AProxy interface.
func (p *successProxy) EnqueueA2A(
	_ context.Context, _ string, _ string, _ int, _ []byte, _ string, _ string, _ *time.Time,
) (string, int, error) {
	return "q-success", 1, nil
}

// ── adapterErrorProxy ─────────────────────────────────────────────────────────

// adapterErrorProxy is a test double whose ProxyA2ARequest returns HTTP 200
// with a JSON-RPC error body, simulating an adapter SDK that throws internally
// but still completes the HTTP round-trip. Issue #1696.
type adapterErrorProxy struct{}

func (p *adapterErrorProxy) ProxyA2ARequest(
	_ context.Context, _ string, _ []byte, _ string, _ bool,
) (int, []byte, error) {
	return 200, []byte(`{"jsonrpc":"2.0","id":"cron-test-123","error":{"code":-32603,"message":"adapter SDK internal error"}}`), nil
}

// EnqueueA2A satisfies the extended A2AProxy interface.
func (p *adapterErrorProxy) EnqueueA2A(
	_ context.Context, _ string, _ string, _ int, _ []byte, _ string, _ string, _ *time.Time,
) (string, int, error) {
	return "q-adaptererr", 1, nil
}

// ── TestFireSchedule_AdapterSDKError (#1696) ──────────────────────────────────
//
// When the adapter SDK throws internally and returns HTTP 200 with an error
// payload, fireSchedule must record last_status='error', not 'ok'.

func TestFireSchedule_AdapterSDKError(t *testing.T) {
	mock := setupTestDB(t)

	sched := scheduleRow{
		ID:          "55555555-dead-beef-0000-000000000005",
		WorkspaceID: "66666666-dead-beef-0000-000000000006",
		Name:        "adapter-err-job",
		CronExpr:    "0 * * * *",
		Timezone:    "UTC",
		Prompt:      "do something",
	}

	// active_tasks check → 0 (workspace is idle; proceed to fire)
	mock.ExpectQuery(`SELECT COALESCE`).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))

	// Post-fire UPDATE must record last_status='error' with the adapter error message.
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID, sqlmock.AnyArg(), "error", "A2A adapter error: adapter SDK internal error").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// activity_logs INSERT must carry status='error' and the error detail.
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(sched.WorkspaceID, sqlmock.AnyArg(), sqlmock.AnyArg(), "error", "A2A adapter error: adapter SDK internal error").
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(&adapterErrorProxy{}, nil)
	s.fireSchedule(context.Background(), sched)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations — adapter error not recorded correctly: %v", err)
	}
}

// ── TestFireSchedule_ComputeNextRunError (#722 Bug 1) ─────────────────────────
//
// When ComputeNextRun fails (bad cron expression), fireSchedule must NOT write
// NULL to next_run_at — it must use COALESCE so the existing DB value is kept.
// Proof: the UPDATE ExecContext must still be called (schedule not abandoned)
// and sqlmock satisfies all expectations (no unexpected SQL).

func TestFireSchedule_ComputeNextRunError(t *testing.T) {
	mock := setupTestDB(t)

	sched := scheduleRow{
		ID:          "11111111-dead-beef-0000-000000000001",
		WorkspaceID: "22222222-dead-beef-0000-000000000002",
		Name:        "bad-cron-job",
		CronExpr:    "not-a-valid-cron", // guaranteed to fail ComputeNextRun
		Timezone:    "UTC",
		Prompt:      "do something",
	}

	// active_tasks check → 0 (workspace is idle; proceed to fire)
	mock.ExpectQuery(`SELECT COALESCE`).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))

	// #795 consecutive_empty_runs reset — successProxy returns {"ok":true}
	// which is non-empty, so the counter is reset to 0.
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// #1696 consecutive_sdk_errors reset — successProxy has no result_kind,
	// so detectResultKind returns "" and lastStatus="ok" → reset.
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// UPDATE must fire — COALESCE($2, next_run_at) keeps existing value when $2 is nil.
	// AnyArg for $2 because it will be nil (ComputeNextRun failed).
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID, sqlmock.AnyArg(), "ok", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// activity_logs INSERT always fires
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(sched.WorkspaceID, sqlmock.AnyArg(), sqlmock.AnyArg(), "ok", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(&successProxy{}, nil)
	s.fireSchedule(context.Background(), sched)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations — schedule update was skipped or next_run_at not preserved: %v", err)
	}
}

// ── TestRecordSkipped_ComputeNextRunError (#722 Bug 1 — skipped path) ─────────
//
// Same invariant as TestFireSchedule_ComputeNextRunError but for the
// recordSkipped path: a bad cron expression must not NULL out next_run_at.

func TestRecordSkipped_ComputeNextRunError(t *testing.T) {
	mock := setupTestDB(t)

	sched := scheduleRow{
		ID:          "33333333-dead-beef-0000-000000000003",
		WorkspaceID: "44444444-dead-beef-0000-000000000004",
		Name:        "bad-cron-skip",
		CronExpr:    "not-a-valid-cron",
		Timezone:    "UTC",
		Prompt:      "skipped task",
	}

	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(sched.WorkspaceID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(nil, nil)
	s.recordSkipped(context.Background(), sched, 2)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// ── TestRepairNullNextRunAt_RepairsRows (#722 Bug 3) ──────────────────────────
//
// repairNullNextRunAt must SELECT enabled schedules with NULL next_run_at,
// compute the next fire time, and UPDATE each row.

func TestRepairNullNextRunAt_RepairsRows(t *testing.T) {
	mock := setupTestDB(t)

	// Two schedules whose next_run_at is NULL and whose cron exprs are valid.
	mock.ExpectQuery(`SELECT id, cron_expr, timezone`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "cron_expr", "timezone"}).
			AddRow("sched-repair-01", "0 * * * *", "UTC").
			AddRow("sched-repair-02", "30 9 * * 1", "America/New_York"))

	// Expect one UPDATE per repaired row.
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs("sched-repair-01", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs("sched-repair-02", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(nil, nil)
	s.repairNullNextRunAt(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// ── TestRepairNullNextRunAt_DBError_NoPanic (#722 Bug 3) ──────────────────────
//
// A DB error from the SELECT must be logged but must not panic — the scheduler
// startup should proceed normally.

func TestRepairNullNextRunAt_DBError_NoPanic(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery(`SELECT id, cron_expr, timezone`).
		WillReturnError(errDBDown)

	s := New(nil, nil)
	// Must not panic:
	s.repairNullNextRunAt(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// repairNullNextRunAt + hibernation (#711 + #722 integration)
// ──────────────────────────────────────────────────────────────────────────────

// TestRepairNullNextRunAt_HibernatedWorkspace_ScheduleRepaired verifies that
// repairNullNextRunAt() repairs schedules belonging to hibernated workspaces.
//
// Context: the repair query is:
//
//	SELECT id, cron_expr, timezone
//	FROM workspace_schedules
//	WHERE enabled = true AND next_run_at IS NULL
//
// Critically, there is NO "AND workspace.status != 'hibernated'" filter.
// This is intentional — a hibernated workspace should wake up on schedule
// (via the auto-wake A2A path). If the repair skipped hibernated workspaces,
// any schedule whose next_run_at was NULL'd before hibernation would never
// fire again even after the workspace wakes.
//
// This test simulates a schedule with a NULL next_run_at whose owning workspace
// is currently hibernated, and asserts the UPDATE fires to set next_run_at.
func TestRepairNullNextRunAt_HibernatedWorkspace_ScheduleRepaired(t *testing.T) {
	mock := setupTestDB(t)

	// The repair SELECT has no workspace status filter — a hibernated workspace's
	// schedule appears in the result set normally.
	mock.ExpectQuery(`SELECT id, cron_expr, timezone`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "cron_expr", "timezone"}).
			AddRow("sched-hibernated-01", "0 9 * * *", "UTC"))

	// Repair must attempt the UPDATE (next_run_at computed from valid cron expr).
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs("sched-hibernated-01", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(nil, nil)
	s.repairNullNextRunAt(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v\n"+
			"repairNullNextRunAt must not filter out hibernated workspaces — "+
			"their schedules must still be repaired so they fire on wake", err)
	}
}

// ── TestRecordSkipped_shortWorkspaceIDNoPanic ─────────────────────────────────
// Guards against the short() regression: recordSkipped must not panic if
// WorkspaceID is unexpectedly shorter than the 12-char prefix used in logs.

func TestRecordSkipped_shortWorkspaceIDNoPanic(t *testing.T) {
	mock := setupTestDB(t)
	s := New(nil, nil)

	// 4-char workspace id — shorter than any substring bound in the code.
	sched := scheduleRow{
		ID:          "11111111-1111-1111-1111-111111111111",
		WorkspaceID: "ws-x",
		Name:        "test",
		CronExpr:    "0 * * * *",
		Timezone:    "UTC",
	}
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("recordSkipped panicked on short WorkspaceID: %v", r)
		}
	}()
	s.recordSkipped(context.Background(), sched, 1)
}

// ── Panic-recovery next_run_at advancement (#1029) ──────────────────────────
//
// Issue #1029: when fireSchedule panics (e.g. a nil-pointer in the A2A proxy
// or a bad JSON marshal), the deferred recover must advance next_run_at to the
// next cron window. Without this fix the schedule's next_run_at stays in the
// past and fires on every 30-second tick — a tight retry loop that amplifies
// the original failure.

// TestPanicRecovery_AdvancesNextRunAt verifies that the recover block in
// fireSchedule issues an UPDATE to advance next_run_at when the proxy panics.
//
// This is the core invariant of the #1029 fix: panic → recover → advance.
// The test calls fireSchedule directly (not via tick) so the sqlmock
// expectations are precise — we know exactly which queries fire and in what
// order.
func TestPanicRecovery_AdvancesNextRunAt(t *testing.T) {
	mock := setupTestDB(t)

	sched := scheduleRow{
		ID:          "aaa11111-1111-1111-1111-111111111111",
		WorkspaceID: "bbb22222-2222-2222-2222-222222222222",
		Name:        "panic-advance-test",
		CronExpr:    "0 * * * *", // every hour — valid expr so ComputeNextRun succeeds
		Timezone:    "UTC",
		Prompt:      "trigger panic",
	}

	// 1. fireSchedule first checks active_tasks on the workspace.
	//    Return 0 so the fire proceeds (not skipped).
	mock.ExpectQuery(`SELECT COALESCE`).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))

	// 2. ProxyA2ARequest panics (panicProxy).
	//    The deferred recover catches it and calls:
	//      ComputeNextRun(cronExpr, tz, time.Now())
	//      db.DB.ExecContext(ctx, `UPDATE workspace_schedules SET next_run_at = $1 ... WHERE id = $2`, nextTime, sched.ID)
	//
	//    We expect this UPDATE with the schedule ID as arg 2.
	mock.ExpectExec(`UPDATE workspace_schedules SET next_run_at`).
		WithArgs(sqlmock.AnyArg(), sched.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(&panicProxy{}, nil)
	// fireSchedule must not propagate the panic — the recover catches it.
	s.fireSchedule(context.Background(), sched)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v\n"+
			"The panic-recovery defer must advance next_run_at via UPDATE (#1029)", err)
	}
}

// TestFireSchedule_NormalSuccess_AdvancesNextRunAt is a regression guard for
// the happy path: when fireSchedule completes without error, next_run_at must
// be advanced as part of the normal UPDATE (not via the panic path).
//
// This ensures the #1029 panic-recovery change didn't accidentally break the
// normal flow where both the proxy call and the post-fire UPDATE succeed.
func TestFireSchedule_NormalSuccess_AdvancesNextRunAt(t *testing.T) {
	mock := setupTestDB(t)

	sched := scheduleRow{
		ID:          "ccc33333-3333-3333-3333-333333333333",
		WorkspaceID: "ddd44444-4444-4444-4444-444444444444",
		Name:        "normal-advance-test",
		CronExpr:    "30 * * * *", // every hour at :30
		Timezone:    "UTC",
		Prompt:      "do work",
	}

	// 1. active_tasks check → workspace idle
	mock.ExpectQuery(`SELECT COALESCE`).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))

	// 2. #795 consecutive_empty_runs reset — successProxy returns {"ok":true}
	//    which is non-empty, so the counter is reset to 0.
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 3. #1696 consecutive_sdk_errors reset — successProxy response has no
	//    result_kind in the body, so detectResultKind returns "" and lastStatus
	//    is "ok" → we hit the SDK-error counter reset branch.
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 4. Normal UPDATE after successful proxy call.
	//    Args: $1=sched.ID, $2=nextRunPtr (computed time), $3=lastStatus, $4=lastError
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID, sqlmock.AnyArg(), "ok", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 4. activity_logs INSERT
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(sched.WorkspaceID, sqlmock.AnyArg(), sqlmock.AnyArg(), "ok", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(&successProxy{}, nil)
	s.fireSchedule(context.Background(), sched)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v\n"+
			"Normal fire must still advance next_run_at via the post-fire UPDATE", err)
	}
}

// TestRecordSkipped_AdvancesNextRunAt verifies that when a workspace is busy
// and the cron fire is skipped, recordSkipped advances next_run_at so the
// schedule doesn't re-fire on the very next tick.
//
// This is the third leg of the #1029 invariant: fire, panic, AND skip must
// all advance next_run_at.
//
// We call recordSkipped directly rather than going through fireSchedule
// because #969 added a deferral loop (poll every 10s for up to 2 min) that
// makes end-to-end testing via fireSchedule impractical with sqlmock.
func TestRecordSkipped_AdvancesNextRunAt(t *testing.T) {
	mock := setupTestDB(t)

	sched := scheduleRow{
		ID:          "eee55555-5555-5555-5555-555555555555",
		WorkspaceID: "fff66666-6666-6666-6666-666666666666",
		Name:        "skipped-advance-test",
		CronExpr:    "15 * * * *", // every hour at :15
		Timezone:    "UTC",
		Prompt:      "skipped work",
	}

	// 1. recordSkipped UPDATE — must include next_run_at ($2) and reason ($3).
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 2. activity_logs INSERT for the skip event
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(sched.WorkspaceID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(&successProxy{}, nil)
	// Call recordSkipped directly — simulates the skip path when workspace is busy.
	s.recordSkipped(context.Background(), sched, 2)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v\n"+
			"recordSkipped must advance next_run_at when workspace is busy (#1029)", err)
	}
}

// trigger CI

// ── TestDetectResultKind ───────────────────────────────────────────────────────

// TestDetectResultKind covers the SDK error detection path: HTTP 200 responses
// with non-ok result_kind in the body must be recognised and returned as the
// kind string, not silently treated as ok.
func TestDetectResultKind(t *testing.T) {
	// The test exercises detectResultKind directly so we don't need a full
	// fireSchedule mock for this unit-test level.
	tests := []struct {
		name     string
		body     string
		wantKind string
	}{
		{
			name:     "clean ok response — empty body",
			body:     `{}`,
			wantKind: "",
		},
		{
			name:     "clean ok response — result.kind absent",
			body:     `{"result":{"parts":[{"text":"hello"}]}}`,
			wantKind: "",
		},
		{
			name:     "clean ok response — result.kind=ok",
			body:     `{"result":{"kind":"ok","parts":[{"text":"hello"}]}}`,
			wantKind: "",
		},
		{
			name:     "clean ok response — result.result_kind=ok",
			body:     `{"result":{"result_kind":"ok","parts":[{"text":"hello"}]}}`,
			wantKind: "",
		},
		{
			// REGRESSION GUARD: A2A-SDK canonical Message envelope.
			// Pre-fix, every successful agent reply was mis-flagged as an SDK
			// error (PM scheduler hit 21 consecutive false-failure ticks before
			// auto-disable; canvas screenshot 2026-05-23).
			name:     "clean ok response — result.kind=message (A2A Message envelope)",
			body:     `{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"hello"}]},"id":"1"}`,
			wantKind: "",
		},
		{
			name:     "clean ok response — result.result_kind=message",
			body:     `{"result":{"result_kind":"message","parts":[{"text":"hello"}]}}`,
			wantKind: "",
		},
		{
			// A2A Task envelope, in-progress — `status.state` discriminator is
			// `submitted` or `working` → treat as clean (not an SDK error).
			name:     "clean ok — task envelope state=working",
			body:     `{"result":{"kind":"task","task_id":"abc","status":{"state":"working"}}}`,
			wantKind: "",
		},
		{
			name:     "clean ok — task envelope state=submitted",
			body:     `{"result":{"kind":"task","status":{"state":"submitted"}}}`,
			wantKind: "",
		},
		{
			name:     "clean ok — task envelope state=completed",
			body:     `{"result":{"kind":"task","status":{"state":"completed"}}}`,
			wantKind: "",
		},
		{
			// Conservative: missing status.state → don't fire false-failure.
			name:     "clean ok — task envelope no status block",
			body:     `{"result":{"kind":"task","task_id":"abc"}}`,
			wantKind: "",
		},
		{
			// REGRESSION GUARD: terminal failure states MUST propagate as last_status.
			// Without taskOKStates gating, the blanket "task" allowlist would have
			// masked these — CR2 review feedback on #1716.
			name:     "SDK error — task envelope state=failed",
			body:     `{"result":{"kind":"task","status":{"state":"failed"}}}`,
			wantKind: "failed",
		},
		{
			name:     "SDK error — task envelope state=canceled",
			body:     `{"result":{"kind":"task","status":{"state":"canceled"}}}`,
			wantKind: "canceled",
		},
		{
			name:     "SDK error — task envelope state=rejected",
			body:     `{"result":{"kind":"task","status":{"state":"rejected"}}}`,
			wantKind: "rejected",
		},
		{
			name:     "SDK error — result.kind=rate_limited",
			body:     `{"result":{"kind":"rate_limited","parts":[{"text":"error"}]}}`,
			wantKind: "rate_limited",
		},
		{
			name:     "SDK error — result.kind=quota_exhausted",
			body:     `{"result":{"kind":"quota_exhausted"}}`,
			wantKind: "quota_exhausted",
		},
		{
			name:     "SDK error — result.kind=sdk_error",
			body:     `{"result":{"kind":"sdk_error"}}`,
			wantKind: "sdk_error",
		},
		{
			name:     "SDK error — result.result_kind=rate_limited",
			body:     `{"result":{"result_kind":"rate_limited"}}`,
			wantKind: "rate_limited",
		},
		{
			name:     "SDK error — error string with rate limit",
			body:     `{"result":{"parts":[]},"error":"An error occurred: rate limit exceeded"}`,
			wantKind: "rate_limited",
		},
		{
			name:     "SDK error — error string with max-plan",
			body:     `{"error":"Max-plan rate limit reached"}`,
			wantKind: "quota_exhausted",
		},
		{
			name:     "SDK error — error string with quota",
			body:     `{"error":"quota exhausted for model"}`,
			wantKind: "quota_exhausted",
		},
		{
			name:     "SDK error — error string with sdk error",
			body:     `{"error":"Claude Code returned an error result: success"}`,
			wantKind: "sdk_error",
		},
		{
			name:     "SDK error — error string with api key",
			body:     `{"error":"invalid API key"}`,
			wantKind: "sdk_error",
		},
		{
			name:     "unknown error string — not an SDK error",
			body:     `{"error":"something went wrong"}`,
			wantKind: "",
		},
		{
			name:     "empty response body",
			body:     ``,
			wantKind: "",
		},
		{
			name:     "malformed JSON",
			body:     `not valid json`,
			wantKind: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectResultKind([]byte(tc.body))
			if got != tc.wantKind {
				t.Errorf("detectResultKind(%q) = %q, want %q", tc.body, got, tc.wantKind)
			}
		})
	}
}

// ── TestFireSchedule_SDKError_RateLimited (#1696) ───────────────────────────────
//
// When ProxyA2ARequest returns HTTP 200 but the response body contains a
// non-ok result_kind, fireSchedule must:
//  1. Set last_status to the result_kind (not 'ok').
//  2. Set last_error to describe the SDK error.
//  3. Increment consecutive_sdk_errors.
//  4. NOT auto-disable on first occurrence (threshold is 3).
//
// This test uses an sdkErrorProxy that returns a rate-limited body and asserts
// the first run is recorded as 'rate_limited' with consecutive_sdk_errors=1
// and enabled=true.
func TestFireSchedule_SDKError_RateLimited(t *testing.T) {
	mock := setupTestDB(t)

	sched := scheduleRow{
		ID:          "sdk1-test-sched-0001",
		WorkspaceID: "sdk1-test-workspace1",
		Name:        "rate-limited-job",
		CronExpr:    "0 * * * *",
		Timezone:    "UTC",
		Prompt:      "do work",
	}

	// 1. active_tasks check → workspace idle
	mock.ExpectQuery(`SELECT COALESCE`).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))

	// 2. #1696 consecutive_sdk_errors bump — RETURNING gives us count=1.
	//    Use ExpectQuery (not Exec) because QueryRowContext + RETURNING
	//    produces a result set consumed via .Scan().
	mock.ExpectQuery(`UPDATE workspace_schedules`).
		WithArgs(sched.ID).
		WillReturnRows(sqlmock.NewRows([]string{"consecutive_sdk_errors"}).AddRow(1))

	// 3. Post-fire UPDATE — last_status='rate_limited', last_error='SDK error: result_kind=rate_limited'
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID, sqlmock.AnyArg(), "rate_limited", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 4. activity_logs INSERT
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(sched.WorkspaceID, sqlmock.AnyArg(), sqlmock.AnyArg(), "rate_limited", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(&sdkErrorProxy{kind: "rate_limited"}, nil)
	s.fireSchedule(context.Background(), sched)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations for SDK-error first run: %v", err)
	}
}

// ── TestFireSchedule_SDKError_AutoDisableOnThirdConsecutive (#1696) ───────────
//
// On the 3rd consecutive SDK error, fireSchedule must auto-disable the
// schedule (enabled=false) in addition to recording the error status.
// Threshold is 3 per #1696 requirement.
func TestFireSchedule_SDKError_AutoDisableOnThirdConsecutive(t *testing.T) {
	mock := setupTestDB(t)

	sched := scheduleRow{
		ID:          "sdk2-test-sched-0002",
		WorkspaceID: "sdk2-test-workspace2",
		Name:        "auto-disable-job",
		CronExpr:    "0 * * * *",
		Timezone:    "UTC",
		Prompt:      "do work",
	}

	// 1. active_tasks check → workspace idle
	mock.ExpectQuery(`SELECT COALESCE`).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))

	// 2. #1696 consecutive_sdk_errors bump — RETURNING gives count=3 (threshold met).
	//    Use ExpectQuery (not Exec) because QueryRowContext + RETURNING
	//    produces a result set consumed via .Scan().
	mock.ExpectQuery(`UPDATE workspace_schedules`).
		WithArgs(sched.ID).
		WillReturnRows(sqlmock.NewRows([]string{"consecutive_sdk_errors"}).AddRow(3))

	// 3. Auto-disable UPDATE — sets enabled=false (schedule has hit 3rd SDK error)
	mock.ExpectExec(`UPDATE workspace_schedules SET enabled`).
		WithArgs(sched.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 4. Post-fire UPDATE
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID, sqlmock.AnyArg(), "rate_limited", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 5. activity_logs INSERT
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(sched.WorkspaceID, sqlmock.AnyArg(), sqlmock.AnyArg(), "rate_limited", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(&sdkErrorProxy{kind: "rate_limited"}, nil)
	s.fireSchedule(context.Background(), sched)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations for SDK-error auto-disable: %v", err)
	}
}

// ── TestFireSchedule_SDKError_CounterResetOnCleanRun (#1696) ──────────────────
//
// A clean HTTP-200 run (no SDK error) must reset consecutive_sdk_errors to 0.
// This prevents false auto-disable after intermittent SDK blips.
func TestFireSchedule_SDKError_CounterResetOnCleanRun(t *testing.T) {
	mock := setupTestDB(t)

	sched := scheduleRow{
		ID:          "sdk3-test-sched-0003",
		WorkspaceID: "sdk3-test-workspace3",
		Name:        "clean-reset-job",
		CronExpr:    "30 * * * *",
		Timezone:    "UTC",
		Prompt:      "do work",
	}

	// 1. active_tasks check → workspace idle
	mock.ExpectQuery(`SELECT COALESCE`).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))

	// 2. No SDK error — #1696 counter is reset to 0
	//    (lastStatus is 'ok', resultKind is empty, so we go to the reset branch)
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 3. Post-fire UPDATE
	mock.ExpectExec(`UPDATE workspace_schedules`).
		WithArgs(sched.ID, sqlmock.AnyArg(), "ok", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 4. activity_logs INSERT
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(sched.WorkspaceID, sqlmock.AnyArg(), sqlmock.AnyArg(), "ok", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := New(&successProxy{}, nil)
	s.fireSchedule(context.Background(), sched)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations for SDK-error counter reset: %v", err)
	}
}

// ── sdkErrorProxy ──────────────────────────────────────────────────────────────
//
// sdkErrorProxy is a test double whose ProxyA2ARequest returns HTTP 200 but
// embeds a non-ok result_kind in the response body, simulating a Claude Code
// SDK that returned 200 but the inner LLM call threw a rate-limit / quota error.
// Used by TestFireSchedule_SDKError_* to cover #1696 SDK error detection.
type sdkErrorProxy struct {
	kind string // result_kind value to embed in the response body
}

func (p *sdkErrorProxy) ProxyA2ARequest(
	_ context.Context, _ string, _ []byte, _ string, _ bool,
) (int, []byte, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"kind":  p.kind,
			"parts": []map[string]interface{}{{"kind": "text", "text": "(no response generated)"}},
		},
	})
	return 200, body, nil
}

// EnqueueA2A satisfies the extended A2AProxy interface.
func (p *sdkErrorProxy) EnqueueA2A(
	_ context.Context, _ string, _ string, _ int, _ []byte, _ string, _ string, _ *time.Time,
) (string, int, error) {
	return "q-sdkerr", 1, nil
}

// ── TestTruncate_utf8Safe_regression2026 ──────────────────────────────────────

// TestTruncate_utf8Safe_regression2026 locks in the #2026 fix: truncate must
// never split a multi-byte UTF-8 rune. Before the fix, a prompt whose byte-197
// landed mid-rune (e.g. U+2026 `…` = 0xe2 0x80 0xa6) would be sliced at
// maxLen-3 and produce the sequence 0xe2 0x80 0x2e when concatenated with
// "...", which Postgres rejects as invalid UTF-8 — wedging the activity_logs
// INSERT and stalling the entire scheduler.
func TestTruncate_utf8Safe_regression2026(t *testing.T) {
	// Build a prompt where the byte at position 197 is the middle of the
	// 3-byte rune U+2026 (`…`). With maxLen=200 the pre-fix code slices at
	// byte 197 (maxLen-3), which lands on `0x80` — a continuation byte.
	filler := ""
	for len(filler) < 195 {
		filler += "a"
	}
	input := filler + "…xxx" // 195 ASCII + 3-byte rune + 3 trailing
	out := textutil.TruncateBytes(input, 200)

	if !utf8.ValidString(out) {
		t.Fatalf("truncate produced invalid UTF-8: %x", []byte(out))
	}
	// Must not contain the 0xe2 0x80 0x2e wedge sequence (partial rune
	// followed by the "..." suffix).
	for i := 0; i < len(out)-2; i++ {
		if out[i] == 0xe2 && out[i+1] == 0x80 && out[i+2] == 0x2e {
			t.Fatalf("truncate produced the 0xe2 0x80 0x2e wedge sequence at byte %d", i)
		}
	}
	if len(out) > 200 {
		t.Fatalf("truncate returned %d bytes, want <= 200", len(out))
	}
}

// ── TestSanitizeUTF8 ──────────────────────────────────────────────────────────

// TestSanitizeUTF8 confirms sanitizeUTF8 leaves valid UTF-8 unchanged and
// replaces invalid sequences with the Unicode replacement character.
func TestSanitizeUTF8(t *testing.T) {
	// Valid UTF-8 passes through unchanged.
	valid := "hello … world"
	if got := sanitizeUTF8(valid); got != valid {
		t.Errorf("sanitizeUTF8(valid) = %q, want %q", got, valid)
	}
	// Invalid UTF-8 (orphan continuation byte) is sanitized.
	bad := "hello \x80 world"
	out := sanitizeUTF8(bad)
	if !utf8.ValidString(out) {
		t.Errorf("sanitizeUTF8 did not produce valid UTF-8: %x", []byte(out))
	}
}

// ── TestClassifyTaskState ───────────────────────────────────────────────────

func TestClassifyTaskState_NoStatus(t *testing.T) {
	result := map[string]json.RawMessage{"other": json.RawMessage(`"x"`)}
	if got := classifyTaskState(result); got != "" {
		t.Errorf("classifyTaskState(no status) = %q, want empty", got)
	}
}

func TestClassifyTaskState_OKStates(t *testing.T) {
	for _, state := range []string{"", "submitted", "working", "completed"} {
		result := map[string]json.RawMessage{
			"status": json.RawMessage(`{"state":"` + state + `"}`),
		}
		if got := classifyTaskState(result); got != "" {
			t.Errorf("classifyTaskState(%q) = %q, want empty (OK state)", state, got)
		}
	}
}

func TestClassifyTaskState_FailureState(t *testing.T) {
	result := map[string]json.RawMessage{
		"status": json.RawMessage(`{"state":"failed"}`),
	}
	if got := classifyTaskState(result); got != "failed" {
		t.Errorf("classifyTaskState(failed) = %q, want failed", got)
	}
}

func TestClassifyTaskState_MalformedStatus(t *testing.T) {
	result := map[string]json.RawMessage{
		"status": json.RawMessage(`{broken`),
	}
	if got := classifyTaskState(result); got != "" {
		t.Errorf("classifyTaskState(malformed) = %q, want empty", got)
	}
}

// ── TestIsEmptyResponse ─────────────────────────────────────────────────────

func TestIsEmptyResponse_EmptyBody(t *testing.T) {
	if !isEmptyResponse([]byte{}) {
		t.Error("isEmptyResponse(empty) should be true")
	}
}

func TestIsEmptyResponse_NoResponseGenerated(t *testing.T) {
	if !isEmptyResponse([]byte(`(no response generated)`)) {
		t.Error("isEmptyResponse(no-response-generated) should be true")
	}
}

func TestIsEmptyResponse_TextFieldEmpty(t *testing.T) {
	if !isEmptyResponse([]byte(`{"result":{"parts":[{"text":""}]}}`)) {
		t.Error("isEmptyResponse(empty text field) should be true")
	}
}

func TestIsEmptyResponse_TextFieldNoResponse(t *testing.T) {
	if !isEmptyResponse([]byte(`{"result":{"parts":[{"text":"(no response generated)"}]}}`)) {
		t.Error("isEmptyResponse(text=no-response-generated) should be true")
	}
}

func TestIsEmptyResponse_HasContent(t *testing.T) {
	if isEmptyResponse([]byte(`{"result":{"parts":[{"text":"hello"}]}}`)) {
		t.Error("isEmptyResponse(with content) should be false")
	}
}

// ── TestA2AErrorFromBody ────────────────────────────────────────────────────

func TestA2AErrorFromBody_Empty(t *testing.T) {
	if got := a2aErrorFromBody([]byte{}); got != "" {
		t.Errorf("a2aErrorFromBody(empty) = %q, want empty", got)
	}
}

func TestA2AErrorFromBody_JSONRPCMessage(t *testing.T) {
	body := []byte(`{"error":{"code":-32603,"message":"internal error"}}`)
	if got := a2aErrorFromBody(body); got != "internal error" {
		t.Errorf("a2aErrorFromBody(JSON-RPC) = %q, want internal error", got)
	}
}

func TestA2AErrorFromBody_PlainString(t *testing.T) {
	body := []byte(`{"error":"something went wrong"}`)
	if got := a2aErrorFromBody(body); got != "something went wrong" {
		t.Errorf("a2aErrorFromBody(plain) = %q, want something went wrong", got)
	}
}

func TestA2AErrorFromBody_NoError(t *testing.T) {
	body := []byte(`{"result":"ok"}`)
	if got := a2aErrorFromBody(body); got != "" {
		t.Errorf("a2aErrorFromBody(no error) = %q, want empty", got)
	}
}

func TestA2AErrorFromBody_InvalidJSON(t *testing.T) {
	body := []byte(`{broken`)
	if got := a2aErrorFromBody(body); got != "" {
		t.Errorf("a2aErrorFromBody(invalid) = %q, want empty", got)
	}
}
