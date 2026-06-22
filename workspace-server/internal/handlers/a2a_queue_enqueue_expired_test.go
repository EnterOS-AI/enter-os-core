package handlers

// a2a_queue_enqueue_expired_test.go — regression for CR3 RC 9853.
//
// Bug: a pending buffered tick that expires before the drain reaches it is
// skipped by the drain (it filters out expired pending rows) yet still occupies
// the active set the idempotency check guards. A later tick for the SAME key
// would then collapse onto that dead row and be silently swallowed — the exact
// drop the busy-buffer path was built to prevent.
//
// Fix: EnqueueA2A retires any already-expired pending row for the key BEFORE the
// insert, so the fresh tick buffers (and the stale row is cleaned up) instead of
// being dropped.
//
// These tests use the QueryMatcherEqual mock (setupTestDBForQueueTests) so the
// SQL strings below must match the handler's queries verbatim.

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	enqWorkspaceID = "ws-enq-expired"
	enqKey         = "sched-aaaa-bbbb" // schedule_id used as idempotency key
	enqBody        = `{"method":"message/send"}`
	enqMethod      = "message/send"
)

// expectSupersedeExpired registers the cleanup UPDATE EnqueueA2A issues before
// the insert when an idempotency key is present. rowsRetired is how many expired
// pending rows the UPDATE claims to have dropped.
func expectSupersedeExpired(mock sqlmock.Sqlmock, workspaceID, key string, rowsRetired int64) {
	mock.ExpectExec(`
			UPDATE a2a_queue
			SET status = 'dropped',
			    last_error = 'superseded: expired before drain; replaced by a fresh enqueue'
			WHERE workspace_id = $1
			  AND idempotency_key = $2
			  AND status = 'queued'
			  AND expires_at IS NOT NULL
			  AND expires_at <= now()
		`).
		WithArgs(workspaceID, key).
		WillReturnResult(sqlmock.NewResult(0, rowsRetired))
}

// expectInsert registers the INSERT ... ON CONFLICT DO NOTHING RETURNING id.
// newID is the id the insert returns (non-conflict / fresh enqueue path).
//
// expectedCallerIDBind pins the exact bind value the test expects for
// the caller_id column. Pass:
//   - sqlmock.AnyArg() for "I don't care" (used by the existing tests
//     that don't pin the caller_id shape — they're not regression-tests
//     for the system-caller normalization, just for the canonical
//     enqueue path).
//   - nil (sqlmock's nil interface{}) for system-caller prefixes — pins
//     that the literal "system:restart-context" (or any other
//     systemCallerPrefixes entry) is normalized to NULL in the bind
//     parameter, not persisted as the literal string. This is the
//     regression-test shape for #2694 RC #99248 (the busy-A2A path
//     that tripped a UUID cast failure on a2a_queue.caller_id).
//   - the actual workspace UUID string — pins that a real workspace
//     UUID-shaped callerID is passed through unchanged (not
//     normalized to NULL — that would hide real-workspace
//     attribution in the queue row).
//
// The two new tests (TestEnqueueA2A_SystemCallerNormalizesToNULLCallerID
// + TestEnqueueA2A_RealWorkspaceUUIDPreserved) use this helper with
// explicit bind expectations to pin the new normalization contract.
func expectInsert(mock sqlmock.Sqlmock, newID string, expectedCallerIDBind interface{}) {
	// Only pin the caller_id bind (position 2). The other 6 parameters are
	// not the focus of the system-caller normalization test — use
	// sqlmock.AnyArg() so the helper doesn't break when a test passes
	// different values for workspace_id / key / expires_at (e.g.
	// NoKey_SkipsSupersede passes enqKey="" which becomes nil at the
	// SQL bind per the EnqueueA2A code's if-idempotencyKey!=""). The
	// point of the test is to pin caller_id specifically, not the rest
	// of the row.
	mock.ExpectQuery(`
		INSERT INTO a2a_queue (workspace_id, caller_id, priority, body, method, idempotency_key, expires_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
		ON CONFLICT (workspace_id, idempotency_key)
			WHERE idempotency_key IS NOT NULL AND status IN ('queued','dispatched')
		DO NOTHING
		RETURNING id
	`).
		WithArgs(sqlmock.AnyArg(), expectedCallerIDBind, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(newID))
}

