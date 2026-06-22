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

// ==================== GET /workspaces/:id/config ====================

func TestConfigGet_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewConfigHandler()

	mock.ExpectQuery("SELECT data FROM workspace_config WHERE workspace_id").
		WithArgs("ws-cfg-1").
		WillReturnRows(sqlmock.NewRows([]string{"data"}).
			AddRow([]byte(`{"model":"gpt-4","tier":2}`)))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cfg-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-cfg-1/config", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["data"] == nil {
		t.Error("expected 'data' field in response")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestConfigGet_NoConfig(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewConfigHandler()

	mock.ExpectQuery("SELECT data FROM workspace_config WHERE workspace_id").
		WithArgs("ws-cfg-empty").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cfg-empty"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-cfg-empty/config", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Should return default empty object
	var resp map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &resp)
	if string(resp["data"]) != "{}" {
		t.Errorf("expected empty object data, got %s", string(resp["data"]))
	}
}

func TestConfigGet_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewConfigHandler()

	mock.ExpectQuery("SELECT data FROM workspace_config WHERE workspace_id").
		WithArgs("ws-cfg-err").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cfg-err"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-cfg-err/config", nil)

	handler.Get(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// ==================== PATCH /workspaces/:id/config ====================

func TestConfigPatch_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewConfigHandler()

	mock.ExpectExec("INSERT INTO workspace_config").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"model": "claude-sonnet-4-20250514", "temperature": 0.7}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-patch"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-patch/config",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Patch(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "updated" {
		t.Errorf("expected status 'updated', got %v", resp["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestConfigPatch_InvalidJSON(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewConfigHandler()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-bad"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-bad/config",
		bytes.NewBufferString("not valid json {{{"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Patch(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConfigPatch_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewConfigHandler()

	mock.ExpectExec("INSERT INTO workspace_config").
		WillReturnError(sql.ErrConnDone)

	body := `{"model": "gpt-4"}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-patch-err"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-patch-err/config",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Patch(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}
