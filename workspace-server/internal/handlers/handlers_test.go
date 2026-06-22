package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/ws"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// liveTestHandlers tracks every WorkspaceHandler built during the test
// binary's lifetime so setupTestDB can drain their in-flight goAsync
// goroutines (notably the detached RestartByID restart cycle, which
// reads the global db.DB) BEFORE restoring db.DB. Without this drain a
// fire-and-forget restart goroutine spawned by one test outlives that
// test and races the db.DB swap in a later test's t.Cleanup — the
// 0x...d548 data race on platform/internal/db.DB.
var (
	liveTestHandlersMu sync.Mutex
	liveTestHandlers   []*WorkspaceHandler
)

func init() {
	gin.SetMode(gin.TestMode)
	newHandlerHook = func(h *WorkspaceHandler) {
		liveTestHandlersMu.Lock()
		liveTestHandlers = append(liveTestHandlers, h)
		liveTestHandlersMu.Unlock()
	}
}

// drainTestAsync waits for every tracked handler's goAsync goroutines to
// finish. Called from setupTestDB's cleanup before db.DB is restored so
// no detached restart/provision goroutine is mid-read of db.DB when the
// pointer is swapped.
//
// Also drains the package-level globalAsync WaitGroup (RFC internal#524
// Layer 1 deliverable 2) so sibling handlers (SecretsHandler /
// PluginsHandler / etc.) that route through globalGoAsync rather than
// h.goAsync are likewise drained before db.DB is swapped. Without this
// drain a SecretsHandler.Set's restartFunc-via-globalGoAsync could race
// the db.DB restore exactly the same way maybeMarkContainerDead did
// before commit 69d9b4e3.
func drainTestAsync() {
	liveTestHandlersMu.Lock()
	handlers := make([]*WorkspaceHandler, len(liveTestHandlers))
	copy(handlers, liveTestHandlers)
	liveTestHandlersMu.Unlock()
	for _, h := range handlers {
		h.waitAsyncForTest()
	}
	waitGlobalAsyncForTest()
}

// setupTestDB creates a sqlmock DB and assigns it to the global db.DB.
// It also disables the SSRF URL check so that httptest.NewServer loopback
// URLs and fake hostnames (*.example) used in tests don't trigger rejections.
//
// IMPORTANT: db.DB is saved before assignment and restored via t.Cleanup so
// that tests running after this one are not polluted by a closed mock.
// This is the single root cause of the systemic CI/Platform (Go) failures on
// main HEAD 8026f020 (mc#975).
func setupTestDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB

	// Disable SSRF checks for the duration of this test only. Restore
	// the previous state via t.Cleanup so that TestIsSafeURL_* tests
	// (which run with SSRF enabled) are not affected by state leak.
	//
	// REGISTRATION ORDER MATTERS (#2132 D step, Researcher RC 103771):
	// t.Cleanup runs hooks in LIFO order. We register the SSRF-state
	// restore FIRST so it runs LAST in cleanup, AFTER the db.DB
	// restore. The natural order — db.DB swap-back, THEN
	// ssrfCheckEnabled restore — matches the natural ordering of
	// the state mutations in setupTestDB itself (db.DB first,
	// ssrfCheckEnabled second). Reversing the order caused
	// Platform(Go) failures in the prior shape when a test enabled
	// SSRF via setSSRFCheckForTest(true) — the LIFO swap put the
	// SSRF restore BEFORE the db.DB swap, leaving a window where
	// db.DB was the previous (real) DB while ssrfCheckEnabled was
	// the test's true (mid-test). Reordering the registrations
	// closes the window.
	restore := setSSRFCheckForTest(false)
	t.Cleanup(restore)

	t.Cleanup(func() {
		// Drain detached async goroutines (e.g. goAsync(RestartByID),
		// which reads db.DB in runRestartCycle before its provisioner
		// gate) BEFORE swapping db.DB back. Doing the restore first
		// would let an in-flight restart goroutine read db.DB while
		// this line writes it — the data race this guards against.
		drainTestAsync()
		db.DB = prevDB
		mockDB.Close()
	})

	// The wsauth.platform_inbound_secret cache (#189) is package-level
	// state in another package — without a reset between tests, a
	// write-through Issue from one test (or even a prior Read populating
	// the cache) shadows the SELECT expectation in the next test that
	// uses the same workspace ID. Reset before each test that builds a
	// fresh sqlmock; the no-op cost is one Range over an empty sync.Map.
	wsauth.ResetInboundSecretCacheForTesting()
	t.Cleanup(wsauth.ResetInboundSecretCacheForTesting)

	return mock
}

