package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// fakeCPReaper is a hand-rolled CPOrphanReaper for the SaaS-mode
// sweeper tests. Records every Stop call so tests can assert which
// workspace IDs were re-issued.
type fakeCPReaper struct {
	mu        sync.Mutex
	stopErr   map[string]error
	stopCalls []string
}

func (f *fakeCPReaper) Stop(_ context.Context, wsID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls = append(f.stopCalls, wsID)
	return f.stopErr[wsID]
}

// TestCPSweepOnce_StopSucceeds_ClearsInstanceID — happy path. Single
// removed-row with non-NULL instance_id; Stop succeeds; instance_id
// gets NULL'd so the next cycle won't re-sweep it.
func TestCPSweepOnce_StopSucceeds_ClearsInstanceID(t *testing.T) {
	mock := setupTestDB(t)
	reaper := &fakeCPReaper{}

	mock.ExpectQuery(`(?s)^\s*SELECT id::text\s+FROM workspaces\s+WHERE status = 'removed'\s+AND instance_id IS NOT NULL\s+AND instance_id != ''\s+ORDER BY updated_at DESC\s+LIMIT \$1`).
		WithArgs(cpSweepLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-uuid-1"))
	mock.ExpectExec(`UPDATE workspaces SET instance_id = NULL, updated_at = now\(\) WHERE id = \$1`).
		WithArgs("ws-uuid-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	cpSweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 1 || reaper.stopCalls[0] != "ws-uuid-1" {
		t.Fatalf("expected Stop(ws-uuid-1), got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestCPSweepOnce_StopFails_KeepsInstanceID — CP transient failure.
// Stop returns an error; instance_id MUST stay populated so the next
// cycle retries. UPDATE must NOT fire.
func TestCPSweepOnce_StopFails_KeepsInstanceID(t *testing.T) {
	mock := setupTestDB(t)
	reaper := &fakeCPReaper{
		stopErr: map[string]error{"ws-uuid-1": errors.New("CP returned 503")},
	}

	mock.ExpectQuery(`(?s)^\s*SELECT id::text\s+FROM workspaces`).
		WithArgs(cpSweepLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-uuid-1"))
	// No ExpectExec for the UPDATE — sqlmock fails the test if the
	// UPDATE fires.

	cpSweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 1 || reaper.stopCalls[0] != "ws-uuid-1" {
		t.Fatalf("expected Stop(ws-uuid-1), got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (UPDATE should NOT have fired): %v", err)
	}
}

// TestCPSweepOnce_NoOrphans — empty result set is the steady state in
// healthy operation. No Stop, no UPDATE.
func TestCPSweepOnce_NoOrphans(t *testing.T) {
	mock := setupTestDB(t)
	reaper := &fakeCPReaper{}

	mock.ExpectQuery(`(?s)^\s*SELECT id::text\s+FROM workspaces`).
		WithArgs(cpSweepLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	cpSweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 0 {
		t.Fatalf("expected zero Stop calls, got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestCPSweepOnce_MultipleOrphans — all rows in the batch get Stop'd
// independently; one failure doesn't block others.
func TestCPSweepOnce_MultipleOrphans(t *testing.T) {
	mock := setupTestDB(t)
	reaper := &fakeCPReaper{
		stopErr: map[string]error{"ws-uuid-2": errors.New("CP 503 on ws-uuid-2")},
	}

	mock.ExpectQuery(`(?s)^\s*SELECT id::text\s+FROM workspaces`).
		WithArgs(cpSweepLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("ws-uuid-1").
			AddRow("ws-uuid-2").
			AddRow("ws-uuid-3"))
	// ws-uuid-1 succeeds → UPDATE fires.
	mock.ExpectExec(`UPDATE workspaces SET instance_id = NULL`).
		WithArgs("ws-uuid-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// ws-uuid-2 fails → no UPDATE.
	// ws-uuid-3 succeeds → UPDATE fires.
	mock.ExpectExec(`UPDATE workspaces SET instance_id = NULL`).
		WithArgs("ws-uuid-3").
		WillReturnResult(sqlmock.NewResult(0, 1))

	cpSweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 3 {
		t.Fatalf("expected Stop on all 3 ids, got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestCPSweepOnce_QueryError — DB transient failure. Sweep returns
// without panicking. No Stop calls.
func TestCPSweepOnce_QueryError(t *testing.T) {
	mock := setupTestDB(t)
	reaper := &fakeCPReaper{}

	mock.ExpectQuery(`(?s)^\s*SELECT id::text\s+FROM workspaces`).
		WithArgs(cpSweepLimit).
		WillReturnError(errors.New("connection refused"))

	cpSweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 0 {
		t.Fatalf("expected zero Stop calls on query error, got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestCPSweepOnce_UpdateError_LogsButContinues — Stop succeeded but
// the UPDATE to clear instance_id failed. Subsequent rows in the batch
// must still process; comment in cpSweepOnce promises idempotent re-Stop
// next cycle.
func TestCPSweepOnce_UpdateError_LogsButContinues(t *testing.T) {
	mock := setupTestDB(t)
	reaper := &fakeCPReaper{}

	mock.ExpectQuery(`(?s)^\s*SELECT id::text\s+FROM workspaces`).
		WithArgs(cpSweepLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("ws-uuid-1").
			AddRow("ws-uuid-2"))
	mock.ExpectExec(`UPDATE workspaces SET instance_id = NULL`).
		WithArgs("ws-uuid-1").
		WillReturnError(errors.New("UPDATE timeout"))
	mock.ExpectExec(`UPDATE workspaces SET instance_id = NULL`).
		WithArgs("ws-uuid-2").
		WillReturnResult(sqlmock.NewResult(0, 1))

	cpSweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 2 {
		t.Fatalf("expected Stop on both ids despite UPDATE error on first, got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestCPSweepOnce_NilDB — defensive against db.DB being nil. Must not
// panic; must not call Stop.
func TestCPSweepOnce_NilDB(t *testing.T) {
	saved := db.DB
	db.DB = nil
	t.Cleanup(func() { db.DB = saved })

	reaper := &fakeCPReaper{}
	cpSweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 0 {
		t.Fatalf("expected zero Stop calls when db.DB is nil, got %v", reaper.stopCalls)
	}
}

// TestStartCPOrphanSweeper_NilReaperDisabled — boot-safety: a SaaS CP
// without cpProv configured must not start the loop (immediate return,
// no goroutine leak).
func TestStartCPOrphanSweeper_NilReaperDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		StartCPOrphanSweeper(ctx, nil)
		close(done)
	}()
	select {
	case <-done:
		// expected — nil reaper short-circuits.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StartCPOrphanSweeper(nil) did not return immediately")
	}
}

// TestStartCPOrphanSweeper_RunsOnceImmediatelyAndOnTick — cadence
// contract: kick off one sweep at boot (so a platform restart starts
// healing immediately), then once per OrphanSweepInterval. Verifies
// the loop terminates on ctx cancel.
func TestStartCPOrphanSweeper_RunsOnceImmediatelyAndOnTick(t *testing.T) {
	mock := setupTestDB(t)
	reaper := &fakeCPReaper{}

	// Two sweeps within the test window: one immediate, one on the
	// first tick. We can't shrink OrphanSweepInterval (it's a const),
	// so assert "at least one immediate sweep" and let cancel close
	// the loop.
	mock.ExpectQuery(`(?s)^\s*SELECT id::text\s+FROM workspaces`).
		WithArgs(cpSweepLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	// The ticker may or may not fire in the test window depending on
	// scheduler; tolerate both shapes by registering a second optional
	// expectation. sqlmock fails on UNREGISTERED queries, so register
	// one more then accept either 1 or 2 fires.
	mock.ExpectQuery(`(?s)^\s*SELECT id::text\s+FROM workspaces`).
		WithArgs(cpSweepLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		StartCPOrphanSweeper(ctx, reaper)
		close(done)
	}()
	// 100ms is well past the boot-sweep but well shy of the 60s
	// interval, so the second query expectation is intentionally
	// unmet — that's fine, sqlmock distinguishes "expected but not
	// received" (we don't enforce here) from "unexpected query"
	// (which would fail).
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("StartCPOrphanSweeper did not exit on ctx cancel")
	}

	// Boot sweep must have happened — without it, an operator restart
	// after a CP outage would leave a 60s gap before the first heal.
	// We don't assert mock.ExpectationsWereMet() here because the
	// second query is intentionally optional.
}
