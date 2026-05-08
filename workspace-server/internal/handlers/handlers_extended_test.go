package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ---------- TestWorkspaceDelete (Extended) ----------

func TestExtended_WorkspaceDelete(t *testing.T) {
	const wsDelID = "aaaaaaaa-0000-0000-0000-000000000001"
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	// Expect children query — no children
	mock.ExpectQuery("SELECT id, name FROM workspaces WHERE parent_id").
		WithArgs(wsDelID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	// CascadeDelete walks descendants unconditionally (the 0-children
	// optimization in the old inline path was dropped during the
	// CascadeDelete extraction — descendant CTE returns 0 rows here,
	// same end state, one extra cheap query).
	mock.ExpectQuery("WITH RECURSIVE descendants").
		WithArgs(wsDelID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	// #73: batch UPDATE happens BEFORE any container teardown.
	// Uses ANY($1::uuid[]) even with a single ID for consistency.
	mock.ExpectExec("UPDATE workspaces SET status =").
		WillReturnResult(sqlmock.NewResult(1, 1))

	// Batch canvas layout delete (same id set).
	mock.ExpectExec("DELETE FROM canvas_layouts WHERE workspace_id = ANY").
		WillReturnResult(sqlmock.NewResult(1, 1))

	// Token revocation for deleted workspaces.
	mock.ExpectExec("UPDATE workspace_auth_tokens SET revoked_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	// #1027: cascade-disable schedules for deleted workspaces.
	mock.ExpectExec("UPDATE workspace_schedules SET enabled = false").
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Expect RecordAndBroadcast INSERT for WORKSPACE_REMOVED
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsDelID}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsDelID+"?confirm=true", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "removed" {
		t.Errorf("expected status 'removed', got %v", resp["status"])
	}
	if resp["cascade_deleted"] != float64(0) {
		t.Errorf("expected cascade_deleted 0, got %v", resp["cascade_deleted"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestWorkspaceUpdate (Extended) ----------

func TestExtended_WorkspaceUpdate(t *testing.T) {
	const wsUpdID = "aaaaaaaa-0000-0000-0000-000000000002"
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	// #120 fix: existence check runs first — workspace must be found before updates proceed.
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(wsUpdID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// Expect name update
	mock.ExpectExec("UPDATE workspaces SET name").
		WithArgs(wsUpdID, "New Name").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect canvas position upsert (x and y both provided)
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WithArgs(wsUpdID, float64(150), float64(250)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsUpdID}}

	body := `{"name":"New Name","x":150,"y":250}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsUpdID, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "updated" {
		t.Errorf("expected status 'updated', got %v", resp["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestExtended_WorkspaceUpdate_NotFound verifies the #120 fix: PATCH /workspaces/:id
// returns 404 (not 200) when the workspace does not exist in the DB.
//
// Before PR #125, the handler ran blind UPDATEs that matched zero rows and still
// returned {"status":"updated"} HTTP 200 — allowing an attacker to probe and
// speculatively modify workspace attributes (name, tier, parent_id, runtime,
// workspace_dir) without any observable error.  The existence guard must fire
// and return 404 before any UPDATE is attempted.
func TestExtended_WorkspaceUpdate_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	// Existence check returns false — workspace does not exist.
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("00000000-0000-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// No UPDATE or INSERT should follow — the handler must short-circuit at 404.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000000"}}

	body := `{"name":"probe"}`
	c.Request = httptest.NewRequest("PATCH",
		"/workspaces/00000000-0000-0000-0000-000000000000",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("#120 regression: expected 404 for nonexistent workspace, got %d: %s",
			w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestWorkspaceRestart (Extended) ----------

func TestExtended_WorkspaceRestart_NoProvisioner(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	// provisioner is nil — should return 503
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	// Expect SELECT for workspace existence check (includes runtime column)
	mock.ExpectQuery("SELECT status, name, tier").
		WithArgs("ws-restart").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name", "tier", "runtime"}).AddRow("offline", "Restarting Agent", 1, "langgraph"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-restart"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-restart/restart", bytes.NewBufferString("{}"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Restart(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "provisioner not available" {
		t.Errorf("expected error 'provisioner not available', got %v", resp["error"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestSecretsListEmpty (Extended) ----------

func TestExtended_SecretsListEmpty(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	// Return empty rows
	mock.ExpectQuery("SELECT key, created_at, updated_at FROM workspace_secrets WHERE workspace_id").
		WithArgs("11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"key", "created_at", "updated_at"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "11111111-1111-1111-1111-111111111111"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/11111111-1111-1111-1111-111111111111/secrets", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected empty array, got %d items", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestSecretsSet (Extended) ----------

func TestExtended_SecretsSet(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	// Expect INSERT (encrypted value is dynamic, use AnyArg)
	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("22222222-2222-2222-2222-222222222222", "OPENAI_API_KEY", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "22222222-2222-2222-2222-222222222222"}}

	body := `{"key":"OPENAI_API_KEY","value":"sk-test-12345"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/22222222-2222-2222-2222-222222222222/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "saved" {
		t.Errorf("expected status 'saved', got %v", resp["status"])
	}
	if resp["key"] != "OPENAI_API_KEY" {
		t.Errorf("expected key 'OPENAI_API_KEY', got %v", resp["key"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestSecretsDelete (Extended) ----------

func TestExtended_SecretsDelete(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	// Expect DELETE
	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("33333333-3333-3333-3333-333333333333", "OLD_KEY").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "33333333-3333-3333-3333-333333333333"},
		{Key: "key", Value: "OLD_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/33333333-3333-3333-3333-333333333333/secrets/OLD_KEY", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "deleted" {
		t.Errorf("expected status 'deleted', got %v", resp["status"])
	}
	if resp["key"] != "OLD_KEY" {
		t.Errorf("expected key 'OLD_KEY', got %v", resp["key"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestDiscoverWithCallerID (Extended) ----------

func TestExtended_DiscoverWithCallerID(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// CanCommunicate needs to look up both workspaces
	// Caller: root-level (no parent)
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-caller").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-caller", nil))
	// Target: also root-level (no parent) — root-level siblings are allowed
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-target", nil))

	// Discover handler looks up workspace name + runtime
	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"name", "runtime"}).AddRow("Target Agent", "langgraph"))

	// No cached internal URL (Redis empty), so falls through to DB status check
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}
	c.Request = httptest.NewRequest("GET", "/registry/discover/ws-target", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-caller")

	handler.Discover(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["id"] != "ws-target" {
		t.Errorf("expected id 'ws-target', got %v", resp["id"])
	}
	if resp["name"] != "Target Agent" {
		t.Errorf("expected name 'Target Agent', got %v", resp["name"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestDiscoverMissingHeader (Extended) ----------

func TestExtended_DiscoverMissingHeader(t *testing.T) {
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

// ---------- TestPeers (Extended) ----------

func TestExtended_Peers(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// Expect parent_id lookup for requesting workspace (root-level, no parent)
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs("ws-peer").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	// Expect root-level siblings query (parent IS NULL, excluding self)
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("ws-peer").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "role", "tier", "status", "agent_card", "url", "parent_id", "active_tasks"}).
			AddRow("ws-sibling", "Sibling Agent", "worker", 1, "online", []byte("null"), "http://localhost:9001", nil, 0))

	// Expect children query (workspaces with parent_id = ws-peer)
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("ws-peer").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "role", "tier", "status", "agent_card", "url", "parent_id", "active_tasks"}))

	// No parent query since workspace is root-level

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-peer"}}
	c.Request = httptest.NewRequest("GET", "/registry/ws-peer/peers", nil)

	handler.Peers(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(resp))
	}
	if resp[0]["name"] != "Sibling Agent" {
		t.Errorf("expected peer name 'Sibling Agent', got %v", resp[0]["name"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestCheckAccess (Extended) ----------

func TestExtended_CheckAccess(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// CanCommunicate will look up both workspaces
	// Both root-level — should be allowed
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-a", nil))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-b").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-b", nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"caller_id":"ws-a","target_id":"ws-b"}`
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
		t.Errorf("expected allowed true, got %v", resp["allowed"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestBundleExportNotFound (Extended) ----------

func TestExtended_BundleExportNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewBundleHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs", nil)

	// bundle.Export queries workspace — return no rows
	mock.ExpectQuery("SELECT name").
		WithArgs("ws-nonexistent").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-nonexistent"}}
	c.Request = httptest.NewRequest("GET", "/bundles/export/ws-nonexistent", nil)

	handler.Export(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestConfigGet (Extended) ----------

func TestExtended_ConfigGet(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewConfigHandler()

	// Return config data
	mock.ExpectQuery("SELECT data FROM workspace_config WHERE workspace_id").
		WithArgs("ws-cfg").
		WillReturnRows(sqlmock.NewRows([]string{"data"}).AddRow([]byte(`{"model":"gpt-4","temperature":0.7}`)))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cfg"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-cfg/config", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// data should be the JSON object
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected data to be an object, got %T", resp["data"])
	}
	if data["model"] != "gpt-4" {
		t.Errorf("expected model 'gpt-4', got %v", data["model"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestConfigGet_Empty (Extended) ----------

func TestExtended_ConfigGet_Empty(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewConfigHandler()

	// Return no rows — should return empty object
	mock.ExpectQuery("SELECT data FROM workspace_config WHERE workspace_id").
		WithArgs("ws-new").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-new"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-new/config", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// data should be empty JSON object
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected data to be an object, got %T", resp["data"])
	}
	if len(data) != 0 {
		t.Errorf("expected empty config object, got %v", data)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestConfigPatch (Extended) ----------

func TestExtended_ConfigPatch(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewConfigHandler()

	// Expect upsert with JSONB merge
	mock.ExpectExec("INSERT INTO workspace_config").
		WithArgs("ws-cfg", `{"model":"gpt-4"}`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cfg"}}

	body := `{"model":"gpt-4"}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-cfg/config", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Patch(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "updated" {
		t.Errorf("expected status 'updated', got %v", resp["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ─── #687 UUID validation ──────────────────────────────────────────────────

func TestGet_InvalidUUID_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", "/tmp/configs")

	for _, badID := range []string{"not-a-uuid", "ws-123", "../etc/passwd", "123"} {
		t.Run(badID, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: badID}}
			c.Request = httptest.NewRequest("GET", "/workspaces/"+badID, nil)
			handler.Get(c)
			if w.Code != http.StatusBadRequest {
				t.Errorf("Get(%q): want 400, got %d", badID, w.Code)
			}
		})
	}
}

func TestUpdate_InvalidUUID_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", "/tmp/configs")

	for _, badID := range []string{"not-a-uuid", "ws-upd", "../../secret"} {
		t.Run(badID, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: badID}}
			body := `{"name":"x"}`
			c.Request = httptest.NewRequest("PATCH", "/workspaces/"+badID, bytes.NewBufferString(body))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.Update(c)
			if w.Code != http.StatusBadRequest {
				t.Errorf("Update(%q): want 400, got %d", badID, w.Code)
			}
		})
	}
}

func TestDelete_InvalidUUID_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", "/tmp/configs")

	for _, badID := range []string{"not-a-uuid", "ws-del", "foobar"} {
		t.Run(badID, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: badID}}
			c.Request = httptest.NewRequest("DELETE", "/workspaces/"+badID+"?confirm=true", nil)
			handler.Delete(c)
			if w.Code != http.StatusBadRequest {
				t.Errorf("Delete(%q): want 400, got %d", badID, w.Code)
			}
		})
	}
}

// ─── #685/#688 field validation ───────────────────────────────────────────

func TestValidateWorkspaceFields_Lengths(t *testing.T) {
	long256 := string(make([]byte, 256))
	long1001 := string(make([]byte, 1001))
	long101 := string(make([]byte, 101))

	cases := []struct {
		label                      string
		name, role, model, runtime string
		wantErr                    bool
	}{
		{"ok", "ok", "ok role", "gpt-4", "langgraph", false},
		{"name_too_long", long256, "", "", "", true},
		{"role_too_long", "", long1001, "", "", true},
		{"model_too_long", "", "", long101, "", true},
		{"runtime_too_long", "", "", "", long101, true},
		{"name_newline", "bad\nname", "", "", "", true},
		{"role_cr", "", "bad\rrole", "", "", true},
		{"model_newline", "", "", "bad\nmodel", "", true},
		{"runtime_newline", "", "", "", "bad\nruntime", true},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			err := validateWorkspaceFields(tc.name, tc.role, tc.model, tc.runtime)
			if tc.wantErr && err == nil {
				t.Errorf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("want nil, got %v", err)
			}
		})
	}
}

func TestCreate_FieldValidation_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", "/tmp/configs")

	cases := []struct{ label, body string }{
		{"name_newline", `{"name":"bad\nname"}`},
		{"role_cr", `{"name":"ok","role":"bad\rrole"}`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.Create(c)
			if w.Code != http.StatusBadRequest {
				t.Errorf("Create(%s): want 400, got %d: %s", tc.label, w.Code, w.Body.String())
			}
		})
	}
}

func TestUpdate_FieldValidation_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", "/tmp/configs")

	validID := "bbbbbbbb-0000-0000-0000-000000000001"
	cases := []struct{ label, body string }{
		{"name_newline", `{"name":"bad\nname"}`},
		{"role_cr", `{"name":"ok","role":"bad\rrole"}`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: validID}}
			c.Request = httptest.NewRequest("PATCH", "/workspaces/"+validID, bytes.NewBufferString(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.Update(c)
			if w.Code != http.StatusBadRequest {
				t.Errorf("Update(%s): want 400, got %d: %s", tc.label, w.Code, w.Body.String())
			}
		})
	}
}
