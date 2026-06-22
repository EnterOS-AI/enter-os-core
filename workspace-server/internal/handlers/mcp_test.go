package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"errors"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/memory/contract"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// newMCPHandler is a test helper that constructs an MCPHandler backed by the
// sqlmock DB set up by setupTestDB. Uses newTestBroadcaster so handlers
// that BroadcastOnly (send_message_to_user, etc.) don't nil-panic on the
// hub — events.NewBroadcaster(nil) crashes inside hub.Broadcast.
func newMCPHandler(t *testing.T) (*MCPHandler, sqlmock.Sqlmock) {
	t.Helper()
	mock := setupTestDB(t)
	h := NewMCPHandler(db.DB, newTestBroadcaster())
	return h, mock
}

// errNotFound is sql.ErrNoRows, used to simulate missing-row DB errors.
var errNotFound = sql.ErrNoRows

// contextForTest returns a cancellable context pre-cancelled so that
// streaming handlers (Stream) return immediately in tests.
func contextForTest() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return ctx, cancel
}

// mcpPost builds a POST /workspaces/:id/mcp request with the given JSON body.
func mcpPost(t *testing.T, h *MCPHandler, workspaceID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBuffer(b))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Call(c)
	return w
}

// assertA2ASendMessageSchema validates that body is a schema-valid A2A
// SendMessageRequest with role="user", messageId, and non-empty parts.
// Issue #2251 contract test: delegate_task must always produce this shape.
func assertA2ASendMessageSchema(t *testing.T, body []byte, wantTask string) {
	t.Helper()
	var envelope map[string]interface{}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("A2A body is not valid JSON: %v", err)
	}
	if envelope["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", envelope["jsonrpc"])
	}
	if envelope["method"] != "message/send" {
		t.Errorf("method = %v, want message/send", envelope["method"])
	}

	params, ok := envelope["params"].(map[string]interface{})
	if !ok {
		t.Fatalf("params missing or not a map: %T", envelope["params"])
	}
	msg, ok := params["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("message missing or not a map: %T", params["message"])
	}

	if msg["role"] != "user" {
		t.Errorf("message.role = %v, want \"user\"", msg["role"])
	}
	if msg["messageId"] == "" {
		t.Error("message.messageId is empty")
	}

	parts, ok := msg["parts"].([]interface{})
	if !ok || len(parts) == 0 {
		t.Fatalf("message.parts missing or empty: %T", msg["parts"])
	}
	firstPart, ok := parts[0].(map[string]interface{})
	if !ok {
		t.Fatalf("first part is not a map: %T", parts[0])
	}
	// A2A v0.3 Part discriminator is `kind`, NOT `type` (#2251)
	if firstPart["kind"] != "text" {
		t.Errorf("first part kind = %v, want text", firstPart["kind"])
	}
	if firstPart["text"] != wantTask {
		t.Errorf("first part text = %v, want %q", firstPart["text"], wantTask)
	}
}

func expectCanCommunicateSiblings(mock sqlmock.Sqlmock, callerID, targetID, parentID string) {
	mock.ExpectQuery(`SELECT id, parent_id FROM workspaces WHERE id = \$1`).
		WithArgs(callerID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow(callerID, parentID))
	mock.ExpectQuery(`SELECT id, parent_id FROM workspaces WHERE id = \$1`).
		WithArgs(targetID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow(targetID, parentID))
}

// ─────────────────────────────────────────────────────────────────────────────
// initialize
// ─────────────────────────────────────────────────────────────────────────────

func TestMCPHandler_Initialize_ReturnsCapabilities(t *testing.T) {
	h, _ := newMCPHandler(t)

	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]interface{}{},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}
	if result["protocolVersion"] != mcpProtocolVersion {
		t.Errorf("protocolVersion: got %v, want %s", result["protocolVersion"], mcpProtocolVersion)
	}
	caps, _ := result["capabilities"].(map[string]interface{})
	if _, ok := caps["tools"]; !ok {
		t.Error("capabilities.tools missing")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// tools/list
// ─────────────────────────────────────────────────────────────────────────────

func TestMCPHandler_ToolsList_ExcludesSendMessageByDefault(t *testing.T) {
	_ = os.Unsetenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE")
	h, _ := newMCPHandler(t)

	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp mcpResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	result, _ := resp.Result.(map[string]interface{})
	toolsRaw, _ := result["tools"].([]interface{})

	for _, ti := range toolsRaw {
		tool, _ := ti.(map[string]interface{})
		if tool["name"] == "send_message_to_user" {
			t.Error("send_message_to_user should be excluded when MOLECULE_MCP_ALLOW_SEND_MESSAGE is unset")
		}
	}
	if len(toolsRaw) == 0 {
		t.Error("tool list should not be empty")
	}
}

func TestMCPHandler_ToolsList_IncludesSendMessageWhenEnvSet(t *testing.T) {
	t.Setenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE", "true")
	h, _ := newMCPHandler(t)

	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/list",
	})

	var resp mcpResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	result, _ := resp.Result.(map[string]interface{})
	toolsRaw, _ := result["tools"].([]interface{})

	found := false
	for _, ti := range toolsRaw {
		tool, _ := ti.(map[string]interface{})
		if tool["name"] == "send_message_to_user" {
			found = true
		}
	}
	if !found {
		t.Error("send_message_to_user should be included when MOLECULE_MCP_ALLOW_SEND_MESSAGE=true")
	}
}

func TestMCPHandler_ToolsList_ContainsExpectedTools(t *testing.T) {
	_ = os.Unsetenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE")
	h, _ := newMCPHandler(t)

	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/list",
	})

	var resp mcpResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	result, _ := resp.Result.(map[string]interface{})
	toolsRaw, _ := result["tools"].([]interface{})

	names := make(map[string]bool)
	for _, ti := range toolsRaw {
		tool, _ := ti.(map[string]interface{})
		names[tool["name"].(string)] = true
	}
	required := []string{"list_peers", "get_workspace_info", "delegate_task", "delegate_task_async", "check_task_status", "commit_memory", "recall_memory"}
	for _, name := range required {
		if !names[name] {
			t.Errorf("tool %q missing from tools/list", name)
		}
	}
}