// expectDepth registers the trailing queue-depth count query.
func expectDepth(mock sqlmock.Sqlmock, workspaceID string, depth int) {
	mock.ExpectQuery(`
		SELECT COUNT(*) FROM a2a_queue
		WHERE workspace_id = $1 AND status = 'queued'
	`).WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(depth))
}

// TestEnqueueA2A_ExpiredRowDoesNotBlockFreshTick is the core CR3 regression:
// an existing expired pending row for a schedule's key must NOT cause the next
// tick's enqueue to be dropped. The expired row is retired first, then the
// fresh tick inserts and returns a NEW id.
func TestEnqueueA2A_ExpiredRowDoesNotBlockFreshTick(t *testing.T) {
	mock := setupTestDBForQueueTests(t)

	// One expired pending row exists for this key and gets retired.
	expectSupersedeExpired(mock, enqWorkspaceID, enqKey, 1)
	// With the active set cleared, the insert proceeds (no conflict) → new id.
	const freshID = "fresh-tick-id"
	expectInsert(mock, freshID, nil)
	expectDepth(mock, enqWorkspaceID, 1)

	nextRun := time.Now().Add(30 * time.Second)
	id, depth, err := EnqueueA2A(
		context.Background(), enqWorkspaceID, "", PriorityTask,
		[]byte(enqBody), enqMethod, enqKey, &nextRun,
	)
	if err != nil {
		t.Fatalf("EnqueueA2A returned error: %v", err)
	}
	if id != freshID {
		t.Errorf("expected the fresh tick to enqueue with a new id %q, got %q "+
			"(an expired row must not swallow the new tick)", freshID, id)
	}
	if depth != 1 {
		t.Errorf("expected depth 1, got %d", depth)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestEnqueueA2A_SystemCallerNormalizesToNULLCallerID: the synthetic
// "system:restart-context" callerID (and all systemCallerPrefixes:
// webhook:, system:, test:, channel:) must be normalized to NULL in
// the a2a_queue.caller_id bind parameter, NOT persisted as the literal
// string. The column is UUID-typed (migrations/042_a2a_queue.up.sql:21),
// so a literal-string insert would trip a Postgres UUID cast failure
// → EnqueueA2A returns an error → the busy-A2A path falls through to
// a 503 instead of queueing. See #2694 RC #99248 + #2693 for the
// broader #2680 lineage. Real workspace UUIDs are passed through
// unchanged (regression-guard).
//
// This test pins the new normalization contract. Without it, the
// restart-context → busy-queue path would have appeared "fixed" by my
// prior activity-log nilIfEmpty PR but still trip the UUID cast on
// the queue insert (a different column, same callerID-typed poison).
func TestEnqueueA2A_SystemCallerNormalizesToNULLCallerID(t *testing.T) {
	// All 4 systemCallerPrefixes from a2a_proxy.go:82-84 must normalize
	// to NULL in the caller_id bind. We test with "system:restart-context"
	// (the actual offender) and the other 3 prefixes for full coverage.
	systemCallerIDs := []string{
		"webhook:github",
		"system:restart-context", // the actual offender
		"system:other-svc",
		"test:integration-1",
		"channel:discord",
	}

	for _, sysCaller := range systemCallerIDs {
		mock := setupTestDBForQueueTests(t)

		// No expired row for this key → the supersede UPDATE affects 0 rows.
		expectSupersedeExpired(mock, enqWorkspaceID, enqKey, 0)
		// The insert proceeds. The mock's ExpectExec will validate the bind
		// parameter shape: caller_id must be a nil interface{} (NOT the
		// literal system-prefix string). sqlmock's default comparison
		// distinguishes nil from non-nil, so passing the literal string
		// would fail the expectationsWereMet check.
		const freshID = "fresh-id-sys-caller"
		expectInsert(mock, freshID, nil)
		expectDepth(mock, enqWorkspaceID, 1)

		nextRun := time.Now().Add(30 * time.Second)
		id, depth, err := EnqueueA2A(
			context.Background(), enqWorkspaceID, sysCaller, PriorityTask,
			[]byte(enqBody), enqMethod, enqKey, &nextRun,
		)
		if err != nil {
			t.Errorf("system-caller %q: EnqueueA2A returned error: %v", sysCaller, err)
		}
		if id != freshID {
			t.Errorf("system-caller %q: expected fresh id %q, got %q", sysCaller, freshID, id)
		}
		if depth != 1 {
			t.Errorf("system-caller %q: expected depth 1, got %d", sysCaller, depth)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("system-caller %q: unmet sqlmock expectations: %v "+
				"(the literal callerID must have been normalized to NULL in the bind)", sysCaller, err)
		}
	}
}

// TestEnqueueA2A_RealWorkspaceUUIDPreserved: regression-guard that a real
// workspace UUID-shaped callerID still gets persisted as a non-nil
// bind parameter (otherwise we'd hide real-workspace attribution in the
// queue row). The fix in #2694 must NOT regress this case.
func TestEnqueueA2A_RealWorkspaceUUIDPreserved(t *testing.T) {
	mock := setupTestDBForQueueTests(t)

	// A real workspace UUID-shaped string (no system prefix). Per the
	// isSystemCaller() rule, this should NOT be normalized to NULL —
	// it must be passed through as the caller_id bind. Declared BEFORE
	// the expectInsert call so the literal is in scope when sqlmock
	// matches the bind parameter (the helper pins the exact value).
	realUUID := "9a40df22-ba4b-3fc0-75c1-66dd6869ff25" // a real UUID-shaped string

	expectSupersedeExpired(mock, enqWorkspaceID, enqKey, 0)
	const freshID = "fresh-id-real-uuid"
	expectInsert(mock, freshID, realUUID)
	expectDepth(mock, enqWorkspaceID, 1)

	nextRun := time.Now().Add(30 * time.Second)
	id, depth, err := EnqueueA2A(
		context.Background(), enqWorkspaceID, realUUID, PriorityTask,
		[]byte(enqBody), enqMethod, enqKey, &nextRun,
	)
	if err != nil {
		t.Fatalf("real workspace UUID: EnqueueA2A returned error: %v", err)
	}
	if id != freshID {
		t.Errorf("real workspace UUID: expected fresh id %q, got %q", freshID, id)
	}
	if depth != 1 {
		t.Errorf("real workspace UUID: expected depth 1, got %d", depth)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("real workspace UUID: unmet sqlmock expectations: %v "+
			"(a real workspace UUID-shaped callerID must be passed through, not normalized to NULL)", err)
	}
}

// TestEnqueueA2A_NoExpiredRow_NormalEnqueue: when no expired row exists the
// supersede UPDATE simply affects zero rows and the enqueue proceeds normally.
func TestEnqueueA2A_NoExpiredRow_NormalEnqueue(t *testing.T) {
	mock := setupTestDBForQueueTests(t)

	expectSupersedeExpired(mock, enqWorkspaceID, enqKey, 0) // nothing to retire
	const newID = "new-id"
	expectInsert(mock, newID, nil)
	expectDepth(mock, enqWorkspaceID, 2)

	nextRun := time.Now().Add(30 * time.Second)
	id, depth, err := EnqueueA2A(
		context.Background(), enqWorkspaceID, "", PriorityTask,
		[]byte(enqBody), enqMethod, enqKey, &nextRun,
	)
	if err != nil {
		t.Fatalf("EnqueueA2A returned error: %v", err)
	}
	if id != newID {
		t.Errorf("expected id %q, got %q", newID, id)
	}
	if depth != 2 {
		t.Errorf("expected depth 2, got %d", depth)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestEnqueueA2A_NoKey_SkipsSupersede: with no idempotency key there is no
// active-set conflict to guard, so the supersede cleanup is skipped entirely
// and only the insert + depth queries run.
func TestEnqueueA2A_NoKey_SkipsSupersede(t *testing.T) {
	mock := setupTestDBForQueueTests(t)

	// No expectSupersedeExpired — it must NOT be issued when key is empty.
	const newID = "no-key-id"
	expectInsert(mock, newID, nil)
	expectDepth(mock, enqWorkspaceID, 1)

	id, _, err := EnqueueA2A(
		context.Background(), enqWorkspaceID, "", PriorityTask,
		[]byte(enqBody), enqMethod, "", nil,
	)
	if err != nil {
		t.Fatalf("EnqueueA2A returned error: %v", err)
	}
	if id != newID {
		t.Errorf("expected id %q, got %q", newID, id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
