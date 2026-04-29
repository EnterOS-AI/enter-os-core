package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== Register — input validation ====================

func TestRegister_BadJSON(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRegister_MissingRequiredFields(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// Missing url and agent_card
	body := `{"id":"ws-123"}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRegister_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// DB insert fails
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-fail", "ws-fail", "http://localhost:8000", `{"name":"test"}`).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"id":"ws-fail","url":"http://localhost:8000","agent_card":{"name":"test"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Heartbeat — offline → online recovery ====================

func TestHeartbeatHandler_OfflineToOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-offline").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// Expect heartbeat UPDATE
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-offline", 0.0, "", 1, 5000, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect evaluateStatus SELECT — currently offline
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-offline").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("offline"))

	// Expect status transition back to online
	mock.ExpectExec("UPDATE workspaces SET status = 'online'").
		WithArgs("ws-offline").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect RecordAndBroadcast INSERT for WORKSPACE_ONLINE
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-offline","error_rate":0.0,"sample_error":"","active_tasks":1,"uptime_seconds":5000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Heartbeat — provisioning → online recovery (#1784) ====================

func TestHeartbeatHandler_ProvisioningToOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-provisioning").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// Expect heartbeat UPDATE
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-provisioning", 0.0, "", 1, 3000, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect evaluateStatus SELECT — currently provisioning
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-provisioning").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("provisioning"))

	// Expect status transition to online (#1784)
	mock.ExpectExec("UPDATE workspaces SET status = 'online'").
		WithArgs("ws-provisioning").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect RecordAndBroadcast INSERT for WORKSPACE_ONLINE
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-provisioning","error_rate":0.0,"sample_error":"","active_tasks":1,"uptime_seconds":3000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestHeartbeatHandler_BadJSON(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHeartbeatHandler_MissingWorkspaceID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"error_rate":0.1}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHeartbeatHandler_DBUpdateError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-dberr").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// Heartbeat UPDATE fails
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-dberr", 0.1, "", 0, 100, "").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-dberr","error_rate":0.1,"sample_error":"","active_tasks":0,"uptime_seconds":100}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Heartbeat — stable (no transition) ====================

func TestHeartbeatHandler_OnlineStaysOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-stable").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// Expect heartbeat UPDATE
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-stable", 0.2, "", 3, 4000, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus: online with error_rate 0.2 — below 0.5 threshold, stays online
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-stable").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-stable","error_rate":0.2,"sample_error":"","active_tasks":3,"uptime_seconds":4000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Heartbeat — runtime wedge (claude_agent_sdk init timeout) ====================

// TestHeartbeatHandler_RuntimeWedged_FlipsOnlineToDegraded verifies the
// runtime_state="wedged" path. Heartbeat task in the workspace lives in
// its own asyncio task and keeps reporting online while the Claude SDK
// is wedged on Control request timeout; the workspace tells us about
// the wedge via this field, and we honor it by flipping status →
// degraded with the wedge reason in last_sample_error.
func TestHeartbeatHandler_RuntimeWedged_FlipsOnlineToDegraded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	wedgeMsg := "claude_agent_sdk wedge: Control request timeout: initialize — restart workspace to recover"

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// Heartbeat UPDATE — sample_error carries the wedge reason from the
	// workspace's _runtime_state_payload() helper.
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-wedged", 0.0, wedgeMsg, 0, 600, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus: currentStatus = online
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	// The wedge-handling branch fires the degraded UPDATE with the
	// `AND status = 'online'` guard (race-safe against concurrent
	// removal). Match the SQL with the guard included.
	mock.ExpectExec("UPDATE workspaces SET status = 'degraded'.*status = 'online'").
		WithArgs("ws-wedged").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// RecordAndBroadcast for WORKSPACE_DEGRADED
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-wedged","error_rate":0.0,"sample_error":"` + wedgeMsg + `","active_tasks":0,"uptime_seconds":600,"runtime_state":"wedged"}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHeartbeatHandler_DegradedRecoversOnlyAfterWedgeClears verifies that
// the degraded → online recovery path requires BOTH error_rate < 0.1
// AND runtime_state cleared. A workspace still reporting wedged stays
// degraded even when error_rate happens to be 0 (no calls have been
// recorded as errors yet — the wedge is captured as a runtime state,
// not an error count).
func TestHeartbeatHandler_DegradedRecoversOnlyAfterWedgeClears(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-still-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-still-wedged", 0.0, "still broken", 0, 800, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// currentStatus = degraded
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-still-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("degraded"))

	// No additional UPDATE expected — the recovery branch's
	// `runtime_state == ""` guard blocks the flip back to online.
	// (sqlmock fails the test if any unmocked Exec runs.)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-still-wedged","error_rate":0.0,"sample_error":"still broken","active_tasks":0,"uptime_seconds":800,"runtime_state":"wedged"}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHeartbeatHandler_DegradedToOnline_AfterWedgeClears verifies the
// happy-path recovery: a workspace previously marked degraded is
// post-restart, error_rate is back to 0, and runtime_state is empty
// (the new process re-imported claude_sdk_executor with the flag
// fresh). Status flips back to online and a WORKSPACE_ONLINE event
// fires.
func TestHeartbeatHandler_DegradedToOnline_AfterWedgeClears(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-recovered").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-recovered", 0.0, "", 0, 30, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-recovered").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("degraded"))

	// Recovery UPDATE fires (degraded → online).
	mock.ExpectExec("UPDATE workspaces SET status = 'online'").
		WithArgs("ws-recovered").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// runtime_state intentionally absent (== ""); error_rate = 0; this
	// is exactly what a freshly-restarted workspace's first heartbeat
	// looks like.
	body := `{"workspace_id":"ws-recovered","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":30}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== UpdateCard ====================

func TestUpdateCard_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect UPDATE query
	mock.ExpectExec("UPDATE workspaces SET agent_card").
		WithArgs("ws-card", `{"name":"Updated Agent","skills":["coding"]}`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect RecordAndBroadcast INSERT for AGENT_CARD_UPDATED
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-card","agent_card":{"name":"Updated Agent","skills":["coding"]}}`
	c.Request = httptest.NewRequest("POST", "/registry/update-card", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateCard(c)

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

func TestUpdateCard_BadJSON(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	c.Request = httptest.NewRequest("POST", "/registry/update-card", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateCard(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateCard_MissingFields(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// Missing agent_card
	body := `{"workspace_id":"ws-card"}`
	c.Request = httptest.NewRequest("POST", "/registry/update-card", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateCard(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateCard_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectExec("UPDATE workspaces SET agent_card").
		WithArgs("ws-card-err", `{"name":"fail"}`).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-card-err","agent_card":{"name":"fail"}}`
	c.Request = httptest.NewRequest("POST", "/registry/update-card", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateCard(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestRegister_GuardAgainstResurrectingRemovedRow verifies the #73 fix:
// the ON CONFLICT UPSERT must carry a `WHERE status IS DISTINCT FROM 'removed'`
// clause so that a late heartbeat from a workspace that was just deleted
// does not resurrect the row to 'online'.
//
// sqlmock matches on a substring of the rendered SQL — we assert the WHERE
// clause is present in the statement issued by Register().
func TestRegister_GuardAgainstResurrectingRemovedRow(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// This regex-ish match requires the guard. If the handler ever drops
	// the clause the test fails because the emitted SQL won't match.
	mock.ExpectExec("ON CONFLICT.*WHERE workspaces.status IS DISTINCT FROM 'removed'").
		WithArgs("ws-resurrect", "ws-resurrect", "http://localhost:8000", `{"name":"x"}`).
		WillReturnResult(sqlmock.NewResult(0, 0)) // 0 rows affected = correctly guarded
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs("ws-resurrect").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://127.0.0.1:54321"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-resurrect","url":"http://localhost:8000","agent_card":{"name":"x"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("#73 guard not present in UPSERT SQL: %v", err)
	}
}

// TestHeartbeat_SkipsRemovedRows verifies #73: heartbeat UPDATE carries
// `AND status != 'removed'` so a late heartbeat from a torn-down container
// doesn't refresh last_heartbeat_at on a tombstoned workspace.
func TestHeartbeat_SkipsRemovedRows(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// prevTask lookup
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-zombie").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// UPDATE must include `AND status != 'removed'`. 0 rows affected is fine —
	// this is the tombstoned case the fix protects against.
	mock.ExpectExec("UPDATE workspaces SET.*WHERE id = .* AND status != 'removed'").
		WithArgs("ws-zombie", 0.0, "", 0, int64(0), "").
		WillReturnResult(sqlmock.NewResult(0, 0))

	// evaluateStatus SELECT
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-zombie").
		WillReturnError(sql.ErrNoRows) // row effectively removed from view

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat",
		bytes.NewBufferString(`{"workspace_id":"ws-zombie","error_rate":0,"sample_error":"","active_tasks":0,"uptime_seconds":0,"current_task":""}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("heartbeat handler must still return 200 even on tombstoned row, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("#73 guard not present in heartbeat UPDATE SQL: %v", err)
	}
}

// ------------------------------------------------------------
// validateAgentURL (C6 SSRF fix)
// ------------------------------------------------------------

func TestValidateAgentURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		// ── Valid URLs (public hostnames / DNS names) ──────────────────────────
		// example.com (RFC-2606) resolves globally; agent.example.com
		// is NXDOMAIN on most resolvers and made this test flake.
		{"valid public https", "https://example.com:443", false},
		{"valid public http", "http://example.com:8000", false},
		// localhost by name is allowed — agents in local-dev use this form.
		{"valid localhost name", "http://localhost:8000", false},

		// ── Must be rejected: bad scheme ─────────────────────────────────────
		{"blocked scheme file", "file:///etc/passwd", true},
		{"blocked scheme ftp", "ftp://internal-server/secrets", true},
		{"blocked malformed url", "://not-a-url", true},
		{"blocked empty url", "", true},

		// ── Must be rejected: 169.254.0.0/16 — link-local / cloud metadata ───
		{"blocked link-local IMDS 169.254.169.254", "http://169.254.169.254/latest/meta-data/", true},
		{"blocked link-local GCP metadata", "http://169.254.169.254/computeMetadata/v1/", true},
		{"blocked link-local 169.254.0.1", "http://169.254.0.1/anything", true},

		// ── Must be rejected: 127.0.0.0/8 — loopback ─────────────────────────
		{"blocked loopback 127.0.0.1", "http://127.0.0.1:8080", true},
		{"blocked loopback 127.0.0.2", "http://127.0.0.2:8080", true},
		{"blocked loopback 127.255.255.255", "http://127.255.255.255:9000", true},

		// ── Must be rejected: 10.0.0.0/8 — RFC-1918 ──────────────────────────
		{"blocked RFC1918 10.0.0.1", "http://10.0.0.1:8080", true},
		{"blocked RFC1918 10.0.0.5", "http://10.0.0.5:8080", true},
		{"blocked RFC1918 10.255.255.254", "http://10.255.255.254:8080", true},

		// ── Must be rejected: 172.16.0.0/12 — RFC-1918 (includes Docker nets) ─
		{"blocked RFC1918 172.16.0.1 (range start)", "http://172.16.0.1:8080", true},
		{"blocked RFC1918 172.18.0.5 (docker bridge)", "http://172.18.0.5:8000", true},
		{"blocked RFC1918 172.31.255.255 (range end)", "http://172.31.255.255:8080", true},

		// ── Must be rejected: 192.168.0.0/16 — RFC-1918 ──────────────────────
		{"blocked RFC1918 192.168.0.1", "http://192.168.0.1:8080", true},
		{"blocked RFC1918 192.168.1.100", "http://192.168.1.100:8080", true},
		{"blocked RFC1918 192.168.255.254", "http://192.168.255.254:8080", true},

		// ── Must be rejected: IPv6 SSRF vectors (C6 gap) ─────────────────────
		// Go's IPv4 CIDRs do not match pure IPv6 addresses via Contains(), so
		// each IPv6 range needs an explicit blocklist entry.
		{"blocked IPv6 loopback [::1]", "http://[::1]:8080", true},
		{"blocked IPv6 link-local [fe80::1]", "http://[fe80::1]:8080", true},
		{"blocked IPv6 ULA [fd00::1]", "http://[fd00::1]:8080", true},

		// ── Must be rejected: RFC 5737 TEST-NET reserved ranges ─────────────
		// These addresses are reserved for documentation and example code.
		// No production agent has a legitimate reason to use them.
		{"blocked TEST-NET-1 192.0.2.x", "http://192.0.2.1:8080", true},
		{"blocked TEST-NET-1 192.0.2.254", "http://192.0.2.254:9000", true},
		{"blocked TEST-NET-2 198.51.100.x", "http://198.51.100.1:8080", true},
		{"blocked TEST-NET-2 198.51.100.99", "http://198.51.100.99:8000", true},
		{"blocked TEST-NET-3 203.0.113.x", "http://203.0.113.1:8080", true},
		{"blocked TEST-NET-3 203.0.113.254", "http://203.0.113.254:9000", true},

		// ── Must be rejected: RFC 3849 IPv6 documentation prefix ────────────
		{"blocked IPv6 documentation 2001:db8::1", "http://[2001:db8::1]:8080", true},
		{"blocked IPv6 documentation 2001:db8::ffff", "http://[2001:db8::ffff]:8000", true},

		// IPv4-mapped IPv6 for a blocked range must also be rejected.
		// Go normalises ::ffff:169.254.x.x to IPv4 via To4(), so the existing
		// 169.254.0.0/16 entry catches it without a dedicated rule.
		{"blocked IPv4-mapped IPv6 link-local", "http://[::ffff:169.254.169.254]:80", true},

		// ── F1083/#1130: DNS names resolved via net.LookupIP ──────────────────
		// localhost is allowed by name (intentional dev-environment special case;
		// the DNS resolution path skips the blocklist to preserve this behaviour).
		{"DNS name: localhost (allowed by name)", "http://localhost:9000", false},
		// github.com resolves to a public IP — must be allowed.
		// Skipped in sandboxed environments where external DNS is unavailable.
		// {"DNS name: github.com (public IP)", "https://github.com/", false},
		// A hostname that fails DNS resolution is blocked — the platform has
		// no use for a workspace it cannot reach; unresolvable hostnames are
		// either misconfigured or intentionally unreachable.
		{"DNS name: nxdomain (must fail)", "https://this-domain-definitely-does-not-exist-12345.invalid/", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("validateAgentURL(%q) = nil, want error", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateAgentURL(%q) = %v, want nil", tc.url, err)
			}
		})
	}
}

// TestValidateAgentURL_SaaSMode_AllowsRFC1918 is the integration-level wrapper test
// for the SaaS-mode SSRF relaxation in validateAgentURL (used at registration).
// It exercises validateAgentURL as called by the Register handler, not just the
// inner blockedRanges slice.  Regression guard for the same class of bug as
// isSafeURL (issue #1785).
func TestValidateAgentURL_SaaSMode_AllowsRFC1918(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	t.Setenv("MOLECULE_ORG_ID", "")
	for _, url := range []string{
		"http://10.1.2.3/agent",
		"http://10.0.0.5:8000/a2a",
		"http://172.16.0.1/agent",
		"http://172.18.0.42:8000/a2a",
		"http://172.31.44.78/agent",
		"http://192.168.1.100/agent",
		"http://192.168.255.254:9000/a2a",
		"http://[fd00::1]/agent",
		"http://[fd12:3456:789a::42]/a2a",
	} {
		if err := validateAgentURL(url); err != nil {
			t.Errorf("validateAgentURL(%q) in saasMode: got %v, want nil", url, err)
		}
	}
}

// TestValidateAgentURL_SaaSMode_StillBlocksMetadataEtAl verifies that even in
// SaaS mode the always-blocked ranges (metadata, loopback, TEST-NET, CGNAT,
// non-fd00 ULA) stay blocked.
func TestValidateAgentURL_SaaSMode_StillBlocksMetadataEtAl(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	t.Setenv("MOLECULE_ORG_ID", "")
	for _, url := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://169.254.0.1/",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
		"http://192.0.2.5/agent",
		"http://198.51.100.5/a2a",
		"http://203.0.113.42/agent",
		"http://100.64.0.1/agent",
		"http://100.127.255.254:8000/a2a",
		"http://[fc00::1]/agent",
		"http://224.0.0.1/",
	} {
		if err := validateAgentURL(url); err == nil {
			t.Errorf("validateAgentURL(%q) in saasMode: got nil, want block", url)
		}
	}
}

// TestValidateAgentURL_StrictMode_BlocksRFC1918 is the strict-mode counterpart
// to TestValidateAgentURL_SaaSMode_AllowsRFC1918.
func TestValidateAgentURL_StrictMode_BlocksRFC1918(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	t.Setenv("MOLECULE_ORG_ID", "")
	for _, url := range []string{
		"http://10.1.2.3/agent",
		"http://172.16.0.1:8000/a2a",
		"http://172.31.44.78/agent",
		"http://192.168.1.100/agent",
		"http://[fd00::1]/agent",
	} {
		if err := validateAgentURL(url); err == nil {
			t.Errorf("validateAgentURL(%q) in strict mode: got nil, want block", url)
		}
	}
}

// TestValidateAgentURL_SaaSMode_LegacyOrgID covers the legacy MOLECULE_ORG_ID
// signal (no MOLECULE_DEPLOY_MODE set) for validateAgentURL.
func TestValidateAgentURL_SaaSMode_LegacyOrgID(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "")
	t.Setenv("MOLECULE_ORG_ID", "7b2179dc-8cc6-4581-a3c6-c8bff4481086")
	for _, url := range []string{
		"http://10.1.2.3/agent",
		"http://172.18.0.42:8000/a2a",
		"http://192.168.1.100/agent",
		"http://[fd00::1]/agent",
	} {
		if err := validateAgentURL(url); err != nil {
			t.Errorf("validateAgentURL(%q) with legacy MOLECULE_ORG_ID: got %v, want nil", url, err)
		}
	}
}

// ==================== C18 — Register ownership ====================

// TestRegister_C18_BootstrapAllowedNoTokens verifies that a workspace with NO
// live tokens (i.e. first-ever registration) is allowed through without a bearer
// token. This is the bootstrap path — the token is issued at the end of Register.
func TestRegister_C18_BootstrapAllowedNoTokens(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// requireWorkspaceToken → HasAnyLiveToken → COUNT(*) returns 0 (no tokens).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-new").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// Workspace upsert proceeds normally.
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-new", "ws-new", "http://localhost:9100", `{"name":"new-agent"}`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs("ws-new").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:9100"))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// HasAnyLiveToken check for token issuance at end of Register.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-new").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// IssueToken INSERT.
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WithArgs("ws-new", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-new","url":"http://localhost:9100","agent_card":{"name":"new-agent"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Errorf("C18 bootstrap: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Token should be present in response (first registration).
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["auth_token"] == nil {
		t.Errorf("C18 bootstrap: expected auth_token in first-registration response, got %v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("C18 bootstrap: unmet expectations: %v", err)
	}
}

// TestRegister_ReturnsPlatformInboundSecret_RFC2312_PRF verifies that
// /registry/register includes the workspace's platform_inbound_secret
// in the response body when one is on file. This is the SaaS delivery
// path: SaaS workspaces have no persistent /configs volume, so they
// re-fetch the secret on every register call (idempotent in Docker mode
// where the provisioner already wrote the same value to the volume at
// workspace creation).
func TestRegister_ReturnsPlatformInboundSecret_RFC2312_PRF(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "00000000-0000-0000-0000-000000002312"
	const inboundSecret = "the-platform-inbound-secret-value"

	// requireWorkspaceToken — bootstrap allowed (no live tokens).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// Workspace upsert.
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:9100"))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Phase 30.1 token issuance — first-register path.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(1, 1))

	// RFC #2312 PR-F: ReadPlatformInboundSecret query — returns the value
	// the provisioner stored at workspace creation. The handler MUST
	// include this in the response body so the workspace can persist it
	// to /configs/.platform_inbound_secret.
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(inboundSecret))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","url":"http://localhost:9100","agent_card":{"name":"x"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	got, ok := resp["platform_inbound_secret"].(string)
	if !ok {
		t.Fatalf("expected platform_inbound_secret in response, got: %v", resp)
	}
	if got != inboundSecret {
		t.Errorf("secret mismatch: got %q, want %q", got, inboundSecret)
	}
	// auth_token should also be present (first-register path).
	if resp["auth_token"] == nil {
		t.Error("expected auth_token in response (first-register path)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_NoInboundSecret_OmitsField verifies that legacy workspaces
// that predate migration 044 (NULL platform_inbound_secret column) still
// get a successful registration — the field is just omitted from the
// response. The Register handler logs the absence quietly.
func TestRegister_NoInboundSecret_OmitsField(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "00000000-0000-0000-0000-000000002312"

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspaces").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:9100"))
	mock.ExpectExec("INSERT INTO structure_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").WillReturnResult(sqlmock.NewResult(1, 1))
	// NULL secret — legacy workspace.
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","url":"http://localhost:9100","agent_card":{"name":"x"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 even without inbound secret, got %d", w.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if _, present := resp["platform_inbound_secret"]; present {
		t.Errorf("expected platform_inbound_secret to be ABSENT for legacy workspace, got: %v", resp["platform_inbound_secret"])
	}
}

// TestRegister_C18_HijackBlockedNoBearer verifies the C18 attack is blocked:
// when a workspace already has a live token, /register without a bearer → 401.
func TestRegister_C18_HijackBlockedNoBearer(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// HasAnyLiveToken returns 1 — workspace already has an active token.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-victim").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// No Authorization header — simulates attacker with no credentials.
	// URL uses example.com (resolves globally) so the validateAgentURL
	// pre-check doesn't short-circuit with 400 "invalid request body"
	// before the C18 auth check fires. We're testing that C18 gates
	// produce 401, not that URL validation produces 400.
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-victim","url":"http://example.com:9999/steal","agent_card":{"name":"hijacked"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("C18 hijack: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	// The malicious URL must NOT have been persisted — no INSERT expectation was set.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("C18 hijack: unmet expectations: %v", err)
	}
}

// ==================== Issue #435 — DB error must not leak raw message ====================

// TestRegister_DBErrorResponseIsOpaque verifies that when the DB upsert fails,
// the HTTP response body contains only the generic "registration failed" message
// and never the raw Go/PostgreSQL error string (issue #435).
func TestRegister_DBErrorResponseIsOpaque(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// C18 pre-check — no live tokens (bootstrap path).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-errtest").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// DB upsert fails with a descriptive internal error.
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-errtest", "ws-errtest", "http://localhost:9200", `{"name":"err-agent"}`).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-errtest","url":"http://localhost:9200","agent_card":{"name":"err-agent"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v — body: %s", err, w.Body.String())
	}

	errMsg, ok := resp["error"].(string)
	if !ok {
		t.Fatalf("expected string 'error' field, got %T: %v", resp["error"], resp["error"])
	}
	if errMsg != "registration failed" {
		t.Errorf("expected opaque 'registration failed', got %q (raw error leaked)", errMsg)
	}
	// Confirm the raw driver error string is absent.
	rawBody := w.Body.String()
	if strings.Contains(rawBody, "sql:") || strings.Contains(rawBody, "pq:") || strings.Contains(rawBody, "connection") {
		t.Errorf("raw DB error leaked into response body: %s", rawBody)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== #615 — monthly_spend clamping ====================

// TestHeartbeat_MonthlySpend_WithinBounds verifies that a valid positive
// monthly_spend is written to the DB unchanged (no clamping needed).
func TestHeartbeat_MonthlySpend_WithinBounds(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-spend-ok").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// Expect the 7-argument UPDATE (with monthly_spend = $7).
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-spend-ok", 0.0, "", 0, 0, "", int64(15000)). // $150.00
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-spend-ok").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-spend-ok","monthly_spend":15000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHeartbeat_MonthlySpend_NegativeClamped verifies that a negative
// monthly_spend value (invalid) is clamped to 0 before the DB write,
// which means the no-spend UPDATE path is taken (zero is "no update"). (#615)
func TestHeartbeat_MonthlySpend_NegativeClamped(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-spend-neg").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// Clamped to 0 → no monthly_spend field → 6-argument UPDATE.
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-spend-neg", 0.0, "", 0, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-spend-neg").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-spend-neg","monthly_spend":-9999}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("negative monthly_spend must be clamped to 0 (no-spend UPDATE path): %v", err)
	}
}

// TestHeartbeat_MonthlySpend_OverflowClamped verifies that an astronomically
// large monthly_spend is clamped to maxMonthlySpend ($10B in cents) rather
// than written raw to the DB, preventing NUMERIC overflow. (#615)
func TestHeartbeat_MonthlySpend_OverflowClamped(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-spend-overflow").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// Expect the 7-argument UPDATE with monthly_spend clamped to 1_000_000_000_000.
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-spend-overflow", 0.0, "", 0, 0, "", int64(1_000_000_000_000)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-spend-overflow").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// Simulate a misbehaving agent reporting math.MaxInt64.
	body := `{"workspace_id":"ws-spend-overflow","monthly_spend":9223372036854775807}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("math.MaxInt64 monthly_spend must be clamped to maxMonthlySpend: %v", err)
	}
}

// TestHeartbeat_MonthlySpend_ExactCap verifies the boundary: a value exactly
// equal to maxMonthlySpend ($10B) passes through without modification.
func TestHeartbeat_MonthlySpend_ExactCap(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-spend-cap").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-spend-cap", 0.0, "", 0, 0, "", int64(1_000_000_000_000)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-spend-cap").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-spend-cap","monthly_spend":1000000000000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("exact-cap monthly_spend should pass through unmodified: %v", err)
	}
}

// TestHeartbeat_MonthlySpend_Zero_NoUpdate verifies that monthly_spend=0 (or
// omitted) does NOT write monthly_spend to the DB — zero means "no update",
// never write zero to avoid clearing a previously-reported spend value.
func TestHeartbeat_MonthlySpend_Zero_NoUpdate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-spend-zero").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// 6-argument UPDATE — monthly_spend NOT included.
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-spend-zero", 0.0, "", 0, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-spend-zero").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// Explicitly set monthly_spend = 0.
	body := `{"workspace_id":"ws-spend-zero","monthly_spend":0}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("monthly_spend=0 must not trigger a DB write for spend: %v", err)
	}
}
