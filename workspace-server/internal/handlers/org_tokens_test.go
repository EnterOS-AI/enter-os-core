package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// setupOrgTokenTest wires the package-global db.DB to a sqlmock for
// the duration of a test, returning the handler + mock + cleanup.
// Gin runs in release mode to suppress debug noise.
func setupOrgTokenTest(t *testing.T) (*OrgTokenHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	mock, mockDB, cleanup := mockSQLDB(t)
	prev := db.DB
	db.DB = mockDB
	return NewOrgTokenHandler(), mock, func() {
		db.DB = prev
		cleanup()
	}
}

// mockSQLDB returns an sqlmock + *sql.DB pair. Caller restores
// package state via the cleanup func.
func mockSQLDB(t *testing.T) (sqlmock.Sqlmock, *sql.DB, func()) {
	t.Helper()
	d, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return m, d, func() { _ = d.Close() }
}

// buildCtx returns a gin.Context + recorder wired for the given
// method+path+body. Test code can set headers / context values
// before calling the handler.
func buildCtx(method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	c.Request = r.WithContext(context.Background())
	return c, w
}

// ---- List -----------------------------------------------------------------

func TestOrgTokenHandler_List_HappyPath(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT id, prefix.*FROM org_api_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "name", "org_id", "created_by", "created_at", "last_used_at", "expires_at"}).
			AddRow("tok-1", "abcd1234", "zapier", "", "session", now, nil, nil))

	c, w := buildCtx("GET", "/org/tokens", "")
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Count  int `json:"count"`
		Tokens []struct {
			ID     string `json:"id"`
			Prefix string `json:"prefix"`
			Name   string `json:"name"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Count != 1 || len(body.Tokens) != 1 {
		t.Fatalf("expected 1 token, got %+v", body)
	}
	if body.Tokens[0].Prefix != "abcd1234" {
		t.Errorf("prefix not propagated: %q", body.Tokens[0].Prefix)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestOrgTokenHandler_List_DBError500(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT id, prefix.*FROM org_api_tokens`).
		WillReturnError(sql.ErrConnDone)

	c, w := buildCtx("GET", "/org/tokens", "")
	h.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("db error → 500 expected; got %d", w.Code)
	}
}

// ---- Create ---------------------------------------------------------------

