package registry

import (
	"context"
	"log"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// OfflineHandler is called when a workspace's liveness key expires.
type OfflineHandler func(ctx context.Context, workspaceID string)

// StartLivenessMonitor subscribes to Redis keyspace expiry events.
// When a workspace's liveness key (ws:{id}) expires, it marks the workspace offline
// and calls the onOffline handler.
func StartLivenessMonitor(ctx context.Context, onOffline OfflineHandler) {
	sub := db.RDB.PSubscribe(ctx, "__keyevent@0__:expired")

	log.Println("Liveness monitor started — listening for Redis key expirations")

	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			sub.Close()
			return
		case msg := <-ch:
			if msg == nil {
				continue
			}
			key := msg.Payload
			if !strings.HasPrefix(key, "ws:") {
				continue
			}
			parts := strings.SplitN(key, ":", 3)
			if len(parts) != 2 {
				continue
			}
			workspaceID := parts[1]

			log.Printf("Liveness: workspace %s TTL expired", workspaceID)

			// Status target depends on runtime:
			//   external → 'awaiting_agent' (re-registrable via
			//     /registry/register; `molecule connect` brings it
			//     back online on next invocation — typical case is
			//     the operator closed their laptop overnight).
			//   non-external → 'offline' (terminal-feeling status
			//     consistent with Docker/CP-managed runtimes whose
			//     recovery path is restart, not re-register).
			//
			// The conditional flip is done in a single UPDATE so the
			// non-external case stays cheap (no extra round-trip)
			// and there's no TOCTOU between the runtime read and the
			// status write.
			_, err := db.DB.ExecContext(ctx, `
				UPDATE workspaces
				SET status = CASE WHEN runtime = 'external' THEN 'awaiting_agent' ELSE 'offline' END,
				    updated_at = now()
				WHERE id = $1 AND status NOT IN ('removed', 'paused', 'hibernated')
			`, workspaceID)
			if err != nil {
				log.Printf("Liveness: failed to mark %s offline: %v", workspaceID, err)
				continue
			}

			if onOffline != nil {
				onOffline(ctx, workspaceID)
			}
		}
	}
}
