package handlers

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/gin-gonic/gin"
)

// maxInstructionContentLen caps content size to prevent token-budget DoS via
// oversized instructions being prepended to every agent's system prompt.
const maxInstructionContentLen = 8192

type InstructionsHandler struct{}

func NewInstructionsHandler() *InstructionsHandler {
	return &InstructionsHandler{}
}

type Instruction struct {
	ID          string    `json:"id"`
	Scope       string    `json:"scope"`
	ScopeTarget *string   `json:"scope_target"`
	Title       string    `json:"title"`
	Content     string    `json:"content"`
	Priority    int       `json:"priority"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// List returns instructions filtered by scope. Agents call this at startup
// to fetch their full instruction set (global + workspace).
//
// GET /instructions?scope=global
// GET /instructions?workspace_id=<uuid>  (returns global + workspace)
//
// Team scope is reserved in the schema but not yet wired — teams/team_members
// tables don't exist in any migration. Adding team support requires a new
// migration first.
func (h *InstructionsHandler) List(c *gin.Context) {
	ctx := c.Request.Context()
	scope := c.Query("scope")
	workspaceID := c.Query("workspace_id")

	if workspaceID != "" {
		query := `SELECT id, scope, scope_target, title, content, priority, enabled, created_at, updated_at
			FROM platform_instructions
			WHERE enabled = true AND (
				scope = 'global'
				OR (scope = 'workspace' AND scope_target = $1)
			)
			ORDER BY CASE scope WHEN 'global' THEN 0 WHEN 'workspace' THEN 2 END,
			         priority DESC`
		r, qErr := db.DB.QueryContext(ctx, query, workspaceID)
		if qErr != nil {
			log.Printf("Instructions list error: %v", qErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		defer r.Close()
		c.JSON(http.StatusOK, scanInstructions(r))
		return
	}

	// Admin listing by scope
	query := `SELECT id, scope, scope_target, title, content, priority, enabled, created_at, updated_at
		FROM platform_instructions WHERE 1=1`
	args := []interface{}{}
	if scope != "" {
		query += ` AND scope = $1`
		args = append(args, scope)
	}
	query += ` ORDER BY scope, priority DESC, created_at`

	r, qErr := db.DB.QueryContext(ctx, query, args...)
	if qErr != nil {
		log.Printf("Instructions list error: %v", qErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer r.Close()
	c.JSON(http.StatusOK, scanInstructions(r))
}

// Create adds a new platform instruction.
// POST /instructions
func (h *InstructionsHandler) Create(c *gin.Context) {
	var body struct {
		Scope       string  `json:"scope" binding:"required"`
		ScopeTarget *string `json:"scope_target"`
		Title       string  `json:"title" binding:"required"`
		Content     string  `json:"content" binding:"required"`
		Priority    int     `json:"priority"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope, title, and content are required"})
		return
	}
	if body.Scope != "global" && body.Scope != "workspace" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be global or workspace (team scope not yet supported)"})
		return
	}
	if body.Scope == "workspace" && (body.ScopeTarget == nil || *body.ScopeTarget == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope_target required for workspace scope"})
		return
	}
	if len(body.Content) > maxInstructionContentLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content exceeds 8192 chars"})
		return
	}
	if len(body.Title) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "title exceeds 200 chars"})
		return
	}

	var id string
	err := db.DB.QueryRowContext(c.Request.Context(),
		`INSERT INTO platform_instructions (scope, scope_target, title, content, priority)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		body.Scope, body.ScopeTarget, body.Title, body.Content, body.Priority,
	).Scan(&id)
	if err != nil {
		log.Printf("Instructions create error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "insert failed"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

// Update modifies an existing instruction.
// PUT /instructions/:id
func (h *InstructionsHandler) Update(c *gin.Context) {
	id := c.Param("id")
	var body struct {
		Title    *string `json:"title"`
		Content  *string `json:"content"`
		Priority *int    `json:"priority"`
		Enabled  *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if body.Content != nil && len(*body.Content) > maxInstructionContentLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content exceeds 8192 chars"})
		return
	}
	if body.Title != nil && len(*body.Title) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "title exceeds 200 chars"})
		return
	}

	result, err := db.DB.ExecContext(c.Request.Context(),
		`UPDATE platform_instructions SET
			title = COALESCE($2, title),
			content = COALESCE($3, content),
			priority = COALESCE($4, priority),
			enabled = COALESCE($5, enabled),
			updated_at = NOW()
		 WHERE id = $1`,
		id, body.Title, body.Content, body.Priority, body.Enabled,
	)
	if err != nil {
		log.Printf("Instructions update error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "instruction not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "updated"})
}

// Delete removes an instruction.
// DELETE /instructions/:id
func (h *InstructionsHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	result, err := db.DB.ExecContext(c.Request.Context(),
		`DELETE FROM platform_instructions WHERE id = $1`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete failed"})
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "instruction not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// Resolve returns the merged instruction text for a workspace — all enabled
// instructions across global → workspace scope, concatenated in order.
// This is what the Python runtime calls to get the full instruction set.
//
// GET /workspaces/:id/instructions/resolve
//
// Mounted under wsAuth so the caller must hold a valid bearer token for
// :id, preventing cross-workspace enumeration of operator policy.
func (h *InstructionsHandler) Resolve(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id required"})
		return
	}
	ctx := c.Request.Context()

	rows, err := db.DB.QueryContext(ctx,
		`SELECT scope, title, content FROM platform_instructions
		 WHERE enabled = true AND (
			scope = 'global'
			OR (scope = 'workspace' AND scope_target = $1)
		 )
		 ORDER BY CASE scope WHEN 'global' THEN 0 WHEN 'workspace' THEN 2 END,
		          priority DESC`,
		workspaceID)
	if err != nil {
		log.Printf("Instructions resolve error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	var b strings.Builder
	currentScope := ""
	for rows.Next() {
		var scope, title, content string
		if err := rows.Scan(&scope, &title, &content); err != nil {
			continue
		}
		if scope != currentScope {
			scopeLabel := "Platform-Wide Rules"
			if scope == "workspace" {
				scopeLabel = "Role-Specific Rules"
			}
			b.WriteString("\n## ")
			b.WriteString(scopeLabel)
			b.WriteString("\n\n")
			currentScope = scope
		}
		b.WriteString("### ")
		b.WriteString(title)
		b.WriteString("\n")
		b.WriteString(content)
		b.WriteString("\n\n")
	}

	c.JSON(http.StatusOK, gin.H{
		"workspace_id": workspaceID,
		"instructions": b.String(),
	})
}

func scanInstructions(rows interface {
	Next() bool
	Scan(dest ...interface{}) error
}) []Instruction {
	var instructions []Instruction
	for rows.Next() {
		var inst Instruction
		if err := rows.Scan(&inst.ID, &inst.Scope, &inst.ScopeTarget, &inst.Title,
			&inst.Content, &inst.Priority, &inst.Enabled, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
			log.Printf("Instructions scan error: %v", err)
			continue
		}
		instructions = append(instructions, inst)
	}
	if instructions == nil {
		instructions = []Instruction{}
	}
	return instructions
}
