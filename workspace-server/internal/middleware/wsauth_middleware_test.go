package middleware

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ────────────────────────────────────────────────────────────────────────────
// WorkspaceAuth middleware tests (covers findings C4, C8 and the full
// per-workspace bearer-token contract).
//
// Since PR #357 (#351 fix) the middleware enforces strictly: every request
// under /workspaces/:id/* must carry a valid bearer token — no fail-open,
// no grace period, no existence check.
// ────────────────────────────────────────────────────────────────────────────

// hasAnyLiveTokenGlobalQuery is matched for HasAnyLiveTokenGlobal.
const hasAnyLiveTokenGlobalQuery = "SELECT COUNT.*FROM workspace_auth_tokens"

// validateTokenQuery is matched for ValidateToken (SELECT).
const validateTokenSelectQuery = "SELECT t\\.id, t\\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces"

// validateAnyTokenQuery is matched for ValidateAnyToken (SELECT).
// The JOIN on workspaces filters removed-workspace tokens (#682 defense-in-depth).
// Identical to validateTokenSelectQuery because both go through the
// shared lookupTokenByHash helper (projects (id, workspace_id)).
const validateAnyTokenSelectQuery = "SELECT t\\.id, t\\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces"

// validateTokenUpdateQuery is matched for the best-effort last_used_at UPDATE.
const validateTokenUpdateQuery = "UPDATE workspace_auth_tokens SET last_used_at"

// newWorkspaceAuthRouter builds a minimal gin router that applies WorkspaceAuth
// to a single GET /workspaces/:id/test route, returning 200 on success.
func newWorkspaceAuthRouter(db sqlmock.Sqlmock, realDB interface{ Close() error }) *gin.Engine {
	_ = db  // unused directly; sqlmock intercepts calls via the *sql.DB pointer
	r := gin.New()
	// We need the *sql.DB, not the mock. The caller passes mockDB via the
	// test-local var — this helper is only used to build the router topology.
	return r
}

