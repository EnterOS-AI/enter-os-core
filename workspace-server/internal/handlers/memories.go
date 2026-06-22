package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/memory/client"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/memory/contract"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/memory/namespace"
	"github.com/gin-gonic/gin"
)

// defaultMemoryNamespace is used when a caller omits the field on POST or
// when querying for memories written before migration 017. Matches the
// column default in platform/migrations/017_memories_fts_namespace.up.sql.
const defaultMemoryNamespace = "general"

// secretPatternEntry is a compiled regex + its human-readable redaction label.
type secretPatternEntry struct {
	re    *regexp.Regexp
	label string
}

// memorySecretPatterns are checked in order — most-specific first so that
// env-var assignments (OPENAI_API_KEY=sk-...) are caught before the generic
// sk-* or base64 patterns consume only part of the match.
//
// Covered by SAFE-T1201 (issue #838). Extended by issue #2832 to cover raw
// tokens pasted without an env-var wrapper, DATABASE_URL with embedded
// credentials, and PEM-encoded private keys.
var memorySecretPatterns = []secretPatternEntry{
	// Env-var assignments:  ANTHROPIC_API_KEY=sk-ant-...  GITHUB_TOKEN=ghp_...
	{regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9_]*_API_KEY\s*=\s*\S+`), "API_KEY"},
	{regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9_]*_TOKEN\s*=\s*\S+`), "TOKEN"},
	{regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9_]*_SECRET\s*=\s*\S+`), "SECRET"},
	// HTTP Bearer header values
	{regexp.MustCompile(`Bearer\s+\S+`), "BEARER_TOKEN"},
	// PEM-encoded private keys (OPENSSH / RSA / EC / DSA / generic).
	// OPENSSH is the most specific, so check it FIRST; the generic
	// "PRIVATE KEY" pattern below also matches the OPENSSH header
	// (the `[ A-Z]+` char class allows the space in "OPENSSH PRIVATE
	// KEY"), so ordering matters — the OPENSSH-specific label wins
	// because it matches first.
	{regexp.MustCompile(`-----BEGIN OPENSSH PRIVATE KEY-----[\s\S]*?-----END OPENSSH PRIVATE KEY-----`), "OPENSSH_PRIVATE_KEY"},
	{regexp.MustCompile(`-----BEGIN[ A-Z]+PRIVATE KEY-----[\s\S]*?-----END[ A-Z]+PRIVATE KEY-----`), "PRIVATE_KEY"},
	// DATABASE_URL with embedded credentials:
	//   postgres://user:pass@host:5432/db
	//   postgresql://user:pass@host/db?sslmode=require
	//   mysql://user:pass@host:3306/db
	//   mongodb://user:pass@host:27017/db
	//   mongodb+srv://user:pass@cluster/db
	//   redis://:pass@host:6379/0          ← user part may be EMPTY
	//     (Redis URLs omit the username when only an AUTH password is set)
	// The credential segment is the part between `://` and `@`. The
	// user part is optional (Redis can pass a bare password); the
	// password part is required. The scheme allowlist keeps the
	// regex from over-matching arbitrary `user:pass@` substrings in
	// prose.
	{regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|amqp|amqps)://(?:[^\s/@:]*:[^\s/@]+)@[^\s/]+`), "DB_URL_WITH_CREDENTIALS"},
	// Raw GitHub / Vercel / AWS / Perplexity tokens — pasted in chat
	// without an env-var wrapper. Each prefix is provider-specific and
	// well-known; matching the prefix + 16+ chars of base64-ish body
	// keeps false-positives low.
	//
	// GitHub token prefix legend:
	//   ghp_  → personal access token (classic)
	//   gho_  → OAuth user token
	//   ghu_  → user-to-server token
	//   ghs_  → GitHub App server-to-server token
	//   ghr_  → refresh token
	//   github_pat_ → fine-grained PAT
	{regexp.MustCompile(`\bghp_[A-Za-z0-9]{16,}`), "GITHUB_PAT"},
	{regexp.MustCompile(`\bgho_[A-Za-z0-9]{16,}`), "GITHUB_OAUTH"},
	{regexp.MustCompile(`\bghs_[A-Za-z0-9]{16,}`), "GITHUB_APP_SERVER_TOKEN"},
	{regexp.MustCompile(`\bghu_[A-Za-z0-9]{16,}`), "GITHUB_USER_TOKEN"},
	{regexp.MustCompile(`\bghr_[A-Za-z0-9]{16,}`), "GITHUB_REFRESH_TOKEN"},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{16,}`), "GITHUB_FINEGRAINED_PAT"},
	{regexp.MustCompile(`\bvc_[A-Za-z0-9]{16,}`), "VERCEL_TOKEN"},
	{regexp.MustCompile(`\bAKIA[A-Z0-9]{16}`), "AWS_ACCESS_KEY_ID"},
	{regexp.MustCompile(`\bpplx-[A-Za-z0-9]{20,}`), "PERPLEXITY_API_KEY"},
	// OpenAI / Anthropic sk-... key format
	{regexp.MustCompile(`sk-[A-Za-z0-9\-_]{16,}`), "SK_TOKEN"},
	// context7 tokens
	{regexp.MustCompile(`ctx7_[A-Za-z0-9]+`), "CTX7_TOKEN"},
	// High-entropy base64 blobs — must contain a base64-only char (+/=) OR
	// be longer than 40 chars to avoid false-positives on plain long words.
	{regexp.MustCompile(`[A-Za-z0-9+/]{33,}={0,2}`), "BASE64_BLOB"},
}

// redactSecrets scrubs known secret patterns from content before persistence.
// Each distinct pattern class that fires logs a warning (without the value).
// Returns the sanitised string and a bool indicating whether anything changed.
// Failure is impossible — returns original content unchanged on any panic.
func redactSecrets(workspaceID, content string) (out string, changed bool) {
	out = content
	for _, p := range memorySecretPatterns {
		replaced := p.re.ReplaceAllString(out, "[REDACTED:"+p.label+"]")
		if replaced != out {
			log.Printf("commit_memory: redacted %s pattern for workspace %s (SAFE-T1201)", p.label, workspaceID)
			out = replaced
			changed = true
		}
	}
	return out, changed
}

// MemoriesHandler owns the legacy POST /workspaces/:id/memories surface,
// which post-#1794 routes through the v2 memory plugin. The legacy
// Search/Update/Delete methods + their HTTP routes were removed in
// #1792 (Phase A3) along with the agent_memories table they read.
// New code that needs memory reads should use the /v2/memories endpoints
// exposed by MemoriesV2Handler (canvas) or the MCP memory tools (agents).
type MemoriesHandler struct {
	// memv2 routes Commit writes through the v2 memory plugin. When nil,
	// Commit returns 503 — matches #1747's "plugin is the only backend"
	// posture for the MCP path.
	memv2 *memoryV2Deps
}

// NewMemoriesHandler constructs a handler with no plugin wired. Call
// WithMemoryV2 to attach the production plugin client.
func NewMemoriesHandler() *MemoriesHandler {
	return &MemoriesHandler{}
}

// WithMemoryV2 wires the plugin client + namespace resolver so Commit
// can route writes through the v2 plugin instead of raw SQL into
// `agent_memories` (issue #1791). Mirrors MCPHandler.WithMemoryV2 so
// the same boot-time pattern works for both surfaces.
//
// Boot-time: main.go calls this after Boot()-ing the plugin client.
// When this is not called (test fixtures or new operators without
// MEMORY_PLUGIN_URL), Commit returns 503 with a clear hint.
func (h *MemoriesHandler) WithMemoryV2(plugin *client.Client, resolver *namespace.Resolver) *MemoriesHandler {
	h.memv2 = &memoryV2Deps{plugin: plugin, resolver: resolver}
	return h
}

// withMemoryV2APIs is the test-only injection path: takes the
// interfaces directly so unit tests don't have to construct a real
// *client.Client / namespace.Resolver. Symmetric with
// MCPHandler.withMemoryV2APIs.
func (h *MemoriesHandler) withMemoryV2APIs(plugin memoryPluginAPI, resolver namespaceResolverAPI) *MemoriesHandler {
	h.memv2 = &memoryV2Deps{plugin: plugin, resolver: resolver}
	return h
}

// Commit handles POST /workspaces/:id/memories
// Stores a memory fact with a scope (LOCAL, TEAM, GLOBAL) and an optional
// namespace (defaults to "general"). Namespaces implement the Holaboss
// knowledge/{facts,procedures,blockers,reference}/ pattern so agents can
// file and recall memories by category.
//
// When an EmbeddingFunc is configured, Commit also stores a vector embedding
// so future Search calls can use cosine-similarity ordering. Embedding
// failure is non-fatal: the memory is stored without an embedding and the
// response is still 201.
func (h *MemoriesHandler) Commit(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		Content   string `json:"content" binding:"required"`
		Scope     string `json:"scope" binding:"required"` // LOCAL, TEAM, GLOBAL
		Namespace string `json:"namespace,omitempty"`      // optional; defaults to "general"
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if body.Scope != "LOCAL" && body.Scope != "TEAM" && body.Scope != "GLOBAL" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be LOCAL, TEAM, or GLOBAL"})
		return
	}

	namespace := body.Namespace
	if namespace == "" {
		namespace = defaultMemoryNamespace
	}
	if len(namespace) > 50 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "namespace must be <= 50 characters"})
		return
	}

	// GLOBAL scope: only root workspaces (no parent) can write
	if body.Scope == "GLOBAL" {
		var parentID *string
		db.DB.QueryRowContext(ctx, `SELECT parent_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&parentID)
		if parentID != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "only root workspaces can write GLOBAL memories"})
			return
		}
	}

	// SAFE-T1201: scrub secret patterns before persistence so that a confused
	// or prompt-injected agent cannot exfiltrate credentials into shared TEAM/
	// GLOBAL memory. Runs on every write, regardless of scope.
	content := body.Content
	content, _ = redactSecrets(workspaceID, content)

	// SAFE-T1201: prevent delimiter spoofing in GLOBAL memories (#807).
	// If content contains the delimiter prefix "[MEMORY ", an attacker could
	// craft a fake nested delimiter to inject instructions when the memory
	// is read back. Escape the bracket so it renders as text, not structure.
	if body.Scope == "GLOBAL" {
		content = strings.ReplaceAll(content, "[MEMORY ", "[_MEMORY ")
	}

	// v2 plugin is the only write backend (issue #1791 — Phase A2 step 1,
	// mirrors #1747's no-fallback posture for the MCP path). When the plugin
	// isn't wired, return 503 with a clear hint rather than silently
	// dropping the write or falling back to a frozen v1 table no one reads.
	if h.memv2 == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "memory plugin is not configured (set MEMORY_PLUGIN_URL)",
		})
		return
	}

	// Resolve the v1 scope (LOCAL/TEAM/GLOBAL) to the v2 plugin namespace
	// kind. The resolver picks the actual namespace string at runtime —
	// we only need the kind here.
	var wantKind contract.NamespaceKind
	switch body.Scope {
	case "LOCAL":
		wantKind = contract.NamespaceKindWorkspace
	case "TEAM":
		wantKind = contract.NamespaceKindTeam
	case "GLOBAL":
		wantKind = contract.NamespaceKindOrg
	}
	writable, err := h.memv2.resolver.WritableNamespaces(ctx, workspaceID)
	if err != nil {
		log.Printf("Commit: resolve writable namespaces for %s failed: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve writable namespaces"})
		return
	}
	var nsName string
	for _, ns := range writable {
		if ns.Kind == wantKind {
			nsName = ns.Name
			break
		}
	}
	if nsName == "" {
		c.JSON(http.StatusForbidden, gin.H{
			"error": fmt.Sprintf("no writable namespace of kind %s for workspace %s", wantKind, workspaceID),
		})
		return
	}

	// Ensure the namespace row exists before the write — the plugin's
	// CommitMemory contract is "namespace must already exist (auto-created
	// by handler if not)" and memory_records carries an FK to
	// memory_namespaces. The MCP tool path (mcp_tools_memory_v2.go) has
	// always done this upsert; this HTTP path skipped it, so any workspace
	// whose namespace row was never seeded (every workspace created after
	// the Phase A2 backfill that only ever wrote through this surface —
	// i.e. the runtime a2a commit_memory tool and the canvas) failed every
	// write with `memory_records_namespace_fkey` (fleet-wide, 2026-06-10).
	// UpsertNamespace is idempotent, so the extra round-trip on warm
	// namespaces is a cheap no-op.
	if _, err := h.memv2.plugin.UpsertNamespace(ctx, nsName, contract.NamespaceUpsert{Kind: kindFromNamespace(nsName)}); err != nil {
		log.Printf(
			"Commit memory namespace upsert error: workspace=%s scope=%s namespace=%s err_class=%T err=%q",
			workspaceID, body.Scope, nsName, err, err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store memory"})
		return
	}

	// Plugin write. The plugin owns its own embedding generation (FTS
	// + vector indices are internal to memory_plugin schema), so we no
	// longer call h.embed here — that becomes dead weight on this path
	// and is left in place only for Search/Get which still read v1.
	resp, err := h.memv2.plugin.CommitMemory(ctx, nsName, contract.MemoryWrite{
		Content: content,
		Kind:    contract.MemoryKindFact,
		// Source=user: HTTP POST /memories is the canvas/operator surface,
		// not the agent MCP path (which uses MemorySourceAgent). The plugin
		// uses this for activity-log + audit attribution.
		Source: contract.MemorySourceUser,
	})
	if err != nil {
		// The underlying plugin error must NOT leak to the HTTP response body
		// (generic 500 keeps client surface stable). Emit full operator context
		// (workspace, scope, namespace, error type + message) server-side so
		// recurring incidents (continuous-synth E2E, HMA memory-commit, etc.)
		// can be distinguished in the log aggregator.
		log.Printf(
			"Commit memory plugin error: workspace=%s scope=%s namespace=%s err_class=%T err=%q",
			workspaceID, body.Scope, nsName, err, err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store memory"})
		return
	}
	memoryID := resp.ID

	// #767 Audit: write a GLOBAL memory audit log entry for forensic replay.
	// Records a SHA-256 hash of the content — never plaintext — so the audit
	// trail can prove what was written without leaking sensitive values.
	// Failure is non-fatal: a logging error must not roll back a successful write.
	if body.Scope == "GLOBAL" {
		// Hash the sanitised content so the audit trail reflects what was
		// actually persisted (not the raw, potentially secret-bearing input).
		sum := sha256.Sum256([]byte(content))
		auditBody, marshalErr := json.Marshal(map[string]string{
			"memory_id":      memoryID,
			"namespace":      nsName,
			"content_sha256": hex.EncodeToString(sum[:]),
		})
		if marshalErr != nil {
			log.Printf("Commit %s: json.Marshal auditBody failed: %v", workspaceID, marshalErr)
		} else {
			summary := "GLOBAL memory written: id=" + memoryID + " namespace=" + nsName
			if _, auditErr := db.DB.ExecContext(ctx, `
				INSERT INTO activity_logs (workspace_id, activity_type, source_id, summary, request_body, status)
				VALUES ($1, $2, $3, $4, $5::jsonb, $6)
			`, workspaceID, "memory_write_global", workspaceID, summary, string(auditBody), "ok"); auditErr != nil {
				log.Printf("Commit: GLOBAL memory audit log failed for %s/%s: %v", workspaceID, memoryID, auditErr)
			}
		}
	}

	// Preserve the legacy response shape ({id, scope, namespace}) so existing
	// HTTP callers (canvas, workspace runtimes) see no contract change. The
	// `namespace` field returns the user-supplied tag, not the v2 plugin
	// namespace — the latter is an internal storage detail.
	c.JSON(http.StatusCreated, gin.H{"id": memoryID, "scope": body.Scope, "namespace": namespace})
}

// Search handles GET /workspaces/:id/memories (legacy v1 read path).
//
// Phase A3 (#1792) removed the original v1 Search because it read the frozen
// agent_memories table. This shim restores the endpoint for old callers
// (AwarenessClient, runtime SDKs) by proxying through the v2 plugin and
// reshaping the response to the legacy contract.
func (h *MemoriesHandler) Search(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	if h.memv2 == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "memory plugin is not configured (set MEMORY_PLUGIN_URL)",
		})
		return
	}

	readable, err := h.memv2.resolver.ReadableNamespaces(ctx, workspaceID)
	if err != nil {
		log.Printf("memories search: resolve readable namespaces for %s failed: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve readable namespaces"})
		return
	}
	nsNames := make([]string, len(readable))
	for i, ns := range readable {
		nsNames[i] = ns.Name
	}

	resp, err := h.memv2.plugin.Search(ctx, contract.SearchRequest{
		Namespaces: nsNames,
		Limit:      50,
	})
	if err != nil {
		log.Printf("memories search: plugin search for %s failed: %v", workspaceID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "memory plugin search failed"})
		return
	}

	type legacyEntry struct {
		ID        string `json:"id"`
		Content   string `json:"content"`
		Scope     string `json:"scope"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]legacyEntry, 0, len(resp.Memories))
	for _, m := range resp.Memories {
		scope := namespaceKindToLegacyScope(m.Namespace)
		out = append(out, legacyEntry{
			ID:        m.ID,
			Content:   m.Content,
			Scope:     scope,
			CreatedAt: m.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}
	c.JSON(http.StatusOK, out)
}
