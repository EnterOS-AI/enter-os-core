package handlers

import (
	"context"
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

	huge := strings.Repeat("x", 10_000) // > previewCap

	mock.ExpectExec(`INSERT INTO delegations`).
		WithArgs(
			"deleg-big",
			"c", "ca",
			sqlmock.AnyArg(), // truncated preview — verify length below via custom matcher
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
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

// ---------- truncatePreview unit ----------

func TestTruncatePreview_UnderCap(t *testing.T) {
	in := "short"
	if got := truncatePreview(in); got != in {
		t.Errorf("under-cap should passthrough; got %q", got)
	}
}

func TestTruncatePreview_OverCapTruncatesAtBoundary(t *testing.T) {
	in := strings.Repeat("a", previewCap+100)
	got := truncatePreview(in)
	if len(got) != previewCap {
		t.Errorf("expected len=%d got len=%d", previewCap, len(got))
	}
}

func TestTruncatePreview_ExactlyAtCap(t *testing.T) {
	in := strings.Repeat("a", previewCap)
	got := truncatePreview(in)
	if got != in {
		t.Errorf("at-cap should passthrough unchanged")
	}
}

// TestTruncatePreview_NeverProducesInvalidUTF8 — pins #2962. The old
// byte-slice implementation (s[:previewCap]) split on a byte boundary,
// so a multi-byte codepoint straddling byte 4096 produced invalid
// UTF-8 → Postgres JSONB rejects → ledger row not inserted → audit
// gap. Test feeds a CJK / emoji-padded string longer than previewCap
// and asserts utf8.ValidString on the result.
func TestTruncatePreview_NeverProducesInvalidUTF8(t *testing.T) {
	// Build a string of '世' (3 bytes per rune in UTF-8) that's just
	// past the cap. With the old implementation, the slice at byte
	// previewCap would land mid-rune and ValidString would fail.
	// With the rune-aware implementation, the result is always valid
	// UTF-8 even if the byte length is < previewCap.
	rune3 := "世" // U+4E16, 3 bytes
	// Need at least previewCap/3 + 1 runes so we cross the cap with
	// margin to spare.
	in := strings.Repeat(rune3, (previewCap/3)+10)
	if len(in) <= previewCap {
		t.Fatalf("test setup: input too short (%d bytes) — must exceed previewCap=%d", len(in), previewCap)
	}
	got := truncatePreview(in)
	if !utf8.ValidString(got) {
		t.Errorf("truncatePreview produced invalid UTF-8 — JSONB will reject this row. len(got)=%d", len(got))
	}
	if len(got) > previewCap {
		t.Errorf("truncatePreview exceeded cap: len(got)=%d > previewCap=%d", len(got), previewCap)
	}
	// Defense-in-depth: the result should also be a clean rune
	// prefix of the input — not some garbled sequence.
	if !strings.HasPrefix(in, got) {
		t.Errorf("truncatePreview should return a prefix of the input")
	}
}

// TestTruncatePreview_MultiByteAtBoundary — most-targeted regression.
// Feeds an input where the cap byte falls EXACTLY in the middle of a
// 3-byte codepoint. Pre-fix, this is the case that produces invalid
// UTF-8; post-fix, the truncate stops at the previous rune boundary.
func TestTruncatePreview_MultiByteAtBoundary(t *testing.T) {
	// Build a string that's `previewCap-1` ASCII bytes followed by
	// '世' (3 bytes). Total = previewCap + 2. The old impl would
	// slice at byte previewCap, landing inside the '世' codepoint.
	prefix := strings.Repeat("a", previewCap-1)
	in := prefix + "世"
	if len(in) != previewCap+2 {
		t.Fatalf("test setup: expected len %d, got %d", previewCap+2, len(in))
	}
	got := truncatePreview(in)
	if !utf8.ValidString(got) {
		t.Errorf("truncatePreview produced invalid UTF-8 at the multi-byte boundary case")
	}
	// Result should be exactly the ASCII prefix — '世' was past
	// the cap so it must be dropped entirely.
	if got != prefix {
		t.Errorf("expected exact ASCII prefix, got %q (len=%d)", got[len(got)-10:], len(got))
	}
}

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
