package handlers

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provlog"
	"github.com/gin-gonic/gin"
)

// restartState coalesces concurrent RestartByID calls for one workspace.
//
// The naive "one mutex per workspace, TryLock+drop" pattern caused a real
// data-loss bug: SetSecret + SetModel both fire `go restartFunc(...)` from
// the HTTP handler, and both writes commit before either restart goroutine
// gets to load workspace_secrets. If the second goroutine arrives while the
// first holds the mutex, TryLock returns false and the second is silently
// skipped. The first goroutine's loadWorkspaceSecrets ran before the second
// write committed, so the new container boots without that env var. Surfaced
// as the "No LLM provider configured" hermes error when MODEL_PROVIDER landed
// after the API-key write but lost its restart to the mutex.
//
// The fix is the pending-flag / coalescing pattern: any restart request that
// arrives while one is in flight sets the pending flag and returns. The
// in-flight runner, on completion, checks the flag and runs another cycle.
// This collapses N concurrent requests into at most 2 sequential restarts
// (the current one + one more that picks up everything written during it),
// while guaranteeing the final container always sees the latest secrets.
type restartState struct {
	mu      sync.Mutex
	running bool // true while a restart cycle is in flight
	pending bool // set by any caller that arrived during the in-flight cycle
}

// restartStates is a per-workspace map of *restartState. Each workspace gets
// its own entry so unrelated workspaces don't serialize on each other.
var restartStates sync.Map // map[workspaceID]*restartState