func TestMCPHandler_DelegateTask_RoutesThroughPlatformA2AProxy(t *testing.T) {
	h, mock := newMCPHandler(t)
	callerID := "11111111-1111-1111-1111-111111111111"
	targetID := "22222222-2222-2222-2222-222222222222"
	parentID := "33333333-3333-3333-3333-333333333333"

	expectCanCommunicateSiblings(mock, callerID, targetID, parentID)
	mock.ExpectExec(`(?s)INSERT INTO activity_logs.*'delegation'.*'delegate'`).
		WithArgs(callerID, callerID, targetID, "Delegating to "+targetID, sqlmock.AnyArg(), "pending").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("dispatched", "", callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	var gotTarget, gotCaller string
	h.a2aProxy = func(ctx context.Context, workspaceID string, body []byte, callerID string, logActivity bool) (int, []byte, error) {
		gotTarget = workspaceID
		gotCaller = callerID
		if !logActivity {
			t.Fatal("delegate_task should log through platform A2A proxy")
		}
		assertA2ASendMessageSchema(t, body, "do work")
		return 200, []byte(`{"result":{"message":{"parts":[{"text":"done"}]}}}`), nil
	}

	out, err := h.toolDelegateTask(context.Background(), callerID, map[string]interface{}{
		"workspace_id": targetID,
		"task":         "do work",
	}, mcpCallTimeout)
	if err != nil {
		t.Fatalf("delegate_task returned error: %v", err)
	}
	if out != "done" {
		t.Fatalf("delegate_task response = %q, want done", out)
	}
	if gotTarget != targetID || gotCaller != callerID {
		t.Fatalf("proxy called with target=%q caller=%q, want target=%q caller=%q", gotTarget, gotCaller, targetID, callerID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestMCPHandler_DelegateTaskAsync_RoutesThroughPlatformA2AProxy(t *testing.T) {
	h, mock := newMCPHandler(t)
	callerID := "11111111-1111-1111-1111-111111111111"
	targetID := "22222222-2222-2222-2222-222222222222"
	parentID := "33333333-3333-3333-3333-333333333333"

	expectCanCommunicateSiblings(mock, callerID, targetID, parentID)
	mock.ExpectExec(`(?s)INSERT INTO activity_logs.*'delegation'.*'delegate'`).
		WithArgs(callerID, callerID, targetID, "Delegating to "+targetID, sqlmock.AnyArg(), "pending").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("queued", "", callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("delivered", "", callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	called := make(chan struct{}, 1)
	h.a2aProxy = func(ctx context.Context, workspaceID string, body []byte, proxyCallerID string, logActivity bool) (int, []byte, error) {
		if workspaceID != targetID || proxyCallerID != callerID {
			t.Fatalf("unexpected proxy route target=%q caller=%q", workspaceID, proxyCallerID)
		}
		assertA2ASendMessageSchema(t, body, "async work")
		called <- struct{}{}
		return 200, []byte(`{"result":{"message":{"parts":[{"text":"accepted"}]}}}`), nil
	}

	out, err := h.toolDelegateTaskAsync(context.Background(), callerID, map[string]interface{}{
		"workspace_id": targetID,
		"task":         "async work",
	})
	if err != nil {
		t.Fatalf("delegate_task_async returned error: %v", err)
	}
	if !strings.Contains(out, `"status":"queued"`) {
		t.Fatalf("delegate_task_async response = %s", out)
	}
	waitGlobalAsyncForTest()
	select {
	case <-called:
	default:
		t.Fatal("async delegate did not call platform A2A proxy")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestMCPHandler_DelegateTaskAsync_MarshalFailureDoesNotCallProxy proves the
// extracted #1933 fix: when the A2A body fails to marshal, the detached
// goroutine returns early and never calls proxyA2ARequest with a nil/empty
// body. Before the fix the goroutine logged the error and fell through,
// dispatching a malformed A2A request.

func TestMCPHandler_DelegateTask_WithAttachments(t *testing.T) {
	h, mock := newMCPHandler(t)
	callerID := "11111111-1111-1111-1111-111111111111"
	targetID := "22222222-2222-2222-2222-222222222222"
	parentID := "33333333-3333-3333-3333-333333333333"

	expectCanCommunicateSiblings(mock, callerID, targetID, parentID)
	mock.ExpectExec(`(?s)INSERT INTO activity_logs.*'delegation'.*'delegate'`).
		WithArgs(callerID, callerID, targetID, "Delegating to "+targetID, sqlmock.AnyArg(), "pending").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("dispatched", "", callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.a2aProxy = func(ctx context.Context, workspaceID string, body []byte, proxyCallerID string, logActivity bool) (int, []byte, error) {
		if workspaceID != targetID || proxyCallerID != callerID {
			t.Fatalf("unexpected proxy route target=%q caller=%q", workspaceID, proxyCallerID)
		}
		assertA2ASendMessageSchema(t, body, "review this video")
		bodyStr := string(body)
		if !strings.Contains(bodyStr, `"kind":"video"`) {
			t.Fatalf("A2A body missing video attachment kind: %s", bodyStr)
		}
		if !strings.Contains(bodyStr, `"uri":"workspace:/tmp/clip.mp4"`) {
			t.Fatalf("A2A body missing attachment uri: %s", bodyStr)
		}
		if !strings.Contains(bodyStr, `"mime_type":"video/mp4"`) {
			t.Fatalf("A2A body missing attachment mime_type: %s", bodyStr)
		}
		return 200, []byte(`{"result":{"message":{"parts":[{"text":"done"}]}}}`), nil
	}

	out, err := h.toolDelegateTask(context.Background(), callerID, map[string]interface{}{
		"workspace_id": targetID,
		"task":         "review this video",
		"attachments": []interface{}{
			map[string]interface{}{
				"uri":      "workspace:/tmp/clip.mp4",
				"name":     "clip.mp4",
				"mimeType": "video/mp4",
				"size":     12345,
			},
		},
	}, mcpCallTimeout)
	if err != nil {
		t.Fatalf("delegate_task returned error: %v", err)
	}
	if out != "done" {
		t.Fatalf("delegate_task response = %q, want done", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestMCPHandler_DelegateTaskAsync_WithAttachments(t *testing.T) {
	h, mock := newMCPHandler(t)
	callerID := "11111111-1111-1111-1111-111111111111"
	targetID := "22222222-2222-2222-2222-222222222222"
	parentID := "33333333-3333-3333-3333-333333333333"

	expectCanCommunicateSiblings(mock, callerID, targetID, parentID)
	mock.ExpectExec(`(?s)INSERT INTO activity_logs.*'delegation'.*'delegate'`).
		WithArgs(callerID, callerID, targetID, "Delegating to "+targetID, sqlmock.AnyArg(), "pending").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("queued", "", callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("delivered", "", callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	called := make(chan []byte, 1)
	h.a2aProxy = func(ctx context.Context, workspaceID string, body []byte, proxyCallerID string, logActivity bool) (int, []byte, error) {
		if workspaceID != targetID || proxyCallerID != callerID {
			t.Fatalf("unexpected proxy route target=%q caller=%q", workspaceID, proxyCallerID)
		}
		called <- body
		return 200, []byte(`{"result":{"message":{"parts":[{"text":"accepted"}]}}}`), nil
	}

	out, err := h.toolDelegateTaskAsync(context.Background(), callerID, map[string]interface{}{
		"workspace_id": targetID,
		"task":         "async work with image",
		"attachments": []interface{}{
			map[string]interface{}{
				"uri":      "workspace:/tmp/screenshot.png",
				"name":     "screenshot.png",
				"mimeType": "image/png",
			},
		},
	})
	if err != nil {
		t.Fatalf("delegate_task_async returned error: %v", err)
	}
	if !strings.Contains(out, `"status":"queued"`) {
		t.Fatalf("delegate_task_async response = %s", out)
	}
	waitGlobalAsyncForTest()
	select {
	case body := <-called:
		assertA2ASendMessageSchema(t, body, "async work with image")
		bodyStr := string(body)
		if !strings.Contains(bodyStr, `"kind":"image"`) {
			t.Fatalf("A2A body missing image attachment kind: %s", bodyStr)
		}
		if !strings.Contains(bodyStr, `"uri":"workspace:/tmp/screenshot.png"`) {
			t.Fatalf("A2A body missing attachment uri: %s", bodyStr)
		}
	default:
		t.Fatal("async delegate did not call platform A2A proxy")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
func TestMCPHandler_DelegateTaskAsync_MarshalFailureDoesNotCallProxy(t *testing.T) {
	h, mock := newMCPHandler(t)
	callerID := "11111111-1111-1111-1111-111111111111"
	targetID := "22222222-2222-2222-2222-222222222222"
	parentID := "33333333-3333-3333-3333-333333333333"

	expectCanCommunicateSiblings(mock, callerID, targetID, parentID)
	mock.ExpectExec(`(?s)INSERT INTO activity_logs.*'delegation'.*'delegate'`).
		WithArgs(callerID, callerID, targetID, "Delegating to "+targetID, sqlmock.AnyArg(), "pending").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("queued", "", callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("failed", sqlmock.AnyArg(), callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Force the (otherwise near-impossible) marshal failure for the A2A body.
	origMarshal := marshalA2ABody
	marshalA2ABody = func(any) ([]byte, error) {
		return nil, errors.New("forced marshal failure")
	}
	t.Cleanup(func() { marshalA2ABody = origMarshal })

	proxyCalled := make(chan struct{}, 1)
	h.a2aProxy = func(ctx context.Context, workspaceID string, body []byte, proxyCallerID string, logActivity bool) (int, []byte, error) {
		proxyCalled <- struct{}{}
		return 200, []byte(`{}`), nil
	}

	out, err := h.toolDelegateTaskAsync(context.Background(), callerID, map[string]interface{}{
		"workspace_id": targetID,
		"task":         "async work",
	})
	if err != nil {
		t.Fatalf("delegate_task_async returned error: %v", err)
	}
	if !strings.Contains(out, `"status":"queued"`) {
		t.Fatalf("delegate_task_async response = %s", out)
	}

	// Wait for the detached goroutine to finish, then assert the proxy was
	// never reached because of the early return on marshal failure.
	waitGlobalAsyncForTest()
	select {
	case <-proxyCalled:
		t.Fatal("proxyA2ARequest was called after marshal failure; expected early return")
	default:
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestMCPHandler_CheckTaskStatus_NullStatusDefaultsToUnknown proves the
// extracted #1933 hardening: when the activity_logs row has a NULL status,
// check_task_status reports "unknown" instead of an empty string (the old
// status.String zero value).
func TestMCPHandler_CheckTaskStatus_NullStatusDefaultsToUnknown(t *testing.T) {
	h, mock := newMCPHandler(t)
	callerID := "11111111-1111-1111-1111-111111111111"
	targetID := "22222222-2222-2222-2222-222222222222"
	taskID := "task-abc"

	mock.ExpectQuery(`(?s)SELECT status, error_detail, response_body.*FROM activity_logs`).
		WithArgs(callerID, targetID, taskID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "error_detail", "response_body"}).
			AddRow(nil, nil, nil))

	out, err := h.toolCheckTaskStatus(context.Background(), callerID, map[string]interface{}{
		"workspace_id": targetID,
		"task_id":      taskID,
	})
	if err != nil {
		t.Fatalf("check_task_status returned error: %v", err)
	}
	if !strings.Contains(out, `"status": "unknown"`) {
		t.Fatalf("expected status \"unknown\" for NULL status row, got: %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// notifications/initialized
// ─────────────────────────────────────────────────────────────────────────────

func TestMCPHandler_NotificationsInitialized_Returns200(t *testing.T) {
	h, _ := newMCPHandler(t)

	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      nil,
		"method":  "notifications/initialized",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp mcpResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Errorf("unexpected error: %+v", resp.Error)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Unknown method
// ─────────────────────────────────────────────────────────────────────────────

// TestMCPHandler_UnknownMethod_Returns32601 verifies dispatchRPC returns
// -32601 for an unknown method. Per OFFSEC-001: the error message must be
// constant — req.Method is user-controlled and must NOT appear in the response.
func TestMCPHandler_UnknownMethod_Returns32601(t *testing.T) {
	h, _ := newMCPHandler(t)

	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "not/a/real/method",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with error body, got %d", w.Code)
	}
	var resp mcpResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("expected code -32601, got %d", resp.Error.Code)
	}
	// Message must be constant — no user-controlled method name leak.
	if resp.Error.Message != "method not found" {
		t.Errorf("error message should be constant 'method not found', got: %q", resp.Error.Message)
	}
	// Double-check the method name never appears in the message (defence-in-depth).
	if strings.Contains(resp.Error.Message, "not/a/real/method") {
		t.Error("error message must not echo the user-controlled method name")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// tools/call — get_workspace_info
// ─────────────────────────────────────────────────────────────────────────────

func TestMCPHandler_GetWorkspaceInfo_Success(t *testing.T) {
	h, mock := newMCPHandler(t)

	mock.ExpectQuery("SELECT id, name").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "role", "tier", "status", "parent_id"}).
			AddRow("ws-1", "Dev Lead", "developer", 2, "online", nil))

	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      6,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "get_workspace_info",
			"arguments": map[string]interface{}{},
		},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp mcpResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result, _ := resp.Result.(map[string]interface{})
	content, _ := result["content"].([]interface{})
	if len(content) == 0 {
		t.Fatal("content is empty")
	}
	item, _ := content[0].(map[string]interface{})
	text, _ := item["text"].(string)
	if text == "" {
		t.Error("tool result text is empty")
	}
	// Verify the JSON contains expected fields.
	var info map[string]interface{}
	if err := json.Unmarshal([]byte(text), &info); err != nil {
		t.Fatalf("tool result is not valid JSON: %v", err)
	}
	if info["id"] != "ws-1" {
		t.Errorf("id: got %v, want ws-1", info["id"])
	}
	if info["name"] != "Dev Lead" {
		t.Errorf("name: got %v, want Dev Lead", info["name"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestMCPHandler_GetWorkspaceInfo_NotFound(t *testing.T) {
	h, mock := newMCPHandler(t)

	mock.ExpectQuery("SELECT id, name").
		WithArgs("ws-missing").
		WillReturnError(errNotFound)

	w := mcpPost(t, h, "ws-missing", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "get_workspace_info",
			"arguments": map[string]interface{}{},
		},
	})

	var resp mcpResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error == nil {
		t.Error("expected JSON-RPC error for missing workspace")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// tools/call — list_peers
// ─────────────────────────────────────────────────────────────────────────────

func TestMCPHandler_ListPeers_ReturnsSiblings(t *testing.T) {
	h, mock := newMCPHandler(t)

	// Parent lookup
	mock.ExpectQuery("SELECT parent_id FROM workspaces").
		WithArgs("ws-child").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow("ws-parent"))

	// Siblings query
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("ws-parent", "ws-child").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "role", "status", "tier"}).
			AddRow("ws-sibling", "Research", "researcher", "online", 1))

	// Children query
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("ws-child").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "role", "status", "tier"}))

	// Parent query
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("ws-parent").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "role", "status", "tier"}).
			AddRow("ws-parent", "PM", "manager", "online", 3))

	w := mcpPost(t, h, "ws-child", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      8,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "list_peers",
			"arguments": map[string]interface{}{},
		},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp mcpResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result, _ := resp.Result.(map[string]interface{})
	content, _ := result["content"].([]interface{})
	item, _ := content[0].(map[string]interface{})
	text, _ := item["text"].(string)
	if !bytes.Contains([]byte(text), []byte("ws-sibling")) {
		t.Errorf("expected sibling ws-sibling in response, got: %s", text)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// tools/call — commit_memory
// ─────────────────────────────────────────────────────────────────────────────

// Issue #1733: the legacy SQL success-path tests for commit_memory and
// recall_memory have been removed — the v2 plugin is the only backend
// and its success paths are covered by:
//   - TestToolCommitMemory_RoutesThroughV2WhenWired (legacy-shim test)
//   - TestToolRecallMemory_RoutesThroughV2WhenWired (legacy-shim test)
//   - Every test in mcp_tools_memory_v2_test.go
// The unwired-path tests live in mcp_tools_memory_legacy_shim_test.go
// (TestToolCommitMemory_ErrorsWhenV2Unwired and its recall sibling).
//
// The two scope-blocked tests below remain because they validate the
// OFFSEC-001 JSON-RPC scrub layer (mcp.go dispatchRPC), which is
// orthogonal to the memory backend. After A1 the underlying error
// shifts from "GLOBAL scope is not permitted" to "memory plugin is
// not configured" — but the client-visible message stays "tool call
// failed", which is what the scrub assertion actually proves.

// TestMCPHandler_CommitMemory_GlobalScope_ScrubsInternalError verifies the
// OFFSEC-001 / #259 scrub contract on the commit_memory tool: the GLOBAL
// scope block at scopeToWritableNamespace produces an internal error
// containing the tokens "GLOBAL", "scope", "permitted", "bridge",
// "LOCAL", "TEAM" — every one of those MUST be scrubbed to the constant
// "tool call failed" + code -32000 before reaching the JSON-RPC wire.
//
// Issue #1747 review fixed the test setup: the handler is now wired
// with a v2 plugin + resolver stub so the request actually reaches
// the GLOBAL-block path in commitMemoryLegacyShim →
// scopeToWritableNamespace. Without that wiring, the handler errors
// earlier in `memoryV2Available()` with "memory plugin is not
// configured", and the leaked-tokens assertion below becomes
// vacuously true — passes even if the entire scrub layer in
// mcp.go:dispatchRPC is deleted. The wired path is the only one
// that actually pins the OFFSEC-001 contract.
func TestMCPHandler_CommitMemory_GlobalScope_ScrubsInternalError(t *testing.T) {
	h, mock := newMCPHandler(t)
	// Wire v2 stubs so toolCommitMemory → commitMemoryLegacyShim
	// actually runs (without v2, it short-circuits with the
	// "plugin not configured" error that doesn't contain the
	// leaked-token strings we're asserting on).
	h.withMemoryV2APIs(&stubMemoryPlugin{}, rootNamespaceResolver())

	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      10,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "commit_memory",
			"arguments": map[string]interface{}{
				"content": "secret global memory",
				"scope":   "GLOBAL",
			},
		},
	})

	// JSON-RPC envelope returns 200 with the error in the body — only
	// malformed-JSON-at-the-envelope-layer returns 400 (see Call() in mcp.go).
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (JSON-RPC error in body), got %d: %s", w.Code, w.Body.String())
	}

	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	// (1) C3: an error must be reported.
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for GLOBAL scope, got nil")
	}

	// (2) OFFSEC-001 positive assertions — exact equality on the scrubbed
	// constants so any change (re-leak of err.Error(), code mutation) trips
	// the test. Substring-match would not catch a partial re-leak.
	if resp.Error.Code != -32000 {
		t.Errorf("error code should be -32000 (Server error / dispatch-failure), got: %d", resp.Error.Code)
	}
	if resp.Error.Message != "tool call failed" {
		t.Errorf("error message should be the OFFSEC-001 constant %q, got: %q", "tool call failed", resp.Error.Message)
	}

	// (3) OFFSEC-001 negative assertions — the internal err.Error() text
	// from scopeToWritableNamespace ("GLOBAL scope is not permitted via the MCP
	// bridge — use LOCAL or TEAM") must NOT appear in the client-visible
	// message. Each token below is a distinct substring of that internal
	// string; if ANY leaks through, the scrub in mcp.go dispatchRPC has
	// regressed and this assertion fires the canary.
	leakedTokens := []string{
		"GLOBAL",    // scope name
		"scope",     // policy lexicon
		"permitted", // policy verb
		"bridge",    // internal architecture term
		"LOCAL",     // alternative scope name
		"TEAM",      // alternative scope name
	}
	for _, tok := range leakedTokens {
		if bytes.Contains([]byte(resp.Error.Message), []byte(tok)) {
			t.Errorf("OFFSEC-001 scrub regression: client-visible error.message leaks internal token %q (got: %q)", tok, resp.Error.Message)
		}
	}

	// (4) C3 invariant preserved: handler must short-circuit before any DB call.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls on GLOBAL scope block: %v", err)
	}
}

// Issue #1733: the legacy SQL-path redaction tests for commit_memory
// (SecretInContent_IsRedactedBeforeInsert, CleanContent_PassesThrough)
// have been removed. The v2 plugin path performs the same redaction
// (mcp_tools_memory_v2.go:122 + :242); its coverage lives in
// mcp_tools_memory_v2_test.go.

// TestMCPHandler_CommitMemory_LegacyName_RedactionAtPlugin verifies that
// the LEGACY MCP tool name `commit_memory` (the one most agents
// actually call — `commit_memory_v2` is the underlying handler the
// shim delegates to) still redacts secret-shaped content before the
// payload reaches the v2 plugin. The deleted SQL-path version of this
// test pinned the same contract against `agent_memories` INSERT
// arguments; #1747 review (finding N6) noted the legacy-name path
// had no direct equivalent post-A1. This test fills that gap by
// capturing the MemoryWrite the stub plugin receives.
func TestMCPHandler_CommitMemory_LegacyName_RedactionAtPlugin(t *testing.T) {
	h, _ := newMCPHandler(t)

	var captured contract.MemoryWrite
	plugin := &stubMemoryPlugin{
		commitFn: func(_ context.Context, _ string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
			captured = body
			return &contract.MemoryWriteResponse{ID: "mem-x", Namespace: "workspace:root-1"}, nil
		},
	}
	h.withMemoryV2APIs(plugin, rootNamespaceResolver())

	rawContent := "key=ANTHROPIC_API_KEY=sk-ant-xxxxxxxxxxxxxxxx auth=Bearer ghp_yyyyyyyyyyyyy note=sk-proj-zzzzzzzzzzzzzzzzzzzz"
	wantRedacted, changed := redactSecrets("root-1", rawContent)
	if !changed {
		t.Fatalf("precondition failed — redactSecrets must change the test content; got %q", wantRedacted)
	}
	if bytes.Contains([]byte(wantRedacted), []byte("sk-ant-xxxxxxxxxxxxxxxx")) {
		t.Fatalf("precondition failed — redacted content still contains raw secret: %s", wantRedacted)
	}

	w := mcpPost(t, h, "root-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "commit_memory",
			"arguments": map[string]interface{}{
				"content": rawContent,
				"scope":   "LOCAL",
			},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %+v", resp.Error)
	}

	// The plugin must have seen the REDACTED content, not the raw
	// secret. If this trips, redaction in the legacy-shim → v2 path
	// has regressed and credentials are flowing through to the
	// plugin's memory_records table.
	if captured.Content == "" {
		t.Fatal("plugin.CommitMemory was not called — the shim short-circuited before reaching v2")
	}
	if captured.Content == rawContent {
		t.Errorf("legacy commit_memory leaked raw secret to plugin: %q", captured.Content)
	}
	if captured.Content != wantRedacted {
		t.Errorf("captured.Content = %q, want redacted %q", captured.Content, wantRedacted)
	}
	if bytes.Contains([]byte(captured.Content), []byte("sk-ant-xxxxxxxxxxxxxxxx")) {
		t.Errorf("captured.Content still contains raw API key fragment: %s", captured.Content)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// tools/call — recall_memory
// ─────────────────────────────────────────────────────────────────────────────

// TestMCPHandler_RecallMemory_GlobalScope_ScrubsInternalError mirrors the
// commit_memory scrub test on the recall_memory path. Same #1747 review
// fix applied: wire v2 stubs so the request reaches the GLOBAL-block
// path in scopeToReadableNamespaces (which produces the same "GLOBAL
// scope is not permitted via the MCP bridge" internal error that the
// leaked-tokens loop below tests for). Without v2 stubs the handler
// short-circuits on `memoryV2Available()` and the leaked-tokens loop
// becomes vacuously true.
func TestMCPHandler_RecallMemory_GlobalScope_ScrubsInternalError(t *testing.T) {
	h, mock := newMCPHandler(t)
	h.withMemoryV2APIs(&stubMemoryPlugin{}, rootNamespaceResolver())
	// No DB expectations — handler must abort before touching the DB.

	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      11,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "recall_memory",
			"arguments": map[string]interface{}{
				"query": "secret",
				"scope": "GLOBAL",
			},
		},
	})

	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	// (1) C3: an error must be reported.
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for GLOBAL scope recall, got nil")
	}
	// (2) OFFSEC-001 positive assertions — exact equality on the scrubbed
	// constants so any change (re-leak of err.Error(), code mutation) trips
	// the test.
	if resp.Error.Code != -32000 {
		t.Errorf("error code should be -32000 (Server error / dispatch-failure), got: %d", resp.Error.Code)
	}
	if resp.Error.Message != "tool call failed" {
		t.Errorf("error message should be the OFFSEC-001 constant %q, got: %q", "tool call failed", resp.Error.Message)
	}
	// (3) OFFSEC-001 negative assertions — the internal reason must NOT appear
	// in the client-visible message.
	leakedTokens := []string{
		"GLOBAL",    // scope name
		"scope",     // policy lexicon
		"permitted", // policy verb
		"bridge",    // internal architecture term
		"LOCAL",     // alternative scope name
		"TEAM",      // alternative scope name
	}
	for _, tok := range leakedTokens {
		if bytes.Contains([]byte(resp.Error.Message), []byte(tok)) {
			t.Errorf("OFFSEC-001 scrub regression: client-visible error.message leaks internal token %q (got: %q)", tok, resp.Error.Message)
		}
	}
	// (4) C3 invariant preserved: handler must short-circuit before any DB call.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls on GLOBAL scope block: %v", err)
	}
}

// Issue #1733: TestMCPHandler_RecallMemory_LocalScope_Empty removed —
// it asserted on the legacy SQL SELECT path. The v2 empty-result
// rendering is covered by TestToolRecallMemory_RoutesThroughV2WhenWired
// (mcp_tools_memory_legacy_shim_test.go) which uses a stub plugin that
// returns an empty SearchResponse.

// ─────────────────────────────────────────────────────────────────────────────
// tools/call — send_message_to_user
// ─────────────────────────────────────────────────────────────────────────────

func TestMCPHandler_SendMessageToUser_Blocked_WhenEnvNotSet(t *testing.T) {
	_ = os.Unsetenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE")
	h, mock := newMCPHandler(t)
	// No DB expectations — handler must abort before touching DB.

	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      13,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "send_message_to_user",
			"arguments": map[string]interface{}{
				"message": "hello",
			},
		},
	})

	var resp mcpResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error == nil {
		t.Error("expected JSON-RPC error when MOLECULE_MCP_ALLOW_SEND_MESSAGE is unset")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// TestMCPHandler_SendMessageToUser_DBErrorLogsAndStill200s pins the
// "best-effort persistence" contract: when the activity_log INSERT
// fails (DB hiccup, constraint violation, transient connection drop),
// the tool MUST still return success to the agent because the WS
// broadcast already succeeded — the user has seen the message.
//
// This matches /notify (activity.go) behavior. Returning an error
// here would cause the agent to retry and re-broadcast, double-
// rendering the message in the user's live chat panel for every
// retry until the DB recovers.
func TestMCPHandler_SendMessageToUser_DBErrorLogsAndStill200s(t *testing.T) {
	t.Setenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE", "true")
	h, mock := newMCPHandler(t)

	mock.ExpectQuery("SELECT name, talk_to_user_enabled FROM workspaces").
		WithArgs("ws-err").
		WillReturnRows(sqlmock.NewRows([]string{"name", "talk_to_user_enabled"}).AddRow("CEO Ryan PC", true))

	// INSERT fails — must NOT abort the tool response.
	mock.ExpectExec(`INSERT INTO activity_logs.*'a2a_receive'.*'notify'`).
		WillReturnError(errors.New("transient db error"))

	w := mcpPost(t, h, "ws-err", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      100,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "send_message_to_user",
			"arguments": map[string]interface{}{
				"message": "should not be lost from the live chat",
			},
		},
	})

	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON-RPC: %v", err)
	}
	// Tool response is success — INSERT failure logged, broadcast
	// already succeeded.
	if resp.Error != nil {
		t.Errorf("tool response should be success on DB error (broadcast won), got JSON-RPC error: %+v", resp.Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected DB calls in order: %v", err)
	}
}

// TestMCPHandler_SendMessageToUser_ResponseBodyShape pins the
// response_body JSON shape stored in activity_logs. This shape MUST
// match what the canvas hydrater (extractResponseText in
// historyHydration.ts) reads — specifically `{"result": "<text>"}`.
// Any drift in the JSON shape silently breaks chat history without
// failing the INSERT.
//
// Caught the same drift class flagged in
// feedback_assert_exact_not_substring.md: a substring match on
// "result" would pass even if the field were renamed; we assert the
// exact JSON shape.
func TestMCPHandler_SendMessageToUser_ResponseBodyShape(t *testing.T) {
	t.Setenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE", "true")
	h, mock := newMCPHandler(t)

	const userMessage = "Hi there from the agent"

	mock.ExpectQuery("SELECT name, talk_to_user_enabled FROM workspaces").
		WithArgs("ws-shape").
		WillReturnRows(sqlmock.NewRows([]string{"name", "talk_to_user_enabled"}).AddRow("CEO Ryan PC", true))

	// Capture the response_body argument and assert its exact shape.
	mock.ExpectExec(`INSERT INTO activity_logs.*'a2a_receive'.*'notify'`).
		WithArgs(
			"ws-shape",
			sqlmock.AnyArg(), // summary
			// The response_body MUST be JSON `{"result": "<message>"}`.
			// Any other shape (e.g., wrapping in a Task object) breaks
			// the canvas hydrater's `body.result` extractor.
			`{"result":"`+userMessage+`"}`,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := mcpPost(t, h, "ws-shape", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      101,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "send_message_to_user",
			"arguments": map[string]interface{}{
				"message": userMessage,
			},
		},
	})

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("response_body shape drift — would silently break canvas chat history: %v", err)
	}
}

