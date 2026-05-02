package handlers

import (
	"bytes"
	"database/sql"
	"log"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// Pin the issue #2486 contract: a panic inside the provision goroutine must
// (1) not propagate (the deferred recover swallows it), (2) log the panic
// with a stack trace so an operator can see what blew up, and (3) mark the
// workspace `failed` AND broadcast WORKSPACE_PROVISION_FAILED so the canvas
// flips the spinner to a failure card immediately — not after the 10-min
// sweeper.
//
// Helper: newPanicTestHandler wires a captureBroadcaster + handler so each
// test exercises the real markProvisionFailed path. The broadcaster capture
// is what proves assertion (3) — without it, the panic recovery would mark
// the row failed in the DB but the canvas wouldn't learn until next refresh.

func newPanicTestHandler() (*WorkspaceHandler, *captureBroadcaster) {
	cap := &captureBroadcaster{}
	return NewWorkspaceHandler(cap, nil, "http://localhost:8080", ""), cap
}

// captureLog swaps log output to a buffer for the test and restores the
// previous writer on cleanup. Capturing `prev` BEFORE SetOutput is
// load-bearing — `log.Writer()` evaluated at defer-fire time would
// return the buffer (not the original writer) and never restore it,
// poisoning subsequent tests in the package.
//
// log.SetOutput is process-global: do NOT call this from a test that
// uses t.Parallel() or two captures will race + clobber. The panic
// tests below are intentionally non-parallel for this reason.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// guardAgainstReraise wraps a function in a recover-arm that flips the
// returned bool to false if anything propagates past `defer
// h.logProvisionPanic(...)`. Used in every panic test (not just
// RecoversAndMarksFailed) so a future regression that re-raises from
// the recovery path surfaces as a clean test failure, not a process
// abort that crashes sibling tests.
func guardAgainstReraise(fn func()) (didNotPanic bool) {
	didNotPanic = true
	defer func() {
		if r := recover(); r != nil {
			didNotPanic = false
		}
	}()
	fn()
	return
}

func TestLogProvisionPanic_NoOpWhenNoPanic(t *testing.T) {
	// Sanity: the deferred recover must be silent when nothing panicked.
	// Otherwise every successful provision would emit a spurious panic log.
	buf := captureLog(t)
	h, cap := newPanicTestHandler()

	if !guardAgainstReraise(func() {
		defer h.logProvisionPanic("ws-no-panic", "cp")
		// no panic
	}) {
		t.Fatal("logProvisionPanic re-raised on the no-panic path — recover() returned non-nil for a goroutine that didn't panic")
	}

	if buf.Len() != 0 {
		t.Fatalf("expected no log output when no panic, got: %q", buf.String())
	}
	if cap.lastData != nil {
		t.Fatalf("expected no broadcast when no panic, got: %v", cap.lastData)
	}
}

func TestLogProvisionPanic_RecoversAndMarksFailed(t *testing.T) {
	// Wire a sqlmock so markProvisionFailed's UPDATE has somewhere to land
	// without needing a real Postgres. The mock asserts the SQL shape +
	// args so a future refactor of the persist call doesn't silently
	// stop marking the row failed.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	prevDB := db.DB
	db.DB = mockDB
	defer func() { db.DB = prevDB }()

	// markProvisionFailed issues:
	//   UPDATE workspaces SET status = $3, last_sample_error = $2, updated_at = now() WHERE id = $1
	// with args (workspaceID, msg, models.StatusFailed).
	mock.ExpectExec(`UPDATE workspaces SET status`).
		WithArgs("ws-panic", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	buf := captureLog(t)
	h, cap := newPanicTestHandler()

	// Exercise: a function that defers logProvisionPanic + then panics.
	// The recover MUST swallow the panic — if it propagates,
	// guardAgainstReraise catches it instead of letting the test
	// process abort.
	if !guardAgainstReraise(func() {
		defer h.logProvisionPanic("ws-panic", "cp")
		panic("simulated provision panic for #2486 regression")
	}) {
		t.Fatal("logProvisionPanic re-raised the panic — the recover() arm did not swallow it")
	}

	logged := buf.String()
	if !strings.Contains(logged, "PANIC during provision goroutine for ws-panic") {
		t.Errorf("missing panic-class log line; got: %q", logged)
	}
	if !strings.Contains(logged, "simulated provision panic for #2486 regression") {
		t.Errorf("panic value not logged; got: %q", logged)
	}
	if !strings.Contains(logged, "stack:") {
		t.Errorf("missing stack trace marker; got: %q", logged)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v — UPDATE workspaces … status=failed was not issued", err)
	}

	// Canvas-broadcast assertion: the panic recovery MUST route through
	// markProvisionFailed, which fires WORKSPACE_PROVISION_FAILED. Without
	// this, the canvas spinner stays on "provisioning" until the sweeper
	// or a poll — defeating the immediate-feedback purpose of this gate.
	if cap.lastData == nil {
		t.Fatal("expected broadcaster.RecordAndBroadcast to be called by panic recovery, got nil — canvas would not see the failure")
	}
	if errMsg, ok := cap.lastData["error"].(string); !ok || !strings.Contains(errMsg, "provision panic:") {
		t.Errorf("broadcast payload missing/wrong 'error' field; got: %v", cap.lastData)
	}
}

func TestLogProvisionPanic_PersistFailureLogged(t *testing.T) {
	// Defense-in-depth: if the panic-mark UPDATE itself fails, log it
	// rather than swallow silently. Otherwise an operator sees the
	// panic-class log line but no persistent-failure row, leaving the
	// workspace in `provisioning` with a misleading "we recovered" log.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	prevDB := db.DB
	db.DB = mockDB
	defer func() { db.DB = prevDB }()

	mock.ExpectExec(`UPDATE workspaces SET status`).
		WithArgs("ws-panic-persist-fail", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(sql.ErrConnDone)

	buf := captureLog(t)
	h, _ := newPanicTestHandler()

	if !guardAgainstReraise(func() {
		defer h.logProvisionPanic("ws-panic-persist-fail", "docker")
		panic("simulated panic with DB unavailable")
	}) {
		t.Fatal("logProvisionPanic re-raised when the persist-failure path was exercised — recover() arm did not swallow")
	}

	logged := buf.String()
	// markProvisionFailed logs `markProvisionFailed: db update failed for <id>: <err>`
	// when its UPDATE fails. That's the line that proves we surfaced the
	// persist failure rather than swallowing it.
	if !strings.Contains(logged, "markProvisionFailed: db update failed for ws-panic-persist-fail") {
		t.Errorf("expected markProvisionFailed db-update-failure log line; got: %q", logged)
	}
}
