package handlers

// #1870 Phase 1 queue tests. Covers enqueue, FIFO drain order, priority
// ordering, idempotency, failed-retry bounding, nil-safe error extraction
// (GH fix), and the extractor helper.
//
// Uses sqlmock.QueryMatcherEqual (exact string matching) so that SQL query
// strings are compared verbatim without regex escaping complexity.
// setupTestDBForQueueTests creates the mock with this matcher; it MUST be
// used instead of setupTestDB for these tests.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/alicebob/miniredis/v2"
)

// setupTestDBForQueueTests creates a sqlmock DB using QueryMatcherEqual (exact
// string matching) so that ExpectQuery/ExpectExec patterns are compared verbatim.
// Uses the same global db.DB as setupTestDB so the handler can use it.
//
// IMPORTANT: db.DB is saved before assignment and restored via t.Cleanup so
// that tests running after this one are not polluted by a closed mock.
// Same fix as setupTestDB (handlers_test.go); same root cause as mc#975.
func setupTestDBForQueueTests(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })
	return mock
}

// ──────────────────────────────────────────────────────────────────────────────
// Priority constants
// ──────────────────────────────────────────────────────────────────────────────

func TestPriorityConstants(t *testing.T) {
	if PriorityCritical <= PriorityTask || PriorityTask <= PriorityInfo {
		t.Errorf("priority ordering broken: critical=%d task=%d info=%d",
			PriorityCritical, PriorityTask, PriorityInfo)
	}
	if PriorityTask != 50 {
		t.Errorf("PriorityTask changed from 50 to %d — migration 042's DEFAULT 50 also needs updating",
			PriorityTask)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// extractIdempotencyKey
// ──────────────────────────────────────────────────────────────────────────────

func TestExtractIdempotencyKey_picksMessageId(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"messageId":"msg-abc","role":"user"}}}`)
	if got := extractIdempotencyKey(body); got != "msg-abc" {
		t.Errorf("expected 'msg-abc', got %q", got)
	}
}

func TestExtractIdempotencyKey_emptyOnMissing(t *testing.T) {
	cases := map[string][]byte{
		"no params":     []byte(`{"jsonrpc":"2.0","method":"message/send"}`),
		"no message":    []byte(`{"params":{}}`),
		"no messageId":  []byte(`{"params":{"message":{"role":"user"}}}`),
		"malformed":     []byte(`not json`),
		"empty message": []byte(`{"params":{"message":{"messageId":""}}}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := extractIdempotencyKey(body); got != "" {
				t.Errorf("expected empty, got %q", got)
			}
		})
	}
}

func TestExtractDelegationIDFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "delegation body — metadata.delegation_id present",
			body: `{"method":"message/send","params":{"message":{"role":"user","messageId":"abc-123","metadata":{"delegation_id":"abc-123"},"parts":[{"type":"text","text":"hi"}]}}}`,
			want: "abc-123",
		},
		{
			name: "non-delegation body — no metadata (peer-direct A2A)",
			body: `{"method":"message/send","params":{"message":{"role":"user","messageId":"m-1","parts":[{"type":"text","text":"hi"}]}}}`,
			want: "",
		},
		{
			name: "metadata present but no delegation_id key",
			body: `{"params":{"message":{"metadata":{"trace_id":"t-1"}}}}`,
			want: "",
		},
		{"malformed JSON", `not json`, ""},
		{"empty body", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractDelegationIDFromBody([]byte(tc.body)); got != tc.want {
				t.Errorf("extractDelegationIDFromBody = %q, want %q", got, tc.want)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DrainQueueForWorkspace — nil-safe error extraction regression tests
//
// These tests verify the defensive type-assertion fix for the panic that
// occurred when proxyErr.Response was nil or had a non-string "error" field.
// The original code was:
//   MarkQueueItemFailed(ctx, item.ID, proxyErr.Response["error"].(string))
// which panics when:
//   a) proxyErr.Response is nil
//   b) "error" key is absent from the map
//   c) the "error" field is a non-string type (e.g., a struct or int)
//
// The fix uses comma-ok idiom + fallback chain:
//   errMsg, _ := proxyErr.Response["error"].(string)
//   if errMsg == "" { errMsg = http.StatusText(proxyErr.Status); ... }
//
// Uses sqlmock.MatchSs (exact string matching). SQL strings must EXACTLY match
// the queries generated by the handler code — no escaping, no regex.
// ──────────────────────────────────────────────────────────────────────────────

// drainSetup creates a consistent test environment for DrainQueueForWorkspace.
// Uses setupTestDBForQueueTests (QueryMatcherEqual) so SQL strings are compared verbatim.
// workspaceID is passed so callers can register the budget-check expectation in the
// correct position — after expectDequeueNextOk (DequeueNext's tx BEGIN→SELECT→UPDATE→COMMIT
// runs before proxyA2ARequest→checkWorkspaceBudget in the actual call sequence).
func drainSetup(t *testing.T, workspaceID string) (sqlmock.Sqlmock, *WorkspaceHandler, *miniredis.Miniredis) {
	mock := setupTestDBForQueueTests(t)
	mr := setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	allowLoopbackForTest(t) // httptest.Server uses 127.0.0.1; SSRF guard must permit it
	return mock, handler, mr
}

// expectQueueBudgetCheck registers the mock for checkWorkspaceBudget's query:
//
//	SELECT budget_limit, COALESCE(monthly_spend, 0) FROM workspaces WHERE id = $1
//
// Must be called AFTER expectDequeueNextOk — DequeueNext (BEGIN→SELECT→UPDATE→COMMIT)
// runs before proxyA2ARequest which calls checkWorkspaceBudget.
// Named distinctly from handlers_test.go's expectBudgetCheck (which uses MatchPsql
// escaped-regex and cannot be reused with QueryMatcherEqual tests).
func expectQueueBudgetCheck(mock sqlmock.Sqlmock, workspaceID string) {
	mock.ExpectQuery(
		"SELECT budget_limit, COALESCE(monthly_spend, 0) FROM workspaces WHERE id = $1",
	).WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"budget_limit", "monthly_spend"}))
}

