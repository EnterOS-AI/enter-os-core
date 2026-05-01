package handlers

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"regexp"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/crypto"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
	"github.com/gin-gonic/gin"
)

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type SecretsHandler struct {
	restartFunc func(workspaceID string) // Optional: auto-restart after secret change
}

func NewSecretsHandler(restartFunc func(string)) *SecretsHandler {
	return &SecretsHandler{restartFunc: restartFunc}
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
	}

	if len(failedKeys) > 0 {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":       "one or more secrets failed to decrypt; refusing to return partial bundle",
			"failed_keys": failedKeys,
		})
		return
	}

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

	// Auto-restart workspace to pick up new secret
	if h.restartFunc != nil {
		go h.restartFunc(workspaceID)
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
	}
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "secret not found"})
		return
	}

	// Auto-restart workspace to pick up removed secret
	if h.restartFunc != nil {
		go h.restartFunc(workspaceID)
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
	go h.restartAllAffectedByGlobalKey(body.Key)

	c.JSON(http.StatusOK, gin.H{"status": "saved", "key": body.Key, "scope": "global"})
}

// restartAllAffectedByGlobalKey restarts every non-paused, non-removed
// workspace that would inherit the given global-secret key (i.e. does NOT
// have a workspace-level override). Used on SetGlobal / DeleteGlobal so
// rotated credentials (OAuth tokens, API keys) propagate without a manual
// restart loop. See issue #15.
func (h *SecretsHandler) restartAllAffectedByGlobalKey(key string) {
	if h.restartFunc == nil {
		return
	}
	ctx := context.Background()
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id FROM workspaces
		WHERE status NOT IN ('removed', 'paused')
		  AND COALESCE(runtime, '') <> 'external'
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
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	log.Printf("Global secret %s changed: auto-restarting %d workspace(s) to refresh env", key, len(ids))
	for _, id := range ids {
		go h.restartFunc(id)
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
	}
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "secret not found"})
		return
	}

	// Issue #15: propagate deletion to running containers — otherwise they
	// keep the stale env var until manual restart.
	go h.restartAllAffectedByGlobalKey(key)

	c.JSON(http.StatusOK, gin.H{"status": "deleted", "key": key, "scope": "global"})
}

// GetModel handles GET /workspaces/:id/model
// Returns the current model configuration for a workspace.
func (h *SecretsHandler) GetModel(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Check if MODEL_PROVIDER secret exists
	var modelBytes []byte
	var modelVersion int
	err := db.DB.QueryRowContext(ctx,
		`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = 'MODEL_PROVIDER'`,
		workspaceID).Scan(&modelBytes, &modelVersion)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusOK, gin.H{"model": "", "source": "default"})
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

// SetModel handles PUT /workspaces/:id/model — writes the model slug
// into workspace_secrets as MODEL_PROVIDER (the key GetModel reads).
// For hermes, the value is a hermes-native slug like "minimax/MiniMax-M2.7";
// for langgraph it's the legacy "provider:model" form. Either way it's just
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

	if body.Model == "" {
		if _, err := db.DB.ExecContext(ctx,
			`DELETE FROM workspace_secrets WHERE workspace_id = $1 AND key = 'MODEL_PROVIDER'`,
			workspaceID); err != nil {
			log.Printf("SetModel delete error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to clear model"})
			return
		}
		if h.restartFunc != nil {
			go h.restartFunc(workspaceID)
		}
		c.JSON(http.StatusOK, gin.H{"status": "cleared"})
		return
	}

	encrypted, err := crypto.Encrypt([]byte(body.Model))
	if err != nil {
		log.Printf("SetModel encrypt error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt model"})
		return
	}
	version := crypto.CurrentEncryptionVersion()
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
		VALUES ($1, 'MODEL_PROVIDER', $2, $3)
		ON CONFLICT (workspace_id, key) DO UPDATE
			SET encrypted_value = $2, encryption_version = $3, updated_at = now()
	`, workspaceID, encrypted, version)
	if err != nil {
		log.Printf("SetModel upsert error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save model"})
		return
	}

	if h.restartFunc != nil {
		go h.restartFunc(workspaceID)
	}
	c.JSON(http.StatusOK, gin.H{"status": "saved", "model": body.Model})
}

// GetProvider handles GET /workspaces/:id/provider
// Returns the explicit LLM provider override stored as the LLM_PROVIDER
// workspace secret. Mirror of GetModel — same shape, same response keys
// (provider/source) to keep canvas wiring symmetric.
//
// Why a sibling endpoint rather than overloading PUT /model: the new
// `provider` field (Option B, PR #2441) is orthogonal to the model
// slug. A user might keep the same model alias and switch providers
// (e.g., route the same alias through a different gateway), or keep
// the same provider and switch models. Co-storing them under one
// endpoint forces a single Save+Restart round-trip per change; two
// endpoints let the canvas update each independently.
func (h *SecretsHandler) GetProvider(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var bytesVal []byte
	var version int
	err := db.DB.QueryRowContext(ctx,
		`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = 'LLM_PROVIDER'`,
		workspaceID).Scan(&bytesVal, &version)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusOK, gin.H{"provider": "", "source": "default"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	decrypted, err := crypto.DecryptVersioned(bytesVal, version)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"provider": string(decrypted), "source": "workspace_secrets"})
}

// SetProvider handles PUT /workspaces/:id/provider — writes the provider
// slug into workspace_secrets as LLM_PROVIDER. Empty string clears the
// override. Triggers auto-restart so the new env is in effect on the
// next boot — without this the canvas Save+Restart can race the
// already-restarting container and miss the window.
//
// CP user-data (controlplane PR #364) reads LLM_PROVIDER from env and
// writes it into /configs/config.yaml at boot, so the choice survives
// restart. Without that PR this endpoint still works but the value is
// only sticky when the workspace_secrets row is read on every restart
// (the secret-load path) — slower failure mode, same eventual behavior.
func (h *SecretsHandler) SetProvider(c *gin.Context) {
	workspaceID := c.Param("id")
	if !uuidRegex.MatchString(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}
	ctx := c.Request.Context()

	var body struct {
		Provider string `json:"provider"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if body.Provider == "" {
		if _, err := db.DB.ExecContext(ctx,
			`DELETE FROM workspace_secrets WHERE workspace_id = $1 AND key = 'LLM_PROVIDER'`,
			workspaceID); err != nil {
			log.Printf("SetProvider delete error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to clear provider"})
			return
		}
		if h.restartFunc != nil {
			go h.restartFunc(workspaceID)
		}
		c.JSON(http.StatusOK, gin.H{"status": "cleared"})
		return
	}

	encrypted, err := crypto.Encrypt([]byte(body.Provider))
	if err != nil {
		log.Printf("SetProvider encrypt error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt provider"})
		return
	}
	version := crypto.CurrentEncryptionVersion()
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
		VALUES ($1, 'LLM_PROVIDER', $2, $3)
		ON CONFLICT (workspace_id, key) DO UPDATE
			SET encrypted_value = $2, encryption_version = $3, updated_at = now()
	`, workspaceID, encrypted, version)
	if err != nil {
		log.Printf("SetProvider upsert error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save provider"})
		return
	}

	if h.restartFunc != nil {
		go h.restartFunc(workspaceID)
	}
	c.JSON(http.StatusOK, gin.H{"status": "saved", "provider": body.Provider})
}
