package handlers

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/gin-gonic/gin"
)

func TestSessionSearchReturnsActivityAndMemory(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	rows := sqlmock.NewRows([]string{
		"kind", "id", "workspace_id", "label", "content", "method", "status", "request_body", "response_body", "created_at",
	}).
		AddRow("activity", "act-1", "ws-123", "task_update", "Working on docs", "POST", "ok", `{"task":"docs"}`, `{"ok":true}`, time.Now()).
		AddRow("activity", "act-2", "ws-123", "skill_promotion", "Promoted repeatable workflow", "memory/skill-promotion", "ok", `{"promote_to_skill":true}`, `{"id":"mem-2"}`, time.Now()).
		AddRow("memory", "mem-1", "ws-123", "TEAM", "remember the docs path", "", "", nil, nil, time.Now())

	mock.ExpectQuery("WITH session_items AS").
		WithArgs("ws-123", "%docs%", 50).
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-123/session-search?q=docs", bytes.NewBufferString(""))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "ws-123"}}

	handler.SessionSearch(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 3 {
		t.Fatalf("expected 3 results, got %d", len(resp))
	}
	if resp[0]["kind"] != "activity" || resp[1]["kind"] != "activity" || resp[2]["kind"] != "memory" {
		t.Fatalf("unexpected result kinds: %#v", resp)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// ---------- Activity List source filter ----------

func TestActivityList_SourceCanvas(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	// Expect query with "source_id IS NULL"
	mock.ExpectQuery(`SELECT .+ FROM activity_logs WHERE workspace_id = .+ AND source_id IS NULL`).
		WithArgs("ws-1", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"duration_ms", "status", "error_detail", "created_at",
		}))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?source=canvas", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestActivityList_SourceAgent(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	// Expect query with "source_id IS NOT NULL"
	mock.ExpectQuery(`SELECT .+ FROM activity_logs WHERE workspace_id = .+ AND source_id IS NOT NULL`).
		WithArgs("ws-1", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"duration_ms", "status", "error_detail", "created_at",
		}))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?source=agent", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestActivityList_SourceInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?source=bogus", nil)
	handler.List(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid source, got %d", w.Code)
	}
}

func TestActivityList_SourceWithType(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	// Both type and source filters
	mock.ExpectQuery(`SELECT .+ FROM activity_logs WHERE workspace_id = .+ AND activity_type = .+ AND source_id IS NULL`).
		WithArgs("ws-1", "a2a_receive", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"duration_ms", "status", "error_detail", "created_at",
		}))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?type=a2a_receive&source=canvas", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// ---------- Activity List peer_id filter ----------
//
// peer_id surfaces the conversation history with one specific peer
// for the wheel-side chat_history MCP tool. The filter joins
// (source_id = $X OR target_id = $X) so both inbound (where this
// peer was the sender) and outbound (where this peer was the
// recipient) turns appear in the same view, ordered by created_at.

const testPeerUUID = "11111111-2222-3333-4444-555555555555"

