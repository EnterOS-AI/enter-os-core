package handlers

// chat_history_test.go — handler-level tests against a fake
// MessageStore. The parser-level parity tests against the canvas TS
// fixtures live in internal/messagestore/postgres_store_test.go;
// this file covers the HTTP-shape concerns (param validation,
// pagination passthrough, error mapping) without touching a DB.
//
// Why the split: PR-D extracted storage to messagestore.MessageStore.
// The handler is now a thin adapter — its tests should exercise the
// adapter (ParseQuery → store.List → emitJSON), not the parser. A
// future MessageStore impl (S3, vector store) shares the same
// handler; testing the handler against the interface keeps the
// adapter test independent of any specific impl.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/messagestore"
	"github.com/gin-gonic/gin"
)

const testWorkspaceID = "550e8400-e29b-41d4-a716-446655440000"

func init() {
	gin.SetMode(gin.TestMode)
}

// fakeStore is a stub MessageStore for handler-level tests. Every
// real store impl (Postgres, S3, vector) shares the handler — so a
// fake that records inputs + returns scripted outputs is the right
// granularity for HTTP-shape coverage.
type fakeStore struct {
	// LastWorkspaceID + LastOpts capture the call shape so the test
	// can assert the handler passed the right args to the store.
	LastWorkspaceID string
	LastOpts        messagestore.ListOptions

	// Returns — set per test.
	ReturnMessages   []messagestore.ChatMessage
	ReturnReachedEnd bool
	ReturnErr        error

	// Panic — if non-empty, List panics with this string. Used by
	// the resilience test to confirm the handler returns 502 on
	// store-impl failures rather than crashing the goroutine.
	PanicWith string
}

func (s *fakeStore) List(ctx context.Context, workspaceID string, opts messagestore.ListOptions) ([]messagestore.ChatMessage, bool, error) {
	if s.PanicWith != "" {
		panic(s.PanicWith)
	}
	s.LastWorkspaceID = workspaceID
	s.LastOpts = opts
	return s.ReturnMessages, s.ReturnReachedEnd, s.ReturnErr
}

// Compile-time assertion that fakeStore satisfies the interface.
// Catches drift if the interface changes and the fake stops being a
// drop-in for tests.
var _ messagestore.MessageStore = (*fakeStore)(nil)

func newRouter(store messagestore.MessageStore) *gin.Engine {
	r := gin.New()
	h := NewChatHistoryHandler(store)
	r.GET("/workspaces/:id/chat-history", h.List)
	return r
}

func doChatHistoryRequest(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// =====================================================================
// Param validation
// =====================================================================

func TestChatHistoryHandler_RejectsNonUUIDWorkspaceID(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store)

	w := doChatHistoryRequest(t, r, "/workspaces/not-a-uuid/chat-history")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-UUID, got %d", w.Code)
	}
	if store.LastWorkspaceID != "" {
		t.Errorf("non-UUID reached the store: %q", store.LastWorkspaceID)
	}
}

func TestChatHistoryHandler_RejectsMalformedBeforeTS(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store)

	w := doChatHistoryRequest(t, r, "/workspaces/"+testWorkspaceID+"/chat-history?before_ts=not-a-timestamp")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for malformed before_ts, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "RFC3339") {
		t.Errorf("error message should mention RFC3339; got %q", w.Body.String())
	}
}

func TestChatHistoryHandler_DefaultsLimitTo100(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store)

	doChatHistoryRequest(t, r, "/workspaces/"+testWorkspaceID+"/chat-history")
	if store.LastOpts.Limit != 100 {
		t.Errorf("default limit=%d want 100", store.LastOpts.Limit)
	}
	if store.LastOpts.HasBefore {
		t.Errorf("HasBefore should be false when no cursor passed")
	}
}

func TestChatHistoryHandler_ClampsLimitToMax1000(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store)

	doChatHistoryRequest(t, r, "/workspaces/"+testWorkspaceID+"/chat-history?limit=99999")
	if store.LastOpts.Limit != 1000 {
		t.Errorf("limit not clamped: got %d, want 1000", store.LastOpts.Limit)
	}
}

func TestChatHistoryHandler_IgnoresInvalidLimit(t *testing.T) {
	// Negative or zero limits should fall back to default rather
	// than reach the store (which rejects them as a programming bug).
	store := &fakeStore{}
	r := newRouter(store)

	for _, bad := range []string{"-1", "0", "abc"} {
		store.LastOpts = messagestore.ListOptions{}
		doChatHistoryRequest(t, r, "/workspaces/"+testWorkspaceID+"/chat-history?limit="+bad)
		if store.LastOpts.Limit != 100 {
			t.Errorf("limit=%q yielded %d, want default 100", bad, store.LastOpts.Limit)
		}
	}
}

