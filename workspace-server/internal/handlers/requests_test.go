package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// requestColumnNames mirrors the requestColumns SELECT projection order so the
// sqlmock rows line up with scanRequest.
var requestColumnNames = []string{
	"id", "kind", "requester_type", "requester_id", "org_id",
	"recipient_type", "recipient_id", "title", "detail", "status",
	"responder_type", "responder_id", "priority", "created_at", "updated_at", "responded_at",
}

// oneRequestRow builds a single-row result set for a Get/List SELECT.
func oneRequestRow(id, kind, requesterID, recipientType, recipientID, status string) *sqlmock.Rows {
	return sqlmock.NewRows(requestColumnNames).AddRow(
		id, kind, "agent", requesterID, nil,
		recipientType, recipientID, "Some title", nil, status,
		nil, nil, nil, "2026-06-10T00:00:00Z", "2026-06-10T00:00:00Z", nil,
	)
}

// ---------- Create ----------

func TestRequests_Create_Task(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Create handler first resolves the org root (org_scope CTE).
	mock.ExpectQuery("WITH RECURSIVE org_chain").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow("org-root-1"))
	// INSERT request → id
	mock.ExpectQuery("INSERT INTO requests").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("req-1"))
	// REQUEST_CREATED broadcast (recipient is a user here → anchors on requester agent)
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	body := `{"kind":"task","recipient_type":"user","recipient_id":"","title":"Review the launch draft","detail":"posts/launch.md"}`
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["request_id"] != "req-1" {
		t.Errorf("expected request_id req-1, got %v", resp["request_id"])
	}
	if resp["status"] != "pending" {
		t.Errorf("expected status pending, got %v", resp["status"])
	}
}

func TestRequests_Create_Approval_AgentRecipient(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("WITH RECURSIVE org_chain").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow("org-root-1"))
	mock.ExpectQuery("INSERT INTO requests").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("req-2"))
	// Recipient is an agent → REQUEST_CREATED anchors on the recipient workspace.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	body := `{"kind":"approval","recipient_type":"agent","recipient_id":"ws-2","title":"Approve deploy"}`
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_Create_MissingTitle(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"kind":"task","recipient_type":"user"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing title, got %d", w.Code)
	}
}

