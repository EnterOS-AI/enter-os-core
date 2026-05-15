package handlers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/channels"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/gin-gonic/gin"
)

type stubProxy struct {
	statusCode int
	respBody   []byte
	err        error
}

func (s *stubProxy) ProxyA2ARequest(ctx context.Context, workspaceID string, body []byte, callerID string, logActivity bool) (int, []byte, error) {
	return s.statusCode, s.respBody, s.err
}

type stubBroadcaster struct{}

func (s *stubBroadcaster) RecordAndBroadcast(ctx context.Context, eventType, workspaceID string, data interface{}) error {
	return nil
}

func newTestChannelManager() *channels.Manager {
	return channels.NewManager(&stubProxy{statusCode: 200}, &stubBroadcaster{})
}

// ==================== ListAdapters ====================

func TestChannelHandler_ListAdapters(t *testing.T) {
	handler := NewChannelHandler(newTestChannelManager())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/channels/adapters", nil)

	handler.ListAdapters(c)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var result []map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	if len(result) == 0 {
		t.Error("expected at least 1 adapter")
	}
	found := false
	for _, a := range result {
		if a["type"] == "telegram" {
			found = true
		}
	}
	if !found {
		t.Error("telegram not in adapter list")
	}
}

// ==================== List ====================

func TestChannelHandler_List(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewChannelHandler(newTestChannelManager())

	rows := sqlmock.NewRows([]string{
		"id", "workspace_id", "channel_type", "channel_config", "enabled",
		"allowed_users", "last_message_at", "message_count", "created_at", "updated_at",
	}).AddRow(
		"ch-1", "ws-1", "telegram",
		[]byte(`{"bot_token":"123:ABCDEFGHIJ","chat_id":"-100"}`),
		true, []byte(`["user-1"]`), nil, 5, nil, nil,
	)
	mock.ExpectQuery("SELECT .* FROM workspace_channels WHERE workspace_id").
		WithArgs("ws-1").
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/workspaces/ws-1/channels", nil)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}

	handler.List(c)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var result []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &result)
	if len(result) != 1 {
		t.Fatalf("expected 1 channel, got %d", len(result))
	}

	// Verify bot_token is masked
	config := result[0]["config"].(map[string]interface{})
	token := config["bot_token"].(string)
	if token == "123:ABCDEFGHIJ" {
		t.Error("bot_token should be masked in list response")
	}
	if token != "123:...GHIJ" {
		t.Errorf("expected masked token '123:...GHIJ', got %q", token)
	}
}

// ==================== Create ====================

