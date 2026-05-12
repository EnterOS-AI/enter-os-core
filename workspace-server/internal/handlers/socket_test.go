package handlers

import (
	"crypto/sha256"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/ws"
	"github.com/gin-gonic/gin"
)

// newSocketHandlerWithDB creates a SocketHandler backed by the given mock DB
// and a Hub with buffered channels so Register/Unregister don't block.
func newSocketHandlerWithDB(t *testing.T, mockDB *sql.DB, hub *ws.Hub) *SocketHandler {
	db.DB = mockDB
	if hub == nil {
		hub = &ws.Hub{
			Register:   make(chan *ws.Client, 1),
			Unregister: make(chan *ws.Client, 1),
		}
	}
	return NewSocketHandler(hub)
}

// socketRequest builds a test request for the WebSocket connect endpoint.
func socketRequest(method, path, workspaceID, authHeader string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if workspaceID != "" {
		req.Header.Set("X-Workspace-ID", workspaceID)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth gate: DB error on HasAnyLiveToken → 500
// ─────────────────────────────────────────────────────────────────────────────

func TestSocketHandler_AuthGate_DBError_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, mock, nil)

	// HasAnyLiveToken issues a query; make it return an error.
	mock.ExpectQuery("SELECT COUNT").
		WithArgs("ws-auth-db-err").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "ws-auth-db-err", "")

	handler.HandleConnect(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("DB error: expected 500, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth gate: workspace HAS live token, missing Bearer → 401
// ─────────────────────────────────────────────────────────────────────────────

func TestSocketHandler_AuthGate_HasLiveToken_MissingBearer_Returns401(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, mock, nil)

	// HasAnyLiveToken succeeds → workspace has a live token.
	mock.ExpectQuery("SELECT COUNT").
		WithArgs("ws-has-token-no-bearer").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "ws-has-token-no-bearer", "")

	handler.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("hasLive but no bearer: expected 401, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth gate: workspace HAS live token, invalid Bearer → 401
// ─────────────────────────────────────────────────────────────────────────────

func TestSocketHandler_AuthGate_HasLiveToken_InvalidBearer_Returns401(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, mock, nil)

	wsID := "ws-invalid-token"
	badToken := "not-a-valid-token"

	// HasAnyLiveToken: workspace has a live token.
	mock.ExpectQuery("SELECT COUNT").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))

	// ValidateToken: lookupTokenByHash returns ErrNoRows for an unknown token.
	// Any token hash is fine since the token doesn't exist — use AnyArg.
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", wsID, "Bearer "+badToken)

	handler.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("hasLive but invalid bearer: expected 401, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth gate: workspace HAS live token, VALID Bearer → upgrade attempted.
// The WebSocket upgrade itself will fail in httptest (gorilla/websocket
// cannot write a real HTTP/1.1 handshake to httptest.ResponseRecorder), but
// the auth gate is passed so we verify no 401/500 was returned before the
// upgrade failure. This is the canvas-client success path.
// ─────────────────────────────────────────────────────────────────────────────

func TestSocketHandler_AuthGate_HasLiveToken_ValidBearer_AuthPassed(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, mock, nil)

	wsID := "ws-valid-token"
	goodToken := "valid-ws-token-123"

	// HasAnyLiveToken: workspace has a live token.
	mock.ExpectQuery("SELECT COUNT").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))

	// ValidateToken: token found and workspace is not removed.
	// sha256TokenHash returns []byte; rational matcher compares as string.
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN`).
		WithArgs(sha256TokenHash(goodToken)).
		WillReturnRows(sqlmock.NewRows([]string{"token_id", "workspace_id"}).
			AddRow("tok-abc", wsID))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", wsID, "Bearer "+goodToken)

	handler.HandleConnect(c)

	// The WebSocket upgrade fails in httptest (httptest.ResponseRecorder is not
	// a real TCP connection), but the auth gate itself succeeded — we should
	// NOT see a 401 or 500 response code. The actual code depends on the
	// upgrade error handling; the critical assertion is that auth passed.
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusInternalServerError {
		t.Errorf("valid token: auth should have passed; got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Canvas client (no X-Workspace-ID): auth gate bypassed, upgrade attempted.
// Same httptest limitation as above — we verify no 401/500 before the upgrade.
// ─────────────────────────────────────────────────────────────────────────────

func TestSocketHandler_CanvasClient_NoAuthGate(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, mock, nil)

	// No X-Workspace-ID header → no auth check → no DB queries expected.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "", "") // no workspace ID

	handler.HandleConnect(c)

	// No auth gate hit → no 401/500. The WebSocket upgrade itself will fail
	// in httptest, but that's expected (see TestSocketHandler_AuthGate_HasLiveToken_ValidBearer_AuthPassed).
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusInternalServerError {
		t.Errorf("canvas client: expected no auth error; got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Legacy workspace: HAS live token flag but workspace exists AND ValidateToken
// is called. Since the workspace has a live token, the handler MUST validate
// the presented token (not grandfather through). This is the Phase 30.1/30.2
// contract — a workspace with tokens on file is NOT grandfathered.
// ─────────────────────────────────────────────────────────────────────────────

func TestSocketHandler_AuthGate_HasLiveToken_EmptyBearer_Returns401(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, mock, nil)

	wsID := "ws-has-live-token-empty-bearer"

	// HasAnyLiveToken: workspace has a live token.
	mock.ExpectQuery("SELECT COUNT").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))

	// Authorization header is "Bearer " (empty token after "Bearer ").
	// wsauth.BearerTokenFromHeader strips "Bearer " and gets "".
	// ValidateToken is called with "" → returns ErrInvalidToken before DB hit.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", wsID, "Bearer ")

	handler.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("empty bearer after Bearer prefix: expected 401, got %d", w.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// sha256TokenHash returns the SHA256 hash of a plaintext token, matching what
// wsauth.ValidateToken does internally before querying the DB.
func sha256TokenHash(plaintext string) []byte {
	h := sha256.Sum256([]byte(plaintext))
	return h[:]
}
