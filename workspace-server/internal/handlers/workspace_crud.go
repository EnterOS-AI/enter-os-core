package handlers

// workspace_crud.go — workspace state queries, updates, deletion, and
// field validation. Covers State (polling endpoint), Update (PATCH),
// Delete (cascade + purge), and input validation helpers.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/approvals"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/crypto"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// State handles GET /workspaces/:id/state — minimal status payload for
// remote-agent polling (Phase 30.4). Returns `{status, paused, deleted,
// workspace_id}` so a remote agent can detect pause/resume/delete
// without needing WebSocket reachability from the platform.
//
// Auth: Phase 30.1 bearer token required when the workspace has any
// live token on file; legacy workspaces grandfathered. Uses the same
// fail-closed posture as secrets.Values — polling this cadence with
// unauth'd callers would be a trivial DoS / workspace-status-scanner
// otherwise.
//
// The endpoint is deliberately NOT merged with GET /workspaces/:id:
// that handler is optimized for canvas (returns config, agent_card,
// position, …) and is unauthenticated by design. State is the
// agent-machinery polling path — tight, token-gated, cache-friendly.
func (h *WorkspaceHandler) State(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Auth gate — same shape as secrets.Values (Phase 30.2). Fail-closed
	// on DB errors because the caller is about to poll this at ~60s
	// cadence; letting unauth'd callers through on a hiccup turns this
	// into a workspace-status scanner.
	hasLive, hlErr := wsauth.HasAnyLiveToken(ctx, db.DB, workspaceID)
	if hlErr != nil {
		log.Printf("wsauth: HasAnyLiveToken(%s) failed for workspace.State: %v", workspaceID, hlErr)
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

	var status string
	err := db.DB.QueryRowContext(ctx, `
		SELECT status
		FROM workspaces
		WHERE id = $1
	`, workspaceID).Scan(&status)
	if err == sql.ErrNoRows {
		// A deleted workspace row no longer exists — remote agent should
		// interpret 404 as "shut yourself down" (our pause path uses
		// status='removed' but keeps the row; a 404 here means the
		// workspace was hard-deleted out from under the agent).
		c.JSON(http.StatusNotFound, gin.H{
			"workspace_id": workspaceID,
			"deleted":      true,
		})
		return
	}
	if err != nil {
		log.Printf("workspace.State query error for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	// Two delete paths: hard-delete (sql.ErrNoRows above → 404) AND
	// soft-delete (status='removed' → also return 404 here so the SDK
	// doesn't have to remember "is it 200 with deleted=true OR 404 with
	// deleted=true?"). Same shape, same status code, same flag set.
	if status == "removed" {
		c.JSON(http.StatusNotFound, gin.H{
			"workspace_id": workspaceID,
			"status":       "removed",
			"deleted":      true,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"workspace_id": workspaceID,
		"status":       status,
		"paused":       status == "paused",
		"deleted":      false,
	})
}

// Update handles PATCH /workspaces/:id
func (h *WorkspaceHandler) Update(c *gin.Context) {
	id := c.Param("id")

	// #687: reject non-UUID IDs before hitting the DB.
	if err := validateWorkspaceID(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}

	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// #685/#688: validate string fields for length and injection safety.
	strField := func(key string) string {
		if v, ok := body[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	if err := validateWorkspaceFields(
		strField("name"), strField("role"), "" /*model not patchable*/, strField("runtime"),
	); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if wsDir, ok := body["workspace_dir"]; ok && wsDir != nil {
		if dirStr, isStr := wsDir.(string); isStr && dirStr != "" {
			if err := validateWorkspaceDir(dirStr); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace directory"})
				return
			}
		}
	}

	// Validate workspace_dir early so invalid paths are rejected before the
	// existence check (consistent with name/role/runtime validation above).
	if wsDir, ok := body["workspace_dir"]; ok {
		if wsDir != nil {
			if dirStr, isStr := wsDir.(string); isStr && dirStr != "" {
				if err := validateWorkspaceDir(dirStr); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace directory"})
					return
				}
			}
		}
	}
	var computeJSON string
	var newComputeProvider string // hoisted: drives the cloud-provider switch detection below
	computePatch := false
	if rawCompute, ok := body["compute"]; ok {
		computePatch = true
		if rawCompute == nil {
			computeJSON = "{}"
		} else {
			b, err := json.Marshal(rawCompute)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid compute config"})
				return
			}
			var compute models.WorkspaceCompute
			if err := json.Unmarshal(b, &compute); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid compute config"})
				return
			}
			if err := validateWorkspaceCompute(compute); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			newComputeProvider = compute.Provider
			encoded, err := workspaceComputeJSON(compute)
			if err != nil {
				log.Printf("Update compute encode error for %s: %v", id, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode compute config"})
				return
			}
			computeJSON = encoded
		}
	}

	ctx := c.Request.Context()

	// Auth is fully enforced at the router layer (WorkspaceAuth middleware, #680).
	// WorkspaceAuth validates that the caller holds a valid bearer token for this
	// specific workspace — no additional auth gate is needed here.

	// #120: guard — return 404 for nonexistent workspace IDs instead of
	// silently applying zero-row UPDATEs and returning 200.
	var exists bool
	if err := db.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = $1)`, id,
	).Scan(&exists); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	if name, ok := body["name"]; ok {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET name = $2, updated_at = now() WHERE id = $1`, id, name); err != nil {
			log.Printf("Update name error for %s: %v", id, err)
		}
	}
	if role, ok := body["role"]; ok {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET role = $2, updated_at = now() WHERE id = $1`, id, role); err != nil {
			log.Printf("Update role error for %s: %v", id, err)
		}
	}
	if tier, ok := body["tier"]; ok {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET tier = $2, updated_at = now() WHERE id = $1`, id, tier); err != nil {
			log.Printf("Update tier error for %s: %v", id, err)
		}
	}
	if parentID, ok := body["parent_id"]; ok {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET parent_id = $2, updated_at = now() WHERE id = $1`, id, parentID); err != nil {
			log.Printf("Update parent_id error for %s: %v", id, err)
		}
	}
	if collapsed, ok := body["collapsed"]; ok {
		// `collapsed` is the canvas UI-only flag that hides descendants
		// in the tree view (WorkspaceNode renders the parent as header-
		// only). It lives on canvas_layouts (005_canvas_layouts.sql),
		// not workspaces — UPSERT because workspaces created outside the
		// canvas flow (e.g. workspace_handler Create before a layout row
		// exists) may not have a canvas_layouts row yet.
		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO canvas_layouts (workspace_id, collapsed) VALUES ($1, $2)
			ON CONFLICT (workspace_id) DO UPDATE SET collapsed = EXCLUDED.collapsed
		`, id, collapsed); err != nil {
			log.Printf("Update collapsed error for %s: %v", id, err)
		}
	}
	needsRestart := false
	if runtime, ok := body["runtime"]; ok {
		// Reject non-string or unrecognized runtime values before the model-
		// compatibility check. Prevents template slugs such as "seo-agent"
		// (a claude-code template variant) from being persisted as a runtime,
		// which wedges the workspace on the next boot because no adapter
		// recognizes the pseudo-runtime. Matches the create-boundary's
		// knownRuntimes gate (workspace.go:427).
		runtimeStr, typeOK := runtime.(string)
		if !typeOK {
			log.Printf("Update: PATCH runtime on %s REJECTED (not a string)", id)
			c.JSON(http.StatusBadRequest, gin.H{"error": "runtime must be a string"})
			return
		}
		if !isKnownRuntime(runtimeStr) {
			log.Printf("Update: PATCH runtime=%q on %s REJECTED (unknown runtime)", runtimeStr, id)
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":   "unsupported workspace runtime",
				"runtime": runtimeStr,
				"code":    "RUNTIME_UNSUPPORTED",
			})
			return
		}
		// (runtime, model) compatibility validation (the new field a PATCH can
		// change). model is NOT patchable per the body whitelist above, so the
		// post-PATCH model is the workspace's CURRENT model — fetched here
		// rather than re-parsed from the body. The (newRuntime, currentModel)
		// pair is what the boot path will try to resolve; an unroutable pair
		// is rejected at the API boundary instead of wedging the agent at
		// boot. validateRegisteredModelForRuntime is the same SSOT the
		// create-boundary uses (workspace_crud.go create + llm_billing_mode
		// resolver); mirroring it here keeps the PATCH-runtime path consistent
		// and catches the drift surface CR2 found on the #21 review.
		// The CURRENT model lives in the MODEL workspace_secret (the SSOT that
		// GET /model + the boot path use), NOT the workspaces.model column.
		// Reading the column wedged this PATCH at 500 for workspaces whose
		// model is only in workspace_secrets (e.g. JRS: GET /model →
		// source:"workspace_secrets"). Read it the same way GetModel does.
		var currentModel string
		var mEnc []byte
		var mVer int
		mErr := db.DB.QueryRowContext(ctx,
			`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = 'MODEL'`, id,
		).Scan(&mEnc, &mVer)
		switch {
		case mErr == nil:
			if dec, derr := crypto.DecryptVersioned(mEnc, mVer); derr == nil {
				currentModel = string(dec)
			} else {
				log.Printf("Update runtime: decrypt MODEL secret for %s failed: %v", id, derr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt model secret"})
				return
			}
		case errors.Is(mErr, sql.ErrNoRows):
			// No stored model → unresolved. Skip the strict (runtime, model)
			// check; a genuinely missing model fails closed at boot, not here.
		default:
			log.Printf("Update runtime: read MODEL secret for %s: %v", id, mErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read model secret"})
			return
		}
		// Only enforce the (runtime, model) pair when we actually HAVE a model.
		if currentModel != "" {
			if ok, reason := validateRegisteredModelForRuntime(runtimeStr, currentModel); !ok {
				log.Printf("Update: PATCH runtime=%q on %s REJECTED (model=%q is not registered for that runtime): %s",
					runtimeStr, id, currentModel, reason)
				// 422 = syntactically valid PATCH body, but the (runtime, model)
				// pair is unroutable per the registry SSOT.
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": reason})
				return
			}
		}
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET runtime = $2, updated_at = now() WHERE id = $1`, id, runtimeStr); err != nil {
			log.Printf("Update runtime error for %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save runtime"})
			return
		}
		needsRestart = true
	}
	if wsDir, ok := body["workspace_dir"]; ok {
		// ValidateWorkspaceDir was already called above before the existence check;
		// the UPDATE itself is unconditional.
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET workspace_dir = $2, updated_at = now() WHERE id = $1`, id, wsDir); err != nil {
			log.Printf("Update workspace_dir error for %s: %v", id, err)
		}
		needsRestart = true
	}
	if computePatch {
		// Cloud-provider SWITCH (in-place): if the incoming provider differs from
		// the one currently stored, the existing box lives on the OLD cloud. We
		// MUST deprovision it on the OLD provider BEFORE overwriting compute —
		// otherwise the subsequent "Save & Restart" restart's provider-aware
		// deprovision (cpProv.Stop → resolveProvider reads compute->>'provider')
		// would target the NEW cloud and ORPHAN the old box (a silently-billing
		// leak). Cloud mode only (the local Docker provisioner has no cross-cloud
		// concept; provider stays "" there so this never fires). After this, the
		// canvas's restart provisions the box on the new cloud; its own Stop is a
		// safe no-op (the box is already gone).
		if h.cpProv != nil {
			var oldProvider sql.NullString
			err := db.DB.QueryRowContext(ctx, `SELECT compute->>'provider' FROM workspaces WHERE id = $1`, id).Scan(&oldProvider)
			// FAIL-CLOSED on the read. The earlier `err == nil` gate was fail-OPEN:
			// a transient/unexpected DB error here skipped the whole switch block and
			// fell through to the compute UPDATE — so during a real switch the later
			// provider-aware restart deprovision would target the NEW cloud and ORPHAN
			// the old box (silent billing, unrecoverable). We cannot tell whether this
			// is a cross-cloud switch without the old provider, so on any error other
			// than "no such row" we abort exactly like a failed deprovision: compute
			// untouched, old box still recoverable, user retries. (sql.ErrNoRows means
			// there is genuinely no prior box — nothing to orphan — so it's safe to
			// skip the switch and let the UPDATE proceed.)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.Printf("Update: provider-switch precheck for %s ABORTED — could not read current cloud provider (provider left unchanged): %v", id, err)
				c.JSON(http.StatusBadGateway, gin.H{"error": "could not read the current cloud provider; provider unchanged — please retry"})
				return
			}
			if err == nil && normalizeCloudProvider(oldProvider.String) != normalizeCloudProvider(newComputeProvider) {
				log.Printf("Update: cloud-provider switch for %s: %q -> %q; deprovisioning old box on old provider before overwriting compute",
					id, normalizeCloudProvider(oldProvider.String), normalizeCloudProvider(newComputeProvider))
				// Use the ERROR-returning variant and ABORT before overwriting
				// compute if the old-box deprovision fails. If we proceeded, the
				// old box would keep running on the OLD cloud while the row now
				// records the NEW provider+instance — stranding it with no DB
				// pointer (an UNRECOVERABLE cross-cloud orphan that no reconciler
				// can map back). Aborting leaves the row pointing at the
				// still-recoverable old box; the user can retry the switch. (The
				// restart paths' void cpStopWithRetry is fine there because the
				// box stays on the SAME cloud, so the provider record is unchanged
				// and a provider-scoped sweep can still find it.)
				if err := h.cpStopWithRetryErr(ctx, id, "provider-switch", false); err != nil {
					log.Printf("Update: provider-switch for %s ABORTED — could not deprovision old box on %q (provider left unchanged, old box recoverable): %v",
						id, normalizeCloudProvider(oldProvider.String), err)
					c.JSON(http.StatusBadGateway, gin.H{"error": "could not deprovision the current cloud box; provider unchanged — please retry"})
					return
				}
			}
		}
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET compute = $2::jsonb, updated_at = now() WHERE id = $1`, id, computeJSON); err != nil {
			log.Printf("Update compute error for %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save compute config"})
			return
		}
		needsRestart = true
	}
	// NOTE: budget_limit is intentionally NOT handled here. The dedicated
	// PATCH /workspaces/:id/budget (AdminAuth) is the only write path.
	// This endpoint uses ValidateAnyToken — any enrolled workspace bearer
	// could otherwise self-clear its own spending ceiling. (#611 Security Auditor)

	// Update canvas position if both x and y provided
	if x, xOk := body["x"]; xOk {
		if y, yOk := body["y"]; yOk {
			if _, err := db.DB.ExecContext(ctx, `
				INSERT INTO canvas_layouts (workspace_id, x, y)
				VALUES ($1, $2, $3)
				ON CONFLICT (workspace_id) DO UPDATE SET x = EXCLUDED.x, y = EXCLUDED.y
			`, id, x, y); err != nil {
				log.Printf("Update position error for %s: %v", id, err)
			}
		}
	}

	resp := gin.H{"status": "updated"}
	if needsRestart {
		resp["needs_restart"] = true
	}
	c.JSON(http.StatusOK, resp)
}

// validateWorkspaceDir checks that a workspace_dir path is safe to bind-mount.
func validateWorkspaceDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("workspace_dir must be an absolute path")
	}
	if strings.Contains(dir, "..") {
		return fmt.Errorf("workspace_dir must not contain '..'")
	}
	// Reject system-critical paths
	clean := filepath.Clean(dir)
	for _, blocked := range []string{"/etc", "/var", "/proc", "/sys", "/dev", "/boot", "/sbin", "/bin", "/lib", "/usr"} {
		if clean == blocked || strings.HasPrefix(clean, blocked+"/") {
			return fmt.Errorf("workspace_dir must not be a system path (%s)", blocked)
		}
	}
	return nil
}

// Delete handles DELETE /workspaces/:id
// If the workspace has children (is a team), cascade deletes all sub-workspaces.
// Use ?confirm=true to actually delete (otherwise returns children list for confirmation).
func (h *WorkspaceHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()
	confirm := c.Query("confirm") == "true"

	// #687: reject non-UUID IDs before hitting the DB.
	if err := validateWorkspaceID(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}

	var workspaceName, workspaceStatus string
	var activeTasks int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT name, COALESCE(active_tasks, 0), status FROM workspaces WHERE id = $1`, id,
	).Scan(&workspaceName, &activeTasks, &workspaceStatus); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
		log.Printf("Delete: workspace lookup failed for %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check workspace"})
		return
	}
	if workspaceStatus == string(models.StatusRemoved) {
		c.JSON(http.StatusGone, gin.H{"error": "workspace removed", "id": id})
		return
	}

	if c.GetHeader("X-Confirm-Name") != workspaceName {
		childCount, scheduleCount := destructiveDeleteCounts(ctx, id)
		c.JSON(http.StatusBadRequest, gin.H{
			"error":          "destructive_action_requires_confirmation",
			"hint":           "Re-send the same request with header X-Confirm-Name: " + workspaceName,
			"workspace_name": workspaceName,
			"active_tasks":   activeTasks,
			"child_count":    childCount,
			"schedule_count": scheduleCount,
		})
		return
	}

	// RFC platform-agent Phase 4b: gate org-token (platform-agent) deletes behind
	// human approval. No-op for ordinary workspace/CP-session callers and when
	// the rollout flag is off (scoping lives in gateDestructive). Placed after
	// the synchronous X-Confirm-Name guard, before any destruction.
	if !gateDestructive(c, h.broadcaster, id, approvals.ActionDeleteWorkspace,
		"delete workspace "+workspaceName,
		map[string]interface{}{"workspace_id": id, "name": workspaceName}) {
		return
	}

	// Check for children
	rows, err := db.DB.QueryContext(ctx,
		`SELECT id, name FROM workspaces WHERE parent_id = $1 AND status != 'removed'`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check children"})
		return
	}
	defer rows.Close()

	var children []map[string]string
	for rows.Next() {
		var childID, childName string
		if rows.Scan(&childID, &childName) == nil {
			children = append(children, map[string]string{"id": childID, "name": childName})
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Delete: child rows error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check children"})
		return
	}

	// If has children and not confirmed, return children list for confirmation.
	// Uses HTTP 409 Conflict (not 200) so `curl --fail`, `fetch().ok`, and any
	// client that treats HTTP 4xx as an error surfaces the confirmation
	// requirement. Body shape unchanged so the canvas UI's parser keeps
	// working. Fixes #88.
	if len(children) > 0 && !confirm {
		c.JSON(http.StatusConflict, gin.H{
			"status":         "confirmation_required",
			"message":        "This workspace has sub-workspaces. Delete with ?confirm=true to cascade delete.",
			"children":       children,
			"children_count": len(children),
		})
		return
	}

	// Delegate the cascade to CascadeDelete so the HTTP path and the
	// OrgImport reconcile path share one teardown sequence (#73 race
	// guard, container stop, volume removal, token revocation, schedule
	// disable, broadcast). The HTTP-specific bits — direct-children 409
	// gate above, ?purge=true hard-delete below, response shaping —
	// stay in this handler.
	// internal#734: the user can ask to erase saved data (browser profile /
	// cookies / downloads / agent memory) on delete. Opt-in — default keeps the
	// data on its volume for the orphan-sweeper grace. Only a genuine
	// permanent-delete reaches here (restart/reconcile use other paths), so this
	// is the one place prune may be requested.
	erase := c.Query("erase_data") == "true"
	descendantIDs, stopErrs, err := h.CascadeDelete(ctx, id, erase)
	if err != nil {
		// Audit 2026-05-09 (Core-Security): raw `err.Error()` here was
		// exposed to HTTP clients verbatim, including wrapped lib/pq
		// driver strings that disclose schema column names + index
		// hints. Log full error server-side; return a sanitized message
		// to the client. Operators trace via the log line below using
		// the workspace id.
		log.Printf("Delete: CascadeDelete(%s) failed: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error processing delete request"})
		return
	}
	allIDs := append([]string{id}, descendantIDs...)

	// If any Stop call failed, surface 500 so the client retries. The DB
	// row is already 'removed' (idempotent), and Stop's instance_id
	// lookup tolerates that — the retry replays the terminate. This is
	// the loud-fail-instead-of-silent-leak choice; users see a 500
	// instead of an orphaned EC2.
	if len(stopErrs) > 0 {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("workspace marked removed, but %d stop call(s) failed — please retry: %v",
				len(stopErrs), errors.Join(stopErrs...)),
			"removed_count": len(allIDs),
			"stop_failures": len(stopErrs),
		})
		return
	}

	// Hard purge: cascade delete all FK data and remove the DB row entirely (#1087)
	if c.Query("purge") == "true" {
		purgeIDs := pq.Array(allIDs)
		// Order matters: delete from leaf tables first, then workspace row
		for _, table := range []string{
			// agent_memories removed in Phase A3 (#1792); memory rows now
			// live in memory_plugin.memory_records. The plugin's
			// namespace-cascade handles cleanup when the workspace's
			// namespace is deleted via DeleteNamespace.
			"activity_logs", "workspace_secrets",
			"workspace_channels", "workspace_config", "workspace_memory",
			"workspace_token_usage", "approval_requests", "audit_events",
			"workflow_checkpoints", "workspace_artifacts", "agents",
			"workspace_auth_tokens", "workspace_schedules", "canvas_layouts",
		} {
			if _, err := db.DB.ExecContext(ctx,
				fmt.Sprintf("DELETE FROM %s WHERE workspace_id = ANY($1::uuid[])", table),
				purgeIDs); err != nil {
				log.Printf("Purge %s error for %v: %v", table, allIDs, err)
			}
		}
		// Null out parent_id / forwarded_to references
		if _, err := db.DB.ExecContext(ctx, "UPDATE workspaces SET parent_id = NULL WHERE parent_id = ANY($1::uuid[])", purgeIDs); err != nil {
			log.Printf("Purge parent_id null error for %v: %v", allIDs, err)
		}
		if _, err := db.DB.ExecContext(ctx, "UPDATE workspaces SET forwarded_to = NULL WHERE forwarded_to = ANY($1::uuid[])", purgeIDs); err != nil {
			log.Printf("Purge forwarded_to null error for %v: %v", allIDs, err)
		}
		// Hard delete the workspace row
		if _, err := db.DB.ExecContext(ctx, "DELETE FROM workspaces WHERE id = ANY($1::uuid[])", purgeIDs); err != nil {
			log.Printf("Purge workspace row error for %v: %v", allIDs, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "purge failed"})
			return
		}

		// I5 (RFC #2728): best-effort plugin namespace cleanup. If
		// MEMORY_V2 is wired, ask the plugin to drop each purged
		// workspace's `workspace:<id>` namespace so stale namespaces
		// don't accumulate. We deliberately do NOT clean up team:* /
		// org:* namespaces — those may still be referenced by other
		// workspaces under the same root.
		//
		// Failures are logged but don't fail the purge (which has
		// already succeeded against the workspaces table).
		if h.namespaceCleanupFn != nil {
			for _, id := range allIDs {
				h.namespaceCleanupFn(ctx, id)
			}
		}

		c.JSON(http.StatusOK, gin.H{"status": "purged", "cascade_deleted": len(descendantIDs)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "removed", "cascade_deleted": len(descendantIDs)})
}

