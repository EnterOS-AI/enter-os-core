package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"
)

// budget_periods.go — SINGLE SOURCE OF TRUTH for the multi-period per-workspace
// LLM budget (#49 follow-up). The supported periods, their rolling windows, the
// per-period spend computation (from the workspace_spend_events ledger), and the
// over-budget decision all live here so the config endpoint (GetBudget/PatchBudget),
// the display, and enforcement (checkWorkspaceBudget) can never drift.
//
// Spend model: the heartbeat records each observed spend INCREMENT into
// workspace_spend_events (recordSpendDelta). Per-period spend is a rolling-window
// SUM over that ledger — so the SERVER owns windowing (the agent keeps reporting
// its cumulative figure unchanged). Rolling (not calendar) windows: no fragile
// month-boundary reset, and "monthly" = a 30-day trailing window.

// BudgetPeriod is one of the supported rolling budget windows.
type BudgetPeriod string

const (
	PeriodHourly  BudgetPeriod = "hourly"
	PeriodDaily   BudgetPeriod = "daily"
	PeriodWeekly  BudgetPeriod = "weekly"
	PeriodMonthly BudgetPeriod = "monthly"
)

// budgetPeriodDef pairs a period with its rolling window.
type budgetPeriodDef struct {
	Name   BudgetPeriod
	Window time.Duration
}

// budgetPeriods is the canonical ordered list. ADD A PERIOD = one line here;
// every consumer iterates this slice, so nothing else needs to change.
var budgetPeriods = []budgetPeriodDef{
	{PeriodHourly, time.Hour},
	{PeriodDaily, 24 * time.Hour},
	{PeriodWeekly, 7 * 24 * time.Hour},
	{PeriodMonthly, 30 * 24 * time.Hour}, // rolling 30-day window
}

// spendLedgerRetention bounds the ledger: rows older than the largest window
// (+ slack) are never read, so the recorder opportunistically prunes them.
var spendLedgerRetention = 35 * 24 * time.Hour

// parseBudgetLimits decodes the workspaces.budget_limits JSONB into a map of
// period → limit (USD cents). A limit of ZERO is valid and means "block all
// spend for that period" (a $0 ceiling); absent / null / negative / unknown
// keys mean "no limit for that period". Tolerant of a NULL/empty column.
func parseBudgetLimits(raw []byte) map[BudgetPeriod]int64 {
	out := make(map[BudgetPeriod]int64, len(budgetPeriods))
	if len(raw) == 0 {
		return out
	}
	var m map[string]*int64
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	for _, def := range budgetPeriods {
		if v, ok := m[string(def.Name)]; ok && v != nil && *v >= 0 {
			out[def.Name] = *v
		}
	}
	return out
}

// encodeBudgetLimits renders a period→limit map back to the canonical JSONB
// shape, keeping only KNOWN periods with a non-negative limit (0 = block-all is
// preserved; a period absent from the map = no limit). Always returns valid JSON.
func encodeBudgetLimits(limits map[BudgetPeriod]int64) []byte {
	m := make(map[string]int64, len(limits))
	for _, def := range budgetPeriods {
		if v, ok := limits[def.Name]; ok && v >= 0 {
			m[string(def.Name)] = v
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// recordSpendDelta appends a positive spend increment to the ledger and
// opportunistically prunes rows past the retention horizon for this workspace.
// No-op for delta <= 0. Errors are returned for the caller to log (non-fatal).
func recordSpendDelta(ctx context.Context, q *sql.DB, workspaceID string, deltaCents int64) error {
	if deltaCents <= 0 {
		return nil
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO workspace_spend_events (workspace_id, delta_cents) VALUES ($1, $2)`,
		workspaceID, deltaCents,
	); err != nil {
		return err
	}
	// Opportunistic prune (cheap; index-backed). Best-effort — ignore error.
	_, _ = q.ExecContext(ctx,
		`DELETE FROM workspace_spend_events
		  WHERE workspace_id = $1 AND occurred_at < now() - $2::interval`,
		workspaceID, pgInterval(spendLedgerRetention),
	)
	return nil
}

// spendByPeriod returns the rolling-window spend (USD cents) for every period,
// computed in a SINGLE query over the ledger. The outer predicate bounds to the
// largest window; per-period FILTERs sum each sub-window. A period with no ledger
// rows reports 0. This is THE spend computation — used by both display + enforcement.
func spendByPeriod(ctx context.Context, q *sql.DB, workspaceID string) (map[BudgetPeriod]int64, error) {
	out := make(map[BudgetPeriod]int64, len(budgetPeriods))
	for _, def := range budgetPeriods {
		out[def.Name] = 0
	}
	row := q.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(delta_cents) FILTER (WHERE occurred_at > now() - interval '1 hour'), 0),
			COALESCE(SUM(delta_cents) FILTER (WHERE occurred_at > now() - interval '24 hours'), 0),
			COALESCE(SUM(delta_cents) FILTER (WHERE occurred_at > now() - interval '7 days'), 0),
			COALESCE(SUM(delta_cents) FILTER (WHERE occurred_at > now() - interval '30 days'), 0)
		FROM workspace_spend_events
		WHERE workspace_id = $1 AND occurred_at > now() - interval '30 days'
	`, workspaceID)
	var h, d, w, mo int64
	if err := row.Scan(&h, &d, &w, &mo); err != nil {
		return out, err
	}
	out[PeriodHourly], out[PeriodDaily], out[PeriodWeekly], out[PeriodMonthly] = h, d, w, mo
	return out, nil
}

// exceededPeriods is PURE: given the configured limits and observed spend, it
// returns the periods whose spend has reached/exceeded their limit (in
// budgetPeriods order). Only periods WITH a positive limit are considered.
// Used by enforcement to decide whether to block.
func exceededPeriods(limits map[BudgetPeriod]int64, spend map[BudgetPeriod]int64) []BudgetPeriod {
	var over []BudgetPeriod
	for _, def := range budgetPeriods {
		limit, ok := limits[def.Name]
		if !ok {
			continue // no limit configured for this period
		}
		// limit >= 0 is a real ceiling (0 = block-all). spend >= limit → over.
		if spend[def.Name] >= limit {
			over = append(over, def.Name)
		}
	}
	return over
}

// pgInterval renders a Go duration as a Postgres-interval string ("N seconds").
func pgInterval(d time.Duration) string {
	return strconv.FormatInt(int64(d.Seconds()), 10) + " seconds"
}
