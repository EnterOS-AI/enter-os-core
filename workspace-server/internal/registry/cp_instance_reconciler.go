package registry

// cp_instance_reconciler.go — authoritative EC2-state reconcile for
// SaaS workspaces (core#2261).
//
// Root cause (core#2247): every existing liveness pass keys off a PROXY
// for "is this workspace alive?":
//
//   - StartLivenessMonitor   — Redis TTL expiry (agent stopped heartbeating).
//   - StartHealthSweep (Docker pass) — local Docker daemon (prov != nil only).
//   - StartHealthSweep (remote pass) — last_heartbeat_at freshness for
//     runtime='external' rows.
//   - StartCPOrphanSweeper    — status='removed' rows with a stray instance_id.
//
// A SaaS claude-code workspace whose EC2 was terminated/stopped out from
// under us (manual AWS action, spot reclaim, CP-side reap, etc.) falls
// through ALL of them: it's not 'removed' (so the orphan sweeper skips
// it), it's not runtime='external' (so the heartbeat pass skips it), and
// on a pure-SaaS front-door prov == nil so the Docker pass never runs.
// The registry kept status='online' pointing at a dead instance forever.
//
// This sweeper closes that gap with the ONE authoritative check the
// others lack: CPProvisioner.IsRunning, which ultimately asks the
// control-plane "is this EC2 actually running?" (DescribeInstances-
// equivalent). When the answer is a CLEAN "no" it feeds the workspace
// into the EXISTING offline/auto-heal machinery (onOffline → status flip
// + RestartByID reprovision with the existing volume) — no new healing
// path, just real ground truth driving the one we already have.
//
// Guardrails:
//   - FAIL-SAFE: IsRunning is (true, err) on any transient DB/transport
//     error and (false, nil) ONLY when CP genuinely reports the instance
//     is not running. We act ONLY on (false, nil); any err short-circuits
//     to "leave it alone" so a CP blip never flips a healthy workspace.
//   - ONLINE + SaaS ONLY: status='online', instance_id present, and
//     runtime <> 'external'. Paused/hibernated/removed/provisioning/
//     awaiting_agent rows are out of scope; external rows are covered by
//     the remote-heartbeat pass.
//   - Per-cycle row cap + per-cycle deadline + per-workspace timeout so
//     one slow CP call (or a degraded-but-not-erroring CP) can't stall
//     the sweep.
//   - TOCTOU re-confirm before any flip: IsRunning resolves instance_id
//     independently, so a row whose instance_id was cleared/NULLed (by a
//     concurrent delete, the CP-orphan-sweeper, or a reprovision) between
//     the reconciler's SELECT and the IsRunning probe yields a STALE
//     (false, nil) that does NOT prove the EC2 is dead. We re-read the
//     row's current (status, instance_id) and flip ONLY when the SAME
//     non-empty instance we asked CP about is still the workspace's
//     recorded instance AND it's still online/degraded. Mirrors the
//     guarded-write re-confirm in healthsweep.

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

// InstanceRunningChecker is the narrow dependency the reconciler takes
// from the CP provisioner. *provisioner.CPProvisioner satisfies this
// naturally; tests inject fakes.
//
// Contract (load-bearing): IsRunning is FAIL-SAFE — it returns
// (true, err) on transient DB/transport errors and (false, nil) ONLY
// when CP reports the instance is genuinely not running. The reconciler
// flips a workspace offline strictly on (false, nil).
type InstanceRunningChecker interface {
	IsRunning(ctx context.Context, workspaceID string) (bool, error)
}

// CPInstanceReconcileLimit caps the per-cycle row count so a sustained
// CP slowdown can't make a single sweep cycle run unbounded. With a 60s
// cadence and a per-workspace timeout below, this bounds worst-case
// cycle wall-time and lets subsequent cycles drain any backlog.
const CPInstanceReconcileLimit = 200

// cpInstanceCheckTimeout bounds a single IsRunning call so one slow CP
// round-trip can't stall the whole sweep. Each workspace gets its own
// timeout context derived from the cycle context.
const cpInstanceCheckTimeout = 10 * time.Second

// cpInstanceCycleDeadline bounds the wall-time of one whole reconcile
// pass. With per-workspace 10s timeouts and a 200-row cap, a degraded-
// but-not-erroring CP (each IsRunning slow but under the per-workspace
// cap) could otherwise drag one cycle out for tens of minutes and starve
// the next tick. Mirrors cp_orphan_sweeper's orphanSweepDeadline; chosen
// under the 60s interval so a stuck cycle is abandoned before the next
// one is due and the backlog drains across subsequent cycles.
const cpInstanceCycleDeadline = 45 * time.Second

