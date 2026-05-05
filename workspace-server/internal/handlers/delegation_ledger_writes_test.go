package handlers

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// delegation_ledger_writes_test.go — RFC #2829 #318 wiring tests.
//
// Scope:
//   - flag off (default) → no ledger SQL fires
//   - flag on, recordLedgerInsert → INSERT INTO delegations
//   - flag on, recordLedgerStatus on lifecycle transitions
//   - flag on, recordLedgerStatus on terminal-state replay → no UPDATE
//
// We test the gate functions in isolation rather than re-asserting the
// full handler test surface (Delegate/Record/UpdateStatus) — those are
// already pinned by delegation_test.go (30 tests) and exercising the
// flag-on path through them would force adding ~20 ExpectExec stanzas
// to existing tests. That refactor lands separately when we're ready
// to flip the flag default to on.

func TestLedgerWritesEnabled_FlagOff(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "")
	if ledgerWritesEnabled() {
		t.Errorf("flag off must report disabled")
	}
}

func TestLedgerWritesEnabled_FlagOn(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	if !ledgerWritesEnabled() {
		t.Errorf("flag on must report enabled")
	}
}

func TestLedgerWritesEnabled_RejectsLooseTruthyValues(t *testing.T) {
	// Only "1" is the on signal — "true", "yes", anything else is
	// off. This matches the existing PR-2 + PR-5 flag conventions
	// (DELEGATION_RESULT_INBOX_PUSH, DELEGATION_SYNC_VIA_INBOX).
	for _, v := range []string{"true", "yes", "TRUE", "0", "on"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("DELEGATION_LEDGER_WRITE", v)
			if ledgerWritesEnabled() {
				t.Errorf("value %q must NOT enable the flag (only \"1\" does)", v)
			}
		})
	}
}

func TestRecordLedgerInsert_FlagOff_NoSQL(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "")

	recordLedgerInsert(context.Background(),
		"caller", "callee", "deleg-1", "task body", "")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("flag off must fire no SQL: %v", err)
	}
}

func TestRecordLedgerInsert_FlagOn_FiresInsert(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	mock.ExpectExec(`INSERT INTO delegations`).
		WithArgs(
			"deleg-1", "caller", "callee", "task body",
			sqlmock.AnyArg(), // deadline
			sqlmock.AnyArg(), // idempotency_key NullString
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	recordLedgerInsert(context.Background(),
		"caller", "callee", "deleg-1", "task body", "")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestRecordLedgerStatus_FlagOff_NoSQL(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "")

	recordLedgerStatus(context.Background(), "deleg-1", "dispatched", "", "")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("flag off must fire no SQL: %v", err)
	}
}

func TestRecordLedgerStatus_FlagOn_FiresUpdate(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// SetStatus reads current status first (forward-only protection).
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("queued"))
	// Then UPDATEs.
	mock.ExpectExec(`UPDATE delegations`).
		WithArgs("deleg-1", "dispatched", "", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	recordLedgerStatus(context.Background(), "deleg-1", "dispatched", "", "")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestRecordLedgerStatus_FlagOn_TerminalReplaySwallowsErr(t *testing.T) {
	// SetStatus returns ErrInvalidTransition when called on a terminal
	// row. recordLedgerStatus must swallow that — the legacy path is
	// authoritative; ledger replay error is not a delegation failure.
	mock := setupTestDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// Row already completed — SELECT returns "completed".
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("completed"))
	// No UPDATE expected — terminal forward-only protection blocks it.

	// Should NOT panic / propagate; mock's ExpectationsWereMet is the
	// behavior assertion — if SetStatus tried to UPDATE, the unset
	// expectation would catch it.
	recordLedgerStatus(context.Background(), "deleg-1", "failed", "post-hoc", "")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("terminal-replay must not fire UPDATE: %v", err)
	}
}