// =====================================================================
// Pagination passthrough
// =====================================================================

func TestChatHistoryHandler_BeforeTSPassedToStore(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store)

	doChatHistoryRequest(t, r, "/workspaces/"+testWorkspaceID+"/chat-history?before_ts=2026-04-25T18:00:00Z&limit=25")

	if !store.LastOpts.HasBefore {
		t.Errorf("HasBefore=false but query passed before_ts")
	}
	got := store.LastOpts.BeforeTS.UTC().Format("2006-01-02T15:04:05Z")
	if got != "2026-04-25T18:00:00Z" {
		t.Errorf("BeforeTS=%q want 2026-04-25T18:00:00Z", got)
	}
	if store.LastOpts.Limit != 25 {
		t.Errorf("limit=%d want 25", store.LastOpts.Limit)
	}
}

// =====================================================================
// Response shape
// =====================================================================

func TestChatHistoryHandler_EmptyResultIsArrayNotNull(t *testing.T) {
	// nil messages slice from the store must serialize as `[]`,
	// not `null` — canvas's JSON parser has one path.
	store := &fakeStore{ReturnMessages: nil, ReturnReachedEnd: true}
	r := newRouter(store)
	w := doChatHistoryRequest(t, r, "/workspaces/"+testWorkspaceID+"/chat-history")

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp ChatHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	// json.Unmarshal of `null` into a []slice yields a nil — assert
	// the JSON literally contains "[]" so a future change that
	// forgets the nil-coercion would fail loudly.
	if !strings.Contains(w.Body.String(), `"messages":[]`) {
		t.Errorf("body should contain `\"messages\":[]`; got %s", w.Body.String())
	}
	if !resp.ReachedEnd {
		t.Errorf("reached_end not propagated")
	}
}

func TestChatHistoryHandler_NonEmptyResponsePreservesShape(t *testing.T) {
	size := int64(4096)
	store := &fakeStore{
		ReturnMessages: []messagestore.ChatMessage{
			{
				ID:        "msg-1",
				Role:      "user",
				Content:   "hi",
				Timestamp: "2026-04-25T18:00:00Z",
			},
			{
				ID:      "msg-2",
				Role:    "agent",
				Content: "hello back",
				Attachments: []messagestore.ChatAttachment{
					{Name: "img.png", URI: "workspace:/img.png", MimeType: "image/png", Size: &size},
				},
				Timestamp: "2026-04-25T18:00:01Z",
			},
		},
		ReturnReachedEnd: false,
	}
	r := newRouter(store)
	w := doChatHistoryRequest(t, r, "/workspaces/"+testWorkspaceID+"/chat-history")

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp ChatHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("messages=%d want 2", len(resp.Messages))
	}
	if resp.Messages[1].Attachments[0].Size == nil || *resp.Messages[1].Attachments[0].Size != 4096 {
		t.Errorf("size pointer flattened in JSON round-trip")
	}
}

// =====================================================================
// Error mapping — store errors become 502, not 500/panic
// =====================================================================

func TestChatHistoryHandler_StoreErrorReturns502(t *testing.T) {
	store := &fakeStore{ReturnErr: errors.New("simulated DB unreachable")}
	r := newRouter(store)
	w := doChatHistoryRequest(t, r, "/workspaces/"+testWorkspaceID+"/chat-history")

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 on store error, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unavailable") {
		t.Errorf("response body should communicate unavailability; got %q", w.Body.String())
	}
}

// =====================================================================
// Interface conformance — the platform-default Postgres impl is the
// only impl in tree today, but the assertion catches future drift if
// the interface evolves and the impl falls behind.
// =====================================================================

func TestMessageStoreInterface_PostgresImplSatisfies(t *testing.T) {
	// Compile-time assertion lives in messagestore/postgres_store.go
	// (`var _ MessageStore = (*PostgresMessageStore)(nil)`). This
	// runtime test exists only to keep the conformance visible in
	// the handler test file — a reader of chat_history_test.go
	// shouldn't have to traverse to the messagestore package to see
	// what the handler is paired with.
	var s messagestore.MessageStore = messagestore.NewPostgresMessageStore(nil)
	_ = s
}
