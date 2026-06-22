package handlers

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/approvals"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/audit"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/crypto"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
)

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var platformManagedDirectLLMBypassKeys = map[string]struct{}{
	"AI_GATEWAY_API_KEY":      {},
	"ANTHROPIC_API_KEY":       {},
	"ANTHROPIC_AUTH_TOKEN":    {},
	"ARCEEAI_API_KEY":         {},
	"CLAUDE_CODE_OAUTH_TOKEN": {},
	"CODEX_AUTH_JSON":         {},
	"DASHSCOPE_API_KEY":       {},
	"DEEPSEEK_API_KEY":        {},
	"GEMINI_API_KEY":          {},
	"GLM_API_KEY":             {},
	"HERMES_CUSTOM_API_KEY":   {},
	"HERMES_CUSTOM_BASE_URL":  {},
	"HF_TOKEN":                {},
	"KIMI_API_KEY":            {},
	"KIMI_CN_API_KEY":         {},
	"MINIMAX_API_KEY":         {},
	"MINIMAX_CN_API_KEY":      {},
	"NOUS_API_KEY":            {},
	"OPENAI_API_KEY":          {},
	"OPENAI_BASE_URL":         {},
	"OPENROUTER_API_KEY":      {},
	"XAI_API_KEY":             {},
	"ZAI_API_KEY":             {},
}

func isPlatformManagedDirectLLMBypassKey(key string) bool {
	_, ok := platformManagedDirectLLMBypassKeys[strings.ToUpper(strings.TrimSpace(key))]
	return ok
}

// platformManagedLLMModeForWorkspace replaces the org-level platformManagedLLMMode
// gate with a per-workspace resolved-mode check (internal#691). The strip-list
// is enforced ONLY when this specific workspace's resolved mode is
// platform_managed — a workspace with a byok override is allowed to write its
// own CLAUDE_CODE_OAUTH_TOKEN / vendor key via the canvas Secrets tab.
//
// Default-closed: if the resolver hits a DB error, falls back to
// platform_managed (the safe-default behavior), so a transient DB failure
// during a secret write still rejects the bypass-list keys — fail safer not
// freer. This matches the resolver's documented contract.
func platformManagedLLMModeForWorkspace(c *gin.Context, workspaceID string) bool {
	ctx := c.Request.Context()
	// CTO 2026-06-12 (per-workspace BYOK SSOT) + CR (agent-researcher): block a
	// vendor-key write whenever it would co-mingle with platform billing. The
	// MODEL is checked FIRST: a platform-servable model (derives to the closed
	// `platform` provider; the proxy serves it and bills the platform) blocks
	// stray vendor-key co-storage REGARDLESS of any (possibly stale/incorrect)
	// billing override — a platform model must never host a co-stored vendor key.
	// Only a specific VENDOR model is a BYOK setup where the customer may write
	// their own key (INCLUDING the first key, before billing has derived byok).
	// For a vendor model, an explicit platform_managed override still forces the
	// block (the operator chose managed billing → a vendor key would co-mingle).
	runtime, model, authEnv := readWorkspaceDeriveInputs(ctx, workspaceID)
	manifest, err := providerRegistry()
	if err != nil || manifest == nil {
		log.Printf("secrets: provider registry unavailable for workspace=%s: %v (blocking vendor-key write for safety)", workspaceID, err)
		return true
	}
	provider, dErr := manifest.DeriveProvider(runtime, model, authEnv)
	if dErr != nil {
		// Unregistered / ambiguous / no model → cannot prove it's a vendor BYOK
		// setup; block (safe default, matches the create-time only-registered gate).
		return true
	}
	if provider.IsPlatform() {
		// Platform-servable model → block, even under a byok override.
		return true
	}
	// Vendor model: allow the key write UNLESS an explicit platform_managed
	// override forces managed billing (then a vendor key would co-mingle).
	if mode, ok, err := readWorkspaceBillingOverride(ctx, workspaceID); err == nil && ok {
		return strings.EqualFold(mode, LLMBillingModePlatformManaged)
	}
	return false
}

// rejectPlatformManagedDirectLLMBypassForWorkspace is the per-workspace
// successor to rejectPlatformManagedDirectLLMBypass (internal#691). The
// strip-list ONLY applies when this specific workspace resolves to
// platform_managed; byok/disabled workspaces can write their own vendor keys.
func rejectPlatformManagedDirectLLMBypassForWorkspace(c *gin.Context, workspaceID, key string) bool {
	if !platformManagedLLMModeForWorkspace(c, workspaceID) || !isPlatformManagedDirectLLMBypassKey(key) {
		return false
	}
	c.JSON(http.StatusBadRequest, gin.H{
		"error":        "direct vendor key writes are blocked for platform-managed workspaces; use MODEL/LLM_PROVIDER or the platform LLM proxy env instead, or set this workspace's billing mode to 'byok' via /admin/workspaces/:id/llm-billing-mode",
		"key":          key,
		"workspace_id": workspaceID,
	})
	return true
}