// TestMCPHandler_SendMessageToUser_PersistsToActivityLog pins the fix
// for the acme-demo / CEO Ryan PC chat-history data-loss bug:
// external claude-code agents using molecule-mcp's send_message_to_user
// tool route through THIS handler (not the HTTP /notify endpoint),
// and the handler used to broadcast WS only — visible live, gone on
// reload because nothing wrote to activity_logs.
//
// Pins:
//   - INSERT happens on the success path (broadcast + DB write).
//   - INSERT shape mirrors the HTTP /notify handler exactly:
//     activity_type='a2a_receive', method='notify', request_body NULL,
//     response_body={"result": message}, status='ok'. The canvas
//     hydration query (`type=a2a_receive&source=canvas`) treats
//     both writers as the same shape — drift here means the bug
//     re-surfaces silently.
func TestMCPHandler_SendMessageToUser_PersistsToActivityLog(t *testing.T) {
	t.Setenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE", "true")
	h, mock := newMCPHandler(t)

	// Workspace lookup — the handler verifies the workspace exists
	// before it does anything else. Returning a name lets the
	// broadcast payload populate; the test doesn't assert on the
	// broadcast (no observable WS in this fake), only on the DB.
	mock.ExpectQuery("SELECT name, talk_to_user_enabled FROM workspaces").
		WithArgs("ws-msg").
		WillReturnRows(sqlmock.NewRows([]string{"name", "talk_to_user_enabled"}).AddRow("CEO Ryan PC", true))

	// The persistence INSERT — pin the exact shape so a future
	// refactor that switches columns or drops `method='notify'`
	// breaks the test loud, not silently. Match by regex on the
	// table + activity_type + method literals.
	mock.ExpectExec(`INSERT INTO activity_logs.*'a2a_receive'.*'notify'`).
		WithArgs(
			"ws-msg",
			sqlmock.AnyArg(), // summary "Agent message: ..."
			sqlmock.AnyArg(), // response_body JSON
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := mcpPost(t, h, "ws-msg", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "send_message_to_user",
			"arguments": map[string]interface{}{
				"message": "Hello, this should persist!",
			},
		},
	})

	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON-RPC: %v\nbody=%s", err, w.Body.String())
	}
	if resp.Error != nil {
		t.Errorf("unexpected JSON-RPC error: %+v", resp.Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations not met (INSERT missing → acme-demo data-loss regression): %v", err)
	}
}

