package handlers

// Sqlmock-backed coverage for tokens.go. Closes #1819.
//
// The existing tokens_test.go uses the real `db.DB` and t.Skip's when
// the test DB isn't reachable — which is the default in CI, so the
// file shows 0% coverage. This file substitutes the package-level
// `db.DB` with a sqlmock instance so every code path (List, Create,
// Revoke + their error branches) is exercised in `go test` without
// any external dependency.
//
// What's covered:
//   List   — happy path, empty rows, scan failure, query error
//   Create — rate-limited, IssueToken DB error, happy path
//   Revoke — happy path, not found, DB error
//
// What's NOT covered here (intentional):
//   - Wsauth/middleware-level cross-tenant gating: those are exercised
//     by middleware/wsauth_middleware_test.go. The handler-level code
//     trusts WorkspaceAuth has already gated the request.
//   - The full IssueToken path's correctness (random bytes,
//     base64 encoding, prefix derivation): wsauth/tokens_test.go owns
//     that. Here we only verify the handler hands off + reports
//     errors correctly.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// Workspace IDs are validated as UUIDs up front (tokens.go validWorkspaceID),
// so handler tests must pass syntactically valid UUIDs. Fixed values keep
// sqlmock WithArgs assertions deterministic.
const (
	wsUUID1 = "11111111-1111-1111-1111-111111111111"
	wsUUID2 = "22222222-2222-2222-2222-222222222222"
	wsUUID3 = "33333333-3333-3333-3333-333333333333"
)

// withMockDB swaps `db.DB` for a sqlmock and returns the mock plus a
// restore func. Tests use this in place of setupTokenTestDB which
// skips on a missing real DB.
func withMockDB(t *testing.T) (sqlmock.Sqlmock, func()) {
	t.Helper()
	mock, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	prev := db.DB
	db.DB = mock
	cleanup := func() {
		db.DB = prev
		_ = mock.Close()
	}
	return m, cleanup
}

// makeReq builds a recorder + Gin context with the given URL params,
// drives the handler, and returns the recorder. Centralised so each
// scenario is one-line setup + assertion.
func makeReq(t *testing.T, h gin.HandlerFunc, method, url string, params gin.Params) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, url, nil)
	c.Params = params
	h(c)
	return w
}

// ---- List ------------------------------------------------------------

func TestTokenHandler_List_HappyPath(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	created := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	last := created.Add(time.Hour)
	mock.ExpectQuery(`SELECT id, prefix, created_at, last_used_at\s+FROM workspace_auth_tokens`).
		WithArgs(wsUUID1, 50, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "created_at", "last_used_at"}).
			AddRow("tok-1", "abc12345", created, last).
			AddRow("tok-2", "def67890", created, nil))

	w := makeReq(t, NewTokenHandler().List, "GET",
		"/workspaces/ws-1/tokens", gin.Params{{Key: "id", Value: wsUUID1}})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Tokens []tokenListItem `json:"tokens"`
		Count  int             `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 2 || len(body.Tokens) != 2 {
		t.Fatalf("count=%d tokens=%d, want 2/2", body.Count, len(body.Tokens))
	}
	if body.Tokens[0].ID != "tok-1" || body.Tokens[1].ID != "tok-2" {
		t.Errorf("wrong order: %+v", body.Tokens)
	}
	if body.Tokens[1].LastUsed != nil {
		t.Errorf("token-2 should have nil last_used_at, got %v", body.Tokens[1].LastUsed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestTokenHandler_List_EmptyResult(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT id, prefix, created_at, last_used_at`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "created_at", "last_used_at"}))

	w := makeReq(t, NewTokenHandler().List, "GET",
		"/workspaces/ws-2/tokens", gin.Params{{Key: "id", Value: wsUUID2}})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on empty list, got %d", w.Code)
	}
	var body struct {
		Tokens []tokenListItem `json:"tokens"`
		Count  int             `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Count != 0 || body.Tokens == nil {
		// Tokens MUST be `[]` not `null` so callers iterating with
		// `.length` or `for...of` don't NPE on JS.
		t.Errorf("empty: count=%d, tokens=%v (want 0 + non-nil)", body.Count, body.Tokens)
	}
}

func TestTokenHandler_List_QueryError(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT id, prefix, created_at, last_used_at`).
		WillReturnError(errors.New("connection refused"))

	w := makeReq(t, NewTokenHandler().List, "GET",
		"/workspaces/ws-3/tokens", gin.Params{{Key: "id", Value: wsUUID3}})

	if w.Code != http.StatusInternalServerError {
		t.Errorf("query error must surface as 500, got %d", w.Code)
	}
}

