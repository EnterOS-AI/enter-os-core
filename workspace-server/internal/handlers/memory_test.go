package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== GET /workspaces/:id/memory (List) ====================

func TestMemoryList_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()

	now := time.Now()
	rows := sqlmock.NewRows([]string{"key", "value", "version", "expires_at", "updated_at"}).
		AddRow("api-key", []byte(`"sk-123"`), int64(1), nil, now).
		AddRow("count", []byte(`42`), int64(3), nil, now)

	mock.ExpectQuery("SELECT key, value, version, expires_at, updated_at").
		WithArgs("ws-mem-1").
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-mem-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-mem-1/memory", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []MemoryEntry
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(resp))
	}
	if resp[0].Key != "api-key" || resp[0].Version != 1 {
		t.Errorf("entry 0: got (%q, v%d), want (api-key, v1)", resp[0].Key, resp[0].Version)
	}
	if resp[1].Version != 3 {
		t.Errorf("entry 1 version: got %d, want 3", resp[1].Version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestMemoryList_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()
	mock.ExpectQuery("SELECT key, value, version").WithArgs("ws-dberr").WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-dberr"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-dberr/memory", nil)
	handler.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// TestMemoryList_RowsErr_Returns500 verifies that a rows.Err() set during
// iteration causes the handler to return 500 rather than partial results.
func TestMemoryList_RowsErr_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()

	cols := []string{"key", "value", "version", "expires_at", "updated_at"}
	mock.ExpectQuery("SELECT key, value, version, expires_at, updated_at").
		WithArgs("ws-rowerr").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("ok-key", []byte(`"val"`), int64(1), nil, time.Now()).
			RowError(0, errors.New("storage engine fault")))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-rowerr"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-rowerr/memory", nil)
	handler.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("rows.Err() must yield 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== GET /workspaces/:id/memory/:key (Get) ====================

func TestMemoryGet_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()

	now := time.Now()
	mock.ExpectQuery("SELECT key, value, version").
		WithArgs("ws-get", "api-key").
		WillReturnRows(sqlmock.NewRows([]string{"key", "value", "version", "expires_at", "updated_at"}).
			AddRow("api-key", []byte(`"sk-123"`), int64(5), nil, now))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-get"}, {Key: "key", Value: "api-key"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-get/memory/api-key", nil)
	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp MemoryEntry
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Version != 5 {
		t.Errorf("expected version 5, got %d", resp.Version)
	}
}

func TestMemoryGet_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()
	mock.ExpectQuery("SELECT key, value, version").
		WithArgs("ws-nf", "missing").WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-nf"}, {Key: "key", Value: "missing"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-nf/memory/missing", nil)
	handler.Get(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ==================== POST /workspaces/:id/memory (Set — no version) ====================

func TestMemorySet_NoVersion_CreateOrOverwrite(t *testing.T) {
	// Back-compat path: no if_match_version — last-write-wins upsert.
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()

	mock.ExpectQuery("INSERT INTO workspace_memory").
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(int64(7)))

	body := `{"key":"counter","value":42}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-set"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-set/memory", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if v, ok := resp["version"].(float64); !ok || int64(v) != 7 {
		t.Errorf("response should include version=7, got %v", resp["version"])
	}
}

func TestMemorySet_MissingKey(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-nokey"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-nokey/memory", bytes.NewBufferString(`{"value":"no-key"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Set(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ==================== POST /workspaces/:id/memory (Set — with if_match_version) ====================

func TestMemorySet_IfMatchVersion_Match_Updates(t *testing.T) {
	// Optimistic-lock happy path: client's expected version matches current.
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()

	mock.ExpectQuery("UPDATE workspace_memory").
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(int64(6)))

	body := `{"key":"queue","value":[1,2,3],"if_match_version":5}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-lock"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-lock/memory", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int64(resp["version"].(float64)) != 6 {
		t.Errorf("version should advance to 6, got %v", resp["version"])
	}
}

func TestMemorySet_IfMatchVersion_Mismatch_Returns409(t *testing.T) {
	// Concurrent writer incremented version while we held v=5. Caller
	// gets 409 + the current version so they can re-read + retry.
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()

	mock.ExpectQuery("UPDATE workspace_memory").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT version FROM workspace_memory").
		WithArgs("ws-conflict", "queue").
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(int64(8)))

	body := `{"key":"queue","value":[1,2,3],"if_match_version":5}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-conflict"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-conflict/memory", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Set(c)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int64(resp["expected_version"].(float64)) != 5 {
		t.Errorf("expected_version in 409 body should be 5, got %v", resp["expected_version"])
	}
	if int64(resp["current_version"].(float64)) != 8 {
		t.Errorf("current_version in 409 body should be 8, got %v", resp["current_version"])
	}
}

func TestMemorySet_IfMatchVersion_CreateOnly_OnAbsentKey(t *testing.T) {
	// if_match_version=0 is the "create-only" marker: succeed iff the
	// key doesn't exist yet. Use case: two agents simultaneously try to
	// seed a shared key — only one should win.
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()

	// UPDATE matches no row (key doesn't exist).
	mock.ExpectQuery("UPDATE workspace_memory").WillReturnError(sql.ErrNoRows)
	// Probe: still no row.
	mock.ExpectQuery("SELECT version FROM workspace_memory").
		WithArgs("ws-create", "new-key").WillReturnError(sql.ErrNoRows)
	// Create path succeeds.
	mock.ExpectQuery("INSERT INTO workspace_memory").
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(int64(1)))

	body := `{"key":"new-key","value":"hello","if_match_version":0}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-create"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-create/memory", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int64(resp["version"].(float64)) != 1 {
		t.Errorf("new row should have version 1, got %v", resp["version"])
	}
}

func TestMemorySet_IfMatchVersion_NonZero_OnAbsentKey_Returns409(t *testing.T) {
	// Caller asserted version=3 on a key that doesn't exist. 409 —
	// caller's mental model is wrong, retry after re-reading.
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()

	mock.ExpectQuery("UPDATE workspace_memory").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT version FROM workspace_memory").
		WithArgs("ws-ghost", "ghost").WillReturnError(sql.ErrNoRows)

	body := `{"key":"ghost","value":"?","if_match_version":3}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-ghost"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-ghost/memory", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Set(c)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

// ==================== DELETE /workspaces/:id/memory/:key ====================

func TestMemoryDelete_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewMemoryHandler()
	mock.ExpectExec("DELETE FROM workspace_memory").
		WithArgs("ws-del", "old-key").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-del"}, {Key: "key", Value: "old-key"}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-del/memory/old-key", nil)
	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}
