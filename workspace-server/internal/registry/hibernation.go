package registry

import (
	"context"
	"log"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/supervised"
)

// HibernateHandler is called for each workspace that the hibernation monitor
// decides should be hibernated. The handler stops the container, updates the
// DB status, and broadcasts the event.
type HibernateHandler func(ctx context.Context, workspaceID string)

// defaultHibernationInterval is how often the hibernation monitor polls the
// database for idle-too-long workspaces. Two minutes is fine-grained enough
// for typical idle_hibernate_minutes values (≥5) and cheap enough on a busy
// platform — the query hits a partial index and does a small range scan.
const defaultHibernationInterval = 2 * time.Minute

// StartHibernationMonitor periodically scans for workspaces that have been
// idle (active_tasks == 0) longer than their configured hibernation_idle_minutes
// and calls onHibernate for each. It runs under supervised.RunWithRecover so a
// panic is recovered with exponential backoff rather than silently dying.
//
// Only workspaces with:
//   - status IN ('online', 'degraded')
//   - active_tasks == 0
//   - hibernation_idle_minutes IS NOT NULL AND > 0
//   - runtime != 'external' (external agents have no Docker container)
//   - last heartbeat older than hibernation_idle_minutes minutes ago
//
// are candidates. The last_heartbeat_at column tracks the most recent
// successful heartbeat from the agent; when it is NULL the workspace has
// never heartbeated and is not yet eligible for hibernation (we give it a
// full grace period equal to hibernation_idle_minutes from its created_at).
func StartHibernationMonitor(ctx context.Context, onHibernate HibernateHandler) {
	StartHibernationMonitorWithInterval(ctx, defaultHibernationInterval, onHibernate)
}

// StartHibernationMonitorWithInterval is StartHibernationMonitor with a
// configurable tick interval — exposed for tests so they don't have to wait
// 2 minutes for a tick.
func StartHibernationMonitorWithInterval(ctx context.Context, interval time.Duration, onHibernate HibernateHandler) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("Hibernation monitor: started (interval=%s)", interval)

	for {
		select {
		case <-ctx.Done():
			log.Println("Hibernation monitor: context done; stopping")
			return
		case <-ticker.C:
			hibernateIdleWorkspaces(ctx, onHibernate)
			supervised.Heartbeat("hibernation-monitor")
		}
	}
}

// hibernateIdleWorkspaces queries for hibernation candidates and calls
// onHibernate for each. Errors from DB are logged but do not crash the loop.
func hibernateIdleWorkspaces(ctx context.Context, onHibernate HibernateHandler) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id
		FROM workspaces
		WHERE hibernation_idle_minutes IS NOT NULL
		  AND hibernation_idle_minutes > 0
		  AND status IN ('online', 'degraded')
		  AND active_tasks = 0
		  AND COALESCE(runtime, 'claude-code') != 'external'
		  AND last_heartbeat_at IS NOT NULL
		  AND last_heartbeat_at < now() - (hibernation_idle_minutes * INTERVAL '1 minute')
	`)
	if err != nil {
		log.Printf("Hibernation monitor: query error: %v", err)
		return
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Hibernation monitor: row iteration error: %v", err)
		return
	}

	for _, id := range ids {
		log.Printf("Hibernation monitor: hibernating idle workspace %s", id)
		if onHibernate != nil {
			onHibernate(ctx, id)
		}
	}
}
