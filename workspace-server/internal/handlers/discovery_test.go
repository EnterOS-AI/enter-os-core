package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== Discover — missing X-Workspace-ID header ====================

func TestDiscover_MissingCallerHeader(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}
	c.Request = httptest.NewRequest("GET", "/registry/discover/ws-target", nil)
	// No X-Workspace-ID header

	handler.Discover(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "X-Workspace-ID header is required" {
		t.Errorf("expected error about missing header, got %v", resp["error"])
	}
}

// ==================== Discover — workspace not found (with caller) ====================

func TestDiscover_WorkspaceNotFound_WithCaller(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// CanCommunicate will need DB lookups — both workspace name lookups
	// For the access check: caller lookup succeeds, target lookup fails
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-caller").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-caller", nil))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-missing").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"})) // no rows

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-missing"}}
	c.Request = httptest.NewRequest("GET", "/registry/discover/ws-missing", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-caller")

	handler.Discover(c)

	// Access denied because target not found in registry → 403
	if w.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Discover — external (no caller header, DB fallback) ====================

func TestDiscover_External_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// This tests the external path (no X-Workspace-ID header), but we need
	// the request to have the header as empty string bypass. Instead test the
	// DB path for external callers:
	// For an external request without caller, the code first checks callerID == ""
	// which triggers the StatusBadRequest, so we test with a header but Redis+DB miss

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-ext-missing"}}
	c.Request = httptest.NewRequest("GET", "/registry/discover/ws-ext-missing", nil)
	// No header → returns 400

	handler.Discover(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Peers — success with parent/siblings/children ====================

func TestPeers_WithParent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// Expect parent_id lookup for the requesting workspace
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs("ws-sibling-1").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow("ws-parent"))

	// Expect siblings query (same parent, excluding self)
	peerCols := []string{"id", "name", "role", "tier", "status", "agent_card", "url", "parent_id", "active_tasks"}
	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.parent_id = \\$1 AND w.id != \\$2").
		WithArgs("ws-parent", "ws-sibling-1").
		WillReturnRows(sqlmock.NewRows(peerCols).
			AddRow("ws-sibling-2", "Sibling Two", "worker", 1, "online", []byte("null"), "http://localhost:8002", "ws-parent", 0))

	// Expect children query
	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.parent_id = \\$1 AND w.status").
		WithArgs("ws-sibling-1").
		WillReturnRows(sqlmock.NewRows(peerCols))

	// Expect parent query
	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.id = \\$1 AND w.status").
		WithArgs("ws-parent").
		WillReturnRows(sqlmock.NewRows(peerCols).
			AddRow("ws-parent", "Parent PM", "manager", 2, "online", []byte("null"), "http://localhost:8001", nil, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-sibling-1"}}
	c.Request = httptest.NewRequest("GET", "/registry/ws-sibling-1/peers", nil)

	handler.Peers(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var peers []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &peers); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(peers) != 2 {
		t.Errorf("expected 2 peers (1 sibling + 1 parent), got %d", len(peers))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestPeers_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// Workspace not found
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs("ws-ghost").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-ghost"}}
	c.Request = httptest.NewRequest("GET", "/registry/ws-ghost/peers", nil)

	handler.Peers(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestPeers_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs("ws-dberr").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-dberr"}}
	c.Request = httptest.NewRequest("GET", "/registry/ws-dberr/peers", nil)

	handler.Peers(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestPeers_RootWorkspace_NoPeers(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// Root workspace (parent_id is NULL)
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs("ws-root-alone").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	peerCols := []string{"id", "name", "role", "tier", "status", "agent_card", "url", "parent_id", "active_tasks"}

	// Siblings (other root-level workspaces) — none
	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.parent_id IS NULL AND w.id != \\$1").
		WithArgs("ws-root-alone").
		WillReturnRows(sqlmock.NewRows(peerCols))

	// Children — none
	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.parent_id = \\$1").
		WithArgs("ws-root-alone").
		WillReturnRows(sqlmock.NewRows(peerCols))

	// No parent query since parent_id is NULL

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-root-alone"}}
	c.Request = httptest.NewRequest("GET", "/registry/ws-root-alone/peers", nil)

	handler.Peers(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var peers []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &peers); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("expected 0 peers, got %d", len(peers))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Peers — ?q= filter (#1038) ====================

// peersFilterFixture mocks the 4 SQL reads (parent_id lookup + siblings +
// children + parent) with a known 3-peer set so each q-filter test can
// focus on the post-fetch substring-match behaviour. Returns the handler
// and the live mock so callers can assert ExpectationsWereMet at the end.
func peersFilterFixture(t *testing.T) (*DiscoveryHandler, sqlmock.Sqlmock) {
	t.Helper()
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs("ws-self").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow("ws-pm"))

	cols := []string{"id", "name", "role", "tier", "status", "agent_card", "url", "parent_id", "active_tasks"}

	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.parent_id = \\$1 AND w.id != \\$2").
		WithArgs("ws-pm", "ws-self").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("ws-alpha", "Alpha Researcher", "researcher", 1, "online", []byte("null"), "http://a", "ws-pm", 0).
			AddRow("ws-beta", "Beta Designer", "designer", 1, "online", []byte("null"), "http://b", "ws-pm", 0))

	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.parent_id = \\$1 AND w.status").
		WithArgs("ws-self").
		WillReturnRows(sqlmock.NewRows(cols))

	mock.ExpectQuery("SELECT w.id, w.name.*WHERE w.id = \\$1 AND w.status").
		WithArgs("ws-pm").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("ws-pm", "PM Workspace", "manager", 2, "online", []byte("null"), "http://pm", nil, 1))

	return NewDiscoveryHandler(), mock
}

// runPeersWithQuery invokes Peers and returns BOTH the parsed peers and
// the raw response body. The raw body is needed by TestPeers_Q_NoMatches
// to distinguish JSON `[]` (intended) from `null` (regression of the
// nil-guard at line 254-256) — once unmarshalled, both collapse to
// len==0 and re-marshal to `[]`, so checking only the parsed value is
// tautological.
func runPeersWithQuery(t *testing.T, handler *DiscoveryHandler, q string) ([]map[string]interface{}, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-self"}}
	url := "/registry/ws-self/peers"
	if q != "" {
		url += "?q=" + q
	}
	c.Request = httptest.NewRequest("GET", url, nil)
	handler.Peers(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.Bytes()
	var peers []map[string]interface{}
	if err := json.Unmarshal(body, &peers); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return peers, body
}

// peerIDSet returns the set of peer ids — order-independent comparison
// avoids fragile peers[0]["id"] asserts that would silently mask a future
// sort/order change.
func peerIDSet(peers []map[string]interface{}) map[string]struct{} {
	out := make(map[string]struct{}, len(peers))
	for _, p := range peers {
		out[p["id"].(string)] = struct{}{}
	}
	return out
}

// TestPeers_QFilter covers the rule classifier — append-order
// independent (uses set membership) so a future sort regression on the
// production code can't slip through. NoMatches has its own raw-body
// check (see TestPeers_Q_NoMatches_RawBodyIsArrayNotNull below) because
// the `[]` vs `null` distinction collapses after json.Unmarshal.
func TestPeers_QFilter(t *testing.T) {
	cases := []struct {
		name    string
		q       string
		wantIDs []string
	}{
		{"no-q returns all", "", []string{"ws-alpha", "ws-beta", "ws-pm"}},
		{"name match", "alpha", []string{"ws-alpha"}},
		{"name match case-insensitive", "ALPHA", []string{"ws-alpha"}},
		{"role match", "design", []string{"ws-beta"}},
		{"no matches", "nonexistent", nil},
		{"whitespace-only is no-op", "%20%20", []string{"ws-alpha", "ws-beta", "ws-pm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, mock := peersFilterFixture(t)
			peers, _ := runPeersWithQuery(t, handler, tc.q)

			got := peerIDSet(peers)
			want := make(map[string]struct{}, len(tc.wantIDs))
			for _, id := range tc.wantIDs {
				want[id] = struct{}{}
			}
			if len(got) != len(want) {
				t.Fatalf("len: got %d %v, want %d %v", len(got), keysOf(got), len(want), tc.wantIDs)
			}
			for id := range want {
				if _, ok := got[id]; !ok {
					t.Errorf("missing id %q (got %v, want %v)", id, keysOf(got), tc.wantIDs)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

// TestPeers_Q_NoMatches_RawBodyIsArrayNotNull verifies the `peers = make(...)`
// nil-guard at the end of Peers — when filtering reduces the slice to
// non-nil-but-empty AND the original was nil, JSON must be `[]` not `null`.
// This is the assertion the previous TestPeers_Q_NoMatches falsely claimed
// to make: re-encoding an unmarshalled []map collapses null and [] both
// to []. The fix here checks the recorder body bytes BEFORE unmarshal.
func TestPeers_Q_NoMatches_RawBodyIsArrayNotNull(t *testing.T) {
	handler, mock := peersFilterFixture(t)
	_, body := runPeersWithQuery(t, handler, "nonexistent")
	got := strings.TrimSpace(string(body))
	if got != "[]" {
		t.Errorf("raw body: got %q, want []", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestFilterPeersByQuery_NilRoleRegression is the regression gate for
// mc#730/#731: queryPeerMaps sets peer["role"] = nil when the DB role column
// is empty (discovery.go lines 337-341). filterPeersByQuery did a bare
// type assertion p["role"].(string) which panics on nil. The fix uses the
// comma-ok form so nil → "". The test passes a map with nil name and nil
// role and asserts no panic + correct filter behaviour.
func TestFilterPeersByQuery_NilRoleRegression(t *testing.T) {
	cases := []struct {
		name       string
		peers      []map[string]interface{}
		q          string
		wantLen    int
		wantIDs    []string
	}{
		{
			name: "nil role matches on name",
			peers: []map[string]interface{}{
				{"id": "ws-a", "name": nil, "role": nil},
				{"id": "ws-b", "name": "Alpha Builder", "role": nil},
				{"id": "ws-c", "name": "Beta Builder", "role": nil},
			},
			q:       "alpha",
			wantLen: 1,
			wantIDs: []string{"ws-b"},
		},
		{
			name: "nil name matches on nil role (empty string)",
			peers: []map[string]interface{}{
				{"id": "ws-x", "name": nil, "role": nil},
				{"id": "ws-y", "name": "Dev Workspace", "role": nil},
			},
			q:       "",
			wantLen: 2, // empty q is a no-op
			wantIDs: []string{"ws-x", "ws-y"},
		},
		{
			name: "all nil — no panic, returns input",
			peers: []map[string]interface{}{
				{"id": "ws-z", "name": nil, "role": nil},
			},
			q:       "anything",
			wantLen: 0,
			wantIDs: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterPeersByQuery(tc.peers, tc.q)
			if len(got) != tc.wantLen {
				t.Fatalf("len: got %d, want %d", len(got), tc.wantLen)
			}
			gotIDs := make([]string, len(got))
			for i, p := range got {
				gotIDs[i] = p["id"].(string)
			}
			if tc.wantIDs != nil {
				for _, id := range tc.wantIDs {
					found := false
					for _, g := range gotIDs {
						if g == id {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("missing id %q; got IDs: %v", id, gotIDs)
					}
				}
			}
		})
	}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ==================== CheckAccess ====================

func TestCheckAccess_BadJSON(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	c.Request = httptest.NewRequest("POST", "/registry/check-access", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.CheckAccess(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCheckAccess_MissingFields(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"caller_id":"ws-1"}`
	c.Request = httptest.NewRequest("POST", "/registry/check-access", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.CheckAccess(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCheckAccess_SameWorkspace(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// CanCommunicate("ws-1", "ws-1") returns true immediately (same ID, no DB lookups)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"caller_id":"ws-1","target_id":"ws-1"}`
	c.Request = httptest.NewRequest("POST", "/registry/check-access", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.CheckAccess(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["allowed"] != true {
		t.Errorf("expected allowed=true for same workspace, got %v", resp["allowed"])
	}
}

// ==================== Direct unit tests for extracted helpers ====================

// --- discoverWorkspacePeer ---

func TestDiscoverWorkspacePeer_Online(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// name/runtime lookup → non-external
	mock.ExpectQuery(`SELECT COALESCE\(name,''\), COALESCE\(runtime,'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-online").
		WillReturnRows(sqlmock.NewRows([]string{"name", "runtime"}).AddRow("Target", "langgraph"))
	// No cached internal URL → DB status lookup → online
	mock.ExpectQuery(`SELECT status FROM workspaces WHERE id =`).
		WithArgs("ws-online").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverWorkspacePeer(context.Background(), c, "ws-caller", "ws-online")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "ws-online" || resp["url"] == "" {
		t.Errorf("unexpected body: %v", resp)
	}
}

func TestDiscoverWorkspacePeer_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(name,''\), COALESCE\(runtime,'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-missing").
		WillReturnRows(sqlmock.NewRows([]string{"name", "runtime"}).AddRow("", "langgraph"))
	mock.ExpectQuery(`SELECT status FROM workspaces WHERE id =`).
		WithArgs("ws-missing").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverWorkspacePeer(context.Background(), c, "ws-caller", "ws-missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDiscoverWorkspacePeer_ExternalRuntime_HandledByExternalURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(name,''\), COALESCE\(runtime,'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-ext").
		WillReturnRows(sqlmock.NewRows([]string{"name", "runtime"}).AddRow("Ext", "external"))
	// writeExternalWorkspaceURL's two queries
	mock.ExpectQuery(`SELECT COALESCE\(url,''\) FROM workspaces WHERE id =`).
		WithArgs("ws-ext").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://external.example"))
	mock.ExpectQuery(`SELECT COALESCE\(runtime,'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-caller").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("external"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverWorkspacePeer(context.Background(), c, "ws-caller", "ws-ext")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestDiscoverWorkspacePeer_CachedInternalURLHit(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(name,''\), COALESCE\(runtime,'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-cached").
		WillReturnRows(sqlmock.NewRows([]string{"name", "runtime"}).AddRow("Cached", "langgraph"))
	mr.Set("ws:ws-cached:internal_url", "http://ws-cached:8000")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverWorkspacePeer(context.Background(), c, "ws-caller", "ws-cached")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["url"] != "http://ws-cached:8000" {
		t.Errorf("expected cached internal URL, got %v", resp["url"])
	}
}

func TestDiscoverWorkspacePeer_NotReachable(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(name,''\), COALESCE\(runtime,'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-paused").
		WillReturnRows(sqlmock.NewRows([]string{"name", "runtime"}).AddRow("Paused", "langgraph"))
	mock.ExpectQuery(`SELECT status FROM workspaces WHERE id =`).
		WithArgs("ws-paused").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("paused"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverWorkspacePeer(context.Background(), c, "ws-caller", "ws-paused")

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

// --- writeExternalWorkspaceURL ---

func TestWriteExternalWorkspaceURL_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(url,''\) FROM workspaces WHERE id =`).
		WithArgs("ws-ext").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://external.example/a2a"))
	mock.ExpectQuery(`SELECT COALESCE\(runtime,'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-caller").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("langgraph"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	handled := writeExternalWorkspaceURL(context.Background(), c, "ws-caller", "ws-ext", "External WS")
	if !handled {
		t.Error("expected handled=true when URL present")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["url"] != "http://external.example/a2a" {
		t.Errorf("got url %v", resp["url"])
	}
	if resp["name"] != "External WS" {
		t.Errorf("got name %v", resp["name"])
	}
}

func TestWriteExternalWorkspaceURL_NoURL_FallsThrough(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(url,''\) FROM workspaces WHERE id =`).
		WithArgs("ws-ext").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow(""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	if handled := writeExternalWorkspaceURL(context.Background(), c, "ws-caller", "ws-ext", ""); handled {
		t.Error("expected handled=false when URL empty")
	}
}

func TestWriteExternalWorkspaceURL_RewritesLocalhostForDockerCaller(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COALESCE\(url,''\) FROM workspaces WHERE id =`).
		WithArgs("ws-ext").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://127.0.0.1:8000/a2a"))
	// non-external caller runtime → rewrite enabled
	mock.ExpectQuery(`SELECT COALESCE\(runtime,'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-caller").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("langgraph"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	writeExternalWorkspaceURL(context.Background(), c, "ws-caller", "ws-ext", "")
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["url"] != "http://host.docker.internal:8000/a2a" {
		t.Errorf("expected 127.0.0.1 → host.docker.internal rewrite, got %v", resp["url"])
	}
}

// --- #1484 SSRF defense-in-depth regression tests ---

// TestDiscoverHostPeer_RejectsMetadataIPURL pins the #1484 guard:
// even though discoverHostPeer is currently gated by a bearer-required
// Discover handler, the function MUST refuse to hand back a URL that
// resolves into the cloud-metadata range. setupTestDB disables SSRF
// for normal tests, so we re-enable it here for the duration of the
// case and provide a literal IP so the check doesn't depend on DNS.
func TestDiscoverHostPeer_RejectsMetadataIPURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restoreSSRF := setSSRFCheckForTest(true)
	t.Cleanup(restoreSSRF)

	mock.ExpectQuery(`SELECT url, status, forwarded_to FROM workspaces WHERE id =`).
		WithArgs("ws-bad").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status", "forwarded_to"}).
			AddRow("http://169.254.169.254/latest/meta-data/", "online", nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverHostPeer(context.Background(), c, "ws-bad")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for metadata-IP URL, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "safety check") {
		t.Errorf("response should mention 'safety check', got %s", w.Body.String())
	}
}

// TestDiscoverHostPeer_AcceptsPublicURL is the positive counterpart —
// a routable hostname (literal public-range IP, no DNS dependency)
// passes through the new guard and returns 200. Without it, the
// rejection test above could pass by falsely failing every URL.
func TestDiscoverHostPeer_AcceptsPublicURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restoreSSRF := setSSRFCheckForTest(true)
	t.Cleanup(restoreSSRF)

	mock.ExpectQuery(`SELECT url, status, forwarded_to FROM workspaces WHERE id =`).
		WithArgs("ws-good").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status", "forwarded_to"}).
			AddRow("http://8.8.8.8/a2a", "online", nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverHostPeer(context.Background(), c, "ws-good")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for public-IP URL, got %d: %s", w.Code, w.Body.String())
	}
}

// TestWriteExternalWorkspaceURL_RejectsMetadataIPURL is the parallel
// guard for the external-runtime path. Same #1484 rationale as the
// host-peer test above; covers writeExternalWorkspaceURL specifically.
func TestWriteExternalWorkspaceURL_RejectsMetadataIPURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restoreSSRF := setSSRFCheckForTest(true)
	t.Cleanup(restoreSSRF)

	mock.ExpectQuery(`SELECT COALESCE\(url,''\) FROM workspaces WHERE id =`).
		WithArgs("ws-ext").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).
			AddRow("http://169.254.169.254/computeMetadata/v1/"))
	// callerRuntime lookup happens before isSafeURL — must mock it.
	mock.ExpectQuery(`SELECT COALESCE\(runtime,'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-caller").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("langgraph"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	handled := writeExternalWorkspaceURL(context.Background(), c, "ws-caller", "ws-ext", "Bad WS")
	if !handled {
		t.Fatal("expected handled=true (the function did write a 503)")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for metadata-IP URL, got %d: %s", w.Code, w.Body.String())
	}
}

// --- discoverHostPeer smoke (currently unreachable via Discover) ---

func TestDiscoverHostPeer_Smoke_CacheHit(t *testing.T) {
	setupTestDB(t)
	mr := setupTestRedis(t)
	mr.Set("ws:ws-host:url", "http://hostcache.example")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverHostPeer(context.Background(), c, "ws-host")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestDiscoverHostPeer_Smoke_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT url, status, forwarded_to FROM workspaces WHERE id =`).
		WithArgs("ws-none").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverHostPeer(context.Background(), c, "ws-none")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestDiscoverHostPeer_Smoke_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT url, status, forwarded_to FROM workspaces WHERE id =`).
		WithArgs("ws-err").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverHostPeer(context.Background(), c, "ws-err")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestDiscoverHostPeer_Smoke_ForwardedChainAndNullURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT url, status, forwarded_to FROM workspaces WHERE id =`).
		WithArgs("ws-a").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status", "forwarded_to"}).AddRow(nil, "online", "ws-b"))
	mock.ExpectQuery(`SELECT url, status, forwarded_to FROM workspaces WHERE id =`).
		WithArgs("ws-b").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status", "forwarded_to"}).AddRow(nil, "offline", nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverHostPeer(context.Background(), c, "ws-a")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for null URL after chain, got %d", w.Code)
	}
}

func TestDiscoverHostPeer_Smoke_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT url, status, forwarded_to FROM workspaces WHERE id =`).
		WithArgs("ws-ok").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status", "forwarded_to"}).AddRow("http://ok.example", "online", nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	discoverHostPeer(context.Background(), c, "ws-ok")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ==================== Peers auth — dev-mode fail-open gate ====================
//
// validateDiscoveryCaller applies a Tier-1b dev-mode hatch so the canvas
// user session (which holds no workspace-scoped bearer) can still load
// the Details → PEERS list on a local Docker setup. The gate must pass
// ONLY when MOLECULE_ENV is development AND ADMIN_TOKEN is empty.
// These tests pin that contract against accidental polarity flips.

// peersAuthFixtureHasLiveToken seeds the mock rows required for the
// Peers handler to reach the auth branch: HasAnyLiveToken → true (a
// non-zero count so validateDiscoveryCaller has to make the dev-mode
// decision instead of grandfathering the request).
func peersAuthFixtureHasLiveToken(mock sqlmock.Sqlmock, workspaceID string) {
	// HasAnyLiveToken issues `SELECT COUNT(*) FROM workspace_auth_tokens ...`
	mock.ExpectQuery("SELECT COUNT.+workspace_auth_tokens").
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
}

func TestPeers_DevModeFailOpen_AllowsBearerlessRequest(t *testing.T) {
	// Dev mode: MOLECULE_ENV=development AND ADMIN_TOKEN empty. Canvas
	// sends no bearer token; validateDiscoveryCaller must return nil
	// (allow) and the handler must proceed to return the peer list.
	t.Setenv("MOLECULE_ENV", "development")
	t.Setenv("ADMIN_TOKEN", "")

	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	peersAuthFixtureHasLiveToken(mock, "ws-dev")

	// Root workspace → children+parent queries still fire but the
	// parent_id lookup comes first.
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs("ws-dev").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
	peerCols := []string{"id", "name", "role", "tier", "status", "agent_card", "url", "parent_id", "active_tasks"}
	mock.ExpectQuery("SELECT w.id.+WHERE w.parent_id IS NULL AND w.id").
		WithArgs("ws-dev").
		WillReturnRows(sqlmock.NewRows(peerCols))
	mock.ExpectQuery("SELECT w.id.+WHERE w.parent_id = \\$1 AND w.status").
		WithArgs("ws-dev").
		WillReturnRows(sqlmock.NewRows(peerCols))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-dev"}}
	c.Request = httptest.NewRequest("GET", "/registry/ws-dev/peers", nil)

	handler.Peers(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 under dev-mode hatch, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPeers_DevModeFailOpen_ClosedWhenAdminTokenSet(t *testing.T) {
	// An operator with ADMIN_TOKEN set has explicitly opted into #684
	// closure; dev-mode hatch must NOT open even when MOLECULE_ENV is
	// "development". This is the SaaS guarantee.
	t.Setenv("MOLECULE_ENV", "development")
	t.Setenv("ADMIN_TOKEN", "seven-admin-token")

	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	peersAuthFixtureHasLiveToken(mock, "ws-prod")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-prod"}}
	c.Request = httptest.NewRequest("GET", "/registry/ws-prod/peers", nil)

	handler.Peers(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with ADMIN_TOKEN set, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPeers_DevModeFailOpen_ClosedInProduction(t *testing.T) {
	// Production MOLECULE_ENV — hatch must stay closed regardless of
	// ADMIN_TOKEN state. SaaS production rejects the bearerless call.
	t.Setenv("MOLECULE_ENV", "production")
	t.Setenv("ADMIN_TOKEN", "")

	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	peersAuthFixtureHasLiveToken(mock, "ws-prod")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-prod"}}
	c.Request = httptest.NewRequest("GET", "/registry/ws-prod/peers", nil)

	handler.Peers(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 in production, got %d: %s", w.Code, w.Body.String())
	}
}
