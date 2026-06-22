package handlers

import (
	"context"
	"database/sql"
	"io"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provlog"
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
	// restartStartedAt records the wall-clock when the most recent cycle
	// flipped running=true. Used by the self-fire debounce (internal#544,
	// the ws-server self-fire restart feedback loop seen in prod-Reviewer/
	// Researcher 2026-05-19 ~00:05Z 4x reprov thrash): any RestartByID
	// arriving within RestartDebounceWindow of this timestamp is silently
	// dropped so a probe firing during the EC2-pending window can't
	// re-trigger a fresh full cycle on the just-launched instance.
	restartStartedAt time.Time
}

// restartStates is a per-workspace map of *restartState. Each workspace gets
// its own entry so unrelated workspaces don't serialize on each other.
var restartStates sync.Map // map[workspaceID]*restartState

// RestartDebounceWindow is the silent-drop window for successive RestartByID
// calls. Sized to cover the typical EC2 pending → online interval (20-30s)
// with a margin so a probe firing during the just-after-online but still-
// flaky heartbeat window also gets dropped. Bigger than that would block
// legitimate "Restart failed, retry" recoveries; smaller would let the
// 4x thrash class through. Package-level so tests can shrink it.
//
// COUPLING: this window MUST be >= the CP instance reconciler interval
// (cmd/server/main.go:355, currently 60s). If the interval ever shrinks
// below this window, a workspace flipped offline by one reconcile tick can
// be reprovisioned again by the next tick before the debounce drops it,
// reopening the double-reprovision thrash class (internal#544).
var RestartDebounceWindow = 60 * time.Second

// restartByIDDropCounter is incremented every time RestartByID drops a call
// inside the debounce window. Exposed as a package-level atomic counter so
// (a) tests can assert the drop fired, (b) ops can grep logs for the drop
// log line + the counter snapshot in a future /admin/metrics endpoint.
// Not a Prometheus metric because the platform doesn't pull metrics from
// workspace-server yet — that's a separate RFC.
var restartByIDDropCounter atomic.Uint64

// restartProvisionGates is a per-workspace mutex that serializes the
// Stop+Start cycle for a given ws-<id>. Closes the race where manual
// POST /workspaces/:id/restart (RestartWorkspaceAutoOpts) and programmatic
// RestartByID (runRestartCycle) BOTH async-dispatch Stop→Start and
// reached provisioner.Start twice for the same ws-<id>: the first call
// created ws-{id} and started writing tokens, the second call raced in
// and either (a) hit a Docker name conflict and markProvisionFailed
// wedged the workspace to "failed", or (b) silently rotated/wrote
// the bearer a second time and the second container start overlapped
// the first. Both surfaced as the "401 invalid auth token" /
// Docker-name-conflict symptom in #2659 Local Provision Lifecycle stub,
// run 353677/job 478450.
//
// Each workspace gets its own *sync.Mutex on first use (sync.Map
// LoadOrStore is the standard "map of locks" pattern — every workspace
// that ever restarted keeps a tiny mutex in this map for the process
// lifetime; if memory ever becomes a concern we can prune on workspace
// removal, but workspace counts are small enough that it's a non-issue).
//
// The gate is intentionally separate from the existing `restartState`
// (coalesceRestart) and the RestartDebounceWindow self-fire debounce:
//   - restartState coalesces: collapses N rapid concurrent RestartByID
//     calls into ≤2 sequential cycles (the in-flight one + one drain).
//   - RestartDebounceWindow self-fire: drops successive RestartByID
//     calls within 60s of the most recent cycle start (closes the
//     probe-during-EC2-pending self-fire loop).
//   - restartProvisionGate (THIS): the load-bearing exclusion that
//     makes "only ONE Docker create per ws-<id> at a time" hold across
//     the TWO different entry points (manual Restart HTTP + programmatic
//     RestartByID). The existing two gates only cover the RestartByID
//     path; the manual Restart HTTP handler bypassed both and called
//     RestartWorkspaceAutoOpts directly.
//
// Both RestartWorkspaceAutoOpts and runRestartCycle acquire this gate
// around their Stop+Start pair. A concurrent caller blocks (Go mutex
// semantics) until the in-flight cycle completes — so the second
// caller's Stop+Start runs AFTER the first's Start is fully done, and
// the second's provisioner.Start is the only one in flight at a time.
// The HTTP UX cost: a user double-clicking Restart gets a delayed
// response on the second click (the second Stop+Start waits). That's
// strictly better than the pre-fix behavior where the second click
// wedged the workspace to "failed".
var restartProvisionGates sync.Map // map[workspaceID]*sync.Mutex

