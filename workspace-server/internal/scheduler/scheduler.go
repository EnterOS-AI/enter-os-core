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

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/metrics"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/supervised"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/textutil"
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
	// priorityTask mirrors handlers.PriorityTask (50) — the default FIFO A2A
	// queue priority. Duplicated as a local const because the scheduler cannot
	// import internal/handlers (handlers imports scheduler → cycle). Buffered
	// cron ticks enqueue at the same priority as normal busy-retry A2A work.
	priorityTask = 50
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
// WorkspaceHandler.ProxyA2ARequest + WorkspaceHandler.EnqueueA2A satisfy this.
type A2AProxy interface {
	ProxyA2ARequest(ctx context.Context, workspaceID string, body []byte, callerID string, logActivity bool) (int, []byte, error)
	// EnqueueA2A durably buffers an A2A message for a busy workspace; the
	// drain dispatches it serially when the agent frees. idempotencyKey
	// collapses duplicate pending buffers per (workspace,key). Returns the
	// buffered entry id, the resulting pending depth, and any error.
	EnqueueA2A(ctx context.Context, workspaceID, callerID string, priority int, body []byte, method, idempotencyKey string, expiresAt *time.Time) (string, int, error)
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
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC recovered in scheduler heartbeat goroutine: %v", r)
			}
		}()
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

	fireCtx, cancel := context.WithTimeout(ctx, fireTimeout)
	defer cancel()

	// Level 3: inject ambient Slack channel context into the cron prompt.
	// The agent sees recent peer messages before acting, enabling cross-agent
	// awareness without explicit A2A delegation. Best-effort — if the fetch
	// fails or the workspace has no Slack channels, the prompt is unchanged.
	//
	// Built BEFORE the capacity check so the busy-enqueue path below buffers
	// the exact same A2A message the fire path would have dispatched.
	prompt := sched.Prompt
	if s.channels != nil {
		if channelCtx := s.channels.FetchWorkspaceChannelContext(fireCtx, sched.WorkspaceID); channelCtx != "" {
			prompt = channelCtx + "\n" + prompt
		}
	}

	msgID := fmt.Sprintf("cron-%s-%s", short(sched.ID, 8), uuid.New().String()[:8])

	a2aBody, marshalErr := json.Marshal(map[string]interface{}{
		"method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":      "user",
				"messageId": msgID,
				"parts":     []map[string]interface{}{{"kind": "text", "text": prompt}},
			},
		},
	})
	if marshalErr != nil {
		log.Printf("Scheduler '%s': json.Marshal a2aBody failed: %v", sched.Name, marshalErr)
		return
	}

	// #969 → durable buffering. When the target workspace is busy
	// (active_tasks >= max_concurrent_tasks) we do NOT skip the tick and we do
	// NOT block the scheduler goroutine waiting for capacity. Instead we durably
	// buffer the cron message, mirroring how busy A2A dispatches already buffer.
	// The drain then dispatches it serially the moment the agent frees —
	// execution stays one-at-a-time; max_concurrent_tasks is unchanged.
	//
	// This supersedes the previous "poll then recordSkipped" behavior, which
	// dropped scheduled ticks on workspaces that stayed busy across the whole
	// poll window.
	//
	// Idempotency key = sched.ID (the SCHEDULE id), NOT msgID/a random uuid.
	// Keying by schedule_id means a busy agent buffers AT MOST ONE pending tick
	// per schedule — the latest one wins, the obsolete newer tick is collapsed —
	// so we hold the next tick instead of stacking a stale backlog.
	if capErr == nil && activeTasks >= maxConcurrent {
		// Buffered ticks expire at the next scheduled fire: a tick that's been
		// sitting in the queue past when the cron would naturally tick again is
		// stale, so let it expire rather than fire late. Best-effort — on a bad
		// cron expr we enqueue with no TTL (NULL) rather than block the tick.
		var expiresAt *time.Time
		if nextRun, nrErr := ComputeNextRun(sched.CronExpr, sched.Timezone, time.Now()); nrErr == nil {
			expiresAt = &nextRun
		}
		enqCtx, enqCancel := context.WithTimeout(ctx, dbQueryTimeout)
		// Empty callerID = canvas-style (source_id NULL), matching the fire path.
		qID, depth, enqErr := s.proxy.EnqueueA2A(enqCtx, sched.WorkspaceID, "", priorityTask, a2aBody, "message/send", sched.ID, expiresAt)
		enqCancel()
		if enqErr != nil {
			// Enqueue failed — fall back to recording a skip so the liveness
			// view still advances and the operator sees the error, rather than
			// silently dropping the tick or firing into a busy agent.
			log.Printf("Scheduler: '%s' enqueue on busy workspace %s failed, recording skip: %v",
				sched.Name, short(sched.WorkspaceID, 12), enqErr)
			s.recordSkipped(ctx, sched, activeTasks)
			return
		}
		log.Printf("Scheduler: '%s' workspace %s busy (active_tasks=%d, max=%d) — enqueued tick %s (queue depth=%d), will drain when idle",
			sched.Name, short(sched.WorkspaceID, 12), activeTasks, maxConcurrent, short(qID, 8), depth)
		s.recordQueued(ctx, sched, activeTasks, qID, depth)
		return
	}

	log.Printf("Scheduler: firing '%s' → workspace %s", sched.Name, short(sched.WorkspaceID, 12))

	// Empty callerID = canvas-style request (bypasses access control, source_id=NULL in activity log).
	// "system:scheduler" was invalid — source_id column is UUID and rejects non-UUID strings.
	statusCode, respBody, proxyErr := s.proxy.ProxyA2ARequest(fireCtx, sched.WorkspaceID, a2aBody, "", true)

	lastStatus := "ok"
	lastError := ""
	resultKind := ""
	if proxyErr != nil {
		lastStatus = "error"
		lastError = fmt.Sprintf("%v", proxyErr)
		log.Printf("Scheduler: '%s' error: %v", sched.Name, proxyErr)
	} else if statusCode < 200 || statusCode >= 300 {
		lastStatus = "error"
		lastError = fmt.Sprintf("HTTP %d", statusCode)
		log.Printf("Scheduler: '%s' non-2xx: %d", sched.Name, statusCode)
	} else if a2aErr := a2aErrorFromBody(respBody); a2aErr != "" {
		lastStatus = "error"
		lastError = fmt.Sprintf("A2A adapter error: %s", a2aErr)
		log.Printf("Scheduler: '%s' A2A adapter error (HTTP %d): %s", sched.Name, statusCode, a2aErr)
	} else {
		// HTTP 200 — inspect response body for SDK-layer errors.
		// The claude-code-sdk adapter returns HTTP 200 even when the inner
		// LLM call throws (e.g. Max-plan rate-limit, quota exhaustion, SDK
		// internal errors). Without this check those failures surface as
		// "completed (HTTP 200)" in last_status while the agent chat shows
		// errors — a silent failure that hides schedule outages.
		// See: #1696.
		resultKind = detectResultKind(respBody)
		if resultKind != "" && resultKind != "ok" {
			lastStatus = resultKind
			lastError = fmt.Sprintf("SDK error: result_kind=%s", resultKind)
			log.Printf("Scheduler: '%s' SDK error detected — result_kind=%s", sched.Name, resultKind)
		} else {
			log.Printf("Scheduler: '%s' completed (HTTP %d)", sched.Name, statusCode)
		}
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
		if _, err := db.DB.ExecContext(resetCtx, `
			UPDATE workspace_schedules
			SET consecutive_empty_runs = 0,
			    updated_at = now()
			WHERE id = $1`, sched.ID); err != nil {
			log.Printf("Scheduler: '%s' empty-run reset failed: %v", sched.Name, err)
		}
		resetCancel()
	}

	// #1696: track consecutive SDK errors. When the adapter returns HTTP 200
	// but the response body signals a non-ok result_kind (rate_limited,
	// sdk_error, quota_exhausted), we increment a counter. After 3 consecutive
	// SDK errors we auto-disable the schedule and log it — the schedule is
	// suffering a persistent LLM-layer failure and firing it again will keep
	// producing the same errors while burning tokens.
	//
	// Only apply when the current lastStatus is a non-ok resultKind (not when
	// we already have 'error' from proxyErr or non-2xx HTTP status — those have
	// their own failure semantics). Also skip when lastStatus is 'stale' (the
	// empty-response escalation path takes priority).
	var consecSDK int
	if resultKind != "" && resultKind != "ok" {
		sdkCtx, sdkCancel := context.WithTimeout(context.Background(), dbQueryTimeout)
		if err := db.DB.QueryRowContext(sdkCtx, `
			UPDATE workspace_schedules
			SET consecutive_sdk_errors = consecutive_sdk_errors + 1,
			    updated_at = now()
			WHERE id = $1
			RETURNING consecutive_sdk_errors`, sched.ID).Scan(&consecSDK); err != nil {
			log.Printf("Scheduler: '%s' SDK-error bump failed: %v", sched.Name, err)
		}
		sdkCancel()
		if consecSDK >= 3 {
			log.Printf("Scheduler: '%s' AUTO-DISABLING after %d consecutive SDK errors (workspace %s)",
				sched.Name, consecSDK, short(sched.WorkspaceID, 12))
			autoDisableCtx, autoDisableCancel := context.WithTimeout(context.Background(), dbQueryTimeout)
			if _, err := db.DB.ExecContext(autoDisableCtx, `
				UPDATE workspace_schedules SET enabled = false, updated_at = now() WHERE id = $1 AND enabled = true`,
				sched.ID); err != nil {
				log.Printf("Scheduler: '%s' auto-disable failed: %v", sched.Name, err)
			}
			autoDisableCancel()
		}
	} else {
		// Non-SDK-error run — reset the counter.
		// Guard: only reset when lastStatus is a clean ok (not 'stale', not
		// 'error', not resultKind). An 'ok' resultKind means the SDK is fine
		// and we should clear the streak.
		if lastStatus == "ok" {
			resetCtx, resetCancel := context.WithTimeout(context.Background(), dbQueryTimeout)
			if _, err := db.DB.ExecContext(resetCtx, `
				UPDATE workspace_schedules
				SET consecutive_sdk_errors = 0,
				    updated_at = now()
				WHERE id = $1`, sched.ID); err != nil {
				log.Printf("Scheduler: '%s' SDK-error reset failed: %v", sched.Name, err)
			}
			resetCancel()
		}
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
	cronMeta, marshalErr := json.Marshal(map[string]interface{}{
		"schedule_id":   sched.ID,
		"schedule_name": sched.Name,
		"cron_expr":     sched.CronExpr,
		"prompt":        sanitizeUTF8(textutil.TruncateBytes(sched.Prompt, 200)),
	})
	if marshalErr != nil {
		log.Printf("Scheduler '%s': json.Marshal cronMeta failed: %v", sched.Name, marshalErr)
	} else {
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
	}

	if s.broadcaster != nil {
		s.broadcaster.RecordAndBroadcast(ctx, string(events.EventCronExecuted), sched.WorkspaceID, map[string]interface{}{
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
				defer func() {
					if r := recover(); r != nil {
						log.Printf("PANIC recovered in broadcast summary goroutine: %v", r)
					}
				}()
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
	if _, err := db.DB.ExecContext(skipUpdCtx, `
		UPDATE workspace_schedules
		SET last_run_at = now(),
		    next_run_at = COALESCE($2, next_run_at),
		    run_count = run_count + 1,
		    last_status = 'skipped',
		    last_error = $3,
		    updated_at = now()
		WHERE id = $1
	`, sched.ID, nextRunPtr, sanitizeUTF8(reason)); err != nil {
		log.Printf("Scheduler: '%s' skip update failed: %v", sched.Name, err)
	}
	skipUpdCancel()

	cronMeta, marshalErr := json.Marshal(map[string]interface{}{
		"schedule_id":   sched.ID,
		"schedule_name": sched.Name,
		"cron_expr":     sched.CronExpr,
		"skipped":       true,
		"active_tasks":  activeTasks,
	})
	if marshalErr != nil {
		log.Printf("Scheduler '%s': json.Marshal cronMeta failed: %v", sched.Name, marshalErr)
	} else {
		// #2026: bounded Background() context on the skipped activity log INSERT
		// for the same reason as the fireSchedule activity_logs INSERT above.
		skipInsCtx, skipInsCancel := context.WithTimeout(context.Background(), dbQueryTimeout)
		if _, err := db.DB.ExecContext(skipInsCtx, `
			INSERT INTO activity_logs (workspace_id, activity_type, source_id, method, summary, request_body, status, error_detail, created_at)
			VALUES ($1, 'cron_run', NULL, 'cron', $2, $3::jsonb, 'skipped', $4, now())
		`, sched.WorkspaceID, sanitizeUTF8("Cron skipped: "+sched.Name), string(cronMeta), sanitizeUTF8(reason)); err != nil {
			log.Printf("Scheduler: '%s' skip activity log failed: %v", sched.Name, err)
		}
		skipInsCancel()
	}

	if s.broadcaster != nil {
		_ = s.broadcaster.RecordAndBroadcast(ctx, string(events.EventCronSkipped), sched.WorkspaceID, map[string]interface{}{
			"schedule_id":   sched.ID,
			"schedule_name": sched.Name,
			"reason":        reason,
		})
	}
}

// recordQueued advances next_run_at and logs a cron_run activity entry with
// status='queued' when the target workspace was busy and the tick was durably
// buffered instead of fired. Mirrors recordSkipped (#115) but records a buffer,
// not a drop: the drain will dispatch qID serially when the agent frees.
// next_run_at still advances so the liveness view keeps ticking and the NEXT
// cron slot enqueues (the schedule_id idempotency key then holds at most one
// pending tick — the latest — per schedule).
func (s *Scheduler) recordQueued(ctx context.Context, sched scheduleRow, activeTasks int, queueID string, depth int) {
	reason := fmt.Sprintf("queued: workspace busy (active_tasks=%d), buffered (id=%s, depth=%d)", activeTasks, short(queueID, 8), depth)

	nextRun, nextErr := ComputeNextRun(sched.CronExpr, sched.Timezone, time.Now())
	var nextRunPtr *time.Time
	if nextErr == nil {
		nextRunPtr = &nextRun
	} else {
		// Same guard as recordSkipped/fireSchedule — preserve existing
		// next_run_at rather than writing NULL on an unparseable cron expr.
		log.Printf("Scheduler: ComputeNextRun error in recordQueued for '%s' (%s) — preserving existing next_run_at: %v",
			sched.Name, sched.ID, nextErr)
	}

	queuedUpdCtx, queuedUpdCancel := context.WithTimeout(context.Background(), dbQueryTimeout)
	if _, err := db.DB.ExecContext(queuedUpdCtx, `
		UPDATE workspace_schedules
		SET last_run_at = now(),
		    next_run_at = COALESCE($2, next_run_at),
		    run_count = run_count + 1,
		    last_status = 'queued',
		    last_error = $3,
		    updated_at = now()
		WHERE id = $1
	`, sched.ID, nextRunPtr, sanitizeUTF8(reason)); err != nil {
		log.Printf("Scheduler: '%s' queued update failed: %v", sched.Name, err)
	}
	queuedUpdCancel()

	cronMeta, marshalErr := json.Marshal(map[string]interface{}{
		"schedule_id":   sched.ID,
		"schedule_name": sched.Name,
		"cron_expr":     sched.CronExpr,
		"queued":        true,
		"active_tasks":  activeTasks,
		"queue_id":      queueID,
		"queue_depth":   depth,
	})
	if marshalErr != nil {
		log.Printf("Scheduler '%s': json.Marshal cronMeta(queued) failed: %v", sched.Name, marshalErr)
	} else {
		queuedInsCtx, queuedInsCancel := context.WithTimeout(context.Background(), dbQueryTimeout)
		if _, err := db.DB.ExecContext(queuedInsCtx, `
			INSERT INTO activity_logs (workspace_id, activity_type, source_id, method, summary, request_body, status, error_detail, created_at)
			VALUES ($1, 'cron_run', NULL, 'cron', $2, $3::jsonb, 'queued', $4, now())
		`, sched.WorkspaceID, sanitizeUTF8("Cron queued (busy): "+sched.Name), string(cronMeta), sanitizeUTF8(reason)); err != nil {
			log.Printf("Scheduler: '%s' queued activity log failed: %v", sched.Name, err)
		}
		queuedInsCancel()
	}

	if s.broadcaster != nil {
		_ = s.broadcaster.RecordAndBroadcast(ctx, string(events.EventCronSkipped), sched.WorkspaceID, map[string]interface{}{
			"schedule_id":   sched.ID,
			"schedule_name": sched.Name,
			"reason":        reason,
			"queued":        true,
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
		// #2865: surface as molecule_phantom_busy_resets_total. High
		// reset rate signals task-lifecycle accounting regressions
		// (e.g. missing env vars causing claude --print timeouts that
		// leave active_tasks elevated until this sweep fires).
		metrics.TrackPhantomBusyReset()
		count++
	}
	if err := rows.Err(); err != nil {
		log.Printf("Scheduler: phantom-busy sweep rows error: %v", err)
	}
	if count > 0 {
		log.Printf("Scheduler: phantom-busy sweep complete — reset %d workspace(s)", count)
	}
}

// detectResultKind inspects an A2A response body for SDK-layer error signals
// that are invisible at the HTTP level. The claude-code-sdk adapter returns
// HTTP 200 even when the inner LLM call throws (Max-plan rate-limit, quota
// exhaustion, SDK internal errors) — the error surfaces only in the response
// body under result.kind or result.result_kind.
//
// Returns an empty string when the response is clean (result_kind is "ok",
// "message" — the A2A-SDK canonical successful Message envelope — or absent).
// Returns the result_kind value when it is a non-ok signal, so callers can
// propagate it as the schedule's last_status.
//
// Known successful (= treat-as-ok) kinds (resultOKKinds):
//   - "ok" — explicit success signal
//   - "message" — A2A-SDK Message envelope (`{"result":{"kind":"message","parts":[...]}}`),
//     emitted by every successful agent reply. Fix: #1696 originally allow-listed only
//     "ok" / empty, which mis-flagged every successful agent response as an SDK error
//     (PM scheduler observed 21 consecutive false-failure ticks before auto-disable;
//     screenshot 2026-05-23). See [#1696 follow-up].
//
// Known non-ok kinds:
//   - "rate_limited" — LLM API rate-limit hit (Max-plan, etc.)
//   - "quota_exhausted" — quota / budget exhausted
//   - "sdk_error" — SDK threw an internal error
//
// See #1696.
//
// resultOKKinds is the allowlist of `result.kind` values that are
// UNCONDITIONALLY successful (no further parsing needed). Anything
// outside this set is treated as a non-ok SDK signal, EXCEPT `task`
// which is gated separately on `result.status.state` (see
// classifyTaskState — A2A Task can be either in-progress or terminally
// failed, depending on its status).
//
// Add to this list when new always-success envelope kinds are introduced
// upstream. NEVER add an envelope that can carry a failure sub-state.
var resultOKKinds = map[string]struct{}{
	"":        {}, // absent / empty → treat as ok (no signal)
	"ok":      {}, // explicit success
	"message": {}, // A2A-SDK Message envelope (always a successful agent reply)
}

// taskOKStates is the A2A Task `status.state` allowlist for results that
// have `kind: "task"`. Tasks can be in-progress (submitted/working) or
// terminally successful (completed) — those are clean signals to the
// scheduler. Terminal failure states (failed/canceled/rejected) are
// surfaced as the scheduler's last_status so operators can see the real
// state. Cf. CR2 review feedback on #1716.
var taskOKStates = map[string]struct{}{
	"":          {}, // status.state absent → conservative: don't fire false-failure
	"submitted": {}, // task accepted, not yet running
	"working":   {}, // task in progress
	"completed": {}, // task finished successfully
}

// classifyTaskState inspects `result.status.state` (or `result.status_state`
// legacy variant) and returns "" when the state is in taskOKStates (success
// or in-progress) or the state string when it is a terminal failure that
// should propagate as last_status.
func classifyTaskState(result map[string]json.RawMessage) string {
	rawStatus, ok := result["status"]
	if !ok {
		return "" // no status block → no signal, leave clean
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal(rawStatus, &status); err != nil {
		return ""
	}
	if rawState, ok := status["state"]; ok {
		var s string
		if json.Unmarshal(rawState, &s) == nil {
			if _, isOK := taskOKStates[s]; !isOK {
				return s
			}
		}
	}
	return ""
}

func detectResultKind(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return ""
	}
	// Check result.kind first (canonical JSON-RPC shape).
	if rawResult, ok := top["result"]; ok {
		var result map[string]json.RawMessage
		if err := json.Unmarshal(rawResult, &result); err == nil {
			// result.kind (canonical JSON-RPC envelope field).
			if rawKind, ok := result["kind"]; ok {
				var k string
				if json.Unmarshal(rawKind, &k) == nil {
					// Special-case task: success or failure depends on status.state.
					if k == "task" {
						if bad := classifyTaskState(result); bad != "" {
							return bad
						}
						// task with ok / in-progress state → clean
					} else if _, isOK := resultOKKinds[k]; !isOK {
						return k
					}
				}
			}
			// result.result_kind (legacy / alternative field name).
			if rawKind, ok := result["result_kind"]; ok {
				var k string
				if json.Unmarshal(rawKind, &k) == nil {
					if k == "task" {
						if bad := classifyTaskState(result); bad != "" {
							return bad
						}
					} else if _, isOK := resultOKKinds[k]; !isOK {
						return k
					}
				}
			}
		}
	}
	// Top-level error: non-ok HTTP 200 with a structured error in the body.
	if rawErr, ok := top["error"]; ok {
		var errMsg string
		if err := json.Unmarshal(rawErr, &errMsg); err == nil && errMsg != "" {
			// Distinguish SDK errors from other errors. SDK-layer errors from the
			// Claude Code runtime include specific markers.
			lower := strings.ToLower(errMsg)
			// Check more specific patterns first (max-plan quota > general rate).
			if strings.Contains(lower, "max-plan") || strings.Contains(lower, "quota") || strings.Contains(lower, "budget") {
				return "quota_exhausted"
			}
			if strings.Contains(lower, "rate limit") || strings.Contains(lower, "rate_limit") {
				return "rate_limited"
			}
			if strings.Contains(lower, "claude code returned an error") || strings.Contains(lower, "sdk error") ||
				strings.Contains(lower, "api key") || strings.Contains(lower, "authentication") {
				return "sdk_error"
			}
		}
	}
	return ""
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

// a2aErrorFromBody extracts an A2A/JSON-RPC error message from a 2xx
// response body. The adapter SDK may return HTTP 200 with an error
// payload when it throws internally; this prevents the scheduler from
// falsely recording last_status='ok'.
// Issue #1696.
func a2aErrorFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var resp map[string]interface{}
	if json.Unmarshal(body, &resp) != nil {
		return ""
	}
	// JSON-RPC style: {"error":{"code":-32603,"message":"..."}}
	if errObj, ok := resp["error"].(map[string]interface{}); ok {
		if msg, ok := errObj["message"].(string); ok {
			return msg
		}
	}
	// Plain style: {"error":"..."}
	if errStr, ok := resp["error"].(string); ok {
		return errStr
	}
	return ""
}

// truncation moved to internal/textutil.TruncateBytes (#2962 SSOT).
// The original #2026 fix lives in textutil's package docs as canonical
// prior art. Ellipsis was previously "..." (3 ASCII bytes); the SSOT
// uses "…" (3 UTF-8 bytes) — same byte budget, single-glyph display.

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