func waitForHandlerAsyncBeforeDBCleanup(t *testing.T, h *WorkspaceHandler) {
	t.Helper()
	t.Cleanup(h.waitAsyncForTest)
}

// setupTestRedis creates a miniredis instance and assigns it to the global db.RDB.
func setupTestRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	db.RDB = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { mr.Close() })
	return mr
}

// newTestBroadcaster creates a Broadcaster backed by a no-op WebSocket hub.
func newTestBroadcaster() *events.Broadcaster {
	hub := ws.NewHub(func(callerID, targetID string) bool { return true })
	return events.NewBroadcaster(hub)
}

// allowLoopbackForTest flips the ssrf.go testAllowLoopback escape hatch
// for the duration of the test, so httptest.NewServer's loopback URLs
// don't trip the SSRF guard. The 169.254 metadata, RFC-1918, TEST-NET,
// CGNAT, and link-local guards stay active — only 127.0.0.0/8 and ::1
// are relaxed. Always paired with t.Cleanup to restore; multiple
// parallel tests won't race because Go test flips it sequentially per
// test unless t.Parallel() is used, and these tests don't parallelize.
func allowLoopbackForTest(t *testing.T) {
	t.Helper()
	prev := testAllowLoopback
	testAllowLoopback = true
	t.Cleanup(func() { testAllowLoopback = prev })
}

// expectBudgetCheck adds the sqlmock expectation for the budget-check
// query that ProxyA2A runs before forwarding. checkWorkspaceBudget
// fails-open on sql.ErrNoRows, so we return a deliberately-empty
// result — budget_limit NULL + monthly_spend 0 means "no limit".
// All a2a_proxy_test.go tests that run ProxyA2A (not just
// dispatchA2A unit tests) need this expectation; it was added to the
// handler in the 2026-04-18 restructure but the tests never caught up,
// leaving Platform (Go) CI red for weeks.
func expectBudgetCheck(mock sqlmock.Sqlmock, workspaceID string) {
	// Multi-period (#49): checkWorkspaceBudget reads budget_limits jsonb. An
	// empty map → no limits → returns early (no spend query), enforcement skipped.
	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"budget_limits"}).AddRow([]byte("{}")))
}

// ---------- TestRegisterHandler ----------