func destructiveDeleteCounts(ctx context.Context, id string) (childCount int, scheduleCount int) {
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspaces WHERE parent_id = $1 AND status != 'removed'`, id,
	).Scan(&childCount); err != nil {
		log.Printf("Delete: child count failed for %s: %v", id, err)
		childCount = 0
	}
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspace_schedules WHERE workspace_id = $1 AND enabled = true`, id,
	).Scan(&scheduleCount); err != nil {
		log.Printf("Delete: schedule count failed for %s: %v", id, err)
		scheduleCount = 0
	}
	return childCount, scheduleCount
}

// CascadeDelete performs the cascade-removal sequence used by the HTTP
// DELETE handler and by OrgImport's reconcile mode: walk descendants, mark
// self+descendants 'removed' first (#73 race guard), stop containers / EC2s,
// remove volumes, revoke tokens, disable schedules, broadcast events.
//
// Idempotent against already-removed rows (the descendant CTE and all UPDATE
// guards skip status='removed'). Returns the descendant id list so the HTTP
// caller can drive the optional `?purge=true` hard-delete path against the
// same set the cascade just touched, plus any per-workspace stop errors so
// callers can surface a retryable failure instead of a silent-leak.
//
// Caller is responsible for the children-confirmation gate (the HTTP handler
// returns 409 when children exist + ?confirm=true is missing); this helper
// always cascades.
// CascadeDelete tears down a workspace and its descendants (stop compute,
// remove volumes, revoke tokens, disable schedules, broadcast). erase=true
// (internal#734) means the user asked to erase saved data, so the CP compute
// teardown prunes each workspace's durable data volume; the HTTP delete passes
// the user's choice, the org-import reconcile passes false (a reconcile is not
// a user-erase).
func (h *WorkspaceHandler) CascadeDelete(ctx context.Context, id string, erase bool) ([]string, []error, error) {
	if err := validateWorkspaceID(id); err != nil {
		return nil, nil, err
	}

	descendantIDs := []string{}
	descRows, err := db.DB.QueryContext(ctx, `
		WITH RECURSIVE descendants AS (
			SELECT id FROM workspaces WHERE parent_id = $1 AND status != 'removed'
			UNION ALL
			SELECT w.id FROM workspaces w JOIN descendants d ON w.parent_id = d.id WHERE w.status != 'removed'
		)
		SELECT id FROM descendants
	`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("descendant query: %w", err)
	}
	defer descRows.Close()
	for descRows.Next() {
		var descID string
		if descRows.Scan(&descID) == nil {
			descendantIDs = append(descendantIDs, descID)
		}
	}
	if err := descRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("CascadeDelete: failed iterating descendants: %w", err)
	}

	allIDs := append([]string{id}, descendantIDs...)

	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, updated_at = now() WHERE id = ANY($2::uuid[])`,
		models.StatusRemoved, pq.Array(allIDs)); err != nil {
		log.Printf("CascadeDelete status update for %s: %v", id, err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM canvas_layouts WHERE workspace_id = ANY($1::uuid[])`,
		pq.Array(allIDs)); err != nil {
		log.Printf("CascadeDelete canvas_layouts for %s: %v", id, err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspace_auth_tokens SET revoked_at = now()
		 WHERE workspace_id = ANY($1::uuid[]) AND revoked_at IS NULL`,
		pq.Array(allIDs)); err != nil {
		log.Printf("CascadeDelete token revocation for %s: %v", id, err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspace_schedules SET enabled = false, updated_at = now()
		 WHERE workspace_id = ANY($1::uuid[]) AND enabled = true`,
		pq.Array(allIDs)); err != nil {
		log.Printf("CascadeDelete schedule disable for %s: %v", id, err)
	}

	cleanupCtx, cleanupCancel := context.WithTimeout(
		context.WithoutCancel(ctx), 30*time.Second)
	defer cleanupCancel()

	var stopErrs []error
	stopAndRemove := func(wsID string) {
		// Delete-path stop uses bounded retry (matches the restart path) and
		// records a durable structure_events row on exhaustion so a leaked /
		// pending EC2 is queryable and handed off to the CP-orphan-sweeper —
		// rather than the bare one-shot StopWorkspaceAuto that produced the
		// silent-leak class (task #15 / workspace-ec2-leak).
		if err := h.stopWorkspaceForDelete(cleanupCtx, wsID, erase); err != nil {
			log.Printf("CascadeDelete %s stop failed: %v — leaving cleanup for orphan sweeper", wsID, err)
			stopErrs = append(stopErrs, fmt.Errorf("stop %s: %w", wsID, err))
			return
		}
		if h.provisioner != nil {
			if err := h.provisioner.RemoveVolume(cleanupCtx, wsID); err != nil {
				log.Printf("CascadeDelete %s volume removal warning: %v", wsID, err)
			}
		}
	}

	for _, descID := range descendantIDs {
		stopAndRemove(descID)
		db.ClearWorkspaceKeys(cleanupCtx, descID)
		restartStates.Delete(descID)
		h.broadcaster.RecordAndBroadcast(cleanupCtx, string(events.EventWorkspaceRemoved), descID, map[string]interface{}{})
	}
	stopAndRemove(id)
	db.ClearWorkspaceKeys(cleanupCtx, id)
	restartStates.Delete(id)
	h.broadcaster.RecordAndBroadcast(cleanupCtx, string(events.EventWorkspaceRemoved), id, map[string]interface{}{
		"cascade_deleted": len(descendantIDs),
	})

	return descendantIDs, stopErrs, nil
}

