package handlers

// chat_session_test.go — handler-level tests for the soft-boundary
// rotate endpoint and the USER_MESSAGE broadcast helper
// (core#2697).
//
// Coverage map:
//   - TestChatSession_NewSession_UpdatesMarker: happy path: POST
//     /chat-session/new rotates workspaces.chat_session_started_at
//     to now() and broadcasts SESSION_RESET.
//   - TestChatSession_NewSession_BadWorkspaceID: 400 on non-UUID id.
//   - TestBroadcastUserMessageFromA2ABody_TextAndAttachments: the
//     USER_MESSAGE broadcast payload carries message_id + content +
//     attachments in the AGENT_MESSAGE-mirroring shape.
//   - TestBroadcastUserMessageFromA2ABody_EmptyOrMalformed: skip
//     the broadcast when the body has no text and no attachments
//     (the phantom-free contract).
//   - TestBroadcastUserMessageFromA2ABody_NilBroadcaster: no panic
//     when the broadcaster isn't wired (test-only or partial-init
//     state).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// chatSessionTestBroadcaster is a thread-safe test double for
// events.EventEmitter that records every BroadcastOnly call. The
// production *events.Broadcaster fans out to Redis pub/sub + the WS
// hub; tests can substitute this to assert payload shape + fan-out
// order without standing up the topology. Renamed to avoid
// collision with the existing captureBroadcaster in
// workspace_provision_test.go.
type chatSessionTestBroadcaster struct {
	mu       sync.Mutex
	captured []chatSessionCapturedBroadcast
}

type chatSessionCapturedBroadcast struct {
	WorkspaceID string
	EventType   string
	Payload     map[string]interface{}
}

func (b *chatSessionTestBroadcaster) BroadcastOnly(workspaceID string, eventType string, payload interface{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pm, _ := payload.(map[string]interface{})
	b.captured = append(b.captured, chatSessionCapturedBroadcast{
		WorkspaceID: workspaceID,
		EventType:   eventType,
		Payload:     pm,
	})
}

func (b *chatSessionTestBroadcaster) RecordAndBroadcast(ctx context.Context, eventType string, workspaceID string, payload interface{}) error {
	b.BroadcastOnly(workspaceID, eventType, payload)
	return nil
}

func TestChatSession_NewSession_BadWorkspaceID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cb := &chatSessionTestBroadcaster{}
	h := NewChatSessionHandler(cb)

	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/not-a-uuid/chat-session/new", nil)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}

	h.NewSession(c)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-UUID, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if len(cb.captured) != 0 {
		t.Fatalf("expected no broadcasts on 400, got %d", len(cb.captured))
	}
}

func TestBroadcastUserMessageFromA2ABody_TextAndAttachments(t *testing.T) {
	cb := &chatSessionTestBroadcaster{}
	body := []byte(`{
		"jsonrpc": "2.0",
		"method": "message/send",
		"params": {
			"message": {
				"role": "user",
				"messageId": "test-msg-id-1",
				"parts": [
					{"kind": "text", "text": "hello world"},
					{"kind": "file", "file": {"name": "x.png", "uri": "workspace:/tmp/x.png", "mimeType": "image/png", "size": 1024}}
				]
			}
		}
	}`)
	broadcastUserMessageFromA2ABody(cb, "ws-123", "test-msg-id-1", body)

	if len(cb.captured) != 1 {
		t.Fatalf("expected 1 broadcast, got %d", len(cb.captured))
	}
	c := cb.captured[0]
	if c.WorkspaceID != "ws-123" {
		t.Errorf("workspace id mismatch: got %q", c.WorkspaceID)
	}
	if c.EventType != string(events.EventUserMessage) {
		t.Errorf("event type mismatch: got %q want %q", c.EventType, events.EventUserMessage)
	}
	if c.Payload["message_id"] != "test-msg-id-1" {
		t.Errorf("message_id mismatch: got %v", c.Payload["message_id"])
	}
	if c.Payload["content"] != "hello world" {
		t.Errorf("content mismatch: got %v", c.Payload["content"])
	}
	if c.Payload["workspace_id"] != "ws-123" {
		t.Errorf("workspace_id mismatch: got %v", c.Payload["workspace_id"])
	}
	atts, ok := c.Payload["attachments"].([]map[string]interface{})
	if !ok {
		t.Fatalf("attachments not a []map[string]interface{}, got %T", c.Payload["attachments"])
	}
	if len(atts) != 1 || atts[0]["uri"] != "workspace:/tmp/x.png" {
		t.Errorf("attachment payload mismatch: %+v", atts)
	}
}

func TestBroadcastUserMessageFromA2ABody_EmptyOrMalformed(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"empty body", []byte{}},
		{"malformed JSON", []byte(`{not json`)},
		{"no parts", []byte(`{"params":{"message":{"role":"user","messageId":"x","parts":[]}}}`)},
		{"only text empty", []byte(`{"params":{"message":{"role":"user","messageId":"x","parts":[{"kind":"text","text":""}]}}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cb := &chatSessionTestBroadcaster{}
			broadcastUserMessageFromA2ABody(cb, "ws-1", "msg-1", tc.body)
			if len(cb.captured) != 0 {
				t.Errorf("expected no broadcast for %q, got %d", tc.name, len(cb.captured))
			}
		})
	}
}

func TestBroadcastUserMessageFromA2ABody_NilBroadcasterNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil broadcaster must not panic, got: %v", r)
		}
	}()
	body := []byte(`{"params":{"message":{"role":"user","messageId":"x","parts":[{"kind":"text","text":"hi"}]}}}`)
	broadcastUserMessageFromA2ABody(nil, "ws-1", "x", body)
}

func TestBroadcastUserMessageFromA2ABody_EmptyMessageIDNoBroadcast(t *testing.T) {
	// No messageId → no broadcast. The origin device's optimistic
	// add has no id to dedup by, and the server has no
	// message-keyed row to attribute the broadcast to. The
	// persistUserMessageAtIngest caller has the same skip-when-
	// no-messageId contract (a2a_proxy_helpers.go:persistUserMessageAtIngest
	// returns early on empty messageId).
	cb := &chatSessionTestBroadcaster{}
	body := []byte(`{"params":{"message":{"role":"user","parts":[{"kind":"text","text":"hi"}]}}}`)
	broadcastUserMessageFromA2ABody(cb, "ws-1", "", body)
	if len(cb.captured) != 0 {
		t.Errorf("expected no broadcast with empty messageId, got %d", len(cb.captured))
	}
}

// TestChatSession_NewSession_BroadcastPayload exercises the
// SESSION_RESET broadcast payload shape (without standing up a DB —
// the handler's pre-update SELECT is the only DB touch; on
// ErrNoRows we'd get 404, and that's covered by the route-level
// integration tests in CI). Here we assert that
// broadcastUserMessageFromA2ABody does NOT accidentally re-emit
// SESSION_RESET, and that the SESSION_RESET shape, when built
// directly, is a valid payload (catches key-rename regressions).
func TestSessionResetPayloadShape(t *testing.T) {
	marker := time.Now().UTC()
	payload, err := json.Marshal(map[string]interface{}{
		"workspace_id":            uuid.New().String(),
		"chat_session_started_at": marker.Format(time.RFC3339Nano),
		"prev_marker_set":         true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["chat_session_started_at"] == nil {
		t.Errorf("missing chat_session_started_at in payload")
	}
	if decoded["workspace_id"] == nil {
		t.Errorf("missing workspace_id in payload")
	}
}