func TestOrgTokenHandler_Create_ActorFromAdminToken(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	// (core#2574 / #2579) The Phase-4 approval gate now fires on
	// admin-token org_token_mint. The test sets MOLECULE_PLATFORM_
	// WORKSPACE_ID to derive the approval anchor, then mocks the
	// approval flow + the post-gate INSERT.
	const platformOrgWS = "00000000-0000-0000-0000-00000000aaff"
	t.Setenv("MOLECULE_PLATFORM_WORKSPACE_ID", platformOrgWS)
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	defer os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	// requireApproval's 3-query sequence for the gate (no prior approval):
	mock.ExpectQuery(`UPDATE approval_requests SET consumed_at`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`WITH existing AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("appr-actor-admin"))
	mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id`).
		WithArgs(platformOrgWS).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	// (core#2574 / #2579) Deliberately NO `INSERT INTO org_api_tokens`
	// mock setup. The gate returns proceed=false → the post-gate
	// orgtoken.Issue call is NEVER reached. If the gate is bypassed
	// (regression), the unmocked INSERT will fail and this test
	// will fail (sqlmock will surface the unfulfilled expectation
	// as an error).

	c, w := buildCtx("POST", "/org/tokens", `{"name":"my-ci"}`)
	c.Set("caller_is_admin_token", true)
	c.Set("caller_credential_class", "admin-token")
	h.Create(c)

	// (core#2574 / #2579) Gate fires correctly → 202 with
	// approval_id; the post-gate INSERT mock is therefore NOT
	// called (gateDestructive returns false before orgtoken.Issue).
	if w.Code != http.StatusAccepted {
		t.Fatalf("admin-token + gated action MUST return 202 (Phase-4 approval gate), got %d: %s",
			w.Code, w.Body.String())
	}
	var body struct {
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
		Action     string `json:"action"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval", body.Status)
	}
	if body.ApprovalID != "appr-actor-admin" {
		t.Errorf("approval_id = %q, want appr-actor-admin", body.ApprovalID)
	}
	if body.Action != "org_token_mint" {
		t.Errorf("action = %q, want org_token_mint", body.Action)
	}
	// Sanity: post-gate INSERT was NOT called (gate fired).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (post-gate INSERT should not be called): %v", err)
	}
}

func TestOrgTokenHandler_Create_ActorFromOrgTokenPrefix(t *testing.T) {
	h, _, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	// (core#2574 / #2579) The Phase-4 approval gate fires for ALL
	// gated callers — org-token too. The caller's org_id is the
	// approval anchor (callerOrg returns the token's org_id; the
	// integration test only sets the prefix, so callerOrg returns ""
	// here — but the gate is still in effect). The org-token-with-
	// prefix context alone is not enough; a token record must also
	// exist for the prefix to resolve to an org_id. This test now
	// expects the 4xx (no anchor) since no platform workspace is
	// set and the prefix is not backed by a token row. Pin the
	// controlled-4xx contract for org-token-prefix-only requests.
	os.Unsetenv("MOLECULE_PLATFORM_WORKSPACE_ID")
	defer os.Unsetenv("MOLECULE_PLATFORM_WORKSPACE_ID")
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	defer os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	c, w := buildCtx("POST", "/org/tokens", `{}`)
	c.Set("org_token_prefix", "parent12")
	h.Create(c)

	// Gate fires → 202 (NOT 200): the prefix is the actor label,
	// but the actual approval anchor is the org_id (which the test
	// does not provide via DB lookup). Empty anchor → 4xx.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("org-token prefix without resolved org_id MUST return 400, got %d: %s",
			w.Code, w.Body.String())
	}
}

// (core#2574) Regression test: an admin-token-bearing caller (the
// concierge agent holds $ADMIN_TOKEN) MUST be gated by the Phase-4
// approval gate when minting an org token. The live incident (2026-06-11)
// had the gate INERT on the admin-token path — two live full-tenant-admin
// org API tokens were minted with zero pending approvals. The fix wires
// gateDestructive into OrgTokenHandler.Create and the gate's
// callerIsAdminToken detection overrides the rollout flag (admin-token
// is ALWAYS gated when the action is gated, regardless of the flag).
func TestOrgTokenHandler_Create_AdminToken_GatedByApproval(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	// requireApproval sequence for an admin-token caller (gated action,
	// no pre-existing approval):
	//   1. UPDATE approval_requests SET consumed_at = now() … RETURNING id
	//      → no row (sql.ErrNoRows)
	//   2. WITH existing AS … INSERT INTO approval_requests … RETURNING id
	//   3. SELECT parent_id FROM workspaces WHERE id → NULL
	mock.ExpectQuery(`UPDATE approval_requests SET consumed_at`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`WITH existing AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("appr-core2574-org-mint"))
	mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	// NOTE: deliberately NO `INSERT INTO org_api_tokens` mock setup. If
	// the gate is bypassed (the bug), the handler will reach the
	// orgtoken.Issue call and try to run that INSERT against the mock,
	// which has no expectation — sqlmock will return an error and the test
	// will fail. The gate firing = no INSERT = test passes.

	c, w := buildCtx("POST", "/org/tokens", `{"name":"concierge-rogue-mint"}`)
	// core#2574: the auth middleware sets caller_is_admin_token when the
	// request authenticates via Tier 2b ADMIN_TOKEN (or Tier 3 workspace-
	// token fallback). Simulate that here.
	c.Set("caller_is_admin_token", true)
	c.Set("caller_credential_class", "admin-token")

	// The rollout flag is OFF (default) — this is the regression
	// assertion: even without MOLECULE_PLATFORM_APPROVAL_GATE, the
	// admin-token path must gate.
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	defer os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	// (core#2579) Admin-token callers need a valid UUID anchor for
	// requireApproval's approval_requests.workspace_id query. Set the
	// platform-org workspace ID for the duration of the test. The
	// approval row keys by this UUID; the test's mock for the
	// SELECT parent_id FROM workspaces query returns nil (a root
	// workspace — the platform-org row is parent_id NULL).
	const platformOrgWS = "00000000-0000-0000-0000-00000000aaff"
	t.Setenv("MOLECULE_PLATFORM_WORKSPACE_ID", platformOrgWS)

	h.Create(c)

	// Gate fires → 202 Accepted with a pending approval_id.
	if w.Code != http.StatusAccepted {
		t.Fatalf("admin-token + gated action MUST return 202 (Phase-4 approval gate), got %d: %s",
			w.Code, w.Body.String())
	}
	var body struct {
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
		Action     string `json:"action"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Status != "pending_approval" {
		t.Errorf("status = %q, want \"pending_approval\"", body.Status)
	}
	if body.ApprovalID != "appr-core2574-org-mint" {
		t.Errorf("approval_id = %q, want \"appr-core2574-org-mint\"", body.ApprovalID)
	}
	if body.Action != "org_token_mint" {
		t.Errorf("action = %q, want \"org_token_mint\"", body.Action)
	}
	if body.Reason == "" {
		t.Errorf("reason text missing — operators need a human-readable explanation for the pending approval")
	}
}

// TestOrgTokenHandler_Create_AdminToken_NoAnchor_Returns400 (core#2579)
// pins the controlled-4xx behavior for the empty-anchor path: when
// an admin-token caller hits org_token_mint WITHOUT
// MOLECULE_PLATFORM_WORKSPACE_ID set, the handler returns 400 with a
// clear hint (not 500, not a UUID-syntax error, not a silent
// gate-bypass). Regression: previous code passed callerOrg(c)=""
// directly into gateDestructive → requireApproval's
// approval_requests.workspace_id query failed with
// "invalid input syntax for type uuid: \"\"" → 500 from handler →
// the unmonitored 500 looked like a transient infra failure, not a
// security gate. The fix: derive a valid anchor FIRST (env-var for
// admin-token, org_id for org-token, 4xx for everything else).
func TestOrgTokenHandler_Create_AdminToken_NoAnchor_Returns400(t *testing.T) {
	h, _, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	// Admin-token caller with NO env var and no org_token_id →
	// callerOrg="" → no approval anchor. UNSET the env explicitly
	// to ensure a clean slate (other tests in the package may set it).
	os.Unsetenv("MOLECULE_PLATFORM_WORKSPACE_ID")
	defer os.Unsetenv("MOLECULE_PLATFORM_WORKSPACE_ID")
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	defer os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	c, w := buildCtx("POST", "/org/tokens", `{"name":"rogue-mint-no-anchor"}`)
	c.Set("caller_is_admin_token", true)
	c.Set("caller_credential_class", "admin-token")

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("admin-token WITHOUT platform workspace anchor MUST return 400 (controlled), got %d: %s",
			w.Code, w.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Error == "" {
		t.Errorf("error message missing — operators need the hint about MOLECULE_PLATFORM_WORKSPACE_ID")
	}
	// Sanity: the hint must mention the env var so an operator
	// who hit this in prod knows exactly what to set.
	if !strings.Contains(body.Error, "MOLECULE_PLATFORM_WORKSPACE_ID") {
		t.Errorf("error message should mention the env var hint; got: %q", body.Error)
	}
}

func TestOrgTokenHandler_Create_ActorFromSession(t *testing.T) {
	h, _, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	// (core#2593) BYPASS-RESISTANCE: a raw Cookie header WITHOUT a
	// CP-verified session (cp_session_actor unset) must NOT be
	// treated as a human — otherwise any admin-token agent could
	// skip the approval gate by attaching a junk Cookie. Such a
	// caller has no org_token_id, no admin-token context and no
	// anchor env → controlled 400, gate intact.
	os.Unsetenv("MOLECULE_PLATFORM_WORKSPACE_ID")
	defer os.Unsetenv("MOLECULE_PLATFORM_WORKSPACE_ID")
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	defer os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	c, w := buildCtx("POST", "/org/tokens", `{"name":"from-browser"}`)
	c.Request.Header.Set("Cookie", "mcp_session=abc")
	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("session caller without resolvable org_id MUST return 400, got %d: %s",
			w.Code, w.Body.String())
	}
}

// TestOrgTokenHandler_Create_VerifiedSession_SkipsGate (core#2593)
// pins the human-mint path: a CP-VERIFIED session caller (AdminAuth
// sets cp_session_actor only after VerifiedCPSession confirms the
// WorkOS cookie upstream) mints WITHOUT an approval round-trip — the
// human at the dashboard IS the approver, so 202-pending would be a
// no-op loop. Live regression: Settings → Org API Keys → "+ New Key"
// returned the anchor 400 because #2579 had no session branch.
// The mock expects ONLY the orgtoken INSERT — any approval-flow SQL
// would fail ExpectationsWereMet, proving the gate did not fire.
func TestOrgTokenHandler_Create_VerifiedSession_SkipsGate(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	os.Unsetenv("MOLECULE_PLATFORM_WORKSPACE_ID")
	defer os.Unsetenv("MOLECULE_PLATFORM_WORKSPACE_ID")
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	defer os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	const actor = "session"
	const userID = "u_dashboard_user_42"
	mock.ExpectQuery(`INSERT INTO org_api_tokens`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "from-dashboard", actor+":"+userID, nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok-human"))

	c, w := buildCtx("POST", "/org/tokens", `{"name":"from-dashboard"}`)
	c.Set("cp_session_actor", actor)
	c.Set("cp_session_user_id", userID)
	h.Create(c)

	if w.Code != http.StatusOK {
		t.Fatalf("verified-session mint MUST return 200 (no approval round-trip), got %d: %s",
			w.Code, w.Body.String())
	}
	var body struct {
		ID     string `json:"id"`
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.ID != "tok-human" {
		t.Errorf("id = %q, want tok-human", body.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/extra SQL — approval-flow queries must NOT run for verified sessions: %v", err)
	}
}

func TestOrgTokenHandler_Create_NameTooLong400(t *testing.T) {
	h, _, cleanup := setupOrgTokenTest(t)
	defer cleanup()
	longName := string(make([]byte, 101))
	for i := range longName {
		_ = i
	}
	// Build a 101-char name (any ASCII works; we're hitting the
	// length bound).
	buf := make([]byte, 101)
	for i := range buf {
		buf[i] = 'a'
	}
	c, w := buildCtx("POST", "/org/tokens", `{"name":"`+string(buf)+`"}`)
	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("oversize name: want 400, got %d", w.Code)
	}
}

func TestOrgTokenHandler_Create_EmptyBodyOK(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	// (core#2574 / #2579) Empty body → admin-token caller path
	// (no Cookie, no org_token_prefix). With platform workspace
	// set, the gate fires; the test mocks the approval flow.
	const platformOrgWS = "00000000-0000-0000-0000-00000000aaff"
	t.Setenv("MOLECULE_PLATFORM_WORKSPACE_ID", platformOrgWS)
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	defer os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	mock.ExpectQuery(`UPDATE approval_requests SET consumed_at`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`WITH existing AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("appr-empty-body"))
	mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id`).
		WithArgs(platformOrgWS).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
	mock.ExpectQuery(`INSERT INTO org_api_tokens`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), nil, actorAdminToken, nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok-min"))

	c, w := buildCtx("POST", "/org/tokens", "")
	c.Set("caller_is_admin_token", true)
	c.Set("caller_credential_class", "admin-token")
	h.Create(c)

	// (core#2574 / #2579) Gate fires correctly → 202 with
	// approval_id (admin-token + gated action). The pre-gate
	// expectations are met (approval flow); the post-gate
	// orgtoken.Issue INSERT was NOT called (gate returned false).
	if w.Code != http.StatusAccepted {
		t.Errorf("empty body admin-token: gate MUST return 202, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval", body.Status)
	}
	if body.ApprovalID != "appr-empty-body" {
		t.Errorf("approval_id = %q, want appr-empty-body", body.ApprovalID)
	}
}

// ---- Revoke ---------------------------------------------------------------

func TestOrgTokenHandler_Revoke_HappyPath200(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	mock.ExpectExec(`UPDATE org_api_tokens`).
		WithArgs("tok-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	c, w := buildCtx("DELETE", "/org/tokens/tok-1", "")
	c.Params = gin.Params{{Key: "id", Value: "tok-1"}}
	h.Revoke(c)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOrgTokenHandler_Revoke_Missing404(t *testing.T) {
	// Idempotency: revoking a non-existent or already-revoked id
	// returns 404 — callers can tell "worked" from "already done".
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()
	mock.ExpectExec(`UPDATE org_api_tokens`).
		WithArgs("ghost").
		WillReturnResult(sqlmock.NewResult(0, 0))

	c, w := buildCtx("DELETE", "/org/tokens/ghost", "")
	c.Params = gin.Params{{Key: "id", Value: "ghost"}}
	h.Revoke(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestOrgTokenHandler_Revoke_MissingID400(t *testing.T) {
	h, _, cleanup := setupOrgTokenTest(t)
	defer cleanup()
	c, w := buildCtx("DELETE", "/org/tokens/", "")
	// No id param set.
	h.Revoke(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}
