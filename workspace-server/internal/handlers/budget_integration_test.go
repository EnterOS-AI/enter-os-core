//go:build integration
// +build integration

// budget_integration_test.go — REAL Postgres integration tests for
// /workspaces/:id/budget (handlers/budget.go).
//
// Mirrors pending_uploads_integration_test.go /
// delegation_ledger_integration_test.go. Unit tests in budget_test.go
// pin the SQL shape (sqlmock); these tests pin the OBSERVABLE row state
// against real Postgres, including:
//   - GET returns budget_limit / monthly_spend / budget_remaining with
//     the exact null-vs-int math the production handler computes
//   - PATCH sets, clears, and rejects bad inputs (negative / missing /
//     non-numeric) against real DB rows
//   - existence check uses status != 'removed' (removed ws → 404)
//   - updated_at advances on PATCH
//   - PATCH re-reads + returns the same shape as GET
//
// Run with:
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	psql ... < workspace-server/migrations/001_workspaces.sql
//	psql ... < workspace-server/migrations/027_workspace_budget.up.sql
//	psql ... < workspace-server/migrations/20260529000000_workspace_multiperiod_budget.up.sql
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_Budget -v

package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

// integrationDB_Budget opens the integration PG connection, wipes our
// test rows, and hot-swaps the package-level db.DB. NOT SAFE for
// t.Parallel() — the global db.DB is shared.
func integrationDB_Budget(t *testing.T) *sql.DB {
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
	for _, stmt := range []string{
		// Wipe ledger rows first (workspace_id is TEXT, no FK, but
		// grouping the cleanup with workspaces makes intent clear).
		// Cast `id::text` because workspace_spend_events.workspace_id
		// is TEXT while workspaces.id is UUID — without the cast,
		// Postgres rejects the IN comparison with `operator does not
		// exist: text = uuid`. The other test files don't hit this
		// because their join tables also store workspace_id as TEXT.
		`DELETE FROM workspace_spend_events WHERE workspace_id IN (SELECT id::text FROM workspaces WHERE name LIKE 'integ-bud-%')`,
		`DELETE FROM workspaces WHERE name LIKE 'integ-bud-%'`,
	} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("cleanup %q: %v", stmt, err)
		}
	}
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		conn.ExecContext(context.Background(), `DELETE FROM workspace_spend_events WHERE workspace_id IN (SELECT id::text FROM workspaces WHERE name LIKE 'integ-bud-%')`)
		conn.ExecContext(context.Background(), `DELETE FROM workspaces WHERE name LIKE 'integ-bud-%'`)
		db.DB = prev
		conn.Close()
	})
	return conn
}