// callerWorkspaceID resolves the caller's workspace identity from the request.
// It prefers the authenticated bearer token (core#2584 hardening: never let an
// unsigned header override token-derived identity), then falls back to the
// X-Workspace-ID header for session/cookie callers and unauthenticated paths.
// Returns empty string when the caller cannot be identified (e.g. admin-token
// requests where the bearer is not a workspace token).
func callerWorkspaceID(c *gin.Context) string {
	// 1. Authenticated workspace bearer token — highest trust.
	tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
	if tok != "" {
		if wsID, err := wsauth.WorkspaceFromToken(c.Request.Context(), db.DB, tok); err == nil {
			return wsID
		}
	}
	// 2. Fallback: A2A proxy / canvas clients set this header for callers
	// that do not present a workspace bearer (session cookies, some proxy
	// paths). We do NOT honor this when a workspace token is present — that
	// closes the header-spoof path where a mismatched X-Workspace-ID could
	// suppress auto-restart for a non-self write.
	if callerID := c.GetHeader("X-Workspace-ID"); callerID != "" {
		return callerID
	}
	return ""
}

type SecretsHandler struct {
	restartFunc func(workspaceID string) // Optional: auto-restart after secret change
}

func NewSecretsHandler(restartFunc func(string)) *SecretsHandler {
	return &SecretsHandler{restartFunc: restartFunc}
}

// conciergeSelfSecretWriteBlocked (core#2566) reports whether a Set call
// from the concierge/agent surface (AdminAuth ADMIN_TOKEN) targeting the
// org's kind='platform' concierge workspace should be refused outright
// (self-DoS guard). Returns a non-empty reason when blocked.
//
// Threat model (core#2566, 2026-06-11 live incident):
//   - The concierge's management MCP authenticates with the tenant
//     ADMIN token. On a Set call against the concierge's own workspace
//     (kind='platform'), #2573's auto-restart skip *does* fire (skip 2
//     covers kind='platform') — but that guard is best-effort and there
//     is a second, earlier failure path: the secret write itself
//     triggers env-var reload side effects inside the live container
//     mid-turn, and any path that later invokes a restart (operator
//     click, restart-on-failure watchdog, the next Set/Delete) tears
//     the concierge down. The org root going offline has already cost
//     a multi-hour outage once.
//   - Approval gating (#2574) is necessary but NOT sufficient: an
//     approval can be issued by a sleepy operator, and the concierge
//     consuming it still self-DoSes. The only safe posture is to
//     refuse the self-targeted write and force a human to apply the
//     change through a non-agent path (canvas Secrets tab, operator
//     session with explicit operator-action audit).
//
// Scope: AdminAuth admin-token callers ONLY. Session-cookie
// (cp_session_actor) and ordinary workspace-token callers are NOT
// blocked — they are human operators / non-concierge agents and may
// legitimately need to write the concierge's own secrets.
//
// Fail-closed: if the kind lookup errors, we refuse the write (and
// log) — a wrongly-fired write on the org root is exactly the outage
// this guard exists to prevent, while a refused write is just a retry
// after the DB hiccup clears.
func conciergeSelfSecretWriteBlocked(c *gin.Context, targetWorkspaceID string) (bool, string) {
	if !callerIsAdminToken(c) {
		return false, ""
	}
	ctx := c.Request.Context()
	var kind string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(kind, 'workspace') FROM workspaces WHERE id = $1`, targetWorkspaceID).
		Scan(&kind); err != nil {
		// Fail closed — see doc comment.
		log.Printf("secrets: blocking admin-token set_workspace_secret on %s (kind lookup failed, fail-closed: %v)", targetWorkspaceID, err)
		return true, "self-DoS guard: workspace kind lookup failed; refusing to write to a workspace whose kind cannot be verified"
	}
	if kind == "platform" {
		return true, "concierge cannot set_workspace_secret on its own platform-root workspace (self-DoS guard, core#2566); apply this secret through the canvas Secrets tab as a human operator"
	}
	return false, ""
}

// autoRestartAllowed (core#2573) decides whether a secret change on
// workspaceID may fire the auto-restart. Two skips:
//
//  1. Self-write: the caller IS the target workspace -- restarting would
//     kill the writing agent mid-turn (original #2573 fix). Only covers
//     callers that present a workspace token / X-Workspace-ID, which the
//     concierge's management MCP does NOT (it authenticates with the
//     tenant ADMIN token, so callerID is "" and skip 1 never fired --
//     that gap terminated the org root's box twice on 2026-06-11, once
//     costing a 14h outage).
//  2. Platform root: the target is the org's kind='platform' concierge.
//     An auto-restart here tears down the ORG ROOT's EC2 (terminate +
//     re-provision, minutes of downtime, and the provision leg has
//     failed silently before -- cp#691). The concierge picks changes up
//     on its next explicit restart; the canvas Restart button covers
//     operators who need it now.
func autoRestartAllowed(ctx context.Context, callerID, workspaceID string) bool {
	if callerID == workspaceID {
		log.Printf("secrets: skipping auto-restart of %s (self-write, core#2573)", workspaceID)
		return false
	}
	var kind string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(kind, 'workspace') FROM workspaces WHERE id = $1`, workspaceID).
		Scan(&kind); err != nil {
		// Fail closed: if we cannot prove the target is a regular
		// workspace, do NOT restart it -- a wrongly-fired restart on the
		// org root is exactly the outage this guard exists to prevent,
		// while a skipped restart just delays env propagation until the
		// next explicit restart.
		log.Printf("secrets: skipping auto-restart of %s (kind lookup failed, fail-closed: %v)", workspaceID, err)
		return false
	}
	if kind == "platform" {
		log.Printf("secrets: skipping auto-restart of %s (platform root, core#2573 -- restart explicitly to apply)", workspaceID)
		return false
	}
	return true
}

