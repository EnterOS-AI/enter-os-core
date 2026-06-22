//go:build integration
// +build integration

// approval_gate_admin_token_integration_test.go — REAL Postgres coverage for the
// core#2574 admin-token approval gate on org-token mint and secret write.
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_AdminTokenGate -v
//
// Why this is NOT a sqlmock test
// ------------------------------
// The gate is about row-state across calls: the first call creates a pending
// approval row, the second call (after the human approves) consumes it via the
// conditional UPDATE ... RETURNING, and a third call creates a fresh pending row
// because the first approval was single-use. Only real Postgres proves the
// consume-once semantics and the hash-matching lookup.

package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func integrationDB_AdminTokenGate(t *testing.T) *sql.DB {
	t.Helper()
	url := requireIntegrationDBURL(t)
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// seedConciergeWorkspace inserts a root workspace (parent_id NULL) that acts as
// the org concierge / platform agent. The gate's approval rows are keyed by
// workspace_id, so a real row must exist.
//
// (core#2579) This helper ALSO sets the MOLECULE_PLATFORM_WORKSPACE_ID
// env var to the seeded workspace UUID so the production approvalAnchorForGate(c)
// logic (workspace-server/internal/handlers/org_tokens.go:approvalAnchorForGate)
// resolves a real anchor for admin-token callers in the integration tests.
// Without this env var, approvalAnchorForGate returns "" → the handler
// returns the controlled 400 "no approval anchor" → the 3 tests that
// expect 202 (pending_approval) would falsely FAIL. The env var is
// restored on cleanup.
func seedConciergeWorkspace(t *testing.T, conn *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	wsID := uuid.New().String()
	name := "gate-itest-" + wsID[:8]
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
		VALUES ($1, $2, 0, 'claude-code', 'online', NULL)
	`, wsID, name); err != nil {
		t.Fatalf("seed concierge workspace: %v", err)
	}

	// (core#2579) Set the platform-org workspace anchor for admin-token
	// approval flows. The handler resolves admin-token callers' anchor
	// from this env var (org_tokens.go:approvalAnchorForGate). Pre-#2579
	// the tests didn't need this because the gate was INERT for admin
	// tokens; post-#2579 the gate is always-on for admin-token, so the
	// anchor must resolve to a real seeded workspace for the gate to
	// fire (return 202) instead of fail-closed (return 400).
	prevEnv, hadPrev := os.LookupEnv("MOLECULE_PLATFORM_WORKSPACE_ID")
	os.Setenv("MOLECULE_PLATFORM_WORKSPACE_ID", wsID)

	t.Cleanup(func() {
		if hadPrev {
			os.Setenv("MOLECULE_PLATFORM_WORKSPACE_ID", prevEnv)
		} else {
			os.Unsetenv("MOLECULE_PLATFORM_WORKSPACE_ID")
		}
		_, _ = conn.ExecContext(ctx, `DELETE FROM approval_requests WHERE workspace_id = $1`, wsID)
		_, _ = conn.ExecContext(ctx, `DELETE FROM org_api_tokens WHERE name LIKE $1`, name+"%")
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspace_secrets WHERE workspace_id = $1`, wsID)
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, wsID)
	})
	return wsID
}

// adminTokenContext returns a gin.Context that simulates the AdminAuth middleware
// for a Tier-2b ADMIN_TOKEN caller (core#2574).
func adminTokenContext(t *testing.T, method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	c.Request = r
	c.Set("caller_is_admin_token", true)
	c.Set("caller_credential_class", "admin-token")
	return c, w
}

