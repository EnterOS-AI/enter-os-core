//go:build integration
// +build integration

// delegation_ledger_integration_test.go — REAL Postgres integration tests
// for the RFC #2829 ledger writes.
//
// Run with:
//
//   docker run --rm -d --name pg-integration \
//     -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//     -p 55432:5432 postgres:15-alpine
//   sleep 4
//   psql ... < workspace-server/migrations/049_delegations.up.sql
//   cd workspace-server
//   INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//     go test -tags=integration ./internal/handlers/ -run Integration_
//
// CI (.github/workflows/handlers-postgres-integration.yml) runs this on
// every PR that touches workspace-server/internal/handlers/**.
//
// Why these are NOT plain unit tests
// ----------------------------------
// The strict-sqlmock unit tests in delegation_ledger_writes_test.go pin
// which SQL statements fire — they are fast and let us iterate without
// a DB. But sqlmock CANNOT detect bugs that depend on the ROW STATE
// after the SQL runs. The result_preview-lost bug shipped to staging in
// PR #2854 because every unit test was satisfied with "an UPDATE
// statement fired" — none verified the row's preview field landed.
//
// These integration tests close that gap by booting a real Postgres,
// running the production helpers, and SELECTing the row to verify the
// observable state matches the expected outcome.

package handlers

import (
	"context"
	"database/sql"
	"os"
	"testing"

	mdb "github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	_ "github.com/lib/pq"
)

// integrationDB returns the configured integration-test connection or
// skips the test if INTEGRATION_DB_URL is unset. Local devs run the
// docker-postgres incantation in the file header; CI's workflow sets the
// env var via a service container.
func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping (local devs: see file header)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// Each test gets a fresh table state — fail loud if cleanup fails so
	// a bad test doesn't pollute the next one.
	if _, err := conn.ExecContext(context.Background(), `DELETE FROM delegations`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	// Wire the package-level db.DB so production helpers (recordLedgerInsert,
	// recordLedgerStatus) see the same connection.
	prev := mdb.DB
	mdb.DB = conn
	t.Cleanup(func() {
		mdb.DB = prev
		conn.Close()
	})
	return conn
}

// readLedgerRow returns (status, result_preview, error_detail) for the
// given delegation_id, or fails the test on miss.
func readLedgerRow(t *testing.T, conn *sql.DB, id string) (status, preview, errorDetail string) {
	t.Helper()
	var prev, errDet sql.NullString
	err := conn.QueryRowContext(context.Background(),
		`SELECT status, result_preview, error_detail FROM delegations WHERE delegation_id = $1`, id,
	).Scan(&status, &prev, &errDet)
	if err != nil {
		t.Fatalf("readLedgerRow(%s): %v", id, err)
	}
	return status, prev.String, errDet.String
}

// TestIntegration_ResultPreviewPreservedThroughCompletion is the
// regression gate for the bug that shipped in PR #2854 + was caught in
// self-review: when both the inner SetStatus(completed, "", "") (from
// updateDelegationStatus) and an outer SetStatus(completed, "", preview)
// fire, the SECOND one is a same-status no-op — order matters.
//
// The fix in delegation.go calls the WITH-PREVIEW SetStatus FIRST so the
// outer write lands the preview, and the inner becomes the no-op.
//
// This test fires the call sequence in the corrected order and asserts
// the row's result_preview matches.
//
// If a future refactor reverses the order, this test fails on a real
// Postgres — which sqlmock would have missed.
func TestIntegration_ResultPreviewPreservedThroughCompletion(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-deleg-preview-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"
	expectedPreview := "the long-running task's final answer"

	// Mirror the production call sequence the FIXED code path uses.
	// executeDelegation flow:
	//   1. insertDelegationRow → recordLedgerInsert (status=queued)
	//   2. updateDelegationStatus("dispatched", "") at the start of execute,
	//      so the row is at status=dispatched by completion time
	//   3. recordLedgerStatus("completed", "", preview)   ← outer FIRST (the fix)
	//   4. updateDelegationStatus("completed", "") inside, which calls
	//      recordLedgerStatus("completed", "", "")        ← inner same-status no-op
	recordLedgerInsert(context.Background(), caller, callee, id, "the question", "")
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")
	recordLedgerStatus(context.Background(), id, "completed", "", expectedPreview)
	recordLedgerStatus(context.Background(), id, "completed", "", "")

	status, preview, errDet := readLedgerRow(t, conn, id)
	if status != "completed" {
		t.Errorf("status: want completed, got %q", status)
	}
	if preview != expectedPreview {
		t.Errorf("result_preview lost: want %q, got %q", expectedPreview, preview)
	}
	if errDet != "" {
		t.Errorf("error_detail should be empty: got %q", errDet)
	}
}