// List handles GET /workspaces/:id/secrets
// Returns a merged view: workspace-level overrides + inherited global secrets.
// Each entry includes a "scope" field ("workspace" or "global") so the frontend
// can distinguish overrides from inherited defaults. Never exposes values.
func (h *SecretsHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	if !uuidRegex.MatchString(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}
	ctx := c.Request.Context()

	// 1. Workspace-level secrets
	wsKeys := map[string]bool{}
	secrets := make([]map[string]interface{}, 0)

	rows, err := db.DB.QueryContext(ctx,
		`SELECT key, created_at, updated_at FROM workspace_secrets WHERE workspace_id = $1 ORDER BY key`,
		workspaceID)
	if err != nil {
		log.Printf("List secrets error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	for rows.Next() {
		var key, createdAt, updatedAt string
		if err := rows.Scan(&key, &createdAt, &updatedAt); err != nil {
			continue
		}
		wsKeys[key] = true
		secrets = append(secrets, map[string]interface{}{
			"key":        key,
			"has_value":  true,
			"scope":      "workspace",
			"created_at": createdAt,
			"updated_at": updatedAt,
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("List workspace secrets iteration error: %v", err)
	}

	// 2. Global secrets not overridden at workspace level
	globalRows, err := db.DB.QueryContext(ctx,
		`SELECT key, created_at, updated_at FROM global_secrets ORDER BY key`)
	if err != nil {
		log.Printf("List global secrets (merged) error: %v", err)
		// Non-fatal: return workspace secrets only
		c.JSON(http.StatusOK, secrets)
		return
	}
	defer globalRows.Close()

	for globalRows.Next() {
		var key, createdAt, updatedAt string
		if err := globalRows.Scan(&key, &createdAt, &updatedAt); err != nil {
			continue
		}
		if wsKeys[key] {
			continue // workspace override exists — skip global
		}
		secrets = append(secrets, map[string]interface{}{
			"key":        key,
			"has_value":  true,
			"scope":      "global",
			"created_at": createdAt,
			"updated_at": updatedAt,
		})
	}
	if err := globalRows.Err(); err != nil {
		log.Printf("List global secrets iteration error: %v", err)
	}

	c.JSON(http.StatusOK, secrets)
}

// Values handles GET /workspaces/:id/secrets/values — returns the merged
// decrypted secrets as a flat `{"KEY": "value"}` JSON map so remote agents
// can pull their secrets on startup instead of having them pushed at
// container-create time. Phase 30.2.
//
// Authentication: the workspace must present its own Phase 30.1 auth token
// in `Authorization: Bearer …`. Legacy workspaces with no live token on file
// are grandfathered through (same lazy-bootstrap contract as
// /registry/heartbeat) so in-flight workspaces keep working during the
// rollout. Anything else → 401.
//
// The same merge rule as List applies: workspace secrets override globals
// with the same key. Values are returned verbatim (no base64, no JSON
// escaping beyond the standard), matching the env-var shape the provisioner
// would have injected at container-create.
func (h *SecretsHandler) Values(c *gin.Context) {
	workspaceID := c.Param("id")
	if !uuidRegex.MatchString(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}
	ctx := c.Request.Context()

	// Auth gate (Phase 30.1/30.2): enforce the bearer token when the
	// workspace has any live token on file. Grandfather legacy workspaces
	// through so a rolling upgrade doesn't lock them out.
	hasLive, hlErr := wsauth.HasAnyLiveToken(ctx, db.DB, workspaceID)
	if hlErr != nil {
		// DB hiccup checking token existence — the handler's security
		// posture is "fail closed" here because unlike heartbeat, we're
		// about to return plaintext secrets. Heartbeat can safely
		// fail-open because it only reports state.
		log.Printf("wsauth: HasAnyLiveToken(%s) failed for secrets.Values: %v", workspaceID, hlErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth check failed"})
		return
	}
	if hasLive {
		tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
		if tok == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing workspace auth token"})
			return
		}
		if err := wsauth.ValidateToken(ctx, db.DB, workspaceID, tok); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid workspace auth token"})
			return
		}
	}

	// Merged secrets: globals first, then workspace overrides (same as
	// provisioner path in workspace_provision.go so env-vars look identical
	// whether the workspace was bootstrapped locally or remotely).
	out := map[string]string{}
	// Track decrypt failures so we can refuse the response with a list
	// instead of returning a partial bundle that boots a broken agent.
	var failedKeys []string

	globalRows, gErr := db.DB.QueryContext(ctx,
		`SELECT key, encrypted_value, encryption_version FROM global_secrets`)
	if gErr == nil {
		defer globalRows.Close()
		for globalRows.Next() {
			var k string
			var v []byte
			var ver int
			if globalRows.Scan(&k, &v, &ver) == nil {
				decrypted, decErr := crypto.DecryptVersioned(v, ver)
				if decErr != nil {
					// Fail-loud (mirrors workspace_provision.go's posture):
					// a remote agent that boots with only PART of its secrets
					// will fail at task time with mysterious KeyErrors. Better
					// to refuse to serve the bundle and force the operator to
					// rotate the broken key.
					log.Printf("secrets.Values: decrypt global %s failed (version=%d): %v", k, ver, decErr)
					failedKeys = append(failedKeys, "global:"+k)
					continue
				}
				out[k] = string(decrypted)
			}
		}
		if err := globalRows.Err(); err != nil {
			log.Printf("secrets.Values: global rows iteration error: %v", err)
		}
	}

	wsRows, wErr := db.DB.QueryContext(ctx,
		`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1`,
		workspaceID)
	if wErr == nil {
		defer wsRows.Close()
		for wsRows.Next() {
			var k string
			var v []byte
			var ver int
			if wsRows.Scan(&k, &v, &ver) == nil {
				decrypted, decErr := crypto.DecryptVersioned(v, ver)
				if decErr != nil {
					log.Printf("secrets.Values: decrypt workspace %s failed (version=%d): %v", k, ver, decErr)
					failedKeys = append(failedKeys, "workspace:"+k)
					continue
				}
				out[k] = string(decrypted) // workspace override wins over global
			}
		}
		if err := wsRows.Err(); err != nil {
			log.Printf("secrets.Values: workspace rows iteration error: %v", err)
		}
	}

	if len(failedKeys) > 0 {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":       "one or more secrets failed to decrypt; refusing to return partial bundle",
			"failed_keys": failedKeys,
		})
		return
	}

	// molecule-core#1994 (corrected model): the remote-pull bundle is the
	// TENANT's own merged secrets (global_secrets + workspace_secrets, the
	// latter winning on collision). `global_secrets` is the tenant's store, not
	// the platform's, so a byok workspace's pull MUST include the tenant's own
	// global-scope LLM credential — that is exactly what it runs on, direct.
	// The earlier internal#711 byok strip here rested on the inverted "global =
	// platform's own" premise and is removed; the platform's own proxy token is
	// never in a tenant's global_secrets (it lives in server env only and is
	// injected separately on the platform_managed provision path), so there is
	// nothing platform-owned to withhold on this path.
	c.JSON(http.StatusOK, out)
}

