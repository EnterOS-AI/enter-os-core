package registry

// orphan_sweeper.go — periodic reconcile pass that cleans up Docker
// containers whose corresponding workspace row in Postgres has
// status='removed'. Defence in depth on top of the inline cleanup
// in handlers/workspace_crud.go.
//
// Why this exists: the inline cleanup is one-shot — if Docker hiccups
// (daemon restart, host load, transient API error), the container
// silently stays alive while the DB row is already 'removed'. Without
// a reconcile pass those leaks accumulate forever. With one, every
// missed cleanup heals on the next sweep.
//
// Cost: O(running containers) per cycle, not O(historical removed
// rows). The Docker name filter trims the candidate set to ws-* only
// (typically the same handful as ContainerList without filter on a
// dev host); the DB lookup is one indexed query against the
// idx_workspaces_status btree.

import (
	"context"
	"log"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/lib/pq"
)

// OrphanReaper is the dependency the sweeper takes from provisioner.
// Extracted as an interface so the sweeper is unit-testable without
// a real Docker daemon — matches the ContainerChecker pattern in
// healthsweep.go. *provisioner.Provisioner satisfies this naturally.
type OrphanReaper interface {
	ListWorkspaceContainerIDPrefixes(ctx context.Context) ([]string, error)
	// ListManagedContainerIDPrefixes returns containers carrying the
	// provisioner's LabelManaged stamp — the "definitely ours" set.
	// Used by the wiped-DB reap pass: a labeled container with no
	// matching workspaces row is something a previous platform process
	// created but whose DB row is gone (e.g. operator did
	// `docker compose down -v` then back up). Without this pass those
	// orphans leak forever, since the existing status='removed' query
	// finds zero matches against a wiped table.
	ListManagedContainerIDPrefixes(ctx context.Context) ([]string, error)
	Stop(ctx context.Context, workspaceID string) error
	RemoveVolume(ctx context.Context, workspaceID string) error
}

// isLikelyWorkspaceID accepts strings shaped like a UUID prefix —
// hex chars and `-` only. Workspace IDs are full UUIDs and the
// container-name truncation keeps the hex prefix intact, so any
// container name that doesn't match this is by definition not one
// of ours and should be skipped. Also doubles as a SQL LIKE
// wildcard guard (rejects `_` and `%`).
func isLikelyWorkspaceID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// OrphanSweepInterval is the cadence of the reconcile loop. 60s
// matches the heartbeat cadence (30s) × 2 — a single missed cleanup
// surfaces within ~90s end-to-end (canvas delete → next sweep tick →
// container gone). Faster cycles would just pay Docker API cost for
// no UX win; slower would let leaks linger long enough to compound
// CPU pressure on dev hosts.
const OrphanSweepInterval = 60 * time.Second

// orphanSweepDeadline bounds a single sweep cycle. A daemon at the
// edge of timing out shouldn't accumulate goroutines. 30s is generous
// for a dev host with dozens of containers and a busy daemon.
const orphanSweepDeadline = 30 * time.Second

// StartOrphanSweeper runs the reconcile loop until ctx is cancelled.
// nil reaper makes the loop a no-op (matches handlers'
// nil-provisioner-tolerant pattern — some test harnesses run without
// Docker available).
func StartOrphanSweeper(ctx context.Context, reaper OrphanReaper) {
	if reaper == nil {
		log.Println("Orphan sweeper: reaper is nil — sweeper disabled")
		return
	}
	log.Printf("Orphan sweeper started — reconciling every %s", OrphanSweepInterval)
	ticker := time.NewTicker(OrphanSweepInterval)
	defer ticker.Stop()
	// Run once immediately so a platform restart cleans up any
	// containers leaked while we were down — don't make the user
	// wait 60s for the first reconcile.
	sweepOnce(ctx, reaper)
	for {
		select {
		case <-ctx.Done():
			log.Println("Orphan sweeper: shutdown")
			return
		case <-ticker.C:
			sweepOnce(ctx, reaper)
		}
	}
}