// cpInstanceReconfirmTimeout bounds the TOCTOU re-confirm read. This is a
// single indexed primary-key lookup, so it should never be slow; a tight
// timeout keeps the re-confirm from itself becoming a stall point.
const cpInstanceReconfirmTimeout = 5 * time.Second

// StartCPInstanceReconciler runs the authoritative EC2-state reconcile
// loop until ctx is cancelled. A nil checker makes the loop a no-op
// (matches the nil-tolerant pattern of the sibling CP sweeper).
//
// Caller is expected to gate on `cpProv != nil` (matching how
// StartCPOrphanSweeper is gated at the wiring site in cmd/server/main.go)
// — passing a nil *CPProvisioner here would also short-circuit, but the
// gate at the call site keeps the call shape symmetric across sweepers.
//
// interval <= 0 falls back to the default 60s cadence so a misconfigured
// caller can't spin a zero-duration ticker (which panics).
func StartCPInstanceReconciler(ctx context.Context, checker InstanceRunningChecker, onOffline OfflineHandler, interval time.Duration) {
	if checker == nil {
		log.Println("cp-instance-reconciler: checker is nil — reconciler disabled")
		return
	}
	if interval <= 0 {
		interval = 60 * time.Second
	}
	log.Printf("cp-instance-reconciler started — reconciling online SaaS workspaces against real EC2 state every %s", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Kick once at boot so a platform restart starts healing immediately
	// rather than waiting a full interval.
	reconcileOnce(ctx, checker, onOffline)
	for {
		select {
		case <-ctx.Done():
			log.Println("cp-instance-reconciler: shutdown")
			return
		case <-ticker.C:
			reconcileOnce(ctx, checker, onOffline)
		}
	}
}

// reconcileRow pairs a workspace id with the instance_id captured in the
// SAME SELECT, so the TOCTOU re-confirm can verify CP's (false, nil)
// answer is about the instance the row still records — not one cleared
// out from under us between the SELECT and the IsRunning probe.
type reconcileRow struct {
	id         string
	instanceID string
}

// reconcileOnce executes one reconcile pass. Defensive against db.DB
// being nil so a misconfigured boot doesn't panic.
//
// Scope: online/degraded + SaaS-EC2 workspaces only. runtime='external'
// rows are excluded (covered by the remote-heartbeat pass); paused/
// hibernated/removed/provisioning/awaiting_agent are excluded by the
// status filter. `degraded` is included because a SaaS workspace whose
// heartbeat handler flipped it degraded then lost its EC2 falls through
// every other sweep (matches healthsweep's `status IN ('online',
// 'degraded')`).
func reconcileOnce(parent context.Context, checker InstanceRunningChecker, onOffline OfflineHandler) {
	if db.DB == nil {
		return
	}

	// Per-cycle deadline so a degraded-but-not-erroring CP (each IsRunning
	// slow but under the per-workspace cap) can't drag one cycle out for
	// tens of minutes and starve the next tick. Per-workspace IsRunning
	// timeouts derive from this cycle context.
	cycleCtx, cancelCycle := context.WithTimeout(parent, cpInstanceCycleDeadline)
	defer cancelCycle()

	rows, err := db.DB.QueryContext(cycleCtx, `
		SELECT id::text, instance_id
		  FROM workspaces
		 WHERE status IN ('online', 'degraded')
		   AND instance_id IS NOT NULL
		   AND instance_id != ''
		   AND COALESCE(runtime, '') <> 'external'
		 ORDER BY updated_at DESC
		 LIMIT $1
	`, CPInstanceReconcileLimit)
	if err != nil {
		log.Printf("cp-instance-reconciler: DB query failed: %v", err)
		return
	}
	defer rows.Close()

	var candidates []reconcileRow
	for rows.Next() {
		var r reconcileRow
		if scanErr := rows.Scan(&r.id, &r.instanceID); scanErr != nil {
			log.Printf("cp-instance-reconciler: row scan failed: %v", scanErr)
			continue
		}
		candidates = append(candidates, r)
	}
	if iterErr := rows.Err(); iterErr != nil {
		log.Printf("cp-instance-reconciler: rows iteration failed: %v", iterErr)
		return
	}

	processed, skipped := 0, 0
	for _, c := range candidates {
		// Abandon the cycle if we've blown the per-cycle deadline; the
		// next tick re-reads from the top (ORDER BY updated_at DESC) and
		// drains the backlog. Without this a slow CP could keep one cycle
		// running past its interval and never let a fresh one start.
		if cycleCtx.Err() != nil {
			log.Printf("cp-instance-reconciler: cycle deadline reached — processed %d, %d skipped (TOCTOU/changed), remaining deferred to next cycle", processed, skipped)
			return
		}
		processed++

		// Per-workspace timeout so one slow CP round-trip can't stall
		// the whole sweep. Derived from cycleCtx so the cycle deadline
		// always dominates.
		checkCtx, cancel := context.WithTimeout(cycleCtx, cpInstanceCheckTimeout)
		running, checkErr := checker.IsRunning(checkCtx, c.id)
		cancel()

		if checkErr != nil {
			// FAIL-SAFE: transient DB/transport error (or a no-backend
			// signal). IsRunning returns (true, err) on these, so never
			// flip — leave the row online and retry next cycle.
			log.Printf("cp-instance-reconciler: IsRunning(%s) errored, leaving online (fail-safe): %v", c.id, checkErr)
			continue
		}
		if running {
			continue
		}

		// (false, nil) is NOT yet proof the EC2 is dead. IsRunning
		// resolves instance_id independently (resolveInstanceID); if the
		// row's instance_id was cleared/NULLed (concurrent delete, the
		// CP-orphan-sweeper NULLing it, a reprovision) or the row moved
		// off online/degraded between our SELECT and this probe,
		// IsRunning returns a STALE (false, nil) that reflects a missing
		// instance_id, NOT a confirmed-terminated EC2. Re-confirm against
		// the row's CURRENT state and flip ONLY when the SAME non-empty
		// instance we asked CP about is still recorded AND the row is
		// still online/degraded. Mirrors healthsweep's guarded write.
		if !reconfirmStillOfflineCandidate(cycleCtx, c) {
			skipped++
			continue
		}

		// CONFIRMED "not running" — CP authoritatively reports the EC2 is
		// terminated/stopped/absent AND the row still records that exact
		// instance as online/degraded. Feed it into the existing offline +
		// auto-heal machinery: onOffline flips the row offline and
		// triggers RestartByID, which reprovisions with the existing
		// volume.
		log.Printf("cp-instance-reconciler: workspace %s (instance %s) is online/degraded but its EC2 is not running (terminated/stopped) — flipping offline + triggering reprovision", c.id, c.instanceID)
		if onOffline != nil {
			onOffline(cycleCtx, c.id)
		}
	}
}

// reconfirmStillOfflineCandidate re-reads the workspace's CURRENT
// (status, instance_id) and reports whether it is STILL a valid offline
// candidate for the instance we just probed. It returns true ONLY when:
//
//   - the row still exists, AND
//   - current status IN ('online','degraded'), AND
//   - current instance_id is non-empty, AND
//   - current instance_id == the instance_id captured in the original
//     SELECT (the one whose liveness CP just answered about).
//
// Any other outcome (row gone, status moved off online/degraded,
// instance_id cleared or now points at a different instance) means the
// IsRunning (false, nil) was a stale/cleared-instance snapshot rather
// than a confirmed-terminated EC2 — return false so the caller skips the
// flip. A DB error during re-confirm is treated as "not confirmed"
// (false): fail-safe toward NOT flipping a workspace we can't re-verify.
func reconfirmStillOfflineCandidate(parent context.Context, c reconcileRow) bool {
	if db.DB == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, cpInstanceReconfirmTimeout)
	defer cancel()

	var curStatus, curInstanceID string
	err := db.DB.QueryRowContext(ctx, `
		SELECT status, COALESCE(instance_id, '')
		  FROM workspaces
		 WHERE id = $1
	`, c.id).Scan(&curStatus, &curInstanceID)
	if err != nil {
		if err == sql.ErrNoRows {
			// Row deleted between SELECT and re-confirm — definitely not a
			// terminated-EC2 signal. Skip.
			log.Printf("cp-instance-reconciler: re-confirm %s: row gone — skipping flip (stale snapshot, not a dead EC2)", c.id)
			return false
		}
		// Transient DB error — fail-safe toward NOT flipping.
		log.Printf("cp-instance-reconciler: re-confirm %s errored, skipping flip (fail-safe): %v", c.id, err)
		return false
	}

	if curStatus != "online" && curStatus != "degraded" {
		log.Printf("cp-instance-reconciler: re-confirm %s: status moved to %q since SELECT — skipping flip", c.id, curStatus)
		return false
	}
	if curInstanceID == "" {
		log.Printf("cp-instance-reconciler: re-confirm %s: instance_id cleared since SELECT — skipping flip (CP answered about a now-detached instance)", c.id)
		return false
	}
	if curInstanceID != c.instanceID {
		log.Printf("cp-instance-reconciler: re-confirm %s: instance_id changed %s -> %s since SELECT (reprovision) — skipping flip", c.id, c.instanceID, curInstanceID)
		return false
	}
	return true
}
