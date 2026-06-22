package handlers

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/google/uuid"
)

// request_nudge_sweeper.go — RFC unified-requests-inbox Phase 4: idle-agent
// inbox nudge sweeper.
//
// What it does
// ------------
// Periodically scans the unified `requests` table (P1 schema) for inbox items
// addressed to an AGENT recipient that have been sitting unhandled for a while,
// and whose recipient agent is currently IDLE and online. For each such agent
// it enqueues ONE short A2A "nudge" message telling the agent to process its
// inbox via list_inbox / respond_request, then stamps last_nudged_at on the
// items the nudge covered so the same items aren't re-nudged for an hour.
//
// Why
// ---
// An agent that's busy will get to its inbox when it frees up; an agent that's
// idle has nothing prompting it to re-check the inbox, so a request can sit
// pending indefinitely. This worker closes that gap WITHOUT spamming: it only
// fires for items that are already stale (created >10min ago), only for idle
// recipients, and at most once per request per hour.
//
// Out of scope: USER recipients. Stale user-addressed items are surfaced by the
// canvas Tasks/Approvals UI already — this worker never enqueues anything for a
// user recipient.
//
// Delivery mechanism
// ------------------
// The nudge is delivered via the existing a2a-queue path (EnqueueA2A) — the
// SAME mechanism the scheduler uses to deliver a cron tick to an agent. The
// message is a normal `message/send` A2A body; the agent drains it from the
// queue on its next heartbeat (registry.Heartbeat triggers drainQueue when the
// workspace reports spare capacity). We do NOT write raw INSERTs into a2a_queue.
//
// On main vs. P1
// --------------
// This file compiles + tests on main, where the `requests` table does not yet
// exist. It queries `requests` via RAW SQL (never imports P1's RequestStore),
// so the build is independent of P1's Go symbols. At runtime the sweep query
// simply finds no rows until the P1 migration + this PR's migration have both
// rolled out. This PR MUST merge AFTER P1 (#2525).
//
// Frequency
// ---------
// 5min default cadence (REQUEST_NUDGE_SWEEPER_INTERVAL_S to override), matching
// delegation_sweeper. Disable entirely via REQUEST_NUDGE_SWEEPER_INTERVAL_S=0?
// No — envDuration treats <=0 as "use default". To disable, set
// REQUEST_NUDGE_SWEEPER_DISABLED=true (checked in main.go wiring).

const (
	defaultRequestNudgeInterval = 5 * time.Minute

	// staleAfter — an item must have been pending at least this long before
	// it's eligible for a nudge. Gives a freshly-created request a grace
	// window in which a still-active or just-freed agent picks it up on its
	// own, so we don't nudge items that are about to be handled anyway.
	requestNudgeStaleAfter = 10 * time.Minute

	// reNudgeAfter — rate-limit: a given request is nudged at most once per
	// this window. Belt-and-suspenders with the queue idempotency key below.
	requestNudgeReNudgeAfter = time.Hour

	// nudgeBatchLimit — bound the work per sweep. Caps both DB scan cost and
	// the number of agents we poke in one tick; the next tick picks up the
	// remainder. Generous enough that a normal backlog clears in one pass.
	requestNudgeBatchLimit = 200
)

// enqueueFunc is the a2a-queue enqueue signature (package-level EnqueueA2A).
// Injected as a field so tests can assert the nudge enqueue without mocking
// EnqueueA2A's internal SQL (depth count, supersede). Production wiring uses
// the real EnqueueA2A.
type enqueueFunc func(
	ctx context.Context,
	workspaceID, callerID string,
	priority int,
	body []byte,
	method, idempotencyKey string,
	expiresAt *time.Time,
) (id string, depth int, err error)

// RequestNudgeSweeper runs the periodic inbox-nudge sweep. Construct via
// NewRequestNudgeSweeper, then Start(ctx) in main.go to begin ticking.
type RequestNudgeSweeper struct {
	db          *sql.DB
	interval    time.Duration
	staleAfter  time.Duration
	reNudgeWait time.Duration
	limit       int
	enqueue     enqueueFunc
}

// NewRequestNudgeSweeper builds a sweeper bound to the package db.DB
// (production wiring) or a test handle. Reads optional env overrides at
// construction time so a long-running process picks them up via restart,
// not mid-flight (mirrors NewDelegationSweeper).
func NewRequestNudgeSweeper(handle *sql.DB) *RequestNudgeSweeper {
	if handle == nil {
		handle = db.DB
	}
	return &RequestNudgeSweeper{
		db:          handle,
		interval:    envDuration("REQUEST_NUDGE_SWEEPER_INTERVAL_S", defaultRequestNudgeInterval),
		staleAfter:  requestNudgeStaleAfter,
		reNudgeWait: requestNudgeReNudgeAfter,
		limit:       requestNudgeBatchLimit,
		enqueue:     EnqueueA2A,
	}
}

