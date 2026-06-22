// Package registry — wedged-agent detection.
//
// Background (2026-06-19 a2a RCA, #3057): a workspace agent can be
// "alive-but-wedged" — the agent process is up (so the platform's TCP
// connect succeeds), but it is hung mid-turn and produces no outbound
// A2A activity, no heartbeats, and (eventually) no progress. The
// existing reactive detection (isUpstreamDeadStatus → auto-restart)
// only fires on dead-origin HTTP statuses (502/521/522/524), not on
// this wedged-while-TCP-alive case. As a result, Kimi (workspace
// 6cb8c061) was observed with `active_tasks=1` (stuck), `last_outbound_at`
// ~48 minutes stale, heartbeat null/fresh:false — but the platform's
// `status: online` flag stayed set and the wedge was only caught by
// MANUAL inspection of (active>0 + no-outbound + null-heartbeat).
//
// Fix: a separate `StartWedgedAgentMonitor` that periodically queries
// for workspaces matching the wedged shape, surfaces the predicate via
// `IsWedgedAgent`, and dispatches a handler that the platform can use
// to (gated) auto-recover — initially logging + flipping a `wedged`
// flag in `get_workspace`, with auto-restart left as a follow-up
// gated on ops review.
package registry

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

// DefaultWedgedThreshold is the "no outbound" interval that, combined
// with active_tasks > 0 and a null/stale heartbeat, classifies a
// workspace as wedged. 5 minutes is long enough that a long synchronous
// busy turn (large LLM response, deep tool chain) does not false-
// positive, but short enough that an operator inspecting a stuck
// workspace sees the wedge within a 5-minute window of staleness —
// matching the spirit of the 180s heartbeat-staleness window in
// healthsweep.go (the heartbeat-staleness and outbound-staleness
// windows are intentionally different: a wedged agent may still send
// heartbeats for a while even after it stops producing outbound A2A).
//
// Override via `WEDGED_AGENT_THRESHOLD_SECONDS` env var (integer
// seconds). Same parse rules as REMOTE_LIVENESS_STALE_AFTER in
// healthsweep.go.
const DefaultWedgedThreshold = 5 * time.Minute

// wedgedThreshold reads the override from env, falling back to default.
func wedgedThreshold() time.Duration {
	v := os.Getenv("WEDGED_AGENT_THRESHOLD_SECONDS")
	if v == "" {
		return DefaultWedgedThreshold
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("Wedged-agent monitor: invalid WEDGED_AGENT_THRESHOLD_SECONDS=%q (want positive integer seconds) — using default %s", v, DefaultWedgedThreshold)
		return DefaultWedgedThreshold
	}
	return time.Duration(n) * time.Second
}

// WedgedThresholdForHTTP is the same env-driven threshold the monitor
// uses, exposed as a public package symbol so the get_workspace
// handler can compute its `wedged` flag with the same threshold the
// monitor is watching. Keeping both on the same source prevents the
// HTTP flag and the monitor from drifting out of sync if the env
// override is set.
//
// 2026-06-19 a2a RCA (#3057).
func WedgedThresholdForHTTP() time.Duration {
	return wedgedThreshold()
}

// IsWedgedAgent classifies a workspace as wedged. A workspace is
// wedged when ALL of the following hold:
//
//   - activeTasks > 0 — the agent claims it is mid-turn, not idle.
//   - lastOutboundAt is NULL OR older than threshold — the agent has
//     not produced an outbound A2A in the window. A null value means
//     the workspace has never sent anything (the heartbeat-driven
//     `active_tasks=1` is the only signal we have, and the absence
//     of any outbound is the wedge signal).
//   - lastHeartbeatAt is NULL OR older than threshold — the agent's
//     heartbeat task is also missing or stale. This separates a
//     wedged agent (no heartbeat, no outbound) from a busy agent
//     that's just slow (active=1, outbound recent, heartbeat recent).
//
// Exposed for unit tests and for the `get_workspace` endpoint to
// surface a `wedged: true` flag. The monitor calls this internally
// too, so the SQL query and the surfaced flag can never disagree.
//
// 2026-06-19 a2a RCA (#3057).
func IsWedgedAgent(activeTasks int, lastOutboundAt, lastHeartbeatAt sql.NullTime, threshold time.Duration) bool {
	if activeTasks <= 0 {
		return false
	}
	if threshold <= 0 {
		// Defensive: a non-positive threshold is operator config
		// footgun (would mark every busy agent as wedged). Treat
		// as "wedge detection disabled" rather than a panic.
		return false
	}
	now := time.Now()
	// A null OR older-than-threshold outbound is one half of the
	// signal. A null OR older-than-threshold heartbeat is the other.
	outboundStale := !lastOutboundAt.Valid || now.Sub(lastOutboundAt.Time) > threshold
	heartbeatStale := !lastHeartbeatAt.Valid || now.Sub(lastHeartbeatAt.Time) > threshold
	return outboundStale && heartbeatStale
}

