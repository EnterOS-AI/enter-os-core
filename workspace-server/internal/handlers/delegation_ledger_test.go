package handlers

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/DATA-DOG/go-sqlmock"
)

// delegation_ledger_test.go — unit coverage for the durable ledger writer
// (RFC #2829 PR-1).
//
// Coverage targets:
//   - Insert: happy path; missing-required no-op; deadline default;
//     idempotency_key NULL vs string passthrough.
//   - SetStatus: queued→dispatched→in_progress→completed; same-status
//     replay no-op; terminal state forward-only protection; missing row
//     no-op; SQL error propagation.
//   - Heartbeat: stamps now() on in-flight; no-op on terminal; missing-id
//     guard.
//   - truncatePreview: under-cap passthrough; over-cap truncates.

// ---------- Insert ----------

func TestLedgerInsert_HappyPath(t *testing.T) {
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil) // uses package db.DB which sqlmock replaced

	mock.ExpectExec(`INSERT INTO delegations`).
		WithArgs(
			"deleg-123",
			"caller-uuid",
			"callee-uuid",
			"task body",
			sqlmock.AnyArg(), // deadline (default = now+6h)
			sqlmock.AnyArg(), // idempotency_key NullString
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	l.Insert(context.Background(), InsertOpts{
		DelegationID: "deleg-123",
		CallerID:     "caller-uuid",
		CalleeID:     "callee-uuid",
		TaskPreview:  "task body",
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestLedgerInsert_MissingRequired_NoSQLFired(t *testing.T) {
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	// Caller-side guard: no DB call expected.
	for _, opts := range []InsertOpts{
		{DelegationID: "", CallerID: "c", CalleeID: "ca", TaskPreview: "t"},
		{DelegationID: "d", CallerID: "", CalleeID: "ca", TaskPreview: "t"},
		{DelegationID: "d", CallerID: "c", CalleeID: "", TaskPreview: "t"},
	} {
		l.Insert(context.Background(), opts)
	}
	// No ExpectExec → ExpectationsWereMet stays clean.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected sqlmock activity: %v", err)
	}
}

func TestLedgerInsert_TruncatesOversizedPreview(t *testing.T) {
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	// 4096 / 3 = 1365 runes; +10 for margin so we cross the cap.
	// '世' is 3 bytes in UTF-8 (worst case for byte-cap rune walking).
	huge := strings.Repeat("世", (previewCap/3)+10)
	if len(huge) <= previewCap {
		t.Fatalf("test setup: input too short (%d bytes) — must exceed previewCap=%d", len(huge), previewCap)
	}

	mock.ExpectExec(`INSERT INTO delegations`).
		WithArgs(
			"deleg-big",
			"c", "ca",
			capValidUTF8Matcher{cap: previewCap}, // truncated preview must fit cap AND be valid UTF-8
			sqlmock.AnyArg(),                     // deadline
			sqlmock.AnyArg(),                     // idempotency_key
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	l.Insert(context.Background(), InsertOpts{
		DelegationID: "deleg-big",
		CallerID:     "c",
		CalleeID:     "ca",
		TaskPreview:  huge,
	})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// capValidUTF8Matcher pins #2962 at the integration boundary: the
// preview that lands in the INSERT MUST be valid UTF-8 (else Postgres
// JSONB rejects → silent audit gap) AND fit within the byte cap. Pre-
// migration this would have asserted on the corrupted "世" mid-codepoint
// byte slice; post-migration it asserts the truncated preview is a
// clean rune-aligned prefix.
type capValidUTF8Matcher struct{ cap int }

func (m capValidUTF8Matcher) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	return len(s) <= m.cap && utf8.ValidString(s)
}

// Helper-level truncation tests now live in
// internal/textutil/truncate_test.go. The integration-level path
// (TestLedgerInsert_TruncatesOversizedPreview above) still exercises
// the previewCap boundary through the SQL write so a regression in
// the wiring (wrong cap, wrong helper, missing call) would still go
// red here.

// ---------- SetStatus lifecycle ----------

func TestLedgerSetStatus_QueuedToDispatched(t *testing.T) {
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("d-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("queued"))

	mock.ExpectExec(`UPDATE delegations`).
		WithArgs("d-1", "dispatched", "", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := l.SetStatus(context.Background(), "d-1", "dispatched", "", ""); err != nil {
		t.Errorf("unexpected: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLedgerSetStatus_QueuedToInProgress_SkipsDispatched(t *testing.T) {
	// Lazy callers that go queued → in_progress without ack should be allowed.
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("d-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("queued"))

	mock.ExpectExec(`UPDATE delegations`).
		WithArgs("d-1", "in_progress", "", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := l.SetStatus(context.Background(), "d-1", "in_progress", "", ""); err != nil {
		t.Errorf("unexpected: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLedgerSetStatus_InProgressToCompleted_StoresResult(t *testing.T) {
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("d-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("in_progress"))

	mock.ExpectExec(`UPDATE delegations`).
		WithArgs("d-1", "completed", "", "answer text").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := l.SetStatus(context.Background(), "d-1", "completed", "", "answer text"); err != nil {
		t.Errorf("unexpected: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLedgerSetStatus_TerminalForwardOnly(t *testing.T) {
	// completed → failed must be rejected: terminal states are forward-only.
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("d-done").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("completed"))

	err := l.SetStatus(context.Background(), "d-done", "failed", "post-hoc error", "")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("expected ErrInvalidTransition, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLedgerSetStatus_SameStatusReplay_NoUpdate(t *testing.T) {
	// Re-applying the same terminal status should NOT bump updated_at —
	// duplicate completion notifications shouldn't generate spurious writes.
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("d-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("completed"))

	// No ExpectExec — UPDATE must not fire.
	if err := l.SetStatus(context.Background(), "d-1", "completed", "", ""); err != nil {
		t.Errorf("same-status replay should be no-op, got err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (or unexpected UPDATE): %v", err)
	}
}

func TestLedgerSetStatus_MissingRowIsNoOp(t *testing.T) {
	// A SetStatus call that arrives before Insert (lost INSERT, race, etc.)
	// must NOT error — it's a transient inconsistency the next agent retry
	// will heal.
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("d-missing").
		WillReturnRows(sqlmock.NewRows([]string{"status"})) // empty

	if err := l.SetStatus(context.Background(), "d-missing", "completed", "", "ok"); err != nil {
		t.Errorf("missing row should be no-op; got err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLedgerSetStatus_RejectsEmptyDelegationID(t *testing.T) {
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	if err := l.SetStatus(context.Background(), "", "completed", "", ""); err == nil {
		t.Errorf("expected error for empty delegation_id")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected sqlmock activity for empty input: %v", err)
	}
}

func TestLedgerSetStatus_RejectsEmptyStatus(t *testing.T) {
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	if err := l.SetStatus(context.Background(), "d-1", "", "", ""); err == nil {
		t.Errorf("expected error for empty status")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected sqlmock activity for empty input: %v", err)
	}
}

// ---------- Heartbeat ----------

func TestLedgerHeartbeat_StampsInflightRow(t *testing.T) {
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	mock.ExpectExec(`UPDATE delegations`).
		WithArgs("d-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	l.Heartbeat(context.Background(), "d-1")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLedgerHeartbeat_EmptyIDIsNoOp(t *testing.T) {
	mock := setupTestDB(t)
	l := NewDelegationLedger(nil)

	l.Heartbeat(context.Background(), "") // no SQL expected
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected SQL on empty id: %v", err)
	}
}

// ---------- Allowed-transition table ----------

// TestAllowedTransitionsTableShape pins the lifecycle map: every starting
// state must have at least one outbound transition, and every terminal
// state (completed/failed/stuck) must be ABSENT from the map keys (forward-
// only enforcement). Catches accidental edits that re-add an outbound edge
// from a terminal state.
func TestAllowedTransitionsTableShape(t *testing.T) {
	for _, terminal := range []string{"completed", "failed", "stuck"} {
		if _, has := allowedTransitions[terminal]; has {
			t.Errorf("terminal state %q must not appear as transition source", terminal)
		}
	}
	for src, dests := range allowedTransitions {
		if len(dests) == 0 {
			t.Errorf("non-terminal state %q has no outbound transitions", src)
		}
	}
}