// Interval exposes the configured tick cadence — tests use it; main.go uses
// it implicitly via Start.
func (s *RequestNudgeSweeper) Interval() time.Duration { return s.interval }

// Start ticks Sweep() at the configured interval until ctx is cancelled.
// Defers panic recovery so a single bad row can't kill the sweeper. Mirrors
// DelegationSweeper.Start: first sweep fires immediately on startup.
//
// No-op until both the `requests` table (P1) and this PR's last_nudged_at
// column have rolled out — the sweep query just finds no rows.
func (s *RequestNudgeSweeper) Start(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	log.Printf("RequestNudgeSweeper: started (interval=%s, stale-after=%s, re-nudge-after=%s)",
		s.interval, s.staleAfter, s.reNudgeWait)

	tickWithRecover := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("RequestNudgeSweeper: PANIC in tick — recovered: %v", r)
			}
		}()
		s.Sweep(ctx)
	}

	tickWithRecover()

	for {
		select {
		case <-ctx.Done():
			log.Printf("RequestNudgeSweeper: stopped")
			return
		case <-t.C:
			tickWithRecover()
		}
	}
}

// NudgeResult records what the last sweep did. Returned for observability and
// so tests assert behavior without diffing log lines.
type NudgeResult struct {
	AgentsNudged int // distinct idle agents poked this sweep
	ItemsCovered int // request rows whose last_nudged_at we stamped
	Errors       int
}

// Sweep runs one pass:
//
//  1. SELECT stale agent-recipient items whose recipient workspace is online
//     and idle (active_tasks=0), grouped by recipient. Each group = one agent
//     with N stale inbox items.
//  2. For each idle agent: enqueue ONE A2A nudge, then stamp last_nudged_at on
//     exactly the items that group covered.
//
// SQL strategy: the SELECT joins requests → workspaces and aggregates the
// covered request ids per recipient with array_agg, so one scan yields the
// per-agent work list. The idle gate (status='online' AND
// COALESCE(active_tasks,0)=0) lives in the JOIN's WHERE so an offline/busy
// agent is never returned — we never even build a nudge for it.
func (s *RequestNudgeSweeper) Sweep(ctx context.Context) NudgeResult {
	res := NudgeResult{}

	// Group stale agent-recipient items by recipient. recipient_id::uuid casts
	// the TEXT recipient_id to the workspaces UUID PK for the join (requests
	// stores ids as TEXT — see P1 migration). Items that don't cast (a
	// malformed id) would error the cast; recipient_type='agent' rows are
	// always workspace UUIDs by construction, so this is safe in practice and
	// any bad row surfaces loudly as a query error rather than a silent skip.
	const sweepQuery = `
		SELECT r.recipient_id,
		       array_agg(r.id::text) AS ids
		  FROM requests r
		  JOIN workspaces w ON w.id = r.recipient_id::uuid
		 WHERE r.recipient_type = 'agent'
		   AND r.status IN ('pending', 'info_requested')
		   AND r.created_at < now() - ($1 * INTERVAL '1 second')
		   AND (r.last_nudged_at IS NULL
		        OR r.last_nudged_at < now() - ($2 * INTERVAL '1 second'))
		   AND w.status = 'online'
		   AND COALESCE(w.active_tasks, 0) = 0
		 GROUP BY r.recipient_id
		 LIMIT $3
	`

	rows, err := s.db.QueryContext(ctx, sweepQuery,
		int(s.staleAfter.Seconds()), int(s.reNudgeWait.Seconds()), s.limit)
	if err != nil {
		log.Printf("RequestNudgeSweeper: sweep query failed: %v", err)
		res.Errors++
		return res
	}
	defer rows.Close()

	type group struct {
		recipientID string
		ids         []string
	}
	var todo []group
	for rows.Next() {
		var g group
		// array_agg(text) comes back as a Postgres text[]; pq scans it into a
		// pq.StringArray. We avoid the pq dependency by scanning into a
		// driver-native []string via a small adapter type.
		var ids stringArray
		if err := rows.Scan(&g.recipientID, &ids); err != nil {
			log.Printf("RequestNudgeSweeper: scan failed: %v", err)
			res.Errors++
			continue
		}
		g.ids = ids
		if len(g.ids) == 0 {
			continue
		}
		todo = append(todo, g)
	}
	if err := rows.Err(); err != nil {
		log.Printf("RequestNudgeSweeper: rows.Err: %v", err)
		res.Errors++
	}

	now := time.Now()
	for _, g := range todo {
		if err := s.nudgeAgent(ctx, g.recipientID, g.ids, now); err != nil {
			log.Printf("RequestNudgeSweeper: nudge agent %s (%d items) failed: %v",
				g.recipientID, len(g.ids), err)
			res.Errors++
			continue
		}
		res.AgentsNudged++
		res.ItemsCovered += len(g.ids)
	}

	if res.AgentsNudged > 0 || res.Errors > 0 {
		log.Printf("RequestNudgeSweeper: sweep complete — agents_nudged=%d items_covered=%d errors=%d",
			res.AgentsNudged, res.ItemsCovered, res.Errors)
	}
	return res
}

