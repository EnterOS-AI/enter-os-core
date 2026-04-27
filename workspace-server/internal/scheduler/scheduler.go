package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	cronlib "github.com/robfig/cron/v3"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/supervised"
)

const (
	pollInterval          = 30 * time.Second
	maxConcurrent         = 10
	batchLimit            = 50
	fireTimeout           = 5 * time.Minute
	phantomSweepInterval  = 5 * time.Minute
	phantomStaleThreshold = 10 * time.Minute
	// #2026: per-DB-op deadline. Every scheduler DB call must complete
	// within this window or the Exec/Query is cancelled and the tick
	// continues. Before this, a slow/stuck DB op (bad UTF-8 rejected by
	// Postgres, connection pool exhausted, replica lag) would block a
	// fireSchedule goroutine indefinitely, which blocked wg.Wait() in
	// tick(), which stalled the entire scheduler until operator restart.
	dbQueryTimeout = 10 * time.Second
)

// sanitizeUTF8 replaces invalid UTF-8 byte sequences with the Unicode
// replacement character. Used before writing agent-produced strings to
// Postgres (text/jsonb columns reject invalid UTF-8, silently failing the
// INSERT and holding the transaction open). #2026.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

// A2AProxy is the interface the scheduler needs to send messages to workspaces.
// WorkspaceHandler.ProxyA2ARequest satisfies this.
type A2AProxy interface {
	ProxyA2ARequest(ctx context.Context, workspaceID string, body []byte, callerID string, logActivity bool) (int, []byte, error)
}

// Broadcaster records events and pushes them to WebSocket clients.
type Broadcaster interface {
	RecordAndBroadcast(ctx context.Context, eventType, workspaceID string, data interface{}) error
}

type scheduleRow struct {
	ID          string
	WorkspaceID string
	Name        string
	CronExpr    string
	Timezone    string
	Prompt      string
}

// ChannelBroadcaster posts messages to and reads context from workspace channels.
type ChannelBroadcaster interface {
	BroadcastToWorkspaceChannels(ctx context.Context, workspaceID, text string)
	FetchWorkspaceChannelContext(ctx context.Context, workspaceID string) string
}

// NativeSchedulerCheck returns true when the workspace's adapter has
// declared `provides_native_scheduler=True` in its capabilities. The
// scheduler skips polling-and-firing for these workspaces — the SDK
// runs the schedule itself (Temporal, Durable Functions, etc.) and the
// platform's polling would cause double-fire on every restart.
//
// Wired at construction by the router (production) or tests. nil is
// allowed and treated as "no override" for every workspace, preserving
// today's behavior — same default-false posture as
// BaseAdapter.capabilities() in workspace/adapter_base.py.
//
// See project memory `project_runtime_native_pluggable.md` and
// handlers.ProvidesNativeScheduler for the production wiring.
type NativeSchedulerCheck func(workspaceID string) bool

// Scheduler polls the workspace_schedules table and fires A2A messages
// when a schedule's next_run_at has passed. Follows the same goroutine
// pattern as registry.StartHealthSweep.
type Scheduler struct {
	proxy       A2AProxy
	broadcaster Broadcaster
	channels    ChannelBroadcaster

	// providesNativeScheduler, when non-nil and returning true, causes
	// tick() to skip firing for this workspace. nil = always-fire (the
	// pre-capability-primitive behavior). Constructor docs above.
	providesNativeScheduler NativeSchedulerCheck

	// lastTickAt records the wall-clock time of the most recent tick
	// (whether it fired schedules or not). Read by Healthy() and the
	// /admin/scheduler/health endpoint to detect stuck-tick conditions.
	// Atomic-ish via the mutex; tick rate is 30s so contention is trivial.
	mu           sync.RWMutex
	lastTickAt   time.Time
	lastSweepAt  time.Time
	tickInterval time.Duration // defaults to pollInterval; overridable in tests
}

func New(proxy A2AProxy, broadcaster Broadcaster) *Scheduler {
	return &Scheduler{
		proxy:        proxy,
		broadcaster:  broadcaster,
		tickInterval: pollInterval,
	}
}

// SetChannels wires the channel manager for auto-posting cron output.
// Called after both scheduler and channel manager are initialized.
func (s *Scheduler) SetChannels(ch ChannelBroadcaster) {
	s.channels = ch
}

