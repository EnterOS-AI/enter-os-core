package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ---------- Delegate: missing target_id → 400 ----------

func TestDelegate_MissingTargetID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	body := `{"task":"do something"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-source/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Delegate: missing task → 400 ----------

func TestDelegate_MissingTask(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	body := `{"target_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-source/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Delegate: invalid UUID target_id → 400 ----------

func TestDelegate_InvalidUUIDTargetID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	body := `{"target_id":"not-a-valid-uuid","task":"do something"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-source/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "target_id must be a valid UUID" {
		t.Errorf("expected UUID error message, got %v", resp["error"])
	}
}

// ---------- Delegate: self-delegation → 400 ----------

func TestDelegate_SelfDelegation_Rejected(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	// Use the same UUID for both source and target to trigger the self-delegation guard.
	selfID := "11111111-2222-3333-4444-555555555555"

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: selfID}}
	body := `{"target_id":"` + selfID + `","task":"do something"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+selfID+"/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "self-delegation not permitted" {
		t.Errorf("expected 'self-delegation not permitted', got %v", resp["error"])
	}
}

// ---------- Delegate: success → 202 with delegation_id ----------

func TestDelegate_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	targetID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Expect INSERT into activity_logs for delegation tracking
	// (6th arg is idempotency_key — nil here since the request omits it)
	mock.ExpectExec("INSERT INTO activity_logs").
		WithArgs("ws-source", "ws-source", targetID, "Delegating to "+targetID, sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect RecordAndBroadcast INSERT into structure_events
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	body := fmt.Sprintf(`{"target_id":"%s","task":"write unit tests"}`, targetID)
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-source/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["delegation_id"] == nil || resp["delegation_id"] == "" {
		t.Error("expected non-empty delegation_id in response")
	}
	if resp["status"] != "delegated" {
		t.Errorf("expected status 'delegated', got %v", resp["status"])
	}
	if resp["target_id"] != targetID {
		t.Errorf("expected target_id %s, got %v", targetID, resp["target_id"])
	}
	// Should NOT have a warning when DB insert succeeds
	if resp["warning"] != nil {
		t.Errorf("expected no warning, got %v", resp["warning"])
	}

	// Wait for background goroutine to run (it will try DB queries that aren't mocked,
	// but we don't want it to race with test cleanup)
	time.Sleep(100 * time.Millisecond)
}

// ---------- Delegate: DB insert fails → still 202 with warning ----------

func TestDelegate_DBInsertFails_Still202WithWarning(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	targetID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// DB insert fails (6th arg = idempotency_key, nil for this test)
	mock.ExpectExec("INSERT INTO activity_logs").
		WithArgs("ws-source", "ws-source", targetID, "Delegating to "+targetID, sqlmock.AnyArg(), nil).
		WillReturnError(fmt.Errorf("database connection lost"))

	// RecordAndBroadcast still fires
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	body := fmt.Sprintf(`{"target_id":"%s","task":"write unit tests"}`, targetID)
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-source/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["warning"] == nil {
		t.Error("expected warning when DB insert fails")
	}
	if resp["delegation_id"] == nil || resp["delegation_id"] == "" {
		t.Error("expected non-empty delegation_id even on DB failure")
	}

	// Wait for background goroutine
	time.Sleep(100 * time.Millisecond)
}

// ---------- ListDelegations: empty results → 200 with [] ----------

func TestListDelegations_Empty(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	rows := sqlmock.NewRows([]string{
		"id", "activity_type", "source_id", "target_id",
		"summary", "status", "error_detail", "response_body",
		"delegation_id", "created_at",
	})
	mock.ExpectQuery("SELECT id, activity_type").
		WithArgs("ws-source").
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-source/delegations", nil)

	dh.ListDelegations(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected empty array, got %d entries", len(resp))
	}
}

// ---------- ListDelegations: with results → 200 with entries ----------