// TestIntegration_AdminToken_OrgTokenMint_WithoutApproval_Rejected proves that
// an admin-token caller (the concierge agent) attempting to mint an org API
// token WITHOUT a pre-existing approval gets HTTP 202 with a pending approval.
// This is the regression for the live exploit (core#2574).
func TestIntegration_AdminToken_OrgTokenMint_WithoutApproval_Rejected(t *testing.T) {
	conn := integrationDB_AdminTokenGate(t)
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev })
	setupTestRedis(t)

	wsID := seedConciergeWorkspace(t, conn)
	t.Setenv("MOLECULE_PLATFORM_WORKSPACE_ID", wsID)
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	h := NewOrgTokenHandler()
	c, w := adminTokenContext(t, "POST", "/org/tokens", `{"name":"exploit-test-mint"}`)

	h.Create(c)

	if w.Code != http.StatusAccepted {
		t.Fatalf("admin-token org-token mint WITHOUT approval: want 202, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body["status"] != "pending_approval" {
		t.Errorf("status = %v, want pending_approval", body["status"])
	}
	if body["action"] != "org_token_mint" {
		t.Errorf("action = %v, want org_token_mint", body["action"])
	}
	approvalID, ok := body["approval_id"].(string)
	if !ok || approvalID == "" {
		t.Fatalf("approval_id missing or empty in 202 body: %v", body)
	}

	// Verify the pending row was actually persisted.
	var count int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM approval_requests WHERE id = $1 AND status = 'pending'`, approvalID).Scan(&count); err != nil {
		t.Fatalf("count pending row: %v", err)
	}
	if count != 1 {
		t.Fatalf("pending approval rows = %d, want 1", count)
	}
}

// TestIntegration_AdminToken_SecretWrite_WithoutApproval_Rejected proves that
// an admin-token caller attempting to write a workspace secret WITHOUT a
// pre-existing approval gets HTTP 202 with a pending approval (core#2574).
func TestIntegration_AdminToken_SecretWrite_WithoutApproval_Rejected(t *testing.T) {
	conn := integrationDB_AdminTokenGate(t)
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev })
	setupTestRedis(t)

	wsID := seedConciergeWorkspace(t, conn)
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	h := NewSecretsHandler(nil)
	c, w := adminTokenContext(t, "POST", "/workspaces/"+wsID+"/secrets",
		fmt.Sprintf(`{"key":"GATED_SECRET_KEY","value":"gated-secret-value"}`))
	c.Params = gin.Params{{Key: "id", Value: wsID}}

	h.Set(c)

	if w.Code != http.StatusAccepted {
		t.Fatalf("admin-token secret write WITHOUT approval: want 202, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body["status"] != "pending_approval" {
		t.Errorf("status = %v, want pending_approval", body["status"])
	}
	if body["action"] != "secret_write" {
		t.Errorf("action = %v, want secret_write", body["action"])
	}
	approvalID, ok := body["approval_id"].(string)
	if !ok || approvalID == "" {
		t.Fatalf("approval_id missing or empty in 202 body: %v", body)
	}

	var count int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM approval_requests WHERE id = $1 AND status = 'pending'`, approvalID).Scan(&count); err != nil {
		t.Fatalf("count pending row: %v", err)
	}
	if count != 1 {
		t.Fatalf("pending approval rows = %d, want 1", count)
	}
}