// nudgeAgent enqueues one A2A nudge for the idle recipient agent covering the
// given stale request ids, then stamps last_nudged_at on those ids. The
// last_nudged_at UPDATE only fires after a successful enqueue so a failed
// enqueue is retried next sweep (the items stay un-stamped, hence eligible).
func (s *RequestNudgeSweeper) nudgeAgent(ctx context.Context, recipientID string, ids []string, now time.Time) error {
	body, err := buildNudgeBody(len(ids))
	if err != nil {
		return fmt.Errorf("build nudge body: %w", err)
	}

	// Idempotency key bucketed to the current hour so two concurrent sweeper
	// boots collapse to one queued nudge per agent per hour at the queue layer
	// too — defense in depth on top of the last_nudged_at rate-limit. Empty
	// callerID = canvas/system-style enqueue (source_id NULL), matching the
	// scheduler's internal fire path.
	idemKey := fmt.Sprintf("inbox-nudge:%s:%d", recipientID, now.Truncate(time.Hour).Unix())

	if _, _, err := s.enqueue(ctx, recipientID, "", PriorityInfo, body, "message/send", idemKey, nil); err != nil {
		return fmt.Errorf("enqueue nudge: %w", err)
	}

	// Stamp last_nudged_at on exactly the items this nudge covered. ANY($1)
	// over the text[] of ids; cast id to text for the comparison so the
	// param type is unambiguous. Re-querying eligibility here would race with
	// the enqueue, so we trust the ids the sweep already gated.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE requests
		   SET last_nudged_at = now()
		 WHERE id::text = ANY($1)
	`, stringArray(ids)); err != nil {
		return fmt.Errorf("stamp last_nudged_at: %w", err)
	}
	return nil
}

// buildNudgeBody constructs the A2A `message/send` JSON-RPC body for the nudge.
// Mirrors the scheduler's body shape (role=user, generated messageId, single
// text part) so the receiving agent processes it like any other inbound turn.
func buildNudgeBody(n int) ([]byte, error) {
	plural := "request"
	if n != 1 {
		plural = "requests"
	}
	text := fmt.Sprintf(
		"You have %d unhandled inbox %s awaiting your response. "+
			"Use list_inbox to see them and respond_request / add_request_message to act.",
		n, plural,
	)
	return json.Marshal(map[string]interface{}{
		"method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":      "user",
				"messageId": "inbox-nudge-" + uuid.New().String(),
				"parts":     []map[string]interface{}{{"kind": "text", "text": text}},
			},
		},
	})
}

// stringArray scans a Postgres text[] into a []string and serializes a
// []string into a Postgres array literal for parameter binding — a minimal
// inline adapter so this file doesn't pull a new driver dependency just for
// array_agg / ANY(text[]). Mirrors lib/pq's StringArray semantics for the
// shapes we use (no NULL elements, no embedded special chars in UUID/text-id
// values).
type stringArray []string

// Scan implements sql.Scanner for a Postgres text[] value (delivered as
// []byte or string like `{a,b,c}`).
func (a *stringArray) Scan(src interface{}) error {
	if src == nil {
		*a = nil
		return nil
	}
	var s string
	switch v := src.(type) {
	case []byte:
		s = string(v)
	case string:
		s = v
	default:
		return fmt.Errorf("stringArray.Scan: unsupported source type %T", src)
	}
	*a = parsePGTextArray(s)
	return nil
}

// Value implements driver.Valuer, emitting a Postgres array literal `{a,b,c}`.
// Used as the ANY($1) param in the last_nudged_at UPDATE. Each element is
// double-quoted and backslash/quote-escaped so ids containing array-special
// characters bind correctly.
func (a stringArray) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	out := make([]byte, 0, len(a)*40+2)
	out = append(out, '{')
	for i, e := range a {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '"')
		for _, c := range []byte(e) {
			if c == '"' || c == '\\' {
				out = append(out, '\\')
			}
			out = append(out, c)
		}
		out = append(out, '"')
	}
	out = append(out, '}')
	return string(out), nil
}

// parsePGTextArray parses a Postgres array literal `{a,"b c",d}` into a
// []string. Handles the unquoted and double-quoted element forms emitted by
// array_agg(text); element values here are UUIDs so the common path is the
// simple comma split, but quoted handling is included for correctness.
func parsePGTextArray(s string) []string {
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil
	}
	body := s[1 : len(s)-1]
	if body == "" {
		return nil
	}
	var out []string
	var cur []byte
	inQuotes := false
	escaped := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case escaped:
			cur = append(cur, c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inQuotes = !inQuotes
		case c == ',' && !inQuotes:
			out = append(out, string(cur))
			cur = cur[:0]
		default:
			cur = append(cur, c)
		}
	}
	out = append(out, string(cur))
	return out
}
