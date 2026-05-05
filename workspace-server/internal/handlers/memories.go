package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/registry"
	"github.com/gin-gonic/gin"
)

// globalMemoryDelimiter is the non-instructable prefix prepended to every
// GLOBAL-scope memory value returned to MCP clients. Prevents stored content
// from being parsed as LLM instructions in the agent's context window (#767).
// Format: [MEMORY id=<uuid> scope=GLOBAL from=<workspace_id>]: <value>
const globalMemoryDelimiter = "[MEMORY id=%s scope=GLOBAL from=%s]: %s"

// defaultMemoryNamespace is used when a caller omits the field on POST or
// when querying for memories written before migration 017. Matches the
// column default in platform/migrations/017_memories_fts_namespace.up.sql.
const defaultMemoryNamespace = "general"

// memoryFTSMinQueryLen is the shortest query length that gets Postgres
// full-text search treatment. Anything shorter uses ILIKE because
// tsvector requires at least one token and single characters tokenise
// to nothing in the 'english' config.
const memoryFTSMinQueryLen = 2

// secretPatternEntry is a compiled regex + its human-readable redaction label.
type secretPatternEntry struct {
	re    *regexp.Regexp
	label string
}

// memorySecretPatterns are checked in order — most-specific first so that
// env-var assignments (OPENAI_API_KEY=sk-...) are caught before the generic
// sk-* or base64 patterns consume only part of the match.
//
// Covered by SAFE-T1201 (issue #838).
var memorySecretPatterns = []secretPatternEntry{
	// Env-var assignments:  ANTHROPIC_API_KEY=sk-ant-...  GITHUB_TOKEN=ghp_...
	{regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9_]*_API_KEY\s*=\s*\S+`), "API_KEY"},
	{regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9_]*_TOKEN\s*=\s*\S+`), "TOKEN"},
	{regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9_]*_SECRET\s*=\s*\S+`), "SECRET"},
	// HTTP Bearer header values
	{regexp.MustCompile(`Bearer\s+\S+`), "BEARER_TOKEN"},
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

// EmbeddingFunc generates a 1536-dimensional dense-vector embedding for the
// given text. Must return exactly 1536 float32 values on success.
// Implementations must honour ctx cancellation.
// nil is not a valid return on success — return a non-nil error instead.
type EmbeddingFunc func(ctx context.Context, text string) ([]float32, error)

// MemoriesHandler manages agent memory storage and recall.
type MemoriesHandler struct {
	// embed generates vector embeddings for semantic search (issue #576).
	// nil disables the semantic path — all operations degrade gracefully to
	// the existing FTS/ILIKE path.
	embed EmbeddingFunc
}

// NewMemoriesHandler constructs a handler with FTS-only mode.
// Wire up semantic search with WithEmbedding.
func NewMemoriesHandler() *MemoriesHandler {
	return &MemoriesHandler{}
}

// WithEmbedding installs a vector-embedding function. Call during router
// wiring, before the first request. Passing nil is a no-op. Chainable.
func (h *MemoriesHandler) WithEmbedding(fn EmbeddingFunc) *MemoriesHandler {
	if fn != nil {
		h.embed = fn
	}
	return h
}

// formatVector encodes a float32 embedding slice as a pgvector literal
// suitable for a ::vector cast, e.g. "[0.1,-0.05,0.42]".
// Returns an empty string for nil/empty slices.
func formatVector(v []float32) string {
	if len(v) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", x)
	}
	b.WriteByte(']')
	return b.String()
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

	var memoryID string
	err := db.DB.QueryRowContext(ctx, `
		INSERT INTO agent_memories (workspace_id, content, scope, namespace)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, workspaceID, content, body.Scope, namespace).Scan(&memoryID)
	if err != nil {
		log.Printf("Commit memory error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store memory"})
		return
	}

	// #767 Audit: write a GLOBAL memory audit log entry for forensic replay.
	// Records a SHA-256 hash of the content — never plaintext — so the audit
	// trail can prove what was written without leaking sensitive values.
	// Failure is non-fatal: a logging error must not roll back a successful write.
	if body.Scope == "GLOBAL" {
		// Hash the sanitised content so the audit trail reflects what was
		// actually persisted (not the raw, potentially secret-bearing input).
		sum := sha256.Sum256([]byte(content))
		auditBody, _ := json.Marshal(map[string]string{
			"memory_id":      memoryID,
			"namespace":      namespace,
			"content_sha256": hex.EncodeToString(sum[:]),
		})
		summary := "GLOBAL memory written: id=" + memoryID + " namespace=" + namespace
		if _, auditErr := db.DB.ExecContext(ctx, `
			INSERT INTO activity_logs (workspace_id, activity_type, source_id, summary, request_body, status)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6)
		`, workspaceID, "memory_write_global", workspaceID, summary, string(auditBody), "ok"); auditErr != nil {
			log.Printf("Commit: GLOBAL memory audit log failed for %s/%s: %v", workspaceID, memoryID, auditErr)
		}
	}

	// Optionally embed and persist the vector. Non-fatal: the memory is
	// already stored above; a failed embedding just means this record will
	// be excluded from future cosine-similarity searches.
	if h.embed != nil {
		if vec, embedErr := h.embed(ctx, content); embedErr != nil {
			log.Printf("Commit: embedding failed workspace=%s memory=%s: %v (stored without embedding)",
				workspaceID, memoryID, embedErr)
		} else if fmtVec := formatVector(vec); fmtVec != "" {
			if _, updateErr := db.DB.ExecContext(ctx,
				`UPDATE agent_memories SET embedding = $1::vector WHERE id = $2`,
				fmtVec, memoryID,
			); updateErr != nil {
				log.Printf("Commit: embedding UPDATE failed workspace=%s memory=%s: %v",
					workspaceID, memoryID, updateErr)
			}
		}
	}

	c.JSON(http.StatusCreated, gin.H{"id": memoryID, "scope": body.Scope, "namespace": namespace})
}

