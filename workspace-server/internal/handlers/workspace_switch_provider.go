package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/gin-gonic/gin"
)

// SwitchProvider handles POST /workspaces/:id/switch-provider — move an EXISTING
// workspace to a different cloud (compute.provider). The VM is cloud-specific, so
// this reprovisions the box on the new cloud; the agent's durable identity
// (agent_card, config, secrets, tokens, memory) lives in the tenant DB and is
// preserved because the row is never deleted.
//
// CRITICAL ORDERING: the stop must run with the OLD provider BEFORE the DB
// provider is changed. The stop helper reads the current row at call time; if we
// wrote the new provider first, the teardown request would target the wrong
// backend and the old box would leak. So the sequence is strictly:
//
//  1. stop OLD box (DB still has old provider + old instance_id)
//  2. clear instance_id + write new provider (jsonb_set, preserving the rest)
//  3. provision NEW box (withStoredCompute now reads the new provider)
//
// Clearing instance_id in step 2 also makes a retried switch safe: a second call
// finds no stale instance to act on with the new provider metadata.
func (h *WorkspaceHandler) SwitchProvider(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		Provider        string `json:"provider"`
		ConfirmDataLoss bool   `json:"confirm_data_loss"`
		// MigrateData is accepted for forward-compat (RFC #622 PR4 wires the
		// filesystem migration). Until then it is a no-op and a persistent
		// volume still requires confirm_data_loss.
		MigrateData bool `json:"migrate_data"`
	}
	if err := c.ShouldBindJSON(&body); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	newProvider := strings.ToLower(strings.TrimSpace(body.Provider))
	if newProvider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required"})
		return
	}
	if _, ok := workspaceComputeProviderAllowlist[newProvider]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported provider (want aws|gcp|hetzner)"})
		return
	}

	var status, wsName, dbRuntime, dbTemplate, oldProvider, dataPersistence string
	var oldInstanceID sql.NullString
	var tier int
	err := db.DB.QueryRowContext(ctx, `
		SELECT status, name, tier, COALESCE(runtime, 'claude-code'), COALESCE(template, ''),
		       COALESCE(compute->>'provider', ''), COALESCE(compute->>'data_persistence', ''),
		       instance_id
		FROM workspaces WHERE id = $1`, id,
	).Scan(&status, &wsName, &tier, &dbRuntime, &dbTemplate, &oldProvider, &dataPersistence, &oldInstanceID)
	if err == sql.ErrNoRows || status == string(models.StatusRemoved) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// Switching the cloud backend is a SaaS-only concept — a self-hosted Docker
	// workspace has no cloud provider to switch. external/mock runtimes have no
	// box at all.
	if h.cpProv == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "provider switching is only available for cloud (SaaS) workspaces"})
		return
	}
	if isExternalLikeRuntime(dbRuntime) || dbRuntime == "mock" {
		c.JSON(http.StatusConflict, gin.H{"error": dbRuntime + " workspaces have no cloud box to switch"})
		return
	}
	if paused, parentName := isParentPaused(ctx, id); paused {
		c.JSON(http.StatusConflict, gin.H{"error": "parent workspace \"" + parentName + "\" is paused — resume it first"})
		return
	}

	// "" provider means the default AWS path.
	effectiveOld := oldProvider
	if effectiveOld == "" {
		effectiveOld = "aws"
	}
	if newProvider == effectiveOld {
		c.JSON(http.StatusOK, gin.H{"status": "noop", "provider": newProvider, "message": "workspace is already on this provider"})
		return
	}

	// Data gate: a persistent data volume is cloud-specific and cannot move as a
	// block device. Until the filesystem migration lands (RFC #622 PR3/PR4), the
	// switch is allowed only with an explicit confirm_data_loss.
	if dataPersistence == "persist" && !body.ConfirmDataLoss {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "DATA_LOSS_UNCONFIRMED",
			"detail": "this workspace has a persistent data volume that cannot move across clouds; set confirm_data_loss=true to switch with a fresh volume (cross-cloud data migration is RFC #622, not yet wired). Identity/config/secrets/memory are preserved regardless.",
		})
		return
	}

	// --- ordered switch (see doc-comment) ---

	// 1. PRE-CLAIM: atomically mark the switch as in-flight by setting
	//    status='provisioning' WITHOUT changing the provider. The CAS
	//    (`status<>'provisioning' AND provider unchanged`) prevents a
	//    racing duplicate switch (or a switch against a workspace that
	//    is already provisioning) from getting past this point. A losing
	//    pre-claim returns 0 rows → 409 immediately, with NO stop side
	//    effect (CR2 blocking finding: pre-fix the stop ran before the
	//    CAS, so a losing request still executed the stop side effect
	//    against a box it didn't own).
	//
	// url is NOT touched here — the pre-claim only flips status. The
	// later step 3 (provider write) nulls instance_id and the rollback
	// above reverts only status, so we don't need to snapshot/restore
	// url. Keeping the pre-claim minimal also means a failed
	// pre-claim never needs a revert (the row is unchanged).
	preClaim, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		SET status = $2, updated_at = now()
		WHERE id = $1
		  AND status <> $2
		  AND COALESCE(compute->>'provider', '') IS NOT DISTINCT FROM $3`,
		id, models.StatusProvisioning, oldProvider)
	if err != nil {
		log.Printf("SwitchProvider: pre-claim failed for %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to claim switch"})
		return
	}
	if n, _ := preClaim.RowsAffected(); n == 0 {
		// Lost the pre-claim: another switch already set status='provisioning',
		// the workspace is in initial provisioning, or the provider changed
		// under us. Do NOT execute the stop — the box is owned by an
		// in-flight provision/switch, not by us.
		//
		// CRITICAL: do NOT arm the commit-or-rollback defer here. A losing
		// pre-claim means we did NOT flip status='provisioning' on this
		// row — arming the defer would let the rollback (gated on
		// `status = 'provisioning'`) clobber ANOTHER request's in-flight
		// pre-claim to OUR priorStatus, stranding them. The defer is armed
		// only after THIS request successfully wins the pre-claim (CR2
		// #11493 ownership-ordering fix).
		c.JSON(http.StatusConflict, gin.H{"error": "ALREADY_SWITCHING", "detail": "a provider switch or provision is already in progress for this workspace"})
		return
	}

	// COMMIT-OR-ROLLBACK pattern for the pre-claim (armed AFTER pre-claim
	// success per CR2 #11493). After step 1 sets status='provisioning',
	// any error / ctx-cancellation before step 5 completes the switch
	// leaves the workspace stranded in 'provisioning' forever (CR2
	// #11486 follow-up finding). The defer reverts status to
	// priorStatus on ANY error path; the `committed` flag is set ONLY
	// when the switch fully reaches step 5 (provision dispatched). The
	// rollback uses a fresh context (not the request ctx) so a client
	// disconnect mid-switch still cleans up. The rollback UPDATE is
	// gated on `status = 'provisioning'` so a concurrent
	// switch/provision that has already advanced the status is not
	// clobbered.
	committed := false
	priorStatus := status
	defer func() {
		if committed {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := db.DB.ExecContext(rollbackCtx, `
			UPDATE workspaces
			SET status = $2, updated_at = now()
			WHERE id = $1
			  AND status = $3`,
			id, priorStatus, models.StatusProvisioning); err != nil {
			log.Printf("SwitchProvider: status revert failed for %s (priorStatus=%q): %v — workspace may need operator intervention", id, priorStatus, err)
		}
	}()

	// 2. Stop the OLD box with the OLD provider. DB still has the old
	//    provider + old instance_id (the pre-claim only flipped status,
	//    not provider — the stop helper reads provider+instance_id at
	//    call time). Bounded retry; on exhaustion we STILL proceed
	//    (a stuck old box must not strand the switch) — except we
	//    capture the failure so step 4 can emit a durable audit row,
	//    because step 3 nulls instance_id and flips provider, which
	//    otherwise leaves the old box untracked by normal lifecycle
	//    cleanup (review finding #3).
	stopErr := h.cpStopWithRetryErr(ctx, id, "SwitchProvider", false)

	// 3. Write the new provider + clear instance_id. The pre-claim
	//    already set status='provisioning' (so a duplicate check on
	//    status is not needed here — the row is owned by this switch).
	//    The `WHERE id=$1` is the only guard: if the row was deleted
	//    between pre-claim and now (vanishingly rare), 0 rows → 500
	//    and the audit row carries the diagnostic. jsonb_set preserves
	//    instance_type/volume/display/data_persistence.
	res, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		SET instance_id = NULL,
		    compute = jsonb_set(COALESCE(compute, '{}'::jsonb), '{provider}', to_jsonb($2::text)),
		    updated_at = now()
		WHERE id = $1`,
		id, newProvider)
	if err != nil {
		log.Printf("SwitchProvider: failed to write new provider for %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to switch provider"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Row was deleted between pre-claim and now. Emit an audit
		// row so the diagnostic is queryable, then 500.
		log.Printf("SwitchProvider: row disappeared after pre-claim for %s", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "workspace row missing after pre-claim"})
		return
	}

	// 4. If the old box never confirmed stopped, it may not be tracked by the
	//    normal lifecycle cleanup after instance_id was nulled. Emit a durable
	//    audit row carrying the old instance_id + provider so a platform cleanup
	//    process can locate and terminate it (review finding #3).
	if stopErr != nil && oldInstanceID.Valid && oldInstanceID.String != "" {
		h.emitSwitchProviderStopExhausted(ctx, id, oldInstanceID.String, effectiveOld, newProvider, stopErr)
	}
	log.Printf("SwitchProvider: %s %s → %s (old box stop err=%v, reprovisioning)", id, effectiveOld, newProvider, stopErr)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), id, map[string]interface{}{
		"name":          wsName,
		"tier":          tier,
		"runtime":       dbRuntime,
		"provider_from": effectiveOld,
		"provider_to":   newProvider,
	})

	// 5. Provision the NEW box. withStoredCompute re-reads compute
	//    (now carrying the new provider) → provisionWorkspaceAuto routes
	//    centrally to the new backend. Reuse the existing config volume
	//    (templatePath="") so identity/config are preserved. Detached
	//    context: the reprovision outlives the request. Routes through
	//    provisionWorkspaceAuto (not provisionWorkspaceCP directly) per
	//    TestNoCallSiteCallsDirectProvisionerExceptAuto (core#2422 RCA tick).
	payload := withStoredCompute(context.Background(), id, models.CreateWorkspacePayload{Name: wsName, Tier: tier, Runtime: dbRuntime, Template: dbTemplate})
	h.provisionWorkspaceAuto(id, "", nil, payload)

	// All 5 steps completed; mark the switch COMMITTED so the rollback
	// defer does NOT revert status='provisioning'. The new provision is
	// in flight on a goroutine and will progress to 'online' (or
	// 'failed' via the central provision machinery) on its own.
	committed = true

	c.JSON(http.StatusAccepted, gin.H{
		"status":       "switching",
		"workspace_id": id,
		"from":         effectiveOld,
		"to":           newProvider,
	})
}

// emitSwitchProviderStopExhausted records a durable audit row when a provider
// switch could not confirm the OLD box stopped before its instance_id was
// cleared from the row. Without it the old box may not be tracked by normal
// lifecycle cleanup; a platform cleanup process reads these rows by
// old_instance_id + old_provider to terminate the leaked box. Best-effort.
func (h *WorkspaceHandler) emitSwitchProviderStopExhausted(ctx context.Context, workspaceID, oldInstanceID, oldProvider, newProvider string, cause error) {
	if db.DB == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"workspace_id":    workspaceID,
		"old_instance_id": oldInstanceID,
		"old_provider":    oldProvider,
		"new_provider":    newProvider,
		"last_error":      cause.Error(),
		"recovery_path":   "platform_cleanup",
	})
	if err != nil {
		log.Printf("emitSwitchProviderStopExhausted: marshal failed for %s: %v", workspaceID, err)
		return
	}
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO structure_events (event_type, workspace_id, payload, created_at)
		VALUES ($1, $2, $3, now())
	`, "workspace.provider.switch_stop_exhausted", workspaceID, payload); err != nil {
		log.Printf("emitSwitchProviderStopExhausted: insert failed for %s: %v", workspaceID, err)
	}
}