// isParentPaused checks if any ancestor of the workspace is paused.
func isParentPaused(ctx context.Context, workspaceID string) (bool, string) {
	var parentID *string
	db.DB.QueryRowContext(ctx, `SELECT parent_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&parentID)
	if parentID == nil {
		return false, ""
	}
	var parentStatus, parentName string
	err := db.DB.QueryRowContext(ctx,
		`SELECT status, name FROM workspaces WHERE id = $1`, *parentID,
	).Scan(&parentStatus, &parentName)
	if err != nil {
		return false, ""
	}
	if parentStatus == "paused" {
		return true, parentName
	}
	// Check grandparent recursively
	return isParentPaused(ctx, *parentID)
}

// Restart handles POST /workspaces/:id/restart
// Works for offline, failed, or degraded workspaces. Stops any existing container, then re-provisions.
func (h *WorkspaceHandler) Restart(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var status, wsName, dbRuntime string
	var tier int
	err := db.DB.QueryRowContext(ctx,
		`SELECT status, name, tier, COALESCE(runtime, 'langgraph') FROM workspaces WHERE id = $1`, id,
	).Scan(&status, &wsName, &tier, &dbRuntime)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	// Block restart if any ancestor is paused — must resume parent first
	if paused, parentName := isParentPaused(ctx, id); paused {
		c.JSON(http.StatusConflict, gin.H{"error": "parent workspace \"" + parentName + "\" is paused — resume it first"})
		return
	}

	// runtime=external: the workspace has no Docker container or EC2 — its
	// lifecycle is operator-driven (a remote poller heartbeats from outside
	// the platform). Pre-fix, this handler still ran the full re-provision
	// pipeline, which calls issueAndInjectToken → RevokeAllForWorkspace.
	// That silently destroyed the operator's local bearer token on every
	// "Restart" click, leaving them with a 401-spamming poller and no
	// platform-side recovery path short of regenerating from the canvas
	// Tokens tab. Auto-restart already short-circuits external (see
	// runRestartCycle below). Mirror that here so manual + automatic
	// behavior agree, and surface a clear message instead of silently
	// no-op'ing — the canvas can show the operator that the fix is on
	// their side.
	if dbRuntime == "external" {
		c.JSON(http.StatusOK, gin.H{
			"status":  "noop",
			"runtime": "external",
			"message": "external workspaces are operator-driven — restart your local poller; platform has nothing to restart",
		})
		return
	}

	// runtime=mock: virtual workspace with canned A2A replies. No
	// container, no EC2, no provisioning state to recycle. Mirror
	// the external no-op so the canvas's Restart button doesn't
	// silently fail or leak through to the (template-less) provisioner.
	if dbRuntime == "mock" {
		c.JSON(http.StatusOK, gin.H{
			"status":  "noop",
			"runtime": "mock",
			"message": "mock workspaces have no container — restart is a no-op",
		})
		return
	}

	// SaaS mode: cpProv handles workspace EC2 lifecycle. Self-hosted mode:
	// provisioner handles local Docker containers. At least one must be
	// available — previously only `provisioner` was checked, which broke
	// restart entirely on every SaaS tenant (the workspace EC2 couldn't
	// be terminated + relaunched, the endpoint 503'd on every try).
	if !h.HasProvisioner() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provisioner not available"})
		return
	}

	// Read runtime from container's config.yaml before stopping. Docker-
	// only: in SaaS mode the workspace runs on a remote EC2 and we can't
	// exec into it, so we trust the DB value (user updates runtime via
	// the Config tab which writes through to both the DB and the container).
	containerRuntime := dbRuntime
	if h.provisioner != nil {
		containerName := configDirName(id) // ws-{id[:12]}
		if cfgBytes, readErr := h.provisioner.ExecRead(ctx, containerName, "/configs/config.yaml"); readErr == nil {
			for _, line := range strings.Split(string(cfgBytes), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "runtime:") {
					parsed := strings.TrimSpace(strings.TrimPrefix(line, "runtime:"))
					if parsed != "" && parsed != containerRuntime {
						log.Printf("Restart: runtime changed in config.yaml %q→%q for %s", containerRuntime, parsed, wsName)
						containerRuntime = parsed
						db.DB.ExecContext(ctx, `UPDATE workspaces SET runtime = $1 WHERE id = $2`, containerRuntime, id)
					}
					break
				}
			}
		}
	}

	// Reset to provisioning
	db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, url = '', updated_at = now() WHERE id = $2`, models.StatusProvisioning, id)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), id, map[string]interface{}{
		"name":    wsName,
		"tier":    tier,
		"runtime": containerRuntime,
	})

	// Read template from request body or try to find matching config
	var body struct {
		Template      string `json:"template"`
		ApplyTemplate bool   `json:"apply_template"` // force re-apply runtime-default template (e.g. after runtime change)
		Reset         bool   `json:"reset"`          // #12: discard claude-sessions volume before restart
		RebuildConfig bool   `json:"rebuild_config"` // #239: re-render config volume from org-template source (recovery path when volume was destroyed)
	}
	c.ShouldBindJSON(&body)

	templatePath, configLabel := resolveRestartTemplate(h.configsDir, wsName, dbRuntime, restartTemplateInput{
		Template:      body.Template,
		ApplyTemplate: body.ApplyTemplate,
		RebuildConfig: body.RebuildConfig,
	})

	// #239: rebuild_config=true — try org-templates as last-resort source so a
	// workspace with a destroyed config volume can self-recover without admin
	// intervention. Only fires when no other template was resolved above.
	if templatePath == "" && body.RebuildConfig {
		if p, label := resolveOrgTemplate(h.configsDir, wsName); p != "" {
			templatePath = p
			configLabel = label
			log.Printf("Restart: rebuild_config — using org-template %s for %s (%s)", label, wsName, id)
		}
	}

	if templatePath == "" {
		log.Printf("Restart: reusing existing config volume for %s (%s)", wsName, id)
	} else {
		log.Printf("Restart: using template %s for %s (%s)", templatePath, wsName, id)
	}

	var configFiles map[string][]byte
	payload := models.CreateWorkspacePayload{Name: wsName, Tier: tier, Runtime: containerRuntime}
	log.Printf("Restart: workspace %s (%s) runtime=%q", wsName, id, containerRuntime)

	// #12: ?reset=true (or body.Reset) discards the claude-sessions volume
	// before restart, giving the agent a clean /root/.claude/sessions dir.
	resetClaudeSession := c.Query("reset") == "true" || body.Reset
	if resetClaudeSession {
		log.Printf("Restart: reset=true — will discard claude-sessions volume for %s (%s)", wsName, id)
	}

	// Capture restart-context data BEFORE provisionWorkspaceOpts flips
	// last_heartbeat_at with the new session. Issue #19 Layer 1.
	restartData := loadRestartContextData(ctx, id)

	// Dispatch through the SoT restart dispatcher. RestartWorkspaceAutoOpts
	// owns "which backend for stop" + "which backend for provision" and
	// keeps the two halves in sync. resetClaudeSession is the one
	// Docker-only per-invocation knob the dispatcher carries through.
	//
	// Stop runs inside the dispatcher's stop leg (synchronous), then the
	// provision leg fires in a goroutine — NOT before the response —
	// because CPProvisioner.Stop is synchronous DELETE /cp/workspaces/:id
	// → CP → AWS EC2 terminate, which can exceed the canvas's 15s default
	// HTTP timeout when the platform has just redeployed (every tenant's
	// CP request queues at once). Pre-fix (2026-04-30) the user saw a
	// misleading "signal timed out" on the canvas even though the
	// restart actually succeeded — caught on hongmingwang hermes
	// workspace 32993ee7-…cb9d75d112a5 right after the heartbeat-fix
	// platform redeploy. context.Background() detaches the dispatch
	// from the request lifecycle so an aborted client connection
	// doesn't cancel the in-flight Stop/provision pair.
	//
	// Pre-2026-05-05 this site inlined the manual if-cpProv-else
	// dispatch with Docker-FIRST ordering (a different drift class from
	// the silent-drop bugs PRs #2811/#2824 closed). RestartWorkspaceAuto
	// enforces CP-FIRST ordering matching the other dispatchers — see
	// docs/architecture/backends.md.
	go func() {
		h.RestartWorkspaceAutoOpts(context.Background(), id, templatePath, configFiles, payload, resetClaudeSession)
	}()
	go h.sendRestartContext(id, restartData)

	c.JSON(http.StatusOK, gin.H{"status": "provisioning", "config_dir": configLabel, "reset_session": resetClaudeSession})
}

