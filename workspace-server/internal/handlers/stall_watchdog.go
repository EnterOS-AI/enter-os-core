package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/google/uuid"
)

// stall_watchdog.go — Agent-Liveness RFC, Layer 3 (A2): stall-watchdog
// (probe → restart) for silently-hung agents.
//
// The problem it solves
// ---------------------
// The Redis TTL liveness monitor (registry/liveness.go) only fires when a
// workspace's heartbeat key EXPIRES — i.e. the agent is dead/offline. The
// operator's status='failed' watchdog only acts on an already-failed row.
// NEITHER catches the "busy but silently hung" case: a workspace that is
// status='online' with active_tasks>0 but has produced NO activity for a
// long time. The agent is wedged mid-task — looping, deadlocked, or stuck
// on a call that never returns — yet looks alive to every existing signal.
// This is exactly what let JRS sit dead for ~2.5h: online, an active task,
// just not advancing.
//
// How it detects
// --------------
// last_activity_at is stamped write-through on EVERY activity_logs write
// (handlers/activity.go logActivityExec, via a CTE). A workspace that is
// genuinely working advances it constantly. So:
//
//	status='online' AND active_tasks>0 AND last_activity_at < now()-STALE_AFTER
//
// uniquely identifies a busy-but-silent workspace. status='online' also
// implicitly excludes paused/hibernated/failed/offline (those are distinct
// status values), so paused/hibernated agents are never touched.
//
// Two-stage state machine (workspace_stall_state table)
// ----------------------------------------------------
//  1. First detection → PROBE. Enqueue ONE liveness A2A message via the same
//     EnqueueA2A path the scheduler/nudge sweeper use ("You've had no activity
//     for N min while marked busy — reply/act or you'll be restarted in M
//     min."), and record state='probed' with probed_at + a snapshot of
//     last_activity_at (probed_activity_at). The probe is non-destructive: a
//     merely-slow agent gets a chance to respond before any restart.
//  2. Next sweep, if still stale AND last_activity_at has NOT advanced past
//     the probe snapshot AND now()-probed_at > PROBE_GRACE → SOFT-RESTART
//     (existing-volume, the same WorkspaceHandler.RestartByID the
//     POST /workspaces/:id/restart handler uses), record last_action_at, and
//     drop back to a cooldown-bearing state.
//  3. If activity RESUMED (live last_activity_at advanced past the snapshot)
//     → clear the stall state. It was just slow; no restart.
//
// Anti-flap
// ---------
// Never restart the same workspace within COOLDOWN of its last_action_at.
// Bounded LIMIT per sweep. Every probe/restart writes a structured log line
// and an activity_logs audit row (activity_type='stall_watchdog').
//
// Independence
// ------------
// Pure RAW SQL against workspaces + workspace_stall_state — no dependency on
// any parallel RFC branch's Go symbols. No-op until this PR's migration has
// rolled out (the sweep query simply finds no last_activity_at column? — no:
// the column is added by THIS PR's migration, so build+rollout are coupled
// within this PR).

const (
	defaultStallWatchdogInterval = 3 * time.Minute

	// stallStaleAfter — how long a busy workspace may produce no activity
	// before it's considered stalled. 12min tolerates a legitimately long
	// single LLM turn / tool call (which can stall activity for minutes)
	// while still catching a real wedge well inside the ~2.5h JRS window.
	stallStaleAfter = 12 * time.Minute

	// stallProbeGrace — after a probe, how long to wait for the agent to
	// act before escalating to a restart. 5min is comfortably longer than
	// a normal turn so a responsive-but-slow agent clears the probe.
	stallProbeGrace = 5 * time.Minute

	// stallCooldown — anti-flap: never soft-restart the same workspace
	// twice within this window. Sized well above a cold-boot + first-
	// heartbeat interval so a workspace that's slow to resume activity
	// after a restart isn't immediately re-restarted.
	stallCooldown = 30 * time.Minute

	// stallBatchLimit — bound work per sweep. Single-digit-hundreds cap is
	// generous; the next tick picks up any remainder.
	stallBatchLimit = 100
)

// stallEnqueueFunc is the package-level EnqueueA2A signature, declared locally
// so this file does NOT depend on any parallel RFC branch's enqueueFunc type
// (the nudge sweeper declares an identical alias on its own branch). Injected
// as a field so tests assert the probe enqueue without mocking EnqueueA2A's
// internal SQL.
type stallEnqueueFunc func(
	ctx context.Context,
	workspaceID, callerID string,
	priority int,
	body []byte,
	method, idempotencyKey string,
	expiresAt *time.Time,
) (id string, depth int, err error)