func TestListDelegations_WithResults(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"id", "activity_type", "source_id", "target_id",
		"summary", "status", "error_detail", "response_body",
		"delegation_id", "created_at",
	}).
		AddRow("1", "delegation", "ws-source", "ws-target",
			"Delegating to ws-target", "pending", "", "",
			"del-111", now).
		AddRow("2", "delegation", "ws-source", "ws-target",
			"Delegation completed (hello world)", "completed", "", "hello world",
			"del-111", now.Add(time.Minute))

	mock.ExpectQuery("SELECT id, activity_type").
		WithArgs("ws-source").
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-source/delegations", nil)

	dh.ListDelegations(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(resp))
	}

	// Check first entry (pending delegation)
	if resp[0]["type"] != "delegation" {
		t.Errorf("expected type 'delegation', got %v", resp[0]["type"])
	}
	if resp[0]["status"] != "pending" {
		t.Errorf("expected status 'pending', got %v", resp[0]["status"])
	}
	if resp[0]["delegation_id"] != "del-111" {
		t.Errorf("expected delegation_id 'del-111', got %v", resp[0]["delegation_id"])
	}
	if resp[0]["source_id"] != "ws-source" {
		t.Errorf("expected source_id 'ws-source', got %v", resp[0]["source_id"])
	}
	if resp[0]["target_id"] != "ws-target" {
		t.Errorf("expected target_id 'ws-target', got %v", resp[0]["target_id"])
	}

	// Check second entry (completed, has response_preview)
	if resp[1]["status"] != "completed" {
		t.Errorf("expected status 'completed', got %v", resp[1]["status"])
	}
	if resp[1]["response_preview"] != "hello world" {
		t.Errorf("expected response_preview 'hello world', got %v", resp[1]["response_preview"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- #74: isTransientProxyError retry classification ----------

func TestIsTransientProxyError_RetriesOnRestartRaceStatuses(t *testing.T) {
	cases := []struct {
		name   string
		err    *proxyA2AError
		expect bool
	}{
		{"nil", nil, false},
		// 503 with restarting:true — container was dead; restart triggered.
		// Message was NOT delivered (dead container). Safe to retry (#74).
		{"503 container restart triggered — retry",
			&proxyA2AError{Status: http.StatusServiceUnavailable, Response: gin.H{"restarting": true}}, true},
		// 503 with busy:true — agent is alive, mid-synthesis on the delivered
		// message. Retrying would double-deliver (#689). Must NOT retry.
		{"503 agent busy (double-delivery risk) — no retry",
			&proxyA2AError{Status: http.StatusServiceUnavailable, Response: gin.H{"busy": true, "retry_after": 30}}, false},
		// 503 with no qualifying flag — conservative: don't retry.
		{"503 plain (no restarting flag) — no retry",
			&proxyA2AError{Status: http.StatusServiceUnavailable}, false},
		// 502 = connection refused = message not delivered → safe to retry.
		{"502 bad gateway (connection refused) — retry",
			&proxyA2AError{Status: http.StatusBadGateway}, true},
		{"404 workspace not found",
			&proxyA2AError{Status: http.StatusNotFound}, false},
		{"403 access denied — static, don't retry",
			&proxyA2AError{Status: http.StatusForbidden}, false},
		{"400 bad request — static, don't retry",
			&proxyA2AError{Status: http.StatusBadRequest}, false},
		{"500 generic — conservative, don't retry",
			&proxyA2AError{Status: http.StatusInternalServerError}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientProxyError(tc.err); got != tc.expect {
				t.Errorf("isTransientProxyError(%+v) = %v, want %v", tc.err, got, tc.expect)
			}
		})
	}
}

// TestIsDeliveryConfirmedSuccess — regression guard for #159: the proxy can
// return a complete 2xx body and THEN raise a transport error (e.g. the TCP
// connection drops after the response is received but before close). In that
// case the agent did the work; marking the delegation "failed" causes the
// retry-storm + Restart-workspace cascade described in #159. The new helper
// distinguishes this from genuine failures.
func TestIsDeliveryConfirmedSuccess(t *testing.T) {
	connErr := &proxyA2AError{Status: http.StatusOK, Response: gin.H{}}
	cases := []struct {
		name     string
		proxyErr *proxyA2AError
		status   int
		body     []byte
		expect   bool
	}{
		// The new branch: 2xx + body + transport error → recover as success.
		{"200 + body + connreset (THE bug fix path)", connErr, http.StatusOK, []byte(`{"text":"ok"}`), true},
		{"299 + body + connreset (boundary high)", connErr, 299, []byte(`{"text":"ok"}`), true},
		{"200 + body + connreset (boundary low)", connErr, 200, []byte(`{"x":1}`), true},
		// Negative cases: any one of the three preconditions failing → false.
		{"nil proxyErr (no decision to make)", nil, http.StatusOK, []byte(`{"text":"ok"}`), false},
		{"empty body (no work-result to recover)", connErr, http.StatusOK, []byte{}, false},
		{"nil body (no work-result to recover)", connErr, http.StatusOK, nil, false},
		{"4xx with body — agent signalled failure, do not promote", connErr, http.StatusBadRequest, []byte(`{"err":"bad"}`), false},
		{"5xx with body — agent signalled failure, do not promote", connErr, http.StatusInternalServerError, []byte(`{"err":"crash"}`), false},
		{"3xx with body — redirect, not a result", connErr, 301, []byte(`{"loc":"/x"}`), false},
		{"199 status (under 200) — not a 2xx", connErr, 199, []byte(`{"x":1}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDeliveryConfirmedSuccess(tc.proxyErr, tc.status, tc.body); got != tc.expect {
				t.Errorf("isDeliveryConfirmedSuccess(%v, %d, %q) = %v, want %v",
					tc.proxyErr, tc.status, string(tc.body), got, tc.expect)
			}
		})
	}
}

func TestIsQueuedProxyResponse(t *testing.T) {
	// Regression guard for the chat-leak bug: when the proxy returns
	// 202 with a queued-shape body, executeDelegation must classify it
	// as "queued" — not "completed". Mis-classifying it causes the
	// queued JSON to land in activity_logs.summary, which the LLM then
	// echoes verbatim into the agent chat as
	// "Delegation completed: Delegation completed (workspace agent
	// busy — request queued, will dispatch...)".
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "real proxy busy-enqueue body",
			body: `{"queued":true,"queue_id":"d0993390-5f5a-4f5d-90a2-66639e53e3c9","queue_depth":1,"message":"workspace agent busy — request queued, will dispatch when capacity available"}`,
			want: true,
		},
		{"queued false explicitly", `{"queued":false}`, false},
		{"queued field absent (real A2A reply)", `{"jsonrpc":"2.0","id":"1","result":{"kind":"message","parts":[{"kind":"text","text":"hi"}]}}`, false},
		{"non-bool queued value (defensive)", `{"queued":"true"}`, false},
		{"malformed JSON", `not-json`, false},
		{"empty body", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isQueuedProxyResponse([]byte(tc.body)); got != tc.want {
				t.Errorf("isQueuedProxyResponse(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestDelegationRetryDelay_IsSaneWindow(t *testing.T) {
	// Regression guard: the retry delay must be long enough for the
	// reactive URL refresh in proxyA2ARequest to kick in (which involves
	// a Docker IsRunning check + DB update + RestartByID call) but short
	// enough that a transient failure doesn't block the 30-min outer
	// timeout. 8s is the chosen balance.
	if delegationRetryDelay < 2*time.Second || delegationRetryDelay > 30*time.Second {
		t.Errorf("delegationRetryDelay = %v, expected [2s, 30s]", delegationRetryDelay)
	}
}

// ---------- #64: Record + UpdateStatus endpoints ----------

func TestDelegationRecord_InsertsActivityLogRow(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	h := NewDelegationHandler(wh, broadcaster)

	mock.ExpectExec("INSERT INTO activity_logs").
		WithArgs(
			"550e8400-e29b-41d4-a716-446655440000",                // workspace_id
			"550e8400-e29b-41d4-a716-446655440000",                // source_id
			"550e8400-e29b-41d4-a716-446655440001",                // target_id
			"Delegating to 550e8400-e29b-41d4-a716-446655440001",  // summary
			sqlmock.AnyArg(),                                       // request_body (jsonb)
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// RecordAndBroadcast INSERT for DELEGATION_SENT
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	body := `{"target_id":"550e8400-e29b-41d4-a716-446655440001","task":"hello","delegation_id":"del-xyz"}`
	c.Request = httptest.NewRequest("POST", "/delegations/record", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Record(c)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["delegation_id"] != "del-xyz" {
		t.Errorf("expected delegation_id=del-xyz, got %v", resp["delegation_id"])
	}
	if resp["status"] != "recorded" {
		t.Errorf("expected status=recorded, got %v", resp["status"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDelegationRecord_RejectsInvalidUUID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	h := NewDelegationHandler(wh, broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	body := `{"target_id":"not-a-uuid","task":"x","delegation_id":"del-1"}`
	c.Request = httptest.NewRequest("POST", "/delegations/record", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Record(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid target UUID, got %d", w.Code)
	}
}

func TestDelegationUpdateStatus_CompletedInsertsResultRow(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	h := NewDelegationHandler(wh, broadcaster)

	// updateDelegationStatus UPDATE
	mock.ExpectExec("UPDATE activity_logs").
		WithArgs("completed", "", "550e8400-e29b-41d4-a716-446655440000", "del-xyz").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// delegate_result INSERT
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// DELEGATION_COMPLETE broadcast
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "delegation_id", Value: "del-xyz"},
	}
	body := `{"status":"completed","response_preview":"task finished ok"}`
	c.Request = httptest.NewRequest("POST", "/delegations/del-xyz/update", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpdateStatus(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDelegationUpdateStatus_RejectsUnknownStatus(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	h := NewDelegationHandler(wh, broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "delegation_id", Value: "del-xyz"},
	}
	body := `{"status":"in_progress"}`
	c.Request = httptest.NewRequest("POST", "/delegations/del-xyz/update", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpdateStatus(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestDelegationUpdateStatus_FailedBroadcastsFailureEvent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	h := NewDelegationHandler(wh, broadcaster)

	mock.ExpectExec("UPDATE activity_logs").
		WithArgs("failed", "boom", "550e8400-e29b-41d4-a716-446655440000", "del-xyz").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// DELEGATION_FAILED broadcast
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "delegation_id", Value: "del-xyz"},
	}
	body := `{"status":"failed","error":"boom"}`
	c.Request = httptest.NewRequest("POST", "/delegations/del-xyz/update", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpdateStatus(c)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- #124 — idempotency: replay returns existing delegation ----------

func TestDelegate_IdempotentReplayReturnsExistingDelegation(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	targetID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	existingID := "11111111-2222-3333-4444-555555555555"

	// Lookup by (workspace_id, idempotency_key) — finds an in-flight row.
	mock.ExpectQuery("SELECT request_body->>'delegation_id', status, target_id").
		WithArgs("ws-source", "key-abc").
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "status", "target_id"}).
			AddRow(existingID, "dispatched", targetID))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	body := fmt.Sprintf(`{"target_id":"%s","task":"work","idempotency_key":"key-abc"}`, targetID)
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-source/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (idempotent hit), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["delegation_id"] != existingID {
		t.Errorf("expected existing delegation_id %s, got %v", existingID, resp["delegation_id"])
	}
	if resp["idempotent_hit"] != true {
		t.Errorf("expected idempotent_hit=true, got %v", resp["idempotent_hit"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- #124 — idempotency: failed prior row is released, new insert wins ----------

func TestDelegate_IdempotentFailedRowIsReleasedAndReplaced(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	targetID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Lookup finds a failed prior attempt.
	mock.ExpectQuery("SELECT request_body->>'delegation_id', status, target_id").
		WithArgs("ws-source", "retry-key").
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "status", "target_id"}).
			AddRow("old-failed-id", "failed", targetID))
	// Failed row is deleted to release the unique slot.
	mock.ExpectExec("DELETE FROM activity_logs").
		WithArgs("ws-source", "retry-key").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Fresh insert with the same idempotency key.
	mock.ExpectExec("INSERT INTO activity_logs").
		WithArgs("ws-source", "ws-source", targetID, "Delegating to "+targetID, sqlmock.AnyArg(), "retry-key").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	body := fmt.Sprintf(`{"target_id":"%s","task":"retry","idempotency_key":"retry-key"}`, targetID)
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-source/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (fresh delegation after failed retry), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["idempotent_hit"] == true {
		t.Error("expected fresh delegation, not idempotent_hit")
	}
	if resp["delegation_id"] == "" || resp["delegation_id"] == nil {
		t.Error("expected non-empty delegation_id on retry")
	}
	time.Sleep(100 * time.Millisecond)
}

// ---------- #124 — idempotency: concurrent insert race resolves to existing ----------

func TestDelegate_IdempotentRaceUniqueViolationReturnsExisting(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	targetID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	winnerID := "99999999-8888-7777-6666-555555555555"

	// Lookup finds nothing first.
	mock.ExpectQuery("SELECT request_body->>'delegation_id', status, target_id").
		WithArgs("ws-source", "race-key").
		WillReturnError(fmt.Errorf("sql: no rows in result set"))
	// Insert loses the race against a concurrent caller.
	mock.ExpectExec("INSERT INTO activity_logs").
		WithArgs("ws-source", "ws-source", targetID, "Delegating to "+targetID, sqlmock.AnyArg(), "race-key").
		WillReturnError(fmt.Errorf("pq: duplicate key value violates unique constraint \"activity_logs_idempotency_uniq\""))
	// Re-query returns the winner.
	mock.ExpectQuery("SELECT request_body->>'delegation_id', status").
		WithArgs("ws-source", "race-key").
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "status"}).
			AddRow(winnerID, "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	body := fmt.Sprintf(`{"target_id":"%s","task":"race","idempotency_key":"race-key"}`, targetID)
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-source/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (race resolved to winner), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["delegation_id"] != winnerID {
		t.Errorf("expected winner delegation_id %s, got %v", winnerID, resp["delegation_id"])
	}
	if resp["idempotent_hit"] != true {
		t.Errorf("expected idempotent_hit=true on race resolution, got %v", resp["idempotent_hit"])
	}
}

// ==================== Direct unit tests for extracted helpers ====================

// --- bindDelegateRequest ---

func TestBindDelegateRequest_ValidJSON(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"target_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","task":"hi"}`
	c.Request = httptest.NewRequest("POST", "/x", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	var out delegateRequest
	if err := bindDelegateRequest(c, &out); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Task != "hi" {
		t.Errorf("got task %q", out.Task)
	}
}

func TestBindDelegateRequest_InvalidJSON(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/x", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	var out delegateRequest
	if err := bindDelegateRequest(c, &out); err == nil {
		t.Fatal("expected error")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestBindDelegateRequest_InvalidTargetUUID(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/x", bytes.NewBufferString(`{"target_id":"not-uuid","task":"x"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	var out delegateRequest
	if err := bindDelegateRequest(c, &out); err == nil {
		t.Fatal("expected error")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- lookupIdempotentDelegation ---

func TestLookupIdempotentDelegation_NoKey(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	if hit := lookupIdempotentDelegation(context.Background(), c, "ws-x", ""); hit {
		t.Error("empty key should never hit")
	}
}

func TestLookupIdempotentDelegation_NoMatch(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	mock.ExpectQuery("SELECT request_body->>'delegation_id', status, target_id").
		WithArgs("ws-x", "some-key").
		WillReturnError(fmt.Errorf("sql: no rows"))

	if hit := lookupIdempotentDelegation(context.Background(), c, "ws-x", "some-key"); hit {
		t.Error("expected false when no row found")
	}
}

func TestLookupIdempotentDelegation_FailedRowDeleted(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	mock.ExpectQuery("SELECT request_body->>'delegation_id', status, target_id").
		WithArgs("ws-x", "k").
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "status", "target_id"}).
			AddRow("old", "failed", "ws-target"))
	mock.ExpectExec("DELETE FROM activity_logs").
		WithArgs("ws-x", "k").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if hit := lookupIdempotentDelegation(context.Background(), c, "ws-x", "k"); hit {
		t.Error("failed row should be released, returning false")
	}
}

func TestLookupIdempotentDelegation_ExistingPending(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	mock.ExpectQuery("SELECT request_body->>'delegation_id', status, target_id").
		WithArgs("ws-x", "k").
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "status", "target_id"}).
			AddRow("del-123", "pending", "ws-target"))

	if hit := lookupIdempotentDelegation(context.Background(), c, "ws-x", "k"); !hit {
		t.Fatal("expected hit=true")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["delegation_id"] != "del-123" || resp["idempotent_hit"] != true {
		t.Errorf("unexpected response: %v", resp)
	}
}

// --- insertDelegationRow ---

func TestInsertDelegationRow_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	out := insertDelegationRow(context.Background(), c,
		"ws-src",
		delegateRequest{TargetID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Task: "hi"},
		"del-1")
	if out != insertOK {
		t.Errorf("got %v, want insertOK", out)
	}
}

func TestInsertDelegationRow_IdempotentConflict(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnError(fmt.Errorf("pq: duplicate key value violates unique constraint"))
	mock.ExpectQuery("SELECT request_body->>'delegation_id', status").
		WithArgs("ws-src", "k1").
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "status"}).
			AddRow("winner-del", "pending"))

	out := insertDelegationRow(context.Background(), c,
		"ws-src",
		delegateRequest{TargetID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Task: "hi", IdempotencyKey: "k1"},
		"loser-del")
	if out != insertHandledByIdempotent {
		t.Errorf("got %v, want insertHandledByIdempotent", out)
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestInsertDelegationRow_OtherDBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// Without IdempotencyKey, the follow-up SELECT is skipped — any insert
	// error falls straight to insertTrackingUnavailable.
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnError(fmt.Errorf("connection refused"))

	out := insertDelegationRow(context.Background(), c,
		"ws-src",
		delegateRequest{TargetID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Task: "hi"},
		"del-x")
	if out != insertTrackingUnavailable {
		t.Errorf("got %v, want insertTrackingUnavailable", out)
	}
}

// Verify the enum zero-value sentinel is defined and distinct from real outcomes.
func TestInsertDelegationOutcome_ZeroValueIsUnknown(t *testing.T) {
	var zero insertDelegationOutcome
	if zero != insertOutcomeUnknown {
		t.Errorf("zero-value insertDelegationOutcome should equal insertOutcomeUnknown")
	}
	if insertOutcomeUnknown == insertOK {
		t.Errorf("insertOutcomeUnknown must not collide with insertOK")
	}
}

// ==================== executeDelegation — delivery-confirmed proxy error regression tests ====================
//
// These test the fix for issue #159: when proxyA2ARequest returns an error but we have a
// non-empty response body with a 2xx status code, executeDelegation must treat it as success.
// The error is a delivery/transport error (e.g., connection reset after response was received).
// Previously, executeDelegation marked these as "failed" even though the work was done,
// causing retry storms and "error" rendering in canvas despite the response being available.
//
// Test strategy: spin up a mock A2A agent server, set up the source/target DB rows, call
// executeDelegation directly, and verify the activity_logs status and delegation status.

const testDelegationID = "del-159-test"
const testSourceID = "ws-source-159"
const testTargetID = "ws-target-159"

// expectExecuteDelegationBase sets up sqlmock expectations for the DB queries that
// executeDelegation always makes, regardless of outcome.
func expectExecuteDelegationBase(mock sqlmock.Sqlmock) {
	// updateDelegationStatus: dispatched
	// Uses prefix match — sqlmock regexes match the full query string.
	mock.ExpectExec("UPDATE activity_logs SET status").
		WithArgs("dispatched", "", testSourceID, testDelegationID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// CanCommunicate (source=target self-call is always allowed — no DB lookup needed)
	// resolveAgentURL: reads ws:{id}:url from Redis, falls back to DB for target
	mock.ExpectQuery("SELECT url, status FROM workspaces WHERE id = ").
		WithArgs(testTargetID).
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}).AddRow("", "online"))
}