func TestRegisterHandler(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// resolveDeliveryMode preflight — no row yet, default push (#2339).
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-123").
		WillReturnError(sql.ErrNoRows)

	// Expect the upsert INSERT ... ON CONFLICT
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-123", "ws-123", "http://localhost:8000", `{"name":"test"}`, "push", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect the SELECT url query (for cache URL logic)
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id =").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:8000"))

	// Expect the RecordAndBroadcast INSERT into structure_events
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"id":"ws-123","url":"http://localhost:8000","agent_card":{"name":"test"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "registered" {
		t.Errorf("expected status 'registered', got %v", resp["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestHeartbeatHandler ----------

func TestHeartbeatHandler_Normal(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT (before UPDATE)
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend"}).AddRow("", 0))

	// Expect heartbeat UPDATE
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-123", 0.1, "", 2, 3600, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect evaluateStatus SELECT
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-123","error_rate":0.1,"sample_error":"","active_tasks":2,"uptime_seconds":3600}`
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

func TestHeartbeatHandler_Degraded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT (before UPDATE)
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend"}).AddRow("", 0))

	// Expect heartbeat UPDATE
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-123", 0.8, "connection timeout", 0, 7200, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect evaluateStatus SELECT — currently online
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	// Expect status transition to degraded
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusDegraded, "ws-123").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect RecordAndBroadcast INSERT for WORKSPACE_DEGRADED
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-123","error_rate":0.8,"sample_error":"connection timeout","active_tasks":0,"uptime_seconds":7200}`
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

func TestHeartbeatHandler_Recovery(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT (before UPDATE)
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend"}).AddRow("", 0))

	// Expect heartbeat UPDATE
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-123", 0.05, "", 1, 9000, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect evaluateStatus SELECT — currently degraded
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("degraded", "", nil, nil))

	// Expect status transition back to online
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusOnline, "ws-123").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect RecordAndBroadcast INSERT for WORKSPACE_ONLINE
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-123","error_rate":0.05,"sample_error":"","active_tasks":1,"uptime_seconds":9000}`
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

// ---------- TestWorkspaceCreate ----------

func TestWorkspaceCreate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	// Expect transaction begin for atomic workspace+secrets creation
	mock.ExpectBegin()

	// Expect workspace INSERT (uuid is dynamic, use AnyArg for id, runtime).
	// Default tier is 3 (Privileged) — see workspace.go create-handler comment.
	// delivery_mode defaults to "push" when payload omits it (#2339).
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(sqlmock.AnyArg(), "Test Agent", nil, 3, "claude-code", "", (*string)(nil), nil, "none", (*int64)(nil), models.DefaultMaxConcurrentTasks, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect transaction commit (no secrets in this payload)
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO workspace_secrets").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect canvas_layouts INSERT
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WithArgs(sqlmock.AnyArg(), float64(100), float64(200)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect RecordAndBroadcast INSERT for WORKSPACE_PROVISIONING
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// Note: model is now required at the Create boundary (CTO 2026-05-22
	// SSOT directive — see feedback_workspace_model_required_no_platform_default_dynamic_credential_intake
	// and TestCreate_ModelRequired_Returns422). This test happens to take
	// the bare-defaults path (no template, no runtime → claude-code), so
	// the body must declare an explicit model. Using a claude-code-compatible
	// id; the test doesn't exercise model semantics beyond presence.
	body := `{"name":"Test Agent","model":"anthropic:claude-opus-4-7","canvas":{"x":100,"y":200}}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "provisioning" {
		t.Errorf("expected status 'provisioning', got %v", resp["status"])
	}
	if resp["id"] == nil || resp["id"] == "" {
		t.Error("expected non-empty id in response")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceCreate_ReturnsAuthToken_201 pins the inline-auth_token
// behaviour added for #1644. Pre-fix, the 201 response was
// {id, status, awareness_namespace, workspace_access} — callers had to
// make a separate POST to /admin/workspaces/:id/tokens (AdminAuth-gated,
// path-prefix differs in CP-admin deploys) OR fall back to the dev-only
// GET /admin/workspaces/:id/test-token (deliberately 404s on
// MOLECULE_ENV=production per feedback_no_dev_only_routes_in_e2e).
//
// Post-fix: every Create response includes an `auth_token` field with
// the freshly-minted plaintext bearer (returned once, never recoverable).
// This is the SSOT path — production E2E + canvas + org_import all
// get the bearer they need in the same round trip.
//
// Failure path is non-fatal: if the IssueToken DB call fails, the 201
// still goes out without auth_token + a fallback log line. That branch
// is exercised by sqlmock returning a non-INSERT-INTO-workspace_auth_tokens
// path here — the test asserts presence on the happy path.
func TestWorkspaceCreate_ReturnsAuthToken_201(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(sqlmock.AnyArg(), "Token Holder", nil, 3, "claude-code", "", (*string)(nil), nil, "none", (*int64)(nil), models.DefaultMaxConcurrentTasks, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The inline mint added in #1644 Part B — wsauth.IssueToken issues
	// a new bearer via INSERT INTO workspace_auth_tokens (workspace_id,
	// token_hash, prefix). This is the assertion that the new code path
	// reaches the DB.
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Token Holder","model":"anthropic:claude-opus-4-7"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	tok, ok := resp["auth_token"].(string)
	if !ok || tok == "" {
		t.Fatalf("expected non-empty auth_token in 201 response (the #1644 SSOT inline mint), got: %s", w.Body.String())
	}
	// Sanity: tokens are base64-RawURL encoded 32-byte payloads (per
	// wsauth/tokens.go::tokenPayloadBytes), so a meaningful lower bound
	// is ~40 chars. If this fails, IssueToken's contract drifted.
	if len(tok) < 40 {
		t.Errorf("auth_token suspiciously short (%d chars) — wsauth.IssueToken contract drift?", len(tok))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations — inline mint path may have skipped IssueToken: %v", err)
	}
}

func TestBuildProvisionerConfig_WorkspacePathFromPayload(t *testing.T) {
	setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	t.Setenv("WORKSPACE_DIR", "/tmp/workspace")

	cfg := handler.buildProvisionerConfig(
		context.Background(),
		"ws-123",
		"/tmp/configs/template",
		map[string][]byte{"config.yaml": []byte("name: test")},
		models.CreateWorkspacePayload{Tier: 2, Runtime: "claude-code", WorkspaceDir: "/tmp/workspace", WorkspaceAccess: "read_write"},
		map[string]string{"OPENAI_API_KEY": "sk-test"},
		nil,
		"/tmp/plugins",
	)

	if cfg.WorkspacePath != "/tmp/workspace" {
		t.Fatalf("expected workspace path from payload, got %q", cfg.WorkspacePath)
	}
}

// ---------- TestWorkspaceList ----------

func TestWorkspaceList(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	// 24 cols: compute added after talk_to_user_enabled.
	// (migration 20260514). Column order must match scanWorkspaceRow exactly.
	columns := []string{
		"id", "name", "role", "tier", "status", "agent_card", "url",
		"parent_id", "active_tasks", "max_concurrent_tasks",
		"last_error_rate", "last_sample_error",
		"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
		"budget_limit", "monthly_spend",
		"broadcast_enabled", "talk_to_user_enabled", "compute", "kind",
	}
	rows := sqlmock.NewRows(columns).
		AddRow("ws-1", "Agent One", "worker", 1, "online", []byte("null"), "http://localhost:8001",
			nil, 0, 1, 0.0, "", 100, "", "claude-code", "", 10.0, 20.0, false, nil, int64(0), false, true, []byte(`{}`), "workspace").
		AddRow("ws-2", "Agent Two", "manager", 2, "provisioning", []byte("null"), "",
			nil, 0, 1, 0.0, "", 0, "", "claude-code", "", 50.0, 60.0, false, nil, int64(0), false, true, []byte(`{}`), "workspace")

	mock.ExpectQuery("SELECT w.id, w.name").
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/workspaces", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 2 {
		t.Errorf("expected 2 workspaces, got %d", len(resp))
	}
	if resp[0]["name"] != "Agent One" {
		t.Errorf("expected first workspace name 'Agent One', got %v", resp[0]["name"])
	}
	if resp[1]["status"] != "provisioning" {
		t.Errorf("expected second workspace status 'provisioning', got %v", resp[1]["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestProxyA2A ----------

func TestProxyA2A_JSONRPCWrapping(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	// Create a mock agent endpoint that captures the request
	var receivedBody map[string]interface{}
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{"status":"ok"}}`)
	}))
	defer agentServer.Close()

	// Cache the agent URL in Redis so the handler finds it
	mr.Set(fmt.Sprintf("ws:%s:url", "ws-proxy"), agentServer.URL)
	expectBudgetCheck(mock, "ws-proxy")

	// Expect async activity log INSERT from the LogActivity goroutine
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-proxy"}}

	// Send a bare payload (no jsonrpc envelope)
	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hello"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-proxy/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	// Give the async LogActivity goroutine a moment to complete
	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the proxy wrapped the payload in a JSON-RPC envelope
	if receivedBody["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc '2.0', got %v", receivedBody["jsonrpc"])
	}
	if receivedBody["id"] == nil || receivedBody["id"] == "" {
		t.Error("expected non-empty id in JSON-RPC envelope")
	}
	if receivedBody["method"] != "message/send" {
		t.Errorf("expected method 'message/send', got %v", receivedBody["method"])
	}

	// Verify messageId was injected
	params, _ := receivedBody["params"].(map[string]interface{})
	msg, _ := params["message"].(map[string]interface{})
	if msg["messageId"] == nil || msg["messageId"] == "" {
		t.Error("expected messageId to be injected into params.message")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestProxyA2A_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t) // empty Redis — no cached URL
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	// Redis miss → DB lookup → no rows
	mock.ExpectQuery("SELECT url, status FROM workspaces WHERE id =").
		WithArgs("ws-missing").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-missing"}}

	body := `{"method":"message/send","params":{}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-missing/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestProxyA2A_WorkspaceOffline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t) // empty Redis — no cached URL
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	// Redis miss → DB lookup → workspace exists but URL is empty
	mock.ExpectQuery("SELECT url, status FROM workspaces WHERE id =").
		WithArgs("ws-offline").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}).AddRow(nil, "offline"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-offline"}}

	body := `{"method":"message/send","params":{}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-offline/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestHeartbeatHandler_TaskChanged ----------

func TestHeartbeatHandler_TaskChanged(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT — currently "old task"
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend"}).AddRow("old task", 0))

	// Expect heartbeat UPDATE with new task
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-123", 0.0, "", 1, 1000, "new task").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect evaluateStatus SELECT
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-123","error_rate":0.0,"sample_error":"","active_tasks":1,"uptime_seconds":1000,"current_task":"new task"}`
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

// ---------- TestActivityHandler ----------

func TestActivityHandler_List(t *testing.T) {
	mock := setupTestDB(t)

	columns := []string{
		"id", "workspace_id", "activity_type", "source_id", "target_id", "method",
		"summary", "request_body", "response_body", "tool_trace", "duration_ms", "status", "error_detail", "created_at",
	}
	rows := sqlmock.NewRows(columns).
		AddRow("act-1", "ws-1", "a2a_receive", nil, "ws-1", "message/send",
			"message/send → ws-1", []byte(`{"method":"message/send"}`), []byte(`{"result":"ok"}`),
			nil, 150, "ok", nil, time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)).
		AddRow("act-2", "ws-1", "error", nil, nil, nil,
			"connection failed", nil, nil,
			nil, nil, "error", "timeout after 120s", time.Date(2026, 4, 5, 9, 0, 0, 0, time.UTC))

	mock.ExpectQuery("SELECT id, workspace_id, activity_type").
		WithArgs("ws-1", 100).
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("expected 2 activities, got %d", len(resp))
	}
	if resp[0]["activity_type"] != "a2a_receive" {
		t.Errorf("expected first activity type 'a2a_receive', got %v", resp[0]["activity_type"])
	}
	if resp[1]["status"] != "error" {
		t.Errorf("expected second activity status 'error', got %v", resp[1]["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestActivityHandler_ListByType(t *testing.T) {
	mock := setupTestDB(t)

	columns := []string{
		"id", "workspace_id", "activity_type", "source_id", "target_id", "method",
		"summary", "request_body", "response_body", "tool_trace", "duration_ms", "status", "error_detail", "created_at",
	}
	rows := sqlmock.NewRows(columns).
		AddRow("act-1", "ws-1", "error", nil, nil, nil,
			"connection failed", nil, nil,
			nil, nil, "error", "timeout", time.Date(2026, 4, 5, 9, 0, 0, 0, time.UTC))

	mock.ExpectQuery("SELECT id, workspace_id, activity_type").
		WithArgs("ws-1", "error", 100).
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?type=error", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Fatalf("expected 1 activity, got %d", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestActivityHandler_Report(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	// Expect the INSERT into activity_logs
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}

	body := `{"activity_type":"agent_log","summary":"Processing user request","method":"inference"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/activity", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Report(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestActivityHandler_Report_InvalidType(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}

	body := `{"activity_type":"invalid_type","summary":"test"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/activity", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Report(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- TestHeartbeatHandler_TaskUnchanged ----------

func TestHeartbeatHandler_TaskUnchanged(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT — task is already "doing work"
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend"}).AddRow("doing work", 0))

	// Expect heartbeat UPDATE with same task
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-123", 0.0, "", 1, 500, "doing work").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect evaluateStatus SELECT
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	// NO TASK_UPDATED broadcast expected — task didn't change

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-123","error_rate":0.0,"sample_error":"","active_tasks":1,"uptime_seconds":500,"current_task":"doing work"}`
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

// ---------- TestHeartbeatHandler_TaskCleared ----------

func TestHeartbeatHandler_TaskCleared(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT — was doing something
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend"}).AddRow("old task", 0))

	// Expect heartbeat UPDATE with empty task
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-123", 0.0, "", 0, 600, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect evaluateStatus SELECT
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	// TASK_UPDATED broadcast expected — changed from "old task" to ""
	// (BroadcastOnly doesn't hit sqlmock, so no expectation needed)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-123","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":600}`
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

// ---------- TestHeartbeatHandler_AlwaysBroadcastsHeartbeat ----------
//
// Regression for the "context canceled" wave on 2026-04-26 (15+ failures
// in 1hr across 6 workspaces). The a2a-proxy idle timer subscribes to
// the broadcaster's SSE channel for the workspace and resets on every
// event. Pre-fix the only broadcast paths from heartbeat were
// TASK_UPDATED (only on current_task change) and the
// WORKSPACE_ONLINE/DEGRADED transitions inside evaluateStatus (only on
// status change). A long-running agent on the same task with stable
// status fired NO broadcasts → idle timer fired → user message
// got cancelled mid-flight.
//
// The fix emits an unconditional WORKSPACE_HEARTBEAT on every successful
// heartbeat. This test pins the property: regardless of whether
// current_task changed, the SSE subscriber observes a broadcast.

func TestHeartbeatHandler_AlwaysBroadcastsHeartbeat(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Subscribe BEFORE the heartbeat so we don't miss the broadcast.
	sub, unsub := broadcaster.SubscribeSSE("ws-123")
	defer unsub()

	// Same-task scenario: task value unchanged across the heartbeat.
	// Pre-fix this path emitted ZERO broadcasts.
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend"}).AddRow("doing work", 0))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-123", 0.0, "", 1, 500, "doing work").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-123").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-123","error_rate":0.0,"sample_error":"","active_tasks":1,"uptime_seconds":500,"current_task":"doing work"}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Drain whatever the handler broadcast (with a tight timeout — the
	// channel is in-process so the event should already be queued by
	// the time Heartbeat returns).
	gotHeartbeat := false
	for i := 0; i < 5; i++ {
		select {
		case msg, ok := <-sub:
			if !ok {
				t.Fatal("broadcaster channel closed unexpectedly")
			}
			if msg.Event == "WORKSPACE_HEARTBEAT" {
				gotHeartbeat = true
				goto done
			}
		case <-time.After(200 * time.Millisecond):
			goto done
		}
	}
done:
	if !gotHeartbeat {
		t.Error("expected WORKSPACE_HEARTBEAT broadcast on every heartbeat (regression: pre-fix, same-task heartbeats fired no broadcast and the a2a-proxy idle timer trip-cancelled in-flight requests)")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestParseIdleTimeoutEnv ----------
//
// Pins the env-override path including the bad-input fallback paths
// that the package-init `var idleTimeoutDuration = parseIdleTimeoutEnv(...)`
// relies on. Without this test, an operator who sets
// A2A_IDLE_TIMEOUT_SECONDS=foo would get the default with no log signal
// (pre-fix behaviour) and the regression would slip in unnoticed.

func TestParseIdleTimeoutEnv(t *testing.T) {
	// core#2723: the default is the deployable safety margin for long
	// blocking tool calls that stall the runtime heartbeat (raised 5m→30m).
	if defaultIdleTimeoutDuration != 30*time.Minute {
		t.Errorf("default idle timeout = %v, want 30m (core#2723)", defaultIdleTimeoutDuration)
	}
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty falls back to default", "", defaultIdleTimeoutDuration},
		{"valid positive integer parses to seconds", "120", 120 * time.Second},
		{"longer override honored (30m)", "1800", 1800 * time.Second},
		{"valid integer at minimum (1) is accepted", "1", 1 * time.Second},
		{"non-numeric falls back to default", "foo", defaultIdleTimeoutDuration},
		{"negative falls back to default", "-30", defaultIdleTimeoutDuration},
		{"zero falls back to default", "0", defaultIdleTimeoutDuration},
		{"float falls back to default (Atoi rejects)", "1.5", defaultIdleTimeoutDuration},
		{"trailing units rejected (we accept seconds only)", "60s", defaultIdleTimeoutDuration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseIdleTimeoutEnv(tc.in)
			if got != tc.want {
				t.Errorf("parseIdleTimeoutEnv(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ---------- TestActivityHandler_ListEmpty ----------

func TestActivityHandler_ListEmpty(t *testing.T) {
	mock := setupTestDB(t)

	columns := []string{
		"id", "workspace_id", "activity_type", "source_id", "target_id", "method",
		"summary", "request_body", "response_body", "tool_trace", "duration_ms", "status", "error_detail", "created_at",
	}
	mock.ExpectQuery("SELECT id, workspace_id, activity_type").
		WithArgs("ws-empty", 100).
		WillReturnRows(sqlmock.NewRows(columns))

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-empty"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-empty/activity", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty array, got %d items", len(resp))
	}
}

// ---------- TestActivityHandler_ListCustomLimit ----------

func TestActivityHandler_ListCustomLimit(t *testing.T) {
	mock := setupTestDB(t)

	columns := []string{
		"id", "workspace_id", "activity_type", "source_id", "target_id", "method",
		"summary", "request_body", "response_body", "tool_trace", "duration_ms", "status", "error_detail", "created_at",
	}
	mock.ExpectQuery("SELECT id, workspace_id, activity_type").
		WithArgs("ws-1", 10).
		WillReturnRows(sqlmock.NewRows(columns))

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?limit=10", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestActivityHandler_ListMaxLimit ----------

func TestActivityHandler_ListMaxLimit(t *testing.T) {
	mock := setupTestDB(t)

	columns := []string{
		"id", "workspace_id", "activity_type", "source_id", "target_id", "method",
		"summary", "request_body", "response_body", "tool_trace", "duration_ms", "status", "error_detail", "created_at",
	}
	// Even though client requests 9999, server caps at 500
	mock.ExpectQuery("SELECT id, workspace_id, activity_type").
		WithArgs("ws-1", 500).
		WillReturnRows(sqlmock.NewRows(columns))

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?limit=9999", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- TestActivityHandler_ReportAllValidTypes ----------

func TestActivityHandler_ReportAllValidTypes(t *testing.T) {
	validTypes := []string{"a2a_send", "a2a_receive", "task_update", "agent_log", "skill_promotion", "error"}

	for _, actType := range validTypes {
		t.Run(actType, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			broadcaster := newTestBroadcaster()
			handler := NewActivityHandler(broadcaster)

			mock.ExpectExec("INSERT INTO activity_logs").
				WillReturnResult(sqlmock.NewResult(0, 1))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "ws-1"}}

			body := fmt.Sprintf(`{"activity_type":"%s","summary":"test %s"}`, actType, actType)
			c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/activity", bytes.NewBufferString(body))
			c.Request.Header.Set("Content-Type", "application/json")

			handler.Report(c)

			if w.Code != http.StatusOK {
				t.Errorf("expected 200 for type %s, got %d: %s", actType, w.Code, w.Body.String())
			}

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations for type %s: %v", actType, err)
			}
		})
	}
}

// ---------- TestActivityHandler_ReportMissingBody ----------

func TestActivityHandler_ReportMissingBody(t *testing.T) {
	setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}

	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/activity", bytes.NewBufferString("{}"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Report(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing activity_type, got %d", w.Code)
	}
}

// ---------- TestWorkspaceGet_CurrentTask ----------

func TestWorkspaceGet_CurrentTask(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp/configs")

	columns := []string{
		"id", "name", "role", "tier", "status", "agent_card", "url",
		"parent_id", "active_tasks", "max_concurrent_tasks", "last_error_rate", "last_sample_error",
		"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
		"budget_limit", "monthly_spend",
		"broadcast_enabled", "talk_to_user_enabled", "compute", "kind",
	}
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("dddddddd-0004-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows(columns).AddRow(
			"dddddddd-0004-0000-0000-000000000000", "Task Worker", "worker", 1, "online", []byte("null"), "http://localhost:9000",
			nil, 2, 1, 0.0, "", 300, "Analyzing document", "claude-code", "", 10.0, 20.0, false,
			nil, int64(0), false, true, []byte(`{}`), "workspace",
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "dddddddd-0004-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-task", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	// current_task stripped from public GET response (#955)
	if _, exists := resp["current_task"]; exists {
		t.Errorf("current_task should be stripped from public GET response")
	}
	if resp["active_tasks"] != float64(2) {
		t.Errorf("expected active_tasks 2, got %v", resp["active_tasks"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestActivityHandler_Report_SourceIDSpoofRejected verifies the #209 spoof
// guard: a workspace authenticated for :id cannot inject activity rows with
// source_id pointing at a different workspace. Bearer-auth middleware would
// already cover the obvious case; this is the belt-and-suspenders body check.
func TestActivityHandler_Report_SourceIDSpoofRejected(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-alice"}}
	// alice's workspace authenticated — but body claims source_id=ws-bob.
	body := `{"activity_type":"agent_log","summary":"fake log","source_id":"ws-bob"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-alice/activity", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Report(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("spoof: got %d, want 403 (%s)", w.Code, w.Body.String())
	}
}

// TestActivityHandler_Report_MatchingSourceIDAccepted — the non-spoof path:
// body.source_id explicitly matches workspaceID, still accepted.
func TestActivityHandler_Report_MatchingSourceIDAccepted(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-alice"}}
	body := `{"activity_type":"agent_log","summary":"self log","source_id":"ws-alice"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-alice/activity", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Report(c)

	if w.Code != http.StatusOK {
		t.Errorf("matching source_id: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// TestActivityHandler_Report_SourceIDLogInjection — #234 regression guard.
// The security log line must emit the attacker-supplied source_id through
// %q so control characters (\n, \r, \t) are escaped instead of splitting
// the log stream into fake entries. Harder to assert directly without a
// log capture, so we just exercise the code path with a payload containing
// newlines and confirm the handler still returns 403 cleanly (no panic,
// no accidental success).
func TestActivityHandler_Report_SourceIDLogInjection(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-alice"}}
	// JSON body with explicit \n escapes — json.Unmarshal decodes these
	// into literal newline bytes before reaching the log call.
	body := `{"activity_type":"agent_log","summary":"x","source_id":"ws-evil\ntimestamp=FORGED level=INFO msg=fake"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-alice/activity",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Report(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("spoof with newline in source_id: got %d, want 403 (%s)",
			w.Code, w.Body.String())
	}
}