// Hibernate handles POST /workspaces/:id/hibernate
// Manually puts a running workspace into hibernation — useful for immediate
// cost savings without waiting for the idle timer. The workspace auto-wakes
// on the next incoming A2A message/send.
func (h *WorkspaceHandler) Hibernate(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var wsName string
	var tier, activeTasks int
	err := db.DB.QueryRowContext(ctx,
		`SELECT name, tier, active_tasks FROM workspaces WHERE id = $1 AND status IN ('online', 'degraded')`, id,
	).Scan(&wsName, &tier, &activeTasks)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found or not in a hibernatable state (must be online or degraded)"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// #822: reject hibernation when active tasks are in flight unless caller
	// passes ?force=true. Prevents operator from accidentally killing a
	// mid-task agent.
	if activeTasks > 0 && c.Query("force") != "true" {
		c.JSON(http.StatusConflict, gin.H{
			"error":        "workspace has active tasks; use ?force=true to terminate them",
			"active_tasks": activeTasks,
		})
		return
	}
	if activeTasks > 0 {
		log.Printf("[WARN] force-hibernating workspace %s (%s) with %d active tasks", id, wsName, activeTasks)
	}

	h.HibernateWorkspace(ctx, id)
	c.JSON(http.StatusOK, gin.H{"status": "hibernated"})
}