func TestMCPHandler_SendMessageToUser_WithAttachments_PersistsFileParts(t *testing.T) {
	t.Setenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE", "true")
	h, mock := newMCPHandler(t)

	mock.ExpectQuery("SELECT name, talk_to_user_enabled FROM workspaces").
		WithArgs("ws-mcp-attach").
		WillReturnRows(sqlmock.NewRows([]string{"name", "talk_to_user_enabled"}).AddRow("Hermes Agent", true))

	mock.ExpectExec(`INSERT INTO activity_logs.*'a2a_receive'.*'notify'`).
		WithArgs(
			"ws-mcp-attach",
			sqlmock.AnyArg(),
			jsonMatcher{
				desc: "MCP send_message_to_user response_body has result + file parts",
				predicate: func(parsed map[string]any) bool {
					if parsed["result"] != "see attached" {
						return false
					}
					parts, ok := parsed["parts"].([]any)
					if !ok || len(parts) != 1 {
						return false
					}
					part, ok := parts[0].(map[string]any)
					if !ok || part["kind"] != "file" {
						return false
					}
					file, ok := part["file"].(map[string]any)
					return ok &&
						file["uri"] == "workspace:/workspace/org_chart_v2.png" &&
						file["name"] == "org_chart_v2.png" &&
						file["mimeType"] == "image/png" &&
						file["size"] == float64(12345)
				},
			},
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := mcpPost(t, h, "ws-mcp-attach", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      102,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "send_message_to_user",
			"arguments": map[string]interface{}{
				"message": "see attached",
				"attachments": []map[string]interface{}{
					{
						"uri":      "workspace:/workspace/org_chart_v2.png",
						"name":     "org_chart_v2.png",
						"mimeType": "image/png",
						"size":     12345,
					},
				},
			},
		},
	})

	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON-RPC: %v\nbody=%s", err, w.Body.String())
	}
	if resp.Error != nil {
		t.Errorf("unexpected JSON-RPC error: %+v", resp.Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("MCP attachment response_body drift: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Parse error
// ─────────────────────────────────────────────────────────────────────────────

func TestMCPHandler_Call_InvalidJSON_Returns400(t *testing.T) {
	h, _ := newMCPHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Call(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SSE Stream
// ─────────────────────────────────────────────────────────────────────────────

func TestMCPHandler_Stream_SendsEndpointEvent(t *testing.T) {
	h, _ := newMCPHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-stream"}}

	// Use a context that is immediately cancelled so Stream returns quickly.
	ctx, cancel := contextForTest()
	defer cancel()

	c.Request = httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	cancel() // cancel before calling so Stream exits after the first write

	h.Stream(c)

	body := w.Body.String()
	if !bytes.Contains([]byte(body), []byte("event: endpoint")) {
		t.Errorf("SSE stream should contain 'event: endpoint', got: %q", body)
	}
	if !bytes.Contains([]byte(body), []byte("/workspaces/ws-stream/mcp")) {
		t.Errorf("SSE endpoint data should contain the POST URL, got: %q", body)
	}
	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type: got %q, want text/event-stream", w.Header().Get("Content-Type"))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// extractA2AText helper
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractA2AText_ArtifactsFormat(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":"x","result":{"artifacts":[{"parts":[{"type":"text","text":"hello from agent"}]}]}}`)
	got := extractA2AText(body)
	if got != "hello from agent" {
		t.Errorf("extractA2AText: got %q, want %q", got, "hello from agent")
	}
}

func TestExtractA2AText_MessageFormat(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":"x","result":{"message":{"role":"assistant","parts":[{"type":"text","text":"agent reply"}]}}}`)
	got := extractA2AText(body)
	if got != "agent reply" {
		t.Errorf("extractA2AText: got %q, want %q", got, "agent reply")
	}
}

func TestExtractA2AText_ErrorFormat(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":"x","error":{"code":-32000,"message":"something went wrong"}}`)
	got := extractA2AText(body)
	if !bytes.Contains([]byte(got), []byte("something went wrong")) {
		t.Errorf("extractA2AText: error message not propagated, got %q", got)
	}
}

func TestExtractA2AText_InvalidJSON_ReturnRaw(t *testing.T) {
	body := []byte(`not json`)
	got := extractA2AText(body)
	if got != "not json" {
		t.Errorf("extractA2AText: expected raw fallback, got %q", got)
	}
}

// ==================== SSRF Defence — isSafeURL ====================

func TestIsSafeURL_AllowsHTTPS(t *testing.T) {
	err := isSafeURL("https://api.openai.com/v1/models")
	if err != nil {
		t.Errorf("isSafeURL: expected https://api.openai.com to be allowed, got %v", err)
	}
}

func TestIsSafeURL_AllowsPublicHTTP(t *testing.T) {
	err := isSafeURL("http://example.com/agent")
	if err != nil {
		t.Errorf("isSafeURL: expected http://example.com to be allowed, got %v", err)
	}
}

func TestIsSafeURL_BlocksFileScheme(t *testing.T) {
	err := isSafeURL("file:///etc/passwd")
	if err == nil {
		t.Errorf("isSafeURL: expected file:// to be blocked, got nil")
	}
}

func TestIsSafeURL_BlocksFtpScheme(t *testing.T) {
	err := isSafeURL("ftp://internal-host/file")
	if err == nil {
		t.Errorf("isSafeURL: expected ftp:// to be blocked, got nil")
	}
}

func TestIsSafeURL_BlocksLocalhost(t *testing.T) {
	err := isSafeURL("http://127.0.0.1:8080/agent")
	if err == nil {
		t.Errorf("isSafeURL: expected 127.0.0.1 to be blocked, got nil")
	}
}

func TestIsSafeURL_BlocksLocalhostV6(t *testing.T) {
	err := isSafeURL("http://[::1]:8080/agent")
	if err == nil {
		t.Errorf("isSafeURL: expected [::1] to be blocked, got nil")
	}
}

func TestIsSafeURL_Blocks169_254_Metadata(t *testing.T) {
	err := isSafeURL("http://169.254.169.254/latest/meta-data/")
	if err == nil {
		t.Errorf("isSafeURL: expected 169.254.169.254 to be blocked, got nil")
	}
}

func TestIsSafeURL_Blocks10xPrivate(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	err := isSafeURL("http://10.0.0.1/agent")
	if err == nil {
		t.Errorf("isSafeURL: expected 10.x.x.x to be blocked, got nil")
	}
}

func TestIsSafeURL_Blocks172Private(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	err := isSafeURL("http://172.16.0.1/agent")
	if err == nil {
		t.Errorf("isSafeURL: expected 172.16.0.0/12 to be blocked, got nil")
	}
}

func TestIsSafeURL_Blocks192_168Private(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	err := isSafeURL("http://192.168.1.100/agent")
	if err == nil {
		t.Errorf("isSafeURL: expected 192.168.x.x to be blocked, got nil")
	}
}

func TestIsSafeURL_BlocksEmptyHost(t *testing.T) {
	err := isSafeURL("http:///")
	if err == nil {
		t.Errorf("isSafeURL: expected empty hostname to be blocked, got nil")
	}
}

func TestIsSafeURL_BlocksInvalidURL(t *testing.T) {
	err := isSafeURL("http://[invalid")
	if err == nil {
		t.Errorf("isSafeURL: expected invalid URL to be blocked, got nil")
	}
}

// ==================== SSRF Defence — isPrivateOrMetadataIP ====================

func TestIsPrivateOrMetadataIP_10Range(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	tests := []string{"10.0.0.0", "10.255.255.255", "10.1.2.3"}
	for _, ip := range tests {
		if !isPrivateOrMetadataIP(net.ParseIP(ip)) {
			t.Errorf("isPrivateOrMetadataIP: expected %s to be private", ip)
		}
	}
}

func TestIsPrivateOrMetadataIP_172Range(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	tests := []string{"172.16.0.0", "172.31.255.255", "172.20.1.1"}
	for _, ip := range tests {
		if !isPrivateOrMetadataIP(net.ParseIP(ip)) {
			t.Errorf("isPrivateOrMetadataIP: expected %s to be private", ip)
		}
	}
}

func TestIsPrivateOrMetadataIP_192_168Range(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	tests := []string{"192.168.0.0", "192.168.255.255", "192.168.1.1"}
	for _, ip := range tests {
		if !isPrivateOrMetadataIP(net.ParseIP(ip)) {
			t.Errorf("isPrivateOrMetadataIP: expected %s to be private", ip)
		}
	}
}

func TestIsPrivateOrMetadataIP_169_254Metadata(t *testing.T) {
	if !isPrivateOrMetadataIP(net.ParseIP("169.254.169.254")) {
		t.Errorf("isPrivateOrMetadataIP: expected 169.254.169.254 to be metadata")
	}
	if !isPrivateOrMetadataIP(net.ParseIP("169.254.0.1")) {
		t.Errorf("isPrivateOrMetadataIP: expected 169.254.0.1 to be metadata")
	}
}

func TestIsPrivateOrMetadataIP_100_64CarrierNAT(t *testing.T) {
	if !isPrivateOrMetadataIP(net.ParseIP("100.64.0.1")) {
		t.Errorf("isPrivateOrMetadataIP: expected 100.64.0.0/10 to be carrier-NAT private")
	}
}

func TestIsPrivateOrMetadataIP_PublicAllowed(t *testing.T) {
	public := []net.IP{
		net.ParseIP("8.8.8.8"),
		net.ParseIP("1.1.1.1"),
		net.ParseIP("34.117.59.81"),
	}
	for _, ip := range public {
		if isPrivateOrMetadataIP(ip) {
			t.Errorf("isPrivateOrMetadataIP: expected %s to be public", ip)
		}
	}
}

// TestMCPHandler_Call_MalformedJSON returns constant parse-error message.
// Per OFFSEC-001 / #259: err.Error() must not leak struct field names or
// JSON library internals in JSON-RPC error.message.
func TestMCPHandler_Call_MalformedJSON_ReturnsConstantParseError(t *testing.T) {
	h, _ := newMCPHandler(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	// Valid JSON-RPC 2.0 envelope but JSON body is malformed.
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBuffer([]byte("not valid json{][")))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Call(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	// Message must be a constant — no err.Error() content.
	if resp.Error.Message != "parse error" {
		t.Errorf("error message should be constant 'parse error', got: %q", resp.Error.Message)
	}
	// Code must be -32700 (Parse error).
	if resp.Error.Code != -32700 {
		t.Errorf("error code should be -32700, got: %d", resp.Error.Code)
	}
}

// TestMCPHandler_dispatchRPC_InvalidParams returns constant message.
// Per OFFSEC-001 / #259: err.Error() from json.Unmarshal must not be
// returned in JSON-RPC error.message.
func TestMCPHandler_dispatchRPC_InvalidParams_ReturnsConstantMessage(t *testing.T) {
	h, _ := newMCPHandler(t)

	// Valid JSON-RPC but params is a string (not an object) — invalid for tools/call.
	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  "not an object", // string instead of object — json.Unmarshal fails
	})

	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	// Message must be a constant — no JSON library error content.
	if resp.Error.Message != "invalid parameters" {
		t.Errorf("error message should be constant 'invalid parameters', got: %q", resp.Error.Message)
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code should be -32602 (Invalid params), got: %d", resp.Error.Code)
	}
}

// TestMCPHandler_dispatchRPC_UnknownTool returns constant tool-failed message.
// Per OFFSEC-001 / #259: dispatch errors must not leak workspace IDs or
// internal paths.  Note: this test exercises the dispatch path through
// dispatchRPC since dispatch is package-private.
func TestMCPHandler_dispatchRPC_UnknownTool_ReturnsConstantMessage(t *testing.T) {
	h, _ := newMCPHandler(t)

	// Valid params shape but tool name does not exist.
	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "nonexistent_tool_xyz",
			"arguments": map[string]interface{}{},
		},
	})

	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for unknown tool, got nil")
	}
	// Message must be a constant — no "unknown tool: nonexistent_tool_xyz" leak.
	if resp.Error.Message != "tool call failed" {
		t.Errorf("error message should be constant 'tool call failed', got: %q", resp.Error.Message)
	}
	if resp.Error.Code != -32000 {
		t.Errorf("error code should be -32000 (Server error), got: %d", resp.Error.Code)
	}
}

// TestMCPHandler_dispatchRPC_InvalidParams_NilParams covers the edge case
// where params is present but not an object (e.g. an array). json.Unmarshal
// into the params struct fails, and we assert the constant error message.
func TestMCPHandler_dispatchRPC_InvalidParams_ArrayInsteadOfObject(t *testing.T) {
	h, _ := newMCPHandler(t)

	w := mcpPost(t, h, "ws-1", map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params":  []interface{}{"one", "two"}, // array instead of object
	})

	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	if resp.Error.Message != "invalid parameters" {
		t.Errorf("error message should be constant 'invalid parameters', got: %q", resp.Error.Message)
	}
}