// PatchTemplate handles PATCH /workspaces/:id/template.
// It sets the installed template for an existing workspace without changing
// its engine runtime. A restart/re-provision is required for the template
// assets to be fetched and applied.
//
// Auth: admin/CP-gated at the router (mirrors PATCH /workspaces/:id/budget).
func (h *WorkspaceHandler) PatchTemplate(c *gin.Context) {
	id := c.Param("id")
	if err := validateWorkspaceID(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var body struct {
		Template string `json:"template" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "template is required"})
		return
	}

	if !isKnownTemplate(body.Template) {
		log.Printf("PatchTemplate: %q is not a known workspace template", body.Template)
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":    "unsupported workspace template",
			"template": body.Template,
			"code":     "TEMPLATE_UNSUPPORTED",
		})
		return
	}

	var exists bool
	if err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = $1)`, id,
	).Scan(&exists); err != nil {
		log.Printf("PatchTemplate: existence check failed for %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	if _, err := db.DB.ExecContext(c.Request.Context(),
		`UPDATE workspaces SET template = $2, updated_at = now() WHERE id = $1`, id, body.Template,
	); err != nil {
		log.Printf("PatchTemplate: update failed for %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update template"})
		return
	}

	log.Printf("PatchTemplate: workspace %s template updated to %q", id, body.Template)
	c.JSON(http.StatusOK, gin.H{"status": "updated", "needs_restart": true})
}

// validateWorkspaceID returns an error when id is not a valid UUID.
// #687: prevents 500s from Postgres when a garbage string (e.g. ../../etc/passwd)
// is passed as the :id path parameter.
func validateWorkspaceID(id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("invalid workspace id")
	}
	return nil
}

