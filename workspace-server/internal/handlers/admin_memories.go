package handlers

import (
	"context"
	"database/sql"
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
// Optimization (#289 fix): the previous implementation was O(workspaces)
// in BOTH resolver CTE walks AND plugin search calls. For a 1000-tenant
// org, that's 1000 × resolver + 1000 × HTTP, where most are redundant
// because workspaces sharing a team/org root see identical namespaces.
//
// New strategy:
//   1. Single SQL pass walks parent_id chains, returning each
//      workspace's root_id alongside its name.
//   2. Group workspaces by root → unique tree count is typically <<
//      workspace count.
//   3. Resolve namespaces ONCE per root (any workspace under that
//      root produces the same readable list).
//   4. Build a UNION of namespaces across all roots; single plugin
//      search call.
//   5. Map each memory back to a workspace_name via a namespace→ws
//      lookup table built up from step 3.
//
// Net cost: 1 SQL + N_roots resolver calls + 1 plugin call (vs
// N_workspaces resolver + N_workspaces plugin in the old code).
func (h *AdminMemoriesHandler) exportViaPlugin(c *gin.Context, ctx context.Context) {
	// 1. One SQL pass: every workspace + its root id.
	wsRows, err := loadWorkspacesWithRoots(ctx, db.DB)
	if err != nil {
		log.Printf("admin/memories/export (cutover): workspaces query: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "export query failed"})
		return
	}

	// 2. Group by root → list of workspaces.
	rootToWorkspaces := make(map[string][]workspaceRow, len(wsRows))
	for _, w := range wsRows {
		rootToWorkspaces[w.RootID] = append(rootToWorkspaces[w.RootID], w)
	}

	// 3. Resolve team/org namespaces once per root, then add each
	// member's private workspace:<id> namespace explicitly.
	//
	// IMPORTANT: ReadableNamespaces(rootID) returns
	// {workspace:rootID, team:rootID, org:rootID}. Calling it once
	// per root is enough for team:/org:/custom: (those are shared by
	// every member of the root group), but the workspace: namespace
	// it returns is rootID's only — child members' private
	// workspace:<childID> namespaces would be silently dropped from
	// the export. Inject each member's workspace:<id> below to keep
	// coverage parity with the legacy per-workspace iteration.
	nsToOwner := make(map[string]string)       // namespace → workspace_name (first matching wins)
	allNamespaces := make(map[string]struct{}) // union for plugin search
	for rootID, members := range rootToWorkspaces {
		readable, err := h.resolver.ReadableNamespaces(ctx, rootID)
		if err != nil {
			log.Printf("admin/memories/export (cutover) root=%s: resolve: %v", rootID, err)
			continue
		}
		// Collect non-workspace namespaces (team:/org:/custom:/...) from
		// the root view; these are identical across every member.
		for _, ns := range readable {
			if strings.HasPrefix(ns.Name, "workspace:") {
				continue
			}
			allNamespaces[ns.Name] = struct{}{}
			if _, alreadyMapped := nsToOwner[ns.Name]; alreadyMapped {
				continue
			}
			if owner := pickOwnerForNamespace(ns.Name, members); owner != "" {
				nsToOwner[ns.Name] = owner
			}
		}
		// Inject each member's private workspace:<id> namespace + its
		// owner. Children's private memories live in workspace:<childID>
		// which the root-only resolve doesn't surface.
		for _, m := range members {
			ns := "workspace:" + m.ID
			allNamespaces[ns] = struct{}{}
			nsToOwner[ns] = m.Name
		}
	}

	if len(allNamespaces) == 0 {
		c.JSON(http.StatusOK, []memoryExportEntry{})
		return
	}

	// 4. Single plugin search across the union.
	nsList := make([]string, 0, len(allNamespaces))
	for ns := range allNamespaces {
		nsList = append(nsList, ns)
	}
	resp, err := h.plugin.Search(ctx, contract.SearchRequest{Namespaces: nsList, Limit: 100})
	if err != nil {
		log.Printf("admin/memories/export (cutover): plugin search: %v", err)
		c.JSON(http.StatusOK, []memoryExportEntry{})
		return
	}

	// 5. Map each memory to a workspace_name, redact, emit.
	seen := make(map[string]struct{})
	memories := make([]memoryExportEntry, 0, len(resp.Memories))
	for _, m := range resp.Memories {
		if _, dup := seen[m.ID]; dup {
			continue
		}
		seen[m.ID] = struct{}{}
		owner := nsToOwner[m.Namespace]
		redacted, _ := redactSecrets(owner, m.Content)
		memories = append(memories, memoryExportEntry{
			ID:            m.ID,
			Content:       redacted,
			Scope:         legacyScopeFromNamespace(m.Namespace),
			Namespace:     m.Namespace,
			CreatedAt:     m.CreatedAt,
			WorkspaceName: owner,
		})
	}
	c.JSON(http.StatusOK, memories)
}

// workspaceRow bundles the per-workspace fields the optimized export
// needs (id + name + root for grouping).
type workspaceRow struct {
	ID     string
	Name   string
	RootID string
}

// loadWorkspacesWithRoots returns one row per workspace with its root
// id computed via a recursive CTE. Single SQL pass — replaces the
// previous N×ReadableNamespaces pattern that walked each tree
// independently.
func loadWorkspacesWithRoots(ctx context.Context, conn *sql.DB) ([]workspaceRow, error) {
	rows, err := conn.QueryContext(ctx, `
		WITH RECURSIVE chain AS (
			SELECT id, parent_id, name, id AS root_id, 0 AS depth
			FROM workspaces
			WHERE parent_id IS NULL
			UNION ALL
			SELECT w.id, w.parent_id, w.name, c.root_id, c.depth + 1
			FROM workspaces w
			JOIN chain c ON w.parent_id = c.id
			WHERE c.depth < 50
		)
		SELECT id::text, name, root_id::text FROM chain ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]workspaceRow, 0)
	for rows.Next() {
		var w workspaceRow
		if err := rows.Scan(&w.ID, &w.Name, &w.RootID); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// pickOwnerForNamespace returns the workspace_name to attribute a
// namespace to in the export. workspace:<id> namespaces map to the
// matching member; team:* / org:* / custom:* fall back to the first
// member of the root group (canonical owner).
func pickOwnerForNamespace(ns string, members []workspaceRow) string {
	if strings.HasPrefix(ns, "workspace:") {
		wantID := strings.TrimPrefix(ns, "workspace:")
		for _, m := range members {
			if m.ID == wantID {
				return m.Name
			}
		}
	}
	// Non-workspace namespaces: attribute to first member of the root
	// group. Stable because loadWorkspacesWithRoots returns ORDER BY
	// name, so the same root group always picks the same owner.
	if len(members) > 0 {
		return members[0].Name
	}
	return ""
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