// SetNativeSchedulerCheck wires the per-workspace native-scheduler
// override lookup. Wired by the router after the scheduler is
// constructed (handlers package owns the cache). Pass nil to disable
// the skip — every schedule fires regardless of adapter declaration,
// matching pre-capability-primitive behavior.
func (s *Scheduler) SetNativeSchedulerCheck(f NativeSchedulerCheck) {
	s.providesNativeScheduler = f
}

// LastTickAt returns the wall-clock time of the most recently completed tick.
// Returns a zero time.Time if the scheduler has never completed a tick.
func (s *Scheduler) LastTickAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastTickAt
}

// Healthy returns true if the scheduler has completed a tick within the last
// 2×pollInterval window. Returns false before the first tick or if the
// scheduler is stalled.
func (s *Scheduler) Healthy() bool {
	s.mu.RLock()
	t := s.lastTickAt
	s.mu.RUnlock()
	if t.IsZero() {
		return false
	}
	return time.Since(t) < 2*pollInterval
}

// Start runs the scheduler poll loop. Blocks until ctx is cancelled.
//
// Defends against panics inside tick() so a single bad row / bad cron
// expression / DB blip can't permanently kill the scheduler. Without
// this recover the goroutine dies and the only signal to the operator
// is "no crons firing" — which we observed as a 12+ hour silent outage
// on 2026-04-14 (issue #85).
func (s *Scheduler) Start(ctx context.Context) {
	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()

	log.Printf("Scheduler: started (poll interval=%s)", s.tickInterval)

	tickWithRecover := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Scheduler: PANIC in tick — recovered: %v (next tick in %s)", r, pollInterval)
			}
		}()
		s.tick(ctx)
		s.mu.Lock()
		s.lastTickAt = time.Now()
		s.mu.Unlock()
	}

	// #722 — startup repair: find any enabled schedule whose next_run_at was
	// NULL'd by the pre-fix bug and recompute it now. Without this pass those
	// schedules would never fire again even after the binary is updated.
	s.repairNullNextRunAt(ctx)

	// Heartbeat + initial lastTickAt so /admin/liveness and Healthy() both
	// pass during the first 30s interval after startup.
	supervised.Heartbeat("scheduler")
	s.mu.Lock()
	s.lastTickAt = time.Now()
	s.mu.Unlock()

	// Independent heartbeat pulse (#140). Decoupled from tick completion so
	// a single long fire (UIUX audits routinely take 60-120s; max fireTimeout
	// is 5min) can't make /admin/liveness look stale for the whole fire window.
	// tick() also calls Heartbeat at its top + each fire goroutine calls it
	// entry/exit — those are kept as redundant signals but this pulse is the
	// one that guarantees liveness freshness regardless of tick state.
	go func() {
		pulseTicker := time.NewTicker(10 * time.Second)
		defer pulseTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-pulseTicker.C:
				supervised.Heartbeat("scheduler")
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			log.Println("Scheduler: stopped")
			return
		case <-ticker.C:
			tickWithRecover()
			s.maybeSweepPhantomBusy(ctx)
			supervised.Heartbeat("scheduler")
		}
	}
}

