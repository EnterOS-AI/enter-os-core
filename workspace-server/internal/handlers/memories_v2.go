package handlers

// memories_v2.go — HTTP endpoints that expose Memory v2 plugin state to
// the canvas Memory tab. Reads-only; writes still go through the MCP
// path (see mcp_tools_memory_v2.go) where SAFE-T1201 redaction +
// org-write audit happen at a single funnel.
//
// Why a separate v2 endpoint set rather than retrofitting memories.go:
//
//   - memories.go reads `agent_memories` (legacy v1 table). After the
//     2026-05-05 cutover, agent commits go to the plugin's
//     memory_records — agent_memories is frozen. The canvas Memory
//     tab reading memories.go shows STALE data.
//   - The plugin is loopback-only on each tenant (127.0.0.1:9100), so
//     the canvas (browser) cannot call it directly. workspace-server
//     proxies through these endpoints.
//   - v2 has different shape (namespace tree, kind/source/pin/TTL,
//     score) — overloading memories.go would break v1 consumers
//     (admin export, the back-compat MCP shim).
//
// All endpoints sit under the same wsAuth group memories.go uses,
// so the existing per-tenant token gates them automatically.

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/client"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/namespace"
	"github.com/gin-gonic/gin"
)

// MemoriesV2Handler bundles the plugin client + namespace resolver
// behind a slim HTTP surface. Construction matches the rest of the
// handlers package: NewMemoriesV2Handler followed by WithMemoryV2 (or
// the test-only withMemoryV2APIs) at boot.
type MemoriesV2Handler struct {
	plugin   memoryPluginAPI
	resolver namespaceResolverAPI
}

// NewMemoriesV2Handler constructs an unwired handler. Every method
// returns 503 until WithMemoryV2 is called — keeps a partial deploy
// (MEMORY_PLUGIN_URL absent) from crashing the canvas with 500s.
func NewMemoriesV2Handler() *MemoriesV2Handler {
	return &MemoriesV2Handler{}
}

// WithMemoryV2 attaches the live plugin client + resolver. Returns
// the receiver for fluent boot-time wiring, mirroring MCPHandler.
func (h *MemoriesV2Handler) WithMemoryV2(plugin *client.Client, resolver *namespace.Resolver) *MemoriesV2Handler {
	h.plugin = plugin
	h.resolver = resolver
	return h
}

// withMemoryV2APIs is the test-only injection path: takes the
// interfaces directly so unit tests don't have to construct a real
// *client.Client / namespace.Resolver. Keep symmetric with
// MCPHandler.withMemoryV2APIs so handler tests can re-use the same
// stubs.
func (h *MemoriesV2Handler) withMemoryV2APIs(plugin memoryPluginAPI, resolver namespaceResolverAPI) *MemoriesV2Handler {
	h.plugin = plugin
	h.resolver = resolver
	return h
}

