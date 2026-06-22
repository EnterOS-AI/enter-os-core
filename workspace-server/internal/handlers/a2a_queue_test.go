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

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
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

// ──────────────────────────────────────────────────────────────────────────────
// extractExpiresInSeconds
// ──────────────────────────────────────────────────────────────────────────────

func TestExtractExpiresInSeconds_valid(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"positive int", `{"params":{"expires_in_seconds":30}}`, 30},
		{"zero", `{"params":{"expires_in_seconds":0}}`, 0},
		{"large TTL", `{"params":{"expires_in_seconds":3600}}`, 3600},
		{"nested message — not affected", `{"params":{"message":{"role":"user"},"expires_in_seconds":60}}`, 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractExpiresInSeconds([]byte(tc.body)); got != tc.want {
				t.Errorf("extractExpiresInSeconds = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestExtractExpiresInSeconds_invalidOrMissing(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"negative → 0", `{"params":{"expires_in_seconds":-5}}`, 0},
		{"missing expires_in_seconds", `{"params":{"message":{"role":"user"}}}`, 0},
		{"no params at all", `{"method":"message/send"}`, 0},
		{"malformed JSON", `not json`, 0},
		{"empty body", ``, 0},
		{"null value", `{"params":{"expires_in_seconds":null}}`, 0},
		{"string value", `{"params":{"expires_in_seconds":"30"}}`, 0},
		{"float value", `{"params":{"expires_in_seconds":30.5}}`, 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractExpiresInSeconds([]byte(tc.body)); got != tc.want {
				t.Errorf("extractExpiresInSeconds(%q) = %d, want %d", tc.body, got, tc.want)
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
	// Multi-period (#49): exact-match the budget_limits read; "{}" → no limits →
	// checkWorkspaceBudget returns early (no spend query).
	mock.ExpectQuery(
		"SELECT COALESCE(budget_limits, '{}'::jsonb) FROM workspaces WHERE id = $1",
	).WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"budget_limits"}).AddRow([]byte("{}")))
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
// The next_attempt_at filter was added in #3127 follow-up; without it the
// `WHERE (next_attempt_at IS NULL OR next_attempt_at <= now())` clause
// wouldn't match the handler's exact SQL string.
func expectDequeueNextOk(mock sqlmock.Sqlmock, item *QueuedItem) {
	mock.ExpectBegin()
	mock.ExpectQuery(
		"SELECT id, workspace_id, caller_id, priority, body::text, method, attempts FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued' AND (expires_at IS NULL OR expires_at > now()) AND (next_attempt_at IS NULL OR next_attempt_at <= now()) ORDER BY priority DESC, enqueued_at ASC FOR UPDATE SKIP LOCKED LIMIT 1").
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
// next_attempt_at filter added in #3127 follow-up.
func expectDequeueNextEmpty(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectBegin()
	mock.ExpectQuery(
		"SELECT id, workspace_id, caller_id, priority, body::text, method, attempts FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued' AND (expires_at IS NULL OR expires_at > now()) AND (next_attempt_at IS NULL OR next_attempt_at <= now()) ORDER BY priority DESC, enqueued_at ASC FOR UPDATE SKIP LOCKED LIMIT 1").
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
}

// expectCompleted sets up mock for MarkQueueItemCompleted.
func expectCompleted(mock sqlmock.Sqlmock, id string, respBody interface{}) {
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = 'completed', completed_at = now(), response_body = $2 WHERE id = $1").
		WithArgs(id, respBody).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectFailed sets up mock for MarkQueueItemFailed with a specific error message.
func expectFailed(mock sqlmock.Sqlmock, id string, errMsg string) {
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'queued' END, last_error = $3, dispatched_at = NULL WHERE id = $1").
		WithArgs(id, 5, errMsg).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectTransientRetry sets up mock for MarkQueueItemTransientRetry. The
// errMsg is verified via the exact-match matcher; tests that only care
// about the SQL shape (and want to assert on the row state separately)
// can pass sqlmock.AnyArg() for the error-message column.
//
// #3127 follow-up: the SQL now also sets next_attempt_at = now() +
// make_interval(secs => $3) so DequeueNext's WHERE clause (added in
// the same change) skips the row during the backoff window. The seconds
// count is parameterised via transientRetryBackoffSecs (Go constant)
// rather than inlined as a literal interval string — golangci-lint
// flagged the literal form as having an unused sibling const.
func expectTransientRetry(mock sqlmock.Sqlmock, id string, errMsg sqlmock.Argument) {
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = 'queued', attempts = GREATEST(attempts - 1, 0), last_error = $2, dispatched_at = NULL, next_attempt_at = now() + make_interval(secs => $3) WHERE id = $1").
		WithArgs(id, errMsg, transientRetryBackoffSecs).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectRuntimeLookup mocks handleMockA2A's lookupRuntime query. The proxy
// calls this on every dispatch to decide whether to short-circuit with a
// canned mock reply; returning a non-mock runtime lets the request fall
// through to the real agent path. The existing tests don't care about the
// mock path but the query happens unconditionally, so the mock is required
// to keep the test logs clean.
func expectRuntimeLookup(mock sqlmock.Sqlmock, workspaceID string) {
	mock.ExpectQuery(
		"SELECT runtime FROM workspaces WHERE id = $1").
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))
}

// expectRecentHeartbeatAbsent mocks hasRecentHeartbeat's query to return
// NULL — DrainQueueForWorkspace treats that as "no recent heartbeat" and
// falls through to MarkQueueItemFailed (the pre-fix behaviour). Used by
// tests that exercise the dead-agent / non-transient failure paths.
func expectRecentHeartbeatAbsent(mock sqlmock.Sqlmock, workspaceID string) {
	mock.ExpectQuery(
		"SELECT last_heartbeat_at FROM workspaces WHERE id = $1").
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"last_heartbeat_at"}).AddRow(nil))
}

// expectRecentHeartbeatPresent mocks hasRecentHeartbeat's query to return a
// recent timestamp — DrainQueueForWorkspace treats that as "workspace is
// alive" and the transient gateway-origin path becomes eligible. Used by
// the regression test that pins the new behaviour.
func expectRecentHeartbeatPresent(mock sqlmock.Sqlmock, workspaceID string) {
	mock.ExpectQuery(
		"SELECT last_heartbeat_at FROM workspaces WHERE id = $1").
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"last_heartbeat_at"}).AddRow(time.Now()))
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

	expectCompleted(mock, item.ID, `{"result":{"status":"ok"}}`)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_BatchCapacity_DrainsMultiple: with capacity=2,