// acquireRestartProvisionGate returns the per-workspace mutex, creating
// it on first use. Caller MUST defer Unlock after acquisition. The
// mutex is intended to wrap the entire Stop+Start cycle, not just
// provisioner.Start — the race is in the full sequence, not only Start.
func acquireRestartProvisionGate(workspaceID string) *sync.Mutex {
	sv, _ := restartProvisionGates.LoadOrStore(workspaceID, &sync.Mutex{})
	return sv.(*sync.Mutex)
}

// fileWriteRestartDebounceWindow is the per-workspace coalescing window for
// the file-write → RestartByID trigger fired by templates.go's WriteFile,
// DeleteFile, and ReplaceFiles handlers (and template_import.go's variants).
//
// Background (internal#624 2026-05-20): canvas Save fires N PUT /files
// requests in a 30-60s burst (claude-code SEO agent observed 10-17 files in
// 60s). Each successful write previously fired `goAsync(RestartByID)`. The
// 60s self-fire debounce in RestartByID itself catches calls 1-60s, but
// writes at T+65s+ pass the debounce, set pending=true on a still-running
// coalesceRestart cycle, and drain immediately into cycle 2 — which DELETEs
// + recreates EC2 mid-burst, returning 500 EC2InstanceStateInvalidException
// on the in-flight user PUTs.
//
// 15s is sized to absorb a canvas Save burst (writes typically land within
// a 5-10s window) while still letting a deliberate "edit, wait, edit again"
// pattern restart twice. Bigger than that would silently swallow legitimate
// rapid-iteration edits; smaller would let burst tails leak through.
var fileWriteRestartDebounceWindow = 15 * time.Second

// fileWriteRestartLastFireAt records the last time `maybeRestartAfterFileWrite`
// actually fired a restart for each workspace. sync.Map (not RWMutex+map)
// because writes happen on every successful file-write handler, reads on
// every subsequent file-write handler call — both per-workspace — and the
// keys are sparse + long-lived. Stored as int64 unix-nano so the load/store
// path can stay lock-free (atomic.Int64 inside sync.Map.Value is fine, but
// time.Time itself isn't atomically loadable).
var fileWriteRestartLastFireAt sync.Map // map[workspaceID]*atomic.Int64

// fileWriteRestartDropCounter counts how many file-write restart triggers
// were silently coalesced. Same observability rationale as
// restartByIDDropCounter — package-level atomic so tests can assert the
// drop fired and ops can correlate with "user clicked Save 10 times,
// only saw 1 restart cycle".
var fileWriteRestartDropCounter atomic.Uint64

// maybeRestartAfterFileWrite is the call-site debounce wrapper for the 9
// file-write trigger sites in templates.go + template_import.go. Replaces
// the direct `goAsync(func() { wh.RestartByID(wsID) })` pattern with a
// 15s per-workspace coalescing window:
//
//   - First call (no prior fire OR last fire >15s ago): records the
//     current timestamp and fires goAsync(RestartByID).
//   - Subsequent calls within 15s of the last fire: silently dropped,
//     drop counter incremented.
//
// This is the call-site-layer protection (internal#624 Path A). The drain-
// loop layer in coalesceRestart (Path B, re-stamping restartStartedAt per
// iteration) is the platform-layer defense in depth — together they close
// the file-write tight-loop class regardless of which entry point fires.
//
// Stateless on the handler so any handler with access to a WorkspaceHandler
// can use it; the per-workspace state lives in the package-level sync.Map.
func (h *WorkspaceHandler) maybeRestartAfterFileWrite(workspaceID string) {
	now := time.Now().UnixNano()

	// LoadOrStore the per-workspace last-fire stamp. First write for a
	// brand-new workspace falls through the CompareAndSwap below because
	// the zero-init value (0) is far enough in the past to satisfy the
	// "last fire >15s ago" predicate.
	sv, _ := fileWriteRestartLastFireAt.LoadOrStore(workspaceID, new(atomic.Int64))
	stamp := sv.(*atomic.Int64)

	// CAS loop: read last, decide, swap. We use CAS instead of Lock/Unlock
	// because the typical case is "thousands of writes, one restart per
	// 15s" — uncontended atomic is ~5ns vs ~30ns mutex. Bounded retry
	// because in the rare contended case (two writes finishing nanoseconds
	// apart) one will win the swap and the other will see the new stamp,
	// drop, and bail.
	for retry := 0; retry < 4; retry++ {
		last := stamp.Load()
		elapsed := time.Duration(now - last)
		if last != 0 && elapsed < fileWriteRestartDebounceWindow {
			// Within debounce window — drop silently.
			fileWriteRestartDropCounter.Add(1)
			log.Printf("maybeRestartAfterFileWrite: %s — coalesced "+
				"(last fire %s ago < %s window; total dropped=%d)",
				workspaceID, elapsed.Round(time.Millisecond),
				fileWriteRestartDebounceWindow,
				fileWriteRestartDropCounter.Load())
			return
		}
		if stamp.CompareAndSwap(last, now) {
			break
		}
		// Another writer beat us to the stamp update. Re-read and retry;
		// the retry will almost certainly see the new value and drop.
	}

	h.goAsync(func() { h.RestartByID(workspaceID) })
}