func TestRequests_Create_InvalidKind(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// org root resolves, then Create rejects the kind before any INSERT.
	mock.ExpectQuery("WITH RECURSIVE org_chain").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow("org-root-1"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"kind":"banana","recipient_type":"user","title":"x"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid kind, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Inbox vs Outgoing listing ----------

func TestRequests_ListInbox(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests").
		WithArgs("agent", "ws-2").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-2"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.ListInbox(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 || resp[0]["id"] != "req-1" {
		t.Errorf("expected one inbox request req-1, got %v", resp)
	}
}

func TestRequests_ListInbox_WithStatusFilter(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests").
		WithArgs("agent", "ws-2", "pending").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-2"}}
	c.Request = httptest.NewRequest("GET", "/?status=pending", nil)

	handler.ListInbox(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_ListOutgoing(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests").
		WithArgs("agent", "ws-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "user", "", "done"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.ListOutgoing(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 || resp[0]["status"] != "done" {
		t.Errorf("expected one outgoing request status done, got %v", resp)
	}
}

// ---------- Get (with thread) ----------

func TestRequests_Get_WithThread(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "user", "", "info_requested"))
	mock.ExpectQuery("FROM request_messages WHERE request_id").
		WithArgs("req-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "request_id", "author_type", "author_id", "body", "created_at"}).
			AddRow("msg-1", "req-1", "user", "u-1", "what file?", "2026-06-10T01:00:00Z"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["request"] == nil || resp["messages"] == nil {
		t.Errorf("expected request + messages keys, got %v", resp)
	}
}

func TestRequests_Get_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("nope").
		WillReturnRows(sqlmock.NewRows(requestColumnNames))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "nope"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.Get(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// TestRequests_Get_AgentPath_Requester_200 verifies that the requester workspace
// can read its own request on the workspace-token auth path.
func TestRequests_Get_AgentPath_Requester_200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))
	mock.ExpectQuery("FROM request_messages WHERE request_id").
		WithArgs("req-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "request_id", "author_type", "author_id", "body", "created_at"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "requestId", Value: "req-1"},
		{Key: "id", Value: "ws-1"},
	}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for requester read, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRequests_Get_AgentPath_Recipient_200 verifies that the recipient workspace
// can read a request addressed to it on the workspace-token auth path.
func TestRequests_Get_AgentPath_Recipient_200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-1", "agent", "ws-2", "pending"))
	mock.ExpectQuery("FROM request_messages WHERE request_id").
		WithArgs("req-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "request_id", "author_type", "author_id", "body", "created_at"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "requestId", Value: "req-1"},
		{Key: "id", Value: "ws-2"},
	}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for recipient read, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRequests_Get_AgentPath_NonParticipant_403 verifies that a workspace that
// is neither the requester nor the recipient gets 403 on the workspace-token
// auth path (core#2542 full fix — read-path org-scoping).
func TestRequests_Get_AgentPath_NonParticipant_403(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "requestId", Value: "req-1"},
		{Key: "id", Value: "ws-3"},
	}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.Get(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-participant read, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Respond (valid + invalid action-for-kind) ----------

func TestRequests_Respond_ApprovalApproved(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Respond does Get first to validate action↔kind.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-1", "user", "", "pending"))
	mock.ExpectExec("UPDATE requests").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// REQUEST_RESPONDED broadcast (anchored on requester agent ws-1).
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"approved","responder_id":"u-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "approved" {
		t.Errorf("expected status approved, got %v", resp["status"])
	}
}

func TestRequests_Respond_TaskDone(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "user", "", "pending"))
	mock.ExpectExec("UPDATE requests").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"done"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_Respond_InvalidActionForKind(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// kind=approval cannot be "done" — Get succeeds, actionToStatus rejects.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-1", "user", "", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"done"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for done-on-approval, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_Respond_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("nope").
		WillReturnRows(sqlmock.NewRows(requestColumnNames))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "nope"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"done"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// TestRequests_Respond_SelfResponse_400 pins RC 10416: the requester must not
// be able to approve/reject/done their own request.
func TestRequests_Respond_SelfResponse_400(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Requester is agent ws-1; responder is also agent ws-1 → self-response.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-1", "user", "", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"approved","responder_type":"agent","responder_id":"ws-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for self-response, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRequests_Respond_AgentPath_BindsWorkspace verifies REAL participant-binding:
// on the workspace-token auth path the responder is forced to the URL workspace,
// ignoring any impersonation attempt in the body.
func TestRequests_Respond_AgentPath_BindsWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Requester is agent ws-1; body claims self-response (ws-1), but the URL
	// workspace is ws-2. Binding overrides the body, so responder = ws-2 and
	// the call succeeds (not self-response).
	// Authz Get in handler, then store Get inside Respond.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-1", "agent", "ws-2", "pending"))
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-1", "agent", "ws-2", "pending"))
	mock.ExpectExec("UPDATE requests SET status").
		WithArgs("approved", "agent", "ws-2", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "requestId", Value: "req-1"},
		{Key: "id", Value: "ws-2"},
	}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"approved","responder_type":"agent","responder_id":"ws-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when bound responder differs from requester, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRequests_Respond_AgentPath_NotRecipient_403 verifies that an agent
// cannot respond to a request that was not addressed to it (core#2542).
func TestRequests_Respond_AgentPath_NotRecipient_403(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Requester is ws-1, recipient is ws-2; URL workspace is ws-1 (not recipient).
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-1", "agent", "ws-2", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "requestId", Value: "req-1"},
		{Key: "id", Value: "ws-1"},
	}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"approved","responder_type":"agent","responder_id":"ws-2"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-recipient respond, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRequests_Respond_AgentPath_SelfResponse_400 verifies that the
// self-response guard still fires on the agent path when the request is
// self-addressed (requester == recipient) and the caller tries to respond.
func TestRequests_Respond_AgentPath_SelfResponse_400(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Self-addressed request: requester = recipient = ws-1.
	// Authz Get in handler, then store Get inside Respond.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-1", "agent", "ws-1", "pending"))
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-1", "agent", "ws-1", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "requestId", Value: "req-1"},
		{Key: "id", Value: "ws-1"},
	}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"approved","responder_type":"agent","responder_id":"ws-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bound self-response, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- AddMessage → info_requested when recipient asks ----------

func TestRequests_AddMessage_RecipientFlipsInfoRequested(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Author is the recipient agent ws-2 → flips to info_requested.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))
	mock.ExpectQuery("INSERT INTO request_messages").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("msg-1"))
	// recipient-authored → status flip UPDATE
	mock.ExpectExec("UPDATE requests SET status = 'info_requested'").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// REQUEST_MESSAGE broadcast (anchored on requester ws-1)
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"body":"which file?","author_type":"agent","author_id":"ws-2"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.AddMessage(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["message_id"] != "msg-1" {
		t.Errorf("expected message_id msg-1, got %v", resp["message_id"])
	}
}