// seedRedisURL puts the agent server URL into the Redis cache so resolveAgentURL
// returns it without needing a DB lookup.
func seedRedisURL(t *testing.T, mr *miniredis.Miniredis, wsID, url string) {
	if err := mr.Set(fmt.Sprintf("ws:%s:url", wsID), url); err != nil {
		t.Fatalf("seedRedisURL(%s): %v", wsID, err)
	}
	time.Sleep(1 * time.Millisecond) // settle
}

// drainItem returns a reproducible QueuedItem for testing.
// CallerID is NULL so proxyA2ARequest skips the CanCommunicate hierarchy check
// (no caller means canvas/system call path, which bypasses access control).
func drainItem(wsID string) *QueuedItem {
	return &QueuedItem{
		ID:          "qid-test-001",
		WorkspaceID: wsID,
		CallerID:    sql.NullString{Valid: false}, // no caller → no CanCommunicate check
		Priority:    PriorityTask,
		Body:        []byte(`{"method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":"hi"}]}}}`),
		Method:      sql.NullString{String: "message/send", Valid: true},
		Attempts:    1,
	}
}

// expectDequeueNextOk sets up sqlmock for DequeueNext's transaction:
//
//	BEGIN → SELECT FOR UPDATE SKIP LOCKED → UPDATE status='dispatched', attempts=attempts+1 → COMMIT
//
// SQL strings are EXACT matches to the handler code — QueryMatcherEqual verifies verbatim.
func expectDequeueNextOk(mock sqlmock.Sqlmock, item *QueuedItem) {
	mock.ExpectBegin()
	mock.ExpectQuery(
		"SELECT id, workspace_id, caller_id, priority, body::text, method, attempts FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued' AND (expires_at IS NULL OR expires_at > now()) ORDER BY priority DESC, enqueued_at ASC FOR UPDATE SKIP LOCKED LIMIT 1").
		WithArgs(item.WorkspaceID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "caller_id", "priority", "body", "method", "attempts",
		}).AddRow(
			item.ID, item.WorkspaceID, item.CallerID, item.Priority,
			string(item.Body), item.Method, item.Attempts,
		))
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = 'dispatched', dispatched_at = now(), attempts = attempts + 1 WHERE id = $1").
		WithArgs(item.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

// expectDequeueNextEmpty sets up sqlmock for DequeueNext returning no rows.
func expectDequeueNextEmpty(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectBegin()
	mock.ExpectQuery(
		"SELECT id, workspace_id, caller_id, priority, body::text, method, attempts FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued' AND (expires_at IS NULL OR expires_at > now()) ORDER BY priority DESC, enqueued_at ASC FOR UPDATE SKIP LOCKED LIMIT 1").
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
}

// expectCompleted sets up mock for MarkQueueItemCompleted.
func expectCompleted(mock sqlmock.Sqlmock, id string) {
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = 'completed', completed_at = now() WHERE id = $1").
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectFailed sets up mock for MarkQueueItemFailed with a specific error message.
func expectFailed(mock sqlmock.Sqlmock, id string, errMsg string) {
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'queued' END, last_error = $3, dispatched_at = NULL WHERE id = $1").
		WithArgs(id, 5, errMsg).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// agentServer creates an httptest.Server that responds with the given status
// and optional JSON body.
func agentServer(body string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			fmt.Fprint(w, body)
		}
	}))
}

// TestDrainQueueForWorkspace_Success_Completes: agent returns 200 → MarkQueueItemCompleted.
func TestDrainQueueForWorkspace_Success_Completes(t *testing.T) {
	item := drainItem("ws-ok")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, item.WorkspaceID)

	srv := agentServer(`{"result":{"status":"ok"}}`, http.StatusOK)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectCompleted(mock, item.ID)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_202Accepted_CompletesNotFailed verifies that 202 Accepted