// StallWatchdog runs the periodic stall sweep. Construct via
// NewStallWatchdog, then Start(ctx) in main.go to begin ticking.
type StallWatchdog struct {
	db         *sql.DB
	interval   time.Duration
	staleAfter time.Duration
	probeGrace time.Duration
	cooldown   time.Duration
	limit      int

	// enqueue is the a2a-queue enqueue (package EnqueueA2A in production).
	// Injected as a field so tests assert the probe without mocking
	// EnqueueA2A's internal SQL. Mirrors RequestNudgeSweeper.enqueue.
	enqueue stallEnqueueFunc

	// restart fires a soft-restart (existing-volume) for a workspace id.
	// Production wiring passes WorkspaceHandler.RestartByID; tests inject a
	// recorder. nil restart = restart stage disabled (probe-only), logged.
	restart func(workspaceID string)
}

// NewStallWatchdog builds a watchdog bound to the package db.DB (production)
// or a test handle. Reads optional env overrides at construction time so a
// long-running process picks them up via restart, not mid-flight (mirrors
// NewRequestNudgeSweeper / NewDelegationSweeper).
//
// restartFunc is the in-process soft-restart (WorkspaceHandler.RestartByID).
func NewStallWatchdog(handle *sql.DB, restartFunc func(string)) *StallWatchdog {
	if handle == nil {
		handle = db.DB
	}
	return &StallWatchdog{
		db:         handle,
		interval:   envDuration("STALL_WATCHDOG_INTERVAL_S", defaultStallWatchdogInterval),
		staleAfter: envDuration("STALL_WATCHDOG_STALE_AFTER_S", stallStaleAfter),
		probeGrace: envDuration("STALL_WATCHDOG_PROBE_GRACE_S", stallProbeGrace),
		cooldown:   envDuration("STALL_WATCHDOG_COOLDOWN_S", stallCooldown),
		limit:      stallBatchLimit,
		enqueue:    EnqueueA2A,
		restart:    restartFunc,
	}
}

// Interval exposes the configured tick cadence — tests use it; main.go uses
// it implicitly via Start.
func (s *StallWatchdog) Interval() time.Duration { return s.interval }

// Start ticks Sweep() at the configured interval until ctx is cancelled.
// Defers panic recovery so a single bad row can't kill the watchdog. Mirrors
// DelegationSweeper/RequestNudgeSweeper.Start: first sweep fires immediately.
func (s *StallWatchdog) Start(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	log.Printf("StallWatchdog: started (interval=%s, stale-after=%s, probe-grace=%s, cooldown=%s)",
		s.interval, s.staleAfter, s.probeGrace, s.cooldown)

	tickWithRecover := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("StallWatchdog: PANIC in tick — recovered: %v", r)
			}
		}()
		s.Sweep(ctx)
	}

	tickWithRecover()

	for {
		select {
		case <-ctx.Done():
			log.Printf("StallWatchdog: stopped")
			return
		case <-t.C:
			tickWithRecover()
		}
	}
}

// StallResult records what the last sweep did. Returned for observability and
// so tests assert behavior without diffing log lines.
type StallResult struct {
	Probed    int // workspaces freshly probed this sweep
	Restarted int // workspaces soft-restarted this sweep
	Cleared   int // workspaces whose stall state was cleared (activity resumed)
	Errors    int
}

// candidate is one busy-but-silent workspace plus its current stall-state row
// (NULLs when no row exists yet — i.e. never probed).
type stallCandidate struct {
	workspaceID    string
	lastActivityAt sql.NullTime
	state          sql.NullString
	probedAt       sql.NullTime
	probedActAt    sql.NullTime
	lastActionAt   sql.NullTime
}