// isRestarting reports whether a restart cycle is currently in flight for
// the workspace. Callers that have their own "container looks dead" probe
// MUST consult this before triggering a restart, because during the
// 20-30s EC2-pending window the workspace's url=” and IsRunning()=false
// looks identical to a dead container — and any restart-triggering probe
// (maybeMarkContainerDead from canvas /delegations poll, or the trailing
// restart-context probe at the end of runRestartCycle) will set
// pending=true and the outer coalesceRestart loop will drain by running
// ANOTHER full cycle, ec2_stopped of the just-booted instance →
// re-provision. That's the self-fire loop closed by this gate.
// restartSettleWindow is the post-restart window during which a single
// IsRunning=false probe is NOT trusted. A workspace that just had its
// config.yaml PUT and was restarted can report IsRunning=false while the
// agent is still registering its first heartbeat / settling the container.
// Widening the self-fire guard to cover this window prevents a lone flaky
// probe from clearing the workspace URL and re-triggering a destructive
// restart. core#2929.
const restartSettleWindow = 30 * time.Second

func isRestarting(workspaceID string) bool {
	sv, ok := restartStates.Load(workspaceID)
	if !ok {
		return false
	}
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.running
}

// inRestartSettleWindow reports whether workspaceID is within
// restartSettleWindow of its most recent restart start. This widens the
// self-fire guard beyond the in-flight restart flag to cover the settle
// window right after a config-PUT restart. core#2929.
func inRestartSettleWindow(workspaceID string) bool {
	sv, ok := restartStates.Load(workspaceID)
	if !ok {
		return false
	}
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.restartStartedAt.IsZero() {
		return false
	}
	return time.Since(state.restartStartedAt) < restartSettleWindow
}

