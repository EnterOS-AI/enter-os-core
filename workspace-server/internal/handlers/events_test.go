package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== GET /events (List) ====================

func TestEventsList_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewEventsHandler()

	now := time.Now()
	wsID := "ws-evt-1"
	rows := sqlmock.NewRows([]string{"id", "event_type", "workspace_id", "payload", "created_at"}).
		AddRow("evt-1", "WORKSPACE_ONLINE", &wsID, []byte(`{"name":"Agent A"}`), now).
		AddRow("evt-2", "WORKSPACE_OFFLINE", &wsID, []byte(`{"name":"Agent A"}`), now)

	mock.ExpectQuery("SELECT id, event_type, workspace_id, payload, created_at").
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/events", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 2 {
		t.Errorf("expected 2 events, got %d", len(resp))
	}
	if resp[0]["event_type"] != "WORKSPACE_ONLINE" {
		t.Errorf("expected event_type 'WORKSPACE_ONLINE', got %v", resp[0]["event_type"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestEventsList_Empty(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewEventsHandler()

	mock.ExpectQuery("SELECT id, event_type, workspace_id, payload, created_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "event_type", "workspace_id", "payload", "created_at"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/events", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty list, got %d events", len(resp))
	}
}

func TestEventsList_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewEventsHandler()

	mock.ExpectQuery("SELECT id, event_type, workspace_id, payload, created_at").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/events", nil)

	handler.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// ==================== GET /events/:workspaceId (ListByWorkspace) ====================

func TestEventsListByWorkspace_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewEventsHandler()

	now := time.Now()
	wsID := "ws-by-ws"
	rows := sqlmock.NewRows([]string{"id", "event_type", "workspace_id", "payload", "created_at"}).
		AddRow("evt-3", "WORKSPACE_PROVISIONING", &wsID, []byte(`{"name":"Agent B"}`), now)

	mock.ExpectQuery("SELECT id, event_type, workspace_id, payload, created_at").
		WithArgs("ws-by-ws").
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "workspaceId", Value: "ws-by-ws"}}
	c.Request = httptest.NewRequest("GET", "/events/ws-by-ws", nil)

	handler.ListByWorkspace(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Errorf("expected 1 event, got %d", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestEventsListByWorkspace_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewEventsHandler()

	mock.ExpectQuery("SELECT id, event_type, workspace_id, payload, created_at").
		WithArgs("ws-err").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "workspaceId", Value: "ws-err"}}
	c.Request = httptest.NewRequest("GET", "/events/ws-err", nil)

	handler.ListByWorkspace(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}