func sweepOnce(parent context.Context, reaper OrphanReaper) {
	ctx, cancel := context.WithTimeout(parent, orphanSweepDeadline)
	defer cancel()

	// Three independent passes. Each handles its own short-circuit; an
	// empty result or transient error in one must NOT stop the others,
	// since the wiped-DB pass exists precisely for cases where the
	// removed-row pass finds zero candidates (DB has been dropped) and
	// the stale-token pass exists for the mirror case (DB persists but
	// /configs volume has been wiped).
	sweepRemovedRows(ctx, reaper)
	sweepLabeledOrphansWithoutRows(ctx, reaper)
	sweepStaleTokensWithoutContainer(ctx, reaper)
}

// sweepRemovedRows is the original sweep: ws-* containers (by name
// filter) whose workspace row has status='removed' get reaped.
// Conservative — only acts on rows the platform explicitly marked
// for cleanup. Runs every cycle.
func sweepRemovedRows(ctx context.Context, reaper OrphanReaper) {
	prefixes, err := reaper.ListWorkspaceContainerIDPrefixes(ctx)
	if err != nil {
		log.Printf("Orphan sweeper: ListWorkspaceContainerIDPrefixes failed: %v — skipping removed-row pass", err)
		return
	}
	if len(prefixes) == 0 {
		return
	}

	// Resolve each prefix to a full workspace_id whose status is
	// 'removed'. The platform's workspace IDs are full UUIDs but
	// container names are truncated to 12 chars — an UPPER BOUND
	// of one match per prefix is guaranteed by the DB (UUID v4
	// collisions in the first 12 chars across active rows are
	// statistically negligible). Use a single IN-style query so
	// the cost is one round-trip regardless of leak count.
	//
	// Defence: drop any prefix whose contents fall outside the
	// hex-and-dash UUID alphabet. Workspace IDs are UUIDs, so
	// container names follow ws-<12 hex chars>. Anything else is
	// either a non-workspace container that slipped past the
	// substring-match Docker filter (workspace-runner, etc.) or a
	// malformed entry — neither should be turned into a LIKE
	// pattern. Also blocks SQL LIKE wildcards (`_` and `%`) from
	// reaching the query, even though Docker's container-name
	// validation would already have rejected them upstream.
	likes := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if !isLikelyWorkspaceID(p) {
			continue
		}
		likes = append(likes, p+"%")
	}
	if len(likes) == 0 {
		return
	}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id::text
		  FROM workspaces
		 WHERE status = 'removed'
		   AND id::text LIKE ANY($1::text[])
	`, pq.Array(likes))
	if err != nil {
		log.Printf("Orphan sweeper: DB query failed: %v — skipping removed-row pass", err)
		return
	}
	defer rows.Close()

	var orphanIDs []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			log.Printf("Orphan sweeper: row scan failed: %v", scanErr)
			continue
		}
		orphanIDs = append(orphanIDs, id)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Orphan sweeper: rows iteration failed: %v", err)
		return
	}

	for _, id := range orphanIDs {
		log.Printf("Orphan sweeper: stopping leaked container for removed workspace %s", id)
		if stopErr := reaper.Stop(ctx, id); stopErr != nil {
			// Stop returns the wrapped Docker error (treating
			// "container not found" as nil-success via
			// isContainerNotFound), so a non-nil here means the
			// container is genuinely still alive — daemon timeout,
			// ctx cancellation, or a transient socket EOF.
			// Skip RemoveVolume so we don't fall into the same
			// Stop-failed-then-volume-in-use trap that motivated
			// this sweeper. The next cycle (60s out) retries Stop.
			log.Printf("Orphan sweeper: Stop failed for %s: %v — leaving volume for next cycle", id, stopErr)
			continue
		}
		if rmErr := reaper.RemoveVolume(ctx, id); rmErr != nil {
			log.Printf("Orphan sweeper: RemoveVolume warning for %s: %v", id, rmErr)
		}
	}
}

// sweepLabeledOrphansWithoutRows reaps containers carrying our
// LabelManaged stamp whose workspace row has been deleted entirely
// (i.e. the row doesn't exist at all, not merely status='removed').
//
// This catches the wiped-DB case: operator does
// `docker compose down -v`, killing the postgres volume. Containers
// keep running. Platform comes back up with an empty workspaces table.
// The first pass finds nothing because there are no status='removed'
// rows. Without this second pass, those containers leak forever.
//
// Safe under multi-platform-on-shared-daemon because the label is
// stamped only by the provisioner: a sibling stack's containers won't
// carry it, so this pass leaves them alone.
func sweepLabeledOrphansWithoutRows(ctx context.Context, reaper OrphanReaper) {
	managedPrefixes, err := reaper.ListManagedContainerIDPrefixes(ctx)
	if err != nil {
		log.Printf("Orphan sweeper: ListManagedContainerIDPrefixes failed: %v — skipping wiped-DB pass", err)
		return
	}
	if len(managedPrefixes) == 0 {
		return
	}
	managedLikes := make([]string, 0, len(managedPrefixes))
	keep := make([]string, 0, len(managedPrefixes))
	for _, p := range managedPrefixes {
		if !isLikelyWorkspaceID(p) {
			continue
		}
		managedLikes = append(managedLikes, p+"%")
		keep = append(keep, p) // index-aligned with managedLikes
	}
	if len(managedLikes) == 0 {
		return
	}
	// Find prefixes that match SOME workspace row (any status). Anything
	// in managedLikes NOT in this returned set is the wiped-DB orphan
	// set — labeled, no row, ours to reap.
	knownRows, err := db.DB.QueryContext(ctx, `
		SELECT lk
		  FROM unnest($1::text[]) AS lk
		 WHERE EXISTS (
		     SELECT 1 FROM workspaces WHERE id::text LIKE lk
		 )
	`, pq.Array(managedLikes))
	if err != nil {
		log.Printf("Orphan sweeper: wiped-DB reverse-lookup failed: %v — skipping wiped-DB pass", err)
		return
	}
	known := make(map[string]struct{}, len(managedLikes))
	for knownRows.Next() {
		var lk string
		if scanErr := knownRows.Scan(&lk); scanErr != nil {
			log.Printf("Orphan sweeper: wiped-DB row scan failed: %v", scanErr)
			continue
		}
		known[lk] = struct{}{}
	}
	if cerr := knownRows.Close(); cerr != nil {
		log.Printf("Orphan sweeper: wiped-DB rows close failed: %v", cerr)
	}
	if iterErr := knownRows.Err(); iterErr != nil {
		log.Printf("Orphan sweeper: wiped-DB rows iteration failed: %v", iterErr)
		return
	}

	for i, lk := range managedLikes {
		if _, ok := known[lk]; ok {
			continue
		}
		// Reap by container-name prefix. ContainerName/Stop/RemoveVolume
		// truncate to 12 chars internally, so passing the prefix as the
		// "workspace ID" resolves to the same `ws-<prefix>` container.
		prefix := keep[i]
		log.Printf("Orphan sweeper: reaping untracked managed container ws-%s (no DB row — wiped-DB orphan)", prefix)
		if stopErr := reaper.Stop(ctx, prefix); stopErr != nil {
			log.Printf("Orphan sweeper: Stop failed for managed orphan ws-%s: %v — retry next cycle", prefix, stopErr)
			continue
		}
		if rmErr := reaper.RemoveVolume(ctx, prefix); rmErr != nil {
			log.Printf("Orphan sweeper: RemoveVolume warning for managed orphan ws-%s: %v", prefix, rmErr)
		}
	}
}

// staleTokenGrace bounds how recently a token must have been used (or
// issued, if never used) for it to be considered "potentially live".
// Anything quieter than this is fair game for the stale-token revoke
// pass when there's no matching container.
//
// Sized vs the heartbeat cadence (30s) and provisioning latency: a
// healthy workspace touches `last_used_at` every heartbeat, so 5min is
// 10× the heartbeat interval — enough headroom that brief container
// restarts (Stop → Start) don't trip the pass. A workspace that's been
// silent past this window AND has no container is either a wiped-volume
// orphan or a workspace nobody is using; either way, revoking is safe
// because the next /registry/register mints a fresh token via the
// no-live-tokens bootstrap branch in registry.go.
const staleTokenGrace = 5 * time.Minute

// sweepStaleTokensWithoutContainer revokes workspace_auth_tokens rows
// for workspaces whose /configs volume must have been wiped — detected
// as "live token in DB whose owning workspace has no live Docker
// container". This heals the user-reported failure mode where
// `docker compose down -v` (or any out-of-band volume removal) leaves
// stale tokens in the DB while the recreated container has an empty
// `/configs/.auth_token`. Without this pass, /registry/register on the
// fresh container 401s forever (requireWorkspaceToken sees live tokens,
// container can't present one), and the workspace is permanently
// wedged until an operator manually revokes via SQL.
//
// The platform's restart endpoint already handles this case correctly
// via wsauth.RevokeAllForWorkspace inside issueAndInjectToken — this
// pass is the safety net for the equivalent action taken outside the
// API (operator did `docker compose down -v`, host crashed mid-restart,
// disk pressure evicted a volume, etc).
//
// Safety filters that bound the revoke radius:
//
//  1. Only runs in single-tenant Docker mode. The orphan sweeper is
//     wired only when prov != nil (see cmd/server/main.go) — in CP/SaaS
//     mode there is no Docker daemon and the sweeper doesn't run, so an
//     empty container list cannot be confused with "no Docker at all"
//     here (which would otherwise revoke every workspace's tokens).
//     The function also short-circuits on a nil reaper as a belt-and-
//     braces guard against a future refactor wiring it incorrectly.
//
//  2. staleTokenGrace skips tokens that were issued or used in the
//     last 5 minutes. Bounds the race with mid-provisioning (token
//     issued moments before docker run completes) and brief restart
//     windows.
//
//  3. CRITICAL: the staleness predicate is enforced AT THE UPDATE,
//     not just at the SELECT. This closes a TOCTOU race against
//     workspace_provision.go:issueAndInjectToken — the platform's
//     restart endpoint Stops the container synchronously then dispatches
//     re-provisioning to a goroutine, so a stale-on-SELECT workspace
//     can have a fresh token inserted by issueAndInjectToken between
//     our SELECT and our UPDATE. A predicate-only `WHERE workspace_id
//     = $1 AND revoked_at IS NULL` UPDATE would catch that fresh token
//     too. Carrying COALESCE(last_used_at, created_at) < now() - grace
//     in the UPDATE makes the operation idempotent against fresh
//     inserts: a token created within the grace window cannot match.
//
//  4. The DB query joins on workspaces.status NOT IN ('removed',
//     'provisioning') so deleted and mid-restart workspaces are not
//     revoked here — those are handled at delete time and by
//     issueAndInjectToken respectively. (`status = 'provisioning'` is
//     set synchronously in workspace_restart.go before the async
//     re-provision begins, so it's a reliable in-flight signal.)
//
//  5. Each revocation is logged with the workspace ID so operators can
//     correlate "workspace just lost auth" with this sweeper, not blame
//     a network blip.
//
// Failure mode: revoke fails for some reason (transient DB error). The
// next sweep cycle (60s out) retries. Worst case: a workspace stays
// 401-blocked an extra minute.
func sweepStaleTokensWithoutContainer(ctx context.Context, reaper OrphanReaper) {
	// Defence-in-depth (F2): a future refactor that wires the sweeper
	// in CP/SaaS mode without checking prov would otherwise hit this
	// pass with a nil reaper. The StartOrphanSweeper entry point
	// already short-circuits on nil, but we don't want to depend on
	// every future caller doing the same.
	if reaper == nil {
		return
	}

	prefixes, err := reaper.ListWorkspaceContainerIDPrefixes(ctx)
	if err != nil {
		log.Printf("Orphan sweeper: ListWorkspaceContainerIDPrefixes failed: %v — skipping stale-token pass", err)
		return
	}

	// Same hex-and-dash filter as the other passes — anything that
	// can't be a workspace UUID prefix doesn't belong in a SQL LIKE
	// pattern.
	//
	// NOTE: an empty `likes` array is intentionally NOT a short-circuit.
	// "No workspace containers" is the load-bearing case for this pass
	// (operator nuked everything). The `cardinality($1) = 0` clause in
	// the SELECT below treats empty likes as "no LIKE filter" → every
	// stale-token workspace becomes a candidate. The first two passes'
	// early-return-on-empty-prefixes pattern would defeat this entire
	// pass's purpose.
	likes := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if !isLikelyWorkspaceID(p) {
			continue
		}
		likes = append(likes, p+"%")
	}

	// Find workspaces with live tokens whose most-recent activity is
	// past the grace window AND whose ID does NOT match any live
	// container prefix. When `likes` is empty (no workspace containers
	// running at all), every stale-activity workspace is a candidate —
	// expressed via the `cardinality($1) = 0` short-circuit so the
	// query has a single shape regardless of container count.
	//
	// make_interval(secs => $2) avoids the time.Duration.String() →
	// `"5m0s"` mismatch with Postgres interval grammar; passing seconds
	// as an int keeps the binding portable.
	graceSeconds := int(staleTokenGrace.Seconds())
	// `runtime != 'external'` is load-bearing: external workspaces have NO
	// local container by design (the agent runs off-host), so the
	// "no live container" predicate below would match every external
	// workspace and revoke its token. The token is the off-host agent's
	// only authentication credential — revoking breaks the entire
	// external-runtime feature. Discovered 2026-05-03 when a fresh
	// external workspace had its token silently revoked ~6 minutes after
	// creation by this sweep, killing the operator's MCP heartbeat and
	// inbox poll with `HTTP 401 — token may be revoked`.
	rows, qErr := db.DB.QueryContext(ctx, `
		SELECT DISTINCT t.workspace_id::text
		  FROM workspace_auth_tokens t
		  JOIN workspaces w ON w.id = t.workspace_id
		 WHERE t.revoked_at IS NULL
		   AND w.status NOT IN ('removed', 'provisioning')
		   AND w.runtime != 'external'
		   AND COALESCE(t.last_used_at, t.created_at) < now() - make_interval(secs => $2)
		   AND (
		         cardinality($1::text[]) = 0
		      OR NOT (t.workspace_id::text LIKE ANY($1::text[]))
		   )
	`, pq.Array(likes), graceSeconds)
	if qErr != nil {
		log.Printf("Orphan sweeper: stale-token query failed: %v — skipping stale-token pass", qErr)
		return
	}
	defer rows.Close()

	var staleWorkspaceIDs []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			log.Printf("Orphan sweeper: stale-token row scan failed: %v", scanErr)
			continue
		}
		staleWorkspaceIDs = append(staleWorkspaceIDs, id)
	}
	if iterErr := rows.Err(); iterErr != nil {
		log.Printf("Orphan sweeper: stale-token rows iteration failed: %v", iterErr)
		return
	}

	// Per-workspace UPDATE with the SAME staleness predicate as the
	// SELECT, so any token inserted between SELECT and UPDATE (e.g.
	// issueAndInjectToken racing during a user-triggered restart of a
	// long-idle workspace) is automatically excluded — its created_at
	// is fresh and won't satisfy `< now() - grace`.
	//
	// We deliberately bypass wsauth.RevokeAllForWorkspace here because
	// that helper revokes EVERY live token for the workspace; we want
	// "every STALE live token", which is a different (safer) operation.
	for _, wsID := range staleWorkspaceIDs {
		log.Printf("Orphan sweeper: revoking stale tokens for workspace %s (no live container; volume likely wiped)", wsID)
		_, revokeErr := db.DB.ExecContext(ctx, `
			UPDATE workspace_auth_tokens
			   SET revoked_at = now()
			 WHERE workspace_id = $1
			   AND revoked_at IS NULL
			   AND COALESCE(last_used_at, created_at) < now() - make_interval(secs => $2)
		`, wsID, graceSeconds)
		if revokeErr != nil {
			// Non-fatal — next sweep retries. Bail on the loop so a
			// systemic DB error doesn't spam the log on every iteration.
			log.Printf("Orphan sweeper: stale-token revoke for %s failed: %v — will retry next cycle", wsID, revokeErr)
			return
		}
	}
}
