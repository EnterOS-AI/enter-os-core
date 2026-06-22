package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// Pricing constants — Claude Sonnet default rates (USD per token).
// Callers with different models should override via env vars in a future phase.
const (
	tokenCostPerInputToken  = 0.000003  // $3 / 1M input tokens
	tokenCostPerOutputToken = 0.000015  // $15 / 1M output tokens
)

// MetricsHandler serves GET /workspaces/:id/metrics.
type MetricsHandler struct{}

// NewMetricsHandler returns a MetricsHandler.
func NewMetricsHandler() *MetricsHandler { return &MetricsHandler{} }

// GetMetrics handles GET /workspaces/:id/metrics.
//
// Returns aggregated LLM token usage for the current UTC day.
// Auth: WorkspaceAuth middleware (bearer token bound to :id).
//
// Response:
//
//	{
//	  "input_tokens":        <N>,
//	  "output_tokens":       <N>,
//	  "total_calls":         <N>,
//	  "estimated_cost_usd":  "0.000000",
//	  "period_start":        "2026-04-17T00:00:00Z",
//	  "period_end":          "2026-04-18T00:00:00Z"
//	}
func (h *MetricsHandler) GetMetrics(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Verify workspace exists — 404 before touching usage table.
	var wsExists bool
	if err := db.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = $1)`,
		workspaceID,
	).Scan(&wsExists); err != nil {
		log.Printf("metrics: workspace check failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify workspace"})
		return
	}
	if !wsExists {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	periodStart := todayUTC()
	periodEnd := periodStart.Add(24 * time.Hour)

	var inputTokens, outputTokens int64
	var callCount int64
	var estimatedCost float64

	err := db.DB.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(call_count), 0),
			COALESCE(SUM(estimated_cost_usd), 0)
		FROM workspace_token_usage
		WHERE workspace_id = $1
		  AND period_start = $2
	`, workspaceID, periodStart).Scan(&inputTokens, &outputTokens, &callCount, &estimatedCost)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("metrics: query failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch metrics"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"input_tokens":       inputTokens,
		"output_tokens":      outputTokens,
		"total_calls":        callCount,
		"estimated_cost_usd": fmt.Sprintf("%.6f", estimatedCost),
		"period_start":       periodStart.Format(time.RFC3339),
		"period_end":         periodEnd.Format(time.RFC3339),
	})
}

// todayUTC returns the start of the current UTC day (midnight).
func todayUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// maxTokensPerCall is the per-call sanity cap applied before upsert (#615).
// An adversarial or buggy agent reporting INT64_MAX would otherwise cause a
// NUMERIC(12,6) overflow in Postgres (silent failure, no cross-workspace
// impact, but corrupts the workspace's cost accounting). 10 M tokens/call is
// generous for any real LLM API response; anything above is clamped.
const maxTokensPerCall = int64(10_000_000)

// upsertTokenUsage accumulates input/output token counts for workspaceID's
// current UTC day. Cost is estimated using the default per-token pricing
// constants. Always call in a detached goroutine — never block the A2A path.
func upsertTokenUsage(ctx context.Context, workspaceID string, inputTokens, outputTokens int64) {
	// Clamp to safe range before any arithmetic — prevents NUMERIC overflow
	// from adversarial or buggy agent responses (#615).
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	if inputTokens > maxTokensPerCall {
		inputTokens = maxTokensPerCall
	}
	if outputTokens > maxTokensPerCall {
		outputTokens = maxTokensPerCall
	}

	if inputTokens == 0 && outputTokens == 0 {
		return
	}
	periodStart := todayUTC()
	cost := float64(inputTokens)*tokenCostPerInputToken + float64(outputTokens)*tokenCostPerOutputToken

	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO workspace_token_usage
			(workspace_id, period_start, input_tokens, output_tokens, call_count, estimated_cost_usd, updated_at)
		VALUES ($1, $2, $3, $4, 1, $5, NOW())
		ON CONFLICT (workspace_id, period_start) DO UPDATE SET
			input_tokens       = workspace_token_usage.input_tokens       + EXCLUDED.input_tokens,
			output_tokens      = workspace_token_usage.output_tokens      + EXCLUDED.output_tokens,
			call_count         = workspace_token_usage.call_count         + 1,
			estimated_cost_usd = workspace_token_usage.estimated_cost_usd + EXCLUDED.estimated_cost_usd,
			updated_at         = NOW()
	`, workspaceID, periodStart, inputTokens, outputTokens, cost)
	if err != nil {
		log.Printf("upsertTokenUsage: failed for workspace %s: %v", workspaceID, err)
	}
}