func TestActivityList_PeerIDFilter(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	// peer_id binds twice in the query (source_id OR target_id) but is
	// added to args once — sqlmock matches positional args, so the
	// binding shape is what matters.
	mock.ExpectQuery(
		`SELECT .+ FROM activity_logs WHERE workspace_id = .+ AND \(source_id = .+ OR target_id = .+\)`,
	).
		WithArgs("ws-1", testPeerUUID, 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"tool_trace", "duration_ms", "status", "error_detail", "created_at",
		}))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(
		"GET", "/workspaces/ws-1/activity?peer_id="+testPeerUUID, nil,
	)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestActivityList_PeerIDComposesWithType(t *testing.T) {
	// peer_id + type + source must compose into a single AND-chain so
	// the wheel can fetch e.g. "all peer_agent inbound from peer X" in
	// one round-trip. Pin both args + arg order so a future refactor
	// of the builder can't silently rearrange placeholders.
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	mock.ExpectQuery(
		`SELECT .+ FROM activity_logs WHERE workspace_id = .+ AND activity_type = .+ AND source_id IS NOT NULL AND \(source_id = .+ OR target_id = .+\)`,
	).
		WithArgs("ws-1", "a2a_receive", testPeerUUID, 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"tool_trace", "duration_ms", "status", "error_detail", "created_at",
		}))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(
		"GET",
		"/workspaces/ws-1/activity?type=a2a_receive&source=agent&peer_id="+testPeerUUID,
		nil,
	)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestActivityList_PeerIDRejectsNonUUID(t *testing.T) {
	// Trust-boundary check: a malformed peer_id must 400 before any
	// query is built. Defends against caller bugs (typoed UUID,
	// leading whitespace) and against any future code path that might
	// otherwise interpolate the value into the URL or another query.
	gin.SetMode(gin.TestMode)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	for _, bad := range []string{
		"not-a-uuid",
		"%27%20OR%201%3D1%20--",                          // URL-encoded ' OR 1=1 --
		"11111111-2222-3333-4444",                        // truncated
		"11111111-2222-3333-4444-555555555555-extra",     // overlong
		"11111111-2222-3333-4444-55555555555G",           // non-hex
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
		c.Request = httptest.NewRequest(
			"GET", "/workspaces/ws-1/activity?peer_id="+bad, nil,
		)
		handler.List(c)

		if w.Code != http.StatusBadRequest {
			t.Errorf("peer_id=%q: expected 400, got %d (%s)", bad, w.Code, w.Body.String())
		}
	}
}

// ---------- before_ts paging knob ----------
//
// before_ts is the wall-clock paging companion to peer_id — the agent
// walks backward through long histories by passing the oldest
// `created_at` from the previous response. Validated as RFC3339 at the
// trust boundary; mirrors the strict-inequality shape since_id uses
// for forward paging.

func TestActivityList_BeforeTSFilter(t *testing.T) {
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	cutoff, _ := time.Parse(time.RFC3339, "2026-05-01T00:00:00Z")
	mock.ExpectQuery(
		`SELECT .+ FROM activity_logs WHERE workspace_id = .+ AND created_at < .+`,
	).
		WithArgs("ws-1", cutoff, 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"tool_trace", "duration_ms", "status", "error_detail", "created_at",
		}))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(
		"GET", "/workspaces/ws-1/activity?before_ts=2026-05-01T00%3A00%3A00Z", nil,
	)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestActivityList_BeforeTSComposesWithPeerID(t *testing.T) {
	// peer_id + before_ts: the canonical wheel-side chat_history paging
	// shape. Pin both args + arg order so a future builder refactor
	// can't silently drop one filter or reorder placeholders.
	mock := setupTestDB(t)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	cutoff, _ := time.Parse(time.RFC3339, "2026-05-01T00:00:00Z")
	mock.ExpectQuery(
		`SELECT .+ FROM activity_logs WHERE workspace_id = .+ AND \(source_id = .+ OR target_id = .+\) AND created_at < .+`,
	).
		WithArgs("ws-1", testPeerUUID, cutoff, 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "activity_type", "source_id", "target_id",
			"method", "summary", "request_body", "response_body",
			"tool_trace", "duration_ms", "status", "error_detail", "created_at",
		}))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(
		"GET",
		"/workspaces/ws-1/activity?peer_id="+testPeerUUID+"&before_ts=2026-05-01T00%3A00%3A00Z",
		nil,
	)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestActivityList_BeforeTSRejectsInvalidFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	for _, bad := range []string{
		"yesterday",
		"2026-05-01",                            // missing time component
		"2026-05-01%2000%3A00%3A00",             // URL-encoded space instead of T
		"%27%20OR%201%3D1%20--",                 // URL-encoded SQL injection
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
		c.Request = httptest.NewRequest(
			"GET", "/workspaces/ws-1/activity?before_ts="+bad, nil,
		)
		handler.List(c)

		if w.Code != http.StatusBadRequest {
			t.Errorf("before_ts=%q: expected 400, got %d (%s)", bad, w.Code, w.Body.String())
		}
	}
}

