package handlers

// workspace_crud.go — workspace state queries, updates, deletion, and
// field validation. Covers State (polling endpoint), Update (PATCH),
// Delete (cascade + purge), and input validation helpers.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
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

// sensitiveUpdateFields documents fields that carry elevated risk — kept as
// an explicit list for code readability and future audits. Auth is now fully
// enforced at the router layer (WorkspaceAuth middleware, #680 IDOR fix);
// this map is no longer used for in-handler gate logic but is preserved to
// surface the risk classification clearly.
//
// budget_limit is intentionally NOT here — the dedicated PATCH
// /workspaces/:id/budget (AdminAuth) is the only write path (#611).
var sensitiveUpdateFields = map[string]struct{}{
	"tier":          {},
	"parent_id":     {},
	"runtime":       {},
	"workspace_dir": {},
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

	ctx := c.Request.Context()

	// Auth is fully enforced at the router layer (WorkspaceAuth middleware, #680).
	// WorkspaceAuth validates that the caller holds a valid bearer token for this
	// specific workspace — no additional auth gate is needed here. The
	// sensitiveUpdateFields map above documents the risk classification for
	// auditors but is no longer used as a runtime gate.

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
	if runtime, ok := body["runtime"]; ok {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET runtime = $2, updated_at = now() WHERE id = $1`, id, runtime); err != nil {
			log.Printf("Update runtime error for %s: %v", id, err)
		}
	}
	needsRestart := false
	if wsDir, ok := body["workspace_dir"]; ok {
		// Allow null to clear workspace_dir
		if wsDir != nil {
			if dirStr, isStr := wsDir.(string); isStr && dirStr != "" {
				if err := validateWorkspaceDir(dirStr); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace directory"})
					return
				}
			}
		}
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET workspace_dir = $2, updated_at = now() WHERE id = $1`, id, wsDir); err != nil {
			log.Printf("Update workspace_dir error for %s: %v", id, err)
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

	// Cascade delete: collect ALL descendants (not just direct children) via
	// recursive CTE, then stop each container and remove each volume.
	// Previous bug: only direct children's containers were stopped, leaving
	// grandchildren as orphan running containers after a cascade delete.
	descendantIDs := []string{}
	if len(children) > 0 {
		descRows, err := db.DB.QueryContext(ctx, `
			WITH RECURSIVE descendants AS (
				SELECT id FROM workspaces WHERE parent_id = $1 AND status != 'removed'
				UNION ALL
				SELECT w.id FROM workspaces w JOIN descendants d ON w.parent_id = d.id WHERE w.status != 'removed'
			)
			SELECT id FROM descendants
		`, id)
		if err != nil {
			log.Printf("Delete: descendant query error for %s: %v", id, err)
		} else {
			for descRows.Next() {
				var descID string
				if descRows.Scan(&descID) == nil {
					descendantIDs = append(descendantIDs, descID)
				}
			}
			descRows.Close()
		}
	}

	// #73 fix: mark rows 'removed' in the DB FIRST, BEFORE stopping containers
	// or removing volumes. Previously the sequence was stop → update-status,
	// which left a gap where:
	//   - the container's last pre-teardown heartbeat could resurrect the row
	//     via the register-handler UPSERT (now also guarded in #73)
	//   - the liveness monitor could observe 'online' status + expired Redis
	//     TTL and trigger RestartByID, recreating a container we're trying
	//     to destroy
	// Marking 'removed' first makes both of those paths no-op via their
	// existing `status NOT IN ('removed', ...)` guards.
	allIDs := append([]string{id}, descendantIDs...)
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, updated_at = now() WHERE id = ANY($2::uuid[])`,
		models.StatusRemoved, pq.Array(allIDs)); err != nil {
		log.Printf("Delete status update error for %s: %v", id, err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM canvas_layouts WHERE workspace_id = ANY($1::uuid[])`,
		pq.Array(allIDs)); err != nil {
		log.Printf("Delete canvas_layouts error for %s: %v", id, err)
	}
	// Revoke all auth tokens for the deleted workspaces. Once the workspace is
	// gone its tokens are meaningless; leaving them alive would keep
	// HasAnyLiveTokenGlobal = true even after the platform is otherwise empty,
	// which prevents AdminAuth from returning to fail-open and breaks the E2E
	// test's count-zero assertion (and local re-run cleanup).
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspace_auth_tokens SET revoked_at = now()
		 WHERE workspace_id = ANY($1::uuid[]) AND revoked_at IS NULL`,
		pq.Array(allIDs)); err != nil {
		log.Printf("Delete token revocation error for %s: %v", id, err)
	}
// #1027: cascade-disable all schedules for the deleted workspaces so
	// the scheduler never fires a cron into a removed container.
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspace_schedules SET enabled = false, updated_at = now()
		 WHERE workspace_id = ANY($1::uuid[]) AND enabled = true`,
		pq.Array(allIDs)); err != nil {
		log.Printf("Delete schedule disable error for %s: %v", id, err)
	}

	// Now stop containers + remove volumes for all descendants (any depth).
	// Any concurrent heartbeat / registration / liveness-triggered restart
	// will see status='removed' and bail out early.
	//
	// Combines two concerns:
	//
	//  1. Detach cleanup from the request ctx via WithoutCancel + a 30s
	//     timeout, so when the canvas's `api.del` resolves on our 200
	//     (and gin cancels c.Request.Context()), in-flight Docker
	//     stop/remove calls don't get cancelled mid-operation. The
	//     previous shape leaked containers every time the canvas hung
	//     up promptly: Stop returned "context canceled", the container
	//     stayed up, and the next RemoveVolume failed with
	//     "volume in use". 30s is generous for Docker daemon round-
	//     trips (typical: <2s) and bounds a stuck daemon.
	//
	//  2. #1843: aggregate Stop() failures into stopErrs so the
	//     post-deletion block surfaces them as 500. On the CP/EC2
	//     backend, Stop() calls control plane's DELETE endpoint to
	//     terminate the EC2; if that errors (transient 5xx, network),
	//     the EC2 stays running with no DB row to track it (the
	//     "orphan EC2 on a 0-customer account" scenario). Loud-fail
	//     instead of silent-leak — clients retry, Stop's instance_id
	//     lookup is idempotent against status='removed'. RemoveVolume
	//     errors stay log-and-continue (local cleanup, not infra-leak).
	cleanupCtx, cleanupCancel := context.WithTimeout(
		context.WithoutCancel(ctx), 30*time.Second)
	defer cleanupCancel()

	var stopErrs []error
	stopAndRemove := func(wsID string) {
		// Stop the workload first via the backend dispatcher (CP for
		// SaaS, Docker for self-hosted). Pre-2026-05-05 this gate was
		// `if h.provisioner == nil { return }` — early-returning on
		// every SaaS tenant left the EC2 running with no DB row to
		// track it (issue #2814; the comment below claimed "loud-fail
		// instead of silent-leak" but the early-return made it the
		// silent path on SaaS).
		//
		// Check Stop's error before any volume cleanup — the previous
		// code discarded it and immediately tried RemoveVolume, which
		// always fails with "volume in use" when Stop didn't actually
		// kill the container. The orphan sweeper
		// (registry/orphan_sweeper.go) catches what we skip here on
		// the next reconcile pass.
		if err := h.StopWorkspaceAuto(cleanupCtx, wsID); err != nil {
			log.Printf("Delete %s stop failed: %v — leaving cleanup for orphan sweeper", wsID, err)
			stopErrs = append(stopErrs, fmt.Errorf("stop %s: %w", wsID, err))
			return
		}
		// Volume cleanup is Docker-only — CP-managed workspaces have
		// no host-bind volumes to remove. Skip silently when no Docker
		// provisioner is wired (the SaaS path already terminated the
		// EC2 above; nothing left to do).
		if h.provisioner != nil {
			if err := h.provisioner.RemoveVolume(cleanupCtx, wsID); err != nil {
				log.Printf("Delete %s volume removal warning: %v", wsID, err)
			}
		}
	}

	for _, descID := range descendantIDs {
		stopAndRemove(descID)
		db.ClearWorkspaceKeys(cleanupCtx, descID)
		// #2269: drop the per-workspace restartState entry so it
		// doesn't accumulate across the platform's lifetime. The
		// LoadOrStore that creates the entry (workspace_restart.go)
		// has no companion remove path; without this Delete, every
		// short-lived workspace leaks ~16 bytes forever.
		restartStates.Delete(descID)
		// Detach broadcaster ctx for the same reason as the cleanup
		// above — RecordAndBroadcast does an INSERT INTO
		// structure_events + Redis Publish. If the canvas hangs up,
		// a request-ctx-bound INSERT can be cancelled mid-write,
		// leaving other WS clients ignorant of the cascade. The DB
		// row is already 'removed' so it's recoverable, but the
		// inconsistency is avoidable.
		h.broadcaster.RecordAndBroadcast(cleanupCtx, "WORKSPACE_REMOVED", descID, map[string]interface{}{})
	}

	stopAndRemove(id)
	db.ClearWorkspaceKeys(cleanupCtx, id)
	restartStates.Delete(id) // #2269: same as descendants above

	h.broadcaster.RecordAndBroadcast(cleanupCtx, "WORKSPACE_REMOVED", id, map[string]interface{}{
		"cascade_deleted": len(descendantIDs),
	})

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
			"agent_memories", "activity_logs", "workspace_secrets",
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
		db.DB.ExecContext(ctx, "UPDATE workspaces SET parent_id = NULL WHERE parent_id = ANY($1::uuid[])", purgeIDs)
		db.DB.ExecContext(ctx, "UPDATE workspaces SET forwarded_to = NULL WHERE forwarded_to = ANY($1::uuid[])", purgeIDs)
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
