package handlers

import (
	"database/sql"
	"log"
	"net/http"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/gin-gonic/gin"
)

// BudgetHandler exposes per-workspace budget read/write endpoints.
// Routes (all behind WorkspaceAuth middleware):
//
//	GET  /workspaces/:id/budget  — current budget_limit, monthly_spend, budget_remaining
//	PATCH /workspaces/:id/budget — set or clear budget_limit
type BudgetHandler struct{}

func NewBudgetHandler() *BudgetHandler { return &BudgetHandler{} }

// budgetResponse is the canonical JSON shape for both GET and PATCH responses.
type budgetResponse struct {
	// BudgetLimit is the monthly spend ceiling in USD cents (null = no limit).
	// budget_limit=500 means $5.00/month.
	BudgetLimit *int64 `json:"budget_limit"`
	// MonthlySpend is the agent's self-reported accumulated LLM API spend
	// for the current month (USD cents). Incremented via heartbeat.
	MonthlySpend int64 `json:"monthly_spend"`
	// BudgetRemaining is null when BudgetLimit is null, otherwise
	// max(0, budget_limit - monthly_spend). Can be negative — we store the
	// actual value so callers can see how far over-budget a workspace is.
	BudgetRemaining *int64 `json:"budget_remaining"`
}

// GetBudget handles GET /workspaces/:id/budget.
// Returns the workspace's current budget ceiling, accumulated spend, and
// computed remaining headroom. Both budget_limit and budget_remaining are
// null when no limit has been configured for the workspace.
func (h *BudgetHandler) GetBudget(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var budgetLimit sql.NullInt64
	var monthlySpend int64
	err := db.DB.QueryRowContext(ctx,
		`SELECT budget_limit, COALESCE(monthly_spend, 0)
		 FROM workspaces
		 WHERE id = $1 AND status != 'removed'`,
		workspaceID,
	).Scan(&budgetLimit, &monthlySpend)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		log.Printf("GetBudget: query failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	resp := budgetResponse{
		MonthlySpend: monthlySpend,
	}
	if budgetLimit.Valid {
		limit := budgetLimit.Int64
		resp.BudgetLimit = &limit
		remaining := limit - monthlySpend
		resp.BudgetRemaining = &remaining
	}

	c.JSON(http.StatusOK, resp)
}

// PatchBudget handles PATCH /workspaces/:id/budget.
// Accepts {"budget_limit": <int64>} to set a new ceiling, or
// {"budget_limit": null} to remove an existing ceiling.
// Returns the updated budget state in the same shape as GetBudget.
func (h *BudgetHandler) PatchBudget(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// We need to distinguish between "field absent" and "field = null",
	// so we unmarshal into a raw map first.
	var raw map[string]interface{}
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	budgetLimitRaw, ok := raw["budget_limit"]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "budget_limit field is required"})
		return
	}

	// Validate and convert the value. JSON numbers decode as float64.
	var budgetArg interface{} // nil → SQL NULL, int64 → new ceiling
	if budgetLimitRaw != nil {
		switch v := budgetLimitRaw.(type) {
		case float64:
			if v < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "budget_limit must be >= 0 (USD cents)"})
				return
			}
			cv := int64(v)
			budgetArg = cv
		case int64:
			if v < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "budget_limit must be >= 0 (USD cents)"})
				return
			}
			budgetArg = v
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "budget_limit must be an integer (USD cents) or null"})
			return
		}
	}
	// budgetArg == nil means "clear the ceiling"

	// Existence check — return 404 for non-existent / removed workspaces.
	var exists bool
	if err := db.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = $1 AND status != 'removed')`,
		workspaceID,
	).Scan(&exists); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET budget_limit = $2, updated_at = now() WHERE id = $1`,
		workspaceID, budgetArg,
	); err != nil {
		log.Printf("PatchBudget: update failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
		return
	}

	// Re-read the current state so the response reflects exactly what is in
	// the DB, including the monthly_spend the agent has already accumulated.
	var newLimit sql.NullInt64
	var monthlySpend int64
	if err := db.DB.QueryRowContext(ctx,
		`SELECT budget_limit, COALESCE(monthly_spend, 0) FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&newLimit, &monthlySpend); err != nil {
		log.Printf("PatchBudget: re-read failed for %s: %v", workspaceID, err)
		// Still success — just omit the echo.
		c.JSON(http.StatusOK, gin.H{"status": "updated"})
		return
	}

	resp := budgetResponse{
		MonthlySpend: monthlySpend,
	}
	if newLimit.Valid {
		limit := newLimit.Int64
		resp.BudgetLimit = &limit
		remaining := limit - monthlySpend
		resp.BudgetRemaining = &remaining
	}

	c.JSON(http.StatusOK, resp)
}
