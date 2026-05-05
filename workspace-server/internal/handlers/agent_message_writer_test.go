package handlers

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// AgentMessageWriter is the SSOT for agent → user chat delivery
// (RFC #2945 PR-A). These tests pin the contract the writer
// guarantees: workspace lookup, broadcast, INSERT, error semantics —
// every shape that producers (Notify, toolSendMessageToUser, future
// channels) rely on.
//
// Pre-consolidation, the broadcast-then-INSERT logic was duplicated
// across two handlers and they drifted (reno-stars, 2026-05-05). With
// the writer being the only place this logic lives, these tests are
// the regression line for every chat-bearing path simultaneously.

// jsonMatcher is a sqlmock Argument matcher that decodes the actual
// SQL arg as JSON and runs a caller-supplied predicate over the
// resulting structure. Tighter than substring matching (which can
// false-pass on a renamed key) and tolerant of map-key ordering
// (which exact-string matching is not).
type jsonMatcher struct {
	predicate func(parsed map[string]any) bool
	desc      string
}

func (m jsonMatcher) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return false
	}
	return m.predicate(parsed)
}

// stringMatcher pins exact prefix/suffix/equality checks against a
// driver.Value that's actually a string.
type stringMatcher func(string) bool

func (f stringMatcher) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	return f(s)
}

// capturingEmitter records every BroadcastOnly call so tests can pin
// the WS event shape without a real ws.Hub. RecordAndBroadcast is
// also captured for completeness — the writer doesn't call it today,
// but a future producer might, and a captured-but-unasserted record
// is easier to diagnose than a nil panic.
type capturingEmitter struct {
	events []capturedEvent
}

type capturedEvent struct {
	workspaceID string
	eventType   string
	payload     interface{}
}

func (c *capturingEmitter) BroadcastOnly(workspaceID string, eventType string, payload interface{}) {
	c.events = append(c.events, capturedEvent{workspaceID, eventType, payload})
}

func (c *capturingEmitter) RecordAndBroadcast(_ context.Context, eventType string, workspaceID string, payload interface{}) error {
	c.events = append(c.events, capturedEvent{workspaceID, eventType, payload})
	return nil
}

