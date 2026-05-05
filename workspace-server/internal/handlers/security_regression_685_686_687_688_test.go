package handlers

// security_regression_685_686_687_688_test.go — regression suite for the
// input-validation security fixes shipped in PR #701.
//
//   #686 — GET /templates and GET /org/templates now require AdminAuth
//   #687 — UUID validation on workspace :id path params (invalid UUID → 400)
//   #688 — Field length limits: name≤255, role≤1000, model/runtime≤100
//   #685 — YAML injection: newline/CR characters rejected in name/role/model/runtime
//
// These tests are intentionally kept at the handler layer (not full router)
// for fast CI execution. The template auth tests are the exception — they wire
// AdminAuth middleware into a mini gin router to verify the actual security gate
// rather than the handler's internal logic.

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/middleware"
	"github.com/gin-gonic/gin"
)

// authTokenQuery matches the SELECT issued by HasAnyLiveTokenGlobal inside AdminAuth.
const authTokenQuery = "SELECT COUNT.*workspace_auth_tokens"

// newEnrolledAuthDB returns a sqlmock DB pre-loaded so that the next
// HasAnyLiveTokenGlobal call reports one enrolled workspace (i.e., auth is enforced).
// The returned Sqlmock lets the caller verify expectations afterwards.
func newEnrolledAuthDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	d, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	m.ExpectQuery(authTokenQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	return d, m
}

// newFreshInstallAuthDB returns a sqlmock DB where HasAnyLiveTokenGlobal
// reports zero enrolled workspaces — the platform is in fail-open bootstrap mode.
func newFreshInstallAuthDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	d, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	m.ExpectQuery(authTokenQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	return d, m
}

// ─────────────────────────────────────────────────────────────────────────────
// #686 — AdminAuth gate on GET /templates
// ─────────────────────────────────────────────────────────────────────────────

// TestSecurity_GetTemplates_NoAuth_Returns401 verifies that once at least one
// workspace is enrolled (tokens exist), GET /templates without a bearer token
// is rejected with 401. Previously the route was unauthenticated (#686).
func TestSecurity_GetTemplates_NoAuth_Returns401(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	authDB, authMock := newEnrolledAuthDB(t)

	tmpDir := t.TempDir()
	tmplh := NewTemplatesHandler(tmpDir, nil, nil)

	r := gin.New()
	r.GET("/templates", middleware.AdminAuth(authDB), tmplh.List)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/templates", nil)
	// Deliberately omit Authorization header — must be rejected.
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#686 GET /templates no-auth: want 401, got %d body=%s", w.Code, w.Body.String())
	}
	if err := authMock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet auth mock expectations: %v", err)
	}
}

// TestSecurity_GetTemplates_FreshInstall_FailsOpen verifies that GET /templates
// still succeeds on a fresh install (zero enrolled workspaces → AdminAuth fail-open).
// This is the regression check: the auth gate must not break new deployments.
func TestSecurity_GetTemplates_FreshInstall_FailsOpen(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	authDB, authMock := newFreshInstallAuthDB(t)

	tmpDir := t.TempDir()
	tmplh := NewTemplatesHandler(tmpDir, nil, nil)

	r := gin.New()
	r.GET("/templates", middleware.AdminAuth(authDB), tmplh.List)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/templates", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("#686 GET /templates fresh-install: want 200 (fail-open), got %d body=%s", w.Code, w.Body.String())
	}
	if err := authMock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet auth mock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// #686 — AdminAuth gate on GET /org/templates
// ─────────────────────────────────────────────────────────────────────────────

// TestSecurity_GetOrgTemplates_NoAuth_Returns401 verifies that GET /org/templates
// requires a bearer token once the platform has enrolled workspaces.
// Previously the route was unauthenticated, exposing org structure details (#686).
func TestSecurity_GetOrgTemplates_NoAuth_Returns401(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	authDB, authMock := newEnrolledAuthDB(t)

	tmpDir := t.TempDir()
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", tmpDir)
	orgh := NewOrgHandler(wh, newTestBroadcaster(), nil, nil, tmpDir, tmpDir)

	r := gin.New()
	r.GET("/org/templates", middleware.AdminAuth(authDB), orgh.ListTemplates)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/org/templates", nil)
	// No Authorization header — must be rejected.
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#686 GET /org/templates no-auth: want 401, got %d body=%s", w.Code, w.Body.String())
	}
	if err := authMock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet auth mock expectations: %v", err)
	}
}