// tick queries all due schedules and fires each in a goroutine.
// Waits for all goroutines to finish before returning so the next tick
// doesn't re-fire schedules whose next_run_at hasn't been updated yet.
//
// Heartbeat is called at three points to keep /admin/liveness fresh during
// long-running fires (some prompts take minutes — without these heartbeats
// the scheduler looks "stale" the whole time it's working):
//   - immediately on entering tick (proves we're past the ticker.C wait)
//   - inside each per-fire goroutine (every fire bumps the heartbeat)
//   - implicitly via the post-tick heartbeat in Start()
func (s *Scheduler) tick(ctx context.Context) {
	supervised.Heartbeat("scheduler")

	// #2026: bound the due-schedules query — if Postgres is slow/stuck
	// this fails fast instead of blocking the tick loop indefinitely.
	queryCtx, queryCancel := context.WithTimeout(ctx, dbQueryTimeout)
	rows, err := db.DB.QueryContext(queryCtx, `
		SELECT id, workspace_id, name, cron_expr, timezone, prompt
		FROM workspace_schedules
		WHERE enabled = true AND next_run_at IS NOT NULL AND next_run_at <= now()
		ORDER BY next_run_at ASC
		LIMIT $1
	`, batchLimit)
	if err != nil {
		queryCancel()
		log.Printf("Scheduler: tick query error: %v", err)
		return
	}
	defer queryCancel()
	defer rows.Close()

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)
	for rows.Next() {
		var sched scheduleRow
		if err := rows.Scan(&sched.ID, &sched.WorkspaceID, &sched.Name, &sched.CronExpr, &sched.Timezone, &sched.Prompt); err != nil {
			log.Printf("Scheduler: scan error: %v", err)
			continue
		}
		// Skip workspaces whose adapter owns scheduling natively (e.g.
		// SDKs with built-in cron / Temporal-style workflows). Without
		// this skip, the platform's polling would fire the same
		// schedule twice — once natively in the SDK, once via this
		// loop. The skip drops only the FIRE; the schedule row stays
		// in the DB and the platform still records it, so observability
		// (next_run_at, last_run_at) is preserved per the principle.
		// Pre-fix this branch was unconditional; nil check preserves
		// behavior for callers that didn't wire the override.
		if s.providesNativeScheduler != nil && s.providesNativeScheduler(sched.WorkspaceID) {
			// Advance next_run_at so we don't tight-loop on the same
			// row every tick. A non-firing schedule is still scheduled.
			if nextTime, err := ComputeNextRun(sched.CronExpr, sched.Timezone, time.Now()); err == nil {
				if _, execErr := db.DB.ExecContext(ctx,
					`UPDATE workspace_schedules SET next_run_at=$1, updated_at=now() WHERE id=$2`,
					nextTime, sched.ID); execErr != nil {
					log.Printf("Scheduler: native-skip next_run_at UPDATE failed for schedule %s: %v", sched.ID, execErr)
				}
			}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(s2 scheduleRow) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Scheduler: PANIC firing '%s' on workspace %s — recovered: %v",
						s2.Name, s2.WorkspaceID, r)
					// Always advance next_run_at even on panic so the schedule doesn't get
					// stuck re-firing the same panicking schedule indefinitely (#1029).
					if nextTime, err := ComputeNextRun(s2.CronExpr, s2.Timezone, time.Now()); err == nil {
						// F1089: use context.Background() so the panic-recovery UPDATE is not
						// silently skipped if the outer ctx was cancelled during the panic window.
						if _, execErr := db.DB.ExecContext(context.Background(), `UPDATE workspace_schedules SET next_run_at=$1, updated_at=now() WHERE id=$2`, nextTime, s2.ID); execErr != nil {
							log.Printf("Scheduler: panic-recovery next_run_at UPDATE failed for schedule %s: %v", s2.ID, execErr)
						}
					}
				}
			}()
			supervised.Heartbeat("scheduler")
			s.fireSchedule(ctx, s2)
			supervised.Heartbeat("scheduler")
		}(sched)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Scheduler: rows error: %v", err)
	}
	wg.Wait()

	// Record tick completion time for health monitoring.
	s.mu.Lock()
	s.lastTickAt = time.Now()
	s.mu.Unlock()
}

