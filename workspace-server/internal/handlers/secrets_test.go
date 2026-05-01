package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== List secrets ====================

func TestSecretsList_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery("SELECT key, created_at, updated_at FROM workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"key", "created_at", "updated_at"}).
			AddRow("API_KEY", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z").
			AddRow("DB_PASSWORD", "2024-01-02T00:00:00Z", "2024-01-03T00:00:00Z"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 2 {
		t.Errorf("expected 2 secrets, got %d", len(resp))
	}
	if resp[0]["key"] != "API_KEY" {
		t.Errorf("expected first key 'API_KEY', got %v", resp[0]["key"])
	}
	if resp[0]["has_value"] != true {
		t.Errorf("expected has_value true, got %v", resp[0]["has_value"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsList_Empty(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery("SELECT key, created_at, updated_at FROM workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"key", "created_at", "updated_at"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected 0 secrets, got %d", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsList_InvalidWorkspaceID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/not-a-uuid/secrets", nil)

	handler.List(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "invalid workspace ID" {
		t.Errorf("expected error 'invalid workspace ID', got %v", resp["error"])
	}
}

func TestSecretsList_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery("SELECT key, created_at, updated_at FROM workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", nil)

	handler.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Set secret ====================

func TestSecretsSet_InvalidWorkspaceID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "bad-id"}}

	body := `{"key":"API_KEY","value":"secret123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/bad-id/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsSet_MissingKey(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}

	body := `{"value":"secret123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsSet_MissingValue(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}

	body := `{"key":"API_KEY"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsSet_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	// The crypto.Encrypt will use plaintext mode if SECRETS_ENCRYPTION_KEY is not set
	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "API_KEY", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}

	body := `{"key":"API_KEY","value":"sk-test123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "saved" {
		t.Errorf("expected status 'saved', got %v", resp["status"])
	}
	if resp["key"] != "API_KEY" {
		t.Errorf("expected key 'API_KEY', got %v", resp["key"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsSet_AutoRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Track whether restart was called via channel (replaces time.Sleep)
	done := make(chan string, 1)
	restartFunc := func(wsID string) {
		done <- wsID
	}
	handler := NewSecretsHandler(restartFunc)

	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "DB_PASS", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}

	body := `{"key":"DB_PASS","value":"password123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case wsID := <-done:
		if wsID != "550e8400-e29b-41d4-a716-446655440000" {
			t.Errorf("expected restart to be called with workspace ID, got %q", wsID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart callback not called within timeout")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsSet_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "API_KEY", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}

	body := `{"key":"API_KEY","value":"secret"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Delete secret ====================

func TestSecretsDelete_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "API_KEY").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "key", Value: "API_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets/API_KEY", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "deleted" {
		t.Errorf("expected status 'deleted', got %v", resp["status"])
	}
	if resp["key"] != "API_KEY" {
		t.Errorf("expected key 'API_KEY', got %v", resp["key"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsDelete_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "MISSING_KEY").
		WillReturnResult(sqlmock.NewResult(0, 0)) // 0 rows affected

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "key", Value: "MISSING_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets/MISSING_KEY", nil)

	handler.Delete(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsDelete_InvalidWorkspaceID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "invalid"},
		{Key: "key", Value: "API_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/invalid/secrets/API_KEY", nil)

	handler.Delete(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsDelete_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "API_KEY").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "key", Value: "API_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets/API_KEY", nil)

	handler.Delete(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsDelete_AutoRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	done := make(chan string, 1)
	restartFunc := func(wsID string) {
		done <- wsID
	}
	handler := NewSecretsHandler(restartFunc)

	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "OLD_KEY").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "key", Value: "OLD_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets/OLD_KEY", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case wsID := <-done:
		if wsID != "550e8400-e29b-41d4-a716-446655440000" {
			t.Errorf("expected restart called for workspace, got %q", wsID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart callback not called within timeout")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== GetModel ====================

func TestSecretsGetModel_Default(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	// No MODEL_PROVIDER secret
	mock.ExpectQuery("SELECT encrypted_value, encryption_version FROM workspace_secrets").
		WithArgs("ws-model").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-model"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-model/model", nil)

	handler.GetModel(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["model"] != "" {
		t.Errorf("expected empty model, got %v", resp["model"])
	}
	if resp["source"] != "default" {
		t.Errorf("expected source 'default', got %v", resp["source"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsGetModel_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery("SELECT encrypted_value, encryption_version FROM workspace_secrets").
		WithArgs("ws-model-err").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-model-err"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-model-err/model", nil)

	handler.GetModel(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== SetModel ====================

func TestSecretsSetModel_Upsert(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restartCalled := make(chan string, 1)
	handler := NewSecretsHandler(func(id string) { restartCalled <- id })

	mock.ExpectExec(`INSERT INTO workspace_secrets`).
		WithArgs("00000000-0000-0000-0000-000000000001", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000001"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000001/model",
		strings.NewReader(`{"model":"minimax/MiniMax-M2.7"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetModel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case id := <-restartCalled:
		if id != "00000000-0000-0000-0000-000000000001" {
			t.Errorf("restart called with wrong id: %s", id)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("restart was not triggered")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsSetModel_EmptyClears(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(func(string) {})

	mock.ExpectExec(`DELETE FROM workspace_secrets`).
		WithArgs("00000000-0000-0000-0000-000000000002").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000002"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000002/model",
		strings.NewReader(`{"model":""}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetModel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsSetModel_InvalidID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/not-a-uuid/model",
		strings.NewReader(`{"model":"x"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetModel(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad UUID, got %d", w.Code)
	}
}

// ==================== GetProvider / SetProvider (Option B PR-2) ====================
//
// Mirror of the GetModel/SetModel suite. Same secret-storage shape (key=
// 'LLM_PROVIDER' instead of 'MODEL_PROVIDER'), same restart-trigger
// contract, same UUID validation gate. We pin the contract symmetrically
// so a future refactor that breaks one without the other shows up in CI.

func TestSecretsGetProvider_Default(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery("SELECT encrypted_value, encryption_version FROM workspace_secrets").
		WithArgs("ws-prov").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-prov"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-prov/provider", nil)

	handler.GetProvider(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["provider"] != "" {
		t.Errorf("expected empty provider, got %v", resp["provider"])
	}
	if resp["source"] != "default" {
		t.Errorf("expected source 'default', got %v", resp["source"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsGetProvider_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery("SELECT encrypted_value, encryption_version FROM workspace_secrets").
		WithArgs("ws-prov-err").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-prov-err"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-prov-err/provider", nil)

	handler.GetProvider(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsSetProvider_Upsert(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restartCalled := make(chan string, 1)
	handler := NewSecretsHandler(func(id string) { restartCalled <- id })

	mock.ExpectExec(`INSERT INTO workspace_secrets`).
		WithArgs("00000000-0000-0000-0000-000000000003", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000003"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000003/provider",
		strings.NewReader(`{"provider":"minimax"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetProvider(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case id := <-restartCalled:
		if id != "00000000-0000-0000-0000-000000000003" {
			t.Errorf("restart called with wrong id: %s", id)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("restart was not triggered")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsSetProvider_EmptyClears(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(func(string) {})

	mock.ExpectExec(`DELETE FROM workspace_secrets`).
		WithArgs("00000000-0000-0000-0000-000000000004").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000004"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000004/provider",
		strings.NewReader(`{"provider":""}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetProvider(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsSetProvider_InvalidID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/not-a-uuid/provider",
		strings.NewReader(`{"provider":"x"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetProvider(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad UUID, got %d", w.Code)
	}
}

// ==================== Values — Phase 30.2 decrypted pull ====================

// These tests target the secrets.Values handler (GET /workspaces/:id/secrets/values)
// which returns decrypted key→value pairs so remote agents can bootstrap their env
// without the provisioner pushing at container-create time. Auth follows the
// Phase 30.1 lazy-bootstrap contract: workspaces with any live token MUST present
// a matching Bearer, legacy workspaces (no tokens yet) are grandfathered through.

const testWsID = "550e8400-e29b-41d4-a716-446655440000"

// secretsValuesRequest builds a GET request with the given Authorization header.
func secretsValuesRequest(w http.ResponseWriter, auth string) *gin.Context {
	c, _ := gin.CreateTestContext(w.(*httptest.ResponseRecorder))
	c.Params = gin.Params{{Key: "id", Value: testWsID}}
	req := httptest.NewRequest("GET", "/workspaces/"+testWsID+"/secrets/values", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	c.Request = req
	return c
}

func TestSecretsValues_LegacyWorkspaceGrandfathered(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	// No tokens on file → grandfather path
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("GLOBAL_KEY", []byte("plainvalue"), 0))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("WS_KEY", []byte("ws_plainvalue"), 0))

	w := httptest.NewRecorder()
	c := secretsValuesRequest(w, "") // no auth — grandfathered
	handler.Values(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if body["GLOBAL_KEY"] != "plainvalue" || body["WS_KEY"] != "ws_plainvalue" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestSecretsValues_MissingTokenWhenOnFile(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := httptest.NewRecorder()
	c := secretsValuesRequest(w, "")
	handler.Values(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsValues_WrongToken(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// ValidateToken lookup returns nothing
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c := secretsValuesRequest(w, "Bearer wrong-token")
	handler.Values(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsValues_ValidTokenReturnsDecryptedMerge(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", testWsID))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at`).
		WithArgs("tok-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Global and workspace secrets — workspace overrides SHARED_KEY
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("ONLY_GLOBAL", []byte("global_val"), 0).
			AddRow("SHARED_KEY", []byte("global_loses"), 0))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("ONLY_WS", []byte("ws_val"), 0).
			AddRow("SHARED_KEY", []byte("ws_wins"), 0))

	w := httptest.NewRecorder()
	c := secretsValuesRequest(w, "Bearer good-token")
	handler.Values(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["ONLY_GLOBAL"] != "global_val" {
		t.Errorf("global missing: %v", body)
	}
	if body["ONLY_WS"] != "ws_val" {
		t.Errorf("ws missing: %v", body)
	}
	if body["SHARED_KEY"] != "ws_wins" {
		t.Errorf("workspace should override global: got %q", body["SHARED_KEY"])
	}
}

func TestSecretsValues_InvalidWorkspaceID(t *testing.T) {
	setupTestDB(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/not-a-uuid/secrets/values", nil)
	handler.Values(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ==================== Global secret auto-restart (issue #15) ====================

// TestSetGlobal_AutoRestartsAffectedWorkspaces documents the fix for #15:
// rotating a global secret (e.g. CLAUDE_CODE_OAUTH_TOKEN) must propagate to
// every running workspace without a manual restart loop. The handler should
// fire RestartByID for each non-paused/non-removed workspace that does NOT
// have a workspace-level override of the same key.
func TestSetGlobal_AutoRestartsAffectedWorkspaces(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 4)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	// INSERT ... ON CONFLICT for the global secret itself.
	mock.ExpectExec("INSERT INTO global_secrets").
		WithArgs("CLAUDE_CODE_OAUTH_TOKEN", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Query for affected workspaces — ws-A inherits, ws-B overrides (excluded).
	mock.ExpectQuery("SELECT id FROM workspaces").
		WithArgs("CLAUDE_CODE_OAUTH_TOKEN").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("ws-a").
			AddRow("ws-c"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"key":"CLAUDE_CODE_OAUTH_TOKEN","value":"sk-ant-oat01-new"}`
	c.Request = httptest.NewRequest("POST", "/admin/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetGlobal(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Collect both expected restarts (order not guaranteed).
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case id := <-restarted:
			seen[id] = true
		case <-deadline:
			t.Fatalf("auto-restart not fired for all affected workspaces; got %v", seen)
		}
	}
	if !seen["ws-a"] || !seen["ws-c"] {
		t.Errorf("expected ws-a and ws-c restarted, got %v", seen)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDeleteGlobal_AutoRestartsAffectedWorkspaces covers the delete branch of #15.
func TestDeleteGlobal_AutoRestartsAffectedWorkspaces(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 2)
	handler := NewSecretsHandler(func(id string) { restarted <- id })

	mock.ExpectExec("DELETE FROM global_secrets").
		WithArgs("OLD_KEY").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT id FROM workspaces").
		WithArgs("OLD_KEY").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-x"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "key", Value: "OLD_KEY"}}
	c.Request = httptest.NewRequest("DELETE", "/admin/secrets/OLD_KEY", nil)

	handler.DeleteGlobal(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case id := <-restarted:
		if id != "ws-x" {
			t.Errorf("expected ws-x, got %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auto-restart not fired")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
