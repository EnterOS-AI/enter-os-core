package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// BudgetHandler exposes per-workspace budget read/write endpoints.
// Routes (all behind WorkspaceAuth middleware):
//
//	GET   /workspaces/:id/budget  — per-period limits, spend, remaining
//	PATCH /workspaces/:id/budget  — set/clear per-period limits
//
// Multi-period (#49): the budget is now four independent rolling windows —
// hourly/daily/weekly/monthly (budget_periods.go is the SSOT for the set). The
// canonical config is workspaces.budget_limits (JSONB, USD cents per period);
// per-period spend is the rolling-window sum over workspace_spend_events. The
// legacy single monthly budget_limit / monthly_spend are still emitted (and
// budget_limit kept in sync to the monthly period) for back-compat with
// pre-deploy canvas/agent builds during the rollout window.
type BudgetHandler struct{}

func NewBudgetHandler() *BudgetHandler { return &BudgetHandler{} }

// periodBudget is the per-period view: configured ceiling (null = no limit),
// rolling-window spend, and remaining headroom (null when no limit; may go
// negative so callers see how far over a period is).
type periodBudget struct {
	Limit     *int64 `json:"limit"`
	Spend     int64  `json:"spend"`
	Remaining *int64 `json:"remaining"`
}

// budgetResponse is the canonical JSON shape for GET and PATCH.
type budgetResponse struct {
	// Periods is keyed by BudgetPeriod ("hourly"/"daily"/"weekly"/"monthly").
	Periods map[string]periodBudget `json:"periods"`

	// --- back-compat (monthly), for pre-multi-period clients ---
	BudgetLimit     *int64 `json:"budget_limit"`
	MonthlySpend    int64  `json:"monthly_spend"`
	BudgetRemaining *int64 `json:"budget_remaining"`
}

// buildBudgetResponse assembles the per-period view from the stored limits +
// the ledger spend. Single place so GET and PATCH return identical shapes.
func buildBudgetResponse(ctx context.Context, workspaceID string, limitsRaw []byte) (budgetResponse, error) {
	limits := parseBudgetLimits(limitsRaw)
	spend, err := spendByPeriod(ctx, db.DB, workspaceID)
	if err != nil {
		return budgetResponse{}, err
	}
	periods := make(map[string]periodBudget, len(budgetPeriods))
	for _, def := range budgetPeriods {
		pb := periodBudget{Spend: spend[def.Name]}
		if lim, ok := limits[def.Name]; ok {
			l := lim
			pb.Limit = &l
			r := lim - spend[def.Name]
			pb.Remaining = &r
		}
		periods[string(def.Name)] = pb
	}
	resp := budgetResponse{Periods: periods, MonthlySpend: spend[PeriodMonthly]}
	if m := periods[string(PeriodMonthly)]; m.Limit != nil {
		resp.BudgetLimit = m.Limit
		resp.BudgetRemaining = m.Remaining
	}
	return resp, nil
}

// GetBudget handles GET /workspaces/:id/budget.
func (h *BudgetHandler) GetBudget(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var limitsRaw []byte
	err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(budget_limits, '{}'::jsonb)
		 FROM workspaces
		 WHERE id = $1 AND status != 'removed'`,
		workspaceID,
	).Scan(&limitsRaw)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		log.Printf("GetBudget: query failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	resp, err := buildBudgetResponse(ctx, workspaceID, limitsRaw)
	if err != nil {
		log.Printf("GetBudget: spend query failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// PatchBudget handles PATCH /workspaces/:id/budget. Accepts EITHER the
// multi-period shape
//
//	{"budget_limits": {"hourly": 100, "daily": null, "weekly": 500, "monthly": 2000}}
//
// (a per-period value of null/absent clears that period; a positive int sets it)
// OR the legacy single-monthly shape {"budget_limit": 2000} / {"budget_limit": null}.
func (h *BudgetHandler) PatchBudget(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var raw map[string]json.RawMessage
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	_, hasLimits := raw["budget_limits"]
	_, hasLegacy := raw["budget_limit"]
	if !hasLimits && !hasLegacy {
		c.JSON(http.StatusBadRequest, gin.H{"error": "budget_limits or budget_limit field is required"})
		return
	}

	limits := make(map[BudgetPeriod]int64, len(budgetPeriods))
	known := make(map[string]bool, len(budgetPeriods))
	for _, def := range budgetPeriods {
		known[string(def.Name)] = true
	}

	if hasLimits {
		var m map[string]*int64
		if err := json.Unmarshal(raw["budget_limits"], &m); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "budget_limits must be an object of period→int|null"})
			return
		}
		for k, v := range m {
			if !known[k] {
				c.JSON(http.StatusBadRequest, gin.H{"error": "unknown budget period: " + k + " (allowed: hourly, daily, weekly, monthly)"})
				return
			}
			if v == nil {
				continue // clear this period (null = no limit)
			}
			if *v < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "budget limit for " + k + " must be >= 0 (USD cents)"})
				return
			}
			limits[BudgetPeriod(k)] = *v // 0 is valid = block-all for this period
		}
	} else { // legacy single-monthly
		var v *int64
		if err := json.Unmarshal(raw["budget_limit"], &v); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "budget_limit must be an integer (USD cents) or null"})
			return
		}
		if v != nil {
			if *v < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "budget_limit must be >= 0 (USD cents)"})
				return
			}
			limits[PeriodMonthly] = *v // 0 is valid = block-all (legacy semantics)
		}
	}

	// Existence check — 404 for non-existent / removed workspaces.
	var exists bool
	if err := db.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = $1 AND status != 'removed')`,
		workspaceID,
	).Scan(&exists); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	// Persist: budget_limits is the SSOT; keep the legacy budget_limit column
	// synced to the monthly period so pre-deploy enforcement paths stay coherent
	// during the rollout window.
	var legacyMonthly interface{}
	if m, ok := limits[PeriodMonthly]; ok {
		legacyMonthly = m
	}
	encoded := encodeBudgetLimits(limits)
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET budget_limits = $2, budget_limit = $3, updated_at = now() WHERE id = $1`,
		workspaceID, encoded, legacyMonthly,
	); err != nil {
		log.Printf("PatchBudget: update failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
		return
	}

	resp, err := buildBudgetResponse(ctx, workspaceID, encoded)
	if err != nil {
		log.Printf("PatchBudget: re-read failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusOK, gin.H{"status": "updated"})
		return
	}
	c.JSON(http.StatusOK, resp)
}
