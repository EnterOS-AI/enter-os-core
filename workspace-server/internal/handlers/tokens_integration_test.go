//go:build integration
// +build integration

// tokens_integration_test.go — REAL Postgres integration tests for
// /workspaces/:id/tokens (GET/POST/DELETE — handlers/tokens.go).
//
// Mirrors pending_uploads_integration_test.go /
// delegation_ledger_integration_test.go. Unit tests in tokens_test.go
// pin the SQL shape; these tests pin the OBSERVABLE row state:
//   - POST mints via real wsauth.IssueToken, plaintext returned once
//   - workspace_auth_tokens has exactly one row with sha256(token_hash)
//   - GET returns only non-revoked rows
//   - DELETE sets revoked_at; subsequent DELETE is 404
//   - max-active-cap (50) returns 429
//
// Run with:
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	psql ... < workspace-server/migrations/001_workspaces.sql
//	psql ... < workspace-server/migrations/020_workspace_auth_tokens.up.sql
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_Tokens -v

package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

// integrationDB_Tokens opens the integration PG connection, wipes our
// test rows, and hot-swaps the package-level db.DB. NOT SAFE for
// t.Parallel() — the global db.DB is shared.
func integrationDB_Tokens(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping (local devs: see file header)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM workspace_auth_tokens WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'integ-tok-%')`); err != nil {
		t.Fatalf("cleanup tokens: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM workspaces WHERE name LIKE 'integ-tok-%'`); err != nil {
		t.Fatalf("cleanup workspaces: %v", err)
	}
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		conn.ExecContext(context.Background(), `DELETE FROM workspace_auth_tokens WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'integ-tok-%')`)
		conn.ExecContext(context.Background(), `DELETE FROM workspaces WHERE name LIKE 'integ-tok-%'`)
		db.DB = prev
		conn.Close()
	})
	return conn
}

func seedWorkspace_Tokens(t *testing.T, conn *sql.DB, id string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status) VALUES ($1, $2, 'online')`,
		id, "integ-tok-"+id); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// countActiveTokens returns COUNT(*) of non-revoked tokens for the workspace.
func countActiveTokens(t *testing.T, conn *sql.DB, workspaceID string) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM workspace_auth_tokens WHERE workspace_id = $1 AND revoked_at IS NULL`,
		workspaceID).Scan(&n); err != nil {
		t.Fatalf("count active: %v", err)
	}
	return n
}

