package handlers

// llm_billing_mode_handler_test.go — admin route coverage for the per-workspace
// LLM billing mode endpoint (internal#691).
//
// What this guards:
//   - GET path validates UUID + returns the BillingModeResolution shape
//   - PUT distinguishes "omitted mode" (400) from "explicit null" (clear)
//     from "string value" (set), so a typo'd field name can't silently no-op
//   - Unknown mode strings 400 from the validator, not from a PG CHECK
//     constraint round-trip (matters because the error message must be
//     actionable to a canvas user)
//   - 404 propagates when the workspace row is missing on a set/clear

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

const testWSID = "44444444-4444-4444-4444-444444444444"

// expectDeriveShimQueries sets up the three reads the legacy-signature
// ResolveLLMBillingMode shim makes on a no-explicit-override path
// (internal#718 P2-B): the override read (NULL here), the workspaces.runtime
// read, and the workspace_secrets scan (for MODEL + auth-env names). model==""
// means no MODEL secret row.
func expectDeriveShimQueries(m sqlmock.Sqlmock, wsID, runtime, model string) {
	// Order: runtime, secrets, override(NULL). ResolveLLMBillingMode loads the
	// derive inputs first, then ResolveLLMBillingModeDerived reads the override
	// ONCE (the prior double-read was removed with the org-level path, 2026-06-12).
	m.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow(runtime))
	secretRows := sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"})
	if model != "" {
		// encryption_version 0 = plaintext passthrough (crypto.DecryptVersioned).
		secretRows.AddRow("MODEL", []byte(model), 0)
	}
	m.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1`).
		WithArgs(wsID).
		WillReturnRows(secretRows)
	m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(nil))
}

// internal#718 P2-B + core#2608: with no workspace override and an unset org
// default, the mode is DERIVED from its stored (runtime, model). A claude-code
// workspace with a non-platform-deriving model (kimi-for-coding) resolves byok
// via derived_provider.
func TestGetWorkspaceLLMBillingMode_HappyPath_DerivesByokFromModel(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BILLING_MODE", "") // no org default; derivation decides
	mock := setupTestDB(t)
	expectDeriveShimQueries(mock, testWSID, "claude-code", "kimi-for-coding")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	c.Request = httptest.NewRequest("GET", "/admin/workspaces/"+testWSID+"/llm-billing-mode", nil)

	GetWorkspaceLLMBillingMode(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	var res BillingModeResolution
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Errorf("resolved mode: got %q want %q", res.ResolvedMode, LLMBillingModeBYOK)
	}
	if res.Source != BillingModeSourceDerivedProvider {
		t.Errorf("source: got %q want %q", res.Source, BillingModeSourceDerivedProvider)
	}
	if res.WorkspaceOverride != nil {
		t.Errorf("expected nil override, got %v", *res.WorkspaceOverride)
	}
	if res.ProviderSelection == nil || *res.ProviderSelection != "kimi-coding" {
		t.Errorf("expected derived provider kimi-coding, got %v", res.ProviderSelection)
	}
}

func TestGetWorkspaceLLMBillingMode_BadUUID_400(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
	c.Request = httptest.NewRequest("GET", "/admin/workspaces/not-a-uuid/llm-billing-mode", nil)
	GetWorkspaceLLMBillingMode(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestPutWorkspaceLLMBillingMode_SetByok(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)
	mock := setupTestDB(t)
	mock.ExpectExec(`UPDATE workspaces SET llm_billing_mode = \$1 WHERE id = \$2`).
		WithArgs(LLMBillingModeBYOK, testWSID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Readback after write.
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(testWSID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeBYOK))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	body := `{"mode":"byok"}`
	c.Request = httptest.NewRequest("PUT",
		"/admin/workspaces/"+testWSID+"/llm-billing-mode",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	PutWorkspaceLLMBillingMode(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	var res BillingModeResolution
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Errorf("post-write resolved: got %q want %q", res.ResolvedMode, LLMBillingModeBYOK)
	}
	if res.Source != BillingModeSourceWorkspaceOverride {
		t.Errorf("post-write source: got %q want %q", res.Source, BillingModeSourceWorkspaceOverride)
	}
}

func TestPutWorkspaceLLMBillingMode_ExplicitNullClearsOverride(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BILLING_MODE", "") // no org default; derivation decides
	withProxyConfigured(t) // SaaS context: cleared override → derived_default → platform_managed.
	mock := setupTestDB(t)
	mock.ExpectExec(`UPDATE workspaces SET llm_billing_mode = NULL WHERE id = \$1`).
		WithArgs(testWSID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// After clear, the post-write re-resolution DERIVES (internal#718 P2-B):
	// no override + no MODEL secret → derived_default → platform_managed.
	expectDeriveShimQueries(mock, testWSID, "claude-code", "")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	body := `{"mode":null}`
	c.Request = httptest.NewRequest("PUT",
		"/admin/workspaces/"+testWSID+"/llm-billing-mode",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	PutWorkspaceLLMBillingMode(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	var res BillingModeResolution
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Errorf("post-clear resolved: got %q want %q", res.ResolvedMode, LLMBillingModePlatformManaged)
	}
	if res.Source != BillingModeSourceDerivedDefault {
		t.Errorf("post-clear source: got %q want %q", res.Source, BillingModeSourceDerivedDefault)
	}
	if res.WorkspaceOverride != nil {
		t.Errorf("post-clear override should be nil, got %v", *res.WorkspaceOverride)
	}
}

func TestPutWorkspaceLLMBillingMode_MissingModeField_400(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	body := `{}`
	c.Request = httptest.NewRequest("PUT",
		"/admin/workspaces/"+testWSID+"/llm-billing-mode",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	PutWorkspaceLLMBillingMode(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestPutWorkspaceLLMBillingMode_UnknownMode_400(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	body := `{"mode":"totally-bogus"}`
	c.Request = httptest.NewRequest("PUT",
		"/admin/workspaces/"+testWSID+"/llm-billing-mode",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	PutWorkspaceLLMBillingMode(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestPutWorkspaceLLMBillingMode_NoSuchWorkspace_404(t *testing.T) {
	mock := setupTestDB(t)
	// SET path: rows affected = 0 → SetWorkspaceLLMBillingMode returns sql.ErrNoRows
	// → handler maps to 404.
	mock.ExpectExec(`UPDATE workspaces SET llm_billing_mode = \$1 WHERE id = \$2`).
		WithArgs(LLMBillingModeBYOK, testWSID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	body := `{"mode":"byok"}`
	c.Request = httptest.NewRequest("PUT",
		"/admin/workspaces/"+testWSID+"/llm-billing-mode",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	PutWorkspaceLLMBillingMode(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404, body=%s", w.Code, w.Body.String())
	}
}