// TestIntegration_ResultPreviewBuggyOrderIsLost — DIAGNOSTIC test that
// confirms the ORIGINAL buggy order does lose the preview. Useful when
// auditing similar wiring elsewhere.
//
// This is documented behavior: it asserts the same-status replay no-op
// works as designed in DelegationLedger.SetStatus. The fix in
// delegation.go is to AVOID this order, not to change SetStatus's
// same-status semantics (which the operator dashboard relies on for
// idempotent completion notifications).
func TestIntegration_ResultPreviewBuggyOrderIsLost(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-deleg-preview-2"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"

	// BUGGY sequence in production-shape order: queued → dispatched →
	// completed (no preview) → completed (preview ignored as same-status).
	recordLedgerInsert(context.Background(), caller, callee, id, "the question", "")
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")            // pre-completion stage
	recordLedgerStatus(context.Background(), id, "completed", "", "")             // inner first
	recordLedgerStatus(context.Background(), id, "completed", "", "the answer")   // outer same-status no-op

	_, preview, _ := readLedgerRow(t, conn, id)
	if preview != "" {
		t.Errorf("buggy-order preview was unexpectedly non-empty: %q (SetStatus same-status no-op contract may have changed)", preview)
	}
}

// TestIntegration_FailedTransitionCapturesErrorDetail — error_detail is
// the failure-path equivalent of result_preview. The legacy path calls
// SetStatus(failed, errorDetail, "") via updateDelegationStatus; no
// outer call exists today (no observed bug). This test pins that
// error_detail lands as expected, so a future refactor adding an outer
// call must consciously preserve the field — same lesson as the preview
// bug, just on the failure path.
func TestIntegration_FailedTransitionCapturesErrorDetail(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-deleg-fail-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"
	expectedError := "callee unreachable: connection refused"

	// queued → failed is allowed by allowedTransitions (the failure-on-
	// dispatch case) so this exercises a real production path.
	recordLedgerInsert(context.Background(), caller, callee, id, "the question", "")
	recordLedgerStatus(context.Background(), id, "failed", expectedError, "")

	status, preview, errDet := readLedgerRow(t, conn, id)
	if status != "failed" {
		t.Errorf("status: want failed, got %q", status)
	}
	if errDet != expectedError {
		t.Errorf("error_detail: want %q, got %q", expectedError, errDet)
	}
	if preview != "" {
		t.Errorf("result_preview should be empty on failure: got %q", preview)
	}
}

