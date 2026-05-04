package handlers

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	mclient "github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/client"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/namespace"
	"github.com/gin-gonic/gin"
)

// envMemoryV2Cutover gates whether admin export/import routes through
// the v2 plugin (PR-8 / RFC #2728). When unset, the legacy direct-DB
// path runs unchanged so operators who haven't enabled the plugin
// keep working.
const envMemoryV2Cutover = "MEMORY_V2_CUTOVER"

// AdminMemoriesHandler provides bulk export/import of agent memories for
// backup and restore across Docker rebuilds (issue #1051).
//
// PR-8 (RFC #2728): when wired with the v2 plugin via WithMemoryV2 AND
// MEMORY_V2_CUTOVER is true, export reads from the plugin's namespaces
// and import writes through the plugin. Both paths preserve the
// SAFE-T1201 redaction shipped in F1084 + F1085.
type AdminMemoriesHandler struct {
	plugin   adminMemoriesPlugin
	resolver adminMemoriesResolver
}

// adminMemoriesPlugin is the slice of the memory plugin client we
// call from this handler.
type adminMemoriesPlugin interface {
	CommitMemory(ctx context.Context, namespace string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error)
	Search(ctx context.Context, body contract.SearchRequest) (*contract.SearchResponse, error)
	UpsertNamespace(ctx context.Context, name string, body contract.NamespaceUpsert) (*contract.Namespace, error)
}

// adminMemoriesResolver mirrors the namespace resolver methods this
// handler calls.
type adminMemoriesResolver interface {
	WritableNamespaces(ctx context.Context, workspaceID string) ([]namespace.Namespace, error)
	ReadableNamespaces(ctx context.Context, workspaceID string) ([]namespace.Namespace, error)
}

// NewAdminMemoriesHandler constructs the handler.
func NewAdminMemoriesHandler() *AdminMemoriesHandler {
	return &AdminMemoriesHandler{}
}

// WithMemoryV2 attaches the v2 plugin + resolver. Production wiring
// path; main.go calls this after Boot()-ing the plugin client.
func (h *AdminMemoriesHandler) WithMemoryV2(plugin *mclient.Client, resolver *namespace.Resolver) *AdminMemoriesHandler {
	h.plugin = plugin
	h.resolver = resolver
	return h
}

// withMemoryV2APIs is the test-only wiring that takes interfaces.
func (h *AdminMemoriesHandler) withMemoryV2APIs(plugin adminMemoriesPlugin, resolver adminMemoriesResolver) *AdminMemoriesHandler {
	h.plugin = plugin
	h.resolver = resolver
	return h
}

// cutoverActive reports whether the export/import path should route
// through the v2 plugin.
func (h *AdminMemoriesHandler) cutoverActive() bool {
	if os.Getenv(envMemoryV2Cutover) != "true" {
		return false
	}
	return h.plugin != nil && h.resolver != nil
}

// memoryExportEntry is the JSON shape for a single exported memory.
type memoryExportEntry struct {
	ID            string    `json:"id"`
	Content       string    `json:"content"`
	Scope         string    `json:"scope"`
	Namespace     string    `json:"namespace"`
	CreatedAt     time.Time `json:"created_at"`
	WorkspaceName string    `json:"workspace_name"`
}