// fireSchedule sends the A2A message and updates the schedule row.
// A deferred recover guards against panics in the A2A proxy so that a single
// misbehaving workspace cannot crash the scheduler goroutine pool.
func (s *Scheduler) fireSchedule(ctx context.Context, sched scheduleRow) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Scheduler: panic recovered in fireSchedule for '%s' (%s): %v",
				sched.Name, sched.ID, r)
			// Always advance next_run_at even on panic so the schedule doesn't get
			// stuck re-firing the same panicking schedule indefinitely (#1029).
			if nextTime, err := ComputeNextRun(sched.CronExpr, sched.Timezone, time.Now()); err == nil {
				// F1089: use context.Background() so the panic-recovery UPDATE is not
				// silently skipped if the outer ctx was cancelled during the panic window.
				if _, execErr := db.DB.ExecContext(context.Background(), `UPDATE workspace_schedules SET next_run_at=$1, updated_at=now() WHERE id=$2`, nextTime, sched.ID); execErr != nil {
					log.Printf("Scheduler: panic-recovery next_run_at UPDATE failed for schedule %s: %v", sched.ID, execErr)
				}
			}
		}
	}()

	// #969 concurrency-aware queue — when the target workspace is busy,
	// defer the fire instead of skipping. Polls every 10s for up to 2 min
	// waiting for the workspace to become idle. If still busy after 2 min,
	// falls back to the original skip behavior.
	//
	// This replaces the #115 "skip when busy" pattern which caused crons
	// to permanently miss when workspaces were perpetually busy from the
	// Orchestrator pulse delegation chain (~30% message drop rate on Dev Lead).
	// Check workspace capacity — fire when active_tasks < max_concurrent_tasks.
	// Default max is 1 (backward compatible). Workspaces can override via config
	// to allow concurrent task processing (e.g. leaders handling A2A while cron runs).
	var activeTasks int
	var maxConcurrent int
	// #2026: bound the capacity check — if the DB is slow, fail open
	// (skip the capacity wait, let fireTimeout catch a truly stuck fire)
	// rather than blocking here indefinitely.
	capCtx, capCancel := context.WithTimeout(ctx, dbQueryTimeout)
	capErr := db.DB.QueryRowContext(capCtx,
		`SELECT COALESCE(active_tasks, 0), COALESCE(max_concurrent_tasks, 1) FROM workspaces WHERE id = $1`,
		sched.WorkspaceID,
	).Scan(&activeTasks, &maxConcurrent)
	capCancel()
	if capErr == nil && activeTasks >= maxConcurrent {
		log.Printf("Scheduler: '%s' workspace %s at capacity (active_tasks=%d, max=%d), deferring up to 2 min",
			sched.Name, short(sched.WorkspaceID, 12), activeTasks, maxConcurrent)
		// Poll every 10s for up to 2 minutes
		waited := false
		for i := 0; i < 12; i++ {
			time.Sleep(10 * time.Second)
			pollCtx, pollCancel := context.WithTimeout(ctx, dbQueryTimeout)
			err := db.DB.QueryRowContext(pollCtx,
				`SELECT COALESCE(active_tasks, 0), COALESCE(max_concurrent_tasks, 1) FROM workspaces WHERE id = $1`,
				sched.WorkspaceID,
			).Scan(&activeTasks, &maxConcurrent)
			pollCancel()
			if err != nil || activeTasks < maxConcurrent {
				waited = true
				break
			}
		}
		if !waited && activeTasks >= maxConcurrent {
			log.Printf("Scheduler: skipping '%s' on busy workspace %s after 2 min wait (active_tasks=%d, max=%d)",
				sched.Name, short(sched.WorkspaceID, 12), activeTasks, maxConcurrent)
			s.recordSkipped(ctx, sched, activeTasks)
			return
		}
		log.Printf("Scheduler: '%s' workspace %s has capacity after deferral, firing",
			sched.Name, short(sched.WorkspaceID, 12))
	}

	fireCtx, cancel := context.WithTimeout(ctx, fireTimeout)
	defer cancel()

	// Level 3: inject ambient Slack channel context into the cron prompt.
	// The agent sees recent peer messages before acting, enabling cross-agent
	// awareness without explicit A2A delegation. Best-effort — if the fetch
	// fails or the workspace has no Slack channels, the prompt is unchanged.
	prompt := sched.Prompt
	if s.channels != nil {
		if channelCtx := s.channels.FetchWorkspaceChannelContext(fireCtx, sched.WorkspaceID); channelCtx != "" {
			prompt = channelCtx + "\n" + prompt
		}
	}

	msgID := fmt.Sprintf("cron-%s-%s", short(sched.ID, 8), uuid.New().String()[:8])

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":      "user",
				"messageId": msgID,
				"parts":     []map[string]interface{}{{"kind": "text", "text": prompt}},
			},
		},
	})

	log.Printf("Scheduler: firing '%s' → workspace %s", sched.Name, short(sched.WorkspaceID, 12))

	// Empty callerID = canvas-style request (bypasses access control, source_id=NULL in activity log).
	// "system:scheduler" was invalid — source_id column is UUID and rejects non-UUID strings.
	statusCode, respBody, proxyErr := s.proxy.ProxyA2ARequest(fireCtx, sched.WorkspaceID, a2aBody, "", true)

	lastStatus := "ok"
	lastError := ""
	if proxyErr != nil {
		lastStatus = "error"
		lastError = fmt.Sprintf("%v", proxyErr)
		log.Printf("Scheduler: '%s' error: %v", sched.Name, proxyErr)
	} else if statusCode < 200 || statusCode >= 300 {
		lastStatus = "error"
		lastError = fmt.Sprintf("HTTP %d", statusCode)
		log.Printf("Scheduler: '%s' non-2xx: %d", sched.Name, statusCode)
	} else {
		log.Printf("Scheduler: '%s' completed (HTTP %d)", sched.Name, statusCode)
	}

	// #795: detect phantom-producing schedules — cron fires successfully
	// but the agent returns empty or "(no response generated)". Track
	// consecutive empties and escalate to 'stale' after 3 in a row.
	isEmpty := isEmptyResponse(respBody)
	if lastStatus == "ok" && isEmpty {
		// One query instead of UPDATE-then-SELECT: RETURNING hands back
		// the post-increment value so the stale-threshold check doesn't
		// cost a second roundtrip. This handler fires once per cron tick
		// per schedule; at 100 tenants × dozens of schedules the saved
		// query matters.
		var consecEmpty int
		// #2026: bound the empty-run UPDATE — survives outer ctx cancellation
		// (uses Background()) so the bookkeeping completes even if fireTimeout
		// cancelled the HTTP call, and has its own deadline so a stuck DB
		// can't block the goroutine.
		emptyCtx, emptyCancel := context.WithTimeout(context.Background(), dbQueryTimeout)
		if err := db.DB.QueryRowContext(emptyCtx, `
			UPDATE workspace_schedules
			SET consecutive_empty_runs = consecutive_empty_runs + 1,
			    updated_at = now()
			WHERE id = $1
			RETURNING consecutive_empty_runs`, sched.ID).Scan(&consecEmpty); err != nil {
			log.Printf("Scheduler: '%s' empty-run bump failed: %v", sched.Name, err)
		}
		emptyCancel()
		if consecEmpty >= 3 {
			lastStatus = "stale"
			lastError = fmt.Sprintf("empty response %d consecutive times — agent may be phantom-producing (#795)", consecEmpty)
			log.Printf("Scheduler: '%s' STALE — %d consecutive empty responses (workspace %s)",
				sched.Name, consecEmpty, short(sched.WorkspaceID, 12))
		}
	} else if lastStatus == "ok" {
		// Non-empty success — reset the counter
		resetCtx, resetCancel := context.WithTimeout(context.Background(), dbQueryTimeout)
		_, _ = db.DB.ExecContext(resetCtx, `
			UPDATE workspace_schedules
			SET consecutive_empty_runs = 0,
			    updated_at = now()
			WHERE id = $1`, sched.ID)
		resetCancel()
	}

	nextRun, nextErr := ComputeNextRun(sched.CronExpr, sched.Timezone, time.Now())
	var nextRunPtr *time.Time
	if nextErr == nil {
		nextRunPtr = &nextRun
	} else {
		// #722: if ComputeNextRun fails, keep the existing next_run_at so the
		// schedule is not silently removed from the fire query (NULL next_run_at
		// is excluded by the tick WHERE clause). COALESCE($2, next_run_at) does
		// this: when $2 is NULL the DB column value is preserved as-is.
		log.Printf("Scheduler: ComputeNextRun error for '%s' (%s) — preserving existing next_run_at: %v",
			sched.Name, sched.ID, nextErr)
	}

	// F1089: use a dedicated context with its own 5s deadline for the
	// post-fire UPDATE. The outer ctx (fireCtx) may be cancelled if the
	// HTTP call timed out or the server is shutting down; using it here
	// would silently skip the UPDATE and leave next_run_at stale, causing
	// the schedule to be immediately re-fired on the next tick.
	updateCtx, updateCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer updateCancel()

	_, err := db.DB.ExecContext(updateCtx, `
		UPDATE workspace_schedules
		SET last_run_at = now(),
		    next_run_at = COALESCE($2, next_run_at),
		    run_count = run_count + 1,
		    last_status = $3,
		    last_error = $4,
		    updated_at = now()
		WHERE id = $1
	`, sched.ID, nextRunPtr, lastStatus, lastError)
	if err != nil {
		log.Printf("Scheduler: post-fire update error for %s [%s]: %v", sched.ID, sched.Name, err)
	}

	// Log a dedicated cron_run activity entry with schedule metadata so the
	// history endpoint can query by schedule_id.
	// #2026: sanitize the truncated prompt — even UTF-8-safe truncate() can
	// carry pre-existing invalid bytes from an agent-edited template. jsonb
	// columns reject invalid UTF-8 and hold the transaction open.
	cronMeta, _ := json.Marshal(map[string]interface{}{
		"schedule_id":   sched.ID,
		"schedule_name": sched.Name,
		"cron_expr":     sched.CronExpr,
		"prompt":        sanitizeUTF8(truncate(sched.Prompt, 200)),
	})
	// #152: persist lastError into error_detail on the activity_logs row
	// so GET /workspaces/:id/schedules/:id/history can surface why a run
	// failed (previously dropped — history returned status without any
	// error context, making root-cause debugging impossible).
	// #2026: bounded Background() context — this INSERT was observed wedging
	// indefinitely on invalid-UTF-8 jsonb payloads, blocking wg.Wait() in
	// tick() and stalling the whole scheduler. Now: 10s deadline, survives
	// outer ctx cancellation, and every string is UTF-8 sanitized.
	insertCtx, insertCancel := context.WithTimeout(context.Background(), dbQueryTimeout)
	if _, insErr := db.DB.ExecContext(insertCtx, `
		INSERT INTO activity_logs (workspace_id, activity_type, source_id, method, summary, request_body, status, error_detail, created_at)
		VALUES ($1, 'cron_run', NULL, 'cron', $2, $3::jsonb, $4, $5, now())
	`, sched.WorkspaceID, sanitizeUTF8("Cron: "+sched.Name), string(cronMeta), lastStatus, sanitizeUTF8(lastError)); insErr != nil {
		log.Printf("Scheduler: activity_logs insert failed for '%s' (%s): %v", sched.Name, sched.ID, insErr)
	}
	insertCancel()

	if s.broadcaster != nil {
		s.broadcaster.RecordAndBroadcast(ctx, "CRON_EXECUTED", sched.WorkspaceID, map[string]interface{}{
			"schedule_id":   sched.ID,
			"schedule_name": sched.Name,
			"status":        lastStatus,
		})
	}

	// Level 1: auto-post cron output to workspace's Slack channels.
	// Only post non-empty successful responses — errors and empties are
	// noise that clutters the channel without adding value.
	if s.channels != nil && lastStatus == "ok" && !isEmpty {
		summary := s.extractResponseSummary(respBody)
		if summary != "" {
			go func(wsID, text string) {
				postCtx, postCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer postCancel()
				s.channels.BroadcastToWorkspaceChannels(postCtx, wsID, text)
			}(sched.WorkspaceID, summary)
		}
	}
}

