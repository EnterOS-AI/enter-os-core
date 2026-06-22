package handlers

// cross_tenant_isolation_test.go — #1953 regression tests.
//
// Three workspace-server paths historically derived an "org-root sibling set"
// as `WHERE parent_id IS NULL`, which matches EVERY tenant's org root (the
// workspaces table has no org_id column) → cross-tenant data exposure:
//
//  1. GET /registry/:id/peers   (discovery.Peers)
//  2. MCP toolListPeers          (mcp_tools.toolListPeers)
//  3. a2a routing                (a2a_proxy.proxyA2ARequest → resolveAgentURL)
//
// These tests assert that a workspace in a DIFFERENT org is never returned as a
// peer and that a2a refuses to resolve/route to a workspace outside the caller's
// org, while same-org peers/targets still work. They reuse the SAME parent_id-
// chain org scoping the OFFSEC-015 broadcast fix introduced (org_scope.go).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// dbHandleForTest returns the global sqlmock-backed *sql.DB that setupTestDB
// installs, for tests that need to hand a *sql.DB to a component (e.g.
// MCPHandler.database, sameOrg) rather than relying on the package-global.
func dbHandleForTest() *sql.DB { return db.DB }

// peerColsForIsolation matches queryPeerMaps' SELECT column set.
var peerColsForIsolation = []string{
	"id", "name", "role", "tier", "status", "agent_card", "url", "parent_id", "active_tasks",
}

// -------------------------------------------------------------------------
// Path 1: GET /registry/:id/peers — discovery.Peers
// -------------------------------------------------------------------------

// TestPeers_CrossTenant_OrgRootNotLeaked is the core #1953 regression for the
// discovery path. The caller is an org root (parent_id IS NULL). Pre-fix the
// handler ran `SELECT ... WHERE w.parent_id IS NULL AND w.id != $1`, returning
// every OTHER tenant's org root as a "sibling" peer. Post-fix an org-root caller
// issues NO sibling query — its only peers are its own children. If the handler
// regressed and issued the cross-tenant sibling query, sqlmock would report an
// unexpected query (the expectation below is intentionally NOT registered) and
// the test fails.
func TestPeers_CrossTenant_OrgRootNotLeaked(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// Behavioural leak test: register the OLD leaky `parent_id IS NULL` sibling
	// query so that IF the handler still issues it, it returns another tenant's
	// org root (org-b-root). The fix removes that query for an org-root caller,
	// so org-b-root must never appear in the output. Unordered matching makes
	// the leaky-sibling expectation optional — the fix simply never consumes it.
	mock.MatchExpectationsInOrder(false)

	caller := "org-a-root" // parent_id IS NULL — an org root for tenant A

	// validateDiscoveryCaller probes HasAnyLiveToken(:id) first; grandfather.
	// (Unordered match is set above, so this can be consumed at any point.)
	seedDiscoveryGrandfather(mock, caller)

	// parent_id lookup → NULL (caller is an org root)
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	// LEAKY sibling query (pre-fix). Returns a DIFFERENT tenant's org root.
	// The fix must NOT issue this query; if it does, org-b-root leaks into the
	// peer list and the output assertion below fails.
	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.parent_id IS NULL AND w.id != \\$1").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows(peerColsForIsolation).
			AddRow("org-b-root", "Org B Root", "lead", 0, "online", []byte("null"), "http://b-root", nil, 0))

	// Children query — caller's own org-A children only. Return one child.
	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.parent_id = \\$1 AND w.id != \\$2").
		WithArgs(caller, caller).
		WillReturnRows(sqlmock.NewRows(peerColsForIsolation).
			AddRow("org-a-child", "Org A Child", "worker", 1, "online", []byte("null"), "http://a-child", caller, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: caller}}
	c.Request = httptest.NewRequest("GET", "/registry/"+caller+"/peers", nil)

	handler.Peers(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var peers []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &peers); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// The other-tenant org root must NEVER appear; only the same-org child.
	for _, p := range peers {
		if id, _ := p["id"].(string); id == "org-b-root" {
			t.Fatalf("cross-tenant leak (#1953): org-b-root appeared in org-a-root's peer list: %v", peers)
		}
	}
	if len(peers) != 1 {
		t.Fatalf("expected exactly 1 peer (same-org child), got %d: %v", len(peers), peers)
	}
	// NOTE: ExpectationsWereMet is intentionally NOT asserted — the leaky
	// sibling expectation is deliberately left unconsumed by the fixed path.
}

