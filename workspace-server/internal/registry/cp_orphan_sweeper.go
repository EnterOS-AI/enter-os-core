package registry

// cp_orphan_sweeper.go — SaaS-mode counterpart to orphan_sweeper.go.
//
// The Docker sweeper (StartOrphanSweeper) runs only when prov != nil
// (single-tenant Docker mode); SaaS tenants run cpProv != nil and prov
// == nil, so they get no sweep coverage from that path. This file fills
// the gap for the deprovision split-write race documented in #2989:
//
//	1. handlers/workspace_crud.go:365 marks workspaces.status = 'removed'.
//	2. workspace_crud.go:439 calls StopWorkspaceAuto → cpProv.Stop, which
//	   issues DELETE /cp/workspaces/:id?instance_id=… to controlplane.
//	3. If step 2 fails (CP transient 5xx, network blip, AWS hiccup), the
//	   inline path returns a 500 to the canvas — but the DB row is already
//	   at status='removed' with instance_id still populated. There's no
//	   retry, and the EC2 lives forever.
//
// This sweeper closes that gap by re-issuing cpProv.Stop on every cycle
// for any workspace at status='removed' with a non-NULL instance_id.
// Stop is idempotent: AWS TerminateInstance on an already-terminated
// instance is a no-op (per AWS docs), and CP's Deprovision handler
// (controlplane/internal/handlers/workspace_provision.go:289) handles
// the already-terminated and already-deleted-DNS cases via best-effort
// guards. On Stop success, the sweeper clears instance_id so the next
// cycle skips the row.
//
// Cadence + safety filters mirror the Docker sweeper:
//   - 60s tick (OrphanSweepInterval)
//   - 30s per-cycle deadline (orphanSweepDeadline)
//   - LIMIT 100 per cycle so a sustained CP outage that backs up many
//     orphans doesn't blow the request timeout; subsequent cycles drain.
//
// SSOT note: Stop's idempotency (no-op on empty instance_id, AWS
// terminate on already-terminated) is the load-bearing invariant. Any
// future change that adds non-idempotent side effects to cpProv.Stop
// must also gate this sweeper, or it will re-execute those side effects
// every 60s for every cleared-but-not-yet-NULL row.

import (
	"context"
	"log"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// CPOrphanReaper is the dependency the SaaS-mode sweeper takes from
// the CP provisioner. *provisioner.CPProvisioner satisfies this
// naturally; tests inject fakes.
type CPOrphanReaper interface {
	Stop(ctx context.Context, workspaceID string) error
}

// cpSweepLimit caps the per-cycle row count so a sustained CP outage
// can't make a single sweep cycle blow orphanSweepDeadline. With a
// 60s cadence and 100-row limit, drain rate is up to 100 orphans/min,
// which has never been approached even during the worst leak windows.
const cpSweepLimit = 100

// StartCPOrphanSweeper runs the SaaS-mode reconcile loop until ctx is
// cancelled. nil reaper makes the loop a no-op (matches the Docker
// sweeper's nil-tolerant pattern).
//
// Caller is expected to gate on `cpProv != nil` (matching how
// StartOrphanSweeper is gated on `prov != nil` at the call site in
// cmd/server/main.go) — passing a nil *CPProvisioner here would also
// short-circuit but the gate at the wiring site keeps the call shape
// symmetric across the two sweepers.
func StartCPOrphanSweeper(ctx context.Context, reaper CPOrphanReaper) {
	if reaper == nil {
		log.Println("CP orphan sweeper: reaper is nil — sweeper disabled")
		return
	}
	log.Printf("CP orphan sweeper started — reconciling every %s", OrphanSweepInterval)
	ticker := time.NewTicker(OrphanSweepInterval)
	defer ticker.Stop()
	cpSweepOnce(ctx, reaper)
	for {
		select {
		case <-ctx.Done():
			log.Println("CP orphan sweeper: shutdown")
			return
		case <-ticker.C:
			cpSweepOnce(ctx, reaper)
		}
	}
}

// cpSweepOnce executes one reconcile pass. Defensive against db.DB
// being nil so a misconfigured boot doesn't panic.
func cpSweepOnce(parent context.Context, reaper CPOrphanReaper) {
	if db.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, orphanSweepDeadline)
	defer cancel()

	rows, err := db.DB.QueryContext(ctx, `
		SELECT id::text
		  FROM workspaces
		 WHERE status = 'removed'
		   AND instance_id IS NOT NULL
		   AND instance_id != ''
		 ORDER BY updated_at DESC
		 LIMIT $1
	`, cpSweepLimit)
	if err != nil {
		log.Printf("CP orphan sweeper: DB query failed: %v", err)
		return
	}
	defer rows.Close()

	var orphanIDs []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			log.Printf("CP orphan sweeper: row scan failed: %v", scanErr)
			continue
		}
		orphanIDs = append(orphanIDs, id)
	}
	if iterErr := rows.Err(); iterErr != nil {
		log.Printf("CP orphan sweeper: rows iteration failed: %v", iterErr)
		return
	}

	for _, id := range orphanIDs {
		log.Printf("CP orphan sweeper: terminating leaked EC2 for removed workspace %s", id)
		if stopErr := reaper.Stop(ctx, id); stopErr != nil {
			// CP-side error — transient 5xx, network, AWS hiccup. Leave
			// instance_id populated so the next cycle retries. Loud-fail
			// only at the log layer; the user-visible 500 was already
			// returned by the inline path that triggered this orphan.
			log.Printf("CP orphan sweeper: Stop failed for %s: %v — retry next cycle", id, stopErr)
			continue
		}
		// Stop succeeded — clear instance_id so the next cycle skips this
		// row. We can't use a tombstone column (no schema change in this
		// PR); NULL'ing instance_id is the SSOT signal for "no live
		// EC2 attached." The matching SELECT predicate above stays in
		// sync with this UPDATE.
		if _, updErr := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET instance_id = NULL, updated_at = now() WHERE id = $1`,
			id,
		); updErr != nil {
			log.Printf("CP orphan sweeper: clear instance_id failed for %s: %v — next cycle will re-Stop (idempotent)", id, updErr)
		}
	}
}