// lastRestartStartedAt returns the timestamp recorded when the most recent
// restart cycle started for workspaceID, if any. Used by the settle-window
// heartbeat freshness check in maybeMarkContainerDead. core#2929.
func lastRestartStartedAt(workspaceID string) (time.Time, bool) {
	sv, ok := restartStates.Load(workspaceID)
	if !ok {
		return time.Time{}, false
	}
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.restartStartedAt.IsZero() {
		return time.Time{}, false
	}
	return state.restartStartedAt, true
}

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

	var status, wsName, dbRuntime, dbTemplate string
	var tier int
	err := db.DB.QueryRowContext(ctx,
		`SELECT status, name, tier, COALESCE(runtime, 'claude-code'), COALESCE(template, '') FROM workspaces WHERE id = $1`, id,
	).Scan(&status, &wsName, &tier, &dbRuntime, &dbTemplate)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	// Block restart if workspace is removed — same 404 as not-found so we don't
	// leak that the row ever existed, and to prevent resurrecting a removed
	// workspace to 'provisioning' before the async runRestartCycle guard fires.
	if status == string(models.StatusRemoved) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
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
	if isExternalLikeRuntime(dbRuntime) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "noop",
			"runtime": dbRuntime,
			"message": dbRuntime + " workspaces are operator-driven — restart your local agent; platform has nothing to restart",
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

	// Read template from request body before consulting the existing
	// config volume. apply_template means the caller just changed the
	// runtime in the DB and wants the runtime-default template to be
	// authoritative; in that case, reading the old container config would
	// roll the DB runtime back before restart.
	var body struct {
		Template      string `json:"template"`
		ApplyTemplate bool   `json:"apply_template"` // force re-apply runtime-default template (e.g. after runtime change)
		Reset         bool   `json:"reset"`          // #12: discard claude-sessions volume before restart
		RebuildConfig bool   `json:"rebuild_config"` // #239: re-render config volume from org-template source (recovery path when volume was destroyed)
	}
	if err := c.ShouldBindJSON(&body); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	// Read runtime from container's config.yaml before stopping. Docker-
	// only: in SaaS mode the workspace runs on a remote EC2 and we can't
	// exec into it, so we trust the DB value (user updates runtime via
	// the Config tab which writes through to both the DB and the container).
	containerRuntime := h.restartRuntimeFromConfig(ctx, id, wsName, dbRuntime, body.ApplyTemplate)

	// Reset to provisioning
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, url = '', updated_at = now() WHERE id = $2`, models.StatusProvisioning, id); err != nil {
		log.Printf("Restart: failed to set provisioning status for %s: %v", id, err)
	}
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), id, map[string]interface{}{
		"name":    wsName,
		"tier":    tier,
		"runtime": containerRuntime,
	})

	templatePath, configLabel := resolveRestartTemplate(h.configsDir, wsName, dbRuntime, dbTemplate, restartTemplateInput{
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
	payload := withStoredCompute(ctx, id, models.CreateWorkspacePayload{Name: wsName, Tier: tier, Runtime: containerRuntime, Template: dbTemplate})
	log.Printf("Restart: workspace %s (%s) runtime=%q template=%q", wsName, id, containerRuntime, dbTemplate)

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
	h.goAsync(func() {
		h.RestartWorkspaceAutoOpts(context.Background(), id, templatePath, configFiles, payload, resetClaudeSession)
	})
	h.goAsync(func() { h.sendRestartContext(id, restartData) })

	c.JSON(http.StatusOK, gin.H{"status": "provisioning", "config_dir": configLabel, "reset_session": resetClaudeSession})
}

func (h *WorkspaceHandler) restartRuntimeFromConfig(ctx context.Context, id, wsName, dbRuntime string, applyTemplate bool) string {
	if h.provisioner == nil || applyTemplate {
		return dbRuntime
	}
	containerRuntime := dbRuntime
	containerName := provisioner.ContainerName(id) // ws-{id} (KI-013 full UUID)
	if cfgBytes, readErr := h.provisioner.ExecRead(ctx, containerName, "/configs/config.yaml"); readErr == nil {
		for _, line := range strings.Split(string(cfgBytes), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "runtime:") {
				parsed := strings.TrimSpace(strings.TrimPrefix(line, "runtime:"))
				if parsed != "" && parsed != containerRuntime {
					log.Printf("Restart: runtime changed in config.yaml %q→%q for %s", containerRuntime, parsed, wsName)
					containerRuntime = parsed
					if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET runtime = $1 WHERE id = $2`, containerRuntime, id); err != nil {
						log.Printf("Restart: failed to persist runtime %q for %s: %v", containerRuntime, id, err)
					}
				}
				break
			}
		}
	}
	return containerRuntime
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
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Hibernate: RowsAffected error for %s: %v", workspaceID, err)
		return
	}
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
	// Self-fire debounce: drop (not coalesce) successive RestartByID calls
	// within RestartDebounceWindow of the most recent cycle's start. This
	// is the load-bearing protection against the 4x reprov thrash class —
	// coalesceRestart's pending-flag would otherwise drain by running
	// ANOTHER full cycle of stop+provision on the just-launched EC2 (still
	// in the pending state), which is the self-fire we're closing.
	//
	// Only applies to RestartByID (programmatic — secrets handler,
	// maybeMarkContainerDead, preflightContainerHealth). The HTTP Restart
	// handler in workspace_restart.go's Restart() bypasses this path and
	// calls RestartWorkspaceAutoOpts directly, so user-initiated restart
	// clicks are unaffected.
	if shouldDebounceRestart(workspaceID) {
		restartByIDDropCounter.Add(1)
		log.Printf("RestartByID: %s — dropped (within %s self-fire debounce window; total dropped=%d)",
			workspaceID, RestartDebounceWindow, restartByIDDropCounter.Load())
		return
	}
	coalesceRestart(workspaceID, func() { h.runRestartCycle(workspaceID) })
}

