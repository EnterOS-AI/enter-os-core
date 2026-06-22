package handlers

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// AdminWorkspaceTokenHandler lets tenant admins mint the first workspace
// bearer for managed SaaS workspaces whose runtime receives its token later
// through registry registration.
type AdminWorkspaceTokenHandler struct{}

func NewAdminWorkspaceTokenHandler() *AdminWorkspaceTokenHandler {
	return &AdminWorkspaceTokenHandler{}
}

// Create handles POST /admin/workspaces/:id/tokens. The route must be mounted
// behind AdminAuth; the plaintext token is returned exactly once.
func (h *AdminWorkspaceTokenHandler) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	if !validWorkspaceID(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace id"})
		return
	}

	var existing string
	err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT id FROM workspaces WHERE id = $1 AND status <> 'removed'`,
		workspaceID).Scan(&existing)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
		log.Printf("admin workspace tokens: workspace lookup failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "workspace lookup failed"})
		return
	}

	var count int
	if err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT COUNT(*) FROM workspace_auth_tokens WHERE workspace_id = $1 AND revoked_at IS NULL`,
		workspaceID).Scan(&count); err != nil {
		log.Printf("admin workspace tokens: count failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count tokens"})
		return
	}
	if count >= maxTokensPerWorkspace {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": fmt.Sprintf("maximum %d active tokens per workspace", maxTokensPerWorkspace)})
		return
	}

	token, err := wsauth.IssueAPIToken(c.Request.Context(), db.DB, workspaceID)
	if err != nil {
		log.Printf("admin workspace tokens: issue failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create token"})
		return
	}

	log.Printf("admin workspace tokens: issued token for workspace %s", workspaceID)
	c.JSON(http.StatusCreated, gin.H{
		"auth_token":   token,
		"workspace_id": workspaceID,
		"message":      "Save this token now — it cannot be retrieved again.",
	})
}