// WedgedHandler is called for each workspace that the wedged-agent
// monitor decides is wedged. The handler should be idempotent — the
// monitor may fire for the same workspace across multiple ticks until
// the wedge clears (heartbeat resumes, outbound resumes) or the
// workspace is taken offline. The platform starts with a log-only
// handler; a gated auto-restart handler is a follow-up.
type WedgedHandler func(ctx context.Context, workspaceID string)

// DefaultWedgedMonitorInterval is how often the wedged-agent monitor
// polls the database. 30s is fine-grained enough that an operator
// inspecting a stuck workspace sees the wedge flag flip within a
// 30s window of the threshold elapsing, and cheap enough on a busy
// platform — the query hits a small partial index on (status,
// active_tasks) and a per-row null-check on the timestamp columns.
//
// Override via `WEDGED_AGENT_MONITOR_INTERVAL_SECONDS` env var.
const DefaultWedgedMonitorInterval = 30 * time.Second

// StartWedgedAgentMonitor periodically scans for wedged workspaces
// (active_tasks > 0 + stale outbound + stale/null heartbeat) and
// dispatches onWedged for each. It runs under supervised.RunWithRecover
// so a panic is recovered with exponential backoff rather than
// silently dying — same contract as StartHibernationMonitor and
// StartHealthSweep.
//
// Only workspaces with status IN ('online', 'degraded') are scanned.
// Removed / provisioning / paused workspaces are excluded. External
// runtimes (no Docker container) are also excluded — the wedge
// signal is defined in terms of the on-platform agent's outbound
// activity, and external runtimes may legitimately have long quiet
// periods when the operator's laptop is asleep.
//
// 2026-06-19 a2a RCA (#3057).
func StartWedgedAgentMonitor(ctx context.Context, onWedged WedgedHandler) {
	StartWedgedAgentMonitorWithInterval(ctx, DefaultWedgedMonitorInterval, onWedged)
}

// StartWedgedAgentMonitorWithInterval is StartWedgedAgentMonitor with
// a configurable tick interval — exposed for tests so they don't
// have to wait 30 seconds for a tick.
func StartWedgedAgentMonitorWithInterval(ctx context.Context, interval time.Duration, onWedged WedgedHandler) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	threshold := wedgedThreshold()
	log.Printf("Wedged-agent monitor: started (interval=%s, threshold=%s)", interval, threshold)

	for {
		select {
		case <-ctx.Done():
			log.Println("Wedged-agent monitor: context done; stopping")
			return
		case <-ticker.C:
			sweepWedgedAgents(ctx, threshold, onWedged)
		}
	}
}

// sweepWedgedAgents queries for wedged workspaces and calls onWedged
// for each. Errors from DB are logged but do not crash the loop.
// The query selects the minimal set of columns needed to apply the
// IsWedgedAgent predicate in code, so the SQL `WHERE` clause can use
// indexed columns and the predicate stays a pure Go function (easier
// to unit-test than a SQL CASE).
func sweepWedgedAgents(ctx context.Context, threshold time.Duration, onWedged WedgedHandler) {
	thresholdSec := int(threshold / time.Second)
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, active_tasks, last_outbound_at, last_heartbeat_at
		FROM workspaces
		WHERE status IN ('online', 'degraded')
		  AND active_tasks > 0
		  AND COALESCE(runtime, 'claude-code') != 'external'
		  AND (
		    last_outbound_at IS NULL
		    OR last_outbound_at < now() - ($1 || ' seconds')::interval
		  )
		  AND (
		    last_heartbeat_at IS NULL
		    OR last_heartbeat_at < now() - ($1 || ' seconds')::interval
		  )
	`, thresholdSec)
	if err != nil {
		log.Printf("Wedged-agent monitor: query error: %v", err)
		return
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		var activeTasks int
		var lastOutbound, lastHeartbeat sql.NullTime
		if err := rows.Scan(&id, &activeTasks, &lastOutbound, &lastHeartbeat); err != nil {
			log.Printf("Wedged-agent monitor: scan error: %v", err)
			continue
		}
		// Defensive: re-apply the predicate in Go even though the SQL
		// already filtered. SQL's NOW() and Go's time.Now() can disagree
		// by milliseconds across the network — the Go predicate is the
		// authoritative one and IsWedgedAgent is the single source of
		// truth shared with the get_workspace flag.
		if !IsWedgedAgent(activeTasks, lastOutbound, lastHeartbeat, threshold) {
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Wedged-agent monitor: rows error: %v", err)
	}

	for _, id := range ids {
		log.Printf("Wedged-agent monitor: detected wedge for %s (active_tasks>0, no outbound, no heartbeat for >%s) — dispatching handler",
			id, threshold)
		if onWedged != nil {
			onWedged(ctx, id)
		}
	}
}
