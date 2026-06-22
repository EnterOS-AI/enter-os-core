package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/registry"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/scheduler"
)

// ErrorResponse is returned for 4xx/5xx errors. (OpenAPI doc shape — used by swaggo.)
type ErrorResponse struct {
	Error string `json:"error"`
}

// StatusResponse is returned by mutating endpoints that only echo a status verb.
type StatusResponse struct {
	Status string `json:"status"`
}

// CreateScheduleResponse is returned by POST /workspaces/{id}/schedules.
type CreateScheduleResponse struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	NextRunAt time.Time `json:"next_run_at"`
}

// RunNowResponse is returned by POST /workspaces/{id}/schedules/{scheduleId}/run.
type RunNowResponse struct {
	Status      string `json:"status"`
	WorkspaceID string `json:"workspace_id"`
	Prompt      string `json:"prompt"`
}

// HistoryEntry is one row of /workspaces/{id}/schedules/{scheduleId}/history.
type HistoryEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	DurationMs  *int            `json:"duration_ms"`
	Status      *string         `json:"status"`
	ErrorDetail string          `json:"error_detail"`
	Request     json.RawMessage `json:"request" swaggertype:"object"`
}

type ScheduleHandler struct{}

func NewScheduleHandler() *ScheduleHandler {
	return &ScheduleHandler{}
}