// shouldDebounceRestart reports whether the most recent cycle for this
// workspace started within RestartDebounceWindow. Read-only on
// restartState; the actual restartStartedAt stamp is written in
// coalesceRestart when running flips false→true.
func shouldDebounceRestart(workspaceID string) bool {
	sv, ok := restartStates.Load(workspaceID)
	if !ok {
		return false
	}
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.restartStartedAt.IsZero() {
		return false
	}
	return time.Since(state.restartStartedAt) < RestartDebounceWindow
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
	// Stamp the start time so the RestartByID debounce can drop any
	// self-fire probe that hits within RestartDebounceWindow. Only the
	// false→true edge stamps; the drain-loop's inner cycles re-use the
	// same start (they're effectively one "restart event" from the
	// debounce's POV).
	state.restartStartedAt = time.Now()
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
	//
	// internal#624 Path B (defense in depth for the file-write tight-loop
	// class): re-stamp restartStartedAt at the top of every drain iteration
	// past the first. The original design (stamp only on false→true edge)
	// treated all drained pending as "one event from the debounce's POV",
	// which is correct for the secrets-batch use case but lets a file-write
	// burst at T+65s of a 60s drain pipe straight into another full cycle.
	// Re-stamping closes that hole — each drained cycle gets its own fresh
	// debounce window, so any RestartByID arriving during cycle N is
	// dropped by shouldDebounceRestart instead of accumulating into
	// pending=true for cycle N+1.
	//
	// The original "one cycle picks up everyone who arrived during it"
	// semantic still holds for the secrets-write path: callers that hit
	// coalesceRestart during cycle 1 still set pending=true and still get
	// their effects landed in cycle 2. What changes is that callers
	// arriving during cycle 2 (via RestartByID) now hit the re-stamped
	// debounce and are dropped instead of being chained into cycle 3,
	// which is exactly the chain that produced the 22:08-22:10 thrash on
	// 3fe84b89.
	iteration := 0
	for {
		state.mu.Lock()
		if !state.pending {
			state.mu.Unlock()
			return // defer clears running
		}
		state.pending = false
		if iteration > 0 {
			// Re-stamp for drained iterations only; the false→true edge
			// already stamped at the top of coalesceRestart.
			state.restartStartedAt = time.Now()
		}
		state.mu.Unlock()
		iteration++

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
	// Restart's contract is "make the workspace alive again": it proceeds
	// with reprovision regardless of the Stop outcome, so it discards the
	// terminal error. The delete path needs the error (it must keep the
	// row recoverable for the orphan-sweeper + emit a durable event), so
	// the actual retry loop lives in cpStopWithRetryErr below.
	_ = h.cpStopWithRetryErr(ctx, workspaceID, source, false) // restart/hibernate never prunes
}

// cpStopWithRetryErr is the shared bounded-retry core for cpProv.Stop.
// It returns the terminal error so callers that need to react to a leak
// (the DELETE path's stopWorkspaceForDelete) can do so, while
// cpStopWithRetry keeps its void contract for the restart paths.
//
// Behaviour (unchanged from the original cpStopWithRetry loop):
//   - cpProv nil          → nil (no-op; nothing to stop).
//   - success on attempt N → nil; logs a retry-success line when N > 1.
//   - ctx cancelled mid-retry → returns ctx.Err(); logs an "abandoned"
//     line and deliberately does NOT emit LEAK-SUSPECT (operator-initiated
//     drain is a different signal than "we tried hard and failed").
//   - all attempts fail   → returns the LAST attempt's error and emits the
//     stable `LEAK-SUSPECT cpProv.Stop ...` log line so the CP-side orphan
//     reconciler can correlate by workspace_id.
//
// cpStopWithRetryErr terminates the workspace's CP-managed compute with bounded
// retry. prune=true (internal#734) additionally requests CP erase the durable
// data volume — set ONLY by the permanent-delete-with-erase path, NEVER by
// restart/hibernate (those pass false), so a recreate can never prune.
func (h *WorkspaceHandler) cpStopWithRetryErr(ctx context.Context, workspaceID, source string, prune bool) error {
	if h.cpProv == nil {
		return nil
	}
	var lastErr error
	delay := cpStopRetryBaseDelay
	for attempt := 1; attempt <= cpStopRetryAttempts; attempt++ {
		var err error
		if prune {
			err = h.cpProv.StopAndPrune(ctx, workspaceID)
		} else {
			err = h.cpProv.Stop(ctx, workspaceID)
		}
		if err == nil {
			if attempt > 1 {
				log.Printf("%s: cpProv.Stop(%s) succeeded on attempt %d", source, workspaceID, attempt)
			}
			return nil
		}
		lastErr = err
		if attempt == cpStopRetryAttempts {
			break
		}
		// Sleep with ctx awareness so a cancelled ctx exits early instead
		// of stalling the goroutine through the remaining backoff.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Printf("%s: cpProv.Stop(%s) abandoned mid-retry: ctx cancelled (last_err=%v)",
				source, workspaceID, lastErr)
			return ctx.Err()
		case <-timer.C:
		}
		delay *= 2
	}
	// Exhausted. Loud-flag: stable prefix `LEAK-SUSPECT` + key=value pairs
	// so logs are greppable / parseable for the CP-side orphan reconciler.
	log.Printf("LEAK-SUSPECT cpProv.Stop workspace_id=%s source=%s attempts=%d last_err=%q",
		workspaceID, source, cpStopRetryAttempts, lastErr.Error())
	return lastErr
}