// ---------- Activity type allowlist (#125: memory_write added) ----------

func TestActivityReport_AcceptsMemoryWriteType(t *testing.T) {
	mockDB, mock, _ := sqlmock.New()
	defer mockDB.Close()
	db.DB = mockDB

	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-mem"}}
	body := `{"workspace_id":"ws-mem","activity_type":"memory_write","summary":"[LOCAL] x","status":"ok"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-mem/activity", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Report(c)

	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Errorf("memory_write should be accepted; got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivityReport_RejectsUnknownType(t *testing.T) {
	mockDB, _, _ := sqlmock.New()
	defer mockDB.Close()
	db.DB = mockDB

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-x"}}
	body := `{"workspace_id":"ws-x","activity_type":"made_up_type","summary":"x","status":"ok"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-x/activity", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Report(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown type should 400; got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "memory_write") {
		t.Errorf("error message should list valid types including memory_write; got %s", w.Body.String())
	}
}

func TestNotify_PersistsToActivityLogsForReloadRecovery(t *testing.T) {
	// Regression guard for the "responses gone on reload" bug. send_message_to_user
	// pushes (which route through Notify) used to be broadcast-only — they
	// rendered in the canvas but vanished on page reload because nothing
	// wrote them to activity_logs. The chat history loader queries
	// `type=a2a_receive&source=canvas`, so the persisted row must:
	//   - Use activity_type='a2a_receive' (loader's filter)
	//   - Have source_id NULL (canvas-source filter)
	//   - Carry the message text in response_body so extractResponseText
	//     can reconstruct the agent reply on reload
	mockDB, mock, _ := sqlmock.New()
	defer mockDB.Close()
	db.DB = mockDB

	// Workspace existence check
	mock.ExpectQuery(`SELECT name FROM workspaces`).
		WithArgs("ws-notify").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("DD"))

	// Persistence INSERT — verify shape
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(
			"ws-notify",
			sqlmock.AnyArg(), // summary
			sqlmock.AnyArg(), // response_body JSON
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-notify"}}
	body := `{"message":"agent reply that arrived after the sync POST timed out"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-notify/notify", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Notify(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations not met: %v", err)
	}
}

func TestNotify_WithAttachments_PersistsFilePartsForReload(t *testing.T) {
	// Pins the response_body shape: must include {result: msg, parts: [{kind:"file", file: {...}}]}
	// so the chat history loader's extractFilesFromTask reconstructs the
	// download chips after a page reload. Without `parts`, the bubble
	// shows up but the attachment chip is silently dropped on every
	// refresh.
	mockDB, mock, _ := sqlmock.New()
	defer mockDB.Close()
	db.DB = mockDB

	mock.ExpectQuery(`SELECT name FROM workspaces`).
		WithArgs("ws-attach").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("DD"))

	// Capture the JSONB arg so we can assert on the persisted shape
	// AFTER the call (must include parts[].kind=file so reload
	// reconstructs download chips). Use AnyArg() for the binding
	// gate — the substring asserts below are what actually validate
	// the shape; a custom matcher that always returned true would
	// be misleading about which step does the gating.
	var capturedRespJSON string
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs("ws-attach", sqlmock.AnyArg(), sqlmockCaptureArg(&capturedRespJSON)).
		WillReturnResult(sqlmock.NewResult(1, 1))

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-attach"}}
	body := `{
		"message": "Here's the build:",
		"attachments": [
			{"uri": "workspace:/workspace/.molecule/chat-uploads/abc-build.zip",
			 "name": "build.zip", "mimeType": "application/zip", "size": 12345}
		]
	}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-attach/notify", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Notify(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations not met: %v", err)
	}
	// Verify the persisted response_body has both the text (so chat
	// reload renders the bubble) AND a parts[].kind=file (so reload
	// renders the download chip).
	if !strings.Contains(capturedRespJSON, `"result":"Here's the build:"`) {
		t.Errorf("response_body missing result text: %s", capturedRespJSON)
	}
	if !strings.Contains(capturedRespJSON, `"kind":"file"`) ||
		!strings.Contains(capturedRespJSON, `"name":"build.zip"`) ||
		!strings.Contains(capturedRespJSON, `workspace:/workspace/.molecule/chat-uploads/abc-build.zip`) {
		t.Errorf("response_body missing file part — chat reload won't render the chip: %s", capturedRespJSON)
	}
}