// Sweep runs one pass. SQL strategy: a single LEFT JOIN of the stalled
// workspaces against their stall-state row yields, per candidate, both the
// live last_activity_at and the prior bookkeeping — so the state-machine
// decision (probe / restart / clear / skip) is made in Go from one scan.
//
// The stale + busy + online gate lives in the WHERE so paused/hibernated/
// offline/idle workspaces are never even returned. The cooldown gate is
// applied in Go (it depends on last_action_at which is on the joined row).
func (s *StallWatchdog) Sweep(ctx context.Context) StallResult {
	res := StallResult{}

	const sweepQuery = `
		SELECT w.id,
		       w.last_activity_at,
		       ss.state,
		       ss.probed_at,
		       ss.probed_activity_at,
		       ss.last_action_at
		  FROM workspaces w
		  LEFT JOIN workspace_stall_state ss ON ss.workspace_id = w.id
		 WHERE w.status = 'online'
		   AND COALESCE(w.active_tasks, 0) > 0
		   AND w.last_activity_at IS NOT NULL
		   AND w.last_activity_at < now() - ($1 * INTERVAL '1 second')
		 ORDER BY w.last_activity_at ASC
		 LIMIT $2
	`

	rows, err := s.db.QueryContext(ctx, sweepQuery, int(s.staleAfter.Seconds()), s.limit)
	if err != nil {
		log.Printf("StallWatchdog: sweep query failed: %v", err)
		res.Errors++
		return res
	}
	defer rows.Close()

	var todo []stallCandidate
	for rows.Next() {
		var c stallCandidate
		if err := rows.Scan(&c.workspaceID, &c.lastActivityAt, &c.state,
			&c.probedAt, &c.probedActAt, &c.lastActionAt); err != nil {
			log.Printf("StallWatchdog: scan failed: %v", err)
			res.Errors++
			continue
		}
		todo = append(todo, c)
	}
	if err := rows.Err(); err != nil {
		log.Printf("StallWatchdog: rows.Err: %v", err)
		res.Errors++
	}

	now := time.Now()
	for _, c := range todo {
		if err := s.act(ctx, c, now, &res); err != nil {
			log.Printf("StallWatchdog: act on %s failed: %v", c.workspaceID, err)
			res.Errors++
		}
	}

	if res.Probed > 0 || res.Restarted > 0 || res.Cleared > 0 || res.Errors > 0 {
		log.Printf("StallWatchdog: sweep complete — probed=%d restarted=%d cleared=%d errors=%d",
			res.Probed, res.Restarted, res.Cleared, res.Errors)
	}
	return res
}

// act runs the state machine for one candidate.
//
//	no state row        → PROBE
//	state='probed' and activity advanced past snapshot → CLEAR
//	state='probed' and probe_grace elapsed             → RESTART (if not in cooldown)
//	state='probed' and within grace                    → wait (no-op)
//	state='restarted' (cooldown row)                   → re-probe only after cooldown
func (s *StallWatchdog) act(ctx context.Context, c stallCandidate, now time.Time, res *StallResult) error {
	// Activity resumed since the probe snapshot → the agent acted; it was
	// just slow. Clear the stall state. (Only meaningful once we've probed.)
	if c.state.Valid && c.state.String == "probed" && c.probedActAt.Valid &&
		c.lastActivityAt.Valid && c.lastActivityAt.Time.After(c.probedActAt.Time) {
		if err := s.clearState(ctx, c.workspaceID); err != nil {
			return fmt.Errorf("clear state: %w", err)
		}
		res.Cleared++
		return nil
	}

	switch {
	case !c.state.Valid:
		// First detection (or post-cooldown re-detection where the row was
		// cleared). Probe.
		return s.probe(ctx, c, now, res)

	case c.state.String == "probed":
		// Awaiting the probe response. Escalate only once the grace window has
		// elapsed since the probe AND we're not in cooldown.
		if !c.probedAt.Valid || now.Sub(c.probedAt.Time) <= s.probeGrace {
			return nil // still within grace — give the agent more time
		}
		if c.lastActionAt.Valid && now.Sub(c.lastActionAt.Time) < s.cooldown {
			// Restarted recently; don't flap. Stay 'probed' and wait out
			// the cooldown; activity-resumed clearing still applies above.
			log.Printf("StallWatchdog: %s past probe grace but within cooldown (%s) — holding",
				c.workspaceID, s.cooldown)
			return nil
		}
		return s.softRestart(ctx, c, now, res)

	case c.state.String == "restarted":
		// A prior restart fired. Re-probe only after the cooldown elapses so a
		// workspace that's genuinely re-wedged still gets re-escalated, but not
		// within the anti-flap window.
		if c.lastActionAt.Valid && now.Sub(c.lastActionAt.Time) < s.cooldown {
			return nil
		}
		return s.probe(ctx, c, now, res)

	default:
		return nil
	}
}

