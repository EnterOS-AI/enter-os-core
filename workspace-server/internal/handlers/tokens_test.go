//go:build integration
// +build integration

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func init() { gin.SetMode(gin.TestMode) }

// setupTokenTestDB connects to $INTEGRATION_DB_URL (skipping the test if
// unset), sets the package-global db.DB for the duration of the test, and
// returns a cleanup func that restores the previous db.DB value.
func setupTokenTestDB(t *testing.T) func() {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping (local devs: start a Postgres container and export INTEGRATION_DB_URL)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open integration DB: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping integration DB: %v", err)
	}
	prevDB := db.DB
	db.DB = conn
	return func() {
		db.DB = prevDB
		conn.Close()
	}
}

func TestIntegration_TokenHandler_CreateAndList(t *testing.T) {
	cleanup := setupTokenTestDB(t)
	defer cleanup()

	// Create a test workspace first
	wsID := createTestWorkspace(t)
	defer deleteTestWorkspace(t, wsID)

	h := NewTokenHandler()

	// Create a token
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/tokens", nil)
	h.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("Create: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var createResp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &createResp)
	if createResp["auth_token"] == nil || createResp["auth_token"] == "" {
		t.Fatal("Create: auth_token missing from response")
	}
	if createResp["workspace_id"] != wsID {
		t.Errorf("Create: workspace_id mismatch: got %v", createResp["workspace_id"])
	}

	// List tokens
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Params = gin.Params{{Key: "id", Value: wsID}}
	c2.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/tokens", nil)
	h.List(c2)

	if w2.Code != http.StatusOK {
		t.Fatalf("List: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	var listResp struct {
		Tokens []map[string]interface{} `json:"tokens"`
		Count  int                      `json:"count"`
	}
	json.Unmarshal(w2.Body.Bytes(), &listResp)
	if listResp.Count < 1 {
		t.Errorf("List: expected at least 1 token, got %d", listResp.Count)
	}

	// Verify token has prefix but NOT the full plaintext
	tok := listResp.Tokens[0]
	if tok["prefix"] == nil || tok["prefix"] == "" {
		t.Error("List: prefix missing")
	}
	if tok["id"] == nil {
		t.Error("List: id missing")
	}
	if _, hasAuth := tok["auth_token"]; hasAuth {
		t.Error("List: auth_token should NOT be in list response")
	}
}

func TestIntegration_TokenHandler_Revoke(t *testing.T) {
	cleanup := setupTokenTestDB(t)
	defer cleanup()

	wsID := createTestWorkspace(t)
	defer deleteTestWorkspace(t, wsID)

	// Issue a token directly
	token, err := wsauth.IssueToken(context.Background(), db.DB, wsID)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	_ = token // we don't need the plaintext, just the DB row

	// Find the token ID
	var tokenID string
	err = db.DB.QueryRow(`
		SELECT id FROM workspace_auth_tokens
		WHERE workspace_id = $1 AND revoked_at IS NULL
		ORDER BY created_at DESC LIMIT 1
	`, wsID).Scan(&tokenID)
	if err != nil {
		t.Fatalf("find token: %v", err)
	}

	h := NewTokenHandler()

	// Revoke it
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "tokenId", Value: tokenID}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsID+"/tokens/"+tokenID, nil)
	h.Revoke(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Revoke: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify it's actually revoked
	var revokedAt sql.NullTime
	db.DB.QueryRow(`SELECT revoked_at FROM workspace_auth_tokens WHERE id = $1`, tokenID).Scan(&revokedAt)
	if !revokedAt.Valid {
		t.Error("Revoke: revoked_at should be set")
	}

	// Revoking again should 404
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "tokenId", Value: tokenID}}
	c2.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsID+"/tokens/"+tokenID, nil)
	h.Revoke(c2)

	if w2.Code != http.StatusNotFound {
		t.Errorf("Revoke again: expected 404, got %d", w2.Code)
	}
}

func TestIntegration_TokenHandler_RevokeWrongWorkspace(t *testing.T) {
	cleanup := setupTokenTestDB(t)
	defer cleanup()

	wsID := createTestWorkspace(t)
	defer deleteTestWorkspace(t, wsID)

	wsauth.IssueToken(context.Background(), db.DB, wsID)

	var tokenID string
	db.DB.QueryRow(`
		SELECT id FROM workspace_auth_tokens
		WHERE workspace_id = $1 AND revoked_at IS NULL LIMIT 1
	`, wsID).Scan(&tokenID)

	h := NewTokenHandler()

	// Try to revoke with a different (valid-UUID) workspace ID that does
	// not own the token — should 404. A valid UUID is required so this
	// exercises the ownership branch, not the up-front uuid-shape 400.
	otherWS := uuid.NewString()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: otherWS}, {Key: "tokenId", Value: tokenID}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+otherWS+"/tokens/"+tokenID, nil)
	h.Revoke(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("Revoke wrong workspace: expected 404, got %d", w.Code)
	}
}

// createTestWorkspace inserts a minimal workspace row for testing.
func createTestWorkspace(t *testing.T) string {
	t.Helper()
	var id string
	err := db.DB.QueryRow(`
		INSERT INTO workspaces (name, status, tier) VALUES ('fixture-token-ws', 'online', 2)
		RETURNING id
	`).Scan(&id)
	if err != nil {
		t.Fatalf("create test workspace: %v", err)
	}
	return id
}

func deleteTestWorkspace(t *testing.T, id string) {
	t.Helper()
	db.DB.Exec(`DELETE FROM workspace_auth_tokens WHERE workspace_id = $1`, id)
	db.DB.Exec(`DELETE FROM workspaces WHERE id = $1`, id)
}
