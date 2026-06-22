package handlers

import (
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

type ConfigHandler struct{}

func NewConfigHandler() *ConfigHandler { return &ConfigHandler{} }

// Get handles GET /workspaces/:id/config
func (h *ConfigHandler) Get(c *gin.Context) {
	workspaceID := c.Param("id")

	var data []byte
	err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT data FROM workspace_config WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&data)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusOK, gin.H{"data": json.RawMessage("{}")})
		return
	}
	if err != nil {
		log.Printf("Config get error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": json.RawMessage(data)})
}

// Patch handles PATCH /workspaces/:id/config
func (h *ConfigHandler) Patch(c *gin.Context) {
	workspaceID := c.Param("id")

	// 256 KiB cap: Postgres jsonb comfortably handles this and real
	// configs are <10 KiB. The cap blocks naive memory-exhaustion DoS
	// — a caller streaming a gigabyte of JSON would OOM the instance.
	const maxConfigBody = 256 << 10
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxConfigBody)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body too large or unreadable"})
		return
	}

	if !json.Valid(body) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
		return
	}

	_, err = db.DB.ExecContext(c.Request.Context(), `
		INSERT INTO workspace_config(workspace_id, data, updated_at)
		VALUES($1, $2::jsonb, NOW())
		ON CONFLICT(workspace_id) DO UPDATE
		SET data = workspace_config.data || $2::jsonb, updated_at = NOW()
	`, workspaceID, string(body))
	if err != nil {
		log.Printf("Config patch error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update config"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "updated"})
}