// seedWorkspace_Budget inserts a workspaces row with the per-period
// budget_limits JSONB (the SSOT since migration 20260529000000) and,
// if monthlySpend > 0, a corresponding workspace_spend_events ledger
// row so the handler's rolling-window SUM picks it up. The legacy
// budget_limit / monthly_spend BIGINT columns are no longer the SSOT —
// the handler reads the JSONB + the ledger. The status is hardcoded
// to 'online' (a valid workspace_status enum value — see migration 043).
// The removed-status case uses a separate helper.
func seedWorkspace_Budget(t *testing.T, conn *sql.DB, id string, budgetLimit *int64, monthlySpend int64) {
	t.Helper()
	// Render the JSONB shape the handler expects: {"monthly":N} when a
	// limit is configured, {} otherwise. Absent keys = no limit (the
	// default) so we only mention periods that have a configured ceiling.
	limits := map[string]int64{}
	if budgetLimit != nil {
		limits["monthly"] = *budgetLimit
	}
	limitsJSON, err := json.Marshal(limits)
	if err != nil {
		t.Fatalf("seed: marshal limits: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status, budget_limits)
		 VALUES ($1, $2, 'online', $3::jsonb)`,
		id, "integ-bud-"+id, string(limitsJSON)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Record the monthly spend as a single ledger event with the full
	// delta. spendByPeriod sums delta_cents over the rolling window —
	// a single recent row (default occurred_at=now()) lands inside all
	// four windows (1h/24h/7d/30d), so the monthly figure the test
	// expects shows up regardless of which window the assertion targets.
	if monthlySpend > 0 {
		if _, err := conn.ExecContext(context.Background(),
			`INSERT INTO workspace_spend_events (workspace_id, delta_cents) VALUES ($1, $2)`,
			id, monthlySpend); err != nil {
			t.Fatalf("seed spend: %v", err)
		}
	}
}

// doPatch_Budget fires PATCH /workspaces/:id/budget with the given JSON body.
func doPatch_Budget(t *testing.T, h *BudgetHandler, workspaceID, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+workspaceID+"/budget", bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchBudget(c)
	return w
}

// doGet_Budget fires GET /workspaces/:id/budget.
func doGet_Budget(t *testing.T, h *BudgetHandler, workspaceID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+workspaceID+"/budget", nil)
	h.GetBudget(c)
	return w
}

// TestIntegration_Budget_GetPatchPersistsAndValidates pins the GET / PATCH
// surface against real Postgres: null math, set/clear, validation, existence
// check, updated_at advancement, and round-trip persistence (the
// "PersistsAndValidates" suffix matches the watch-fail-first name PM-cited
// in the #2151 CHUNK 2 dispatch).
func TestIntegration_Budget_GetPatchPersistsAndValidates(t *testing.T) {
	conn := integrationDB_Budget(t)
	handler := NewBudgetHandler()

	wsA := integUUID("integ-bud-ws-a")
	wsB := integUUID("integ-bud-ws-b")
	wsAOver := integUUID("integ-bud-ws-a-over")
	wsRemoved := integUUID("integ-bud-ws-removed")
	wsGhost := integUUID("integ-bud-ws-ghost")

	// Case A: no budget set (budget_limit NULL)
	// Case B: under budget (limit 10000, spend 2500 → remaining 7500)
	// Case C: over budget (limit 1000, spend 1500 → remaining -500, per
	//          the comment in budget.go: "Can be negative")
	seedWorkspace_Budget(t, conn, wsA, nil, 0)
	seedWorkspace_Budget(t, conn, wsB, int64Ptr(10000), 2500)
	overLim := int64(1000)
	seedWorkspace_Budget(t, conn, wsAOver, &overLim, 1500)
	// removed-workspace case (status='removed' so the handler's
	// `WHERE status != 'removed'` existence check rejects it with 404).
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status, budget_limits)
		 VALUES ($1, 'integ-bud-removed', 'removed', '{}'::jsonb)`, wsRemoved); err != nil {
		t.Fatalf("seed removed: %v", err)
	}

	// --- Case 1: GET — no budget set → budget_limit=nil, budget_remaining=nil, monthly_spend=0 ---
	w := doGet_Budget(t, handler, wsA)
	if w.Code != http.StatusOK {
		t.Fatalf("GET no-budget: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var r1 struct {
		BudgetLimit     *int64 `json:"budget_limit"`
		MonthlySpend    int64  `json:"monthly_spend"`
		BudgetRemaining *int64 `json:"budget_remaining"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r1); err != nil {
		t.Fatalf("GET no-budget: parse: %v", err)
	}
	if r1.BudgetLimit != nil {
		t.Errorf("GET no-budget: budget_limit want nil, got %d", *r1.BudgetLimit)
	}
	if r1.BudgetRemaining != nil {
		t.Errorf("GET no-budget: budget_remaining want nil, got %d", *r1.BudgetRemaining)
	}
	if r1.MonthlySpend != 0 {
		t.Errorf("GET no-budget: monthly_spend want 0, got %d", r1.MonthlySpend)
	}

	// --- Case 2: GET — under budget → remaining = limit - spend (positive) ---
	w = doGet_Budget(t, handler, wsB)
	if w.Code != http.StatusOK {
		t.Fatalf("GET under: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var r2 struct {
		BudgetLimit     *int64 `json:"budget_limit"`
		MonthlySpend    int64  `json:"monthly_spend"`
		BudgetRemaining *int64 `json:"budget_remaining"`
	}
	json.Unmarshal(w.Body.Bytes(), &r2)
	if r2.BudgetLimit == nil || *r2.BudgetLimit != 10000 {
		t.Errorf("GET under: budget_limit want 10000, got %v", r2.BudgetLimit)
	}
	if r2.MonthlySpend != 2500 {
		t.Errorf("GET under: monthly_spend want 2500, got %d", r2.MonthlySpend)
	}
	if r2.BudgetRemaining == nil || *r2.BudgetRemaining != 7500 {
		t.Errorf("GET under: budget_remaining want 7500, got %v", r2.BudgetRemaining)
	}

	// --- Case 3: GET — over budget → remaining is NEGATIVE (per budget.go doc) ---
	w = doGet_Budget(t, handler, wsAOver)
	if w.Code != http.StatusOK {
		t.Fatalf("GET over: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var r3 struct {
		BudgetLimit     *int64 `json:"budget_limit"`
		MonthlySpend    int64  `json:"monthly_spend"`
		BudgetRemaining *int64 `json:"budget_remaining"`
	}
	json.Unmarshal(w.Body.Bytes(), &r3)
	if r3.BudgetRemaining == nil || *r3.BudgetRemaining != -500 {
		t.Errorf("GET over: budget_remaining want -500, got %v", r3.BudgetRemaining)
	}

	// --- Case 4: GET — removed workspace → 404 (existence check) ---
	w = doGet_Budget(t, handler, wsRemoved)
	if w.Code != http.StatusNotFound {
		t.Errorf("GET removed: status want 404, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 5: GET — unknown workspace → 404 ---
	w = doGet_Budget(t, handler, wsGhost)
	if w.Code != http.StatusNotFound {
		t.Errorf("GET ghost: status want 404, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 6: PATCH — set budget_limit on wsA from NULL → 5000, persist + re-read ---
	before := time.Now().UTC().Add(-2 * time.Second)
	w = doPatch_Budget(t, handler, wsA, `{"budget_limit": 5000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH set: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var p1 struct {
		BudgetLimit     *int64 `json:"budget_limit"`
		MonthlySpend    int64  `json:"monthly_spend"`
		BudgetRemaining *int64 `json:"budget_remaining"`
	}
	json.Unmarshal(w.Body.Bytes(), &p1)
	if p1.BudgetLimit == nil || *p1.BudgetLimit != 5000 {
		t.Errorf("PATCH set: budget_limit want 5000, got %v", p1.BudgetLimit)
	}
	// re-read: GET should now return limit=5000, spend=0, remaining=5000
	w = doGet_Budget(t, handler, wsA)
	json.Unmarshal(w.Body.Bytes(), &p1)
	if p1.BudgetLimit == nil || *p1.BudgetLimit != 5000 {
		t.Errorf("PATCH set re-read: budget_limit want 5000, got %v", p1.BudgetLimit)
	}
	if p1.MonthlySpend != 0 {
		t.Errorf("PATCH set re-read: monthly_spend want 0, got %d", p1.MonthlySpend)
	}
	if p1.BudgetRemaining == nil || *p1.BudgetRemaining != 5000 {
		t.Errorf("PATCH set re-read: budget_remaining want 5000, got %v", p1.BudgetRemaining)
	}
	// updated_at advanced
	var updatedAt time.Time
	if err := conn.QueryRowContext(context.Background(),
		`SELECT updated_at FROM workspaces WHERE id = $1`, wsA).Scan(&updatedAt); err != nil {
		t.Fatalf("updated_at: %v", err)
	}
	if !updatedAt.After(before) {
		t.Errorf("PATCH set: updated_at want > %v, got %v", before, updatedAt)
	}

	// --- Case 7: PATCH — clear budget_limit (explicit null) → 200, GET returns nil ---
	w = doPatch_Budget(t, handler, wsA, `{"budget_limit": null}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH clear: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	w = doGet_Budget(t, handler, wsA)
	json.Unmarshal(w.Body.Bytes(), &p1)
	if p1.BudgetLimit != nil {
		t.Errorf("PATCH clear re-read: budget_limit want nil, got %d", *p1.BudgetLimit)
	}

	// --- Case 8: PATCH — negative budget_limit → 400 ---
	w = doPatch_Budget(t, handler, wsA, `{"budget_limit": -1}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("PATCH negative: status want 400, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 9: PATCH — missing budget_limit field → 400 ---
	w = doPatch_Budget(t, handler, wsA, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("PATCH missing: status want 400, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 10: PATCH — non-numeric budget_limit → 400 ---
	w = doPatch_Budget(t, handler, wsA, `{"budget_limit": "abc"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("PATCH non-numeric: status want 400, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 11: PATCH — unknown workspace → 404 ---
	w = doPatch_Budget(t, handler, wsGhost, `{"budget_limit": 1000}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("PATCH ghost: status want 404, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 12: PATCH — removed workspace → 404 (existence check) ---
	w = doPatch_Budget(t, handler, wsRemoved, `{"budget_limit": 1000}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("PATCH removed: status want 404, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 13: PATCH — set then update again, PATCH response shape matches GET ---
	w = doPatch_Budget(t, handler, wsB, `{"budget_limit": 8000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH update: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	w = doGet_Budget(t, handler, wsB)
	json.Unmarshal(w.Body.Bytes(), &p1)
	if p1.BudgetLimit == nil || *p1.BudgetLimit != 8000 {
		t.Errorf("PATCH update re-read: budget_limit want 8000, got %v", p1.BudgetLimit)
	}
	// monthly_spend unchanged at 2500
	if p1.MonthlySpend != 2500 {
		t.Errorf("PATCH update re-read: monthly_spend want 2500, got %d", p1.MonthlySpend)
	}
	// remaining = 8000 - 2500 = 5500
	if p1.BudgetRemaining == nil || *p1.BudgetRemaining != 5500 {
		t.Errorf("PATCH update re-read: budget_remaining want 5500, got %v", p1.BudgetRemaining)
	}
}

// int64Ptr returns &i — small helper so call sites stay readable.
func int64Ptr(i int64) *int64 { return &i }