// recordSkipped advances next_run_at and logs a cron_run activity entry
// with status='skipped' when the target workspace was already busy.
// Issue #115 — replaces the previous "busy → fire → fail → retry next
// tick" loop with "busy → skip → advance → try next slot". Keeps the
// history surface honest (a skip is not an error) and stops filling
// last_error with noise.
func (s *Scheduler) recordSkipped(ctx context.Context, sched scheduleRow, activeTasks int) {
	reason := fmt.Sprintf("skipped: workspace busy (active_tasks=%d)", activeTasks)

	nextRun, nextErr := ComputeNextRun(sched.CronExpr, sched.Timezone, time.Now())
	var nextRunPtr *time.Time
	if nextErr == nil {
		nextRunPtr = &nextRun
	} else {
		// #722: same guard as in fireSchedule — preserve existing next_run_at
		// rather than writing NULL when the cron expression cannot be parsed.
		log.Printf("Scheduler: ComputeNextRun error in recordSkipped for '%s' (%s) — preserving existing next_run_at: %v",
			sched.Name, sched.ID, nextErr)
	}

	// Advance next_run_at + bump run_count so the liveness view reflects
	// that we're still ticking. last_status='skipped', last_error carries
	// the reason for operators debugging via the schedule history API.
	// #2026: bounded Background() context so the bookkeeping can't block
	// on a stuck DB and stall the scheduler.
	skipUpdCtx, skipUpdCancel := context.WithTimeout(context.Background(), dbQueryTimeout)
	_, _ = db.DB.ExecContext(skipUpdCtx, `
		UPDATE workspace_schedules
		SET last_run_at = now(),
		    next_run_at = COALESCE($2, next_run_at),
		    run_count = run_count + 1,
		    last_status = 'skipped',
		    last_error = $3,
		    updated_at = now()
		WHERE id = $1
	`, sched.ID, nextRunPtr, sanitizeUTF8(reason))
	skipUpdCancel()

	cronMeta, _ := json.Marshal(map[string]interface{}{
		"schedule_id":   sched.ID,
		"schedule_name": sched.Name,
		"cron_expr":     sched.CronExpr,
		"skipped":       true,
		"active_tasks":  activeTasks,
	})
	// #2026: bounded Background() context on the skipped activity log INSERT
	// for the same reason as the fireSchedule activity_logs INSERT above.
	skipInsCtx, skipInsCancel := context.WithTimeout(context.Background(), dbQueryTimeout)
	_, _ = db.DB.ExecContext(skipInsCtx, `
		INSERT INTO activity_logs (workspace_id, activity_type, source_id, method, summary, request_body, status, error_detail, created_at)
		VALUES ($1, 'cron_run', NULL, 'cron', $2, $3::jsonb, 'skipped', $4, now())
	`, sched.WorkspaceID, sanitizeUTF8("Cron skipped: "+sched.Name), string(cronMeta), sanitizeUTF8(reason))
	skipInsCancel()

	if s.broadcaster != nil {
		_ = s.broadcaster.RecordAndBroadcast(ctx, "CRON_SKIPPED", sched.WorkspaceID, map[string]interface{}{
			"schedule_id":   sched.ID,
			"schedule_name": sched.Name,
			"reason":        reason,
		})
	}
}