// available reports whether the v2 deps are wired. Each route checks
// this and returns 503 + a clear hint when the plugin isn't
// configured, matching the MCP-side error.
func (h *MemoriesV2Handler) available() error {
	if h == nil || h.plugin == nil || h.resolver == nil {
		return errors.New("memory plugin is not configured (set MEMORY_PLUGIN_URL)")
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /workspaces/:id/v2/namespaces
//
// Returns the namespace tree the canvas uses to drive the Memory tab's
// namespace dropdown. Two arrays:
//
//   - readable[]: every namespace this workspace can READ from. Drives
//     the "show me memories from X" filter dropdown.
//   - writable[]: subset of readable that this workspace can WRITE to.
//     Used for future canvas-side commit (not in this PR but the
//     contract is symmetric so the dropdown can disable read-only
//     entries when wiring up commit).
//
// Each entry carries name + kind + a friendly label so the canvas
// doesn't have to parse `workspace:abc-123` itself. Kind ranks the
// dropdown grouping (workspace → team → org → custom).
// ─────────────────────────────────────────────────────────────────────────────

// NamespaceView is the UI-friendly DTO returned by GET v2/namespaces.
// Internal namespace.Namespace has fields the canvas doesn't need
// (resolver-internal flags, raw metadata blobs); this strips it down.
type NamespaceView struct {
	Name string                 `json:"name"`
	Kind contract.NamespaceKind `json:"kind"`
	// Label is a stable display string the canvas can render directly.
	// For workspace:<id> it's "Workspace (<short-id>)"; for team:<id>
	// it's "Team (<short-id>)"; org/custom carry the raw suffix.
	Label string `json:"label"`
}

// NamespacesResponse is the body of GET v2/namespaces.
type NamespacesResponse struct {
	Readable []NamespaceView `json:"readable"`
	Writable []NamespaceView `json:"writable"`
}

// Namespaces handles GET /workspaces/:id/v2/namespaces.
func (h *MemoriesV2Handler) Namespaces(c *gin.Context) {
	if err := h.available(); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	readable, err := h.resolver.ReadableNamespaces(ctx, workspaceID)
	if err != nil {
		log.Printf("v2/namespaces readable error workspace=%s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve readable namespaces"})
		return
	}
	writable, err := h.resolver.WritableNamespaces(ctx, workspaceID)
	if err != nil {
		log.Printf("v2/namespaces writable error workspace=%s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve writable namespaces"})
		return
	}

	c.JSON(http.StatusOK, NamespacesResponse{
		Readable: namespacesToViews(readable),
		Writable: namespacesToViews(writable),
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /workspaces/:id/v2/memories
//
// Search the plugin for memories visible to this workspace.
//
// Query params (all optional):
//   - namespace: a single readable namespace to scope to. Omitted ⇒ all
//     readable namespaces (dropdown's "All" mode).
//   - q: full-text query string. Empty ⇒ recency-ordered listing.
//   - kind: one of fact|summary|checkpoint. Empty ⇒ all kinds.
//   - limit: max rows. Defaults to 50, clamped to 100. Matches the
//     v1 endpoint's clamp shape (memories.go:memoryRecallMaxLimit).
//
// Server-side ACL invariant: the request is ALWAYS intersected with
// the resolver's readable set on the server. A canvas-supplied
// `namespace=foo:bar` that this workspace can't read returns an empty
// list, NOT 403 — the canvas dropdown is built from /v2/namespaces
// so a forbidden value is a stale-cache bug, not malice. Existence
// non-inference: empty result is indistinguishable from "you can't
// read this namespace" — same as the wsAuth-protected v1 endpoints.
// ─────────────────────────────────────────────────────────────────────────────

const memoriesV2DefaultLimit = 50
const memoriesV2MaxLimit = 100

// Search handles GET /workspaces/:id/v2/memories.
func (h *MemoriesV2Handler) Search(c *gin.Context) {
	if err := h.available(); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	requestedNS := c.Query("namespace")
	query := c.Query("q")
	kindStr := c.Query("kind")
	limit := parseLimit(c.Query("limit"))

	// Resolve the readable set, then intersect the request.
	// IntersectReadable handles both the empty-request case (return
	// all readable) and the explicit-namespace case (return [ns] iff
	// readable, else []).
	var requested []string
	if requestedNS != "" {
		requested = []string{requestedNS}
	}
	scopedNamespaces, err := h.resolver.IntersectReadable(ctx, workspaceID, requested)
	if err != nil {
		log.Printf("v2/memories intersect error workspace=%s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve namespaces"})
		return
	}
	// Empty after intersection — caller asked for a namespace they
	// can't read, OR they have no readable namespaces at all. Return
	// [] (not 404) so the canvas can render its empty-state without
	// special-casing.
	if len(scopedNamespaces) == 0 {
		c.JSON(http.StatusOK, MemoriesResponse{Memories: []MemoryView{}})
		return
	}

	req := contract.SearchRequest{
		Namespaces: scopedNamespaces,
		Query:      query,
		Limit:      limit,
	}
	if kindStr != "" {
		req.Kinds = []contract.MemoryKind{contract.MemoryKind(kindStr)}
	}

	resp, err := h.plugin.Search(ctx, req)
	if err != nil {
		log.Printf("v2/memories plugin error workspace=%s: %v", workspaceID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "memory plugin search failed"})
		return
	}

	out := MemoriesResponse{Memories: make([]MemoryView, 0, len(resp.Memories))}
	for _, m := range resp.Memories {
		out.Memories = append(out.Memories, memoryToView(m))
	}
	c.JSON(http.StatusOK, out)
}

// ─────────────────────────────────────────────────────────────────────────────
// DELETE /workspaces/:id/v2/memories/:memoryId
//
// Forget a memory. The plugin enforces its own ownership model — we
// pass `requested_by_namespace = workspace:<id>` so the audit trail
// records who initiated the forget; the plugin's ACL gate decides
// whether the deletion is allowed.
//
// 404 (not 403) on a missing or non-owned memory: existence-non-
// inferring response, matches the v1 DELETE in memories.go.
// ─────────────────────────────────────────────────────────────────────────────

// Forget handles DELETE /workspaces/:id/v2/memories/:memoryId.
func (h *MemoriesV2Handler) Forget(c *gin.Context) {
	if err := h.available(); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	workspaceID := c.Param("id")
	memoryID := c.Param("memoryId")
	ctx := c.Request.Context()

	if memoryID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "memoryId is required"})
		return
	}

	body := contract.ForgetRequest{
		RequestedByNamespace: "workspace:" + workspaceID,
	}
	if err := h.plugin.ForgetMemory(ctx, memoryID, body); err != nil {
		// Map plugin not_found → 404. Anything else is upstream error.
		var ce *contract.Error
		if errors.As(err, &ce) && ce.Code == contract.ErrorCodeNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "memory not found"})
			return
		}
		log.Printf("v2/memories forget error workspace=%s memory=%s: %v", workspaceID, memoryID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "memory plugin delete failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// ─────────────────────────────────────────────────────────────────────────────
// View shaping helpers
// ─────────────────────────────────────────────────────────────────────────────

// MemoryView is the canvas-facing shape of a v2 memory record. The raw
// contract.Memory carries internal fields we don't expose (raw
// `propagation` blob); MemoryView strips it to what the Memory tab
// renders.
type MemoryView struct {
	ID        string                 `json:"id"`
	Namespace string                 `json:"namespace"`
	Content   string                 `json:"content"`
	Kind      contract.MemoryKind    `json:"kind"`
	Source    contract.MemorySource  `json:"source"`
	Pin       bool                   `json:"pin"`
	ExpiresAt *time.Time             `json:"expires_at,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	// Score is the plugin's similarity score (1.0 = exact); only
	// populated when ?q= is set and the plugin supports embedding.
	Score *float64 `json:"score,omitempty"`
	// SourceWorkspaceID is parsed out of `propagation.source_workspace_id`
	// when present (cross-workspace propagation) — lets the canvas
	// render a "from <peer>" badge so users can tell their own writes
	// apart from team-shared memory.
	SourceWorkspaceID string `json:"source_workspace_id,omitempty"`
}

// MemoriesResponse is the body of GET v2/memories.
type MemoriesResponse struct {
	Memories []MemoryView `json:"memories"`
}

func memoryToView(m contract.Memory) MemoryView {
	v := MemoryView{
		ID:        m.ID,
		Namespace: m.Namespace,
		Content:   m.Content,
		Kind:      m.Kind,
		Source:    m.Source,
		Pin:       m.Pin,
		ExpiresAt: m.ExpiresAt,
		CreatedAt: m.CreatedAt,
		Score:     m.Score,
	}
	if m.Propagation != nil {
		// `source_workspace_id` is a propagation contract field
		// (RFC #2728 §5). Plugin emits it on writes that originated
		// from a different workspace. Best-effort string extraction —
		// don't fail rendering if shape drifts.
		if raw, ok := m.Propagation["source_workspace_id"]; ok {
			if s, ok := raw.(string); ok && s != "" {
				v.SourceWorkspaceID = s
			}
		}
	}
	return v
}

// namespacesToViews converts resolver namespaces into UI-friendly
// views. Prefers `DisplayName` from the resolver (workspace.name from
// the DB) when present; falls back to a UUID-prefix label.
//
// Issue #2988: pre-fix, every namespace used a shortID-truncated UUID
// label. On a root workspace where workspace==team==org IDs collide
// (resolver derive() degenerate case), all three labels rendered
// identically. DisplayName disambiguates by surfacing real workspace
// names — the canvas dropdown now reads "Workspace (mac laptop)" /
// "Team (mac laptop)" / "Org (mac laptop)" for a root workspace
// rather than three identical UUID prefixes. The `kind` prefix
// "Workspace/Team/Org" still carries the semantic distinction.
func namespacesToViews(in []namespace.Namespace) []NamespaceView {
	views := make([]NamespaceView, 0, len(in))
	for _, n := range in {
		views = append(views, NamespaceView{
			Name:  n.Name,
			Kind:  n.Kind,
			Label: namespaceLabelWithName(n.Name, n.Kind, n.DisplayName),
		})
	}
	return views
}

// namespaceLabel renders a human-friendly label for a namespace using
// the UUID-prefix fallback only. Kept for back-compat with callers
// that don't yet plumb a display name. New callers should use
// namespaceLabelWithName which prefers the workspace's display name
// when available.
//
// Format (UUID-prefix fallback):
//   workspace:abc-123 → "Workspace (abc-123)"
//   team:t-1          → "Team (t-1)"
//   org:acme          → "Org (acme)"
//   custom:foo        → "foo"
func namespaceLabel(name string, kind contract.NamespaceKind) string {
	return namespaceLabelWithName(name, kind, "")
}

// namespaceLabelWithName renders the human-friendly label, preferring
// `displayName` when non-empty.
//
// When displayName is set:
//   Workspace, "mac laptop"    → "Workspace (mac laptop)"
//   Team, "Engineering team"   → "Team (Engineering team)"
//   Org, "Hongming's Org"      → "Org (Hongming's Org)"
//
// When displayName is empty (lookup miss, future-migration drop, etc.),
// falls back to the UUID-prefix shape for back-compat.
//
// Custom namespaces ignore displayName because they're operator-defined
// — the operator chose the raw suffix as the label, surfacing a
// different "name" would be a UX surprise.
func namespaceLabelWithName(name string, kind contract.NamespaceKind, displayName string) string {
	suffix := ""
	if i := indexOfColon(name); i >= 0 && i+1 < len(name) {
		suffix = name[i+1:]
	}
	switch kind {
	case contract.NamespaceKindWorkspace:
		if displayName != "" {
			return "Workspace (" + displayName + ")"
		}
		return "Workspace (" + shortID(suffix) + ")"
	case contract.NamespaceKindTeam:
		if displayName != "" {
			return "Team (" + displayName + ")"
		}
		return "Team (" + shortID(suffix) + ")"
	case contract.NamespaceKindOrg:
		if displayName != "" {
			return "Org (" + displayName + ")"
		}
		return "Org (" + suffix + ")"
	case contract.NamespaceKindCustom:
		// Operator-defined; the suffix IS the label they chose.
		// displayName is ignored — surfacing a different name would
		// be a UX surprise for an operator who deliberately named
		// the namespace.
		if suffix == "" {
			return name
		}
		return suffix
	default:
		return name
	}
}

// shortID truncates a UUID-like string to the first 8 chars so the
// dropdown stays readable. Keeps the full id available via the
// `name` field for click-to-copy / debugging.
func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// indexOfColon is strings.IndexByte without the import, kept inline so
// the helper stays trivially auditable next to namespaceLabel.
func indexOfColon(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

// parseLimit validates the ?limit= query value. Defaults +
// clamps mirror memoriesV2DefaultLimit / memoriesV2MaxLimit.
func parseLimit(raw string) int {
	if raw == "" {
		return memoriesV2DefaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return memoriesV2DefaultLimit
	}
	if n > memoriesV2MaxLimit {
		return memoriesV2MaxLimit
	}
	return n
}