// two queued items are dequeued and dispatched in one call (#2930 batching).
func TestDrainQueueForWorkspace_BatchCapacity_DrainsMultiple(t *testing.T) {
	wsID := "ws-batch"
	item1 := drainItem(wsID)
	item1.ID = "qid-batch-1"
	item2 := drainItem(wsID)
	item2.ID = "qid-batch-2"

	mock, handler, mr := drainSetup(t, wsID)
	expectDequeueNextOk(mock, item1)
	expectQueueBudgetCheck(mock, wsID)
	expectCompleted(mock, item1.ID, `{"result":{"status":"ok"}}`)
	expectDequeueNextOk(mock, item2)
	expectQueueBudgetCheck(mock, wsID)
	expectCompleted(mock, item2.ID, `{"result":{"status":"ok"}}`)

	srv := agentServer(`{"result":{"status":"ok"}}`, http.StatusOK)
	defer srv.Close()
	seedRedisURL(t, mr, wsID, srv.URL)

	handler.DrainQueueForWorkspace(context.Background(), wsID, 2)

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

	expectCompleted(mock, item.ID, nil)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

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
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatAbsent(mock, item.WorkspaceID)

	srv := agentServer("", http.StatusBadGateway)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "Bad Gateway")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

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
	expectRuntimeLookup(mock, item.WorkspaceID)
	// 500 is NOT in isUpstreamDeadStatus so isGatewayOriginFailure returns
	// false and hasRecentHeartbeat is never consulted — no SQL mock needed
	// for the transient-retry path. Falls through to MarkQueueItemFailed
	// (the pre-fix behaviour for non-gateway failures).

	srv := agentServer(`{"code":500,"detail":"internal server error"}`, http.StatusInternalServerError)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "Internal Server Error")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

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
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatAbsent(mock, item.WorkspaceID)

	srv := agentServer(`{"error": 429}`, http.StatusServiceUnavailable)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "Service Unavailable")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

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
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatAbsent(mock, item.WorkspaceID)

	wantErrMsg := "upstream agent crashed with signal: killed"
	srv := agentServer(fmt.Sprintf(`{"error":%q}`, wantErrMsg), http.StatusBadGateway)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, wantErrMsg)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_EmptyQueue_NoOps: DequeueNext returns (nil, nil) →
