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

// ContainerChecker checks if a workspace container is running via Docker API.
type ContainerChecker interface {
	IsRunning(ctx context.Context, workspaceID string) (bool, error)
}

// DefaultRemoteStaleAfter is the default heartbeat-freshness window for
// `runtime='external'` workspaces before they're marked offline. Chosen
// slightly longer than the 60s Redis TTL so transient network hiccups
// don't cause a flapping online/offline ping-pong on the canvas. Override
// via `REMOTE_LIVENESS_STALE_AFTER` env var (integer seconds).
const DefaultRemoteStaleAfter = 90 * time.Second

// remoteStaleAfter reads the override from env, falling back to default.
// Called once per sweep tick — we don't cache because ops occasionally
// tune this live via a container restart, and the overhead of reading
// an env var on a 15s cadence is irrelevant.
func remoteStaleAfter() time.Duration {
	if v := os.Getenv("REMOTE_LIVENESS_STALE_AFTER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return DefaultRemoteStaleAfter
}

// StartHealthSweep periodically checks all "online" workspaces. For
// container-backed runtimes (langgraph, claude-code, …) it calls the
// Docker API via `checker.IsRunning`. For `runtime='external'` (remote
// agents per Phase 30) it checks heartbeat freshness: a heartbeat older
// than `REMOTE_LIVENESS_STALE_AFTER` (default 90s) marks the workspace
// offline and calls `onOffline`.
//
// If `checker` is nil we still run the remote-liveness path — a
// deployment without Docker (e.g. a pure SaaS front-door) is a valid
// configuration and shouldn't lose liveness monitoring for its remote
// agents.
func StartHealthSweep(ctx context.Context, checker ContainerChecker, interval time.Duration, onOffline OfflineHandler) {
	if checker == nil {
		log.Println("Health sweep: no Docker container checker — running remote-liveness sweep only")
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("Health sweep: started (interval=%s, remote stale-after=%s)", interval, remoteStaleAfter())

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if checker != nil {
				sweepOnlineWorkspaces(ctx, checker, onOffline)
			}
			sweepStaleRemoteWorkspaces(ctx, onOffline)
		}
	}
}

func sweepOnlineWorkspaces(ctx context.Context, checker ContainerChecker, onOffline OfflineHandler) {
	// Skip external + mock workspaces — neither has a Docker container.
	// external: agent runs on operator's laptop, polled via heartbeat.
	// mock: virtual workspace, every reply is canned (see
	// workspace-server/internal/handlers/mock_runtime.go). Both would
	// false-positive as "container gone" on every sweep tick and
	// auto-restart would loop forever (provisioner has no template
	// for either runtime).
	rows, err := db.DB.QueryContext(ctx,
		`SELECT id FROM workspaces WHERE status IN ('online', 'degraded') AND COALESCE(runtime, 'langgraph') NOT IN ('external', 'mock')`)
	if err != nil {
		log.Printf("Health sweep: query error: %v", err)
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

	for _, id := range ids {
		running, err := checker.IsRunning(ctx, id)
		if err != nil {
			continue // Docker API error — skip, don't false-positive
		}
		if running {
			continue
		}

		log.Printf("Health sweep: container for %s is gone — marking offline", id)

		_, err = db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = $1, updated_at = now()
			 WHERE id = $2 AND status NOT IN ('removed', 'provisioning')`,
			models.StatusOffline, id)
		if err != nil {
			log.Printf("Health sweep: failed to mark %s offline: %v", id, err)
			continue
		}

		db.ClearWorkspaceKeys(ctx, id)

		if onOffline != nil {
			onOffline(ctx, id)
		}
	}
}

// sweepStaleRemoteWorkspaces marks `runtime='external'` workspaces offline
// when their last heartbeat is older than `remoteStaleAfter()`. This is
// the Phase 30.7 analogue of `sweepOnlineWorkspaces` — instead of asking
// Docker "is the container alive?" we ask the DB "did the agent check in
// recently?". Workspaces that never heartbeated (last_heartbeat_at IS
// NULL) are eligible for sweep only after they've been online longer
// than the staleness window, so a newly-registered agent gets a full
// grace period to send its first heartbeat.
func sweepStaleRemoteWorkspaces(ctx context.Context, onOffline OfflineHandler) {
	staleAfter := remoteStaleAfter()
	staleAfterSec := int(staleAfter / time.Second)

	// Use Postgres age arithmetic so the cutoff is computed server-side
	// (no clock skew between platform host and DB). `COALESCE` ensures
	// a NULL heartbeat is compared against updated_at (which is set
	// when the external workspace was created + marked online) — that
	// way an agent that registered but immediately crashed before its
	// first heartbeat still gets swept after the grace window.
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id FROM workspaces
		WHERE status IN ('online', 'degraded')
		  AND COALESCE(runtime, 'langgraph') = 'external'
		  AND COALESCE(last_heartbeat_at, updated_at) < now() - ($1 || ' seconds')::interval
	`, staleAfterSec)
	if err != nil {
		log.Printf("Health sweep (remote): query error: %v", err)
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

	for _, id := range ids {
		// External workspaces flip to 'awaiting_agent' (re-registrable
		// via /registry/register) instead of 'offline' (which was the
		// terminal-feeling status used pre-2026-04-30). The CLI's
		// `molecule connect` command (RFC #10 in molecule-cli) re-
		// registers on each invocation, bringing the workspace back
		// online. 'offline' was confusing because it implied "agent
		// crashed and needs operator intervention" when often the
		// operator simply closed their laptop overnight.
		log.Printf("Health sweep (remote): %s heartbeat stale (>%s) — marking awaiting_agent", id, staleAfter)

		_, err = db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = $1, updated_at = now()
			 WHERE id = $2 AND status NOT IN ('removed', 'provisioning', 'paused')`,
			models.StatusAwaitingAgent, id)
		if err != nil {
			log.Printf("Health sweep (remote): failed to mark %s awaiting_agent: %v", id, err)
			continue
		}

		db.ClearWorkspaceKeys(ctx, id)

		if onOffline != nil {
			onOffline(ctx, id)
		}
	}
}
