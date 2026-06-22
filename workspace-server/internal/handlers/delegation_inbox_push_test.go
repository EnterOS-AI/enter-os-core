package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// delegation_inbox_push_test.go — coverage for the RFC #2829 PR-2
// result-push behavior. The push is feature-flagged via
// DELEGATION_RESULT_INBOX_PUSH=1; default off keeps the existing
// strict-sqlmock test surface unchanged.
//
// What we pin:
//   1. Flag off (default) → no a2a_receive INSERT fires.
//   2. Flag on, status=completed → a2a_receive row written with the
//      response_preview and no error_detail.
//   3. Flag on, status=failed → a2a_receive row written with status=error
//      and the error_detail set.
//   4. INSERT failure on inbox-push does NOT bubble up — UpdateStatus
//      still returns 200.

// ---------- pushDelegationResultToInbox in isolation ----------

func TestPushDelegationResultToInbox_FlagOff_NoSQL(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "")

	pushDelegationResultToInbox(
		context.Background(),
		"caller", "deleg-1", "completed", "answer body", "",
	)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("flag off must not fire SQL: %v", err)
	}
}

func TestPushDelegationResultToInbox_FlagOn_CompletedInsertsA2AReceiveRow(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(
			"caller-ws",
			"caller-ws", // source_id mirrors workspace_id
			"Delegation result delivered",
			sqlmock.AnyArg(), // request_body json
			sqlmock.AnyArg(), // response_body json
			"ok",
			"", // error_detail empty for completed
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	pushDelegationResultToInbox(
		context.Background(),
		"caller-ws", "deleg-1", "completed", "answer body", "",
	)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestPushDelegationResultToInbox_FlagOn_FailedInsertsErrorRow(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(
			"caller-ws",
			"caller-ws",
			"Delegation failed",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"error",
			"target unreachable",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	pushDelegationResultToInbox(
		context.Background(),
		"caller-ws", "deleg-2", "failed", "", "target unreachable",
	)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// ---------- UpdateStatus end-to-end ----------

func TestUpdateStatus_FlagOn_PushesA2AReceiveOnCompleted(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	// 1. updateDelegationStatus — UPDATE activity_logs SET status='completed'
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("completed", "", "ws-source", "deleg-9").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 2. existing delegate_result INSERT (caller-side dashboard view)
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(
			"ws-source", "ws-source",
			sqlmock.AnyArg(), // summary
			sqlmock.AnyArg(), // response_body
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 3. NEW: PR-2 a2a_receive row for inbox-poller
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(
			"ws-source", "ws-source",
			"Delegation result delivered",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"ok",
			"",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-source"},
		{Key: "delegation_id", Value: "deleg-9"},
	}
	body := `{"status":"completed","response_preview":"all done"}`
	c.Request = httptest.NewRequest("POST",
		"/workspaces/ws-source/delegations/deleg-9/update",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.UpdateStatus(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestUpdateStatus_FlagOn_PushesA2AReceiveOnFailed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	// 1. updateDelegationStatus — UPDATE activity_logs
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("failed", "boom", "ws-source", "deleg-10").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 2. NEW: PR-2 a2a_receive row for inbox-poller (failure path doesn't
	// have the existing delegate_result INSERT — only the new push).
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(
			"ws-source", "ws-source",
			"Delegation failed",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"error",
			"boom",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-source"},
		{Key: "delegation_id", Value: "deleg-10"},
	}
	body := `{"status":"failed","error":"boom"}`
	c.Request = httptest.NewRequest("POST",
		"/workspaces/ws-source/delegations/deleg-10/update",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.UpdateStatus(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestUpdateStatus_FlagOff_NoNewSQL — sanity check that the existing
// behavior is preserved when the flag is off. Critical for safe rollout.
func TestUpdateStatus_FlagOff_NoNewSQL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	// explicitly empty — flag off
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "")

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	// Only the two pre-existing queries — no third (a2a_receive) INSERT.
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("completed", "", "ws-source", "deleg-11").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(
			"ws-source", "ws-source",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-source"},
		{Key: "delegation_id", Value: "deleg-11"},
	}
	c.Request = httptest.NewRequest("POST",
		"/workspaces/ws-source/delegations/deleg-11/update",
		bytes.NewBufferString(`{"status":"completed","response_preview":"ok"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.UpdateStatus(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("flag-off must not fire extra SQL: %v", err)
	}
}