// TestSecurity_GetOrgTemplates_FreshInstall_FailsOpen mirrors the /templates
// regression check for /org/templates — fresh installs must still work.
func TestSecurity_GetOrgTemplates_FreshInstall_FailsOpen(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	authDB, authMock := newFreshInstallAuthDB(t)

	tmpDir := t.TempDir()
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", tmpDir)
	orgh := NewOrgHandler(wh, newTestBroadcaster(), nil, nil, tmpDir, tmpDir)

	r := gin.New()
	r.GET("/org/templates", middleware.AdminAuth(authDB), orgh.ListTemplates)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/org/templates", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("#686 GET /org/templates fresh-install: want 200 (fail-open), got %d body=%s", w.Code, w.Body.String())
	}
	if err := authMock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet auth mock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// #687 — UUID validation on workspace :id path params
// ─────────────────────────────────────────────────────────────────────────────

// TestSecurity_Get_URLEncodedTraversal_Returns400 verifies that a URL-encoded
// path traversal sequence — the type a browser or curl submits as
// /workspaces/..%252f..%252fetc%252fpasswd (double-encoded → decoded to
// ..%2f..%2fetc%2fpasswd by the HTTP layer) — is rejected 400 before any DB
// query. Previously a non-UUID id caused a Postgres syntax error → 500.
func TestSecurity_Get_URLEncodedTraversal_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// gin decodes %25 → %, so the outer HTTP layer hands the handler this value.
	traversalID := "..%2f..%2fetc%2fpasswd"

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: traversalID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+traversalID, nil)

	handler.Get(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("#687 URL-encoded traversal Get(%q): want 400, got %d body=%s",
			traversalID, w.Code, w.Body.String())
	}
}

// TestSecurity_Get_NotUUID_Returns400 checks the simplest non-UUID rejection.
func TestSecurity_Get_NotUUID_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	for _, badID := range []string{
		"not-a-uuid",
		"ws-123",
		"123",
		"../etc/passwd",
		"<script>alert(1)</script>",
	} {
		t.Run(badID, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: badID}}
			c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+badID, nil)
			handler.Get(c)
			if w.Code != http.StatusBadRequest {
				t.Errorf("#687 Get(%q): want 400, got %d", badID, w.Code)
			}
		})
	}
}