func TestTokenHandler_List_RespectsLimit(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT id, prefix, created_at, last_used_at`).
		WithArgs(wsUUID1, 10, 5).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "created_at", "last_used_at"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/tokens?limit=10&offset=5", nil)
	c.Params = gin.Params{{Key: "id", Value: wsUUID1}}
	NewTokenHandler().List(c)

	if w.Code != http.StatusOK {
		t.Errorf("limit/offset query: %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("limit/offset args not bound correctly: %v", err)
	}
}

func TestTokenHandler_List_ScanError(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	// Inject a bad row that fails to Scan: pass non-time value where
	// created_at expects time.Time. sqlmock surfaces this as a Scan err.
	mock.ExpectQuery(`SELECT id, prefix, created_at, last_used_at`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "created_at", "last_used_at"}).
			AddRow("tok-1", "abc", "not-a-timestamp", nil))

	w := makeReq(t, NewTokenHandler().List, "GET",
		"/workspaces/ws-1/tokens", gin.Params{{Key: "id", Value: wsUUID1}})

	if w.Code != http.StatusInternalServerError {
		t.Errorf("scan error must surface as 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- Create ----------------------------------------------------------

func TestTokenHandler_Create_RateLimited(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	// Count query returns 50 (== max) → 429.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(50))

	w := makeReq(t, NewTokenHandler().Create, "POST",
		"/workspaces/ws-1/tokens", gin.Params{{Key: "id", Value: wsUUID1}})

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("max active tokens should 429, got %d", w.Code)
	}
}

func TestTokenHandler_Create_IssueFails(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	// Count = 0 → fall through to IssueToken, which does an INSERT
	// into workspace_auth_tokens. Mock the INSERT to fail; handler
	// surfaces as 500.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO workspace_auth_tokens`).
		WillReturnError(errors.New("disk full"))

	w := makeReq(t, NewTokenHandler().Create, "POST",
		"/workspaces/ws-1/tokens", gin.Params{{Key: "id", Value: wsUUID1}})

	if w.Code != http.StatusInternalServerError {
		t.Errorf("IssueToken DB error must 500, got %d", w.Code)
	}
}

func TestTokenHandler_Create_HappyPath(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO workspace_auth_tokens`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := makeReq(t, NewTokenHandler().Create, "POST",
		"/workspaces/ws-1/tokens", gin.Params{{Key: "id", Value: wsUUID1}})

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		AuthToken   string `json:"auth_token"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AuthToken == "" {
		t.Errorf("auth_token must be present and non-empty in response")
	}
	if body.WorkspaceID != wsUUID1 {
		t.Errorf("workspace_id mismatch: %q", body.WorkspaceID)
	}
}

// ---- Revoke ----------------------------------------------------------

func TestTokenHandler_Revoke_HappyPath(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectExec(`UPDATE workspace_auth_tokens\s+SET revoked_at = now\(\)`).
		WithArgs("tok-1", wsUUID1).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := makeReq(t, NewTokenHandler().Revoke, "DELETE",
		"/workspaces/ws-1/tokens/tok-1", gin.Params{
			{Key: "id", Value: wsUUID1},
			{Key: "tokenId", Value: "tok-1"},
		})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on revoke, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTokenHandler_Revoke_NotFound(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	// 0 rows affected → token not found OR already revoked.
	mock.ExpectExec(`UPDATE workspace_auth_tokens`).
		WithArgs("tok-ghost", wsUUID1).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := makeReq(t, NewTokenHandler().Revoke, "DELETE",
		"/workspaces/ws-1/tokens/tok-ghost", gin.Params{
			{Key: "id", Value: wsUUID1},
			{Key: "tokenId", Value: "tok-ghost"},
		})

	if w.Code != http.StatusNotFound {
		t.Errorf("revoke missing token must 404, got %d", w.Code)
	}
}

func TestTokenHandler_Revoke_DBError(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectExec(`UPDATE workspace_auth_tokens`).
		WillReturnError(errors.New("conn lost"))

	w := makeReq(t, NewTokenHandler().Revoke, "DELETE",
		"/workspaces/ws-1/tokens/tok-1", gin.Params{
			{Key: "id", Value: wsUUID1},
			{Key: "tokenId", Value: "tok-1"},
		})

	if w.Code != http.StatusInternalServerError {
		t.Errorf("DB error must 500, got %d", w.Code)
	}
}

// ---- UUID validation (regression: "global" sentinel 500) ------------

// The canvas Settings → Workspace Tokens tab sent the literal sentinel
// "global" as the workspace id when no node was selected. workspace_id
// is a `uuid` column, so the query raised
// `invalid input syntax for type uuid: "global"` which leaked as an
// opaque 500. List/Create/Revoke now reject any non-UUID id with a
// clean 400 before touching the DB. No DB expectation is set on the
// mock — a DB hit would fail ExpectationsWereMet, proving short-circuit.
func TestTokenHandler_RejectsNonUUIDWorkspaceID(t *testing.T) {
	h := NewTokenHandler()
	cases := []struct {
		name   string
		run    func(c *gin.Context)
		method string
		params gin.Params
	}{
		{"List", h.List, "GET", gin.Params{{Key: "id", Value: "global"}}},
		{"Create", h.Create, "POST", gin.Params{{Key: "id", Value: "global"}}},
		{"Revoke", h.Revoke, "DELETE", gin.Params{
			{Key: "id", Value: "global"},
			{Key: "tokenId", Value: "tok-1"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := withMockDB(t)
			defer cleanup()

			w := makeReq(t, tc.run, tc.method,
				"/workspaces/global/tokens", tc.params)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s with non-UUID id must 400, got %d: %s",
					tc.name, w.Code, w.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body.Error != "invalid workspace id" {
				t.Errorf("%s: want error=%q, got %q",
					tc.name, "invalid workspace id", body.Error)
			}
			// No query/exec was expected → if the handler hit the DB
			// this fails, proving the guard short-circuits before SQL.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("%s leaked a DB call past the uuid guard: %v", tc.name, err)
			}
		})
	}
}

// Compile-time noise removal: the imports list pulls in the sql /
// driver packages and the silenced ctx so a future scenario that
// needs them doesn't have to re-add the import. Documented here so
// the apparent "unused import" dance isn't surprising.
var (
	_ context.Context = context.Background()
	_ driver.Value    = (sql.RawBytes)(nil)
)