// TestIntegration_Tokens_CreateListRevoke_RoundTrip pins the full
// create → list → revoke lifecycle and the max-active-cap 429 path.
func TestIntegration_Tokens_CreateListRevoke_RoundTrip(t *testing.T) {
	conn := integrationDB_Tokens(t)
	handler := NewTokenHandler()

	wsA := integUUID("integ-tok-ws-a")
	wsB := integUUID("integ-tok-ws-b")
	seedWorkspace_Tokens(t, conn, wsA)
	seedWorkspace_Tokens(t, conn, wsB)

	// --- Case 1: POST mints, plaintext once, DB row has matching sha256 ---
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsA+"/tokens", nil)
	handler.Create(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST: status want 201, got %d: %s", w.Code, w.Body.String())
	}
	var mint1 struct {
		AuthToken   string `json:"auth_token"`
		WorkspaceID string `json:"workspace_id"`
		Message     string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &mint1); err != nil {
		t.Fatalf("POST: parse: %v", err)
	}
	if mint1.AuthToken == "" {
		t.Fatal("POST: auth_token empty")
	}
	if mint1.WorkspaceID != wsA {
		t.Errorf("POST: workspace_id want %q, got %q", wsA, mint1.WorkspaceID)
	}
	// Verify the row in workspace_auth_tokens: count should be 1, and
	// the row's token_hash should be sha256(mint1.AuthToken).
	if n := countActiveTokens(t, conn, wsA); n != 1 {
		t.Errorf("POST: active count want 1, got %d", n)
	}
	want := sha256.Sum256([]byte(mint1.AuthToken))
	var hashMatch int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM workspace_auth_tokens WHERE workspace_id = $1 AND token_hash = $2`,
		wsA, want[:]).Scan(&hashMatch); err != nil {
		t.Fatalf("verify hash: %v", err)
	}
	if hashMatch != 1 {
		t.Errorf("POST: want exactly 1 row with sha256(token), got %d", hashMatch)
	}

	// --- Case 2: POST second token, GET lists both (non-revoked only) ---
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsA+"/tokens", nil)
	handler.Create(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST 2: status want 201, got %d: %s", w.Code, w.Body.String())
	}
	if n := countActiveTokens(t, conn, wsA); n != 2 {
		t.Errorf("after 2 mints: active count want 2, got %d", n)
	}

	// GET should return 2 tokens.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsA+"/tokens", nil)
	handler.List(c)
	if w.Code != http.StatusOK {
		t.Fatalf("LIST: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var list1 struct {
		Tokens []tokenListItem `json:"tokens"`
		Count  int             `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list1); err != nil {
		t.Fatalf("LIST: parse: %v", err)
	}
	if list1.Count != 2 || len(list1.Tokens) != 2 {
		t.Errorf("LIST: want 2 tokens, got count=%d len=%d", list1.Count, len(list1.Tokens))
	}
	// The list should NOT include the plaintext or hash.
	for _, tk := range list1.Tokens {
		if tk.Prefix == "" {
			t.Errorf("LIST: token prefix empty (got %+v)", tk)
		}
	}

	// --- Case 3: GET filters out revoked tokens (pre-revoke + post-revoke check) ---
	// Pick the first token's ID, revoke it, then GET — should return 1.
	targetID := list1.Tokens[0].ID
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}, {Key: "tokenId", Value: targetID}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsA+"/tokens/"+targetID, nil)
	handler.Revoke(c)
	if w.Code != http.StatusOK {
		t.Fatalf("REVOKE: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	// Verify revoked_at is set in DB.
	var revokedAt sql.NullTime
	if err := conn.QueryRowContext(context.Background(),
		`SELECT revoked_at FROM workspace_auth_tokens WHERE id = $1`, targetID).Scan(&revokedAt); err != nil {
		t.Fatalf("read revoked_at: %v", err)
	}
	if !revokedAt.Valid {
		t.Errorf("REVOKE: revoked_at in DB should be set, got NULL")
	}

	// GET after revoke: should show only 1 token.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsA+"/tokens", nil)
	handler.List(c)
	var list2 struct {
		Count int `json:"count"`
	}
	json.Unmarshal(w.Body.Bytes(), &list2)
	if list2.Count != 1 {
		t.Errorf("LIST after revoke: want 1, got %d", list2.Count)
	}

	// --- Case 4: DELETE on already-revoked token → 404 ---
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}, {Key: "tokenId", Value: targetID}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsA+"/tokens/"+targetID, nil)
	handler.Revoke(c)
	if w.Code != http.StatusNotFound {
		t.Errorf("REVOKE revoked: status want 404, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 5: max-active-cap (50) — seed 50, then 51st → 429 ---
	wsCap := integUUID("integ-tok-ws-cap")
	seedWorkspace_Tokens(t, conn, wsCap)
	// Insert 50 active tokens directly to avoid hammering IssueToken 50 times.
	for i := 0; i < maxTokensPerWorkspace; i++ {
		if _, err := conn.ExecContext(context.Background(),
			`INSERT INTO workspace_auth_tokens (workspace_id, token_hash, prefix) VALUES ($1, $2, $3)`,
			wsCap, []byte{byte(i)}, "pre"); err != nil {
			t.Fatalf("seed cap: %v", err)
		}
	}
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsCap}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsCap+"/tokens", nil)
	handler.Create(c)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("max-cap: status want 429, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 6: wsB is isolated — its tokens don't show in wsA's list ---
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsB}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsB+"/tokens", nil)
	handler.Create(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST wsB: status want 201, got %d: %s", w.Code, w.Body.String())
	}
	// wsA should still have 1 active (the one not revoked).
	if n := countActiveTokens(t, conn, wsA); n != 1 {
		t.Errorf("isolation: wsA active count want 1, got %d", n)
	}
	if n := countActiveTokens(t, conn, wsB); n != 1 {
		t.Errorf("isolation: wsB active count want 1, got %d", n)
	}
}

// keep the import block referenced even if a case is removed in a future edit.
var _ = bytes.NewReader