// repairNullNextRunAt is called once during Start() to recompute next_run_at
// for any enabled schedule where it is NULL — a state left by the pre-#722 bug
// where a ComputeNextRun error caused an UPDATE that wrote NULL.
// Without this repair those schedules would never appear in the tick query
// (which requires next_run_at IS NOT NULL) even after the binary is patched.
func (s *Scheduler) repairNullNextRunAt(ctx context.Context) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, cron_expr, timezone
		FROM workspace_schedules
		WHERE enabled = true AND next_run_at IS NULL
	`)
	if err != nil {
		log.Printf("Scheduler: startup repair query error: %v", err)
		return
	}
	defer rows.Close()

	type repairRow struct {
		ID       string
		CronExpr string
		Timezone string
	}

	var repaired, failed int
	for rows.Next() {
		var r repairRow
		if err := rows.Scan(&r.ID, &r.CronExpr, &r.Timezone); err != nil {
			log.Printf("Scheduler: startup repair scan error: %v", err)
			continue
		}
		nextRun, err := ComputeNextRun(r.CronExpr, r.Timezone, time.Now())
		if err != nil {
			log.Printf("Scheduler: startup repair: cannot compute next_run_at for schedule %s (%s): %v — leaving NULL",
				r.ID, r.CronExpr, err)
			failed++
			continue
		}
		if _, err := db.DB.ExecContext(ctx, `
			UPDATE workspace_schedules SET next_run_at = $2, updated_at = now() WHERE id = $1
		`, r.ID, nextRun); err != nil {
			log.Printf("Scheduler: startup repair: update failed for schedule %s: %v", r.ID, err)
			failed++
		} else {
			repaired++
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Scheduler: startup repair rows error: %v", err)
	}
	if repaired > 0 || failed > 0 {
		log.Printf("Scheduler: startup repair: %d schedule(s) repaired, %d skipped (bad cron/tz)", repaired, failed)
	}
}

// maybeSweepPhantomBusy runs sweepPhantomBusy at most once every
// phantomSweepInterval (5 min). Called on every tick but gated by a timer
// so the DB query doesn't run on every 30s poll.
func (s *Scheduler) maybeSweepPhantomBusy(ctx context.Context) {
	s.mu.RLock()
	last := s.lastSweepAt
	s.mu.RUnlock()

	if time.Since(last) < phantomSweepInterval {
		return
	}

	s.sweepPhantomBusy(ctx)

	s.mu.Lock()
	s.lastSweepAt = time.Now()
	s.mu.Unlock()
}

// sweepPhantomBusy finds workspaces stuck with active_tasks > 0 but no
// recent activity_log entry (within phantomStaleThreshold). This happens
// when an agent errors out (MiniMax timeout, OOM, etc.) and the finally
// block fails to decrement active_tasks. Without this sweep the scheduler
// skips cron fires for those workspaces indefinitely ("workspace busy —
// retry"), requiring manual DB intervention.
//
// The query mirrors the manual fix that was being run every 30 min:
//
//	UPDATE workspaces SET active_tasks = 0
//	WHERE active_tasks > 0
//	  AND id NOT IN (SELECT DISTINCT workspace_id
//	                 FROM activity_logs
//	                 WHERE created_at > NOW() - INTERVAL '10 minutes')
func (s *Scheduler) sweepPhantomBusy(ctx context.Context) {
	rows, err := db.DB.QueryContext(ctx, `
		UPDATE workspaces
		SET active_tasks = 0,
		    current_task = '',
		    updated_at   = now()
		WHERE active_tasks > 0
		  AND status != 'removed'
		  AND id NOT IN (
		      SELECT DISTINCT workspace_id
		      FROM activity_logs
		      WHERE created_at > NOW() - $1::interval
		  )
		RETURNING id, name
	`, fmt.Sprintf("%d minutes", int(phantomStaleThreshold.Minutes())))
	if err != nil {
		log.Printf("Scheduler: phantom-busy sweep query error: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			log.Printf("Scheduler: phantom-busy sweep scan error: %v", err)
			continue
		}
		log.Printf("Scheduler: phantom-busy sweep — reset %s (no activity in %d min)", name, int(phantomStaleThreshold.Minutes()))
		count++
	}
	if err := rows.Err(); err != nil {
		log.Printf("Scheduler: phantom-busy sweep rows error: %v", err)
	}
	if count > 0 {
		log.Printf("Scheduler: phantom-busy sweep complete — reset %d workspace(s)", count)
	}
}

// isEmptyResponse checks if an A2A response body indicates the agent
// produced no meaningful output. Catches "(no response generated)" from
// the workspace runtime + genuinely empty/null responses. Used by the
// consecutive-empty tracker (#795) to detect phantom-producing crons.
// extractResponseSummary pulls the agent's text from the A2A response body.
// Returns empty string if parsing fails or the response has no text content.
func (s *Scheduler) extractResponseSummary(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var resp map[string]interface{}
	if json.Unmarshal(body, &resp) != nil {
		return ""
	}
	// A2A response: result.parts[].text
	if result, ok := resp["result"].(map[string]interface{}); ok {
		if parts, ok := result["parts"].([]interface{}); ok {
			for _, p := range parts {
				if part, ok := p.(map[string]interface{}); ok {
					if text, ok := part["text"].(string); ok && text != "" {
						return text
					}
				}
			}
		}
	}
	return ""
}

func isEmptyResponse(body []byte) bool {
	if len(body) == 0 {
		return true
	}
	s := string(body)
	// The A2A response wraps the agent text in {"result":{"parts":[{"text":"..."}]}}
	// Check for the sentinel the workspace runtime emits when the agent produces nothing.
	for _, marker := range []string{
		`(no response generated)`,
		`"text": "(no response generated)"`,
		`"text":""`,
		`"text": ""`,
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// truncate shortens s to at most maxLen bytes, appending "..." if truncated.
// #2026: UTF-8 safe — byte-slicing at maxLen-3 would split multi-byte runes
// (observed: U+2026 `…` = 0xe2 0x80 0xa6, sliced mid-char, concatenated with
// "..." producing 0xe2 0x80 0x2e — rejected by Postgres as invalid UTF-8,
// which wedged the activity_logs INSERT with no deadline and stalled the
// scheduler).
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	cut := maxLen - 3
	if cut < 0 {
		cut = 0
	}
	// Back up to a rune boundary — utf8.RuneStart returns true for any
	// non-continuation byte (ASCII, or the lead byte of a multi-byte rune).
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// short returns up to n leading characters of s without panicking when s is
// shorter than n. Used to safely display UUID prefixes in log lines where
// the full ID would be noisy but the full-length bounds check is repetitive.
func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ComputeNextRun parses a cron expression and returns the next fire time
// after the given time, in the specified timezone.
func ComputeNextRun(cronExpr, tz string, after time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timezone %q: %w", tz, err)
	}

	parser := cronlib.NewParser(cronlib.Minute | cronlib.Hour | cronlib.Dom | cronlib.Month | cronlib.Dow)
	sched, err := parser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}

	return sched.Next(after.In(loc)).UTC(), nil
}
