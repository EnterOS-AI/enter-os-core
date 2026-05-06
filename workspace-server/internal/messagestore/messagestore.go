// Package messagestore defines the read-side interface and canonical
// data shapes for chat-history retrieval.
//
// Origin: RFC #2945 PR-D (issue #3026). PR-A extracted the WRITE path
// (AgentMessageWriter), PR-B/B-1 typed the WS event taxonomy, PR-C
// centralized read-side parsing in the server. PR-D abstracts the
// underlying storage layer so OSS operators can plug in alternative
// backends without forking the handler.
//
// # Why this package exists
//
// Today's only consumer is ChatHistoryHandler, but exposing storage as
// an interface is what makes the platform's chat-history layer pluggable
// for OSS operators. Operators wanting to:
//
//   - Tier hot/warm/cold storage (recent in Postgres, archival in S3 parquet)
//   - Use a vector store with hybrid search (Pinecone, Weaviate)
//   - Run an in-memory store for ephemeral tests / sandbox tenants
//   - Federate history across regions
//
// …implement MessageStore against their backend. The platform-default
// PostgresMessageStore wraps today's activity_logs query + parser
// behavior unchanged.
//
// # Implementation contract
//
// Implementations MUST:
//
//   - Return messages newest-first, up to opts.Limit. Caller (the
//     handler) is responsible for opts.Limit clamping.
//   - Honor opts.BeforeTS as a strict less-than cursor when
//     opts.HasBefore is true; ignore it when false. Use HasBefore (not
//     a zero-time check) so a legitimate "start of epoch" cursor is
//     distinguishable from "no cursor."
//   - Set reachedEnd=true when the underlying store has no more
//     messages older than the returned page. Caller uses this to
//     disable further older-batch fetches in the lazy-load UX.
//   - Parse agent-emitted JSON DEFENSIVELY. Any malformed message body
//     becomes an empty ChatMessage (or is dropped); never panic, never
//     return an error for parse failures alone — chat falls through to
//     text-only rather than 500.
//   - NEVER log full message bodies, attachment URIs, or anything that
//     would be a sensitive screenshot. Workspace ID + activity-log
//     row id at DEBUG is the ceiling.
//   - Honor ctx cancellation. A canceled ctx must abort the lookup
//     and return ctx.Err().
//
// Implementations MAY:
//
//   - Cache aggressively (history is read-only).
//   - Filter out additional rows beyond what the interface requires
//     (e.g., role-based redaction in regulated environments) as long
//     as reachedEnd is set conservatively (false if uncertain).
//
// # Threading
//
// Implementations MUST be safe for concurrent calls. The handler
// dispatches a goroutine per request; a non-thread-safe impl would
// race on every chat reload.
package messagestore

import (
	"context"
	"time"
)

// ChatMessage is the canonical shape returned to chat-history clients.
// Mirrors canvas's ChatMessage TS type so the canvas can render
// without per-row mapping.
//
// ID is server-minted per ChatMessage. Activity-log rows don't carry
// message-shaped ids; canvas dedupes by (role, content, timestamp
// window) not by id, so id stability across requests is not required.
type ChatMessage struct {
	ID          string           `json:"id"`
	Role        string           `json:"role"` // "user" | "agent" | "system"
	Content     string           `json:"content"`
	Attachments []ChatAttachment `json:"attachments,omitempty"`
	Timestamp   string           `json:"timestamp"` // RFC3339, pinned to row.created_at
}

// ChatAttachment mirrors canvas ChatAttachment / ParsedFilePart.
// Size is *int64 (not int64) so JSON omits the field when unknown,
// rather than emitting `"size": 0` which the renderer would interpret
// as "zero-byte file."
type ChatAttachment struct {
	Name     string `json:"name"`
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Size     *int64 `json:"size,omitempty"`
}

// ListOptions is the page-window the handler hands to the store.
// Constructed by the handler from query parameters; the store should
// not inspect the request directly.
type ListOptions struct {
	// Limit is the page size. Caller (the handler) clamps to a sane
	// bound (default 100, max 1000); store treats Limit ≤ 0 as a
	// programming error.
	Limit int

	// BeforeTS is the cursor for paginating backward. The store MUST
	// only consider this when HasBefore is true; using a zero-time
	// fallback would silently exclude the legitimate epoch-start case.
	BeforeTS  time.Time
	HasBefore bool
}

// MessageStore is the read-side interface. Implementations pluggable
// via constructor injection at handler creation time.
//
// Why "List" and not "GetMessages" / "ReadHistory" / etc: List matches
// the verb on /workspaces/:id/chat-history (HTTP GET on a collection)
// and the existing handler method. One-name-one-thing keeps the
// interface and the route lined up.
type MessageStore interface {
	List(ctx context.Context, workspaceID string, opts ListOptions) (messages []ChatMessage, reachedEnd bool, err error)
}