func TestRequests_AddMessage_RequesterDoesNotFlip(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Author is the requester ws-1 (not recipient) → NO info_requested flip.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "info_requested"))
	mock.ExpectQuery("INSERT INTO request_messages").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("msg-2"))
	// No status-flip UPDATE expected here.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"body":"posts/launch.md","author_type":"agent","author_id":"ws-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.AddMessage(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- AddMessage workspace-token authz (core#2542 / core#2606) ----------

func TestRequests_AddMessage_AgentPath_Recipient_BindsToCaller(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// URL workspace ws-2 is the recipient. The body tries to spoof ws-EVIL.
	// The handler's authz Get AND the store's own Get both fetch the row.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))
	// Handler must bind author to ws-2, not the spoofed body value.
	mock.ExpectQuery("INSERT INTO request_messages").
		WithArgs("req-1", "agent", "ws-2", "which file?").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("msg-1"))
	mock.ExpectExec("UPDATE requests SET status = 'info_requested'").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "requestId", Value: "req-1"},
		{Key: "id", Value: "ws-2"},
	}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"body":"which file?","author_type":"agent","author_id":"ws-EVIL"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.AddMessage(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_AddMessage_AgentPath_Requester_BindsToCaller(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// URL workspace ws-1 is the requester. Body author_id is ignored.
	// Handler authz Get + store Get → two fetches.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "info_requested"))
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "info_requested"))
	mock.ExpectQuery("INSERT INTO request_messages").
		WithArgs("req-1", "agent", "ws-1", "here is the file").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("msg-2"))
	// No status flip — requester is not the recipient.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "requestId", Value: "req-1"},
		{Key: "id", Value: "ws-1"},
	}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"body":"here is the file","author_type":"agent","author_id":"ws-EVIL"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.AddMessage(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_AddMessage_AgentPath_NonParticipant_403(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// URL workspace ws-3 is neither requester nor recipient.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "requestId", Value: "req-1"},
		{Key: "id", Value: "ws-3"},
	}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"body":"pwned","author_type":"agent","author_id":"ws-2"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.AddMessage(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-participant, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Cancel ----------

func TestRequests_Cancel_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectExec("UPDATE requests SET status = 'cancelled'").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", nil)

	handler.Cancel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRequests_Cancel_AgentPath_NotRequester_403 verifies that an agent
// cannot cancel a request it did not raise (core#2542).
func TestRequests_Cancel_AgentPath_NotRequester_403(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Requester is ws-1; URL workspace is ws-2 (not requester).
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "requestId", Value: "req-1"},
		{Key: "id", Value: "ws-2"},
	}
	c.Request = httptest.NewRequest("POST", "/", nil)

	handler.Cancel(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-requester cancel, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_Cancel_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectExec("UPDATE requests SET status = 'cancelled'").
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "gone"}}
	c.Request = httptest.NewRequest("POST", "/", nil)

	handler.Cancel(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ---------- ListPending (org view) + kind filter ----------

// pendingColumnNames mirrors ListPendingForOrg's projection (requestColumns + w.name).
var pendingColumnNames = append(append([]string{}, requestColumnNames...), "workspace_name")

func onePendingRow(id, kind string) *sqlmock.Rows {
	return sqlmock.NewRows(pendingColumnNames).AddRow(
		id, kind, "agent", "ws-1", "org-root-1",
		"user", "", "Some title", nil, "pending",
		nil, nil, nil, "2026-06-10T00:00:00Z", "2026-06-10T00:00:00Z", nil,
		"Marketing Agent",
	)
}

func TestRequests_ListPending_NoFilter(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests r").
		WillReturnRows(onePendingRow("req-1", "task").AddRow(
			"req-2", "approval", "agent", "ws-2", "org-root-1",
			"user", "", "Approve deploy", nil, "pending",
			nil, nil, nil, "2026-06-10T00:00:00Z", "2026-06-10T00:00:00Z", nil,
			"Ops Agent",
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.ListPending(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 2 {
		t.Errorf("expected 2 pending, got %d (%v)", len(resp), resp)
	}
	if resp[0]["workspace_name"] != "Marketing Agent" {
		t.Errorf("expected decorated workspace_name, got %v", resp[0]["workspace_name"])
	}
}

func TestRequests_ListPending_KindFilter(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// ?kind=approval must add an arg.
	mock.ExpectQuery("FROM requests r").
		WithArgs("approval").
		WillReturnRows(onePendingRow("req-2", "approval"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/?kind=approval", nil)

	handler.ListPending(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_ListPending_InvalidKind(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/?kind=banana", nil)

	handler.ListPending(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid kind, got %d", w.Code)
	}
}

// ---------- Requester notification on respond / more-info (CTO 2026-06-11) ----------

// interceptRequestNotify swaps the package-level enqueue for the test and
// returns a capture slice + restore func.
func interceptRequestNotify(t *testing.T) *[]map[string]string {
	t.Helper()
	captured := &[]map[string]string{}
	prev := requestNotifyEnqueue
	requestNotifyEnqueue = func(ctx context.Context, workspaceID, callerID string, priority int, body []byte, method, idemKey string, expiresAt *time.Time) (string, int, error) {
		*captured = append(*captured, map[string]string{
			"workspace_id": workspaceID,
			"method":       method,
			"idem":         idemKey,
			"body":         string(body),
		})
		return "q-1", 1, nil
	}
	t.Cleanup(func() { requestNotifyEnqueue = prev })
	return captured
}

// TestRequests_Respond_NotifiesRequesterAgent: a terminal response on a
// request raised by an AGENT must enqueue a message/send turn to that agent
// — the user clicking Done/Approve must actually reach the requester.
func TestRequests_Respond_NotifiesRequesterAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())
	captured := interceptRequestNotify(t)

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-agent-1", "user", "", "pending"))
	mock.ExpectExec("UPDATE requests").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"approved","responder_id":"u-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 requester notification, got %d", len(*captured))
	}
	n := (*captured)[0]
	if n["workspace_id"] != "ws-agent-1" || n["method"] != "message/send" {
		t.Errorf("notification misrouted: %+v", n)
	}
	if n["idem"] != "request-responded:req-1" {
		t.Errorf("idempotency key = %q", n["idem"])
	}
	if !strings.Contains(n["body"], "approved") || !strings.Contains(n["body"], "Some title") {
		t.Errorf("notification body missing outcome/title: %s", n["body"])
	}
}

// TestRequests_AddMessage_MoreInfo_NotifiesRequesterAgent: a recipient-authored
// More-Info message must reach the requester agent as a turn carrying the ask.
func TestRequests_AddMessage_MoreInfo_NotifiesRequesterAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())
	captured := interceptRequestNotify(t)

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-2").
		WillReturnRows(oneRequestRow("req-2", "task", "ws-agent-1", "user", "u-1", "pending"))
	mock.ExpectQuery("INSERT INTO request_messages").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("msg-9"))
	mock.ExpectExec("UPDATE requests SET status = 'info_requested'").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-2"}}
	c.Request = httptest.NewRequest("POST", "/", bytesNewBufferStringHelper(`{"author_type":"user","author_id":"u-1","body":"which environment do you mean?"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.AddMessage(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 requester notification, got %d", len(*captured))
	}
	n := (*captured)[0]
	if n["workspace_id"] != "ws-agent-1" || n["idem"] != "request-message:msg-9" {
		t.Errorf("notification misrouted: %+v", n)
	}
	if !strings.Contains(n["body"], "which environment do you mean?") {
		t.Errorf("notification body missing ask: %s", n["body"])
	}
}

// TestRequests_AddMessage_MoreInfo_GenericUserRecipient_NotifiesRequesterAgent
// is the REAL production path: an agent→user request is stored with an EMPTY
// recipient_id (the generic "the user"), but the canvas posts the More-Info
// reply with a CONCRETE author_id (the session user_id, or the "admin"
// placeholder when no session). The old gate (authorID == RecipientID) was
// "admin" == "" → false, so neither the info_requested flip NOR the requester
// notification ever fired — the agent silently never received the thread.
// The fix keys off "not the requester", so this must now flip + notify.
func TestRequests_AddMessage_MoreInfo_GenericUserRecipient_NotifiesRequesterAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())
	captured := interceptRequestNotify(t)

	// recipient_id is EMPTY (generic user); author_id is the "admin" placeholder.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-7").
		WillReturnRows(oneRequestRow("req-7", "approval", "ws-agent-1", "user", "", "pending"))
	mock.ExpectQuery("INSERT INTO request_messages").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("msg-7"))
	// Must STILL flip to info_requested even though author_id != recipient_id.
	mock.ExpectExec("UPDATE requests SET status = 'info_requested'").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-7"}}
	c.Request = httptest.NewRequest("POST", "/", bytesNewBufferStringHelper(`{"author_type":"user","author_id":"admin","body":"which secret did you mean?"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.AddMessage(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 requester notification (generic-user recipient), got %d", len(*captured))
	}
	n := (*captured)[0]
	if n["workspace_id"] != "ws-agent-1" || n["idem"] != "request-message:msg-7" {
		t.Errorf("notification misrouted: %+v", n)
	}
	if !strings.Contains(n["body"], "which secret did you mean?") {
		t.Errorf("notification body missing ask: %s", n["body"])
	}
}

// TestRequests_AddMessage_RequesterReply_NoFlipNoNotify: when the REQUESTER
// agent itself appends a message (replying to the clarification), it must NOT
// flip to info_requested and must NOT notify itself.
func TestRequests_AddMessage_RequesterReply_NoFlipNoNotify(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())
	captured := interceptRequestNotify(t)

	// Requester is agent ws-agent-1; the author IS that requester.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-8").
		WillReturnRows(oneRequestRow("req-8", "task", "ws-agent-1", "user", "", "info_requested"))
	mock.ExpectQuery("INSERT INTO request_messages").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("msg-8"))
	// No status-flip UPDATE expected (requester-authored).
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-8"}}
	c.Request = httptest.NewRequest("POST", "/", bytesNewBufferStringHelper(`{"author_type":"agent","author_id":"ws-agent-1","body":"I meant the staging DB secret."}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.AddMessage(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if len(*captured) != 0 {
		t.Fatalf("requester-authored message must NOT self-notify; got %d", len(*captured))
	}
}

// TestRequests_Respond_NoNotifyForUserRequester: a request raised by a USER
// must not enqueue an agent notification on respond.
func TestRequests_Respond_NoNotifyForUserRequester(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())
	captured := interceptRequestNotify(t)

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-3").
		WillReturnRows(sqlmock.NewRows(requestColumnNames).AddRow(
			"req-3", "task", "user", "u-1", nil,
			"agent", "ws-agent-2", "Some title", nil, "pending",
			nil, nil, nil, "2026-06-10T00:00:00Z", "2026-06-10T00:00:00Z", nil,
		))
	mock.ExpectExec("UPDATE requests").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-3"}}
	c.Request = httptest.NewRequest("POST", "/", bytesNewBufferStringHelper(`{"action":"done","responder_type":"agent","responder_id":"ws-agent-2"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(*captured) != 0 {
		t.Fatalf("expected no notification for user requester, got %d", len(*captured))
	}
}

// bytesNewBufferStringHelper keeps the new tests free of an extra import
// alias; identical to bytes.NewBufferString.
func bytesNewBufferStringHelper(s string) *bytes.Buffer { return bytes.NewBufferString(s) }