// TestPeers_SameOrg_SiblingsStillWork is the positive companion: a non-root
// child caller still sees its same-org siblings, children, and parent. This
// guards against the fix over-scoping and breaking legitimate intra-org
// discovery.
func TestPeers_SameOrg_SiblingsStillWork(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	caller := "org-a-child-1"
	parent := "org-a-root"

	// validateDiscoveryCaller probes HasAnyLiveToken(:id) first; grandfather.
	seedDiscoveryGrandfather(mock, caller)

	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(parent))

	// Siblings — scoped to the shared parent (one tenant).
	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.parent_id = \\$1 AND w.id != \\$2").
		WithArgs(parent, caller).
		WillReturnRows(sqlmock.NewRows(peerColsForIsolation).
			AddRow("org-a-child-2", "Org A Sibling", "worker", 1, "online", []byte("null"), "http://a-sib", parent, 0))

	// Children — none.
	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.parent_id = \\$1 AND w.id != \\$2 AND w.status").
		WithArgs(caller, caller).
		WillReturnRows(sqlmock.NewRows(peerColsForIsolation))

	// Parent.
	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.id = \\$1 AND w.id != \\$2 AND w.status").
		WithArgs(parent, caller).
		WillReturnRows(sqlmock.NewRows(peerColsForIsolation).
			AddRow(parent, "Org A Root", "lead", 0, "online", []byte("null"), "http://a-root", nil, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: caller}}
	c.Request = httptest.NewRequest("GET", "/registry/"+caller+"/peers", nil)

	handler.Peers(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var peers []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &peers); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	// Sibling + parent = 2 same-org peers.
	if len(peers) != 2 {
		t.Fatalf("expected 2 same-org peers (sibling + parent), got %d: %v", len(peers), peers)
	}
	names := map[string]bool{}
	for _, p := range peers {
		names[fmt.Sprint(p["name"])] = true
	}
	if !names["Org A Sibling"] || !names["Org A Root"] {
		t.Errorf("expected same-org sibling + parent in peer list, got %v", names)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// -------------------------------------------------------------------------
// Path 2: MCP toolListPeers — mcp_tools.toolListPeers
// -------------------------------------------------------------------------

// mcpPeerCols matches toolListPeers' SELECT column set.
var mcpPeerCols = []string{"id", "name", "role", "status", "tier"}

// TestToolListPeers_CrossTenant_OrgRootNotLeaked is the #1953 regression for
// the MCP path. Same shape as the discovery test: an org-root caller must NOT
// enumerate other tenants' org roots. The cross-tenant `parent_id IS NULL`
// sibling query is intentionally not registered, so if it runs sqlmock fails.
func TestToolListPeers_CrossTenant_OrgRootNotLeaked(t *testing.T) {
	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	h := &MCPHandler{database: dbHandleForTest()}

	caller := "org-a-root"

	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	// LEAKY sibling query (pre-fix). Returns another tenant's org root. The fix
	// must NOT issue this for an org-root caller; if it does, org-b-root leaks
	// into the output and the assertion below fails. Left optional via
	// unordered matching, so the fixed path simply never consumes it.
	mock.ExpectQuery("WHERE w.parent_id IS NULL AND w.id != \\$1").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows(mcpPeerCols).
			AddRow("org-b-root", "Org B Root", "lead", "online", 0))

	// Children — caller's own org-A children only.
	mock.ExpectQuery("WHERE w.parent_id = \\$1 AND w.status").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows(mcpPeerCols).
			AddRow("org-a-child", "Org A Child", "worker", "online", 1))

	out, err := h.toolListPeers(context.Background(), caller)
	if err != nil {
		t.Fatalf("toolListPeers returned error: %v", err)
	}
	if strings.Contains(out, "org-b-root") || strings.Contains(out, "Org B Root") {
		t.Fatalf("cross-tenant leak (#1953): another tenant's org root appeared in toolListPeers output:\n%s", out)
	}
	if !strings.Contains(out, "org-a-child") {
		t.Errorf("same-org child missing from toolListPeers output:\n%s", out)
	}
	// ExpectationsWereMet intentionally NOT asserted — leaky sibling expectation
	// is deliberately left unconsumed by the fixed path.
}