// probe enqueues ONE liveness A2A message and upserts state='probed' with
// probed_at + a snapshot of the current last_activity_at. The state upsert
// only fires after a successful enqueue so a failed enqueue is retried next
// sweep (no state row written → still treated as a first detection).
func (s *StallWatchdog) probe(ctx context.Context, c stallCandidate, now time.Time, res *StallResult) error {
	mins := int(s.staleAfter.Minutes())
	graceMins := int(s.probeGrace.Minutes())

	// Enqueue the probe FIRST; only write the 'probed' state row after a
	// successful enqueue so a failed enqueue is retried next sweep (no state
	// row → still a first detection). enqueue is always EnqueueA2A in
	// production; the nil guard keeps a probe-disabled test/build sane.
	if s.enqueue != nil {
		body, err := buildStallProbeBody(mins, graceMins)
		if err != nil {
			return fmt.Errorf("build probe body: %w", err)
		}
		// Hourly-bucketed idempotency key: collapse duplicate probes for the
		// same workspace at the queue layer too (defense in depth with the
		// state row).
		idemKey := fmt.Sprintf("stall-probe:%s:%d", c.workspaceID, now.Truncate(time.Hour).Unix())
		if _, _, err := s.enqueue(ctx, c.workspaceID, "", PriorityCritical, body, "message/send", idemKey, nil); err != nil {
			return fmt.Errorf("enqueue probe: %w", err)
		}
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO workspace_stall_state (workspace_id, state, probed_at, probed_activity_at, updated_at)
		VALUES ($1, 'probed', $2, $3, now())
		ON CONFLICT (workspace_id) DO UPDATE
		   SET state = 'probed', probed_at = $2, probed_activity_at = $3, updated_at = now()
	`, c.workspaceID, now, c.lastActivityAt); err != nil {
		return fmt.Errorf("upsert probed state: %w", err)
	}

	s.audit(ctx, c.workspaceID, "probe",
		fmt.Sprintf("no activity for >%dm while busy; probed, will restart in %dm if still silent", mins, graceMins))
	log.Printf("StallWatchdog: PROBE %s (no activity for >%dm, active_tasks>0)", c.workspaceID, mins)
	res.Probed++
	return nil
}

// softRestart fires the in-process soft-restart (existing-volume) and records
// last_action_at + state='restarted'. The restart is dispatched through the
// injected restartFunc (WorkspaceHandler.RestartByID), which is itself
// debounced/coalesced; we fire it async so a slow Stop+provision can't block
// the sweep. State is recorded BEFORE dispatch so the cooldown gate is armed
// even if the process dies mid-restart.
func (s *StallWatchdog) softRestart(ctx context.Context, c stallCandidate, now time.Time, res *StallResult) error {
	if s.restart == nil {
		log.Printf("StallWatchdog: %s past probe grace but no restartFunc wired — probe-only mode, NOT restarting", c.workspaceID)
		return nil
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO workspace_stall_state (workspace_id, state, probed_at, probed_activity_at, last_action_at, updated_at)
		VALUES ($1, 'restarted', $2, $3, $4, now())
		ON CONFLICT (workspace_id) DO UPDATE
		   SET state = 'restarted', last_action_at = $4, updated_at = now()
	`, c.workspaceID, c.probedAt, c.probedActAt, now); err != nil {
		return fmt.Errorf("record restart state: %w", err)
	}

	s.audit(ctx, c.workspaceID, "restart",
		"still silent after probe grace; soft-restarting (existing volume)")
	log.Printf("StallWatchdog: SOFT-RESTART %s (silent past probe grace)", c.workspaceID)

	wsID := c.workspaceID
	restartFn := s.restart
	globalGoAsync(func() { restartFn(wsID) })

	res.Restarted++
	return nil
}

// clearState deletes the stall-state row — the agent resumed activity, so it
// was merely slow, not hung.
func (s *StallWatchdog) clearState(ctx context.Context, workspaceID string) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM workspace_stall_state WHERE workspace_id = $1
	`, workspaceID); err != nil {
		return err
	}
	s.audit(ctx, workspaceID, "clear", "activity resumed after probe; stall state cleared")
	log.Printf("StallWatchdog: CLEAR %s (activity resumed)", workspaceID)
	return nil
}

// audit writes a best-effort activity_logs row recording a watchdog action.
// Failures are logged and swallowed — the audit trail is observability, not a
// correctness dependency, and must never fail the probe/restart it records.
func (s *StallWatchdog) audit(ctx context.Context, workspaceID, action, detail string) {
	summary := "stall-watchdog: " + action
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, activity_type, method, summary, status)
		VALUES ($1, 'stall_watchdog', $2, $3, 'ok')
	`, workspaceID, action, summary+" — "+detail); err != nil {
		log.Printf("StallWatchdog: audit row for %s (%s) failed (non-fatal): %v", workspaceID, action, err)
	}
}

// buildStallProbeBody constructs the A2A message/send JSON-RPC body for the
// liveness probe. Mirrors the scheduler/nudge body shape (role=user, generated
// messageId, single text part) so the receiving agent processes it as a normal
// inbound turn — replying or acting on it advances last_activity_at, which
// clears the stall state on the next sweep.
func buildStallProbeBody(staleMins, graceMins int) ([]byte, error) {
	text := fmt.Sprintf(
		"Liveness check: you've had no recorded activity for over %d minutes while still marked busy "+
			"(an active task is in progress). If you are working, reply or take an action now — anything "+
			"that records activity. If you do not within %d minutes, you'll be automatically restarted to "+
			"recover from a possible hang.",
		staleMins, graceMins,
	)
	return json.Marshal(map[string]interface{}{
		"method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":      "user",
				"messageId": "stall-probe-" + uuid.New().String(),
				"parts":     []map[string]interface{}{{"kind": "text", "text": text}},
			},
		},
	})
}
