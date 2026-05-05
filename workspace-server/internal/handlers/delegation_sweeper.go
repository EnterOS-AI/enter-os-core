package handlers

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// delegation_sweeper.go — RFC #2829 PR-3: stuck-task sweeper.
//
// What it does
// ------------
// Periodically scans the `delegations` table (PR-1 schema) for in-flight
// rows that have either:
//
//   1. Blown past their `deadline` — agent claims to still be working but
//      the hard ceiling fired. Mark `failed` with error_detail = "deadline
//      exceeded".
//   2. Stopped heartbeating for >stuckThreshold while still claiming
//      in_progress. Mark `stuck` with error_detail = "no heartbeat for Ns".
//
// Why both rules
// --------------
// Deadline catches forever-heartbeating agents that never make progress
// (a wedged agent looping on a heartbeat call inside its main work loop
// looks "alive" by liveness signals but is not actually advancing).
// Heartbeat-staleness catches agents that crash or get OOM-killed
// without graceful shutdown — no terminal status update fires, but the
// heartbeat stops cold.
//
// Order matters: deadline check fires first because deadline → failed
// is a stronger statement than deadline → stuck. A stuck row can be
// retried by the operator; a failed row says "give up, retry was
// already exhausted or not viable."
//
// Frequency
// ---------
// 5min default cadence. Faster than that wastes DB round-trips for the
// hot index; slower means a stuck task isn't caught until ~5min after
// the heartbeat stops. Operators can override via DELEGATION_SWEEPER_INTERVAL_S.
//
// Threshold
// ---------
// Default 10× the runtime's heartbeat interval (≈100s for hermes that
// beats every 10s during stream output). 10× is the heuristic from the
// RFC #2829 design discussion: it tolerates legitimate slow LLM
// responses (a single completion can stall a heartbeat for 30-60s) while
// still catching real wedges within ~2 minutes. Operators override via
// DELEGATION_STUCK_THRESHOLD_S.
//
// Safety
// ------
// All transitions go through DelegationLedger.SetStatus so the
// terminal-state forward-only protection applies — a delegation that
// just transitioned to completed concurrently with the sweep won't be
// flipped back to failed/stuck. The ledger's same-status replay no-op
// also makes re-running the sweep idempotent.

const (
	defaultSweeperInterval = 5 * time.Minute

	// 10min = 60× the typical 10s hermes heartbeat. Tightens to ~10×
	// once the user community settles on a tighter heartbeat cadence;
	// today's mix of runtimes (hermes 10s, claude-code 30-60s, langchain
	// minute-scale) needs the looser threshold to avoid false positives.
	defaultStuckThreshold = 10 * time.Minute
)

// DelegationSweeper runs the periodic sweep. Construct via
// NewDelegationSweeper, then Start(ctx) in main.go to begin ticking.
type DelegationSweeper struct {
	db        *sql.DB
	ledger    *DelegationLedger
	interval  time.Duration
	threshold time.Duration
}

// NewDelegationSweeper builds a sweeper bound to the package db.DB
// (production wiring) or a test handle. Reads optional env overrides
// at construction time so a long-running process picks them up via
// restart, not mid-flight.
func NewDelegationSweeper(handle *sql.DB, ledger *DelegationLedger) *DelegationSweeper {
	if handle == nil {
		handle = db.DB
	}
	if ledger == nil {
		ledger = NewDelegationLedger(handle)
	}
	return &DelegationSweeper{
		db:        handle,
		ledger:    ledger,
		interval:  envDuration("DELEGATION_SWEEPER_INTERVAL_S", defaultSweeperInterval),
		threshold: envDuration("DELEGATION_STUCK_THRESHOLD_S", defaultStuckThreshold),
	}
}

// envDuration parses an integer-seconds env var into a Duration. Falls
// back to def on missing/invalid input — never fails fast on misconfig
// (a typo'd env var should run with sane defaults, not crash startup).
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("delegation_sweeper: invalid %s=%q, using default %s", key, v, def)
		return def
	}
	return time.Duration(n) * time.Second
}

// Interval exposes the configured tick cadence — tests use it; main.go
// uses it implicitly via Start.
func (s *DelegationSweeper) Interval() time.Duration { return s.interval }

// Threshold exposes the heartbeat-staleness threshold.
func (s *DelegationSweeper) Threshold() time.Duration { return s.threshold }