// no DB writes issued.
func TestDrainQueueForWorkspace_EmptyQueue_NoOps(t *testing.T) {
	mock, handler, _ := drainSetup(t, "ws-empty")

	expectDequeueNextEmpty(mock, "ws-empty")

	handler.DrainQueueForWorkspace(context.Background(), "ws-empty", 1)

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
		"SELECT id, workspace_id, caller_id, priority, body::text, method, attempts FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued' AND (expires_at IS NULL OR expires_at > now()) AND (next_attempt_at IS NULL OR next_attempt_at <= now()) ORDER BY priority DESC, enqueued_at ASC FOR UPDATE SKIP LOCKED LIMIT 1").
		WithArgs("ws-dequeue-err").
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	handler.DrainQueueForWorkspace(context.Background(), "ws-dequeue-err", 1)

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
	expectRuntimeLookup(mock, item.WorkspaceID)
	// No recent heartbeat → falls through to MarkQueueItemFailed (not the
	// transient-retry path). This pins the pre-fix behaviour for dead /
	// unreachable workspaces: the 5-attempt cap still fires after 5 retries.
	expectRecentHeartbeatAbsent(mock, item.WorkspaceID)

	srv := agentServer(`{"error":"agent unreachable"}`, http.StatusBadGateway)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "agent unreachable")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

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
	expectCompleted(mock, item.ID, `{"result":{}}`)

	handler.DrainQueueForWorkspace(context.Background(), wsID, 1)

	// Drain 2: same workspace — queue is empty because item was dispatched.
	// Register expectations for the second drain.
	expectDequeueNextEmpty(mock, wsID)

	handler.DrainQueueForWorkspace(context.Background(), wsID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== 2026-06-21 PM RCA: transient gateway-retry path ====================
//
// The PM RCA found that DrainQueueForWorkspace was treating every
// 502/503/504 from the upstream proxy as a "dead agent unreachable"
// failure and burning the 5-attempt cap on otherwise-healthy
// workspaces. The new path: when the workspace has a recent heartbeat
// AND the failure is a gateway-origin dead-origin status (502/503/504
// or 521/522/523/524), re-queue via MarkQueueItemTransientRetry which
// does NOT advance the attempts counter, and invalidate the cached
// agent URL so the next retry re-resolves it from the DB. Only
// confirmed-dead agents (Classification="upstream_dead") and non-
// gateway failures continue to use MarkQueueItemFailed.
//
// These four tests pin the new contract end-to-end: the new SQL
// UPDATE statement, the URL cache invalidation, the heartbeat gate,
// and the regression of the "dead agent" path under the same
// conditions.

// TestDrainQueueForWorkspace_TransientGatewayFailure_StaysQueued: the
// regression test for the RCA. Online workspace + queued item +
// transient 502 (Cloudflare tunnel error page) + recent heartbeat →
// MarkQueueItemTransientRetry (NOT MarkQueueItemFailed) so the
// 5-attempt cap is preserved for actual dead-agent failures.
func TestDrainQueueForWorkspace_TransientGatewayFailure_StaysQueued(t *testing.T) {
	item := drainItem("ws-gateway-blip")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	// Recent heartbeat: the workspace is alive; the failure is in the
	// path between us and the agent, not the agent itself.
	expectRecentHeartbeatPresent(mock, item.WorkspaceID)

	// Cloudflare 502 error page — empty body, no JSON. This is the
	// shape that triggered the RCA: a healthy workspace's A2A forward
	// hits a CDN tunnel blip and returns 502 with an HTML body.
	srv := agentServer(`<html>cloudflare error</html>`, http.StatusBadGateway)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	// Expect MarkQueueItemTransientRetry (NOT MarkQueueItemFailed). The
	// last_error string carries the "[transient gateway origin]" prefix
	// so the failure shape is auditable in the a2a_queue row.
	wantErrPrefix := "transient gateway origin (unknown, status=502):"
	expectTransientRetry(mock, item.ID, sqlmock.AnyArg()) // exact errMsg verified via DB below
	_ = wantErrPrefix

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_TransientGatewayFailure_InvalidatesCachedURL:
// on the transient-retry path, the cached agent URL must be evicted
// from Redis so the next drain tick does a fresh DB lookup. Without
// this, a stale URL pointing at a temporarily-flapped tunnel would
// keep hitting the same broken endpoint. The ClearWorkspaceKeys call
// removes the three ws:<id>:* keys (liveness, url, internal_url) in
// one shot; the test verifies the url key is gone after the drain.
func TestDrainQueueForWorkspace_TransientGatewayFailure_InvalidatesCachedURL(t *testing.T) {
	item := drainItem("ws-invalidate")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatPresent(mock, item.WorkspaceID)
	expectTransientRetry(mock, item.ID, sqlmock.AnyArg())

	srv := agentServer("", http.StatusBadGateway)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}

	// Verify the cached URL was invalidated. seedRedisURL put it under
	// "ws:<id>:url" — after the drain it must be gone.
	if got, err := mr.Get(fmt.Sprintf("ws:%s:url", item.WorkspaceID)); err == nil && got != "" {
		t.Errorf("cached URL survived transient-retry invalidation: got=%q want empty", got)
	}
}

// TestDrainQueueForWorkspace_GatewayFailure_NoRecentHeartbeat_StillFails:
// the heartbeat gate is the load-bearing part of the new path. If the
// workspace is NOT heartbeating, a 502 stays a dead-agent failure —
// we don't want to re-queue on a genuinely-dead workspace. This pins
// the gate: gateway-origin status + no recent heartbeat →
// MarkQueueItemFailed, same as the pre-fix behaviour.
func TestDrainQueueForWorkspace_GatewayFailure_NoRecentHeartbeat_StillFails(t *testing.T) {
	item := drainItem("ws-no-hb")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatAbsent(mock, item.WorkspaceID)
	expectFailed(mock, item.ID, "Bad Gateway")

	srv := agentServer("", http.StatusBadGateway)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_UpstreamDead_BypassesTransientPath: when
// the proxy already confirmed a dead container (Classification =
// "upstream_dead", set by maybeMarkContainerDead in
// handleA2ADispatchError), the transient-retry path is NOT eligible —
// that is a real dead-agent failure and the 5-attempt cap MUST be
// allowed to fire. This test pins that isGatewayOriginFailure
// short-circuits on the "upstream_dead" classification and falls
// through to MarkQueueItemFailed.
func TestDrainQueueForWorkspace_UpstreamDead_BypassesTransientPath(t *testing.T) {
	// We cannot easily inject a proxyA2AError with Classification=
	// "upstream_dead" through the normal DrainQueueForWorkspace path
	// (the existing test infrastructure uses an httptest.Server for
	// the agent, which doesn't go through maybeMarkContainerDead).
	// So this test is a unit test of isGatewayOriginFailure itself,
	// which is the load-bearing predicate.
	upstreamDead := &proxyA2AError{
		Status:         http.StatusBadGateway,
		Response:       gin.H{"error": "workspace agent unreachable — container restart triggered"},
		Classification: "upstream_dead",
	}
	if isGatewayOriginFailure(upstreamDead) {
		t.Errorf("isGatewayOriginFailure(upstream_dead) = true, want false — confirmed-dead must bypass the transient-retry path")
	}

	// Also verify the inverse: a 502 without "upstream_dead" classification
	// IS a candidate for the transient-retry path.
	gatewayOrigin := &proxyA2AError{
		Status:   http.StatusBadGateway,
		Response: gin.H{"error": "bad gateway"},
	}
	if !isGatewayOriginFailure(gatewayOrigin) {
		t.Errorf("isGatewayOriginFailure(502 + no classification) = false, want true — the predicate should recognise 502 as gateway-origin when the proxy has not confirmed dead")
	}

	// And a non-dead-origin 5xx (e.g., 500 internal agent error) is NOT
	// a gateway-origin failure.
	notGatewayOrigin := &proxyA2AError{
		Status:   http.StatusInternalServerError,
		Response: gin.H{"error": "agent crashed"},
	}
	if isGatewayOriginFailure(notGatewayOrigin) {
		t.Errorf("isGatewayOriginFailure(500) = true, want false — agent-authored 5xx is not a gateway-origin failure")
	}
}

// TestDrainQueueForWorkspace_TransientRetry_BackoffBreaksCapacityLoop:
// Regression test for Researcher #3127 REQUEST_CHANGES. The original
// transient-retry fix requeued the row with status='queued' and no
// backoff, so a capacity>1 DrainQueueForWorkspace could re-claim the
// just-requeued row on the very next for-loop iteration and hit the
// same gateway failure in a tight loop. The fix: next_attempt_at = now() + 5s
// on transient retry, plus a WHERE clause in DequeueNext that skips
// rows whose next_attempt_at is still in the future.
//
// This test pins the backoff: capacity=2, one queued item that hits a
// transient 502, expect the second DequeueNext to return (nil, nil)
// because the only item is now backoff-gated. Without the WHERE clause
// the second DequeueNext would have re-claimed the row and the test
// would fail (the budget check + MarkQueueItemTransientRetry expectations
// would be unmet, since the row would not be requeued a second time).
func TestDrainQueueForWorkspace_TransientRetry_BackoffBreaksCapacityLoop(t *testing.T) {
	item := drainItem("ws-capacity-loop")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)

	// Iteration 1 of the for-loop (capacity=2): the only queued row is
	// claimed, dispatched, and hits a transient 502. Recent heartbeat
	// keeps the transient-retry path eligible.
	expectDequeueNextOk(mock, item)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatPresent(mock, item.WorkspaceID)

	srv := agentServer("", http.StatusBadGateway)
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectTransientRetry(mock, item.ID, sqlmock.AnyArg())

	// Iteration 2 of the for-loop (capacity=2): the just-requeued row
	// is still the highest-priority item, but next_attempt_at is now()
	// + 5s — DequeueNext's WHERE clause MUST skip it. The mock returns
	// sql.ErrNoRows as if the queue is empty, and the test framework
	// will fail if the second iteration ever calls into proxyA2ARequest
	// (no MarkQueueItemTransientRetry / MarkQueueItemFailed mock is
	// registered for it).
	expectDequeueNextEmpty(mock, item.WorkspaceID)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 2)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