// TestIntegration_AdminToken_OrgTokenMint_WithApproval_Succeeds proves the
// happy path: after a human grants the pending approval, the retried mint
// consumes the approval and returns 200 with the plaintext token.
func TestIntegration_AdminToken_OrgTokenMint_WithApproval_Succeeds(t *testing.T) {
	conn := integrationDB_AdminTokenGate(t)
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev })
	setupTestRedis(t)

	wsID := seedConciergeWorkspace(t, conn)
	t.Setenv("MOLECULE_PLATFORM_WORKSPACE_ID", wsID)
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	h := NewOrgTokenHandler()
	name := "approved-mint-test"

	// 1. First call → pending.
	c1, w1 := adminTokenContext(t, "POST", "/org/tokens", fmt.Sprintf(`{"name":%q}`, name))
	h.Create(c1)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first call: want 202, got %d: %s", w1.Code, w1.Body.String())
	}
	var body1 map[string]interface{}
	if err := json.Unmarshal(w1.Body.Bytes(), &body1); err != nil {
		t.Fatalf("parse first: %v", err)
	}
	approvalID := body1["approval_id"].(string)

	// 2. Human grants the approval via DB (simulating the Decide handler).
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE approval_requests SET status='approved', decided_by='integration-test', decided_at=now() WHERE id=$1`,
		approvalID); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// 3. Retry with the SAME name → hash matches → approval consumed → 200.
	c2, w2 := adminTokenContext(t, "POST", "/org/tokens", fmt.Sprintf(`{"name":%q}`, name))
	h.Create(c2)
	if w2.Code != http.StatusOK {
		t.Fatalf("retry after approval: want 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var body2 map[string]interface{}
	if err := json.Unmarshal(w2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("parse second: %v", err)
	}
	if body2["auth_token"] == "" {
		t.Errorf("auth_token missing from 200 response — mint did not proceed")
	}
	if body2["id"] == "" {
		t.Errorf("id missing from 200 response")
	}

	// 4. The approval row is now consumed.
	var consumedAt *string
	if err := conn.QueryRowContext(context.Background(),
		`SELECT consumed_at::text FROM approval_requests WHERE id=$1`, approvalID).Scan(&consumedAt); err != nil {
		t.Fatalf("read consumed_at: %v", err)
	}
	if consumedAt == nil || *consumedAt == "" {
		t.Fatalf("approval %s was not consumed after the 200 mint", approvalID)
	}
}

// TestIntegration_AdminToken_SecretWrite_WithApproval_Succeeds proves the
// happy path: after a human grants the pending approval, the retried secret
// write consumes the approval and returns 200.
func TestIntegration_AdminToken_SecretWrite_WithApproval_Succeeds(t *testing.T) {
	conn := integrationDB_AdminTokenGate(t)
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev })
	setupTestRedis(t)

	wsID := seedConciergeWorkspace(t, conn)
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	h := NewSecretsHandler(nil)
	key := "GATED_SECRET_KEY"

	// 1. First call → pending.
	c1, w1 := adminTokenContext(t, "POST", "/workspaces/"+wsID+"/secrets",
		fmt.Sprintf(`{"key":%q,"value":"secret-value"}`, key))
	c1.Params = gin.Params{{Key: "id", Value: wsID}}
	h.Set(c1)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first call: want 202, got %d: %s", w1.Code, w1.Body.String())
	}
	var body1 map[string]interface{}
	if err := json.Unmarshal(w1.Body.Bytes(), &body1); err != nil {
		t.Fatalf("parse first: %v", err)
	}
	approvalID := body1["approval_id"].(string)

	// 2. Human grants the approval.
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE approval_requests SET status='approved', decided_by='integration-test', decided_at=now() WHERE id=$1`,
		approvalID); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// 3. Retry with the SAME key → hash matches → approval consumed → 200.
	c2, w2 := adminTokenContext(t, "POST", "/workspaces/"+wsID+"/secrets",
		fmt.Sprintf(`{"key":%q,"value":"secret-value"}`, key))
	c2.Params = gin.Params{{Key: "id", Value: wsID}}
	h.Set(c2)
	if w2.Code != http.StatusOK {
		t.Fatalf("retry after approval: want 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var body2 map[string]interface{}
	if err := json.Unmarshal(w2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("parse second: %v", err)
	}
	if body2["status"] != "saved" {
		t.Errorf("status = %v, want saved", body2["status"])
	}
	if body2["key"] != key {
		t.Errorf("key = %v, want %s", body2["key"], key)
	}

	// 4. Approval consumed.
	var consumedAt *string
	if err := conn.QueryRowContext(context.Background(),
		`SELECT consumed_at::text FROM approval_requests WHERE id=$1`, approvalID).Scan(&consumedAt); err != nil {
		t.Fatalf("read consumed_at: %v", err)
	}
	if consumedAt == nil || *consumedAt == "" {
		t.Fatalf("approval %s was not consumed after the 200 write", approvalID)
	}
}

// TestIntegration_AdminToken_OrgTokenMint_ExploitRegression blocks the exact
// live-exploit scenario: a concierge holding ADMIN_TOKEN tries to mint a
// full-tenant-admin org token with ZERO pending approvals, and the gate MUST
// reject it with 202. The old code (pre-core#2574) would have bypassed the
// gate entirely and returned 200 with a live token.
func TestIntegration_AdminToken_OrgTokenMint_ExploitRegression(t *testing.T) {
	conn := integrationDB_AdminTokenGate(t)
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev })
	setupTestRedis(t)

	wsID := seedConciergeWorkspace(t, conn)
	t.Setenv("MOLECULE_PLATFORM_WORKSPACE_ID", wsID)

	// The exploit ran with the default rollout flag OFF (no
	// MOLECULE_PLATFORM_APPROVAL_GATE env var set). That is the
	// regression posture: the gate must fire EVEN when the flag is off,
	// because admin-token callers are ALWAYS gated.
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	h := NewOrgTokenHandler()
	c, w := adminTokenContext(t, "POST", "/org/tokens", `{"name":"concierge-exploit-token"}`)

	h.Create(c)

	if w.Code == http.StatusOK {
		var body map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		t.Fatalf("EXPLOIT REGRESSION: admin-token mint returned 200 (token created) with ZERO approvals — %v", body)
	}
	if w.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: want 202, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body["status"] != "pending_approval" {
		t.Errorf("status = %v, want pending_approval", body["status"])
	}

	// Verify NO org token was actually minted.
	var count int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM org_api_tokens WHERE name = $1`, "concierge-exploit-token").Scan(&count); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if count != 0 {
		t.Fatalf("EXPLOIT REGRESSION: %d org token(s) minted despite zero approvals", count)
	}
}