// Set handles POST /workspaces/:id/secrets
func (h *SecretsHandler) Set(c *gin.Context) {
	workspaceID := c.Param("id")
	if !uuidRegex.MatchString(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}
	ctx := c.Request.Context()

	var body struct {
		Key   string `json:"key" binding:"required"`
		Value string `json:"value" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if rejectPlatformManagedDirectLLMBypassForWorkspace(c, workspaceID, body.Key) {
		return
	}

	// core#2566: refuse concierge/agent self-targeted secret writes on the
	// org-root (kind='platform') workspace. Fires BEFORE the approval gate so
	// a refused write does not even create a pending approval — the operator
	// is told to apply the change through the canvas Secrets tab instead, and
	// the audit trail records the refused attempt.
	if blocked, reason := conciergeSelfSecretWriteBlocked(c, workspaceID); blocked {
		log.Printf("secrets: refusing admin-token set_workspace_secret on %s key=%s (core#2566 self-DoS guard): %s", workspaceID, body.Key, reason)
		c.JSON(http.StatusForbidden, gin.H{
			"error":        reason,
			"workspace_id": workspaceID,
			"key":          body.Key,
			"code":         "CONCIERGE_SELF_WRITE_BLOCKED",
		})
		audit.Emit(c.Request.Context(), "secret.set.refused", map[string]any{
			"workspace_id": workspaceID,
			"key":          body.Key,
			"reason":       "concierge_self_write_blocked",
			"issue":        "core#2566",
		})
		return
	}

	// RFC platform-agent Phase 4b: gate org-token (platform-agent) secret writes
	// behind human approval. The context includes the key so an approval for one
	// secret cannot authorise writing another. No-op for ordinary callers and
	// when the rollout flag is off (scoping lives in gateDestructive).
	// SecretsHandler has no broadcaster, so pass nil — requireApproval persists
	// the pending row regardless; only the live canvas push is skipped.
	if !gateDestructive(c, nil, workspaceID, approvals.ActionSecretWrite,
		"write secret "+body.Key,
		map[string]interface{}{"workspace_id": workspaceID, "key": body.Key}) {
		return
	}

	// Encrypt the value (AES-256-GCM if SECRETS_ENCRYPTION_KEY is set, plaintext otherwise)
	encrypted, err := crypto.Encrypt([]byte(body.Value))
	if err != nil {
		log.Printf("Encrypt secret error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt secret"})
		return
	}

	// Persist encryption_version alongside the bytes (#85). ON CONFLICT
	// also rewrites the version — re-setting a secret while encryption
	// is enabled upgrades a historical plaintext row to AES-GCM.
	version := crypto.CurrentEncryptionVersion()
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workspace_id, key) DO UPDATE
			SET encrypted_value = $3, encryption_version = $4, updated_at = now()
	`, workspaceID, body.Key, encrypted, version)
	if err != nil {
		log.Printf("Set secret error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save secret"})
		return
	}

	// Phase 1 audit: structured event for the security trail. Inline (not
	// goroutine) so the event is durable before we ack the user; emit is
	// best-effort and never errors out of the request path.
	audit.Emit(c.Request.Context(), "secret.set", map[string]any{
		"workspace_id": workspaceID,
		"key":          body.Key,
		"value_hash":   audit.HashValuePrefix(body.Value, 8),
		"scope":        "workspace",
		"operation":    "set",
	})

	// Auto-restart workspace to pick up new secret.
	// RFC internal#524 Layer 1: route through globalGoAsync so tests can
	// drain the detached restart goroutine before db.DB is swapped — see
	// drainTestAsync in handlers_test.go and the canonical 69d9b4e3 fix.
	//
	// #2573: skip the auto-restart for self-writes AND for the platform
	// root (see autoRestartAllowed for the full rationale).
	callerID := callerWorkspaceID(c)
	if h.restartFunc != nil && autoRestartAllowed(c.Request.Context(), callerID, workspaceID) {
		wsID := workspaceID
		globalGoAsync(func() { h.restartFunc(wsID) })
	}

	c.JSON(http.StatusOK, gin.H{"status": "saved", "key": body.Key})
}