// memoryRecallMaxLimit is the hard ceiling for results returned by Search.
// Callers may request fewer via ?limit=N but never more (#377).
const memoryRecallMaxLimit = 50

// Search handles GET /workspaces/:id/memories
// Searches memories visible to the requesting workspace.
//
// Supports:
//   - ?scope=LOCAL|TEAM|GLOBAL for access-control slicing
//   - ?q=... semantic search (cosine similarity) when an EmbeddingFunc is
//     configured AND the query can be embedded; falls back to FTS when the
//     embed call fails or no func is configured.
//   - ?q=... full-text search (ts_rank ordered) when len>=memoryFTSMinQueryLen
//     and no embedding is available; falls back to ILIKE for shorter strings.
//   - ?namespace=... additional filter on the Holaboss-style namespace tag
//   - ?limit=N max results (1–50); values >50 are silently clamped to 50 (#377)
//
// Semantic results include a "similarity_score" field (1 - cosine_distance).
func (h *MemoriesHandler) Search(c *gin.Context) {
	workspaceID := c.Param("id")
	scope := c.DefaultQuery("scope", "")
	query := c.DefaultQuery("q", "")
	namespace := c.DefaultQuery("namespace", "")

	// Parse and cap the limit. Anything ≤0 or absent → 50 (full page).
	// Anything >50 → 50 (hard ceiling — never error, just clamp).
	limit := memoryRecallMaxLimit
	if raw := c.Query("limit"); raw != "" {
		var n int
		if _, err := fmt.Sscanf(raw, "%d", &n); err == nil && n > 0 && n < memoryRecallMaxLimit {
			limit = n
		}
	}
	ctx := c.Request.Context()

	// Get workspace info for access control
	var parentID *string
	db.DB.QueryRowContext(ctx, `SELECT parent_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&parentID)

	// Try to generate a query embedding for semantic search.
	// Falls back to the existing FTS/ILIKE path on failure or when no
	// embedding function is configured.
	semanticVec := ""
	if query != "" && h.embed != nil {
		if vec, err := h.embed(ctx, query); err != nil {
			log.Printf("Search: embedding failed workspace=%s: %v — falling back to FTS", workspaceID, err)
		} else {
			semanticVec = formatVector(vec)
		}
	}

	var sqlQuery string
	var args []interface{}
	semantic := semanticVec != ""

	if semantic {
		// ── Semantic search path ──────────────────────────────────────────
		// Build scope-specific WHERE fragment and initial args.
		isJoin := scope == "TEAM"
		var baseWhere string
		switch scope {
		case "LOCAL":
			baseWhere = `workspace_id = $1 AND scope = 'LOCAL'`
			args = []interface{}{workspaceID}
		case "TEAM":
			if parentID != nil {
				baseWhere = `m.scope = 'TEAM' AND w.status != 'removed' AND (w.parent_id = $1 OR w.id = $1)`
				args = []interface{}{*parentID}
			} else {
				baseWhere = `m.scope = 'TEAM' AND w.status != 'removed' AND (w.parent_id = $1 OR w.id = $1)`
				args = []interface{}{workspaceID}
			}
		case "GLOBAL":
			baseWhere = `scope = 'GLOBAL'`
			args = []interface{}{}
		default:
			baseWhere = `workspace_id = $1`
			args = []interface{}{workspaceID}
		}
		if namespace != "" {
			nsArg := nextArg(len(args))
			if isJoin {
				baseWhere += ` AND m.namespace = ` + nsArg
			} else {
				baseWhere += ` AND namespace = ` + nsArg
			}
			args = append(args, namespace)
		}

		// $vecPos appears twice (SELECT + ORDER BY) — PostgreSQL resolves
		// both to the same bound value, so we append it only once.
		vecPos := nextArg(len(args))
		limitPos := nextArg(len(args) + 1)

		if isJoin {
			sqlQuery = `SELECT m.id, m.workspace_id, m.content, m.scope, m.namespace, m.created_at,` +
				` 1 - (m.embedding <=> ` + vecPos + `::vector) AS similarity_score` +
				` FROM agent_memories m JOIN workspaces w ON w.id = m.workspace_id` +
				` WHERE ` + baseWhere + ` AND m.embedding IS NOT NULL` +
				` ORDER BY m.embedding <=> ` + vecPos + `::vector` +
				` LIMIT ` + limitPos
		} else {
			sqlQuery = `SELECT id, workspace_id, content, scope, namespace, created_at,` +
				` 1 - (embedding <=> ` + vecPos + `::vector) AS similarity_score` +
				` FROM agent_memories` +
				` WHERE ` + baseWhere + ` AND embedding IS NOT NULL` +
				` ORDER BY embedding <=> ` + vecPos + `::vector` +
				` LIMIT ` + limitPos
		}
		args = append(args, semanticVec, limit)

	} else {
		// ── FTS / ILIKE / plain path ──────────────────────────────────────
		switch scope {
		case "LOCAL":
			// Only this workspace's memories
			sqlQuery = `SELECT id, workspace_id, content, scope, namespace, created_at FROM agent_memories WHERE workspace_id = $1 AND scope = 'LOCAL'`
			args = []interface{}{workspaceID}

		case "TEAM":
			// Team = self + parent + siblings (same parent_id)
			if parentID != nil {
				// Child workspace: team is parent + siblings sharing same parent_id
				sqlQuery = `SELECT m.id, m.workspace_id, m.content, m.scope, m.namespace, m.created_at
				FROM agent_memories m
				JOIN workspaces w ON w.id = m.workspace_id
				WHERE m.scope = 'TEAM' AND w.status != 'removed'
				AND (w.parent_id = $1 OR w.id = $1)`
				args = []interface{}{*parentID}
			} else {
				// Root workspace: team is self + direct children only
				sqlQuery = `SELECT m.id, m.workspace_id, m.content, m.scope, m.namespace, m.created_at
				FROM agent_memories m
				JOIN workspaces w ON w.id = m.workspace_id
				WHERE m.scope = 'TEAM' AND w.status != 'removed'
				AND (w.parent_id = $1 OR w.id = $1)`
				args = []interface{}{workspaceID}
			}

		case "GLOBAL":
			// All GLOBAL memories (readable by everyone)
			sqlQuery = `SELECT id, workspace_id, content, scope, namespace, created_at FROM agent_memories WHERE scope = 'GLOBAL'`
			args = []interface{}{}

		default:
			// All accessible memories
			sqlQuery = `SELECT id, workspace_id, content, scope, namespace, created_at FROM agent_memories WHERE workspace_id = $1`
			args = []interface{}{workspaceID}
		}

		// Namespace filter (optional) — applies regardless of scope.
		if namespace != "" {
			sqlQuery += ` AND namespace = ` + nextArg(len(args))
			args = append(args, namespace)
		}

		// Text search: FTS with ts_rank ordering for multi-char queries,
		// ILIKE fallback for 1-char and empty-after-tokenization edge cases.
		ftsActive := false
		if len(query) >= memoryFTSMinQueryLen {
			sqlQuery += ` AND content_tsv @@ plainto_tsquery('english', ` + nextArg(len(args)) + `)`
			args = append(args, query)
			ftsActive = true
		} else if query != "" {
			sqlQuery += ` AND content ILIKE ` + nextArg(len(args))
			args = append(args, "%"+query+"%")
		}

		if ftsActive {
			// Rank FTS hits first, tie-break by recency.
			sqlQuery += ` ORDER BY ts_rank(content_tsv, plainto_tsquery('english', ` + nextArg(len(args)) + `)) DESC, created_at DESC`
			args = append(args, query)
		} else {
			sqlQuery += ` ORDER BY created_at DESC`
		}
		sqlQuery += ` LIMIT ` + nextArg(len(args))
		args = append(args, limit)
	}

	rows, err := db.DB.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		log.Printf("Search memories error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "search failed"})
		return
	}
	defer rows.Close()

	memories := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, wsID, content, memScope, memNS, createdAt string
		entry := map[string]interface{}{}

		if semantic {
			var simScore float64
			if rows.Scan(&id, &wsID, &content, &memScope, &memNS, &createdAt, &simScore) != nil {
				continue
			}
			entry["similarity_score"] = simScore
		} else {
			if rows.Scan(&id, &wsID, &content, &memScope, &memNS, &createdAt) != nil {
				continue
			}
		}

		// Access control check for TEAM scope
		if memScope == "TEAM" && wsID != workspaceID {
			if !registry.CanCommunicate(workspaceID, wsID) {
				continue // Skip memories from workspaces we can't reach
			}
		}

		// #767: wrap GLOBAL-scope content with a non-instructable delimiter so
		// MCP tool outputs cannot be hijacked by stored prompt-injection payloads.
		// The raw content in the DB is unchanged — only the value returned to
		// callers is wrapped. Applied on both the semantic and FTS paths.
		if memScope == "GLOBAL" {
			content = fmt.Sprintf(globalMemoryDelimiter, id, wsID, content)
		}

		entry["id"] = id
		entry["workspace_id"] = wsID
		entry["content"] = content
		entry["scope"] = memScope
		entry["namespace"] = memNS
		entry["created_at"] = createdAt
		memories = append(memories, entry)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Search memories rows.Err: %v", err)
	}

	c.JSON(http.StatusOK, memories)
}

// Update handles PATCH /workspaces/:id/memories/:memoryId
//
// Edits an existing semantic-memory row's content and/or namespace.
// Both body fields are optional; at least one must be present (a body
// with neither returns 400 — there's nothing to do, and silently
// no-op'ing would let a buggy client think it had succeeded).
//
// Content edits re-run the same security pipeline as Commit: secret
// redaction (#1201) on every scope, plus delimiter-spoofing escape on
// GLOBAL. Skipping either when content changes would mean an Edit
// becomes a back-door past the policies a Commit enforces. The same
// re-embedding rule applies — a stale embedding for the new content
// would silently break semantic search. GLOBAL audit log fires on
// content change so the forensic trail captures edits, not just
// initial writes.
//
// Namespace edits are validated against the same 50-char ceiling
// Commit uses; cross-scope changes (e.g. LOCAL→GLOBAL) are NOT
// supported here — that's a delete + recreate so the GLOBAL
// access-control gate (only root workspaces can write GLOBAL) gets
// re-evaluated from scratch.
//
// Returns 200 with the updated row's id+scope+namespace on success,
// 400 on bad body, 404 when the memory doesn't exist or isn't owned
// by this workspace, 500 on DB failure.
func (h *MemoriesHandler) Update(c *gin.Context) {
	workspaceID := c.Param("id")
	memoryID := c.Param("memoryId")
	ctx := c.Request.Context()

	// json.Decode (not gin's ShouldBindJSON) so we can distinguish
	// "field omitted" from "field set to empty string" — content="" is
	// invalid; content omitted means "don't change content".
	var body struct {
		Content   *string `json:"content,omitempty"`
		Namespace *string `json:"namespace,omitempty"`
	}
	if err := json.NewDecoder(c.Request.Body).Decode(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if body.Content == nil && body.Namespace == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "at least one of content or namespace must be set",
		})
		return
	}
	if body.Content != nil && *body.Content == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content cannot be empty"})
		return
	}
	if body.Namespace != nil {
		if len(*body.Namespace) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "namespace cannot be empty"})
			return
		}
		if len(*body.Namespace) > 50 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "namespace must be <= 50 characters"})
			return
		}
	}

	// Fetch current row to discover the scope (we need it for the
	// GLOBAL delimiter-escape + audit log) and to confirm ownership.
	// One round-trip rather than two: SELECT ... WHERE id AND
	// workspace_id covers the 404 path without an extra existence check.
	var existingScope, existingContent, existingNamespace string
	if err := db.DB.QueryRowContext(ctx, `
		SELECT scope, content, namespace
		FROM agent_memories
		WHERE id = $1 AND workspace_id = $2
	`, memoryID, workspaceID).Scan(&existingScope, &existingContent, &existingNamespace); err != nil {
		// sql.ErrNoRows or any other read failure — both surface as 404
		// to avoid leaking row existence across workspaces.
		c.JSON(http.StatusNotFound, gin.H{"error": "memory not found or not owned by this workspace"})
		return
	}

	// Compute the new content (post-redaction, post-delimiter-escape)
	// only when content is actually changing. This keeps namespace-only
	// edits cheap (no embed call, no audit row).
	newContent := existingContent
	contentChanged := false
	if body.Content != nil && *body.Content != existingContent {
		c2 := *body.Content
		c2, _ = redactSecrets(workspaceID, c2)
		if existingScope == "GLOBAL" {
			c2 = strings.ReplaceAll(c2, "[MEMORY ", "[_MEMORY ")
		}
		if c2 != existingContent {
			newContent = c2
			contentChanged = true
		}
	}

	newNamespace := existingNamespace
	if body.Namespace != nil && *body.Namespace != existingNamespace {
		newNamespace = *body.Namespace
	}

	if !contentChanged && newNamespace == existingNamespace {
		// Nothing to do post-normalisation (e.g. caller passed the
		// SAME content + namespace). Return the existing shape so the
		// caller's response-handling can stay uniform with the change
		// path — silently no-op would force every client to special-
		// case 204.
		c.JSON(http.StatusOK, gin.H{
			"id": memoryID, "scope": existingScope, "namespace": existingNamespace,
			"changed": false,
		})
		return
	}

	if _, err := db.DB.ExecContext(ctx, `
		UPDATE agent_memories
		SET content = $1, namespace = $2, updated_at = now()
		WHERE id = $3 AND workspace_id = $4
	`, newContent, newNamespace, memoryID, workspaceID); err != nil {
		log.Printf("Update memory error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update memory"})
		return
	}

	// GLOBAL content edits write an audit row mirroring Commit's #767
	// pattern. Namespace-only edits don't get an audit entry — the
	// content (and its sha256) is unchanged, so there's nothing new
	// for forensic replay to capture.
	if existingScope == "GLOBAL" && contentChanged {
		sum := sha256.Sum256([]byte(newContent))
		auditBody, _ := json.Marshal(map[string]string{
			"memory_id":      memoryID,
			"namespace":      newNamespace,
			"content_sha256": hex.EncodeToString(sum[:]),
			"reason":         "edited",
		})
		summary := "GLOBAL memory edited: id=" + memoryID + " namespace=" + newNamespace
		if _, auditErr := db.DB.ExecContext(ctx, `
			INSERT INTO activity_logs (workspace_id, activity_type, source_id, summary, request_body, status)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6)
		`, workspaceID, "memory_edit_global", workspaceID, summary, string(auditBody), "ok"); auditErr != nil {
			log.Printf("Update: GLOBAL memory audit log failed for %s/%s: %v", workspaceID, memoryID, auditErr)
		}
	}

	// Re-embed when content changed. Same non-fatal pattern as Commit:
	// a failed embed leaves the row with its OLD vector (or no vector
	// if the original Commit's embed also failed). Future Search calls
	// fall through to FTS for this row.
	if contentChanged && h.embed != nil {
		if vec, embedErr := h.embed(ctx, newContent); embedErr != nil {
			log.Printf("Update: embedding failed workspace=%s memory=%s: %v (kept stale embedding)",
				workspaceID, memoryID, embedErr)
		} else if fmtVec := formatVector(vec); fmtVec != "" {
			if _, updateErr := db.DB.ExecContext(ctx,
				`UPDATE agent_memories SET embedding = $1::vector WHERE id = $2`,
				fmtVec, memoryID,
			); updateErr != nil {
				log.Printf("Update: embedding UPDATE failed workspace=%s memory=%s: %v",
					workspaceID, memoryID, updateErr)
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"id":        memoryID,
		"scope":     existingScope,
		"namespace": newNamespace,
		"changed":   true,
	})
}

// Delete handles DELETE /workspaces/:id/memories/:memoryId
func (h *MemoriesHandler) Delete(c *gin.Context) {
	workspaceID := c.Param("id")
	memoryID := c.Param("memoryId")
	ctx := c.Request.Context()

	result, err := db.DB.ExecContext(ctx,
		`DELETE FROM agent_memories WHERE id = $1 AND workspace_id = $2`, memoryID, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete failed"})
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "memory not found or not owned by this workspace"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

func nextArg(current int) string {
	return fmt.Sprintf("$%d", current+1)
}