// TestAgentMessageWriter_Send_Success_NoAttachments pins the happy
// path: workspace lookup, broadcast, INSERT, return nil.
func TestAgentMessageWriter_Send_Success_NoAttachments(t *testing.T) {
	mock := setupTestDB(t)
	w := NewAgentMessageWriter(db.DB, newTestBroadcaster())

	mock.ExpectQuery("SELECT name FROM workspaces").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("CEO Ryan PC"))

	mock.ExpectExec(`INSERT INTO activity_logs.*'a2a_receive'.*'notify'`).
		WithArgs(
			"ws-1",
			sqlmock.AnyArg(), // summary
			`{"result":"hi"}`,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := w.Send(context.Background(), "ws-1", "hi", nil); err != nil {
		t.Fatalf("Send returned %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
}

// TestAgentMessageWriter_Send_Success_WithAttachments pins the file
// attachment shape — response_body MUST contain a parts[] array with
// kind=file entries so the canvas hydrater renders download chips.
// Drift here = chips disappear on chat reload.
func TestAgentMessageWriter_Send_Success_WithAttachments(t *testing.T) {
	mock := setupTestDB(t)
	w := NewAgentMessageWriter(db.DB, newTestBroadcaster())

	mock.ExpectQuery("SELECT name FROM workspaces").
		WithArgs("ws-att").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Ryan"))

	mock.ExpectExec(`INSERT INTO activity_logs.*'a2a_receive'.*'notify'`).
		WithArgs(
			"ws-att",
			sqlmock.AnyArg(),
			jsonMatcher{
				desc: "response_body has result + parts with kind=file metadata",
				predicate: func(p map[string]any) bool {
					if p["result"] != "see attached" {
						return false
					}
					parts, ok := p["parts"].([]any)
					if !ok || len(parts) != 1 {
						return false
					}
					part, ok := parts[0].(map[string]any)
					if !ok {
						return false
					}
					if part["kind"] != "file" {
						return false
					}
					file, ok := part["file"].(map[string]any)
					if !ok {
						return false
					}
					return file["uri"] == "workspace://x.zip" &&
						file["name"] == "x.zip" &&
						file["mimeType"] == "application/zip" &&
						file["size"].(float64) == 1234
				},
			},
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	atts := []AgentMessageAttachment{
		{URI: "workspace://x.zip", Name: "x.zip", MimeType: "application/zip", Size: 1234},
	}
	if err := w.Send(context.Background(), "ws-att", "see attached", atts); err != nil {
		t.Fatalf("Send returned %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
}

// TestAgentMessageWriter_Send_WorkspaceNotFound pins ErrWorkspaceNotFound
// short-circuit. Must NOT broadcast, MUST NOT INSERT — caller will 404
// or surface a JSON-RPC error.
func TestAgentMessageWriter_Send_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	w := NewAgentMessageWriter(db.DB, emitter)

	mock.ExpectQuery("SELECT name FROM workspaces").
		WithArgs("ws-missing").
		WillReturnRows(sqlmock.NewRows([]string{"name"}))

	err := w.Send(context.Background(), "ws-missing", "lost in the void", nil)
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Errorf("Send returned %v, want ErrWorkspaceNotFound", err)
	}
	if len(emitter.events) != 0 {
		t.Errorf("workspace-not-found path MUST NOT broadcast, got %d events", len(emitter.events))
	}
	// Implicit: no INSERT expectation registered, so a stray INSERT
	// would fail ExpectationsWereMet.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations (INSERT must NOT fire on workspace-not-found): %v", err)
	}
}

// TestAgentMessageWriter_Send_DBInsertFailureStillReturnsNil pins the
// "best-effort persistence" contract: when the activity_log INSERT
// fails (DB hiccup, transient connection, constraint), the writer
// MUST still return nil. The broadcast already succeeded; the user
// has seen the message; returning an error here would cause the
// caller (and the agent calling the tool) to retry and double-
// broadcast.
func TestAgentMessageWriter_Send_DBInsertFailureStillReturnsNil(t *testing.T) {
	mock := setupTestDB(t)
	w := NewAgentMessageWriter(db.DB, newTestBroadcaster())

	mock.ExpectQuery("SELECT name FROM workspaces").
		WithArgs("ws-dbfail").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("CEO Ryan PC"))

	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnError(errors.New("transient db error"))

	err := w.Send(context.Background(), "ws-dbfail", "should not be lost from live chat", nil)
	if err != nil {
		t.Errorf("DB INSERT failure must return nil (broadcast already succeeded), got %v", err)
	}
}

// TestAgentMessageWriter_Send_PreviewTruncation pins the summary
// preview cap. Long messages (Ryan's onboarding-friction report was
// ~2k chars) must summarise to ≤80 chars + ellipsis so the activity
// table doesn't carry multi-KB summaries that bloat list queries.
func TestAgentMessageWriter_Send_PreviewTruncation(t *testing.T) {
	mock := setupTestDB(t)
	w := NewAgentMessageWriter(db.DB, newTestBroadcaster())

	mock.ExpectQuery("SELECT name FROM workspaces").
		WithArgs("ws-trunc").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Ryan"))

	longMsg := strings.Repeat("x", 200)
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(
			"ws-trunc",
			stringMatcher(func(s string) bool {
				if !strings.HasPrefix(s, "Agent message: ") {
					return false
				}
				preview := strings.TrimPrefix(s, "Agent message: ")
				if !strings.HasSuffix(preview, "…") {
					return false
				}
				body := strings.TrimSuffix(preview, "…")
				return len(body) == 80
			}),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := w.Send(context.Background(), "ws-trunc", longMsg, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("preview truncation drift: %v", err)
	}
}

// TestAgentMessageWriter_Send_BroadcastsAgentMessageEvent pins the
// WS event name + payload shape. The canvas's
// canvas-events.ts:AGENT_MESSAGE handler reads {message, workspace_id,
// name, attachments?} — drift here orphans every live chat panel.
func TestAgentMessageWriter_Send_BroadcastsAgentMessageEvent(t *testing.T) {
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	w := NewAgentMessageWriter(db.DB, emitter)

	mock.ExpectQuery("SELECT name FROM workspaces").
		WithArgs("ws-bc").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Workspace Name"))
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	atts := []AgentMessageAttachment{
		{URI: "workspace://a.txt", Name: "a.txt"},
	}
	if err := w.Send(context.Background(), "ws-bc", "hi", atts); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(emitter.events) != 1 {
		t.Fatalf("expected exactly 1 broadcast, got %d", len(emitter.events))
	}
	ev := emitter.events[0]
	if ev.eventType != "AGENT_MESSAGE" {
		t.Errorf("event type = %q, want AGENT_MESSAGE", ev.eventType)
	}
	if ev.workspaceID != "ws-bc" {
		t.Errorf("workspace_id = %q, want ws-bc", ev.workspaceID)
	}
	pl, ok := ev.payload.(map[string]interface{})
	if !ok {
		t.Fatalf("payload not a map: %T", ev.payload)
	}
	if pl["message"] != "hi" {
		t.Errorf("payload.message = %v, want hi", pl["message"])
	}
	if pl["workspace_id"] != "ws-bc" {
		t.Errorf("payload.workspace_id = %v, want ws-bc", pl["workspace_id"])
	}
	if pl["name"] != "Workspace Name" {
		t.Errorf("payload.name = %v, want Workspace Name", pl["name"])
	}
	if pl["attachments"] == nil {
		t.Error("payload.attachments missing on attachment-bearing send")
	}
}

// TestAgentMessageWriter_Send_DBErrorOnLookupReturnsWrapped pins the
// distinction between sql.ErrNoRows (legit not-found → 404) and real
// DB errors (connection drop → 503). Pre-followup the lookup branch
// returned ErrWorkspaceNotFound for ANY error, so during a DB outage
// every notify call surfaced as "workspace not found" and masked
// real incidents in alerting.
func TestAgentMessageWriter_Send_DBErrorOnLookupReturnsWrapped(t *testing.T) {
	mock := setupTestDB(t)
	w := NewAgentMessageWriter(db.DB, newTestBroadcaster())

	transientErr := errors.New("connection refused")
	mock.ExpectQuery("SELECT name FROM workspaces").
		WithArgs("ws-dbdown").
		WillReturnError(transientErr)

	err := w.Send(context.Background(), "ws-dbdown", "hi", nil)
	if err == nil {
		t.Fatal("expected wrapped DB error, got nil")
	}
	if errors.Is(err, ErrWorkspaceNotFound) {
		t.Errorf("DB outage MUST NOT surface as ErrWorkspaceNotFound (masks incidents in alerting); got %v", err)
	}
	if !errors.Is(err, transientErr) {
		t.Errorf("expected wrapped %v, got %v", transientErr, err)
	}
}

// TestTruncatePreviewRunes_RuneBoundary pins the multi-byte-safe
// truncation. The previous byte-slice version produced invalid UTF-8
// when the cut landed mid-codepoint (CJK, emoji, accented), and
// Postgres JSONB rejects invalid UTF-8 — INSERT fails, log.Printf
// fires, message vanishes from chat history. Per memory
// feedback_assert_exact_not_substring.md, pin the boundary cases
// directly.
func TestTruncatePreviewRunes_RuneBoundary(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		max      int
		want     string
	}{
		{"under-max ASCII", "hi", 80, "hi"},
		{"under-max CJK", "你好", 80, "你好"},
		{"exactly-at-max", "abcde", 5, "abcde"},
		{"truncate ASCII", "abcdefghij", 5, "abcde…"},
		{"truncate CJK at rune boundary", "你好世界你好世界", 4, "你好世界…"},
		{"truncate emoji at rune boundary", "😀😀😀😀😀😀", 3, "😀😀😀…"},
		// The pre-fix bug shape: byte-slice on non-ASCII would have
		// mangled the codepoint here. With rune-boundary truncation
		// the result is well-formed UTF-8.
		{"non-zero with emoji prefix", "🚀abcdefghijk", 5, "🚀abcd…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncatePreviewRunes(c.in, c.max)
			if got != c.want {
				t.Errorf("truncatePreviewRunes(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
			}
			// Always-valid UTF-8 invariant. A byte-slice truncation
			// could leave partial codepoints; this version must not.
			if !utf8.ValidString(got) {
				t.Errorf("truncatePreviewRunes(%q, %d) returned invalid UTF-8: %q", c.in, c.max, got)
			}
		})
	}
}

// TestAgentMessageWriter_Send_NonASCIIMessagePersists pins the end-to-end
// path for non-ASCII messages — the original reno-stars regression
// surfaced via byte-slice truncation breaking JSONB INSERT. Every
// handler-level test had ASCII content, so this branch had no
// coverage. Now it does.
func TestAgentMessageWriter_Send_NonASCIIMessagePersists(t *testing.T) {
	mock := setupTestDB(t)
	w := NewAgentMessageWriter(db.DB, newTestBroadcaster())

	// 200-rune CJK message — exceeds the 80-rune cap, would have hit
	// the byte-slice bug.
	msg := strings.Repeat("你", 200)

	mock.ExpectQuery("SELECT name FROM workspaces").
		WithArgs("ws-cjk").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("CEO Ryan PC"))

	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs(
			"ws-cjk",
			stringMatcher(func(s string) bool {
				if !strings.HasPrefix(s, "Agent message: ") {
					return false
				}
				preview := strings.TrimPrefix(s, "Agent message: ")
				if !strings.HasSuffix(preview, "…") {
					return false
				}
				body := strings.TrimSuffix(preview, "…")
				// 80 runes of 你 = 80 codepoints. Each is 3 bytes UTF-8.
				if utf8.RuneCountInString(body) != 80 {
					return false
				}
				// MUST be valid UTF-8 — pre-fix byte-slice would have
				// returned half a codepoint here.
				return utf8.ValidString(body)
			}),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := w.Send(context.Background(), "ws-cjk", msg, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("non-ASCII path drift: %v", err)
	}
}

// TestAgentMessageWriter_Send_OmitsAttachmentsKeyWhenEmpty pins the
// "no key when nil" wire contract — extra empty fields would force
// canvas consumers to defensively check for [] vs undefined; the
// existing AGENT_MESSAGE handler treats absence as "no attachments".
func TestAgentMessageWriter_Send_OmitsAttachmentsKeyWhenEmpty(t *testing.T) {
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	w := NewAgentMessageWriter(db.DB, emitter)

	mock.ExpectQuery("SELECT name FROM workspaces").
		WithArgs("ws-noatt").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("X"))
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := w.Send(context.Background(), "ws-noatt", "plain text", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(emitter.events))
	}
	pl := emitter.events[0].payload.(map[string]interface{})
	if _, present := pl["attachments"]; present {
		t.Errorf("attachments key MUST NOT be present when empty (canvas treats absence as 'none'); payload=%v", pl)
	}
}