// Delete handles DELETE /workspaces/:id/secrets/:key
func (h *SecretsHandler) Delete(c *gin.Context) {
	workspaceID := c.Param("id")
	if !uuidRegex.MatchString(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}
	key := c.Param("key")
	ctx := c.Request.Context()

	result, err := db.DB.ExecContext(ctx,
		`DELETE FROM workspace_secrets WHERE workspace_id = $1 AND key = $2`,
		workspaceID, key)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete secret"})
		return
	}

	rows, err := result.RowsAffected()
	if err != nil {
		log.Printf("DeleteWorkspace: RowsAffected error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete secret"})
		return
	}
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "secret not found"})
		return
	}

	// Phase 1 audit: structured event for the security trail. Only on
	// real deletes (rows>0) — a 404 is not a state change.
	audit.Emit(c.Request.Context(), "secret.delete", map[string]any{
		"workspace_id": workspaceID,
		"key":          key,
		"scope":        "workspace",
		"operation":    "delete",
	})

	// Auto-restart workspace to pick up removed secret.
	// RFC internal#524 Layer 1: see Set() above for the drain rationale.
	// #2573: skip the auto-restart for self-writes AND for the platform
	// root (see autoRestartAllowed).
	callerID := callerWorkspaceID(c)
	if h.restartFunc != nil && autoRestartAllowed(c.Request.Context(), callerID, workspaceID) {
		wsID := workspaceID
		globalGoAsync(func() { h.restartFunc(wsID) })
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted", "key": key})
}

