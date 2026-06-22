package handlers

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func attachDisplayControlAdminToken(t *testing.T, c *gin.Context) {
	t.Helper()
	t.Setenv("ADMIN_TOKEN", "test-admin-secret")
	c.Request.Header.Set("Authorization", "Bearer test-admin-secret")
}

func TestWorkspaceDisplayControl_NoActiveLockReturnsNone(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = \$1 AND expires_at > now\(\)`).
		WithArgs("ws-display").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-display/display/control", nil)

	handler.DisplayControl(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["controller"] != "none" {
		t.Fatalf("controller = %v, want none", resp["controller"])
	}
	if _, ok := resp["expires_at"]; ok {
		t.Fatalf("none response included expires_at: %#v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlAcquire_ClaimsUnlockedDisplay(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "display-session-test-secret")
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	expiresAt := time.Date(2026, 5, 23, 18, 30, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"dcv","width":1920,"height":1080}}`))
	mock.ExpectQuery(`INSERT INTO workspace_display_control_locks`).
		WithArgs("ws-display", "user", "admin-token", 300).
		WillReturnRows(sqlmock.NewRows([]string{"controller", "controlled_by", "expires_at"}).
			AddRow("user", "admin-token", expiresAt))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/acquire", bytes.NewBufferString(`{"controller":"user","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	attachDisplayControlAdminToken(t, c)

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["controller"] != "user" || resp["controlled_by"] != "admin-token" {
		t.Fatalf("lock response = %#v, want user/admin-token", resp)
	}
	if resp["expires_at"] == "" {
		t.Fatalf("expires_at missing in response: %#v", resp)
	}
	sessionURL, ok := resp["session_url"].(string)
	if !ok || !strings.HasPrefix(sessionURL, "/workspaces/ws-display/display/session/websockify#token=") {
		t.Fatalf("session_url = %#v, want signed websockify URL fragment", resp["session_url"])
	}
	if strings.Contains(sessionURL, "?token=") {
		t.Fatalf("session_url must not put display token in logged query string: %q", sessionURL)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestDisplaySessionToken_RequiresDedicatedSigningSecret(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "client-exposed-admin-token")
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "")
	expiresAt := time.Now().Add(5 * time.Minute)

	if token := signDisplaySessionToken("ws-display", "admin-token", expiresAt); token != "" {
		t.Fatalf("signDisplaySessionToken minted token with no dedicated signing secret: %q", token)
	}

	payload := "ws-display|admin-token|" + strconv.FormatInt(expiresAt.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(""))
	_, _ = mac.Write([]byte(payload))
	forged := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if validateDisplaySessionToken(forged, "ws-display", "admin-token", expiresAt) {
		t.Fatal("validateDisplaySessionToken accepted empty-secret forged token")
	}
}

func TestWorkspaceDisplayControlAcquire_ActiveLockReturnsConflict(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "display-session-test-secret")
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	expiresAt := time.Date(2026, 5, 23, 18, 30, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"dcv","width":1920,"height":1080}}`))
	mock.ExpectQuery(`INSERT INTO workspace_display_control_locks`).
		WithArgs("ws-display", "user", "admin-token", 300).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = \$1 AND expires_at > now\(\)`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"controller", "controlled_by", "expires_at"}).
			AddRow("agent", "sidecar", expiresAt))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/acquire", bytes.NewBufferString(`{"controller":"user","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	attachDisplayControlAdminToken(t, c)

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status 409, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "display control already held" {
		t.Fatalf("error = %v, want display control already held", resp["error"])
	}
	current, ok := resp["current"].(map[string]interface{})
	if !ok || current["controller"] != "agent" || current["controlled_by"] != "sidecar" {
		t.Fatalf("current lock = %#v, want agent/sidecar", resp["current"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlAcquire_RejectsMissingSessionSigningSecret(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"dcv","width":1920,"height":1080}}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/acquire", bytes.NewBufferString(`{"controller":"user","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	attachDisplayControlAdminToken(t, c)
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "")

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlAcquire_RejectsDisplayDisabledWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-no-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-no-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-no-display/display/control/acquire", bytes.NewBufferString(`{"controller":"user","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	attachDisplayControlAdminToken(t, c)

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "display not enabled" {
		t.Fatalf("error = %v, want display not enabled", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlAcquire_RejectsCoarseSessionActor(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"dcv","width":1920,"height":1080}}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/acquire", bytes.NewBufferString(`{"controller":"user","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Cookie", "molecule_session=present")

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "display control requires admin-token or org-token auth" {
		t.Fatalf("error = %v, want display control requires admin-token or org-token auth", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlAcquire_AcceptsVerifiedBrowserSessionActor(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "display-session-test-secret")
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	expiresAt := time.Date(2026, 5, 23, 18, 30, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"dcv","width":1920,"height":1080}}`))
	mock.ExpectQuery(`INSERT INTO workspace_display_control_locks`).
		WithArgs("ws-display", "user", "session:abc123", 300).
		WillReturnRows(sqlmock.NewRows([]string{"controller", "controlled_by", "expires_at"}).
			AddRow("user", "session:abc123", expiresAt))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/acquire", bytes.NewBufferString(`{"controller":"user","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("cp_session_actor", "session:abc123")

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["controller"] != "user" || resp["controlled_by"] != "session:abc123" {
		t.Fatalf("lock response = %#v, want user/session actor", resp)
	}
	sessionURL, ok := resp["session_url"].(string)
	if !ok || !strings.HasPrefix(sessionURL, "/workspaces/ws-display/display/session/websockify#token=") {
		t.Fatalf("session_url = %#v, want signed websockify URL fragment", resp["session_url"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlRelease_RemovesCallerLock(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectExec(`DELETE FROM workspace_display_control_locks WHERE workspace_id = \$1 AND controlled_by = \$2`).
		WithArgs("ws-display", "admin-token").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/release", nil)
	attachDisplayControlAdminToken(t, c)

	handler.ReleaseDisplayControl(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["controller"] != "none" {
		t.Fatalf("controller = %v, want none", resp["controller"])
	}
	if _, ok := resp["expires_at"]; ok {
		t.Fatalf("none response included expires_at: %#v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlRelease_ConflictWhenCallerDoesNotOwnLock(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	expiresAt := time.Date(2026, 5, 23, 18, 30, 0, 0, time.UTC)

	mock.ExpectExec(`DELETE FROM workspace_display_control_locks WHERE workspace_id = \$1 AND controlled_by = \$2`).
		WithArgs("ws-display", "admin-token").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = \$1 AND expires_at > now\(\)`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"controller", "controlled_by", "expires_at"}).
			AddRow("user", "org-token:abcd1234", expiresAt))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/release", nil)
	attachDisplayControlAdminToken(t, c)

	handler.ReleaseDisplayControl(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status 409, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "display control held by another caller" {
		t.Fatalf("error = %v, want display control held by another caller", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlRelease_RejectsOrgTokenForceRelease(t *testing.T) {
	setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Set("org_token_prefix", "abcd1234")
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/release", bytes.NewBufferString(`{"force":true}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ReleaseDisplayControl(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "force release requires admin-token auth" {
		t.Fatalf("error = %v, want force release requires admin-token auth", resp["error"])
	}
}

func TestWorkspaceDisplayControlAcquire_RejectsAgentImpersonation(t *testing.T) {
	setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/acquire", bytes.NewBufferString(`{"controller":"agent","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	attachDisplayControlAdminToken(t, c)

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "browser callers may only acquire user display control" {
		t.Fatalf("error = %v, want browser callers may only acquire user display control", resp["error"])
	}
}
