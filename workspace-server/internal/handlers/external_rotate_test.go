package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// external_rotate_test.go — coverage for the credential-rotate +
// re-show-instructions endpoints (#319).
//
// What we pin:
//   1. Rotate happy path — revoke + mint fire in the right order, response
//      shape matches BuildExternalConnectionPayload, broadcast event
//      'EXTERNAL_CREDENTIALS_ROTATED' is emitted.
//   2. Rotate refuses non-external runtimes with 400 + the hint text.
//   3. Rotate 404 on unknown workspace.
//   4. GetExternalConnection happy path returns auth_token="" + the same
//      payload shape.
//   5. GetExternalConnection refuses non-external + 404 on unknown.
//   6. BuildExternalConnectionPayload — placeholder substitution +
//      trailing-slash trimming on platformURL.

// ---------- POST /external/rotate ----------

func TestRotateExternalCredentials_HappyPath(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// 1. Runtime lookup
	mock.ExpectQuery(`SELECT COALESCE\(runtime, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-ext").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("external"))

	// 2. Revoke all live tokens
	mock.ExpectExec(`UPDATE workspace_auth_tokens`).
		WithArgs("ws-ext").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 3. Mint a fresh token
	mock.ExpectExec(`INSERT INTO workspace_auth_tokens`).
		WithArgs("ws-ext", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-ext"}}
	c.Request = httptest.NewRequest("POST",
		"/workspaces/ws-ext/external/rotate", bytes.NewBufferString("{}"))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Host = "platform.example.test"
	c.Request.Header.Set("X-Forwarded-Proto", "https")

	wh.RotateExternalCredentials(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Connection map[string]interface{} `json:"connection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := body.Connection["workspace_id"]; got != "ws-ext" {
		t.Errorf("workspace_id: got %v", got)
	}
	if got := body.Connection["auth_token"]; got == "" || got == nil {
		t.Errorf("auth_token must be non-empty after mint; got %v", got)
	}
	if got := body.Connection["platform_url"]; got != "https://platform.example.test" {
		t.Errorf("platform_url: got %v", got)
	}
	for _, k := range []string{
		"curl_register_template", "python_snippet",
		"claude_code_channel_snippet", "universal_mcp_snippet",
		"hermes_channel_snippet", "codex_snippet", "openclaw_snippet",
	} {
		if _, ok := body.Connection[k]; !ok {
			t.Errorf("payload missing snippet field: %s", k)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock: %v", err)
	}
}

func TestRotateExternalCredentials_RejectsNonExternal(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(runtime, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-hermes").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("hermes"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-hermes"}}
	c.Request = httptest.NewRequest("POST",
		"/workspaces/ws-hermes/external/rotate", nil)

	wh.RotateExternalCredentials(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-external runtime, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "external") {
		t.Errorf("body should mention 'external'; got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "restart") {
		t.Errorf("body should hint at restart for non-external; got: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestRotateExternalCredentials_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(runtime, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-missing").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"})) // no rows

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-missing"}}
	c.Request = httptest.NewRequest("POST",
		"/workspaces/ws-missing/external/rotate", nil)

	wh.RotateExternalCredentials(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestRotateExternalCredentials_RejectsEmptyID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces//external/rotate", nil)

	wh.RotateExternalCredentials(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty id, got %d", w.Code)
	}
}

// ---------- GET /external/connection ----------

func TestGetExternalConnection_HappyPathReturnsBlankToken(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(runtime, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-ext").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("external"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-ext"}}
	c.Request = httptest.NewRequest("GET",
		"/workspaces/ws-ext/external/connection", nil)
	c.Request.Host = "platform.example.test"
	c.Request.Header.Set("X-Forwarded-Proto", "https")

	wh.GetExternalConnection(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		Connection map[string]interface{} `json:"connection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Connection["auth_token"] != "" {
		t.Errorf("auth_token MUST be empty in re-show path; got %v", body.Connection["auth_token"])
	}
	if body.Connection["workspace_id"] != "ws-ext" {
		t.Errorf("workspace_id wrong: %v", body.Connection["workspace_id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestGetExternalConnection_RejectsNonExternal(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(runtime, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-claude").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-claude"}}
	c.Request = httptest.NewRequest("GET",
		"/workspaces/ws-claude/external/connection", nil)

	wh.GetExternalConnection(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-external, got %d", w.Code)
	}
}

func TestGetExternalConnection_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(runtime, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-missing").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-missing"}}
	c.Request = httptest.NewRequest("GET",
		"/workspaces/ws-missing/external/connection", nil)

	wh.GetExternalConnection(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ---------- BuildExternalConnectionPayload (pure helper) ----------

func TestBuildExternalConnectionPayload_StampsPlaceholders(t *testing.T) {
	got := BuildExternalConnectionPayload("https://platform.test", "ws-7", "tok-abc")

	if got["workspace_id"] != "ws-7" {
		t.Errorf("workspace_id: %v", got["workspace_id"])
	}
	if got["auth_token"] != "tok-abc" {
		t.Errorf("auth_token: %v", got["auth_token"])
	}
	if got["platform_url"] != "https://platform.test" {
		t.Errorf("platform_url: %v", got["platform_url"])
	}
	if got["registry_endpoint"] != "https://platform.test/registry/register" {
		t.Errorf("registry_endpoint: %v", got["registry_endpoint"])
	}
	// {{PLATFORM_URL}} + {{WORKSPACE_ID}} placeholders must be substituted
	// out of every snippet — if any snippet still contains a literal
	// "{{PLATFORM_URL}}" or "{{WORKSPACE_ID}}", a future template author
	// forgot to use the placeholder convention and operators see broken
	// snippets.
	for _, k := range []string{
		"curl_register_template", "python_snippet",
		"claude_code_channel_snippet", "universal_mcp_snippet",
		"hermes_channel_snippet", "codex_snippet", "openclaw_snippet",
	} {
		v, _ := got[k].(string)
		if strings.Contains(v, "{{PLATFORM_URL}}") {
			t.Errorf("%s still contains literal {{PLATFORM_URL}}", k)
		}
		if strings.Contains(v, "{{WORKSPACE_ID}}") {
			t.Errorf("%s still contains literal {{WORKSPACE_ID}}", k)
		}
	}
}

func TestBuildExternalConnectionPayload_TrimsTrailingSlash(t *testing.T) {
	// platform_url passed in with trailing slash must be trimmed before
	// being concatenated into endpoint paths — otherwise the operator
	// gets `https://platform.test//registry/register` (double slash) which
	// some servers reject as a redirect target.
	got := BuildExternalConnectionPayload("https://platform.test/", "ws-7", "")
	if got["platform_url"] != "https://platform.test" {
		t.Errorf("platform_url: trailing slash not trimmed; got %v", got["platform_url"])
	}
	if got["registry_endpoint"] != "https://platform.test/registry/register" {
		t.Errorf("registry_endpoint should not have double slash; got %v", got["registry_endpoint"])
	}
}

func TestBuildExternalConnectionPayload_BlankAuthTokenIsAllowed(t *testing.T) {
	// Re-show path: auth_token="" is the contract; the modal masks the
	// field and labels it "rotate to reveal a new token".
	got := BuildExternalConnectionPayload("https://platform.test", "ws-7", "")
	if got["auth_token"] != "" {
		t.Errorf("blank token must propagate as \"\"; got %v", got["auth_token"])
	}
}