func TestNotify_RejectsAttachmentWithEmptyURIOrName(t *testing.T) {
	// Critical regression guard. gin's go-playground/validator does NOT
	// iterate slice elements without `dive`, so `binding:"required"` on
	// NotifyAttachment.URI/Name would silently fail to enforce on
	// `attachments: [{"uri":"","name":""}]`. Without this explicit
	// per-element check, the platform broadcasts empty-URI chips that
	// render blank in the canvas AND get persisted in activity_logs
	// for every page reload to re-render. Pre-fix: passed validation.
	cases := []struct {
		name string
		body string
	}{
		{"empty uri", `{"message":"hi","attachments":[{"uri":"","name":"file.zip"}]}`},
		{"empty name", `{"message":"hi","attachments":[{"uri":"workspace:/x","name":""}]}`},
		{"both empty", `{"message":"hi","attachments":[{"uri":"","name":""}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockDB, _, _ := sqlmock.New()
			defer mockDB.Close()
			db.DB = mockDB
			// No DB expectations — handler must reject with 400 BEFORE
			// reaching SELECT/INSERT. sqlmock will fail "expectations not met"
			// only if the handler unexpectedly queries.

			broadcaster := newTestBroadcaster()
			handler := NewActivityHandler(broadcaster)
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "ws-x"}}
			c.Request = httptest.NewRequest("POST", "/workspaces/ws-x/notify", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.Notify(c)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for %s, got %d: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// sqlmockCaptureArg returns an sqlmock.Argument that always matches AND
// writes the string-coerced driver value into `dst`. Lets a test
// inspect the actual JSON bytes written to a JSONB column without
// pretending to enforce shape — that's what the downstream substring
// asserts in the test body do.
func sqlmockCaptureArg(dst *string) sqlmock.Argument {
	return sqlmockArgFn(func(v driver.Value) bool {
		if s, ok := v.(string); ok {
			*dst = s
		}
		return true
	})
}

type sqlmockArgFn func(driver.Value) bool

func (f sqlmockArgFn) Match(v driver.Value) bool { return f(v) }

func TestNotify_DBFailure_StillBroadcastsAnd200(t *testing.T) {
	// Persistence is best-effort — a DB hiccup must NOT block the
	// WebSocket push (which the user is already seeing in their open
	// canvas). Pre-fix the WS push always succeeded; we don't want
	// the new persistence step to regress that path.
	mockDB, mock, _ := sqlmock.New()
	defer mockDB.Close()
	db.DB = mockDB

	mock.ExpectQuery(`SELECT name FROM workspaces`).
		WithArgs("ws-x").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("DD"))
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnError(fmt.Errorf("simulated db hiccup"))

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-x"}}
	body := `{"message":"hi"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-x/notify", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Notify(c)

	if w.Code != http.StatusOK {
		t.Errorf("DB failure must not break the response; got %d", w.Code)
	}
}

// ==================== Direct unit tests for SessionSearch helpers ====================

// --- parseSessionSearchParams ---

func TestParseSessionSearchParams_Defaults(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)

	q, limit := parseSessionSearchParams(c)
	if q != "" {
		t.Errorf("expected empty q, got %q", q)
	}
	if limit != 50 {
		t.Errorf("expected default limit 50, got %d", limit)
	}
}

func TestParseSessionSearchParams_CustomLimit(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x?q=foo&limit=75", nil)

	q, limit := parseSessionSearchParams(c)
	if q != "foo" {
		t.Errorf("expected q=foo, got %q", q)
	}
	if limit != 75 {
		t.Errorf("expected limit=75, got %d", limit)
	}
}

func TestParseSessionSearchParams_LimitCappedAt200(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x?limit=9999", nil)

	_, limit := parseSessionSearchParams(c)
	if limit != 200 {
		t.Errorf("expected cap 200, got %d", limit)
	}
}

func TestParseSessionSearchParams_InvalidLimitUsesDefault(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x?limit=abc", nil)

	_, limit := parseSessionSearchParams(c)
	if limit != 50 {
		t.Errorf("expected default on invalid, got %d", limit)
	}
}

// --- buildSessionSearchQuery ---

func TestBuildSessionSearchQuery_NoFilters(t *testing.T) {
	sqlQuery, args := buildSessionSearchQuery("ws-1", "", 50)
	if strings.Contains(sqlQuery, "ILIKE") {
		t.Error("expected no ILIKE when query empty")
	}
	if len(args) != 2 || args[0] != "ws-1" || args[1] != 50 {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestBuildSessionSearchQuery_WithQuery(t *testing.T) {
	sqlQuery, args := buildSessionSearchQuery("ws-1", "foo", 25)
	if !strings.Contains(sqlQuery, "ILIKE") {
		t.Error("expected ILIKE when query provided")
	}
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d: %v", len(args), args)
	}
	if args[1] != "%foo%" {
		t.Errorf("expected LIKE pattern, got %v", args[1])
	}
	if args[2] != 25 {
		t.Errorf("expected limit=25, got %v", args[2])
	}
}

// --- scanSessionSearchRows ---

// fakeRows implements the minimal rows interface scanSessionSearchRows expects.
type fakeRows struct {
	data [][]interface{}
	i    int
	err  error
}

func (f *fakeRows) Next() bool { return f.i < len(f.data) }
func (f *fakeRows) Scan(dest ...interface{}) error {
	row := f.data[f.i]
	f.i++
	for i, v := range row {
		switch d := dest[i].(type) {
		case *string:
			*d = v.(string)
		case *[]byte:
			if v == nil {
				*d = nil
			} else {
				*d = v.([]byte)
			}
		case *time.Time:
			*d = v.(time.Time)
		}
	}
	return nil
}
func (f *fakeRows) Err() error { return f.err }

func TestScanSessionSearchRows_EmptyRows(t *testing.T) {
	items, err := scanSessionSearchRows(&fakeRows{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected empty result, got %d", len(items))
	}
}

func TestScanSessionSearchRows_MultipleRows(t *testing.T) {
	now := time.Now()
	rows := &fakeRows{
		data: [][]interface{}{
			{"activity", "a1", "ws-1", "task_update", "hello", "POST", "ok", []byte(`{"x":1}`), []byte(`{"y":2}`), now},
			{"memory", "m1", "ws-1", "TEAM", "note", "", "", []byte(nil), []byte(nil), now},
		},
	}
	items, err := scanSessionSearchRows(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0]["kind"] != "activity" {
		t.Errorf("first row kind: %v", items[0]["kind"])
	}
	if items[0]["request_body"] == nil {
		t.Error("expected request_body present on activity row")
	}
	if _, has := items[1]["request_body"]; has {
		t.Error("memory row should not have request_body (nil bytes)")
	}
}

// scanErrorRows returns a Scan error on the first row to cover the
// log-and-skip branch in scanSessionSearchRows.
type scanErrorRows struct {
	called bool
}

func (s *scanErrorRows) Next() bool {
	if !s.called {
		s.called = true
		return true
	}
	return false
}
func (s *scanErrorRows) Scan(dest ...interface{}) error { return fmt.Errorf("scan bad") }
func (s *scanErrorRows) Err() error                     { return nil }

func TestScanSessionSearchRows_ScanErrorSkipped(t *testing.T) {
	items, err := scanSessionSearchRows(&scanErrorRows{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items (scan error skipped), got %d", len(items))
	}
}

func TestScanSessionSearchRows_RowsErrPropagates(t *testing.T) {
	f := &fakeRows{err: fmt.Errorf("boom")}
	_, err := scanSessionSearchRows(f)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
}

// recordingBroadcaster records every BroadcastOnly invocation so a test
// can assert what made it onto the wire. Implements events.EventEmitter.
type recordingBroadcaster struct {
	calls []recordedBroadcast
}

type recordedBroadcast struct {
	workspaceID string
	eventType   string
	payload     map[string]interface{}
}

func (c *recordingBroadcaster) RecordAndBroadcast(_ context.Context, _ string, _ string, _ interface{}) error {
	return nil
}

func (c *recordingBroadcaster) BroadcastOnly(workspaceID string, eventType string, payload interface{}) {
	// Re-marshal/unmarshal so tests assert the actual wire shape (matches
	// what hub.Broadcast does before sending). json.RawMessage values in
	// the source payload survive the round-trip as their underlying JSON.
	raw, err := json.Marshal(payload)
	if err != nil {
		c.calls = append(c.calls, recordedBroadcast{workspaceID, eventType, nil})
		return
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		c.calls = append(c.calls, recordedBroadcast{workspaceID, eventType, nil})
		return
	}
	c.calls = append(c.calls, recordedBroadcast{workspaceID, eventType, out})
}

// TestLogActivity_Broadcast_IncludesRequestAndResponseBodies pins the
// fix for the canvas Agent Comms "Delegating to <peer>" boilerplate
// regression: without request_body/response_body in the live broadcast,
// the panel renders the fallback string and the actual task text only
// appears after a refresh re-fetches the row from /activity.
func TestLogActivity_Broadcast_IncludesRequestAndResponseBodies(t *testing.T) {
	mock := setupTestDB(t)
	defer mock.ExpectationsWereMet()

	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(1, 1))

	cb := &recordingBroadcaster{}
	srcID := "ws-source"
	tgtID := "ws-target"
	method := "message/send"
	summary := "Delegating to ws-target"
	status := "ok"

	LogActivity(context.Background(), cb, ActivityParams{
		WorkspaceID:  "ws-source",
		ActivityType: "a2a_send",
		SourceID:     &srcID,
		TargetID:     &tgtID,
		Method:       &method,
		Summary:      &summary,
		RequestBody:  map[string]interface{}{"task": "audit moleculesai.app for performance"},
		ResponseBody: nil,
		Status:       status,
	})

	if len(cb.calls) != 1 {
		t.Fatalf("expected 1 broadcast, got %d", len(cb.calls))
	}
	payload := cb.calls[0].payload
	if payload["activity_type"] != "a2a_send" {
		t.Errorf("activity_type missing/wrong: %v", payload["activity_type"])
	}
	// Critical: request_body must be present and carry the task text so
	// the canvas's live-update path can render the actual delegation
	// content instead of "Delegating to <peer>".
	rb, ok := payload["request_body"].(map[string]interface{})
	if !ok {
		t.Fatalf("request_body missing from broadcast payload: got %#v", payload["request_body"])
	}
	if got := rb["task"]; got != "audit moleculesai.app for performance" {
		t.Errorf("request_body.task = %v, want the actual task text", got)
	}
	// response_body was nil — must NOT be present (otherwise the canvas
	// renders an empty agent reply bubble).
	if _, present := payload["response_body"]; present {
		t.Errorf("response_body should be omitted when nil, got %v", payload["response_body"])
	}
}

// TestLogActivityTx_DefersBroadcastUntilCommitHook pins the #149
// contract: LogActivityTx returns a commitHook that the caller MUST
// invoke after tx.Commit(); the broadcast MUST NOT fire from inside
// LogActivityTx itself. Firing inside would leak a websocket event
// for a row that the caller may roll back, painting a ghost message
// into the canvas's optimistic UI that disappears on the next refresh.
func TestLogActivityTx_DefersBroadcastUntilCommitHook(t *testing.T) {
	mock := setupTestDB(t)
	defer mock.ExpectationsWereMet()

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tx, err := db.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	cb := &recordingBroadcaster{}
	method := "chat_upload_receive"
	hook, err := LogActivityTx(context.Background(), tx, cb, ActivityParams{
		WorkspaceID:  "ws-123",
		ActivityType: "a2a_receive",
		Method:       &method,
		Status:       "ok",
	})
	if err != nil {
		t.Fatalf("LogActivityTx: %v", err)
	}
	if len(cb.calls) != 0 {
		t.Errorf("broadcast leaked before commitHook: got %d calls", len(cb.calls))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	hook()
	if len(cb.calls) != 1 {
		t.Fatalf("commitHook must broadcast exactly once, got %d", len(cb.calls))
	}
	if cb.calls[0].eventType != "ACTIVITY_LOGGED" {
		t.Errorf("event type = %q, want ACTIVITY_LOGGED", cb.calls[0].eventType)
	}
}

// TestLogActivityTx_InsertError_NoHook_NoBroadcast — when the INSERT
// fails inside the Tx, LogActivityTx returns an error and a nil
// commitHook. The caller is expected to Rollback; no broadcast can
// possibly fire because the hook never exists.
func TestLogActivityTx_InsertError_NoHook_NoBroadcast(t *testing.T) {
	mock := setupTestDB(t)
	defer mock.ExpectationsWereMet()

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnError(errors.New("constraint violation simulated"))
	mock.ExpectRollback()

	tx, err := db.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	cb := &recordingBroadcaster{}
	method := "chat_upload_receive"
	hook, err := LogActivityTx(context.Background(), tx, cb, ActivityParams{
		WorkspaceID:  "ws-123",
		ActivityType: "a2a_receive",
		Method:       &method,
		Status:       "ok",
	})
	if err == nil {
		t.Fatal("expected error on INSERT failure, got nil")
	}
	if hook != nil {
		t.Errorf("commitHook must be nil on insert error, got non-nil hook")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(cb.calls) != 0 {
		t.Errorf("broadcast must NOT fire on insert error, got %d calls", len(cb.calls))
	}
}

// TestLogActivityTx_NilTx_Errors — passing a nil tx is caller misuse.
// Return an error rather than panicking on the nil receiver inside
// ExecContext (which would crash the request goroutine and surface as
// a 500 with no log line tying it to the bad call site).
func TestLogActivityTx_NilTx_Errors(t *testing.T) {
	cb := &recordingBroadcaster{}
	hook, err := LogActivityTx(context.Background(), nil, cb, ActivityParams{
		WorkspaceID:  "ws-123",
		ActivityType: "a2a_receive",
		Status:       "ok",
	})
	if err == nil {
		t.Fatal("nil tx must error, got nil")
	}
	if hook != nil {
		t.Errorf("commitHook must be nil when tx is nil, got non-nil hook")
	}
	if len(cb.calls) != 0 {
		t.Errorf("broadcast must NOT fire on nil-tx error, got %d", len(cb.calls))
	}
}

func TestLogActivity_Broadcast_IncludesResponseBody(t *testing.T) {
	mock := setupTestDB(t)
	defer mock.ExpectationsWereMet()

	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(1, 1))

	cb := &recordingBroadcaster{}
	srcID := "ws-source"
	method := "message/send"
	status := "ok"

	LogActivity(context.Background(), cb, ActivityParams{
		WorkspaceID:  "ws-source",
		ActivityType: "a2a_receive",
		SourceID:     &srcID,
		Method:       &method,
		RequestBody:  map[string]interface{}{"task": "audit"},
		ResponseBody: map[string]interface{}{"result": "LCP 2.1s, INP 180ms, CLS 0.05"},
		Status:       status,
	})

	if len(cb.calls) != 1 {
		t.Fatalf("expected 1 broadcast, got %d", len(cb.calls))
	}
	payload := cb.calls[0].payload
	rb, ok := payload["response_body"].(map[string]interface{})
	if !ok {
		t.Fatalf("response_body missing from broadcast: got %#v", payload["response_body"])
	}
	if got := rb["result"]; got != "LCP 2.1s, INP 180ms, CLS 0.05" {
		t.Errorf("response_body.result = %v, want the actual reply text", got)
	}
}
