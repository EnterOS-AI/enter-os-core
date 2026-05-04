package handlers

// mcp_tools_memory_v2.go — v2 memory MCP tools wired through the
// memory plugin (RFC #2728). Adds six new tools alongside the legacy
// commit_memory / recall_memory implementations:
//
//   commit_memory_v2 / search_memory / commit_summary
//   list_writable_namespaces / list_readable_namespaces / forget_memory
//
// PR-6 will alias the legacy names to these implementations; PR-9
// drops the legacy entries. Until then both stacks coexist so existing
// agents keep working without breakage.
//
// Server-side enforcement layers in this file (workspace-server is the
// security perimeter for the plugin):
//   - SAFE-T1201 redaction runs BEFORE every plugin write
//   - Namespace ACL re-derived from the live tree on every write +
//     read; client-supplied namespaces are always intersected
//   - org:* writes are audited to activity_logs (SHA256, not plaintext)
//   - org:* memories are delimiter-wrapped on read output (prompt-
//     injection mitigation; matches memories.go:455-461 today)

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/client"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/namespace"
)

// memoryV2Deps bundles the dependencies the v2 tools need. Lifted
// onto MCPHandler via WithMemoryV2; tests inject their own.
type memoryV2Deps struct {
	plugin   memoryPluginAPI
	resolver namespaceResolverAPI
}

// memoryPluginAPI is the slice of the HTTP plugin client we actually
// call. Defining an interface here lets handler tests stub the plugin
// without spinning up an HTTP server.
type memoryPluginAPI interface {
	CommitMemory(ctx context.Context, namespace string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error)
	Search(ctx context.Context, body contract.SearchRequest) (*contract.SearchResponse, error)
	ForgetMemory(ctx context.Context, id string, body contract.ForgetRequest) error
}

// namespaceResolverAPI mirrors the methods on
// internal/memory/namespace.Resolver that the handlers call.
type namespaceResolverAPI interface {
	ReadableNamespaces(ctx context.Context, workspaceID string) ([]namespace.Namespace, error)
	WritableNamespaces(ctx context.Context, workspaceID string) ([]namespace.Namespace, error)
	CanWrite(ctx context.Context, workspaceID, ns string) (bool, error)
	IntersectReadable(ctx context.Context, workspaceID string, requested []string) ([]string, error)
}

// WithMemoryV2 attaches the v2 dependencies. Returns the receiver for
// fluent wiring. Boot-time: workspace-server's main.go calls this
// after Boot()-ing the plugin client.
func (h *MCPHandler) WithMemoryV2(plugin *client.Client, resolver *namespace.Resolver) *MCPHandler {
	h.memv2 = &memoryV2Deps{plugin: plugin, resolver: resolver}
	return h
}

// withMemoryV2APIs is the test-only wiring path; takes the interfaces
// directly so unit tests don't have to construct a real *client.Client.
func (h *MCPHandler) withMemoryV2APIs(plugin memoryPluginAPI, resolver namespaceResolverAPI) *MCPHandler {
	h.memv2 = &memoryV2Deps{plugin: plugin, resolver: resolver}
	return h
}