// yamlSpecialChars is the set of YAML-special characters banned from workspace
// name and role. Newlines are handled separately below (same error message for
// all four fields); these additional characters target YAML block indicators,
// flow-sequence/mapping delimiters, and shell-expansion metacharacters that
// yamlQuote does NOT escape inside a double-quoted scalar (#685).
const yamlSpecialChars = "{}[]|>*&!"

// validateWorkspaceFields enforces maximum field lengths and rejects characters
// that could enable YAML-injection in downstream provisioning paths.
// #685 (defence-in-depth over yamlQuote — newline + YAML-special chars in name/role),
// #688 (max field lengths).
func validateWorkspaceFields(name, role, model, runtime string) error {
	// All four fields: reject newline / carriage-return.
	for _, f := range []struct{ label, val string }{
		{"name", name},
		{"role", role},
		{"model", model},
		{"runtime", runtime},
	} {
		if strings.ContainsAny(f.val, "\n\r") {
			return fmt.Errorf("%s must not contain newline characters", f.label)
		}
	}
	// name and role only: reject YAML-special characters (#685).
	for _, f := range []struct{ label, val string }{
		{"name", name},
		{"role", role},
	} {
		if strings.ContainsAny(f.val, yamlSpecialChars) {
			return fmt.Errorf("%s contains invalid characters", f.label)
		}
	}
	if len(name) > 255 {
		return fmt.Errorf("name must be at most 255 characters")
	}
	if len(role) > 1000 {
		return fmt.Errorf("role must be at most 1000 characters")
	}
	if len(model) > 100 {
		return fmt.Errorf("model must be at most 100 characters")
	}
	if len(runtime) > 100 {
		return fmt.Errorf("runtime must be at most 100 characters")
	}
	return nil
}