// Export handles GET /admin/memories/export
// Returns all agent memories joined with workspace name so the dump is
// human-readable and can be re-imported after workspaces are re-provisioned
// (UUIDs change, names stay stable).
//
// SECURITY (F1084 / #1131): applies redactSecrets to each content field
// before returning so that any credentials stored before SAFE-T1201 (#838)
// was applied do not leak out via the admin export endpoint.
//
// CUTOVER (PR-8 / RFC #2728): when MEMORY_V2_CUTOVER=true and the v2
// plugin is wired, reads from the plugin instead of agent_memories.
func (h *AdminMemoriesHandler) Export(c *gin.Context) {
	ctx := c.Request.Context()

	if h.cutoverActive() {
		h.exportViaPlugin(c, ctx)
		return
	}

	rows, err := db.DB.QueryContext(ctx, `
		SELECT am.id, am.content, am.scope, am.namespace, am.created_at,
		       w.name AS workspace_name
		FROM agent_memories am
		JOIN workspaces w ON am.workspace_id = w.id
		ORDER BY am.created_at
	`)
	if err != nil {
		log.Printf("admin/memories/export: query error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "export query failed"})
		return
	}
	defer rows.Close()

	memories := make([]memoryExportEntry, 0)
	for rows.Next() {
		var m memoryExportEntry
		if err := rows.Scan(&m.ID, &m.Content, &m.Scope, &m.Namespace, &m.CreatedAt, &m.WorkspaceName); err != nil {
			log.Printf("admin/memories/export: scan error: %v", err)
			continue
		}
		// F1084 / #1131: redact secrets before returning so pre-SAFE-T1201
		// memories (stored before redactSecrets was mandatory) don't leak.
		redacted, _ := redactSecrets(m.WorkspaceName, m.Content)
		m.Content = redacted
		memories = append(memories, m)
	}
	if err := rows.Err(); err != nil {
		log.Printf("admin/memories/export: rows error: %v", err)
	}

	c.JSON(http.StatusOK, memories)
}

// memoryImportEntry is the JSON shape accepted on import. Matches export format.
type memoryImportEntry struct {
	Content       string `json:"content"`
	Scope         string `json:"scope"`
	Namespace     string `json:"namespace"`
	CreatedAt     string `json:"created_at"` // RFC3339 string, preserved on insert
	WorkspaceName string `json:"workspace_name"`
}

// Import handles POST /admin/memories/import
// Accepts a JSON array of memories (same format as export). Matches each
// workspace by name (not UUID). Skips duplicates where workspace_id + content
// + scope already exist. Returns counts of imported and skipped entries.
//
// SECURITY (F1085 / #1132): calls redactSecrets on each content field
// before both the deduplication check and the INSERT so that imported memories
// with embedded credentials cannot land unredacted in agent_memories (SAFE-T1201
// parity with the commit_memory MCP bridge path).
//
// CUTOVER (PR-8 / RFC #2728): when MEMORY_V2_CUTOVER=true and the v2
// plugin is wired, writes through the plugin instead of agent_memories.
func (h *AdminMemoriesHandler) Import(c *gin.Context) {
	ctx := c.Request.Context()

	var entries []memoryImportEntry
	if err := c.ShouldBindJSON(&entries); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON: " + err.Error()})
		return
	}

	if h.cutoverActive() {
		h.importViaPlugin(c, ctx, entries)
		return
	}

	imported := 0
	skipped := 0
	errors := 0

	for _, entry := range entries {
		// 1. Resolve workspace by name
		var workspaceID string
		err := db.DB.QueryRowContext(ctx,
			`SELECT id FROM workspaces WHERE name = $1 LIMIT 1`,
			entry.WorkspaceName,
		).Scan(&workspaceID)
		if err != nil {
			log.Printf("admin/memories/import: workspace %q not found, skipping", entry.WorkspaceName)
			skipped++
			continue
		}

		// F1085 / #1132: scrub credential patterns before persistence so that
		// imported memories with secrets don't bypass SAFE-T1201 (#838).
		// Must run BEFORE the dedup check so the redacted content is what
		// gets stored — otherwise re-importing the same backup would produce
		// a duplicate with different (original, unredacted) content.
		content, _ := redactSecrets(workspaceID, entry.Content)

		// 2. Check for duplicate (same workspace + content + scope) using
		// the redacted content so that two backups with the same original
		// secret (same placeholder output) are treated as duplicates.
		var exists bool

		err = db.DB.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM agent_memories WHERE workspace_id = $1 AND content = $2 AND scope = $3)`,
			workspaceID, content, entry.Scope,
		).Scan(&exists)
		if err != nil {
			log.Printf("admin/memories/import: duplicate check error for workspace %q: %v", entry.WorkspaceName, err)
			errors++
			continue
		}
		if exists {
			skipped++
			continue
		}

		// 3. Insert the memory, preserving original created_at if provided
		namespace := entry.Namespace
		if namespace == "" {
			namespace = "general"
		}

		if entry.CreatedAt != "" {
			_, err = db.DB.ExecContext(ctx,
				`INSERT INTO agent_memories (workspace_id, content, scope, namespace, created_at) VALUES ($1, $2, $3, $4, $5)`,
				workspaceID, content, entry.Scope, namespace, entry.CreatedAt,
			)
		} else {
			_, err = db.DB.ExecContext(ctx,
				`INSERT INTO agent_memories (workspace_id, content, scope, namespace) VALUES ($1, $2, $3, $4)`,
				workspaceID, content, entry.Scope, namespace,
			)
		}
		if err != nil {
			log.Printf("admin/memories/import: insert error for workspace %q: %v", entry.WorkspaceName, err)
			errors++
			continue
		}
		imported++
	}

	c.JSON(http.StatusOK, gin.H{
		"imported": imported,
		"skipped":  skipped,
		"errors":   errors,
		"total":    len(entries),
	})
}

// exportViaPlugin reads memories from the v2 plugin and emits them in
// the legacy memoryExportEntry shape so existing tooling that consumes
// the export keeps working.
//
// Strategy: enumerate workspaces, ask the resolver for each one's
// readable namespaces, search each namespace once. Deduplicate by
// memory id (a single memory in team:X is visible to every workspace
// under root X — we want one row per memory, not N).
func (h *AdminMemoriesHandler) exportViaPlugin(c *gin.Context, ctx context.Context) {
	rows, err := db.DB.QueryContext(ctx, `SELECT id::text, name FROM workspaces ORDER BY created_at`)
	if err != nil {
		log.Printf("admin/memories/export (cutover): workspaces query: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "export query failed"})
		return
	}
	defer rows.Close()

	type wsRow struct{ ID, Name string }
	var workspaces []wsRow
	for rows.Next() {
		var w wsRow
		if err := rows.Scan(&w.ID, &w.Name); err != nil {
			continue
		}
		workspaces = append(workspaces, w)
	}

	seen := make(map[string]struct{})
	memories := make([]memoryExportEntry, 0)
	for _, w := range workspaces {
		readable, err := h.resolver.ReadableNamespaces(ctx, w.ID)
		if err != nil {
			log.Printf("admin/memories/export (cutover) workspace=%s: resolve: %v", w.Name, err)
			continue
		}
		nsList := make([]string, len(readable))
		for i, ns := range readable {
			nsList[i] = ns.Name
		}
		if len(nsList) == 0 {
			continue
		}
		resp, err := h.plugin.Search(ctx, contract.SearchRequest{Namespaces: nsList, Limit: 100})
		if err != nil {
			log.Printf("admin/memories/export (cutover) workspace=%s: plugin search: %v", w.Name, err)
			continue
		}
		for _, m := range resp.Memories {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = struct{}{}
			redacted, _ := redactSecrets(w.Name, m.Content)
			memories = append(memories, memoryExportEntry{
				ID:            m.ID,
				Content:       redacted,
				Scope:         legacyScopeFromNamespace(m.Namespace),
				Namespace:     m.Namespace,
				CreatedAt:     m.CreatedAt,
				WorkspaceName: w.Name,
			})
		}
	}
	c.JSON(http.StatusOK, memories)
}

// importViaPlugin writes the entries through the plugin instead of
// directly to agent_memories. Workspaces are resolved by name like
// the legacy path. Scope→namespace mapping mirrors the PR-6 shim.
func (h *AdminMemoriesHandler) importViaPlugin(c *gin.Context, ctx context.Context, entries []memoryImportEntry) {
	imported := 0
	skipped := 0
	errs := 0

	for _, entry := range entries {
		var workspaceID string
		if err := db.DB.QueryRowContext(ctx,
			`SELECT id::text FROM workspaces WHERE name = $1 LIMIT 1`,
			entry.WorkspaceName,
		).Scan(&workspaceID); err != nil {
			log.Printf("admin/memories/import (cutover): workspace %q not found, skipping", entry.WorkspaceName)
			skipped++
			continue
		}

		// Redact BEFORE the plugin sees it (SAFE-T1201 parity).
		content, _ := redactSecrets(workspaceID, entry.Content)

		ns, err := h.scopeToWritableNamespaceForImport(ctx, workspaceID, entry.Scope)
		if err != nil {
			log.Printf("admin/memories/import (cutover): %v", err)
			skipped++
			continue
		}

		// Idempotent namespace upsert before commit.
		if _, err := h.plugin.UpsertNamespace(ctx, ns, contract.NamespaceUpsert{
			Kind: namespaceKindFromLegacyScope(entry.Scope),
		}); err != nil {
			log.Printf("admin/memories/import (cutover): upsert ns %s: %v", ns, err)
			errs++
			continue
		}

		if _, err := h.plugin.CommitMemory(ctx, ns, contract.MemoryWrite{
			Content: content,
			Kind:    contract.MemoryKindFact,
			Source:  contract.MemorySourceAgent,
		}); err != nil {
			log.Printf("admin/memories/import (cutover): commit %s: %v", ns, err)
			errs++
			continue
		}
		imported++
	}

	c.JSON(http.StatusOK, gin.H{
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
		"total":    len(entries),
	})
}

// scopeToWritableNamespaceForImport mirrors the PR-6 shim translation.
// Returns the namespace string the resolver picks for the requested
// scope; errors out cleanly on GLOBAL or unmapped values so importing
// a malformed entry doesn't crash the run.
func (h *AdminMemoriesHandler) scopeToWritableNamespaceForImport(ctx context.Context, workspaceID, scope string) (string, error) {
	writable, err := h.resolver.WritableNamespaces(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	wantKind := contract.NamespaceKindWorkspace
	switch strings.ToUpper(scope) {
	case "", "LOCAL":
		wantKind = contract.NamespaceKindWorkspace
	case "TEAM":
		wantKind = contract.NamespaceKindTeam
	case "GLOBAL":
		wantKind = contract.NamespaceKindOrg
	default:
		return "", &skipImport{reason: "unknown scope: " + scope}
	}
	for _, ns := range writable {
		if ns.Kind == wantKind {
			return ns.Name, nil
		}
	}
	return "", &skipImport{reason: "no writable namespace of kind " + string(wantKind)}
}

// skipImport is a typed error so the caller can distinguish "skip
// this entry" from a hard failure.
type skipImport struct{ reason string }

func (e *skipImport) Error() string { return "skip: " + e.reason }

// legacyScopeFromNamespace reverses the namespace→scope mapping for
// the export shape. Mirrors namespaceKindToLegacyScope from the PR-6
// shim but is lifted out so admin_memories doesn't depend on the MCP
// handler's helpers.
func legacyScopeFromNamespace(ns string) string {
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

// namespaceKindFromLegacyScope returns the contract.NamespaceKind for
// a legacy scope value. Unknown defaults to workspace so importing
// an unexpected row still produces a typed namespace.
func namespaceKindFromLegacyScope(scope string) contract.NamespaceKind {
	switch strings.ToUpper(scope) {
	case "TEAM":
		return contract.NamespaceKindTeam
	case "GLOBAL":
		return contract.NamespaceKindOrg
	default:
		return contract.NamespaceKindWorkspace
	}
}

