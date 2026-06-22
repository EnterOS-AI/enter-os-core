package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ---------- AgentHandler: Assign ----------

func TestAgentAssign_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewAgentHandler(broadcaster)

	// Workspace status check
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	// Active agent count check
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM agents WHERE workspace_id").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// Insert agent
	mock.ExpectQuery("INSERT INTO agents").
		WithArgs("ws-1", "claude-3-5-sonnet").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("agent-abc"))

	// RecordAndBroadcast
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/agent",
		bytes.NewBufferString(`{"model":"claude-3-5-sonnet"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Assign(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["agent_id"] != "agent-abc" {
		t.Errorf("expected agent_id agent-abc, got %v", resp["agent_id"])
	}
}

func TestAgentAssign_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAgentHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-missing").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-missing"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"model":"gpt-4"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Assign(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestAgentAssign_AlreadyHasAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAgentHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM agents WHERE workspace_id").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"model":"gpt-4"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Assign(c)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d", w.Code)
	}
}

func TestAgentAssign_MissingModel(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewAgentHandler(newTestBroadcaster())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Assign(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ---------- AgentHandler: Replace ----------

func TestAgentReplace_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewAgentHandler(broadcaster)

	// Deactivate current agent (only workspace_id is passed — model comes from RETURNING)
	mock.ExpectQuery("UPDATE agents SET status = 'replaced'").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"model"}).AddRow("old-model"))

	// Insert new agent
	mock.ExpectQuery("INSERT INTO agents").
		WithArgs("ws-1", "claude-3-5-haiku").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("agent-new"))

	// RecordAndBroadcast
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("PATCH", "/", bytes.NewBufferString(`{"model":"claude-3-5-haiku"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Replace(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["old_model"] != "old-model" {
		t.Errorf("expected old_model 'old-model', got %v", resp["old_model"])
	}
}

func TestAgentReplace_NoActiveAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAgentHandler(newTestBroadcaster())

	mock.ExpectQuery("UPDATE agents SET status = 'replaced'").
		WithArgs("ws-1").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("PATCH", "/", bytes.NewBufferString(`{"model":"gpt-4"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Replace(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestAgentReplace_MissingModel(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewAgentHandler(newTestBroadcaster())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("PATCH", "/", bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Replace(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ---------- AgentHandler: Remove ----------

func TestAgentRemove_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewAgentHandler(broadcaster)

	mock.ExpectQuery("UPDATE agents SET status = 'removed'").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "model"}).AddRow("agent-del", "gpt-4"))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("DELETE", "/", nil)

	handler.Remove(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "removed" {
		t.Errorf("expected status 'removed', got %v", resp["status"])
	}
}

func TestAgentRemove_NoActiveAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAgentHandler(newTestBroadcaster())

	mock.ExpectQuery("UPDATE agents SET status = 'removed'").
		WithArgs("ws-1").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("DELETE", "/", nil)

	handler.Remove(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ---------- AgentHandler: Move ----------

func TestAgentMove_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewAgentHandler(broadcaster)

	// Target workspace lookup
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	// Target agent count
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM agents WHERE workspace_id").
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// Move agent
	mock.ExpectQuery("UPDATE agents SET workspace_id").
		WithArgs("ws-source", "ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"id", "model"}).AddRow("agent-mov", "gpt-4"))

	// Two broadcast calls
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	c.Request = httptest.NewRequest("POST", "/",
		bytes.NewBufferString(`{"target_workspace_id":"ws-target"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Move(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["from_workspace"] != "ws-source" || resp["to_workspace"] != "ws-target" {
		t.Errorf("unexpected move response: %v", resp)
	}
}

func TestAgentMove_TargetNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAgentHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-missing").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	c.Request = httptest.NewRequest("POST", "/",
		bytes.NewBufferString(`{"target_workspace_id":"ws-missing"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Move(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestAgentMove_TargetAlreadyHasAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAgentHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM agents WHERE workspace_id").
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	c.Request = httptest.NewRequest("POST", "/",
		bytes.NewBufferString(`{"target_workspace_id":"ws-target"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Move(c)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d", w.Code)
	}
}

func TestAgentMove_MissingTargetID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewAgentHandler(newTestBroadcaster())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-source"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Move(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}