// HibernateWorkspace stops the container and sets the workspace status to
// 'hibernated'. Called by the hibernation monitor when a workspace has had
// active_tasks == 0 for longer than its configured hibernation_idle_minutes.
// Hibernated workspaces auto-wake on the next incoming A2A message.
//
// TOCTOU safety (#819): the three-step pattern below is atomic at the DB level.
//
//  1. Atomic claim: a single UPDATE WHERE locks the row by transitioning
//     status → 'hibernating', gated on status IN ('online','degraded') AND
//     active_tasks = 0.  If any concurrent caller (another goroutine, the
//     idle-timer, or a manual API call) already claimed the row, or if tasks
//     arrived since the caller decided to hibernate, rowsAffected == 0 and
//     this function returns immediately without stopping anything.
//
//  2. provisioner.Stop: safe to call now because status == 'hibernating';
//     the routing layer rejects new tasks for non-online/degraded workspaces,
//     so no new task can be dispatched between step 1 and step 2.
//
//  3. Final UPDATE to 'hibernated': records the completed hibernation.
func (h *WorkspaceHandler) HibernateWorkspace(ctx context.Context, workspaceID string) {
	// ── Step 1: Atomic claim ──────────────────────────────────────────────────
	// The UPDATE acts as a DB-level advisory lock: only one concurrent caller
	// can transition the row from online/degraded → hibernating.  The
	// active_tasks = 0 predicate ensures we never interrupt a running task.
	result, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		SET    status = $2, updated_at = now()
		WHERE  id = $1
		  AND  status IN ('online', 'degraded')
		  AND  active_tasks = 0`, workspaceID, models.StatusHibernating)
	if err != nil {
		log.Printf("Hibernate: atomic claim failed for %s: %v", workspaceID, err)
		return
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		// Either already hibernating/hibernated/paused/removed, or active_tasks > 0 —
		// safe to abort without side-effects.
		return
	}

	// Fetch name/tier for logging and event broadcast (after the claim, so we
	// can use a simple SELECT without a status guard).
	var wsName string
	var tier int
	if scanErr := db.DB.QueryRowContext(ctx,
		`SELECT name, tier FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&wsName, &tier); scanErr != nil {
		wsName = workspaceID // fallback for log messages
	}

	// ── Step 2: Stop the container ────────────────────────────────────────────
	// Status is now 'hibernating'; the router rejects new task routing here, so
	// there is no race window between claiming the row and stopping the container.
	log.Printf("Hibernate: stopping container for %s (%s)", wsName, workspaceID)
	if h.stopFnOverride != nil {
		h.stopFnOverride(ctx, workspaceID)
	} else if h.provisioner != nil {
		h.provisioner.Stop(ctx, workspaceID)
	}

	// ── Step 3: Mark fully hibernated ─────────────────────────────────────────
	if _, err = db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, url = '', updated_at = now() WHERE id = $2`,
		models.StatusHibernated, workspaceID); err != nil {
		log.Printf("Hibernate: failed to mark hibernated for %s: %v", workspaceID, err)
		return
	}

	db.ClearWorkspaceKeys(ctx, workspaceID)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceHibernated), workspaceID, map[string]interface{}{
		"name": wsName,
		"tier": tier,
	})
	log.Printf("Hibernate: workspace %s (%s) is now hibernated", wsName, workspaceID)
}

// RestartByID restarts a workspace by ID — for programmatic use (e.g.,
// auto-restart after secret change). Calls that arrive while one is in flight
// are coalesced via the pending-flag pattern (see restartState above): the
// in-flight runner picks up the pending request after its current cycle
// completes, so writes that committed mid-restart are guaranteed to land.
func (h *WorkspaceHandler) RestartByID(workspaceID string) {
	// At least one of the two provisioners must be wired. Pre-fix this
	// short-circuited on h.provisioner==nil alone, which silently disabled
	// reactive auto-restart on every SaaS tenant (where the local Docker
	// provisioner is intentionally nil). The runRestartCycle below now
	// branches on which one is set for the Stop call.
	if !h.HasProvisioner() {
		return
	}
	coalesceRestart(workspaceID, func() { h.runRestartCycle(workspaceID) })
}

// coalesceRestart implements the pending-flag gate around an arbitrary cycle
// function. Extracted from RestartByID for direct unit testing — the cycle
// function in production is `runRestartCycle`, but tests pass a counter to
// verify the coalescing math (N concurrent requests → ≤2 cycles).
func coalesceRestart(workspaceID string, cycle func()) {
	sv, _ := restartStates.LoadOrStore(workspaceID, &restartState{})
	state := sv.(*restartState)

	// Mark a restart as wanted. If one is already running, return — that
	// runner will see pending=true on its next loop iteration and run
	// another cycle that picks up our request's effects. NOT dropped.
	state.mu.Lock()
	state.pending = true
	if state.running {
		state.mu.Unlock()
		log.Printf("Auto-restart: %s — coalescing with in-flight cycle (pending=true)", workspaceID)
		return
	}
	state.running = true
	state.mu.Unlock()

	// Always clear running on exit — including panic — so a panicking
	// cycle (e.g. a future provisionWorkspace nil-deref) doesn't leave
	// the workspace permanently locked out of restarts.
	//
	// recover()-and-DON'T-re-raise on purpose: this runs in a goroutine
	// (callers are `go h.RestartByID(...)`); an unrecovered panic in a
	// goroutine crashes the whole platform process, taking down every
	// workspace served by this binary because of one bug in cycle for
	// one workspace. Log the panic with stack trace for debuggability,
	// then recover and let the goroutine exit cleanly. The next restart
	// request for this workspace will see running=false and proceed.
	defer func() {
		state.mu.Lock()
		state.running = false
		state.mu.Unlock()
		if r := recover(); r != nil {
			log.Printf("Auto-restart: %s — cycle panicked, restart-state cleared: %v\n%s",
				workspaceID, r, debug.Stack())
		}
	}()

	// Drain pending requests. Each iteration re-loads workspace_secrets
	// inside provisionWorkspace, so any writes that committed since the
	// last cycle are picked up. Continues until no pending request was
	// observed at the top of an iteration.
	for {
		state.mu.Lock()
		if !state.pending {
			state.mu.Unlock()
			return // defer clears running
		}
		state.pending = false
		state.mu.Unlock()

		cycle()
	}
}

// stopForRestart dispatches Stop to whichever provisioner is wired (Docker or
// CP/EC2 — mutually exclusive in production). Docker provisioner.Stop kills
// the local container; CP provisioner.Stop calls DELETE /cp/workspaces/:id
// which terminates the EC2 instance. Pre-fix runRestartCycle only called the
// Docker path, so on SaaS (h.provisioner=nil) the auto-restart cycle silently
// NPE'd before reaching the reprovision step — which is why every SaaS dead-
// agent incident pre-this-fix required manual restart from canvas.
func (h *WorkspaceHandler) stopForRestart(ctx context.Context, workspaceID string) {
	backend := "none"
	if h.provisioner != nil {
		backend = "docker"
	} else if h.cpProv != nil {
		backend = "cp"
	}
	provlog.Event("restart.pre_stop", map[string]any{
		"workspace_id": workspaceID,
		"backend":      backend,
	})
	if h.provisioner != nil {
		h.provisioner.Stop(ctx, workspaceID)
		return
	}
	if h.cpProv != nil {
		h.cpStopWithRetry(ctx, workspaceID, "Auto-restart")
	}
}

// cpStopRetryAttempts caps total Stop attempts (initial + retries). 3 catches
// the transient CP/AWS hiccups that produce most leaks (one EC2 metadata
// service stall, one IAM rate-limit blip) without slowing recovery noticeably
// — worst-case wait is ~7s (1 + 2 + 4 backoff) and we run in a detached
// goroutine, so user UX is unaffected. Package-level so tests can shrink it.
var cpStopRetryAttempts = 3

// cpStopRetryBaseDelay is the first-retry backoff. Doubles each attempt:
// 1s, 2s, 4s for default attempts=3.
var cpStopRetryBaseDelay = 1 * time.Second

// cpStopWithRetry wraps cpProv.Stop with bounded exponential backoff for
// the restart paths. Different policy from workspace_crud.go's Delete:
// Delete returns 500 to the client on Stop failure (loud-fail-and-block,
// since the user asked to destroy and silent leak is unacceptable),
// whereas Restart's contract is "make the workspace alive again" — if we
// refuse to reprovision when Stop fails, we strand the user with a dead
// workspace. So this helper retries to absorb transient failures, then on
// final exhaustion emits a structured `LEAK-SUSPECT` log and returns —
// the caller proceeds with reprovision regardless. The leak signal is
// the bridge to the (forthcoming) CP-side workspace orphan reconciler;
// grep `LEAK-SUSPECT cpProv.Stop` to find affected workspace IDs.
//
// source tags the originating path ("Restart" / "Auto-restart") so the
// log line attributes leaks to the path that produced them.
//
// Returns nothing — caller's contract is unchanged.
func (h *WorkspaceHandler) cpStopWithRetry(ctx context.Context, workspaceID, source string) {
	if h.cpProv == nil {
		return
	}
	var lastErr error
	delay := cpStopRetryBaseDelay
	for attempt := 1; attempt <= cpStopRetryAttempts; attempt++ {
		err := h.cpProv.Stop(ctx, workspaceID)
		if err == nil {
			if attempt > 1 {
				log.Printf("%s: cpProv.Stop(%s) succeeded on attempt %d", source, workspaceID, attempt)
			}
			return
		}
		lastErr = err
		if attempt == cpStopRetryAttempts {
			break
		}
		// Sleep with ctx awareness so a cancelled ctx exits early instead
		// of stalling the goroutine through the remaining backoff.
		select {
		case <-ctx.Done():
			log.Printf("%s: cpProv.Stop(%s) abandoned mid-retry: ctx cancelled (last_err=%v)",
				source, workspaceID, lastErr)
			return
		case <-time.After(delay):
		}
		delay *= 2
	}
	// Exhausted. Loud-flag: stable prefix `LEAK-SUSPECT` + key=value pairs
	// so logs are greppable / parseable for the CP-side orphan reconciler.
	log.Printf("LEAK-SUSPECT cpProv.Stop workspace_id=%s source=%s attempts=%d last_err=%q",
		workspaceID, source, cpStopRetryAttempts, lastErr.Error())
}

// runRestartCycle does the actual stop+provision work for one restart
// iteration. Synchronous (waits for provisionWorkspace to complete) so the
// outer pending-flag loop in RestartByID can correctly coalesce — if this
// returned before the new container was up, the loop would race the
// in-progress provision goroutine on the next iteration's Stop call.
func (h *WorkspaceHandler) runRestartCycle(workspaceID string) {
	ctx := context.Background()

	var wsName, status, dbRuntime string
	var tier int
	err := db.DB.QueryRowContext(ctx,
		`SELECT name, status, tier, COALESCE(runtime, 'langgraph') FROM workspaces WHERE id = $1 AND status NOT IN ('removed', 'paused', 'hibernated')`, workspaceID,
	).Scan(&wsName, &status, &tier, &dbRuntime)
	if err != nil {
		return // includes paused/hibernated — don't auto-restart those
	}

	// Don't auto-restart external workspaces (no Docker container)
	// or mock workspaces (no container, every reply is canned —
	// see workspace-server/internal/handlers/mock_runtime.go).
	if dbRuntime == "external" || dbRuntime == "mock" {
		return
	}

	// Don't auto-restart if any ancestor is paused
	if paused, _ := isParentPaused(ctx, workspaceID); paused {
		return
	}

	// If still provisioning, brief wait so container exists for Stop()
	if status == "provisioning" {
		log.Printf("Auto-restart: interrupting provisioning for %s (%s)", wsName, workspaceID)
		time.Sleep(10 * time.Second)
	}

	log.Printf("Auto-restart: restarting %s (%s) runtime=%q (was: %s)", wsName, workspaceID, dbRuntime, status)

	h.stopForRestart(ctx, workspaceID)

	db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, url = '', updated_at = now() WHERE id = $2`, models.StatusProvisioning, workspaceID)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), workspaceID, map[string]interface{}{
		"name": wsName, "tier": tier, "runtime": dbRuntime,
	})

	// Runtime from DB — no more config file parsing
	payload := models.CreateWorkspacePayload{Name: wsName, Tier: tier, Runtime: dbRuntime}

	// Snapshot restart-context data before the new session overwrites
	// last_heartbeat_at. Issue #19 Layer 1.
	restartData := loadRestartContextData(ctx, workspaceID)

	// On auto-restart, do NOT re-apply templates — preserve existing config volume.
	// provisionWorkspaceAutoSync is the SYNCHRONOUS dispatcher (mirrors
	// provisionWorkspaceAuto but blocks instead of spawning a goroutine):
	// returns when the new container is up (or has failed). The outer
	// pending-flag loop in RestartByID relies on this to know when it's
	// safe to start another restart cycle without racing this one's
	// Stop call.
	//
	// Pre-2026-05-05 this site inlined the if-cpProv-else dispatch. On
	// SaaS the cycle would NPE inside provisionWorkspace's
	// `h.provisioner.VolumeHasFile` call, get swallowed by
	// coalesceRestart's recover()-without-re-raise (a platform-stability
	// safeguard), and leave the workspace permanently stuck in
	// status='provisioning' (the UPDATE above already ran). User-
	// observable result on SaaS pre-fix: dead workspace → manual canvas
	// restart was the only recovery path.
	h.provisionWorkspaceAutoSync(workspaceID, "", nil, payload)
	// sendRestartContext is a one-way notification to the new container; safe
	// to fire async — the next restart cycle won't depend on it completing.
	go h.sendRestartContext(workspaceID, restartData)
}

