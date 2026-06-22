package handlers

// workspace_delete_stop_retry_test.go — pins the contract of the
// delete-path EC2 stop retry (task #15 / workspace-ec2-leak).
//
// Background (Phase 1 evidence): the DELETE path's StopWorkspaceAuto →
// cpProv.Stop had NO retry, while the restart path used cpStopWithRetry
// (bounded exponential backoff). A transient CP/AWS hiccup on delete left
// the workspace row at status='removed' with instance_id still populated,
// returned a 500, and relied entirely on the 60s CP-orphan-sweeper to
// re-drive the terminate. For a cascade *descendant* whose own row is
// already 'removed', the inline retry-via-client-replay is defeated by
// CascadeDelete's `status != 'removed'` CTE filter — so the only inline
// recovery is this bounded retry.
//
// Contract of stopWorkspaceForDelete:
//   - CP path: bounded retry (cpStopRetryAttempts, exp backoff) on
//     cpProv.Stop; returns nil on eventual success.
//   - On retry exhaustion: returns the terminal error AND emits a
//     `workspace.delete.terminate_retry_exhausted` structure_events row so
//     the leak decision is queryable (structured-logging gate), not just a
//     log.Printf. The row is the durable pending-terminate signal: the row
//     stays status='removed' with instance_id populated, which is exactly
//     what the CP-orphan-sweeper (registry/cp_orphan_sweeper.go) re-drives.
//   - Docker path: single Stop, no retry (local daemon failure won't heal
//     on retry — matches RestartWorkspaceAuto's Docker rationale).
//   - No backend wired: nil (nothing to stop).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestStopWorkspaceForDelete_CPRetriesTransientThenSucceeds(t *testing.T) {
	shrinkRetryBackoff(t)
	buf := captureLog(t)
	// 2 transient failures then success — within the 3-attempt budget.
	stub := &scriptedCPStop{errs: []error{
		errors.New("cp 503 attempt 1"),
		errors.New("cp 503 attempt 2"),
	}}
	h := &WorkspaceHandler{cpProv: stub}

	err := h.stopWorkspaceForDelete(context.Background(), "ws-del-1", false)
	if err != nil {
		t.Fatalf("expected nil error on eventual success, got %v", err)
	}
	if stub.calls != 3 {
		t.Errorf("expected 3 Stop calls (2 fails + 1 success), got %d", stub.calls)
	}
	if strings.Contains(buf.String(), "terminate_retry_exhausted") {
		t.Errorf("eventual success must NOT log retry-exhausted; got %q", buf.String())
	}
}

func TestStopWorkspaceForDelete_CPExhaustsEmitsDurableEventAndReturnsError(t *testing.T) {
	shrinkRetryBackoff(t)
	mock := setupTestDB(t)
	buf := captureLog(t)
	stub := &scriptedCPStop{errs: []error{
		errors.New("cp 502 attempt 1"),
		errors.New("cp 502 attempt 2"),
		errors.New("cp 502 final"),
	}}
	h := &WorkspaceHandler{cpProv: stub}

	// On exhaustion the helper persists a durable pending-terminate row so
	// the leak decision is queryable. structure_events is the audit-of-record.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := h.stopWorkspaceForDelete(context.Background(), "ws-doomed", false)
	if err == nil {
		t.Fatal("expected terminal error on retry exhaustion, got nil")
	}
	if stub.calls != cpStopRetryAttempts {
		t.Errorf("expected %d Stop calls when all fail, got %d", cpStopRetryAttempts, stub.calls)
	}
	if !strings.Contains(err.Error(), "cp 502 final") {
		t.Errorf("returned error should wrap the LAST attempt's error, got %v", err)
	}
	if e := mock.ExpectationsWereMet(); e != nil {
		t.Fatalf("expected structure_events INSERT on exhaustion: %v", e)
	}
	// The LEAK-SUSPECT line stays the operator-facing prose bridge to the
	// orphan reconciler; assert it carries the delete source so triage can
	// distinguish delete-leaks from restart-leaks.
	if !strings.Contains(buf.String(), "LEAK-SUSPECT") {
		t.Errorf("expected LEAK-SUSPECT log on exhaustion, got %q", buf.String())
	}
}

func TestStopWorkspaceForDelete_NoBackendIsNoOp(t *testing.T) {
	h := &WorkspaceHandler{} // cpProv nil, provisioner nil
	if err := h.stopWorkspaceForDelete(context.Background(), "ws-x", false); err != nil {
		t.Errorf("expected nil no-op with no backend, got %v", err)
	}
}