// runRestartCycle does the actual stop+provision work for one restart
// iteration. Synchronous (waits for provisionWorkspace to complete) so the
// outer pending-flag loop in RestartByID can correctly coalesce — if this
// returned before the new container was up, the loop would race the
// in-progress provision goroutine on the next iteration's Stop call.
//
// The cycle is wrapped in the per-workspace restart/provision GATE
// (acquireRestartProvisionGate) so concurrent programmatic RestartByID
// calls and the manual HTTP Restart handler (RestartWorkspaceAutoOpts)
// cannot overlap their provisioner.Start calls for the same ws-<id>.
// The outer coalesceRestart pending-flag already serializes N
// programmatic RestartByID calls into ≤2 sequential cycles; this gate
// closes the second class of race (manual + programmatic, or two
// distinct programmatic entry points firing near-simultaneously) that
// the pending-flag didn't cover.
func (h *WorkspaceHandler) runRestartCycle(workspaceID string) {
	ctx := context.Background()

	// Per-workspace restart/provision gate. The same gate is acquired
	// in RestartWorkspaceAutoOpts (the manual HTTP Restart path), so
	// the manual and programmatic paths mutually exclude on Stop+Start.
	// Held for the entire cycle (Stop → provision); the coalesceRestart
	// drain loop's inner cycles re-use the same gate because the
	// outer cycle holds it across the drain.
	gate := acquireRestartProvisionGate(workspaceID)
	gate.Lock()
	defer gate.Unlock()

	var wsName, status, dbRuntime, dbTemplate string
	var tier int
	err := db.DB.QueryRowContext(ctx,
		`SELECT name, status, tier, COALESCE(runtime, 'claude-code'), COALESCE(template, '') FROM workspaces WHERE id = $1 AND status NOT IN ('removed', 'paused', 'hibernated')`, workspaceID,
	).Scan(&wsName, &status, &tier, &dbRuntime, &dbTemplate)
	if err != nil {
		return // includes paused/hibernated — don't auto-restart those
	}

	// Don't auto-restart external workspaces (no Docker container)
	// or mock workspaces (no container, every reply is canned —
	// see workspace-server/internal/handlers/mock_runtime.go).
	if isExternalLikeRuntime(dbRuntime) || dbRuntime == "mock" {
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

	log.Printf("Auto-restart: restarting %s (%s) runtime=%q template=%q (was: %s)", wsName, workspaceID, dbRuntime, dbTemplate, status)

	// #125 Phase 1: send pre-restart drain signal to the workspace agent.
	// For native_session targets, A2A messages go directly to the SDK session
	// and bypass the platform's a2a_queue buffering. If the container dies
	// mid-request, those messages are lost. The pre-restart signal gives the
	// SDK a chance to drain in-flight work before the container stops.
	//
	// Fire-and-forget: gracefulPreRestart runs in a detached goroutine with its
	// own 10s timeout. If the workspace doesn't implement the handler (404) or
	// times out, we proceed with the stop anyway — identical to the pre-fix
	// behaviour.
	h.gracefulPreRestart(ctx, workspaceID)

	h.stopForRestart(ctx, workspaceID)

	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, url = '', updated_at = now() WHERE id = $2`, models.StatusProvisioning, workspaceID); err != nil {
		log.Printf("Auto-restart: failed to set provisioning status for %s: %v", workspaceID, err)
	}
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), workspaceID, map[string]interface{}{
		"name": wsName, "tier": tier, "runtime": dbRuntime, "template": dbTemplate,
	})

	// Runtime from DB — no more config file parsing
	payload := withStoredCompute(ctx, workspaceID, models.CreateWorkspacePayload{Name: wsName, Tier: tier, Runtime: dbRuntime, Template: dbTemplate})

	// RFC#2843 #33 + SaaS restart re-stub fix: on SaaS (cpProv), restore the
	// persisted template AND resolve its LOCAL template dir so the re-provision
	// re-delivers config.yaml + prompts.
	//
	// Setting payload.Template alone is NOT enough: it only drives the fetcher's
	// TemplateAssets wire field, which the deployed control-plane does NOT consume
	// (CP /cp/workspaces/provision reads `config_files` only — see
	// controlplane wsProvisionRequest). config.yaml/prompts reach a SaaS box
	// EXCLUSIVELY via `config_files`, which collectCPConfigFiles populates by
	// walking cfg.TemplatePath. The restart previously passed templatePath="" →
	// config routed only into the dropped TemplateAssets → every SaaS restart
	// (each a fresh EC2 with empty /configs) landed a 218-byte stub config.yaml.
	// That is the template-delivery-e2e intermittent (loses the race vs the
	// post-online plugin-reconcile restart) AND a prod identity-loss-on-restart
	// bug. Re-applying only touches template-owned, allowlisted files
	// (config.yaml, prompts/**); user/agent paths (CLAUDE.md, MEMORY.md,
	// .claude/**, /workspace) are excluded by IsCPTemplateAssetPath, so a
	// persisted /configs is never clobbered. Docker keeps templatePath="" to
	// preserve its persistent config volume.
	restartTemplatePath := ""
	if h.cpProv != nil {
		if storedTmpl := storedWorkspaceTemplate(ctx, workspaceID); storedTmpl != "" {
			payload.Template = storedTmpl
			if p, resolveErr := resolveWorkspaceTemplatePath(h.configsDir, h.cacheDir, storedTmpl); resolveErr != nil {
				log.Printf("Auto-restart: %s template %q path resolution failed: %v — re-provision may land a stub config", workspaceID, storedTmpl, resolveErr)
			} else if _, statErr := os.Stat(p); statErr == nil {
				restartTemplatePath = p
			} else {
				log.Printf("Auto-restart: %s template %q dir absent at %q — re-provision may land a stub config", workspaceID, storedTmpl, p)
			}
		}
	}

	// RFC#2843 #33: on SaaS (cpProv), restore the persisted template so the
	// re-provision re-delivers config.yaml + prompts — TemplateIdentity is
	// derived from payload.Template (workspace_provision.go). Without this the
	// SaaS re-provision ran with template="" → 218-byte stub config + dropped
	// skills on every restart. Docker keeps its persistent config volume, so it
	// retains the "do not re-apply templates" behavior (template left empty).
	if h.cpProv != nil {
		if storedTmpl := storedWorkspaceTemplate(ctx, workspaceID); storedTmpl != "" {
			payload.Template = storedTmpl
		}
	}

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
	// core#2771: call the *Locked* variant — the per-workspace provision
	// gate is ALREADY held by this cycle (acquired at the top of
	// runRestartCycle, held for the entire Stop+Start). Calling the
	// unlocked variant would re-lock the same non-reentrant sync.Mutex
	// and deadlock the programmatic restart path.
	//
	// Pre-2026-05-05 this site inlined the if-cpProv-else dispatch. On
	// SaaS the cycle would NPE inside provisionWorkspace's
	// `h.provisioner.VolumeHasFile` call, get swallowed by
	// coalesceRestart's recover()-without-re-raise (a platform-stability
	// safeguard), and leave the workspace permanently stuck in
	// status='provisioning' (the UPDATE above already ran). User-
	// observable result on SaaS pre-fix: dead workspace → manual canvas
	// restart was the only recovery path.
	h.provisionWorkspaceAutoSyncLocked(workspaceID, restartTemplatePath, nil, payload)
	// sendRestartContext is a one-way notification to the new container; safe
	// to fire async — the next restart cycle won't depend on it completing.
	// Tracked via h.goAsync so tests can wait for it via h.asyncWG before
	// closing the sqlmock. Without this, untracked goroutines hit the restored
	// mock and cause "was not expected" errors in parallel CI execution (mc#1264).
	h.goAsync(func() { h.sendRestartContext(workspaceID, restartData) })
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
	var descendantList []gin.H
	rows, err := db.DB.QueryContext(ctx,
		`WITH RECURSIVE descendants AS (
			SELECT id, name FROM workspaces WHERE parent_id = $1 AND status NOT IN ('removed', 'paused')
			UNION ALL
			SELECT w.id, w.name FROM workspaces w JOIN descendants d ON w.parent_id = d.id WHERE w.status NOT IN ('removed', 'paused')
		) SELECT id, name FROM descendants`, id)
	if err != nil {
		log.Printf("Pause: descendant query failed for %s: %v", id, err)
	}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var cid, cname string
			if rows.Scan(&cid, &cname) == nil {
				toPause = append(toPause, struct{ id, name string }{cid, cname})
				descendantList = append(descendantList, gin.H{"id": cid, "name": cname})
			}
		}
		if err := rows.Err(); err != nil {
			log.Printf("Pause: descendant query rows.Err: %v", err)
		}
	}

	// Default: single-workscope pause unless ?cascade=true
	if c.Query("cascade") != "true" && len(descendantList) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":       "workspace has descendants — use ?cascade=true to pause all",
			"descendants": descendantList,
		})
		return
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
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = $1, url = '', updated_at = now() WHERE id = $2`, models.StatusPaused, ws.id); err != nil {
			log.Printf("Pause: failed to set paused status for %s: %v", ws.id, err)
		}
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

	var wsName, dbRuntime, dbTemplate string
	var tier int
	err := db.DB.QueryRowContext(ctx,
		`SELECT name, tier, COALESCE(runtime, 'claude-code'), COALESCE(template, '') FROM workspaces WHERE id = $1 AND status = 'paused'`, id,
	).Scan(&wsName, &tier, &dbRuntime, &dbTemplate)
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
		id, name, runtime, template string
		tier                        int
	}
	toResume := []wsInfo{{id, wsName, dbRuntime, dbTemplate, tier}}
	var descendantList []gin.H
	rows, err := db.DB.QueryContext(ctx,
		`WITH RECURSIVE descendants AS (
			SELECT id, name, tier, COALESCE(runtime, 'claude-code') AS runtime, COALESCE(template, '') AS template FROM workspaces WHERE parent_id = $1 AND status = 'paused'
			UNION ALL
			SELECT w.id, w.name, w.tier, COALESCE(w.runtime, 'claude-code'), COALESCE(w.template, '') FROM workspaces w JOIN descendants d ON w.parent_id = d.id WHERE w.status = 'paused'
		) SELECT id, name, tier, runtime, template FROM descendants`, id)
	if err != nil {
		log.Printf("Resume: descendant query failed for %s: %v", id, err)
	}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var ws wsInfo
			if rows.Scan(&ws.id, &ws.name, &ws.tier, &ws.runtime, &ws.template) == nil {
				toResume = append(toResume, ws)
				descendantList = append(descendantList, gin.H{"id": ws.id, "name": ws.name})
			}
		}
		if err := rows.Err(); err != nil {
			log.Printf("Resume: descendant query rows.Err: %v", err)
		}
	}

	// Default: single-workspace resume unless ?cascade=true
	if c.Query("cascade") != "true" && len(descendantList) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":       "workspace has descendants — use ?cascade=true to resume all",
			"descendants": descendantList,
		})
		return
	}

	// Re-provision all
	for _, ws := range toResume {
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2`, models.StatusProvisioning, ws.id); err != nil {
			log.Printf("Resume: failed to set provisioning status for %s: %v", ws.id, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), ws.id, map[string]interface{}{
			"name": ws.name, "tier": ws.tier, "runtime": ws.runtime, "template": ws.template,
		})
		// Phase 1 template decoupling: the workspace row stores the template
		// explicitly, so resume carries it through CreateWorkspacePayload.
		payload := withStoredCompute(ctx, ws.id, models.CreateWorkspacePayload{Name: ws.name, Tier: ws.tier, Runtime: ws.runtime, Template: ws.template})
		// RFC#2843 #33: if the row template is empty (legacy row), restore the
		// persisted template on SaaS resume so config + prompts re-deliver.
		if payload.Template == "" && h.cpProv != nil {
			if storedTmpl := storedWorkspaceTemplate(ctx, ws.id); storedTmpl != "" {
				payload.Template = storedTmpl
			}
		}
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