// Pause handles POST /workspaces/:id/pause
// Stops the container and sets status to 'paused'. The workspace remains on the canvas
// but won't receive heartbeats, won't be auto-restarted, and won't consume resources.
// Config volume is preserved — resume will re-provision with the same config.
func (h *WorkspaceHandler) Pause(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var status, wsName string
	err := db.DB.QueryRowContext(ctx,
		`SELECT status, name FROM workspaces WHERE id = $1 AND status NOT IN ('removed', 'paused')`, id,
	).Scan(&status, &wsName)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found or already paused"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// Collect this workspace + all descendants to pause
	toPause := []struct{ id, name string }{{id, wsName}}
	rows, _ := db.DB.QueryContext(ctx,
		`WITH RECURSIVE descendants AS (
			SELECT id, name FROM workspaces WHERE parent_id = $1 AND status NOT IN ('removed', 'paused')
			UNION ALL
			SELECT w.id, w.name FROM workspaces w JOIN descendants d ON w.parent_id = d.id WHERE w.status NOT IN ('removed', 'paused')
		) SELECT id, name FROM descendants`, id)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var cid, cname string
			if rows.Scan(&cid, &cname) == nil {
				toPause = append(toPause, struct{ id, name string }{cid, cname})
			}
		}
	}

	// Stop containers and mark all as paused. StopWorkspaceAuto routes
	// to whichever backend is wired (CP for SaaS, Docker for self-hosted)
	// — pre-2026-05-05 this site inlined `if h.provisioner != nil { Stop }`,
	// which silently leaked EC2s on every SaaS Pause (same drift class as
	// the team-collapse leak #2813 and the workspace-delete leak #2814,
	// both closed by PR #2824). StopWorkspaceAuto returns nil on no-backend
	// (no-op), so the Pause-specific bookkeeping (mark paused, clear keys,
	// broadcast) still fires regardless of whether anything was actually
	// stopped — matches the pre-fix behavior on misconfigured deployments.
	for _, ws := range toPause {
		if err := h.StopWorkspaceAuto(ctx, ws.id); err != nil {
			log.Printf("Pause: stop %s failed: %v — orphan sweeper will reconcile", ws.id, err)
		}
		db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = $1, url = '', updated_at = now() WHERE id = $2`, models.StatusPaused, ws.id)
		db.ClearWorkspaceKeys(ctx, ws.id)
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspacePaused), ws.id, map[string]interface{}{
			"name": ws.name,
		})
	}

	log.Printf("Paused workspace %s (%s) + %d children", wsName, id, len(toPause)-1)
	c.JSON(http.StatusOK, gin.H{"status": "paused", "paused_count": len(toPause)})
}

// Resume handles POST /workspaces/:id/resume
// Re-provisions a paused workspace. Config volume is preserved from before the pause.
func (h *WorkspaceHandler) Resume(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var wsName, dbRuntime string
	var tier int
	err := db.DB.QueryRowContext(ctx,
		`SELECT name, tier, COALESCE(runtime, 'langgraph') FROM workspaces WHERE id = $1 AND status = 'paused'`, id,
	).Scan(&wsName, &tier, &dbRuntime)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found or not paused"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// Accept either provisioner (Docker self-hosted OR CP SaaS). See the
	// same guard in Restart above for context — Resume previously 503'd
	// on every SaaS tenant.
	if !h.HasProvisioner() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provisioner not available"})
		return
	}

	// Block resume if any ancestor is still paused — must resume from the top down
	if paused, parentName := isParentPaused(ctx, id); paused {
		c.JSON(http.StatusConflict, gin.H{"error": "parent workspace \"" + parentName + "\" is paused — resume it first"})
		return
	}

	// Collect this workspace + all paused descendants to resume
	type wsInfo struct {
		id, name, runtime string
		tier              int
	}
	toResume := []wsInfo{{id, wsName, dbRuntime, tier}}
	rows, _ := db.DB.QueryContext(ctx,
		`WITH RECURSIVE descendants AS (
			SELECT id, name, tier, COALESCE(runtime, 'langgraph') AS runtime FROM workspaces WHERE parent_id = $1 AND status = 'paused'
			UNION ALL
			SELECT w.id, w.name, w.tier, COALESCE(w.runtime, 'langgraph') FROM workspaces w JOIN descendants d ON w.parent_id = d.id WHERE w.status = 'paused'
		) SELECT id, name, tier, runtime FROM descendants`, id)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var ws wsInfo
			if rows.Scan(&ws.id, &ws.name, &ws.tier, &ws.runtime) == nil {
				toResume = append(toResume, ws)
			}
		}
	}

	// Re-provision all
	for _, ws := range toResume {
		db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2`, models.StatusProvisioning, ws.id)
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), ws.id, map[string]interface{}{
			"name": ws.name, "tier": ws.tier, "runtime": ws.runtime,
		})
		payload := models.CreateWorkspacePayload{Name: ws.name, Tier: ws.tier, Runtime: ws.runtime}
		// Resume is provision-only (workspace is paused, no live container
		// to stop). provisionWorkspaceAuto handles backend routing and the
		// no-backend mark-failed fallback identically to Create. Pre-
		// 2026-05-05 this site inlined the if-cpProv-else dispatch; the
		// dispatcher is the SoT now.
		h.provisionWorkspaceAuto(ws.id, "", nil, payload)
	}

	log.Printf("Resuming workspace %s (%s) + %d children", wsName, id, len(toResume)-1)
	c.JSON(http.StatusOK, gin.H{"status": "provisioning", "resumed_count": len(toResume)})
}