// memoryV2Available reports whether the v2 deps are wired. Tools
// return a clear error when the plugin is not configured rather than
// crashing on a nil dereference — keeps a partial deployment from
// taking down chat for everyone.
func (h *MCPHandler) memoryV2Available() error {
	if h == nil || h.memv2 == nil || h.memv2.plugin == nil || h.memv2.resolver == nil {
		return fmt.Errorf("memory plugin is not configured (set MEMORY_PLUGIN_URL)")
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// commit_memory_v2
// ─────────────────────────────────────────────────────────────────────────────

func (h *MCPHandler) toolCommitMemoryV2(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	if err := h.memoryV2Available(); err != nil {
		return "", err
	}
	content, _ := args["content"].(string)
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("content is required")
	}
	ns, _ := args["namespace"].(string)
	if ns == "" {
		ns = "workspace:" + workspaceID
	}
	kindStr := pickStr(args, "kind", string(contract.MemoryKindFact))
	kind := contract.MemoryKind(kindStr)

	// Server-side ACL: ALWAYS revalidate, never trust the client. A
	// canvas re-parent between list_writable_namespaces and this call
	// would otherwise let a stale namespace string slip through.
	ok, err := h.memv2.resolver.CanWrite(ctx, workspaceID, ns)
	if err != nil {
		return "", fmt.Errorf("acl check: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("workspace %s cannot write to namespace %s", workspaceID, ns)
	}

	// SAFE-T1201: scrub credential-shaped strings BEFORE the plugin sees
	// them. Non-negotiable; see memories.go:180.
	content, _ = redactSecrets(workspaceID, content)

	body := contract.MemoryWrite{
		Content: content,
		Kind:    kind,
		Source:  contract.MemorySourceAgent,
	}
	if exp, ok := args["expires_at"].(string); ok && exp != "" {
		t, err := time.Parse(time.RFC3339, exp)
		if err != nil {
			return "", fmt.Errorf("invalid expires_at: must be RFC3339 (got %q): %w", exp, err)
		}
		body.ExpiresAt = &t
	}
	if pin, ok := args["pin"].(bool); ok {
		body.Pin = pin
	}

	resp, err := h.memv2.plugin.CommitMemory(ctx, ns, body)
	if err != nil {
		return "", fmt.Errorf("plugin commit: %w", err)
	}

	// Audit org:* writes — SHA256, not plaintext. Matches the GLOBAL
	// audit shape from memories.go:201-221 so the activity_logs schema
	// stays uniform across legacy + v2.
	if strings.HasPrefix(ns, "org:") {
		if err := h.auditOrgWrite(ctx, workspaceID, ns, content, resp.ID); err != nil {
			// Audit failure does NOT block the write; we just log.
			// Failing closed here would deny any org-scope write any
			// time activity_logs is unhappy.
			log.Printf("v2 org-write audit failed (workspace=%s ns=%s): %v", workspaceID, ns, err)
		}
	}

	out, _ := json.Marshal(resp)
	return string(out), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// search_memory
// ─────────────────────────────────────────────────────────────────────────────

func (h *MCPHandler) toolSearchMemory(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	if err := h.memoryV2Available(); err != nil {
		return "", err
	}
	query, _ := args["query"].(string)
	requested := pickStringSlice(args, "namespaces")

	allowed, err := h.memv2.resolver.IntersectReadable(ctx, workspaceID, requested)
	if err != nil {
		return "", fmt.Errorf("namespace intersect: %w", err)
	}
	if len(allowed) == 0 {
		// Caller is gone or has no readable namespaces — return empty
		// rather than 404. Matches the "memory is non-critical" stance.
		return `{"memories":[]}`, nil
	}

	body := contract.SearchRequest{
		Namespaces: allowed,
		Query:      query,
	}
	if kinds := pickStringSlice(args, "kinds"); len(kinds) > 0 {
		body.Kinds = make([]contract.MemoryKind, 0, len(kinds))
		for _, k := range kinds {
			body.Kinds = append(body.Kinds, contract.MemoryKind(k))
		}
	}
	if l, ok := args["limit"].(float64); ok {
		body.Limit = int(l)
	}

	resp, err := h.memv2.plugin.Search(ctx, body)
	if err != nil {
		return "", fmt.Errorf("plugin search: %w", err)
	}

	// Apply org-namespace delimiter wrap on output. memories.go:455-461
	// wraps GLOBAL memories with `[MEMORY id=X scope=GLOBAL from=Y]:`
	// to defang prompt injection from cross-workspace content. We
	// preserve that here for org:* memories.
	for i, m := range resp.Memories {
		if strings.HasPrefix(m.Namespace, "org:") {
			resp.Memories[i].Content = wrapOrgDelimiter(m)
		}
	}

	out, _ := json.Marshal(resp)
	return string(out), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// commit_summary
// ─────────────────────────────────────────────────────────────────────────────

const defaultSummaryTTL = 30 * 24 * time.Hour

func (h *MCPHandler) toolCommitSummary(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	if err := h.memoryV2Available(); err != nil {
		return "", err
	}
	content, _ := args["content"].(string)
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("content is required")
	}
	ns, _ := args["namespace"].(string)
	if ns == "" {
		ns = "workspace:" + workspaceID
	}

	ok, err := h.memv2.resolver.CanWrite(ctx, workspaceID, ns)
	if err != nil {
		return "", fmt.Errorf("acl check: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("workspace %s cannot write to namespace %s", workspaceID, ns)
	}

	content, _ = redactSecrets(workspaceID, content)

	exp := time.Now().Add(defaultSummaryTTL)
	if expStr, ok := args["expires_at"].(string); ok && expStr != "" {
		if t, err := time.Parse(time.RFC3339, expStr); err == nil {
			exp = t
		}
	}

	body := contract.MemoryWrite{
		Content:   content,
		Kind:      contract.MemoryKindSummary,
		Source:    contract.MemorySourceAgent,
		ExpiresAt: &exp,
	}
	resp, err := h.memv2.plugin.CommitMemory(ctx, ns, body)
	if err != nil {
		return "", fmt.Errorf("plugin commit: %w", err)
	}
	out, _ := json.Marshal(resp)
	return string(out), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// list_writable_namespaces / list_readable_namespaces
// ─────────────────────────────────────────────────────────────────────────────

func (h *MCPHandler) toolListWritableNamespaces(ctx context.Context, workspaceID string, _ map[string]interface{}) (string, error) {
	if err := h.memoryV2Available(); err != nil {
		return "", err
	}
	ns, err := h.memv2.resolver.WritableNamespaces(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("resolve writable: %w", err)
	}
	b, _ := json.MarshalIndent(ns, "", "  ")
	return string(b), nil
}

func (h *MCPHandler) toolListReadableNamespaces(ctx context.Context, workspaceID string, _ map[string]interface{}) (string, error) {
	if err := h.memoryV2Available(); err != nil {
		return "", err
	}
	ns, err := h.memv2.resolver.ReadableNamespaces(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("resolve readable: %w", err)
	}
	b, _ := json.MarshalIndent(ns, "", "  ")
	return string(b), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// forget_memory
// ─────────────────────────────────────────────────────────────────────────────

func (h *MCPHandler) toolForgetMemory(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	if err := h.memoryV2Available(); err != nil {
		return "", err
	}
	memID, _ := args["memory_id"].(string)
	if memID == "" {
		return "", fmt.Errorf("memory_id is required")
	}
	ns, _ := args["namespace"].(string)
	if ns == "" {
		ns = "workspace:" + workspaceID
	}

	ok, err := h.memv2.resolver.CanWrite(ctx, workspaceID, ns)
	if err != nil {
		return "", fmt.Errorf("acl check: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("workspace %s cannot forget memory in namespace %s", workspaceID, ns)
	}

	if err := h.memv2.plugin.ForgetMemory(ctx, memID, contract.ForgetRequest{
		RequestedByNamespace: ns,
	}); err != nil {
		return "", fmt.Errorf("plugin forget: %w", err)
	}
	return `{"forgotten":true}`, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// auditOrgWrite mirrors the audit-log shape memories.go uses for
// GLOBAL writes (SHA256 of content, not plaintext) so legacy + v2
// rows are queryable with a single activity_logs schema.
func (h *MCPHandler) auditOrgWrite(ctx context.Context, workspaceID, ns, content, memID string) error {
	hash := sha256.Sum256([]byte(content))
	hashHex := hex.EncodeToString(hash[:])
	// json.Marshal, not Sprintf-%q. %q produces Go-quoted strings,
	// which are NOT valid JSON for non-ASCII inputs (Go's escapes
	// like \xNN aren't part of the JSON spec). Today's values are
	// pure-ASCII so the bug was latent; if metadata grows to include
	// arbitrary content snippets it would silently produce invalid
	// JSON in activity_logs.
	metadata, err := json.Marshal(map[string]string{
		"memory_id": memID,
		"sha256":    hashHex,
	})
	if err != nil {
		return fmt.Errorf("audit metadata marshal: %w", err)
	}
	_, err = h.database.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, action, target, metadata, created_at)
		VALUES ($1, 'memory.org_write', $2, $3, now())
	`, workspaceID, ns, string(metadata))
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	return nil
}

// wrapOrgDelimiter prepends the prompt-injection mitigation prefix to
// org-namespace memories. Keeps cross-workspace content from being
// misinterpreted by an LLM as instructions, matching memories.go:455-461.
func wrapOrgDelimiter(m contract.Memory) string {
	return fmt.Sprintf("[MEMORY id=%s scope=ORG ns=%s]: %s", m.ID, m.Namespace, m.Content)
}

// pickStr extracts a string arg with a default fallback.
func pickStr(args map[string]interface{}, key, dflt string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return dflt
}

// pickStringSlice extracts a []string from args[key] tolerantly:
// JSON arrays of strings come through as []interface{} after JSON
// decoding, so we convert.
func pickStringSlice(args map[string]interface{}, key string) []string {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	switch arr := v.(type) {
	case []string:
		return arr
	case []interface{}:
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
