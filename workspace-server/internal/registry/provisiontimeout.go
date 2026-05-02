package registry

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
)

// ProvisionTimeoutEmitter is the narrow broadcaster dependency the sweeper
// needs. Defined locally so the registry package stays event-bus agnostic
// (same pattern as OfflineHandler in healthsweep.go).
type ProvisionTimeoutEmitter interface {
	RecordAndBroadcast(ctx context.Context, eventType string, workspaceID string, payload interface{}) error
}

// DefaultProvisioningTimeout is how long a workspace may sit in
// status='provisioning' before the sweeper flips it to 'failed'.
// Default for non-hermes runtimes (claude-code, langgraph, crewai,
// autogen, etc.) which cold-boot in <5 min. The container-launch path
// has its own 3-minute context timeout (provisioner.ProvisionTimeout)
// but that only bounds the docker API call — a container that started
// but crashes before /registry/register never triggers that path and
// would sit in provisioning forever. 10 minutes covers pathological
// image-pull + user-data execution on a cold EC2 worker while still
// getting well ahead of the "15+ minute" stuck state users see in
// production.
const DefaultProvisioningTimeout = 10 * time.Minute

// HermesProvisioningTimeout matches the CP bootstrap-watcher's
// runtime-aware deadline (cp#245) for hermes workspaces: 25 min watcher
// + 5 min sweep slack. Hermes cold-boot does apt + uv + Python venv +
// Node + hermes-agent install — 13–25 min on slow apt mirrors is
// normal. Without this, the sweep would flip the workspace to 'failed'
// at 10 min while the watcher (and the workspace itself) is still
// happily progressing through install. Issue #1843 follow-up: a
// healthy 10.5-min hermes boot was killed by the 10-min sweep on
// 2026-04-26, breaking #2061's E2E.
const HermesProvisioningTimeout = 30 * time.Minute

// DefaultProvisionSweepInterval is how often the sweeper polls. Same cadence
// as the hibernation monitor — cheap and bounded by the provisioning-state
// query which hits the primary key / status partial index.
const DefaultProvisionSweepInterval = 30 * time.Second

// RuntimeTimeoutLookup returns the per-runtime provision timeout in
// seconds when a template's config.yaml declared
// `runtime_config.provision_timeout_seconds`, else zero (= "no override,
// fall through to runtime defaults below"). Same shape as
// runtimeProvisionTimeoutsCache.get in handlers — wired through main.go
// so this package stays template-discovery agnostic.
//
// Why an interface instead of importing the cache directly: registry
// already sits below handlers in the import graph (handlers → registry,
// not the reverse). A function-typed argument keeps that flow.
type RuntimeTimeoutLookup func(runtime string) int

// provisioningTimeoutFor picks the per-runtime sweep deadline. Resolution
// order:
//
//  1. PROVISION_TIMEOUT_SECONDS env — global override, ops-debug only.
//  2. Template manifest override (lookup) — what the canvas spinner
//     also reads via #2054 phase 2. Without this, a template that
//     declared `runtime_config.provision_timeout_seconds: 900` would
//     still get killed by the sweeper at the 10-min hardcoded floor —
//     a real wiring gap that drove every claude-code burst on a cold
//     EC2 to false-positive timeout.
//  3. Hermes special-case (CP bootstrap-watcher 25 min + 5 min slack).
//  4. DefaultProvisioningTimeout (10 min) for everything else.
//
// lookup may be nil (during package tests, or before main.go has wired
// it) — falls through to the legacy hermes/default split.
func provisioningTimeoutFor(runtime string, lookup RuntimeTimeoutLookup) time.Duration {
	if v := os.Getenv("PROVISION_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if lookup != nil {
		if secs := lookup(runtime); secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	if runtime == "hermes" {
		return HermesProvisioningTimeout
	}
	return DefaultProvisioningTimeout
}

// StartProvisioningTimeoutSweep periodically scans for workspaces stuck in
// `status='provisioning'` past the timeout window, flips them to `failed`,
// and broadcasts a WORKSPACE_PROVISION_TIMEOUT event so the canvas can
// render a fail-state instead of the indefinite cosmetic "Provisioning
// Timeout" banner.
//
// The sweep is idempotent: the UPDATE's WHERE clause re-checks both status
// and age under the same row lock, so a workspace that raced to `online` or
// was restarted while the sweep was scanning will not get flipped.
func StartProvisioningTimeoutSweep(ctx context.Context, emitter ProvisionTimeoutEmitter, interval time.Duration, lookup RuntimeTimeoutLookup) {
	if emitter == nil {
		log.Println("Provision-timeout sweep: emitter is nil — skipping (no one to broadcast to)")
		return
	}
	if interval <= 0 {
		interval = DefaultProvisionSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("Provision-timeout sweep: started (interval=%s, timeout=%s default / %s hermes / per-runtime manifest override=%v)",
		interval, DefaultProvisioningTimeout, HermesProvisioningTimeout, lookup != nil)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepStuckProvisioning(ctx, emitter, lookup)
		}
	}
}