// TestSecurity_ValidUUID_PassesUUIDValidation verifies that a well-formed UUID
// passes the validateWorkspaceID guard — i.e., the fix doesn't false-positive
// on legitimate workspace IDs.
func TestSecurity_ValidUUID_PassesUUIDValidation(t *testing.T) {
	if err := validateWorkspaceID("550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Errorf("regression: valid UUID rejected: %v", err)
	}
	if err := validateWorkspaceID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); err != nil {
		t.Errorf("regression: valid UUID rejected: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// #688 — Field length limits on POST /workspaces
// ─────────────────────────────────────────────────────────────────────────────

// TestSecurity_Create_NameTooLong_Returns400 verifies a 256-character name is
// rejected before any DB interaction. The limit is 255 characters (#688).
func TestSecurity_Create_NameTooLong_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	name256 := strings.Repeat("a", 256)
	body := `{"name":"` + name256 + `"}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("#688 name=256 chars: want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestSecurity_Create_RoleTooLong_Returns400 verifies a 1001-character role is
// rejected. The limit is 1000 characters (#688).
func TestSecurity_Create_RoleTooLong_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	role1001 := strings.Repeat("r", 1001)
	body := `{"name":"valid-name","role":"` + role1001 + `"}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("#688 role=1001 chars: want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestSecurity_Create_ModelTooLong_Returns400 verifies a 101-character model
// is rejected (#688). The limit is 100 characters.
func TestSecurity_Create_ModelTooLong_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	model101 := strings.Repeat("m", 101)
	body := `{"name":"valid-name","model":"` + model101 + `"}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("#688 model=101 chars: want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// #685 — YAML injection: newline/CR rejection
// ─────────────────────────────────────────────────────────────────────────────

// TestSecurity_Create_NameWithNewline_Returns400 verifies that a workspace name
// containing a literal newline character is rejected before DB interaction.
// Newlines break YAML multi-line quoting even with yamlQuote escaping (#685).
func TestSecurity_Create_NameWithNewline_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// JSON \n is a literal newline in the parsed string value.
	body := `{"name":"bad\nname"}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("#685 name with \\n: want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestSecurity_Create_YAMLInjectionViaNewline_Returns400 verifies that a
// workspace name crafted to inject YAML fields via a newline is caught by the
// newline-rejection gate before reaching the provisioner.
//
// The attack string "agent\nrole: injected_value" would, if written unquoted
// into a YAML config, silently set the role field to "injected_value". The
// newline is the injection vector — it is rejected by #685.
//
// Note: curly-brace injection like "{inject: yaml}" does not contain newlines
// and is handled separately by yamlQuote escaping in the provisioner
// (defence-in-depth). That value is intentionally allowed through here and
// must be tested against the provisioner's yamlQuote output, not this gate.
func TestSecurity_Create_YAMLInjectionViaNewline_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// The injected string breaks out of a YAML scalar via newline.
	body := "{\"name\":\"agent\\nrole: injected_value\"}"

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("#685 YAML injection via \\n: want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestSecurity_Create_RoleWithCR_Returns400 verifies carriage-return rejection
// in the role field (#685). CR alone can also break YAML multi-line values.
func TestSecurity_Create_RoleWithCR_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	body := "{\"name\":\"ok\",\"role\":\"bad\\rrole\"}"

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("#685 role with \\r: want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Regression: validateWorkspaceFields direct unit coverage
// ─────────────────────────────────────────────────────────────────────────────

// TestSecurity_ValidateWorkspaceFields_BoundaryValues exercises exact-boundary
// values for all four field limits to ensure the fence posts are correct.
// These are regression checks: fixing the upper limits must not accidentally
// tighten or loosen the constraint by ±1.
func TestSecurity_ValidateWorkspaceFields_BoundaryValues(t *testing.T) {
	cases := []struct {
		label           string
		name            string
		role            string
		model           string
		runtime         string
		wantErr         bool
	}{
		// Exact maximum lengths — must PASS.
		{"name_at_255", strings.Repeat("a", 255), "", "", "", false},
		{"role_at_1000", "", strings.Repeat("r", 1000), "", "", false},
		{"model_at_100", "", "", strings.Repeat("m", 100), "", false},
		{"runtime_at_100", "", "", "", strings.Repeat("x", 100), false},
		// One over the limit — must FAIL.
		{"name_at_256", strings.Repeat("a", 256), "", "", "", true},
		{"role_at_1001", "", strings.Repeat("r", 1001), "", "", true},
		{"model_at_101", "", "", strings.Repeat("m", 101), "", true},
		{"runtime_at_101", "", "", "", strings.Repeat("x", 101), true},
		// Newline/CR in each field — must FAIL.
		{"name_newline", "a\nb", "", "", "", true},
		{"role_cr", "", "a\rb", "", "", true},
		{"model_newline", "", "", "a\nb", "", true},
		{"runtime_newline", "", "", "", "a\nb", true},
		// Fully valid — must PASS.
		{"all_valid", "My Agent", "You are a helpful agent.", "claude-opus-4-7", "langgraph", false},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			err := validateWorkspaceFields(tc.name, tc.role, tc.model, tc.runtime)
			if tc.wantErr && err == nil {
				t.Errorf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("want nil, got %v", err)
			}
		})
	}
}

// TestSecurity_ValidateWorkspaceID_ValidUUIDs verifies that real workspace UUIDs
// (RFC 4122 v4) are accepted. Regression check: the fix must not reject valid IDs.
func TestSecurity_ValidateWorkspaceID_ValidUUIDs(t *testing.T) {
	valid := []string{
		"550e8400-e29b-41d4-a716-446655440000", // RFC 4122 example
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"00000000-0000-0000-0000-000000000000",
		"dddddddd-0001-0000-0000-000000000000", // used in other handler tests
	}
	for _, id := range valid {
		if err := validateWorkspaceID(id); err != nil {
			t.Errorf("regression: valid UUID %q rejected: %v", id, err)
		}
	}
}

// TestSecurity_ValidateWorkspaceID_InvalidIDs checks that non-UUID strings all
// return errors from validateWorkspaceID.
func TestSecurity_ValidateWorkspaceID_InvalidIDs(t *testing.T) {
	invalid := []string{
		"not-a-uuid",
		"ws-abc",
		"",
		"../etc/passwd",
		"..%2f..%2fetc%2fpasswd",
		"<script>",
		"1",
		"00000000-0000-0000-0000", // too short
	}
	for _, id := range invalid {
		if err := validateWorkspaceID(id); err == nil {
			t.Errorf("expected error for id %q, got nil", id)
		}
	}
}