// ---------------------------------------------------------------------------
// Global secrets — platform-wide API keys that apply to all workspaces.
// Workspace-level secrets with the same key override globals.
// ---------------------------------------------------------------------------

// ListGlobal handles GET /admin/secrets
func (h *SecretsHandler) ListGlobal(c *gin.Context) {
	ctx := c.Request.Context()
	rows, err := db.DB.QueryContext(ctx,
		`SELECT key, created_at, updated_at FROM global_secrets ORDER BY key`)
	if err != nil {
		log.Printf("List global secrets error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	secrets := make([]map[string]interface{}, 0)
	for rows.Next() {
		var key, createdAt, updatedAt string
		if err := rows.Scan(&key, &createdAt, &updatedAt); err != nil {
			continue
		}
		secrets = append(secrets, map[string]interface{}{
			"key":        key,
			"has_value":  true,
			"created_at": createdAt,
			"updated_at": updatedAt,
			"scope":      "global",
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("ListGlobal iteration error: %v", err)
	}
	c.JSON(http.StatusOK, secrets)
}

// SetGlobal handles POST /admin/secrets
func (h *SecretsHandler) SetGlobal(c *gin.Context) {
	ctx := c.Request.Context()
	var body struct {
		Key   string `json:"key" binding:"required"`
		Value string `json:"value" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	// internal#718: the org-level LLM billing rung was retired — billing is
	// resolved per-workspace, not per-org. A global secret is the tenant's OWN
	// shared credential; the provision-time provider-matched strip
	// (workspace_provision) removes any global cred a given workspace's resolved
	// provider does not accept, so a platform-managed workspace can never USE a
	// non-matching global vendor/oauth key. The legacy org-env SetGlobal gate
	// (keyed off the retired MOLECULE_LLM_BILLING_MODE) is therefore removed;
	// per-workspace writes still enforce the strip-list via
	// rejectPlatformManagedDirectLLMBypassForWorkspace.

	encrypted, err := crypto.Encrypt([]byte(body.Value))
	if err != nil {
		log.Printf("Encrypt global secret error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt"})
		return
	}

	globalVersion := crypto.CurrentEncryptionVersion()
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO global_secrets (key, encrypted_value, encryption_version)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE
			SET encrypted_value = $2, encryption_version = $3, updated_at = now()
	`, body.Key, encrypted, globalVersion)
	if err != nil {
		log.Printf("Set global secret error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save"})
		return
	}

	// Issue #15: global secrets are injected into containers as env vars at
	// Start() time, so a rotating token (e.g. CLAUDE_CODE_OAUTH_TOKEN) doesn't
	// reach existing workspaces until the container is recreated. Auto-restart
	// every workspace whose env is affected — i.e. those WITHOUT a
	// workspace-level override of the same key.
	//
	// RFC internal#524 Layer 1: globalGoAsync so tests drain the fan-out
	// (which itself spawns N more globalGoAsync restart calls below) before
	// db.DB swap. Without this, the SELECT for affected workspaces races a
	// subsequent test's db.DB restore.
	//
	// #2573: pass caller workspace ID so the writing agent doesn't restart
	// itself mid-turn when it sets a global secret.
	key := body.Key
	callerID := callerWorkspaceID(c)
	globalGoAsync(func() { h.restartAllAffectedByGlobalKey(key, callerID) })

	// Phase 1 audit: admin-scope secret write — high-value security event.
	auditCtx := audit.WithActorKind(c.Request.Context(), audit.ActorAdmin)
	audit.Emit(auditCtx, "secret.set", map[string]any{
		"key":        body.Key,
		"value_hash": audit.HashValuePrefix(body.Value, 8),
		"scope":      "global",
		"operation":  "set",
	})

	c.JSON(http.StatusOK, gin.H{"status": "saved", "key": body.Key, "scope": "global"})
}

// restartAllAffectedByGlobalKey restarts every non-paused, non-removed
// workspace that would inherit the given global-secret key (i.e. does NOT
// have a workspace-level override). Used on SetGlobal / DeleteGlobal so
// rotated credentials (OAuth tokens, API keys) propagate without a manual
// restart loop. See issue #15.
//
// #2573: excludeWorkspaceID prevents the writing agent's own workspace from
// being restarted mid-turn when the org platform agent sets a global secret.
func (h *SecretsHandler) restartAllAffectedByGlobalKey(key, excludeWorkspaceID string) {
	if h.restartFunc == nil {
		return
	}
	ctx := context.Background()
	// core#2573: the org's kind='platform' root is excluded for the same
	// reason autoRestartAllowed skips it -- an auto-restart terminates the
	// org root's box. It picks up rotated globals on its next explicit
	// restart.
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id FROM workspaces
		WHERE status NOT IN ('removed', 'paused')
		  AND COALESCE(runtime, '') <> 'external'
		  AND COALESCE(kind, 'workspace') <> 'platform'
		  AND id NOT IN (
		      SELECT workspace_id FROM workspace_secrets WHERE key = $1
		  )
	`, key)
	if err != nil {
		log.Printf("Global secret %s: failed to list affected workspaces for auto-restart: %v", key, err)
		return
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			if id == excludeWorkspaceID {
				continue
			}
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("restartAllAffectedByGlobalKey: iteration error: %v", err)
	}
	if len(ids) == 0 {
		return
	}
	log.Printf("Global secret %s changed: auto-restarting %d workspace(s) to refresh env", key, len(ids))
	for _, id := range ids {
		// RFC internal#524 Layer 1: per-workspace restart via globalGoAsync
		// so each restart goroutine is drained before db.DB is swapped in
		// the test cleanup chain.
		wsID := id
		globalGoAsync(func() { h.restartFunc(wsID) })
	}
}

// DeleteGlobal handles DELETE /admin/secrets/:key
func (h *SecretsHandler) DeleteGlobal(c *gin.Context) {
	key := c.Param("key")
	ctx := c.Request.Context()

	result, err := db.DB.ExecContext(ctx,
		`DELETE FROM global_secrets WHERE key = $1`, key)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete"})
		return
	}

	rows, err := result.RowsAffected()
	if err != nil {
		log.Printf("DeleteGlobal: RowsAffected error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete"})
		return
	}
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "secret not found"})
		return
	}

	// Issue #15: propagate deletion to running containers — otherwise they
	// keep the stale env var until manual restart.
	// RFC internal#524 Layer 1: globalGoAsync for the same drain rationale
	// as SetGlobal above.
	// #2573: pass caller workspace ID so the writing agent doesn't restart
	// itself mid-turn when it deletes a global secret.
	k := key
	callerID := callerWorkspaceID(c)
	globalGoAsync(func() { h.restartAllAffectedByGlobalKey(k, callerID) })

	// Phase 1 audit: admin-scope secret delete.
	auditCtx := audit.WithActorKind(c.Request.Context(), audit.ActorAdmin)
	audit.Emit(auditCtx, "secret.delete", map[string]any{
		"key":       key,
		"scope":     "global",
		"operation": "delete",
	})

	c.JSON(http.StatusOK, gin.H{"status": "deleted", "key": key, "scope": "global"})
}

// GetModel handles GET /workspaces/:id/model
// Returns the current model configuration for a workspace.
func (h *SecretsHandler) GetModel(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Check if MODEL secret exists.
	//
	// Historical note: this row was named MODEL_PROVIDER pre-2026-05-19
	// (see ab12af50 + a7e8892 root-cause analysis). The column name
	// MODEL_PROVIDER was misleading — it never held a provider slug,
	// only the picked model id (e.g. "minimax/MiniMax-M2.7"). The
	// misnomer caused workspace-server's applyRuntimeModelEnv to
	// overwrite a legitimate persona-env MODEL with whatever literal
	// string lived in MODEL_PROVIDER (often "minimax" or "claude-code"
	// — not a valid model id), wedging adapters at SDK initialize.
	// CP-side slot-separation (cp#213 + cp#220) already corrected the
	// CP-side analogue; this is the workspace-server companion. A
	// migration in 20260519000000_workspace_secrets_model_provider_rename.up.sql
	// moves any legacy rows to the new key on rollout.
	var modelBytes []byte
	var modelVersion int
	err := db.DB.QueryRowContext(ctx,
		`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = 'MODEL'`,
		workspaceID).Scan(&modelBytes, &modelVersion)
	if err == sql.ErrNoRows {
		// core#2594: no stored model. Report source "unresolved" — NOT "default":
		// the platform no longer provides a default model (the
		// MOLECULE_LLM_DEFAULT_MODEL fail-open was removed), so an empty MODEL
		// secret means the effective model is genuinely unresolved and the
		// workspace will fail closed at provision rather than run an opaque
		// default. "default" implied a silent fallback that no longer exists and
		// misled the canvas Config tab into showing a blank as if intentional.
		// (A correctly-provisioned concierge stores its declared model, so this
		// branch is the genuine "model not set" state, surfaced truthfully.)
		c.JSON(http.StatusOK, gin.H{"model": "", "source": "unresolved"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	decrypted, err := crypto.DecryptVersioned(modelBytes, modelVersion)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"model": string(decrypted), "source": "workspace_secrets"})
}

// setModelSecret writes (or clears, when value=="") the MODEL workspace
// secret. Extracted from SetModel so non-handler call sites (notably
// WorkspaceHandler.Create — first-deploy path that persists the
// canvas-selected model so applyRuntimeModelEnv's restart fallback finds
// it) can reuse the encryption + upsert logic without inlining the SQL.
//
// The row was previously keyed MODEL_PROVIDER (misnomer — it never held
// a provider, only a model id). Renamed to MODEL on 2026-05-19; the
// 20260519000000_workspace_secrets_model_provider_rename migration moves
// any legacy rows on rollout.
//
// Returns nil on success. Caller is responsible for any restart trigger;
// the gin handler re-adds that after a successful write.
func setModelSecret(ctx context.Context, workspaceID, model string) error {
	if model == "" {
		_, err := db.DB.ExecContext(ctx,
			`DELETE FROM workspace_secrets WHERE workspace_id = $1 AND key = 'MODEL'`,
			workspaceID)
		return err
	}
	encrypted, err := crypto.Encrypt([]byte(model))
	if err != nil {
		return err
	}
	version := crypto.CurrentEncryptionVersion()
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
		VALUES ($1, 'MODEL', $2, $3)
		ON CONFLICT (workspace_id, key) DO UPDATE
			SET encrypted_value = $2, encryption_version = $3, updated_at = now()
	`, workspaceID, encrypted, version)
	return err
}

// SetModel handles PUT /workspaces/:id/model — writes the model slug
// into workspace_secrets as MODEL (the key GetModel reads).
// For hermes, the value is a hermes-native slug like "minimax/MiniMax-M2.7";
// for claude-code it's the legacy "provider:model" form. Either way it's just
// an opaque string the runtime interprets on its next start.
//
// Empty string clears the override. Triggers auto-restart so the new
// env (HERMES_DEFAULT_MODEL etc.) takes effect immediately — without
// this the user clicks Save+Restart, the canvas PUT lands, but the
// already-restarting container misses the window and boots with the
// old value.
func (h *SecretsHandler) SetModel(c *gin.Context) {
	workspaceID := c.Param("id")
	if !uuidRegex.MatchString(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}
	ctx := c.Request.Context()

	var body struct {
		Model string `json:"model"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// issue #2172: validate the model against the registry before persisting.
	// Empty model clears the override — skip validation (MODEL_REQUIRED owns
	// the empty case at create time; clearing is always allowed).
	if body.Model != "" {
		var runtime string
		if err := db.DB.QueryRowContext(ctx,
			`SELECT runtime FROM workspaces WHERE id = $1`, workspaceID,
		).Scan(&runtime); err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
				return
			}
			log.Printf("SetModel: runtime lookup failed for %s: %v", workspaceID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read workspace runtime"})
			return
		}
		if ok, why := validateRegisteredModelForRuntime(runtime, body.Model); !ok {
			log.Printf("SetModel: 422 UNREGISTERED_MODEL_FOR_RUNTIME (runtime=%q model=%q): %s", runtime, body.Model, why)
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":   why,
				"runtime": runtime,
				"model":   body.Model,
				"code":    "UNREGISTERED_MODEL_FOR_RUNTIME",
			})
			return
		}
		if ok, why := validateDerivedProviderInRegistry(runtime, body.Model); !ok {
			log.Printf("SetModel: 422 DERIVED_PROVIDER_NOT_IN_REGISTRY (runtime=%q model=%q): %s", runtime, body.Model, why)
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":   why,
				"runtime": runtime,
				"model":   body.Model,
				"code":    "DERIVED_PROVIDER_NOT_IN_REGISTRY",
			})
			return
		}
	}

	if err := setModelSecret(ctx, workspaceID, body.Model); err != nil {
		log.Printf("SetModel error: %v", err)
		if body.Model == "" {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to clear model"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save model"})
		}
		return
	}

	if h.restartFunc != nil {
		// RFC internal#524 Layer 1: globalGoAsync (see Set()).
		wsID := workspaceID
		globalGoAsync(func() { h.restartFunc(wsID) })
	}
	if body.Model == "" {
		c.JSON(http.StatusOK, gin.H{"status": "cleared"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "saved", "model": body.Model})
}

// internal#718 P4 closure: GetProvider, SetProvider, and the shared
// setProviderSecret helper were retired together with the
// LLM_PROVIDER workspace_secret. The provider is now DERIVED at every
// decision point from (runtime, model) via the registry
// (internal/providers.Manifest.DeriveProvider), so storing it is
// pure write-ghost — no consumer remains.
//
// Route registrations in internal/router/router.go now point both
// GET and PUT /workspaces/:id/provider at providerEndpointGone, which
// returns 410 Gone with a structured body so older canvases that
// still call PUT /provider on Save surface a loud failure rather
// than silently writing a vanished row.
//
// Migration 20260528000000_drop_llm_provider_workspace_secret.up.sql
// removes any straggler rows in workspace_secrets (key='LLM_PROVIDER')
// so the table is in the same state as a freshly-provisioned tenant.
