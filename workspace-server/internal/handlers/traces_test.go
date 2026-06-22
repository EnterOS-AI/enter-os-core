package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/crypto"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== GET /workspaces/:id/traces ====================

func TestTracesList_NoLangfuseConfig(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewTracesHandler()

	os.Unsetenv("LANGFUSE_HOST")
	os.Unsetenv("LANGFUSE_PUBLIC_KEY")
	os.Unsetenv("LANGFUSE_SECRET_KEY")

	expectMissingGlobalLangfuseSecrets(mock)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-traces"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-traces/traces", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Should return empty array when Langfuse is not configured
	var resp []interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected empty list when Langfuse not configured, got %d items", len(resp))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestTracesList_PartialLangfuseConfig(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewTracesHandler()

	os.Setenv("LANGFUSE_HOST", "http://localhost:3000")
	os.Unsetenv("LANGFUSE_PUBLIC_KEY")
	os.Unsetenv("LANGFUSE_SECRET_KEY")
	defer func() {
		os.Unsetenv("LANGFUSE_HOST")
	}()

	expectMissingGlobalLangfuseSecrets(mock)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-traces-partial"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-traces-partial/traces", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty list with partial config, got %d items", len(resp))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestTracesList_LangfuseUnreachable(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewTracesHandler()

	// Set all env vars but point to unreachable host
	os.Setenv("LANGFUSE_HOST", "http://localhost:99999")
	os.Setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
	os.Setenv("LANGFUSE_SECRET_KEY", "sk-test")
	defer func() {
		os.Unsetenv("LANGFUSE_HOST")
		os.Unsetenv("LANGFUSE_PUBLIC_KEY")
		os.Unsetenv("LANGFUSE_SECRET_KEY")
	}()

	expectMissingGlobalLangfuseSecrets(mock)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-traces-down"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-traces-down/traces", nil)

	handler.List(c)

	// Should gracefully return empty when Langfuse is unreachable
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty list when Langfuse unreachable, got %d items", len(resp))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestTracesList_GlobalSecretsFallback(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewTracesHandler()

	os.Unsetenv("LANGFUSE_HOST")
	os.Unsetenv("LANGFUSE_PUBLIC_KEY")
	os.Unsetenv("LANGFUSE_SECRET_KEY")

	expectGlobalLangfuseSecret(mock, "LANGFUSE_HOST", "http://localhost:3000")
	expectGlobalLangfuseSecret(mock, "LANGFUSE_PUBLIC_KEY", "pk-global")
	expectGlobalLangfuseSecret(mock, "LANGFUSE_SECRET_KEY", "sk-global")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-traces-global"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-traces-global/traces", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty list when Langfuse unreachable, got %d items", len(resp))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestTracesList_GlobalPartialConfig(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewTracesHandler()

	os.Unsetenv("LANGFUSE_HOST")
	os.Unsetenv("LANGFUSE_PUBLIC_KEY")
	os.Unsetenv("LANGFUSE_SECRET_KEY")

	expectGlobalLangfuseSecret(mock, "LANGFUSE_HOST", "http://localhost:3000")
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM global_secrets WHERE key = \$1`).
		WithArgs("LANGFUSE_PUBLIC_KEY").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM global_secrets WHERE key = \$1`).
		WithArgs("LANGFUSE_SECRET_KEY").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-traces-partial"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-traces-partial/traces", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty list with partial config, got %d items", len(resp))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestTracesList_LangfuseUpstreamError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewTracesHandler()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("<html><body>Internal Server Error</body></html>"))
	}))
	defer upstream.Close()

	os.Setenv("LANGFUSE_HOST", upstream.URL)
	os.Setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
	os.Setenv("LANGFUSE_SECRET_KEY", "sk-test")
	defer func() {
		os.Unsetenv("LANGFUSE_HOST")
		os.Unsetenv("LANGFUSE_PUBLIC_KEY")
		os.Unsetenv("LANGFUSE_SECRET_KEY")
	}()

	expectMissingGlobalLangfuseSecrets(mock)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-traces-500"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-traces-500/traces", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty list on upstream error, got %d items", len(resp))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestTracesList_WorkspaceSecretsIgnored(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewTracesHandler()

	os.Unsetenv("LANGFUSE_HOST")
	os.Unsetenv("LANGFUSE_PUBLIC_KEY")
	os.Unsetenv("LANGFUSE_SECRET_KEY")

	expectMissingGlobalLangfuseSecrets(mock)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-traces-ssrf"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-traces-ssrf/traces", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty list when workspace secrets ignored, got %d items", len(resp))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func expectMissingGlobalLangfuseSecrets(mock sqlmock.Sqlmock) {
	for _, key := range []string{"LANGFUSE_HOST", "LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY"} {
		mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM global_secrets WHERE key = \$1`).
			WithArgs(key).
			WillReturnError(sql.ErrNoRows)
	}
}

func expectGlobalLangfuseSecret(mock sqlmock.Sqlmock, key, value string) {
	enc, _ := crypto.Encrypt([]byte(value))
	ver := crypto.CurrentEncryptionVersion()
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM global_secrets WHERE key = \$1`).
		WithArgs(key).
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).AddRow(enc, ver))
}