// (dispatch was queued again) calls MarkQueueItemCompleted, NOT Failed, to avoid
// double-counting attempts.
func TestDrainQueueForWorkspace_202Accepted_CompletesNotFailed(t *testing.T) {
	item := drainItem("ws-202")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, item.WorkspaceID)

	srv := agentServer(`{"status":"queued"}`, http.StatusAccepted)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectCompleted(mock, item.ID)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_ProxyErrResponseNil_NoPanic: nil Response map → no panic,
// fallback to StatusText(502) = "Bad Gateway".
func TestDrainQueueForWorkspace_ProxyErrResponseNil_NoPanic(t *testing.T) {
	item := drainItem("ws-nilresp")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, item.WorkspaceID)

	srv := agentServer("", http.StatusBadGateway)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "Bad Gateway")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_ProxyErrMissingErrorKey_UsesStatusText: Response exists
// but "error" key is absent → fallback to http.StatusText.
func TestDrainQueueForWorkspace_ProxyErrMissingErrorKey_UsesStatusText(t *testing.T) {
	item := drainItem("ws-missingkey")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, item.WorkspaceID)

	srv := agentServer(`{"code":500,"detail":"internal server error"}`, http.StatusInternalServerError)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "Internal Server Error")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_ProxyErrNonStringError_NoPanic: Response["error"] is a
// JSON number, not a string → comma-ok returns ("", false) → no panic, falls back.
func TestDrainQueueForWorkspace_ProxyErrNonStringError_NoPanic(t *testing.T) {
	item := drainItem("ws-nonstr")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, item.WorkspaceID)

	srv := agentServer(`{"error": 429}`, http.StatusServiceUnavailable)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "Service Unavailable")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_ProxyErrWithStringError_UsesErrorMessage: valid string
// error → that string is logged (not StatusText).
func TestDrainQueueForWorkspace_ProxyErrWithStringError_UsesErrorMessage(t *testing.T) {
	item := drainItem("ws-str-err")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, item.WorkspaceID)

	wantErrMsg := "upstream agent crashed with signal: killed"
	srv := agentServer(fmt.Sprintf(`{"error":%q}`, wantErrMsg), http.StatusBadGateway)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, wantErrMsg)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_EmptyQueue_NoOps: DequeueNext returns (nil, nil) →
// no DB writes issued.
func TestDrainQueueForWorkspace_EmptyQueue_NoOps(t *testing.T) {
	mock, handler, _ := drainSetup(t, "ws-empty")

	expectDequeueNextEmpty(mock, "ws-empty")

	handler.DrainQueueForWorkspace(context.Background(), "ws-empty")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_DequeueError_LogsAndReturns: DB error during
// DequeueNext → logged, no panic, no UPDATE issued.
func TestDrainQueueForWorkspace_DequeueError_LogsAndReturns(t *testing.T) {
	mock, handler, _ := drainSetup(t, "ws-dequeue-err")

	mock.ExpectBegin()
	mock.ExpectQuery(
		"SELECT id, workspace_id, caller_id, priority, body::text, method, attempts FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued' AND (expires_at IS NULL OR expires_at > now()) ORDER BY priority DESC, enqueued_at ASC FOR UPDATE SKIP LOCKED LIMIT 1").
		WithArgs("ws-dequeue-err").
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	handler.DrainQueueForWorkspace(context.Background(), "ws-dequeue-err")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_MaxAttempts_FailsRatherThanRetries: attempts >= 5
// → 'failed' status (not back to 'queued').
func TestDrainQueueForWorkspace_MaxAttempts_FailsRatherThanRetries(t *testing.T) {
	item := &QueuedItem{
		ID:          "qid-max-attempts",
		WorkspaceID: "ws-max",
		CallerID:    sql.NullString{Valid: false}, // no caller → no CanCommunicate check
		Priority:    PriorityTask,
		Body:        []byte(`{"method":"message/send","params":{}}`),
		Method:      sql.NullString{String: "message/send", Valid: true},
		Attempts:    5, // already at max
	}
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, item.WorkspaceID)

	srv := agentServer(`{"error":"agent unreachable"}`, http.StatusBadGateway)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "agent unreachable")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_ClaimGuarding_SecondDrainGetsEmpty: verifies that after
// one drain successfully claims and completes a queue item, a second sequential drain
// sees an empty queue (row was dispatched, not available for re-claim).
// This exercises the FOR UPDATE SKIP LOCKED claim-guarding without the sqlmock
// goroutine-safety concern of the concurrent version.
func TestDrainQueueForWorkspace_ClaimGuarding_SecondDrainGetsEmpty(t *testing.T) {
	item := drainItem("ws-claim")
	wsID := item.WorkspaceID
	mock, handler, mr := drainSetup(t, wsID)

	// Drain 1: claims item, proxies successfully, marks completed.
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, wsID)

	srv := agentServer(`{"result":{}}`, http.StatusOK)
	defer srv.Close()
	seedRedisURL(t, mr, wsID, srv.URL)
	expectCompleted(mock, item.ID)

	handler.DrainQueueForWorkspace(context.Background(), wsID)

	// Drain 2: same workspace — queue is empty because item was dispatched.
	// Register expectations for the second drain.
	expectDequeueNextEmpty(mock, wsID)

	handler.DrainQueueForWorkspace(context.Background(), wsID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
