package handlers

// chat_history.go — HTTP-shape adapter over messagestore.MessageStore
// (RFC #2945 PR-D).
//
// Pre-PR-D, this file owned the activity_logs query AND the parser
// AND the HTTP plumbing. PR-D extracts the storage + parser into
// internal/messagestore/ so OSS operators can plug in alternative
// backends (S3-tiered, vector store, in-memory). The handler is now
// a thin adapter: parse query params → call store → emit JSON.
//
// Endpoint: GET /workspaces/:id/chat-history?limit=N&before_ts=T
// Auth: same wsAuth chain as /workspaces/:id/activity (tenant
// ADMIN_TOKEN + X-Molecule-Org-Id header). No new trust boundary.
//
// Behavioral parity with canvas TS is enforced at the messagestore
// layer (internal/messagestore/postgres_store_test.go); this file's
// tests cover the HTTP-shape concerns only.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/messagestore"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ChatHistoryResponse is the wire shape for GET /chat-history.
type ChatHistoryResponse struct {
	Messages   []messagestore.ChatMessage `json:"messages"`
	ReachedEnd bool                       `json:"reached_end"`
}

// ChatHistoryHandler exposes the typed chat-history endpoint over a
// MessageStore. The store is injected so OSS operators can swap the
// backend without forking the handler.
type ChatHistoryHandler struct {
	store messagestore.MessageStore
}

// NewChatHistoryHandler wires a MessageStore (typically
// messagestore.NewPostgresMessageStore at production startup).
//
// Tests inject fakes (see internal/handlers/chat_history_test.go).
// Constructor takes the interface, not a concrete type, so the
// platform-default vs OSS-alternative decision happens at wiring
// time in router.go.
func NewChatHistoryHandler(store messagestore.MessageStore) *ChatHistoryHandler {
	return &ChatHistoryHandler{store: store}
}

// List handles GET /workspaces/:id/chat-history?limit=N&before_ts=T.
//
// Query parameters mirror /activity for caller convenience:
//
//   - limit (default 100, max 1000) — page size
//   - before_ts (RFC3339, optional) — cursor for paginating backward
//
// Validates inputs at the trust boundary; the store sees only
// well-formed ListOptions.
func (h *ChatHistoryHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	if _, err := uuid.Parse(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id must be a UUID"})
		return
	}

	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}

	opts := messagestore.ListOptions{Limit: limit}
	if v := c.Query("before_ts"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "before_ts must be an RFC3339 timestamp (e.g. 2026-05-01T00:00:00Z)",
			})
			return
		}
		opts.BeforeTS = t
		opts.HasBefore = true
	}

	messages, reachedEnd, err := h.store.List(c.Request.Context(), workspaceID, opts)
	if err != nil {
		// Errors here are infra (DB unreachable, store impl failure).
		// Surface as 502 so the canvas can retry vs. treating as
		// "no rows."
		c.JSON(http.StatusBadGateway, gin.H{"error": "chat history unavailable"})
		return
	}

	// Defensive: if the store returns nil messages slice (any impl
	// might), emit empty array rather than `null` so canvas's JSON
	// parser doesn't have to handle two empty representations.
	if messages == nil {
		messages = []messagestore.ChatMessage{}
	}

	c.JSON(http.StatusOK, ChatHistoryResponse{
		Messages:   messages,
		ReachedEnd: reachedEnd,
	})
}
