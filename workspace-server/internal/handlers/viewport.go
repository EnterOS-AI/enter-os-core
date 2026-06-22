package handlers

import (
	"log"
	"net/http"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

type ViewportHandler struct{}

func NewViewportHandler() *ViewportHandler {
	return &ViewportHandler{}
}

// Get handles GET /canvas/viewport
func (h *ViewportHandler) Get(c *gin.Context) {
	ctx := c.Request.Context()

	var x, y, zoom float64
	err := db.DB.QueryRowContext(ctx,
		`SELECT x, y, zoom FROM canvas_viewport ORDER BY saved_at DESC LIMIT 1`,
	).Scan(&x, &y, &zoom)
	if err != nil {
		// No saved viewport — return defaults
		c.JSON(http.StatusOK, gin.H{"x": 0, "y": 0, "zoom": 1})
		return
	}

	c.JSON(http.StatusOK, gin.H{"x": x, "y": y, "zoom": zoom})
}

// Save handles PUT /canvas/viewport
func (h *ViewportHandler) Save(c *gin.Context) {
	var body struct {
		X    float64 `json:"x"`
		Y    float64 `json:"y"`
		Zoom float64 `json:"zoom"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid viewport data"})
		return
	}

	ctx := c.Request.Context()

	// Upsert — keep only one viewport record
	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO canvas_viewport (id, x, y, zoom, saved_at)
		VALUES ('00000000-0000-0000-0000-000000000001', $1, $2, $3, now())
		ON CONFLICT (id) DO UPDATE SET x = $1, y = $2, zoom = $3, saved_at = now()
	`, body.X, body.Y, body.Zoom)
	if err != nil {
		log.Printf("Save viewport error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save viewport"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "saved"})
}
