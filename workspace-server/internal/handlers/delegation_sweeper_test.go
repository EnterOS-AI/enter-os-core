package handlers

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// delegation_sweeper_test.go — coverage for the RFC #2829 PR-3 stuck-task
// sweeper. Validates:
//
//   1. Deadline-exceeded rows are marked failed.
//   2. Heartbeat-stale rows (lastBeat older than threshold) are marked stuck.
//   3. NULL last_heartbeat is NOT marked stuck (free first-beat pass).
//   4. Healthy in-flight rows (recent heartbeat, future deadline) are
//      left alone.
//   5. Empty in-flight set is a clean no-op.
//   6. Both rules apply in one sweep without double-marking.
//   7. Env-override interval/threshold parse correctly + fall back on
//      invalid input.

func TestSweeper_HappyPath_NoInflightRowsIsCleanNoOp(t *testing.T) {
	mock := setupTestDB(t)
	ledger := NewDelegationLedger(nil)
	sw := NewDelegationSweeper(nil, ledger)

	mock.ExpectQuery(`SELECT delegation_id, last_heartbeat, deadline\s+FROM delegations`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "last_heartbeat", "deadline"}))

	res := sw.Sweep(context.Background())
	if res.DeadlineFailures != 0 || res.StuckMarked != 0 || res.Errors != 0 {
		t.Errorf("empty in-flight set must produce zero changes; got %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweeper_DeadlineExceededIsMarkedFailed(t *testing.T) {
	mock := setupTestDB(t)
	ledger := NewDelegationLedger(nil)
	sw := NewDelegationSweeper(nil, ledger)

	past := time.Now().Add(-1 * time.Minute)
	mock.ExpectQuery(`SELECT delegation_id, last_heartbeat, deadline\s+FROM delegations`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "last_heartbeat", "deadline"}).
			AddRow("deleg-overdue", time.Now(), past))

	// SetStatus reads current status...
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-overdue").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("in_progress"))
	// ...then updates to failed.
	mock.ExpectExec(`UPDATE delegations`).
		WithArgs("deleg-overdue", "failed", "deadline exceeded by sweeper", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res := sw.Sweep(context.Background())
	if res.DeadlineFailures != 1 {
		t.Errorf("expected 1 deadline failure, got %d", res.DeadlineFailures)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweeper_StaleHeartbeatIsMarkedStuck(t *testing.T) {
	mock := setupTestDB(t)
	ledger := NewDelegationLedger(nil)
	sw := NewDelegationSweeper(nil, ledger)

	// Last heartbeat 30min ago — well past the 10min default threshold.
	staleBeat := time.Now().Add(-30 * time.Minute)
	future := time.Now().Add(2 * time.Hour) // deadline NOT exceeded

	mock.ExpectQuery(`SELECT delegation_id, last_heartbeat, deadline\s+FROM delegations`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "last_heartbeat", "deadline"}).
			AddRow("deleg-stuck", staleBeat, future))

	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-stuck").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("in_progress"))

	// We can't predict the exact "no heartbeat for Xs" string because
	// the suffix depends on now() at sweep time; just match against any.
	mock.ExpectExec(`UPDATE delegations`).
		WithArgs("deleg-stuck", "stuck", sqlmock.AnyArg(), "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res := sw.Sweep(context.Background())
	if res.StuckMarked != 1 {
		t.Errorf("expected 1 stuck mark, got %d", res.StuckMarked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweeper_NullHeartbeatIsLeftAlone(t *testing.T) {
	// A delegation that was JUST inserted (queued, no heartbeat yet) must
	// not be flipped to stuck on the first sweep — give it the chance to
	// emit its first beat.
	mock := setupTestDB(t)
	ledger := NewDelegationLedger(nil)
	sw := NewDelegationSweeper(nil, ledger)

	future := time.Now().Add(2 * time.Hour)
	mock.ExpectQuery(`SELECT delegation_id, last_heartbeat, deadline\s+FROM delegations`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "last_heartbeat", "deadline"}).
			AddRow("deleg-fresh", sql.NullTime{}, future))

	res := sw.Sweep(context.Background())
	if res.StuckMarked != 0 {
		t.Errorf("NULL heartbeat must not be stuck-marked; got %d", res.StuckMarked)
	}
	if res.DeadlineFailures != 0 {
		t.Errorf("future deadline must not fail; got %d", res.DeadlineFailures)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweeper_HealthyInflightRowsAreLeftAlone(t *testing.T) {
	mock := setupTestDB(t)
	ledger := NewDelegationLedger(nil)
	sw := NewDelegationSweeper(nil, ledger)

	freshBeat := time.Now().Add(-1 * time.Minute) // well within 10min threshold
	future := time.Now().Add(2 * time.Hour)

	mock.ExpectQuery(`SELECT delegation_id, last_heartbeat, deadline\s+FROM delegations`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "last_heartbeat", "deadline"}).
			AddRow("deleg-healthy", freshBeat, future))

	res := sw.Sweep(context.Background())
	if res.DeadlineFailures != 0 || res.StuckMarked != 0 {
		t.Errorf("healthy row must produce zero changes; got %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweeper_DeadlineFiresFirstNotStuck(t *testing.T) {
	// Row that's BOTH past deadline AND stale-heartbeat must be marked
	// failed (deadline) not stuck — deadline is the stronger statement.
	mock := setupTestDB(t)
	ledger := NewDelegationLedger(nil)
	sw := NewDelegationSweeper(nil, ledger)

	staleBeat := time.Now().Add(-30 * time.Minute)
	past := time.Now().Add(-5 * time.Minute)

	mock.ExpectQuery(`SELECT delegation_id, last_heartbeat, deadline\s+FROM delegations`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "last_heartbeat", "deadline"}).
			AddRow("deleg-both", staleBeat, past))

	// Only the failed transition fires; no stuck transition.
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-both").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("in_progress"))
	mock.ExpectExec(`UPDATE delegations`).
		WithArgs("deleg-both", "failed", "deadline exceeded by sweeper", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res := sw.Sweep(context.Background())
	if res.DeadlineFailures != 1 || res.StuckMarked != 0 {
		t.Errorf("expected 1 deadline failure, 0 stuck; got %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (stuck UPDATE may have fired by accident): %v", err)
	}
}

func TestSweeper_MixedSetAppliesBothRules(t *testing.T) {
	mock := setupTestDB(t)
	ledger := NewDelegationLedger(nil)
	sw := NewDelegationSweeper(nil, ledger)

	now := time.Now()
	mock.ExpectQuery(`SELECT delegation_id, last_heartbeat, deadline\s+FROM delegations`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "last_heartbeat", "deadline"}).
			AddRow("deleg-overdue", now, now.Add(-1*time.Minute)).      // deadline → failed
			AddRow("deleg-stuck", now.Add(-30*time.Minute), now.Add(2*time.Hour)). // stale → stuck
			AddRow("deleg-healthy", now.Add(-30*time.Second), now.Add(2*time.Hour)), // healthy → no-op
		)

	// 1st: deadline → failed
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-overdue").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("in_progress"))
	mock.ExpectExec(`UPDATE delegations`).
		WithArgs("deleg-overdue", "failed", "deadline exceeded by sweeper", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 2nd: stale → stuck
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-stuck").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("in_progress"))
	mock.ExpectExec(`UPDATE delegations`).
		WithArgs("deleg-stuck", "stuck", sqlmock.AnyArg(), "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 3rd: healthy — no SQL fired

	res := sw.Sweep(context.Background())
	if res.DeadlineFailures != 1 || res.StuckMarked != 1 {
		t.Errorf("expected 1 failure + 1 stuck, got %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweeper_TerminalReplayFromConcurrentCompletionIsIgnored(t *testing.T) {
	// Edge case: row was marked completed by UpdateStatus between the
	// SELECT and the SetStatus call. SetStatus's forward-only protection
	// returns ErrInvalidTransition; sweeper increments Errors but the
	// row is correctly left in completed state.
	mock := setupTestDB(t)
	ledger := NewDelegationLedger(nil)
	sw := NewDelegationSweeper(nil, ledger)

	past := time.Now().Add(-1 * time.Minute)
	mock.ExpectQuery(`SELECT delegation_id, last_heartbeat, deadline\s+FROM delegations`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "last_heartbeat", "deadline"}).
			AddRow("deleg-raced", time.Now(), past))

	// SetStatus's status read finds the row already completed (concurrent UpdateStatus won).
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-raced").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("completed"))
	// No UPDATE — terminal forward-only blocks it.

	res := sw.Sweep(context.Background())
	if res.Errors != 1 {
		t.Errorf("forward-only block must surface as Error count; got %+v", res)
	}
	if res.DeadlineFailures != 0 {
		t.Errorf("must NOT credit a deadline failure that didn't fire; got %d", res.DeadlineFailures)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// ---------- env override parsing ----------

func TestEnvDuration_Default(t *testing.T) {
	t.Setenv("MY_TEST_KEY", "")
	if got := envDuration("MY_TEST_KEY", 7*time.Second); got != 7*time.Second {
		t.Errorf("expected default 7s, got %v", got)
	}
}

func TestEnvDuration_ParsesPositiveSeconds(t *testing.T) {
	t.Setenv("MY_TEST_KEY", "42")
	if got := envDuration("MY_TEST_KEY", 1*time.Second); got != 42*time.Second {
		t.Errorf("expected 42s, got %v", got)
	}
}

func TestEnvDuration_FallsBackOnInvalid(t *testing.T) {
	t.Setenv("MY_TEST_KEY", "garbage")
	if got := envDuration("MY_TEST_KEY", 5*time.Second); got != 5*time.Second {
		t.Errorf("invalid input must fall back to default; got %v", got)
	}
}

func TestEnvDuration_FallsBackOnNegative(t *testing.T) {
	t.Setenv("MY_TEST_KEY", "-10")
	if got := envDuration("MY_TEST_KEY", 5*time.Second); got != 5*time.Second {
		t.Errorf("negative must fall back to default; got %v", got)
	}
}

// TestSweeperConstructor_PicksUpEnvOverrides — interval + threshold env
// vars are read at construction time. Confirms the wiring contract; if
// somebody adds a new env var without plumbing it, this fails.
func TestSweeperConstructor_PicksUpEnvOverrides(t *testing.T) {
	t.Setenv("DELEGATION_SWEEPER_INTERVAL_S", "60")
	t.Setenv("DELEGATION_STUCK_THRESHOLD_S", "120")

	mock := setupTestDB(t)
	_ = mock // unused — constructor doesn't fire SQL
	sw := NewDelegationSweeper(nil, nil)

	if sw.Interval() != 60*time.Second {
		t.Errorf("interval override not picked up: got %v", sw.Interval())
	}
	if sw.Threshold() != 120*time.Second {
		t.Errorf("threshold override not picked up: got %v", sw.Threshold())
	}
}

func TestSweeperConstructor_DefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("DELEGATION_SWEEPER_INTERVAL_S", "")
	t.Setenv("DELEGATION_STUCK_THRESHOLD_S", "")

	mock := setupTestDB(t)
	_ = mock
	sw := NewDelegationSweeper(nil, nil)

	if sw.Interval() != defaultSweeperInterval {
		t.Errorf("default interval not used: got %v", sw.Interval())
	}
	if sw.Threshold() != defaultStuckThreshold {
		t.Errorf("default threshold not used: got %v", sw.Threshold())
	}
}
