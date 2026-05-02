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
// workspace `failed` so the canvas surfaces the failure instead of the row
// sitting in `provisioning` until the 10-min sweeper.

func TestLogProvisionPanic_NoOpWhenNoPanic(t *testing.T) {
	// Sanity: the deferred recover must be silent when nothing panicked.
	// Otherwise every successful provision would emit a spurious panic log.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(log.Writer())

	func() {
		defer logProvisionPanic("ws-no-panic", "cp")
		// no panic
	}()

	if buf.Len() != 0 {
		t.Fatalf("expected no log output when no panic, got: %q", buf.String())
	}
}

func TestLogProvisionPanic_RecoversAndMarksFailed(t *testing.T) {
	// Wire a sqlmock so logProvisionPanic's UPDATE has somewhere to land
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

	mock.ExpectExec(`UPDATE workspaces SET status='failed'`).
		WithArgs("ws-panic", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(log.Writer())

	// Exercise: a function that defers logProvisionPanic + then panics.
	// The recover MUST swallow the panic — if it propagates, the test
	// process crashes and the panic message bubbles up as a Go test
	// failure rather than the assertion below.
	didNotPanic := true
	func() {
		defer func() {
			// If logProvisionPanic re-raised, this catches it for the
			// test. We assert below that it did NOT re-raise.
			if r := recover(); r != nil {
				didNotPanic = false
			}
		}()
		defer logProvisionPanic("ws-panic", "cp")
		panic("simulated provision panic for #2486 regression")
	}()

	if !didNotPanic {
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
		t.Errorf("sql expectations: %v — UPDATE workspaces … status='failed' was not issued", err)
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

	mock.ExpectExec(`UPDATE workspaces SET status='failed'`).
		WithArgs("ws-panic-persist-fail", sqlmock.AnyArg()).
		WillReturnError(sql.ErrConnDone)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(log.Writer())

	func() {
		defer logProvisionPanic("ws-panic-persist-fail", "docker")
		panic("simulated panic with DB unavailable")
	}()

	logged := buf.String()
	if !strings.Contains(logged, "failed to persist panic-failure for ws-panic-persist-fail") {
		t.Errorf("expected persist-failure log line; got: %q", logged)
	}
}