func TestChannelHandler_Create_Success(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewChannelHandler(newTestChannelManager())

	mock.ExpectQuery("INSERT INTO workspace_channels").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("new-ch-id"))
	// Reload query
	mock.ExpectQuery("SELECT .* FROM workspace_channels").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "channel_type", "channel_config", "enabled", "allowed_users"}))

	body, _ := json.Marshal(map[string]interface{}{
		"channel_type":  "telegram",
		"config":        map[string]interface{}{"bot_token": "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "chat_id": "-100"},
		"allowed_users": []string{"user-1"},
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/workspaces/ws-1/channels", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}

	handler.Create(c)

	if w.Code != 201 {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &result)
	if result["id"] != "new-ch-id" {
		t.Errorf("expected id 'new-ch-id', got %v", result["id"])
	}
}

func TestChannelHandler_Create_MissingType(t *testing.T) {
	handler := NewChannelHandler(newTestChannelManager())

	body, _ := json.Marshal(map[string]interface{}{
		"config": map[string]interface{}{"bot_token": "123"},
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/workspaces/ws-1/channels", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}

	handler.Create(c)

	if w.Code != 400 {
		t.Errorf("expected 400 for missing channel_type, got %d", w.Code)
	}
}

func TestChannelHandler_Create_UnsupportedType(t *testing.T) {
	handler := NewChannelHandler(newTestChannelManager())

	body, _ := json.Marshal(map[string]interface{}{
		"channel_type": "whatsapp",
		"config":       map[string]interface{}{},
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/workspaces/ws-1/channels", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}

	handler.Create(c)

	if w.Code != 400 {
		t.Errorf("expected 400 for unsupported type, got %d", w.Code)
	}
}

func TestChannelHandler_Create_InvalidConfig(t *testing.T) {
	handler := NewChannelHandler(newTestChannelManager())

	body, _ := json.Marshal(map[string]interface{}{
		"channel_type": "telegram",
		"config":       map[string]interface{}{}, // missing bot_token + chat_id
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/workspaces/ws-1/channels", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}

	handler.Create(c)

	if w.Code != 400 {
		t.Errorf("expected 400 for invalid config, got %d", w.Code)
	}
}

// ==================== Update ====================

func TestChannelHandler_Update_Success(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewChannelHandler(newTestChannelManager())

	mock.ExpectExec("UPDATE workspace_channels").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT .* FROM workspace_channels").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "channel_type", "channel_config", "enabled", "allowed_users"}))

	enabled := false
	body, _ := json.Marshal(map[string]interface{}{
		"enabled": enabled,
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("PATCH", "/workspaces/ws-1/channels/ch-1", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-1"}}

	handler.Update(c)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestChannelHandler_Update_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewChannelHandler(newTestChannelManager())

	mock.ExpectExec("UPDATE workspace_channels").
		WillReturnResult(sqlmock.NewResult(0, 0))

	body, _ := json.Marshal(map[string]interface{}{"enabled": false})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("PATCH", "/workspaces/ws-1/channels/ch-999", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-999"}}

	handler.Update(c)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ==================== Delete ====================

func TestChannelHandler_Delete_Success(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewChannelHandler(newTestChannelManager())

	mock.ExpectExec("DELETE FROM workspace_channels").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT .* FROM workspace_channels").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "channel_type", "channel_config", "enabled", "allowed_users"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("DELETE", "/workspaces/ws-1/channels/ch-1", nil)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-1"}}

	handler.Delete(c)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestChannelHandler_Delete_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewChannelHandler(newTestChannelManager())

	mock.ExpectExec("DELETE FROM workspace_channels").
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("DELETE", "/workspaces/ws-1/channels/ch-999", nil)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-999"}}

	handler.Delete(c)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ==================== Send ====================

func TestChannelHandler_Send_EmptyText(t *testing.T) {
	handler := NewChannelHandler(newTestChannelManager())

	body, _ := json.Marshal(map[string]interface{}{"text": ""})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/workspaces/ws-1/channels/ch-1/send", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-1"}}

	handler.Send(c)

	if w.Code != 400 {
		t.Errorf("expected 400 for empty text, got %d", w.Code)
	}
}

// ==================== Test (send outbound) ====================

// TestChannelHandler_Test_Success exercises the /channels/:channelId/test endpoint
// with a mock SendAdapter so the full success path is covered without hitting real
// Telegram/Slack/etc. APIs.
func TestChannelHandler_Test_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewChannelHandler(newTestChannelManager())

	mockAdapter := &channels.MockSendAdapter{Err: nil}
	channels.SetGetSendAdapter(func(ct string) (channels.SendAdapter, bool) {
		if ct == "telegram" {
			return mockAdapter, true
		}
		return channels.GetSendAdapter(ct)
	})
	t.Cleanup(channels.ResetSendAdapters)

	// loadChannel → valid row
	mock.ExpectQuery("SELECT .+ FROM workspace_channels WHERE id").
		WithArgs("ch-test-ok").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "channel_type", "channel_config",
			"enabled", "allowed_users",
		}).AddRow("ch-test-ok", "ws-1", "telegram",
			`{"bot_token":"123:AAA","chat_id":"-100"}`,
			true, `[]`))

	// UPDATE message_count + last_message_at
	mock.ExpectExec("UPDATE workspace_channels SET last_message_at").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/channels/ch-test-ok/test", nil)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-test-ok"}}

	handler.Test(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", resp["status"])
	}
	if mockAdapter.Calls != 1 {
		t.Errorf("expected SendMessage called once, got %d", mockAdapter.Calls)
	}
	if mockAdapter.SentChat != "-100" {
		t.Errorf("expected chat_id '-100', got %q", mockAdapter.SentChat)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestChannelHandler_Test_ChannelNotFound verifies that when loadChannel returns
// no rows, the Test handler returns 500 with a "test message failed" error.
func TestChannelHandler_Test_ChannelNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewChannelHandler(newTestChannelManager())

	// loadChannel → no rows
	mock.ExpectQuery("SELECT .+ FROM workspace_channels WHERE id").
		WithArgs("ch-missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "channel_type", "channel_config",
			"enabled", "allowed_users",
		}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/channels/ch-missing/test", nil)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-missing"}}

	handler.Test(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for missing channel, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "test message failed" {
		t.Errorf("expected error 'test message failed', got %v", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestChannelHandler_Send_Success covers the full outbound send success path:
// budget check passes → loadChannel → mock SendMessage succeeds → UPDATE count → 200.
func TestChannelHandler_Send_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewChannelHandler(newTestChannelManager())

	mockAdapter := &channels.MockSendAdapter{Err: nil}
	channels.SetGetSendAdapter(func(ct string) (channels.SendAdapter, bool) {
		if ct == "telegram" {
			return mockAdapter, true
		}
		return channels.GetSendAdapter(ct)
	})
	t.Cleanup(channels.ResetSendAdapters)

	// Budget check: count=0, no budget limit
	mock.ExpectQuery("SELECT message_count, channel_budget FROM workspace_channels WHERE id").
		WithArgs("ch-send-ok").
		WillReturnRows(sqlmock.NewRows([]string{"message_count", "channel_budget"}).
			AddRow(0, nil))

	// loadChannel → valid row
	mock.ExpectQuery("SELECT .+ FROM workspace_channels WHERE id").
		WithArgs("ch-send-ok").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "channel_type", "channel_config",
			"enabled", "allowed_users",
		}).AddRow("ch-send-ok", "ws-1", "telegram",
			`{"bot_token":"123:AAA","chat_id":"-100"}`,
			true, `[]`))

	// UPDATE message_count
	mock.ExpectExec("UPDATE workspace_channels SET last_message_at").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body, _ := json.Marshal(map[string]string{"text": "hello from test"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/channels/ch-send-ok/send", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-send-ok"}}

	handler.Send(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "sent" {
		t.Errorf("expected status 'sent', got %v", resp["status"])
	}
	if mockAdapter.Calls != 1 {
		t.Errorf("expected SendMessage called once, got %d", mockAdapter.Calls)
	}
	if mockAdapter.SentText != "hello from test" {
		t.Errorf("expected 'hello from test', got %q", mockAdapter.SentText)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestChannelHandler_Send_ChannelNotFound verifies that after the budget check
// passes, a missing channel returns 500 (not 404) with "send failed".
func TestChannelHandler_Send_ChannelNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewChannelHandler(newTestChannelManager())

	// Budget check passes (NULL budget → no limit)
	mock.ExpectQuery("SELECT message_count, channel_budget FROM workspace_channels WHERE id").
		WithArgs("ch-send-missing").
		WillReturnRows(sqlmock.NewRows([]string{"message_count", "channel_budget"}).
			AddRow(0, nil))

	// loadChannel → no rows
	mock.ExpectQuery("SELECT .+ FROM workspace_channels WHERE id").
		WithArgs("ch-send-missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "channel_type", "channel_config",
			"enabled", "allowed_users",
		}))

	body, _ := json.Marshal(map[string]string{"text": "hello"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/channels/ch-send-missing/send", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-send-missing"}}

	handler.Send(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for missing channel, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "send failed" {
		t.Errorf("expected error 'send failed', got %v", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// ==================== Webhook ====================

func TestChannelHandler_Webhook_UnknownType(t *testing.T) {
	handler := NewChannelHandler(newTestChannelManager())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/webhooks/whatsapp", nil)
	c.Params = gin.Params{{Key: "type", Value: "whatsapp"}}

	handler.Webhook(c)

	if w.Code != 404 {
		t.Errorf("expected 404 for unknown type, got %d", w.Code)
	}
}

// ==================== Discover ====================

func TestChannelHandler_Discover_MissingToken(t *testing.T) {
	handler := NewChannelHandler(newTestChannelManager())

	body, _ := json.Marshal(map[string]interface{}{"channel_type": "telegram"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/channels/discover", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Discover(c)

	if w.Code != 400 {
		t.Errorf("expected 400 for missing token, got %d", w.Code)
	}
}

func TestChannelHandler_Discover_UnsupportedType(t *testing.T) {
	// Set up db.DB so PausePollersForToken (called inside Discover) doesn't panic.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { mockDB.Close() })
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB })

	mock.ExpectQuery(`SELECT id, channel_config FROM workspace_channels WHERE enabled = true AND workspace_id`).
		WithArgs("ws-test").
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_config"}))

	handler := NewChannelHandler(newTestChannelManager())

	// #329: workspace_id required — include so we actually reach the
	// unsupported-type check instead of bouncing at the new scope gate.
	body, _ := json.Marshal(map[string]interface{}{
		"channel_type": "whatsapp",
		"bot_token":    "fake",
		"workspace_id": "ws-test",
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/channels/discover", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Discover(c)

	if w.Code != 400 {
		t.Errorf("expected 400 for unsupported type, got %d", w.Code)
	}
}

func TestChannelHandler_Discover_InvalidBotToken(t *testing.T) {
	// Set up db.DB so PausePollersForToken (called inside Discover) doesn't panic.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { mockDB.Close() })
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB })

	mock.ExpectQuery(`SELECT id, channel_config FROM workspace_channels WHERE enabled = true AND workspace_id`).
		WithArgs("ws-test").
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_config"}))

	handler := NewChannelHandler(newTestChannelManager())

	body, _ := json.Marshal(map[string]interface{}{
		"channel_type": "telegram",
		"bot_token":    "clearly-not-a-real-token",
		"workspace_id": "ws-test",
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/channels/discover", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Discover(c)

	if w.Code != 400 {
		t.Errorf("expected 400 for invalid token, got %d", w.Code)
	}

	// Verify error is user-friendly (not a raw tgbotapi error)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	errMsg, _ := resp["error"].(string)
	if errMsg == "" {
		t.Error("expected error field in response")
	}
}

// #329: workspace_id is now required. Without it, Discover must 400
// *before* issuing the unscoped DB query that would decrypt every
// tenant's bot tokens.
func TestChannelHandler_Discover_329_RequiresWorkspaceID(t *testing.T) {
	handler := NewChannelHandler(newTestChannelManager())

	body, _ := json.Marshal(map[string]interface{}{
		"channel_type": "telegram",
		"bot_token":    "any-non-empty-token",
		// workspace_id intentionally omitted
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/channels/discover", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Discover(c)

	if w.Code != 400 {
		t.Errorf("expected 400 when workspace_id missing, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if errMsg, _ := resp["error"].(string); errMsg != "workspace_id is required" {
		t.Errorf("expected workspace_id error, got %q", errMsg)
	}
}

// ==================== System Caller Prefix ====================

func TestSystemCallerPrefix_ChannelIncluded(t *testing.T) {
	if !isSystemCaller("channel:telegram") {
		t.Error("channel:telegram should be recognized as system caller")
	}
	if !isSystemCaller("channel:slack") {
		t.Error("channel:slack should be recognized as system caller")
	}
	if isSystemCaller("user:someone") {
		t.Error("user:someone should NOT be a system caller")
	}
}

// ==================== Per-channel budget (#368) ====================

// TestChannelHandler_Send_BudgetExceeded verifies that when message_count
// equals channel_budget, the Send handler returns 429 {"error":"channel budget exceeded"}
// and does NOT call SendOutbound.
func TestChannelHandler_Send_BudgetExceeded_Returns429(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewChannelHandler(newTestChannelManager())

	// Budget = 10, message_count = 10 → at the ceiling → 429.
	mock.ExpectQuery("SELECT message_count, channel_budget FROM workspace_channels WHERE id").
		WithArgs("ch-budget-hit").
		WillReturnRows(sqlmock.NewRows([]string{"message_count", "channel_budget"}).
			AddRow(10, 10))

	body, _ := json.Marshal(map[string]string{"text": "hello"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/workspaces/ws-1/channels/ch-budget-hit/send", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-budget-hit"}}

	handler.Send(c)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 when budget exceeded, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "channel budget exceeded" {
		t.Errorf("expected error 'channel budget exceeded', got %v", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestChannelHandler_Send_BudgetExceeded_AboveLimit verifies that when
// message_count exceeds channel_budget (not just equals it), 429 is returned.
func TestChannelHandler_Send_BudgetExceeded_AboveLimit_Returns429(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewChannelHandler(newTestChannelManager())

	// Budget = 5, message_count = 99 → well above limit.
	mock.ExpectQuery("SELECT message_count, channel_budget FROM workspace_channels WHERE id").
		WithArgs("ch-over").
		WillReturnRows(sqlmock.NewRows([]string{"message_count", "channel_budget"}).
			AddRow(99, 5))

	body, _ := json.Marshal(map[string]string{"text": "hi"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/workspaces/ws-1/channels/ch-over/send", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-over"}}

	handler.Send(c)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 for over-limit, got %d: %s", w.Code, w.Body.String())
	}
}

// TestChannelHandler_Send_NoBudget verifies that when channel_budget IS NULL
// (no limit), the send proceeds past the budget check (no 429 returned).
// The eventual 500 comes from loadChannel not finding the mock channel —
// the important assertion is NOT 429.
func TestChannelHandler_Send_NoBudget_PassesThrough(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewChannelHandler(newTestChannelManager())

	// NULL budget → no restriction.
	mock.ExpectQuery("SELECT message_count, channel_budget FROM workspace_channels WHERE id").
		WithArgs("ch-unlimited").
		WillReturnRows(sqlmock.NewRows([]string{"message_count", "channel_budget"}).
			AddRow(9999, nil))

	// SendOutbound → loadChannel SELECT — channel not found → error.
	mock.ExpectQuery("SELECT id, workspace_id, channel_type, channel_config, enabled, allowed_users").
		WithArgs("ch-unlimited").
		WillReturnRows(sqlmock.NewRows([]string{}))

	body, _ := json.Marshal(map[string]string{"text": "unlimited send"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/workspaces/ws-1/channels/ch-unlimited/send", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-unlimited"}}

	handler.Send(c)

	if w.Code == http.StatusTooManyRequests {
		t.Errorf("expected budget check to pass (NULL budget), but got 429")
	}
}

// TestChannelHandler_Send_BudgetNotYetReached verifies that when
// message_count < channel_budget, the send proceeds past the budget check.
func TestChannelHandler_Send_BudgetNotYetReached_PassesThrough(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewChannelHandler(newTestChannelManager())

	// Budget = 100, message_count = 9 → still under limit.
	mock.ExpectQuery("SELECT message_count, channel_budget FROM workspace_channels WHERE id").
		WithArgs("ch-under").
		WillReturnRows(sqlmock.NewRows([]string{"message_count", "channel_budget"}).
			AddRow(9, 100))

	// SendOutbound → loadChannel SELECT — channel not found → error.
	mock.ExpectQuery("SELECT id, workspace_id, channel_type, channel_config, enabled, allowed_users").
		WithArgs("ch-under").
		WillReturnRows(sqlmock.NewRows([]string{}))

	body, _ := json.Marshal(map[string]string{"text": "still under budget"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/workspaces/ws-1/channels/ch-under/send", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "channelId", Value: "ch-under"}}

	handler.Send(c)

	if w.Code == http.StatusTooManyRequests {
		t.Errorf("expected budget check to pass (under limit), but got 429")
	}
}

// ==================== Discord Ed25519 signature verification ====================
//
// These tests cover verifyDiscordSignature and the Discord signature gate in
// the Webhook handler. They use real Ed25519 key pairs generated in-process so
// the cryptographic assertions are load-bearing (not hand-crafted hex strings).

// genDiscordKey generates a fresh Ed25519 key pair for tests.
// Returns (pubKeyHex, privKey).
func genDiscordKey(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return hex.EncodeToString(pub), priv
}

// discordSignedRequest builds an *http.Request with the correct Discord
// Ed25519 headers signed by privKey.
func discordSignedRequest(t *testing.T, body string, ts string, privKey ed25519.PrivateKey) *http.Request {
	t.Helper()
	msg := append([]byte(ts), []byte(body)...)
	sig := ed25519.Sign(privKey, msg)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/discord", strings.NewReader(body))
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(sig))
	req.Header.Set("X-Signature-Timestamp", ts)
	return req
}

// TestVerifyDiscordSignature_Valid asserts that a correctly signed request
// passes verification.
func TestVerifyDiscordSignature_Valid(t *testing.T) {
	pubHex, priv := genDiscordKey(t)
	body := `{"type":1}`
	req := discordSignedRequest(t, body, "1700000000", priv)

	if !verifyDiscordSignature(req, pubHex) {
		t.Error("expected true for valid Discord signature, got false")
	}
	// Body must be restored so subsequent reads still work.
	restored, _ := io.ReadAll(req.Body)
	if string(restored) != body {
		t.Errorf("body not restored: got %q, want %q", restored, body)
	}
}

// TestVerifyDiscordSignature_WrongKey asserts that a signature verified with
// a different public key returns false.
func TestVerifyDiscordSignature_WrongKey(t *testing.T) {
	_, priv := genDiscordKey(t)
	wrongPubHex, _ := genDiscordKey(t) // different key pair
	req := discordSignedRequest(t, `{"type":1}`, "1700000000", priv)

	if verifyDiscordSignature(req, wrongPubHex) {
		t.Error("expected false for signature verified with wrong public key")
	}
}

// TestVerifyDiscordSignature_TamperedBody asserts that modifying the body
// after signing invalidates the signature.
func TestVerifyDiscordSignature_TamperedBody(t *testing.T) {
	pubHex, priv := genDiscordKey(t)
	req := discordSignedRequest(t, `{"type":1}`, "1700000000", priv)
	// Replace the body with different content after signing.
	req.Body = io.NopCloser(strings.NewReader(`{"type":2,"tampered":true}`))

	if verifyDiscordSignature(req, pubHex) {
		t.Error("expected false for tampered body, got true")
	}
}

// TestVerifyDiscordSignature_MissingTimestamp asserts that a missing
// X-Signature-Timestamp header returns false.
func TestVerifyDiscordSignature_MissingTimestamp(t *testing.T) {
	pubHex, priv := genDiscordKey(t)
	req := discordSignedRequest(t, `{"type":1}`, "1700000000", priv)
	req.Header.Del("X-Signature-Timestamp")

	if verifyDiscordSignature(req, pubHex) {
		t.Error("expected false for missing X-Signature-Timestamp")
	}
}

// TestVerifyDiscordSignature_MissingSignature asserts that a missing
// X-Signature-Ed25519 header returns false.
func TestVerifyDiscordSignature_MissingSignature(t *testing.T) {
	pubHex, priv := genDiscordKey(t)
	req := discordSignedRequest(t, `{"type":1}`, "1700000000", priv)
	req.Header.Del("X-Signature-Ed25519")

	if verifyDiscordSignature(req, pubHex) {
		t.Error("expected false for missing X-Signature-Ed25519")
	}
}

// TestVerifyDiscordSignature_InvalidHexSignature asserts that a non-hex
// signature returns false.
func TestVerifyDiscordSignature_InvalidHexSignature(t *testing.T) {
	pubHex, _ := genDiscordKey(t)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/discord", strings.NewReader(`{}`))
	req.Header.Set("X-Signature-Ed25519", "not-valid-hex!!!")
	req.Header.Set("X-Signature-Timestamp", "1700000000")

	if verifyDiscordSignature(req, pubHex) {
		t.Error("expected false for invalid hex signature")
	}
}

// TestVerifyDiscordSignature_InvalidHexPubKey asserts that a non-hex public
// key returns false.
func TestVerifyDiscordSignature_InvalidHexPubKey(t *testing.T) {
	_, priv := genDiscordKey(t)
	req := discordSignedRequest(t, `{}`, "1700000000", priv)

	if verifyDiscordSignature(req, "not-hex-at-all!!!") {
		t.Error("expected false for non-hex public key")
	}
}

// TestVerifyDiscordSignature_WrongLengthPubKey asserts that a hex-encoded
// byte slice that is not 32 bytes returns false.
func TestVerifyDiscordSignature_WrongLengthPubKey(t *testing.T) {
	_, priv := genDiscordKey(t)
	req := discordSignedRequest(t, `{}`, "1700000000", priv)
	// 16 bytes — too short for Ed25519.
	shortKey := hex.EncodeToString(make([]byte, 16))

	if verifyDiscordSignature(req, shortKey) {
		t.Error("expected false for short public key")
	}
}

// TestChannelHandler_Webhook_Discord_NoKey_Returns401 verifies that a Discord
// webhook request is rejected with 401 when no public key is configured in the
// DB and DISCORD_APP_PUBLIC_KEY env var is not set.
func TestChannelHandler_Webhook_Discord_NoKey_Returns401(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewChannelHandler(newTestChannelManager())

	// discordPublicKey: DB returns no rows (no Discord channels with app_public_key).
	mock.ExpectQuery(`SELECT COALESCE\(channel_config->>'app_public_key'`).
		WillReturnRows(sqlmock.NewRows([]string{"pubkey"}))

	// Ensure env var is not set.
	t.Setenv("DISCORD_APP_PUBLIC_KEY", "")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/webhooks/discord", strings.NewReader(`{"type":1}`))
	c.Request.Header.Set("X-Signature-Ed25519", "aabbcc")
	c.Request.Header.Set("X-Signature-Timestamp", "1700000000")
	c.Params = gin.Params{{Key: "type", Value: "discord"}}

	handler.Webhook(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 (no public key), got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestChannelHandler_Webhook_Discord_InvalidSig_Returns401 verifies that a
// Discord webhook with an invalid signature is rejected with 401, even when a
// valid public key is configured.
func TestChannelHandler_Webhook_Discord_InvalidSig_Returns401(t *testing.T) {
	pubHex, _ := genDiscordKey(t) // generate key but sign with a DIFFERENT key
	_, wrongPriv := genDiscordKey(t)

	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewChannelHandler(newTestChannelManager())

	// discordPublicKey: DB returns the correct pubHex.
	mock.ExpectQuery(`SELECT COALESCE\(channel_config->>'app_public_key'`).
		WillReturnRows(sqlmock.NewRows([]string{"pubkey"}).AddRow(pubHex))

	// Build a request signed with the wrong private key.
	req := discordSignedRequest(t, `{"type":1}`, "1700000000", wrongPriv)
	req.URL.Path = "/webhooks/discord"

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{{Key: "type", Value: "discord"}}

	handler.Webhook(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 (invalid sig), got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestChannelHandler_Webhook_Discord_ValidSig_PingAccepted verifies that a
// correctly signed Discord PING (type=1) passes the signature gate and the
// handler returns 200 (PING returns nil msg → "ignored" status).
func TestChannelHandler_Webhook_Discord_ValidSig_PingAccepted(t *testing.T) {
	pubHex, priv := genDiscordKey(t)

	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewChannelHandler(newTestChannelManager())

	// discordPublicKey: DB returns pubHex.
	mock.ExpectQuery(`SELECT COALESCE\(channel_config->>'app_public_key'`).
		WillReturnRows(sqlmock.NewRows([]string{"pubkey"}).AddRow(pubHex))

	body := `{"type":1}`
	req := discordSignedRequest(t, body, "1700000000", priv)
	req.URL.Path = "/webhooks/discord"

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{{Key: "type", Value: "discord"}}

	handler.Webhook(c)

	// Discord PING → ParseWebhook returns nil, nil → handler responds "ignored"
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for valid PING, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ignored") {
		t.Errorf("expected body to contain 'ignored', got: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
