package handlers

// mcp_tools_memory_legacy_shim.go — translates legacy commit_memory /
// recall_memory calls (scope-based) into the v2 plugin path
// (namespace-based) when the v2 plugin is wired.
//
// Behavior:
//   - If h.memv2 is wired (MEMORY_PLUGIN_URL set + plugin reachable),
//     legacy tools translate scope→namespace and delegate to v2.
//   - If h.memv2 is NOT wired, legacy tools fall through to the
//     original DB-backed path in mcp_tools.go (zero behavior change
//     for operators who haven't enabled the plugin yet).
//
// Translation:
//   commit:  LOCAL  → workspace:<self>
//            TEAM   → team:<root>     (resolved server-side)
//            GLOBAL → still blocked at the MCP bridge (C3)
//   recall:  LOCAL  → search restricted to workspace:<self>
//            TEAM   → search restricted to team:<root> + workspace:<self>
//            empty  → search all readable namespaces (default)
//
// PR-9 (~60 days post-cutover) drops this file when the legacy tool
// names are removed entirely.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
)

// scopeToWritableNamespace maps a legacy scope value to the namespace
// the resolver should be queried for. Returns "" + error if the scope
// isn't translatable (GLOBAL is the canonical case).
//
// The resolver picks the actual namespace string at runtime — we only
// need the kind here.
func (h *MCPHandler) scopeToWritableNamespace(ctx context.Context, workspaceID, scope string) (string, error) {
	if scope == "GLOBAL" {
		return "", fmt.Errorf("GLOBAL scope is not permitted via the MCP bridge — use LOCAL or TEAM")
	}
	writable, err := h.memv2.resolver.WritableNamespaces(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("resolve writable: %w", err)
	}
	wantKind := contract.NamespaceKindWorkspace
	switch scope {
	case "", "LOCAL":
		wantKind = contract.NamespaceKindWorkspace
	case "TEAM":
		wantKind = contract.NamespaceKindTeam
	}
	for _, ns := range writable {
		if ns.Kind == wantKind {
			return ns.Name, nil
		}
	}
	return "", fmt.Errorf("no writable namespace of kind %s available for workspace %s", wantKind, workspaceID)
}

// scopeToReadableNamespaces returns the namespace list to search when
// the caller passed a legacy scope. Empty scope → all readable.
func (h *MCPHandler) scopeToReadableNamespaces(ctx context.Context, workspaceID, scope string) ([]string, error) {
	if scope == "GLOBAL" {
		return nil, fmt.Errorf("GLOBAL scope is not permitted via the MCP bridge — use LOCAL, TEAM, or empty")
	}
	readable, err := h.memv2.resolver.ReadableNamespaces(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("resolve readable: %w", err)
	}
	switch scope {
	case "":
		out := make([]string, len(readable))
		for i, ns := range readable {
			out[i] = ns.Name
		}
		return out, nil
	case "LOCAL":
		for _, ns := range readable {
			if ns.Kind == contract.NamespaceKindWorkspace {
				return []string{ns.Name}, nil
			}
		}
	case "TEAM":
		out := []string{}
		for _, ns := range readable {
			if ns.Kind == contract.NamespaceKindWorkspace || ns.Kind == contract.NamespaceKindTeam {
				out = append(out, ns.Name)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	default:
		return nil, fmt.Errorf("unknown scope: %s", scope)
	}
	return nil, fmt.Errorf("no readable namespace of scope %s for workspace %s", scope, workspaceID)
}

// commitMemoryLegacyShim is the v2-routed implementation invoked by
// the legacy commit_memory tool when the v2 plugin is wired. Returns
// JSON in the SAME shape the legacy tool always returned
// ({"id":"...","scope":"..."}) so existing agents see no diff.
func (h *MCPHandler) commitMemoryLegacyShim(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	content, _ := args["content"].(string)
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("content is required")
	}
	scope, _ := args["scope"].(string)
	if scope == "" {
		scope = "LOCAL"
	}
	if scope != "LOCAL" && scope != "TEAM" && scope != "GLOBAL" {
		return "", fmt.Errorf("scope must be LOCAL or TEAM")
	}

	ns, err := h.scopeToWritableNamespace(ctx, workspaceID, scope)
	if err != nil {
		return "", err
	}

	// Delegate to the v2 tool. Reuses its redaction + audit + ACL
	// re-validation paths uniformly so legacy callers can't bypass
	// the security perimeter.
	v2args := map[string]interface{}{
		"content":   content,
		"namespace": ns,
		// kind defaults to "fact"; preserve legacy implicit shape
	}
	v2resp, err := h.toolCommitMemoryV2(ctx, workspaceID, v2args)
	if err != nil {
		return "", err
	}

	// Reshape v2 response ({"id":"...","namespace":"..."}) into the
	// legacy shape ({"id":"...","scope":"..."}). Don't change the
	// agent-visible contract just because the storage layer moved.
	var parsed contract.MemoryWriteResponse
	if jerr := json.Unmarshal([]byte(v2resp), &parsed); jerr != nil {
		// Bug if it parses; the v2 tool always returns valid JSON.
		return "", fmt.Errorf("v2 response parse: %w", jerr)
	}
	return fmt.Sprintf(`{"id":%q,"scope":%q}`, parsed.ID, scope), nil
}

// recallMemoryLegacyShim mirrors commitMemoryLegacyShim for reads.
// Returns JSON in the legacy "memory entries" shape:
//   [{"id":"...","content":"...","scope":"...","created_at":"..."}, ...]
func (h *MCPHandler) recallMemoryLegacyShim(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	query, _ := args["query"].(string)
	scope, _ := args["scope"].(string)

	namespaces, err := h.scopeToReadableNamespaces(ctx, workspaceID, scope)
	if err != nil {
		return "", err
	}

	resp, err := h.memv2.plugin.Search(ctx, contract.SearchRequest{
		Namespaces: namespaces,
		Query:      query,
		Limit:      50,
	})
	if err != nil {
		return "", fmt.Errorf("plugin search: %w", err)
	}

	// Apply the same org-namespace delimiter wrap the v2 search uses.
	for i, m := range resp.Memories {
		if strings.HasPrefix(m.Namespace, "org:") {
			resp.Memories[i].Content = wrapOrgDelimiter(m)
		}
	}

	type legacyEntry struct {
		ID        string `json:"id"`
		Content   string `json:"content"`
		Scope     string `json:"scope"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]legacyEntry, 0, len(resp.Memories))
	for _, m := range resp.Memories {
		out = append(out, legacyEntry{
			ID:        m.ID,
			Content:   m.Content,
			Scope:     namespaceKindToLegacyScope(m.Namespace),
			CreatedAt: m.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}
	if len(out) == 0 {
		return "No memories found.", nil
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// namespaceKindToLegacyScope maps a v2 namespace string back to its
// legacy scope label so legacy agents see "LOCAL"/"TEAM"/"GLOBAL" in
// recall responses, not the namespace string. This reverses the
// scopeToWritableNamespace mapping.
func namespaceKindToLegacyScope(ns string) string {
	switch {
	case strings.HasPrefix(ns, "workspace:"):
		return "LOCAL"
	case strings.HasPrefix(ns, "team:"):
		return "TEAM"
	case strings.HasPrefix(ns, "org:"):
		return "GLOBAL"
	default:
		return ""
	}
}