// TestToolListPeers_SameOrg_SiblingsStillWork — positive companion for the MCP
// path: a non-root child still enumerates its same-org siblings + children + parent.
func TestToolListPeers_SameOrg_SiblingsStillWork(t *testing.T) {
	mock := setupTestDB(t)
	h := &MCPHandler{database: dbHandleForTest()}

	caller := "org-a-child-1"
	parent := "org-a-root"

	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(parent))

	// Siblings — scoped to shared parent.
	mock.ExpectQuery("WHERE w.parent_id = \\$1 AND w.id != \\$2 AND w.status").
		WithArgs(parent, caller).
		WillReturnRows(sqlmock.NewRows(mcpPeerCols).
			AddRow("org-a-child-2", "Org A Sibling", "worker", "online", 1))

	// Children — none.
	mock.ExpectQuery("WHERE w.parent_id = \\$1 AND w.status").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows(mcpPeerCols))

	// Parent.
	mock.ExpectQuery("WHERE w.id = \\$1 AND w.status").
		WithArgs(parent).
		WillReturnRows(sqlmock.NewRows(mcpPeerCols).
			AddRow(parent, "Org A Root", "lead", "online", 0))

	out, err := h.toolListPeers(context.Background(), caller)
	if err != nil {
		t.Fatalf("toolListPeers returned error: %v", err)
	}
	if !strings.Contains(out, "Org A Sibling") || !strings.Contains(out, "Org A Root") {
		t.Errorf("expected same-org sibling + parent in toolListPeers output:\n%s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// -------------------------------------------------------------------------
// Path 3: a2a routing — a2a_proxy.proxyA2ARequest / resolveAgentURL
// -------------------------------------------------------------------------

// TestProxyA2A_CrossTenant_RoutingDenied is the #1953 regression for a2a
// routing. Caller and target are both org roots (parent_id IS NULL) belonging
// to DIFFERENT tenants. Pre-fix, CanCommunicate's "root-level siblings" rule
// waved this through and resolveAgentURL routed to the foreign tenant. Post-fix
// the org-scope guard resolves each to a different org root and returns 403
// BEFORE resolveAgentURL/dispatch.
func TestProxyA2A_CrossTenant_RoutingDenied(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	caller := "org-a-root"
	target := "org-b-root" // different tenant

	// A URL exists for the target; the guard must deny BEFORE it is used.
	mr.Set(fmt.Sprintf("ws:%s:url", target), "http://localhost:1")

	// Post-#1955: CanCommunicate no longer has the root-sibling bypass.
	// Both root-level (parent_id NULL) but unrelated org roots → hierarchy
	// check DENIES with 403 BEFORE the org-scope guard or resolveAgentURL.
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id = ").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow(caller, nil))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id = ").
		WithArgs(target).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow(target, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: target}}
	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"cross-tenant"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+target+"/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Workspace-ID", caller)

	handler.ProxyA2A(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-tenant a2a routing, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "cannot communicate") {
		t.Errorf("expected hierarchy denial message, got %v", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestResolveAgentURL_CrossTenant_RejectedViaSameOrg is a direct unit test of
// the sameOrg primitive that gates resolveAgentURL: a target in a different org
// must be reported as NOT same-org, so the a2a guard rejects it before
// resolveAgentURL is ever called.
func TestResolveAgentURL_CrossTenant_RejectedViaSameOrg(t *testing.T) {
	mock := setupTestDB(t)

	caller := "org-a-root"
	target := "org-b-root"

	mock.ExpectQuery("WITH RECURSIVE org_chain AS").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(caller))
	mock.ExpectQuery("WITH RECURSIVE org_chain AS").
		WithArgs(target).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(target))

	ok, err := sameOrg(context.Background(), dbHandleForTest(), caller, target)
	if err != nil {
		t.Fatalf("sameOrg returned unexpected error: %v", err)
	}
	if ok {
		t.Errorf("expected cross-tenant workspaces to be reported as DIFFERENT orgs, got sameOrg=true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestProxyA2A_SameOrg_RoutingAllowed — positive companion for a2a: two
// same-org siblings route successfully (mirrors TestProxyA2A_CallerIDPropagated
// but named to document the #1953 same-org allow path).
func TestProxyA2A_SameOrg_RoutingAllowed(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	waitForHandlerAsyncBeforeDBCleanup(t, handler)

	caller := "org-a-child-1"
	target := "org-a-child-2"
	parent := "org-a-root"

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{}}`)
	}))
	defer agentServer.Close()
	mr.Set(fmt.Sprintf("ws:%s:url", target), agentServer.URL)

	// CanCommunicate — siblings under shared parent.
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id = ").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow(caller, parent))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id = ").
		WithArgs(target).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow(target, parent))

	// #1953 org guard — both resolve to the same org root → allowed.
	mock.ExpectQuery("WITH RECURSIVE org_chain AS").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(parent))
	mock.ExpectQuery("WITH RECURSIVE org_chain AS").
		WithArgs(target).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(parent))

	expectBudgetCheck(mock, target)
	mock.ExpectExec("INSERT INTO activity_logs").WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: target}}
	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"same-org"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+target+"/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Workspace-ID", caller)

	handler.ProxyA2A(c)
	time.Sleep(50 * time.Millisecond) // allow the async logA2ASuccess INSERT to flush

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for same-org a2a routing, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