type ScheduleResponse struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Name        string     `json:"name"`
	CronExpr    string     `json:"cron_expr"`
	Timezone    string     `json:"timezone"`
	Prompt      string     `json:"prompt"`
	Enabled     bool       `json:"enabled"`
	LastRunAt   *time.Time `json:"last_run_at"`
	NextRunAt   *time.Time `json:"next_run_at"`
	RunCount    int        `json:"run_count"`
	LastStatus  string     `json:"last_status"`
	LastError   string     `json:"last_error"`
	Source      string     `json:"source,omitempty"` // 'template' (seeded by org/import) | 'runtime' (created via Canvas/API). Issue #24.
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// List returns all schedules for a workspace.
//
//	@Summary	List schedules for a workspace
//	@Tags		schedules
//	@Produce	json
//	@Param		id	path		string	true	"Workspace ID"
//	@Success	200	{array}		ScheduleResponse
//	@Failure	500	{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules [get]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, workspace_id, name, cron_expr, timezone, prompt, enabled,
		       last_run_at, next_run_at, run_count, last_status, last_error,
		       source, created_at, updated_at
		FROM workspace_schedules
		WHERE workspace_id = $1
		ORDER BY created_at ASC
	`, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query schedules"})
		return
	}
	defer rows.Close()

	schedules := make([]ScheduleResponse, 0)
	for rows.Next() {
		var s ScheduleResponse
		if err := rows.Scan(
			&s.ID, &s.WorkspaceID, &s.Name, &s.CronExpr, &s.Timezone,
			&s.Prompt, &s.Enabled, &s.LastRunAt, &s.NextRunAt, &s.RunCount,
			&s.LastStatus, &s.LastError, &s.Source, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			log.Printf("Schedules.List: scan error: %v", err)
			continue
		}
		schedules = append(schedules, s)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Schedules.List: rows error: %v", err)
	}

	c.JSON(http.StatusOK, schedules)
}

type CreateScheduleRequest struct {
	Name     string `json:"name"`
	CronExpr string `json:"cron_expr" binding:"required"`
	Timezone string `json:"timezone"`
	Prompt   string `json:"prompt" binding:"required"`
	Enabled  *bool  `json:"enabled"`
}

// Create adds a new schedule for a workspace.
//
//	@Summary	Create a schedule
//	@Tags		schedules
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string					true	"Workspace ID"
//	@Param		body	body		CreateScheduleRequest	true	"Schedule fields"
//	@Success	201		{object}	CreateScheduleResponse
//	@Failure	400		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules [post]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var body CreateScheduleRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cron_expr and prompt are required"})
		return
	}

	// Strip CRLF from prompts — org-template files committed on Windows
	// inject \r\n, causing empty agent responses (issue #958).
	body.Prompt = strings.ReplaceAll(body.Prompt, "\r", "")

	if body.Timezone == "" {
		body.Timezone = "UTC"
	}

	// Validate timezone
	loc, err := time.LoadLocation(body.Timezone)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid timezone: " + body.Timezone})
		return
	}

	// Validate and compute next run
	nextRun, err := scheduler.ComputeNextRun(body.CronExpr, body.Timezone, time.Now().In(loc))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	var id string
	// source='runtime' marks this row as user-created (Canvas/API). The
	// org/import path inserts with source='template' and only refreshes
	// template-source rows on re-import (issue #24), so runtime rows survive.
	err = db.DB.QueryRowContext(ctx, `
		INSERT INTO workspace_schedules (workspace_id, name, cron_expr, timezone, prompt, enabled, next_run_at, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'runtime')
		RETURNING id
	`, workspaceID, body.Name, body.CronExpr, body.Timezone, body.Prompt, enabled, nextRun).Scan(&id)
	if err != nil {
		log.Printf("Schedules.Create: insert error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create schedule"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":          id,
		"status":      "created",
		"next_run_at": nextRun,
	})
}

type UpdateScheduleRequest struct {
	Name     *string `json:"name"`
	CronExpr *string `json:"cron_expr"`
	Timezone *string `json:"timezone"`
	Prompt   *string `json:"prompt"`
	Enabled  *bool   `json:"enabled"`
}

// Update modifies a schedule. Uses a fixed UPDATE with COALESCE so only
// provided fields are changed — no dynamic SQL construction.
//
//	@Summary	Update a schedule
//	@Tags		schedules
//	@Accept		json
//	@Produce	json
//	@Param		id			path		string					true	"Workspace ID"
//	@Param		scheduleId	path		string					true	"Schedule ID"
//	@Param		body		body		UpdateScheduleRequest	true	"Partial schedule fields (only provided keys are updated)"
//	@Success	200			{object}	ScheduleResponse
//	@Failure	400			{object}	ErrorResponse
//	@Failure	404			{object}	ErrorResponse
//	@Failure	500			{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules/{scheduleId} [patch]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) Update(c *gin.Context) {
	scheduleID := c.Param("scheduleId")
	workspaceID := c.Param("id") // #113: bind to owning workspace to prevent IDOR
	ctx := c.Request.Context()

	var body UpdateScheduleRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
		return
	}

	// Strip CRLF from prompt if provided (issue #958).
	if body.Prompt != nil {
		clean := strings.ReplaceAll(*body.Prompt, "\r", "")
		body.Prompt = &clean
	}

	// If cron_expr or timezone changed, revalidate and recompute next_run
	var nextRunAt *time.Time
	if body.CronExpr != nil || body.Timezone != nil {
		var currentCron, currentTZ string
		err := db.DB.QueryRowContext(ctx,
			`SELECT cron_expr, timezone FROM workspace_schedules WHERE id = $1 AND workspace_id = $2`,
			scheduleID, workspaceID,
		).Scan(&currentCron, &currentTZ)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
			return
		}
		cronExpr := currentCron
		if body.CronExpr != nil {
			cronExpr = *body.CronExpr
		}
		tz := currentTZ
		if body.Timezone != nil {
			tz = *body.Timezone
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid timezone: " + tz})
			return
		}
		nextRun, err := scheduler.ComputeNextRun(cronExpr, tz, time.Now().In(loc))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		nextRunAt = &nextRun
	}

	result, err := db.DB.ExecContext(ctx, `
		UPDATE workspace_schedules SET
			name      = COALESCE($2, name),
			cron_expr = COALESCE($3, cron_expr),
			timezone  = COALESCE($4, timezone),
			prompt    = COALESCE($5, prompt),
			enabled   = COALESCE($6, enabled),
			next_run_at = COALESCE($7, next_run_at),
			updated_at = now()
		WHERE id = $1 AND workspace_id = $8
	`, scheduleID, body.Name, body.CronExpr, body.Timezone, body.Prompt, body.Enabled, nextRunAt, workspaceID)
	if err != nil {
		log.Printf("Schedules.Update: error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update schedule"})
		return
	}
	n, err := result.RowsAffected()
	if err != nil {
		log.Printf("Schedules.Update: RowsAffected error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update schedule"})
		return
	}
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "updated"})
}

// Delete removes a schedule.
//
//	@Summary	Delete a schedule
//	@Tags		schedules
//	@Produce	json
//	@Param		id			path		string	true	"Workspace ID"
//	@Param		scheduleId	path		string	true	"Schedule ID"
//	@Success	200			{object}	StatusResponse
//	@Failure	404			{object}	ErrorResponse
//	@Failure	500			{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules/{scheduleId} [delete]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) Delete(c *gin.Context) {
	scheduleID := c.Param("scheduleId")
	workspaceID := c.Param("id") // #113: bind to owning workspace to prevent IDOR
	ctx := c.Request.Context()

	result, err := db.DB.ExecContext(ctx,
		`DELETE FROM workspace_schedules WHERE id = $1 AND workspace_id = $2`,
		scheduleID, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete schedule"})
		return
	}
	n, err := result.RowsAffected()
	if err != nil {
		log.Printf("Schedules.Delete: RowsAffected error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete schedule"})
		return
	}
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// RunNow manually fires a schedule immediately.
//
//	@Summary	Fire a schedule manually
//	@Tags		schedules
//	@Produce	json
//	@Param		id			path		string	true	"Workspace ID"
//	@Param		scheduleId	path		string	true	"Schedule ID"
//	@Success	200			{object}	RunNowResponse
//	@Failure	404			{object}	ErrorResponse
//	@Failure	500			{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules/{scheduleId}/run [post]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) RunNow(c *gin.Context) {
	scheduleID := c.Param("scheduleId")
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var prompt string
	err := db.DB.QueryRowContext(ctx,
		`SELECT prompt FROM workspace_schedules WHERE id = $1 AND workspace_id = $2`,
		scheduleID, workspaceID,
	).Scan(&prompt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read schedule"})
		return
	}

	// The actual A2A fire is done by the caller via the proxy — we just
	// return the prompt so the frontend can POST it to /workspaces/:id/a2a.
	// This keeps the handler stateless and avoids circular deps on WorkspaceHandler.
	c.JSON(http.StatusOK, gin.H{
		"status":       "fired",
		"workspace_id": workspaceID,
		"prompt":       prompt,
	})
}

// History returns recent runs for a schedule from activity_logs.
//
//	@Summary	Get past runs of a schedule
//	@Tags		schedules
//	@Produce	json
//	@Param		id			path		string	true	"Workspace ID"
//	@Param		scheduleId	path		string	true	"Schedule ID"
//	@Success	200			{array}		HistoryEntry
//	@Failure	500			{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules/{scheduleId}/history [get]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) History(c *gin.Context) {
	scheduleID := c.Param("scheduleId")
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// #152: include error_detail in history so UI can show why a run failed.
	// activity_logs.error_detail is populated by scheduler.fireSchedule when
	// the A2A proxy returns non-2xx or the update SQL reports an error.
	rows, err := db.DB.QueryContext(ctx, `
		SELECT created_at, duration_ms, status,
		       COALESCE(error_detail, '') as error_detail,
		       COALESCE(request_body::text, '{}') as request_body
		FROM activity_logs
		WHERE workspace_id = $1
		  AND activity_type = 'cron_run'
		  AND request_body->>'schedule_id' = $2
		ORDER BY created_at DESC
		LIMIT 20
	`, workspaceID, scheduleID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query history"})
		return
	}
	defer rows.Close()

	entries := make([]HistoryEntry, 0)
	for rows.Next() {
		var e HistoryEntry
		var reqStr string
		if err := rows.Scan(&e.Timestamp, &e.DurationMs, &e.Status, &e.ErrorDetail, &reqStr); err != nil {
			continue
		}
		e.Request = json.RawMessage(reqStr)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("ScheduleHistory: rows error: %v", err)
	}

	c.JSON(http.StatusOK, entries)
}

// ScheduleHealthResponse is the read-only health view of a schedule.
// It deliberately omits prompt and cron_expr so sensitive task content is
// never exposed to peer workspaces — only execution-state fields needed to
// detect silent cron failures are returned (issue #249).
type ScheduleHealthResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Enabled    bool       `json:"enabled"`
	LastRunAt  *time.Time `json:"last_run_at"`
	NextRunAt  *time.Time `json:"next_run_at"`
	RunCount   int        `json:"run_count"`
	LastStatus string     `json:"last_status"`
	LastError  string     `json:"last_error"`
}

// Health returns schedule health fields (last_run_at, last_status, run_count,
// etc.) for all schedules belonging to a workspace.
//
// Unlike GET /workspaces/:id/schedules (which requires the workspace's own
// bearer token), this endpoint is accessible to CanCommunicate peers — i.e.,
// any workspace in the same org hierarchy — so peer agents can detect silent
// cron failures without needing admin auth (issue #249).
//
// Auth rules (mirrors the A2A proxy pattern):
//   - X-Workspace-ID header is required to identify the caller.
//   - If the caller workspace has any live tokens, the Authorization: Bearer
//     header must carry that caller's own valid token (lazy-bootstrap: legacy
//     workspaces with no tokens are grandfathered through).
//   - registry.CanCommunicate(callerID, workspaceID) must return true.
//   - System callers (webhook:*, system:*, test:*) bypass token + access checks.
//   - Self-calls (callerID == workspaceID) are always allowed.
//
// Prompt and cron_expr are intentionally absent from the response.
func (h *ScheduleHandler) Health(c *gin.Context) {
	workspaceID := c.Param("id")
	callerID := c.GetHeader("X-Workspace-ID")
	ctx := c.Request.Context()

	// Caller identity is mandatory — anonymous reads are not permitted.
	if callerID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "X-Workspace-ID header required"})
		return
	}

	// Validate the caller's own bearer token (Phase 30.5 contract).
	// Skip for system callers and self-calls, same as the A2A proxy.
	// Post-RFC#637: canvas users may read schedule health too.
	isCanvasUser := false
	if !isSystemCaller(callerID) && callerID != workspaceID {
		var err error
		isCanvasUser, err = validateCallerToken(ctx, c, callerID)
		if err != nil {
			return // response already written with 401
		}
	}

	// CanCommunicate gate — only peers in the org hierarchy may read health.
	// Canvas users (human operators) bypass this gate.
	if callerID != workspaceID && !isSystemCaller(callerID) && !isCanvasUser {
		if !registry.CanCommunicate(callerID, workspaceID) {
			log.Printf("ScheduleHealth: access denied %s → %s", callerID, workspaceID)
			c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
			return
		}
	}

	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, name, enabled, last_run_at, next_run_at, run_count, last_status, last_error
		FROM workspace_schedules
		WHERE workspace_id = $1
		ORDER BY created_at ASC
	`, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query schedules"})
		return
	}
	defer rows.Close()

	schedules := make([]ScheduleHealthResponse, 0)
	for rows.Next() {
		var s ScheduleHealthResponse
		if err := rows.Scan(
			&s.ID, &s.Name, &s.Enabled, &s.LastRunAt, &s.NextRunAt,
			&s.RunCount, &s.LastStatus, &s.LastError,
		); err != nil {
			log.Printf("ScheduleHealth: scan error: %v", err)
			continue
		}
		schedules = append(schedules, s)
	}
	if err := rows.Err(); err != nil {
		log.Printf("ScheduleHealth: rows error: %v", err)
	}

	c.JSON(http.StatusOK, schedules)
}