// Start ticks Sweep() at the configured interval until ctx is cancelled.
// Defers panic recovery so a single bad row can't kill the sweeper.
//
// Wired into main.go: `go sweeper.Start(ctx)`. No-op until both the
// `delegations` table (PR-1) and the result-push flag (PR-2) have rolled
// out — the sweeper just won't find any rows to mark.
func (s *DelegationSweeper) Start(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	log.Printf("DelegationSweeper: started (interval=%s, stuck-threshold=%s)",
		s.interval, s.threshold)

	tickWithRecover := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("DelegationSweeper: PANIC in tick — recovered: %v", r)
			}
		}()
		s.Sweep(ctx)
	}

	// First sweep immediately so operators see it run on startup, not
	// after waiting one interval.
	tickWithRecover()

	for {
		select {
		case <-ctx.Done():
			log.Printf("DelegationSweeper: stopped")
			return
		case <-t.C:
			tickWithRecover()
		}
	}
}

// SweepResult records what the last sweep changed. Surfaced via the
// admin dashboard (PR-4); also useful for tests to assert behavior
// without diffing log lines.
type SweepResult struct {
	DeadlineFailures int
	StuckMarked      int
	Errors           int
}

// Sweep runs one pass: find every in-flight delegation, mark deadline-
// exceeded as failed, mark heartbeat-stale as stuck. Returns counts
// for observability.
//
// SQL strategy: one indexed scan over the partial inflight index, two
// updaters per offending row. We fold both checks into a single SELECT
// to amortize the round-trip — the row count in flight at any time
// is small (single-digit hundreds even on a busy tenant), so reading
// them all and dispatching SetStatus per-row is cheaper than two
// separate UPDATEs with bespoke WHERE clauses.
func (s *DelegationSweeper) Sweep(ctx context.Context) SweepResult {
	res := SweepResult{}

	rows, err := s.db.QueryContext(ctx, `
		SELECT delegation_id, last_heartbeat, deadline
		  FROM delegations
		 WHERE status IN ('queued','dispatched','in_progress')
	`)
	if err != nil {
		log.Printf("DelegationSweeper: query failed: %v", err)
		res.Errors++
		return res
	}
	defer rows.Close()

	now := time.Now()
	type candidate struct {
		id       string
		lastBeat sql.NullTime
		deadline time.Time
	}
	var todo []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.lastBeat, &c.deadline); err != nil {
			log.Printf("DelegationSweeper: scan failed: %v", err)
			res.Errors++
			continue
		}
		todo = append(todo, c)
	}
	if err := rows.Err(); err != nil {
		log.Printf("DelegationSweeper: rows.Err: %v", err)
		res.Errors++
	}

	for _, c := range todo {
		// Deadline first — stronger statement than stuck.
		if now.After(c.deadline) {
			if err := s.ledger.SetStatus(ctx, c.id, "failed",
				"deadline exceeded by sweeper", ""); err != nil {
				log.Printf("DelegationSweeper: SetStatus(%s, failed): %v", c.id, err)
				res.Errors++
				continue
			}
			res.DeadlineFailures++
			continue
		}

		// Heartbeat staleness. A NULL last_heartbeat counts as stale ONLY
		// if the row has lived past one threshold since creation — gives
		// the agent one full window to emit its first beat. We fold this
		// by treating NULL as "created_at — but we don't have created_at
		// in the SELECT. Approximate: NULL last_heartbeat + deadline more
		// than (5h, default deadline=6h) away from now means the row was
		// created ≤1h ago, give it a free pass. Simpler heuristic: NULL
		// heartbeat is only stale if deadline is already imminent (within
		// 1 threshold).
		var lastBeat time.Time
		if c.lastBeat.Valid {
			lastBeat = c.lastBeat.Time
		}
		if !c.lastBeat.Valid {
			// Row never heartbeat. Don't mark stuck — let the deadline
			// catch it. Reduces false positives during the agent's first
			// beat window after restart.
			continue
		}
		if now.Sub(lastBeat) > s.threshold {
			if err := s.ledger.SetStatus(ctx, c.id, "stuck",
				"no heartbeat for "+now.Sub(lastBeat).Round(time.Second).String(),
				""); err != nil {
				log.Printf("DelegationSweeper: SetStatus(%s, stuck): %v", c.id, err)
				res.Errors++
				continue
			}
			res.StuckMarked++
		}
	}

	if res.DeadlineFailures > 0 || res.StuckMarked > 0 || res.Errors > 0 {
		log.Printf("DelegationSweeper: sweep complete — deadline_failures=%d stuck=%d errors=%d",
			res.DeadlineFailures, res.StuckMarked, res.Errors)
	}
	return res
}