// TestWorkspaceAuth_351_NoBearer_Returns401 — strict contract: every request
// under /workspaces/:id/* must carry a valid bearer, period. No fail-open,
// no grace period, no existence check. The middleware goes straight to
// ValidateToken and 401s when the bearer is missing — no DB calls happen.
// #351 closed the last fail-open hole (zombie workspaces with no tokens);
// this test pins the strict behaviour.
func TestWorkspaceAuth_351_NoBearer_Returns401_NoDBCalls(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// NO expected queries — strict path short-circuits before any DB call.

	r := gin.New()
	r.GET("/workspaces/:id/secrets", WorkspaceAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// Any UUID — fake, legit, zombie — all must 401 without a bearer.
	for _, id := range []string{
		"00000000-0000-0000-0000-000000000000", // fake UUID (#318 scenario)
		"ffffffff-ffff-ffff-ffff-ffffffffffff", // zombie test-artifact (#351)
		"ws-bootstrap",                         // legitimate pre-token (was grace-period)
	} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/workspaces/"+id+"/secrets", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("no-bearer %q: expected 401, got %d: %s", id, w.Code, w.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceAuth_C4_C8_NoBearer_Returns401 — C4/C8 critical path:
// when a workspace has live tokens and the caller sends NO bearer token,
// the middleware must return 401.  This was the confirmed attack vector —
// unauthenticated POSTs to /delegations/:id/update and /memories succeeded.
func TestWorkspaceAuth_C4_C8_NoBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// #351: no DB calls expected — strict path short-circuits at missing bearer.

	r := gin.New()
	r.POST("/workspaces/:id/delegations/:delegation_id/update",
		WorkspaceAuth(mockDB),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	// C4 attack: no Authorization header.
	req, _ := http.NewRequest(http.MethodPost,
		"/workspaces/ws-enrolled/delegations/del-1/update", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("C4 no-bearer: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceAuth_C8_MemoriesCommit_NoBearer_Returns401 tests specifically
// the C8 vector: POST /workspaces/:id/memories without auth.
func TestWorkspaceAuth_C8_MemoriesCommit_NoBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// #351: no DB calls expected — strict path short-circuits at missing bearer.

	r := gin.New()
	r.POST("/workspaces/:id/memories",
		WorkspaceAuth(mockDB),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost,
		"/workspaces/ws-memory-target/memories", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("C8 no-bearer: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceAuth_InvalidBearer_Returns401 — wrong token must be rejected.
func TestWorkspaceAuth_InvalidBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// ValidateToken: hash doesn't match any live row.
	wrongToken := "wrong-token-value"
	wrongHash := sha256.Sum256([]byte(wrongToken))
	mock.ExpectQuery(validateTokenSelectQuery).
		WithArgs(wrongHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"})) // empty → ErrNoRows

	r := gin.New()
	r.GET("/workspaces/:id/test",
		WorkspaceAuth(mockDB),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/workspaces/ws-enrolled/test", nil)
	req.Header.Set("Authorization", "Bearer "+wrongToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("invalid bearer: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceAuth_ValidBearer_Passes — correct token for the right workspace
// must be accepted and the handler reached (200).
func TestWorkspaceAuth_ValidBearer_Passes(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	testToken := "valid-workspace-bearer-token-abc123"
	tokenHash := sha256.Sum256([]byte(testToken))

	// ValidateToken SELECT — returns matching token_id + workspace_id.
	mock.ExpectQuery(validateTokenSelectQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).
			AddRow("tok-1", "ws-enrolled"))

	// Best-effort last_used_at UPDATE (ignored on error, but we expect it).
	mock.ExpectExec(validateTokenUpdateQuery).
		WithArgs("tok-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	r := gin.New()
	r.POST("/workspaces/:id/memories",
		WorkspaceAuth(mockDB),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/workspaces/ws-enrolled/memories", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("valid bearer: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceAuth_WrongWorkspace_Returns401 — token valid for workspace A must
// not authenticate workspace B (cross-workspace token replay attack).
func TestWorkspaceAuth_WrongWorkspace_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	tokenForA := "token-issued-to-workspace-a"
	tokenHash := sha256.Sum256([]byte(tokenForA))

	// ValidateToken SELECT returns workspace-a from DB — strict middleware
	// catches the mismatch via the workspace-binding check in wsauth.ValidateToken.
	mock.ExpectQuery(validateTokenSelectQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).
			AddRow("tok-a", "workspace-a")) // workspace mismatch!

	r := gin.New()
	r.GET("/workspaces/:id/test",
		WorkspaceAuth(mockDB),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/workspaces/workspace-b/test", nil)
	req.Header.Set("Authorization", "Bearer "+tokenForA)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("cross-workspace replay: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// AdminAuth middleware tests (covers findings C10, C11 and the
// global bearer-token contract for /admin/secrets, /settings/secrets).
// ────────────────────────────────────────────────────────────────────────────

// TestAdminAuth_FailOpen_NoTokensGlobally — C10/C11: on a fresh install (no
// live tokens anywhere) the middleware must let the request through so existing
// deployments keep working during the Phase-30 rollout.
func TestAdminAuth_FailOpen_NoTokensGlobally(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// HasAnyLiveTokenGlobal returns 0 — fresh install.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	r := gin.New()
	r.GET("/admin/secrets", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/secrets", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("C10 fail-open (no global tokens): expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_C10_NoBearer_Returns401 — C10 critical path: when at least
// one workspace has tokens, GET /admin/secrets without a bearer → 401.
func TestAdminAuth_C10_NoBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// HasAnyLiveTokenGlobal returns 1 — platform has at least one enrolled workspace.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := gin.New()
	r.GET("/admin/secrets", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"secrets": []string{"GITHUB_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}})
	})

	w := httptest.NewRecorder()
	// C10 attack: no Authorization header — must not leak secrets.
	req, _ := http.NewRequest(http.MethodGet, "/admin/secrets", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("C10 no-bearer: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_C11_PostNoBearer_Returns401 — C11 critical path: env poisoning
// via POST /admin/secrets without auth must be rejected.
func TestAdminAuth_C11_PostNoBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := gin.New()
	r.POST("/admin/secrets", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/admin/secrets", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("C11 POST no-bearer: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_C11_DeleteNoBearer_Returns401 — C11: DELETE /admin/secrets/:key
// without auth must be rejected (env poisoning → RCE on agent restart).
func TestAdminAuth_C11_DeleteNoBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := gin.New()
	r.DELETE("/admin/secrets/:key", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/admin/secrets/CLAUDE_CODE_OAUTH_TOKEN", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("C11 DELETE no-bearer: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_ValidBearer_Passes — a valid bearer token (from any workspace)
// must be accepted for admin routes.
func TestAdminAuth_ValidBearer_Passes(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	adminToken := "admin-bearer-token-from-any-workspace"
	tokenHash := sha256.Sum256([]byte(adminToken))

	// HasAnyLiveTokenGlobal: tokens exist.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	// ValidateAnyToken SELECT — token matches a live row.
	mock.ExpectQuery(validateAnyTokenSelectQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-admin-1", "ws-admin"))

	// Best-effort last_used_at UPDATE.
	mock.ExpectExec(validateTokenUpdateQuery).
		WithArgs("tok-admin-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	r := gin.New()
	r.GET("/admin/secrets", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("C10 valid bearer: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_InvalidBearer_Returns401 — wrong token must not grant admin access.
func TestAdminAuth_InvalidBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	wrongToken := "completely-wrong-token"
	wrongHash := sha256.Sum256([]byte(wrongToken))

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	// ValidateAnyToken SELECT — no matching row.
	mock.ExpectQuery(validateAnyTokenSelectQuery).
		WithArgs(wrongHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // empty → ErrNoRows

	r := gin.New()
	r.GET("/admin/secrets", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+wrongToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("C10 invalid bearer: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// F1097 regression — org-scoped token Validate() must also set org_id in context
//
// Before PR #1210 (fix/org-api-token-org-id-column), org tokens had no org_id
// column so requireCallerOwnsOrg fell back to created_by lookup. After PR #1210,
// requireCallerOwnsOrg queries org_api_tokens.org_id directly — but if
// c.Set("org_id", ...) is never called, orgCallerID() always returns "" and
// all token callers are denied org-scoped access even within their own org.
//
// The fix (wsauth_middleware.go): after orgtoken.Validate succeeds, also look up
// the token's org_id column and set it in the context. This test verifies the
// middleware sets org_id for a pre-fix token (org_id=NULL) and a post-fix
// token (org_id="ws-org-1").
// ────────────────────────────────────────────────────────────────────────────

// orgTokenValidateQueryV1 is matched for orgtoken.Validate().
// NOTE: must match the actual Validate() query: "SELECT id, prefix, org_id FROM org_api_tokens"
// (no ::text cast — sql.NullString handles the NULL scan natively).
const orgTokenValidateQueryV1 = "SELECT id, prefix, org_id FROM org_api_tokens"

// orgTokenOrgIDQuery is deprecated — org_id is now returned by the primary Validate query.
// Kept here to avoid breaking other test files that may reference it.
const orgTokenOrgIDQuery = "SELECT org_id::text FROM org_api_tokens"

// orgTokenLastUsedQuery is matched for the best-effort last_used_at UPDATE.
const orgTokenLastUsedQuery = "UPDATE org_api_tokens SET last_used_at"

// TestAdminAuth_OrgToken_SetsOrgID verifies that AdminAuth's org-token tier
// reads the org_id column and sets it in the gin context so that requireCallerOwnsOrg
// and orgCallerID can look it up downstream.
func TestAdminAuth_OrgToken_SetsOrgID(t *testing.T) {
	tests := []struct {
		name          string
		orgIDFromDB   interface{} // sqlmock row value: nil, "", or "ws-org-1"
		wantOrgIDCtx  bool        // expect c.Get("org_id") to be set
		wantOrgIDVal  string      // if set, expected value
	}{
		{
			name:         "post-fix token has org_id set in context",
			orgIDFromDB:  "ws-org-1",
			wantOrgIDCtx: true,
			wantOrgIDVal: "ws-org-1",
		},
		{
			name:         "pre-fix token (org_id=NULL) — no org_id set in context",
			orgIDFromDB:  nil,
			wantOrgIDCtx: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer mockDB.Close()

			orgBearer := "valid-org-token"
			orgTokenHash := sha256.Sum256([]byte(orgBearer))

			// HasAnyLiveTokenGlobal: tokens exist.
			mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

			// orgtoken.Validate: org token hash matches, returns id + prefix + org_id.
			// The org_id is returned directly from the primary query.
			// Note: org tokens are checked BEFORE the workspace token path
			// (ValidateAnyToken), so ValidateAnyToken is NOT called here.
			mock.ExpectQuery(orgTokenValidateQueryV1).
				WithArgs(orgTokenHash[:]).
				WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id"}).
					AddRow("tok-org-1", "tok-org-1", tt.orgIDFromDB))

			// Best-effort last_used_at UPDATE (after Validate).
			mock.ExpectExec(orgTokenLastUsedQuery).
				WithArgs("tok-org-1").
				WillReturnResult(sqlmock.NewResult(0, 1))

			r := gin.New()
			var gotOrgID string
			var haveOrgID bool
			r.GET("/admin/org/tokens", AdminAuth(mockDB), func(c *gin.Context) {
				if v, ok := c.Get("org_id"); ok {
					if s, ok := v.(string); ok {
						gotOrgID = s
						haveOrgID = true
					}
				}
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodGet, "/admin/org/tokens", nil)
			req.Header.Set("Authorization", "Bearer "+orgBearer)
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			if haveOrgID != tt.wantOrgIDCtx {
				t.Errorf("c.Get(\"org_id\") present = %v, want %v", haveOrgID, tt.wantOrgIDCtx)
			}
			if tt.wantOrgIDCtx && gotOrgID != tt.wantOrgIDVal {
				t.Errorf("org_id = %q, want %q", gotOrgID, tt.wantOrgIDVal)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Issue #170 regression — unauthenticated DELETE /workspaces/:id/secrets/:key
//
// Before this fix, the route was registered as:
//   r.DELETE("/workspaces/:id/secrets/:key", sech.Delete)
// on the bare Gin router — no auth at all.  Any caller could delete a secret
// AND trigger a forced workspace restart (the handler calls go restartFunc(id)
// on every successful delete).  CWE-306.
//
// The fix: move the route inside the wsAuth group so it matches all other
// /workspaces/:id/secrets mutations (POST + PUT are already auth-gated).
// ────────────────────────────────────────────────────────────────────────────

// TestWorkspaceAuth_Issue170_SecretDelete_NoBearer_Returns401 is the primary
// regression test: when the workspace has live tokens, a DELETE /secrets/:key
// without a bearer token must be rejected with 401.
func TestWorkspaceAuth_Issue170_SecretDelete_NoBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// #351: strict path — no DB calls on missing bearer.

	r := gin.New()
	// Mirror the fix: DELETE /secrets/:key is inside the wsAuth group.
	r.DELETE("/workspaces/:id/secrets/:key",
		WorkspaceAuth(mockDB),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "deleted"}) })

	w := httptest.NewRecorder()
	// #170 attack: no Authorization header.
	req, _ := http.NewRequest(http.MethodDelete,
		"/workspaces/ws-secret-owner/secrets/OPENAI_API_KEY", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#170 secret delete no-bearer: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceAuth_Issue170_SecretDelete_NoTokensStillRejected — #351: the
// former "fail-open for tokenless workspaces" path is gone. Even a legitimate
// pre-Phase-30.1 workspace now must present a bearer; otherwise 401. This
// closes the zombie-workspace secret-enumeration vector.
func TestWorkspaceAuth_Issue170_SecretDelete_NoTokensStillRejected(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// #351: no DB calls on missing bearer.

	r := gin.New()
	r.DELETE("/workspaces/:id/secrets/:key",
		WorkspaceAuth(mockDB),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "deleted"}) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete,
		"/workspaces/ws-legacy/secrets/SOME_KEY", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#351 tokenless-no-bearer: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Issue #120 regression — unauthenticated PATCH /workspaces/:id
//
// Before PR #125, PATCH /workspaces/:id was registered outside the wsAdmin
// group and did NOT enforce AdminAuth.  An attacker could change workspace
// name, tier, parent_id, runtime, or workspace_dir without any token.
// Security Auditor confirmed the live exploit:
//   curl -X PATCH .../workspaces/00000000-.../  -d '{"name":"probe"}' → 200
//
// This test asserts AdminAuth applied to the PATCH route blocks unauthenticated
// requests — the route-level fix in router.go is the enforcement point.
// ────────────────────────────────────────────────────────────────────────────

// ────────────────────────────────────────────────────────────────────────────
// Issue #180 regression — unauthenticated GET /approvals/pending
//
// GET /approvals/pending was registered on the open router (no middleware)
// and returned all pending approvals across every workspace to any caller,
// with no token required.
// Attack vector confirmed by Security Auditor:
//   curl http://host/approvals/pending → 200 with full cross-workspace list
//
// Fixed by adding inline AdminAuth to the route registration in router.go.
// This test asserts the gate blocks unauthenticated reads.
// ────────────────────────────────────────────────────────────────────────────

// TestAdminAuth_Issue180_ApprovalsListing_NoBearer_Returns401 documents the #180
// attack vector and verifies that AdminAuth returns 401 for GET without a token.
func TestAdminAuth_Issue180_ApprovalsListing_NoBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// HasAnyLiveTokenGlobal returns 1 — at least one workspace is token-enrolled.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := gin.New()
	// Mirror the router.go fix: GET /approvals/pending is behind AdminAuth.
	r.GET("/approvals/pending", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"approvals": []interface{}{}})
	})

	w := httptest.NewRecorder()
	// #180 attack: no Authorization header on GET /approvals/pending.
	req, _ := http.NewRequest(http.MethodGet, "/approvals/pending", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#180 GET /approvals/pending no-bearer: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_Issue180_ApprovalsListing_FailOpen_NoTokens documents the
// fail-open contract: on a fresh install (no tokens anywhere), the middleware
// must not block the canvas from polling /approvals/pending.
func TestAdminAuth_Issue180_ApprovalsListing_FailOpen_NoTokens(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// HasAnyLiveTokenGlobal returns 0 — fresh install, no tokens yet.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	r := gin.New()
	r.GET("/approvals/pending", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"approvals": []interface{}{}})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/approvals/pending", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("#180 fail-open (no tokens): expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceAuth_DevModeEscapeHatch_NoBearer_FailsOpen documents the
// local-dev escape hatch on WorkspaceAuth. On `go run ./cmd/server` +
// `npm run dev`, Canvas at localhost:3000 calls the platform at
// localhost:8080 cross-port, so isSameOriginCanvas's Host==Referer
// check fails. Without this hatch the Canvas can't show per-workspace
// activity/delegations.
//
// SaaS never fires this branch because tenant provisioning sets both
// MOLECULE_ENV=production and ADMIN_TOKEN.
func TestWorkspaceAuth_DevModeEscapeHatch_NoBearer_FailsOpen(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "development")
	t.Setenv("ADMIN_TOKEN", "")

	mockDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// No DB queries expected — the hatch short-circuits before any lookup.

	r := gin.New()
	r.GET("/workspaces/:id/activity", WorkspaceAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"activity": []interface{}{}})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet,
		"/workspaces/00000000-0000-0000-0000-000000000000/activity", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("WorkspaceAuth dev-mode hatch: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceAuth_DevModeEscapeHatch_IgnoredInProduction verifies
// the hatch never fires in production mode. This is the SaaS-safety
// guarantee — no one should get a bearer-free 200 in prod just because
// MOLECULE_ENV leaks an unexpected value.
func TestWorkspaceAuth_DevModeEscapeHatch_IgnoredInProduction(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "production")
	t.Setenv("ADMIN_TOKEN", "")

	mockDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	r := gin.New()
	r.GET("/workspaces/:id/activity", WorkspaceAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"activity": []interface{}{}})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet,
		"/workspaces/00000000-0000-0000-0000-000000000000/activity", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("production mode: expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceAuth_DevModeEscapeHatch_IgnoredWhenAdminTokenSet verifies
// setting ADMIN_TOKEN on the server (the #684 opt-in) disables the
// dev-mode hatch — callers MUST present a valid bearer. Setting
// ADMIN_TOKEN is the explicit SaaS-mode opt-in.
func TestWorkspaceAuth_DevModeEscapeHatch_IgnoredWhenAdminTokenSet(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "development")
	t.Setenv("ADMIN_TOKEN", "operator-set-this")

	mockDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	r := gin.New()
	r.GET("/workspaces/:id/activity", WorkspaceAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"activity": []interface{}{}})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet,
		"/workspaces/00000000-0000-0000-0000-000000000000/activity", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("dev-mode + ADMIN_TOKEN: expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAdminAuth_DevModeEscapeHatch_FailsOpenWithHasLiveTokens documents the
// Tier-1b dev-mode escape hatch. When the platform runs with MOLECULE_ENV=development
// and ADMIN_TOKEN is unset, AdminAuth must stay fail-open even after workspace
// tokens land in the DB. This keeps the Canvas dashboard usable in local dev
// after the first workspace is created (PR #1871 — quickstart bugless).
//
// SaaS never hits this path because tenant provisioning sets both
// ADMIN_TOKEN and MOLECULE_ENV=production.
func TestAdminAuth_DevModeEscapeHatch_FailsOpenWithHasLiveTokens(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "development")
	t.Setenv("ADMIN_TOKEN", "")

	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// HasAnyLiveTokenGlobal returns 1 — tokens exist (post first-workspace).
	// The Tier-1 fail-open branch WOULD close here. Tier-1b must still open.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := gin.New()
	r.GET("/workspaces", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"workspaces": []interface{}{}})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/workspaces", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("dev-mode escape hatch: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_DevModeEscapeHatch_IgnoredWhenAdminTokenSet verifies that the
// dev-mode escape hatch does NOT override an operator who has set ADMIN_TOKEN.
// Setting ADMIN_TOKEN is the explicit opt-in to #684 closure; dev-mode must not
// silently reopen the gate.
func TestAdminAuth_DevModeEscapeHatch_IgnoredWhenAdminTokenSet(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "development")
	t.Setenv("ADMIN_TOKEN", "operator-explicitly-set-this")

	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// Tokens exist — Tier 1 closes.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := gin.New()
	r.GET("/workspaces", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"workspaces": []interface{}{}})
	})

	w := httptest.NewRecorder()
	// No bearer token — must 401 even in dev mode because ADMIN_TOKEN is set.
	req, _ := http.NewRequest(http.MethodGet, "/workspaces", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("dev-mode + ADMIN_TOKEN set: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_DevModeEscapeHatch_IgnoredInProduction verifies the hatch never
// fires when MOLECULE_ENV=production. This is the SaaS-safety guarantee.
func TestAdminAuth_DevModeEscapeHatch_IgnoredInProduction(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "production")
	t.Setenv("ADMIN_TOKEN", "")

	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := gin.New()
	r.GET("/workspaces", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"workspaces": []interface{}{}})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/workspaces", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("production mode: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_Issue120_PatchWorkspace_NoBearer_Returns401 documents the #120
// attack vector and verifies that AdminAuth returns 401 for PATCH without a token.
func TestAdminAuth_Issue120_PatchWorkspace_NoBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// HasAnyLiveTokenGlobal returns 1 — at least one workspace is token-enrolled.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := gin.New()
	// Mirror the PR #125 router change: PATCH is inside the wsAdmin AdminAuth group.
	r.PATCH("/workspaces/:id", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "updated"})
	})

	w := httptest.NewRecorder()
	// #120 attack: no Authorization header on PATCH.
	req, _ := http.NewRequest(http.MethodPatch,
		"/workspaces/00000000-0000-0000-0000-000000000000",
		nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#120 PATCH no-bearer: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ── CanvasOrBearer (#168) ────────────────────────────────────────────────────
// Narrow softer variant of AdminAuth used ONLY on PUT /canvas/viewport.
// Accepts bearer or a matching Origin header. MUST NOT be used anywhere a
// forged request would leak data or create resources.

func TestCanvasOrBearer_NoTokens_FailOpen(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	r := gin.New()
	r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("bootstrap fail-open: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

func TestCanvasOrBearer_TokensExist_NoCreds_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	handlerCalled := false
	r := gin.New()
	r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
		handlerCalled = true
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("no creds: got %d, want 401", w.Code)
	}
	if handlerCalled {
		t.Error("handler was called after AbortWithStatusJSON — missing return allows fall-through")
	}
	if body := w.Body.String(); body == `{"ok":true}` {
		t.Error("handler body written after AbortWithStatusJSON")
	}
}

func TestCanvasOrBearer_TokensExist_WrongOrigin_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	t.Setenv("CORS_ORIGINS", "https://acme.moleculesai.app")

	handlerCalled := false
	r := gin.New()
	r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
		handlerCalled = true
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong origin: got %d, want 401", w.Code)
	}
	if handlerCalled {
		t.Error("handler was called after AbortWithStatusJSON — missing return allows fall-through")
	}
	if body := w.Body.String(); body == `{"ok":true}` {
		t.Error("handler body written after AbortWithStatusJSON")
	}
}

func TestCanvasOrBearer_TokensExist_CanvasOrigin_Passes(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	t.Setenv("CORS_ORIGINS", "https://acme.moleculesai.app,https://bob.moleculesai.app")

	r := gin.New()
	r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
	req.Header.Set("Origin", "https://acme.moleculesai.app")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("canvas origin: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// ────────────────────────────────────────────────────────────────────────────
// #682 defense-in-depth — ValidateAnyToken JOIN on workspaces
//
// Tokens belonging to 'removed' workspaces must be rejected by AdminAuth even
// if the token row itself is not yet revoked. The JOIN in ValidateAnyToken
// filters them at the DB layer before revoked_at is checked.
// ────────────────────────────────────────────────────────────────────────────

// TestAdminAuth_RemovedWorkspaceToken_Returns401 — a bearer token whose
// issuing workspace has status='removed' must not grant admin access.
// The JOIN in ValidateAnyToken filters the row out, resulting in ErrNoRows.
func TestAdminAuth_RemovedWorkspaceToken_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	removedToken := "token-from-removed-workspace"
	removedHash := sha256.Sum256([]byte(removedToken))

	// HasAnyLiveTokenGlobal: tokens exist (other workspaces are live).
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	// ValidateAnyToken SELECT with JOIN — removed workspace filtered out → empty result.
	mock.ExpectQuery(validateAnyTokenSelectQuery).
		WithArgs(removedHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // empty: w.status='removed'

	r := gin.New()
	r.GET("/admin/secrets", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+removedToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#682 removed-workspace token: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestCanvasOrBearer_WrongOrigin_Blocked(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	t.Setenv("CORS_ORIGINS", "https://acme.moleculesai.app")

	r := gin.New()
	r.PUT("/canvas/viewport", CanvasOrBearer(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/canvas/viewport", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong origin: got %d, want 401", w.Code)
	}
}

func TestCanvasOriginAllowed_EmptyOriginRejected(t *testing.T) {
	if canvasOriginAllowed("") {
		t.Error("empty Origin must not pass")
	}
}

func TestCanvasOriginAllowed_LocalhostDefault(t *testing.T) {
	t.Setenv("CORS_ORIGINS", "")
	if !canvasOriginAllowed("http://localhost:3000") {
		t.Error("localhost:3000 should be allowed by default")
	}
	if canvasOriginAllowed("http://evil.example.com") {
		t.Error("random origin should not be allowed")
	}
}

// ── Issue #623 regression ─────────────────────────────────────────────────────
// AdminAuth must NOT accept forged Origin headers. Any container on the Docker
// network can set Origin: http://localhost:3000 without a bearer token, which
// previously bypassed AdminAuth on ALL admin-gated routes. (#623, dup #626)

// TestAdminAuth_623_ForgedOrigin_Returns401 — the main regression test:
// a request with a matching CORS origin but no bearer token must be rejected.
func TestAdminAuth_623_ForgedOrigin_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	// Platform has live tokens — AdminAuth is active.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	t.Setenv("CORS_ORIGINS", "http://localhost:3000")

	r := gin.New()
	r.GET("/settings/secrets", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"secrets": []string{"OPENAI_API_KEY"}})
	})

	w := httptest.NewRecorder()
	// #623 attack: forge the canvas Origin header — no bearer token.
	req, _ := http.NewRequest(http.MethodGet, "/settings/secrets", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#623 forged Origin bypass: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_623_ForgedCORSOrigin_Returns401 — variant: attacker uses the
// tenant-domain CORS origin from CORS_ORIGINS (not just localhost).
func TestAdminAuth_623_ForgedCORSOrigin_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	t.Setenv("CORS_ORIGINS", "https://acme.moleculesai.app")

	r := gin.New()
	r.GET("/admin/secrets", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/secrets", nil)
	req.Header.Set("Origin", "https://acme.moleculesai.app")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#623 forged tenant Origin: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_623_ValidBearer_WithOrigin_Passes — bearer + matching Origin
// should still work (the Origin is irrelevant once the bearer validates).
func TestAdminAuth_623_ValidBearer_WithOrigin_Passes(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	goodToken := "valid-bearer-token-xyz"
	tokenHash := sha256.Sum256([]byte(goodToken))

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(validateAnyTokenSelectQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", "ws-x"))
	mock.ExpectExec(validateTokenUpdateQuery).
		WithArgs("tok-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	t.Setenv("CORS_ORIGINS", "http://localhost:3000")

	r := gin.New()
	r.GET("/settings/secrets", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/settings/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+goodToken)
	req.Header.Set("Origin", "http://localhost:3000") // present but irrelevant
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("bearer+origin: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ── Issue #684 — AdminAuth accepts any workspace bearer as admin credential ──
//
// Root cause: AdminAuth called ValidateAnyToken which matched any live
// workspace token.  A compromised workspace agent could present its own bearer
// and reach /admin/github-installation-token, /approvals/pending, etc.
//
// Fix: when ADMIN_TOKEN env var is set the middleware verifies the bearer
// against that secret exclusively (constant-time).  Workspace tokens are
// rejected even if valid.  When ADMIN_TOKEN is not set the old behaviour is
// preserved for backward-compat (deprecated fallback, tier 3).

// TestAdminAuth_684_AdminTokenSet_WorkspaceTokenRejected — the primary
// regression test: when ADMIN_TOKEN is configured, a valid workspace bearer
// token MUST be rejected with 401 on admin routes (#684).
func TestAdminAuth_684_AdminTokenSet_WorkspaceTokenRejected(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	t.Setenv("ADMIN_TOKEN", "super-secret-admin-token-xyz")

	// Platform has live workspace tokens — AdminAuth is active.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	// ValidateAnyToken must NOT be called — workspace tokens must be rejected
	// before any DB lookup when ADMIN_TOKEN is set.

	r := gin.New()
	r.GET("/admin/github-installation-token", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"token": "ghp_live_token"})
	})

	w := httptest.NewRecorder()
	// #684 attack: compromised workspace agent sends its own bearer.
	req, _ := http.NewRequest(http.MethodGet, "/admin/github-installation-token", nil)
	req.Header.Set("Authorization", "Bearer some-valid-workspace-bearer-token")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#684 workspace token w/ ADMIN_TOKEN set: expected 401, got %d: %s",
			w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_684_AdminTokenSet_CorrectAdminTokenAccepted — when ADMIN_TOKEN
// is set, presenting the exact ADMIN_TOKEN value must grant access (200).
func TestAdminAuth_684_AdminTokenSet_CorrectAdminTokenAccepted(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	const adminSecret = "super-secret-admin-token-xyz"
	t.Setenv("ADMIN_TOKEN", adminSecret)

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	// No DB token lookup — ADMIN_TOKEN check is env-only, no DB round-trip.

	r := gin.New()
	r.GET("/admin/github-installation-token", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"token": "ghp_live_token"})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/github-installation-token", nil)
	req.Header.Set("Authorization", "Bearer "+adminSecret)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("#684 correct ADMIN_TOKEN: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_684_AdminTokenSet_WrongAdminToken_Returns401 — when ADMIN_TOKEN
// is set, presenting a different value must return 401.
func TestAdminAuth_684_AdminTokenSet_WrongAdminToken_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	t.Setenv("ADMIN_TOKEN", "correct-admin-secret")

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := gin.New()
	r.GET("/admin/liveness", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"subsystems": gin.H{}})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/liveness", nil)
	req.Header.Set("Authorization", "Bearer wrong-admin-secret")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#684 wrong ADMIN_TOKEN: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_684_AdminTokenSet_NoBearer_Returns401 — when ADMIN_TOKEN is
// set, a request with no bearer must still return 401.
func TestAdminAuth_684_AdminTokenSet_NoBearer_Returns401(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	t.Setenv("ADMIN_TOKEN", "correct-admin-secret")

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := gin.New()
	r.GET("/approvals/pending", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"approvals": []interface{}{}})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/approvals/pending", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("#684 no bearer w/ ADMIN_TOKEN set: expected 401, got %d: %s",
			w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_684_AdminTokenNotSet_FallsBackToWorkspaceToken — when
// ADMIN_TOKEN is NOT set, a valid workspace token is still accepted (deprecated
// tier-3 fallback for backward compatibility).
func TestAdminAuth_684_AdminTokenNotSet_FallsBackToWorkspaceToken(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// ADMIN_TOKEN explicitly unset — tier-3 fallback active.
	t.Setenv("ADMIN_TOKEN", "")

	workspaceToken := "any-live-workspace-token"
	tokenHash := sha256.Sum256([]byte(workspaceToken))

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	mock.ExpectQuery(validateAnyTokenSelectQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-ws-1", "ws-x"))

	mock.ExpectExec(validateTokenUpdateQuery).
		WithArgs("tok-ws-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	r := gin.New()
	r.GET("/admin/secrets", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+workspaceToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("#684 fallback (no ADMIN_TOKEN): expected 200, got %d: %s",
			w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestAdminAuth_684_FailOpen_AdminTokenSet_NoGlobalTokens — even when
// Regression for SaaS-launch blocker C4: when ADMIN_TOKEN is set, a
// fresh install (zero live workspace tokens) MUST fail closed. Hosted
// SaaS tenants boot with ADMIN_TOKEN set but an empty tokens table —
// without this guard, an anonymous caller can POST /org/import or
// /workspaces before the first real user and pre-empt the instance.
// Fail-open is only acceptable when ADMIN_TOKEN is also unset
// (self-hosted dev with zero auth configured).
func TestAdminAuth_C4_AdminTokenSet_FreshInstall_FailsClosed(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	t.Setenv("ADMIN_TOKEN", "some-admin-secret")

	// HasAnyLiveTokenGlobal returns 0 — fresh install.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	r := gin.New()
	r.GET("/admin/secrets", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/secrets", nil)
	// No bearer — ADMIN_TOKEN is set so the no-tokens tier-1 escape
	// no longer applies; the request must be rejected.
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("C4 fresh-install w/ ADMIN_TOKEN set: expected 401, got %d: %s",
			w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ── Issue #684 route-specific regression ─────────────────────────────────────
// The tests above validate the core AdminAuth middleware contract. These
// table-driven tests pin the same contract for the three specific routes named
// in the #684 security report: /admin/liveness, /admin/github-installation-token,
// and /approvals/pending. Coverage: workspace-token rejected, correct ADMIN_TOKEN
// accepted, no-bearer rejected (with and without ADMIN_TOKEN configured).

// TestAdminAuth_684_SpecificRoutes_WorkspaceTokenRejected — a workspace bearer
// must be rejected on each vulnerable route when ADMIN_TOKEN is set (tier 2).
// The workspace token value intentionally differs from ADMIN_TOKEN.
func TestAdminAuth_684_SpecificRoutes_WorkspaceTokenRejected(t *testing.T) {
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/liveness"},
		{http.MethodGet, "/admin/github-installation-token"},
		{http.MethodGet, "/approvals/pending"},
	}

	for _, rt := range routes {
		rt := rt
		t.Run(rt.path, func(t *testing.T) {
			mockDB, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer mockDB.Close()

			const adminSecret = "correct-admin-secret-not-a-workspace-token"
			t.Setenv("ADMIN_TOKEN", adminSecret)

			mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

			// With ADMIN_TOKEN set, ValidateAnyToken is never called — the env-var
			// check short-circuits. No DB token lookup expectation is set here.

			r := gin.New()
			r.Handle(rt.method, rt.path, AdminAuth(mockDB), func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(rt.method, rt.path, nil)
			// Workspace-scoped token — valid for a workspace, but ≠ ADMIN_TOKEN.
			req.Header.Set("Authorization", "Bearer workspace-agent-bearer-not-admin")
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("#684 %s %s: workspace token should be rejected, got %d: %s",
					rt.method, rt.path, w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

// TestAdminAuth_684_SpecificRoutes_CorrectAdminTokenAccepted — the exact
// ADMIN_TOKEN value must grant access on each vulnerable route. No DB token
// lookup occurs — the env-var comparison is constant-time only.
func TestAdminAuth_684_SpecificRoutes_CorrectAdminTokenAccepted(t *testing.T) {
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/liveness"},
		{http.MethodGet, "/admin/github-installation-token"},
		{http.MethodGet, "/approvals/pending"},
	}

	for _, rt := range routes {
		rt := rt
		t.Run(rt.path, func(t *testing.T) {
			mockDB, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer mockDB.Close()

			const adminSecret = "correct-admin-secret-not-a-workspace-token"
			t.Setenv("ADMIN_TOKEN", adminSecret)

			mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

			// No DB token lookup — ADMIN_TOKEN match triggers c.Next() directly.

			r := gin.New()
			r.Handle(rt.method, rt.path, AdminAuth(mockDB), func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(rt.method, rt.path, nil)
			req.Header.Set("Authorization", "Bearer "+adminSecret)
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("#684 %s %s: correct ADMIN_TOKEN should pass, got %d: %s",
					rt.method, rt.path, w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

// TestAdminAuth_684_SpecificRoutes_NoBearer_Returns401 — no bearer returns
// 401 on each vulnerable route, both with and without ADMIN_TOKEN set.
func TestAdminAuth_684_SpecificRoutes_NoBearer_Returns401(t *testing.T) {
	routes := []struct {
		method     string
		path       string
		adminToken string // empty = ADMIN_TOKEN not configured (tier-3 fallback)
	}{
		// ADMIN_TOKEN configured — explicit rejection before any DB lookup.
		{http.MethodGet, "/admin/liveness", "some-admin-secret"},
		{http.MethodGet, "/admin/github-installation-token", "some-admin-secret"},
		{http.MethodGet, "/approvals/pending", "some-admin-secret"},
		// ADMIN_TOKEN absent — tier-3 fallback, still rejects missing bearer.
		{http.MethodGet, "/admin/liveness", ""},
		{http.MethodGet, "/admin/github-installation-token", ""},
		{http.MethodGet, "/approvals/pending", ""},
	}

	for _, rt := range routes {
		rt := rt
		label := rt.path + "/ADMIN_TOKEN=" + rt.adminToken
		t.Run(label, func(t *testing.T) {
			mockDB, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer mockDB.Close()

			t.Setenv("ADMIN_TOKEN", rt.adminToken)

			mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

			r := gin.New()
			r.Handle(rt.method, rt.path, AdminAuth(mockDB), func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(rt.method, rt.path, nil)
			// No Authorization header — must be rejected unconditionally.
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("#684 no-bearer %s %s: expected 401, got %d: %s",
					rt.method, rt.path, w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

// ==================== platform-unavailable classification ====================
//
// abortAuthLookupError replaces the prior opaque
// `500 {"error":"auth check failed"}` with a 503 + structured code so
// the canvas can render a dedicated diagnostic instead of a confusing
// toast. Pin both the status code and the body shape against
// regression — this is the contract the canvas's
// PlatformUnavailableError classifier reads at api.ts.

func TestAdminAuth_DatastoreError_Returns503PlatformUnavailable(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	// Simulate Postgres being down — HasAnyLiveTokenGlobal's COUNT
	// query returns a connection error.
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnError(errors.New("dial tcp [::1]:5432: connect: connection refused"))

	r := gin.New()
	r.GET("/workspaces", AdminAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/workspaces", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response body must be JSON: %v (body=%s)", err, w.Body.String())
	}
	if resp["code"] != "platform_unavailable" {
		t.Errorf("response code = %v, want platform_unavailable (canvas reads this for the dedicated diagnostic)", resp["code"])
	}
	if _, ok := resp["error"].(string); !ok {
		t.Errorf("response must include human-readable error string, got %v", resp["error"])
	}
	// The body must NOT leak the underlying DB error string —
	// production hostnames / connection-string fragments could land
	// in an error toast otherwise.
	if errStr, _ := resp["error"].(string); strings.Contains(errStr, "dial tcp") {
		t.Errorf("response leaks underlying DB error: %q", errStr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