// sweepStuckProvisioning is one tick of the sweeper. Exported-for-test via
// the package boundary: keep all time.Now reads inside so tests can drive it
// deterministically by seeding updated_at rather than manipulating time.
//
// Runtime-aware: the per-workspace timeout depends on `runtime`. Hermes
// gets 30 min (matching the CP bootstrap-watcher's 25-min deadline + 5
// min slack); everything else gets 10 min. Without this distinction a
// healthy hermes cold-boot at 10–25 min got killed mid-install by this
// sweep, leaving an incoherent "marked failed but actually working"
// state. See bootstrap_watcher.go's bootstrapTimeoutFn for the
// canonical CP-side gating.
func sweepStuckProvisioning(ctx context.Context, emitter ProvisionTimeoutEmitter, lookup RuntimeTimeoutLookup) {
	// We can't pre-filter by age in SQL because the threshold depends
	// on the row's runtime. Pull every provisioning row + its runtime
	// + its age, evaluate per-row in Go. Still cheap — the
	// status='provisioning' row count is bounded (workspaces in
	// flight, not historical) and the partial index on status keeps
	// it fast.
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, COALESCE(runtime, ''), EXTRACT(EPOCH FROM (now() - updated_at))::int
		FROM workspaces
		WHERE status = 'provisioning'
	`)
	if err != nil {
		log.Printf("Provision-timeout sweep: query error: %v", err)
		return
	}
	defer rows.Close()

	type candidate struct {
		id      string
		runtime string
		ageSec  int
	}
	var ids []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.runtime, &c.ageSec); err == nil {
			ids = append(ids, c)
		}
	}

	for _, c := range ids {
		timeout := provisioningTimeoutFor(c.runtime, lookup)
		timeoutSec := int(timeout / time.Second)
		if c.ageSec < timeoutSec {
			continue
		}
		msg := "provisioning timed out — container started but never called /registry/register. Check container logs and network connectivity to the platform."
		res, err := db.DB.ExecContext(ctx, `
			UPDATE workspaces
			   SET status = $4,
			       last_sample_error = $2,
			       updated_at = now()
			 WHERE id = $1
			   AND status = 'provisioning'
			   AND updated_at < now() - ($3 || ' seconds')::interval
		`, c.id, msg, timeoutSec, models.StatusFailed)
		if err != nil {
			log.Printf("Provision-timeout sweep: failed to flip %s to failed: %v", c.id, err)
			continue
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			// Raced with restart / register — no harm, just skip.
			continue
		}
		log.Printf("Provision-timeout sweep: %s (runtime=%q) stuck in provisioning > %s — marked failed", c.id, c.runtime, timeout)
		// Emit as WORKSPACE_PROVISION_FAILED, not _TIMEOUT, because the
		// canvas event handler only flips node state on the _FAILED case.
		// A separate event type was considered but the UI reaction is
		// identical either way — operators who need to distinguish can
		// tell from the `source` payload field.
		if emitErr := emitter.RecordAndBroadcast(ctx, "WORKSPACE_PROVISION_FAILED", c.id, map[string]interface{}{
			"error":        msg,
			"timeout_secs": timeoutSec,
			"runtime":      c.runtime,
			"source":       "provision_timeout_sweep",
		}); emitErr != nil {
			log.Printf("Provision-timeout sweep: broadcast failed for %s: %v", c.id, emitErr)
		}
	}
}