// expectExecuteDelegationSuccess sets up expectations for a completed delegation.
func expectExecuteDelegationSuccess(mock sqlmock.Sqlmock, respBody string) {
	// INSERT activity_logs for delegation completion (response_body status = 'completed')
	mock.ExpectExec("INSERT INTO activity_logs").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "completed").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// updateDelegationStatus: completed
	mock.ExpectExec("UPDATE activity_logs SET status").
		WithArgs("completed", "", testSourceID, testDelegationID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectExecuteDelegationFailed sets up expectations for a failed delegation.
func expectExecuteDelegationFailed(mock sqlmock.Sqlmock) {
	// INSERT activity_logs for delegation failure (response_body status = 'failed')
	mock.ExpectExec("INSERT INTO activity_logs").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "failed").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// updateDelegationStatus: failed
	mock.ExpectExec("UPDATE activity_logs SET status").
		WithArgs("failed", sqlmock.AnyArg(), testSourceID, testDelegationID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestExecuteDelegation_DeliveryConfirmedProxyError_TreatsAsSuccess is the primary regression
// test for issue #159. The scenario:
//   - Attempt 1: server sends 200 OK headers + partial body, then closes connection.
//     proxyA2ARequest: body read gets io.EOF (partial body read), returns (200, <partial>, BadGateway).
//     isTransientProxyError(BadGateway) = TRUE → retry.
//   - Attempt 2: server does the same thing (closes after partial body).
//     proxyA2ARequest: same (200, <partial>, BadGateway).
//     isTransientProxyError(BadGateway) = TRUE → retry AGAIN (but outer context will fire soon,
//     or we get one more attempt). For the test we let it run.
//     POST-FIX: the executeDelegation new condition sees status=200, body=<partial>, err!=nil
//     and routes to handleSuccess immediately.
//
// The key pre/post-fix difference: pre-fix, executeDelegation received status=0 (hardcoded)
// even when the server sent 200, so the condition always failed. Post-fix, status=200 is
// preserved through the error return path (proxyA2ARequest now returns resp.StatusCode, respBody).
// In this test the retry ultimately succeeds (server eventually sends full body), but
// the critical assertion is that a 2xx partial-body delivery-confirmed response is never
// classified as "failed" — it always routes to success.
func TestExecuteDelegation_DeliveryConfirmedProxyError_TreatsAsSuccess(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	// Server that sends a 200 response with declared Content-Length but closes
	// the connection before sending all bytes. Go's http.Client sees io.EOF on
	// the body read. proxyA2ARequest captures the partial body + status=200 and
	// returns (200, <partial>, error). executeDelegation's new condition sees
	// status=200 + body > 0 + error != nil → routes to handleSuccess.
	var wg sync.WaitGroup
	wg.Add(1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Consume the HTTP request
		buf := make([]byte, 2048)
		conn.Read(buf)
		// Send 200 OK with Content-Length: 100 but only 74 bytes of body
		// (less than declared length → io.LimitReader returns io.EOF after reading all 74)
		resp := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n"
		resp += `{"result":{"parts":[{"text":"work completed successfully"}]}}` // 74 bytes
		conn.Write([]byte(resp))
		// Close immediately — client gets io.EOF on body read
	}()

	agentURL := "http://" + ln.Addr().String()
	mr.Set(fmt.Sprintf("ws:%s:url", testTargetID), agentURL)
	allowLoopbackForTest(t)

	expectExecuteDelegationBase(mock)
	expectExecuteDelegationSuccess(mock, `{"result":{"parts":[{"text":"work completed successfully"}]}}`)

	// Execute synchronously (not as a goroutine) so we can check DB state immediately.
	// The handler fires it as goroutine; we call it directly for deterministic testing.
	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)

	time.Sleep(100 * time.Millisecond) // let DB writes settle

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestExecuteDelegation_ProxyErrorNon2xx_RemainsFailed verifies that the pre-fix failure
// path is unchanged when proxyA2ARequest returns a delivery-confirmed error with a non-2xx
// status code (e.g., 500 Internal Server Error with partial body read before connection drop).
// The new condition requires status >= 200 && status < 300, so non-2xx always routes to failure.
func TestExecuteDelegation_ProxyErrorNon2xx_RemainsFailed(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	// Server returns 500 with declared Content-Length but closes connection early.
	// proxyA2ARequest: reads 500 headers, partial body, then connection drop → body read error.
	// Returns (500, <partial_body>, BadGateway).
	// New condition: status=500 is NOT >= 200 && < 300 → routes to failure.
	// isTransientProxyError(500) = false → no retry.
	var wg sync.WaitGroup
	wg.Add(1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 2048)
		conn.Read(buf)
		// 500 with Content-Length: 100 but only ~60 bytes of body
		resp := "HTTP/1.1 500 Internal Server Error\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n"
		resp += `{"error":"agent crashed"}` // ~24 bytes, less than declared
		conn.Write([]byte(resp))
		// Close immediately — client gets io.EOF on body read
	}()

	agentURL := "http://" + ln.Addr().String()
	mr.Set(fmt.Sprintf("ws:%s:url", testTargetID), agentURL)
	allowLoopbackForTest(t)

	expectExecuteDelegationBase(mock)
	expectExecuteDelegationFailed(mock)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)

	time.Sleep(100 * time.Millisecond)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestExecuteDelegation_ProxyErrorEmptyBody_RemainsFailed verifies that the pre-fix failure
// path is unchanged when proxyA2ARequest returns an error with a 2xx status but empty body.
// The new condition requires len(respBody) > 0, so empty body routes to failure.
func TestExecuteDelegation_ProxyErrorEmptyBody_RemainsFailed(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	// Server returns 502 Bad Gateway — proxyA2ARequest returns 502, body="" (empty), error != nil.
	// New condition: proxyErr != nil && len(respBody) > 0 && status >= 200 && status < 300
	// → len(respBody) == 0 → condition FALSE → falls through to failure.
	// isTransientProxyError(502) is TRUE → retry → same result → failure.
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		// No body — connection closes normally
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", testTargetID), agentServer.URL)
	allowLoopbackForTest(t)

	// First attempt: updateDelegationStatus(dispatched) — from expectExecuteDelegationBase
	expectExecuteDelegationBase(mock)
	// Second attempt (retry): updateDelegationStatus(dispatched) again
	mock.ExpectExec("UPDATE activity_logs SET status").
		WithArgs("dispatched", "", testSourceID, testDelegationID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Failure: INSERT + UPDATE (failed)
	expectExecuteDelegationFailed(mock)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)

	time.Sleep(100 * time.Millisecond)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestExecuteDelegation_CleanProxyResponse_Unchanged verifies that a clean proxy response
// (no error, 200 with body) is unaffected by the new condition. This is the baseline:
// proxyErr == nil so the new condition never fires.
func TestExecuteDelegation_CleanProxyResponse_Unchanged(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":{"parts":[{"text":"all good"}]}}`))
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", testTargetID), agentServer.URL)
	allowLoopbackForTest(t)

	expectExecuteDelegationBase(mock)
	expectExecuteDelegationSuccess(mock, `{"result":{"parts":[{"text":"all good"}]}}`)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)

	time.Sleep(100 * time.Millisecond)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
}
