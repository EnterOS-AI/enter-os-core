package handlers

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// validWorkspaceID returns true when id is a syntactically valid UUID.
// workspace_id is a `uuid` column; passing a non-UUID (e.g. the canvas
// "global" sentinel sent when no node is selected) makes Postgres raise
// `invalid input syntax for type uuid`, which previously leaked as an
// opaque 500. Reject up front with a clean 400 instead. Mirrors the
// uuid.Parse guard already used in handlers/activity.go.
func validWorkspaceID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// TokenHandler exposes user-facing token management for workspaces.
// Routes: GET/POST/DELETE /workspaces/:id/tokens (behind WorkspaceAuth).
type TokenHandler struct{}

func NewTokenHandler() *TokenHandler {
	return &TokenHandler{}
}

type tokenListItem struct {
	ID        string     `json:"id"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used_at"`
}

// List returns non-revoked tokens for the workspace (prefix + metadata only,
// never the plaintext or hash).
func (h *TokenHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	if !validWorkspaceID(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace id"})
		return
	}

	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	offset := 0
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	rows, err := db.DB.QueryContext(c.Request.Context(), `
		SELECT id, prefix, created_at, last_used_at
		FROM workspace_auth_tokens
		WHERE workspace_id = $1 AND revoked_at IS NULL
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, workspaceID, limit, offset)
	if err != nil {
		log.Printf("tokens: list query failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tokens"})
		return
	}
	defer rows.Close()

	tokens := []tokenListItem{}
	for rows.Next() {
		var t tokenListItem
		if err := rows.Scan(&t.ID, &t.Prefix, &t.CreatedAt, &t.LastUsed); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan token"})
			return
		}
		tokens = append(tokens, t)
	}
	if err := rows.Err(); err != nil {
		log.Printf("ListTokens rows.Err workspace=%s: %v", workspaceID, err)
	}

	c.JSON(http.StatusOK, gin.H{
		"tokens": tokens,
		"count":  len(tokens),
	})
}

// maxTokensPerWorkspace prevents unbounded token creation. 50 is generous —
// most workspaces need 1-3 tokens (primary + rotation spare).
const maxTokensPerWorkspace = 50

// Create mints a new token for the workspace. The plaintext is returned
// exactly once in the response — it cannot be recovered afterwards.
func (h *TokenHandler) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	if !validWorkspaceID(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace id"})
		return
	}

	// Rate limit: max active tokens per workspace
	var count int
	db.DB.QueryRowContext(c.Request.Context(),
		`SELECT COUNT(*) FROM workspace_auth_tokens WHERE workspace_id = $1 AND revoked_at IS NULL`,
		workspaceID).Scan(&count)
	if count >= maxTokensPerWorkspace {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": fmt.Sprintf("maximum %d active tokens per workspace", maxTokensPerWorkspace)})
		return
	}

	token, err := wsauth.IssueAPIToken(c.Request.Context(), db.DB, workspaceID)
	if err != nil {
		log.Printf("tokens: issue failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create token"})
		return
	}

	log.Printf("tokens: issued new token for workspace %s", workspaceID)

	c.JSON(http.StatusCreated, gin.H{
		"auth_token":   token,
		"workspace_id": workspaceID,
		"message":      "Save this token now — it cannot be retrieved again.",
	})
}

// Revoke invalidates a specific token by ID. The token ID is the database
// row ID visible from List, not the plaintext token itself.
func (h *TokenHandler) Revoke(c *gin.Context) {
	workspaceID := c.Param("id")
	tokenID := c.Param("tokenId")
	if !validWorkspaceID(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace id"})
		return
	}

	result, err := db.DB.ExecContext(c.Request.Context(), `
		UPDATE workspace_auth_tokens
		SET revoked_at = now()
		WHERE id = $1 AND workspace_id = $2 AND revoked_at IS NULL
	`, tokenID, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke token"})
		return
	}

	rows, err := result.RowsAffected()
	if err != nil {
		log.Printf("tokens: revoke RowsAffected error token=%s workspace=%s: %v", tokenID, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke token"})
		return
	}
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "token not found or already revoked"})
		return
	}

	log.Printf("tokens: revoked token %s for workspace %s", tokenID, workspaceID)
	c.JSON(http.StatusOK, gin.H{"status": "revoked"})
}