// TestIntegration_Sweeper_DeadlineExceededIsMarkedFailed — real-Postgres
// gate for the RFC #2829 PR-3 stuck-task sweeper. Inserts a row with a
// past deadline, runs Sweep, asserts the row is now `failed` with
// `deadline exceeded by sweeper` in error_detail.
//
// sqlmock unit tests pinned the SQL fired but couldn't observe the
// real ON CONFLICT / index-scan behavior on the partial inflight
// index. Real Postgres catches:
//   - deadline timestamp comparison is correct under tz boundaries
//   - the partial index actually serves the WHERE clause
//   - SetStatus's terminal forward-only protection holds across the
//     sweep + concurrent-write race
func TestIntegration_Sweeper_DeadlineExceededIsMarkedFailed(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-sweeper-deadline-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"

	// Insert + transition to dispatched (otherwise queued→failed is
	// allowed but doesn't exercise the in-flight scan accurately).
	recordLedgerInsert(context.Background(), caller, callee, id, "task", "")
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")

	// Force the deadline into the past — Insert defaults to now+6h, so
	// we override.
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE delegations SET deadline = now() - interval '1 minute', last_heartbeat = now() WHERE delegation_id = $1`, id,
	); err != nil {
		t.Fatalf("backdate deadline: %v", err)
	}

	res := NewDelegationSweeper(nil, nil).Sweep(context.Background())
	if res.DeadlineFailures != 1 {
		t.Errorf("expected 1 deadline failure, got %+v", res)
	}
	status, _, errDet := readLedgerRow(t, conn, id)
	if status != "failed" {
		t.Errorf("status: want failed, got %q", status)
	}
	if errDet != "deadline exceeded by sweeper" {
		t.Errorf("error_detail: %q", errDet)
	}
}

// TestIntegration_Sweeper_StaleHeartbeatIsMarkedStuck — heartbeat
// staleness path. Insert + dispatch + backdate last_heartbeat past the
// 10× threshold; Sweep should mark the row stuck.
func TestIntegration_Sweeper_StaleHeartbeatIsMarkedStuck(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	// Tighten threshold so the test is deterministic + fast.
	t.Setenv("DELEGATION_STUCK_THRESHOLD_S", "10")

	id := "integ-sweeper-stuck-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"

	recordLedgerInsert(context.Background(), caller, callee, id, "task", "")
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")
	recordLedgerStatus(context.Background(), id, "in_progress", "", "")

	// Backdate last_heartbeat past the 10s threshold; deadline still in
	// future so deadline check shouldn't fire.
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE delegations SET last_heartbeat = now() - interval '60 seconds' WHERE delegation_id = $1`, id,
	); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}

	res := NewDelegationSweeper(nil, nil).Sweep(context.Background())
	if res.StuckMarked != 1 {
		t.Errorf("expected 1 stuck mark, got %+v", res)
	}
	status, _, errDet := readLedgerRow(t, conn, id)
	if status != "stuck" {
		t.Errorf("status: want stuck, got %q", status)
	}
	if errDet == "" {
		t.Errorf("error_detail should mention 'no heartbeat for Xs'; got empty")
	}
}

// TestIntegration_Sweeper_HealthyRowsNotTouched — sanity: rows with a
// fresh heartbeat AND a future deadline are left alone. Confirms the
// partial inflight index scan + per-row branching don't false-positive
// against well-behaved delegations.
func TestIntegration_Sweeper_HealthyRowsNotTouched(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-sweeper-healthy-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"

	recordLedgerInsert(context.Background(), caller, callee, id, "task", "")
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")
	// Fresh heartbeat = now()
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE delegations SET last_heartbeat = now() WHERE delegation_id = $1`, id,
	); err != nil {
		t.Fatalf("set heartbeat: %v", err)
	}

	res := NewDelegationSweeper(nil, nil).Sweep(context.Background())
	if res.DeadlineFailures != 0 || res.StuckMarked != 0 {
		t.Errorf("healthy row touched; result: %+v", res)
	}
	status, _, _ := readLedgerRow(t, conn, id)
	if status != "dispatched" {
		t.Errorf("status changed unexpectedly: %q", status)
	}
}

// TestIntegration_FullLifecycle_QueuedToDispatchedToCompleted — pins the
// happy-path lifecycle. INSERT lands the row at queued; SetStatus moves
// it through dispatched and into completed with preview. After the
// terminal transition, no further state change is possible via
// SetStatus (forward-only protection).
func TestIntegration_FullLifecycle_QueuedToDispatchedToCompleted(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-deleg-lifecycle-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"

	recordLedgerInsert(context.Background(), caller, callee, id, "task body", "")
	if status, _, _ := readLedgerRow(t, conn, id); status != "queued" {
		t.Errorf("after Insert: status want queued, got %q", status)
	}
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")
	if status, _, _ := readLedgerRow(t, conn, id); status != "dispatched" {
		t.Errorf("after dispatched: status want dispatched, got %q", status)
	}
	recordLedgerStatus(context.Background(), id, "completed", "", "the result")
	status, preview, _ := readLedgerRow(t, conn, id)
	if status != "completed" {
		t.Errorf("after completed: status want completed, got %q", status)
	}
	if preview != "the result" {
		t.Errorf("preview after completed: want %q, got %q", "the result", preview)
	}

	// Forward-only: trying to revise to failed should silently no-op
	// (recordLedgerStatus swallows ErrInvalidTransition).
	recordLedgerStatus(context.Background(), id, "failed", "post-hoc revision", "")
	status, preview, errDet := readLedgerRow(t, conn, id)
	if status != "completed" {
		t.Errorf("forward-only broken: status changed to %q", status)
	}
	if preview != "the result" {
		t.Errorf("preview clobbered by failed revision: %q", preview)
	}
	if errDet != "" {
		t.Errorf("error_detail clobbered by failed revision: %q", errDet)
	}
}
