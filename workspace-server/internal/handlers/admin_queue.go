package handlers

import (
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// AdminQueueHandler serves POST /admin/a2a-queue/drop-stale — an ops tool for
// post-incident queue cleanup. Marks queued items older than the given TTL as
// 'dropped', preventing PM agents from spending cycles on stale post-incident
// TASK-priority messages.
//
// POST /admin/a2a-queue/drop-stale
//   ?max_age_minutes=N  (default 60)
//   &workspace_id=<id> (optional; empty = all workspaces)
//
// Returns JSON { "dropped": <count> } on success, 500 on error.
type AdminQueueHandler struct{}

func NewAdminQueueHandler() *AdminQueueHandler {
	return &AdminQueueHandler{}
}

func (h *AdminQueueHandler) DropStale(c *gin.Context) {
	maxAgeStr := c.DefaultQuery("max_age_minutes", "60")
	maxAge, err := strconv.Atoi(maxAgeStr)
	if err != nil || maxAge < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max_age_minutes must be a positive integer"})
		return
	}

	workspaceID := c.Query("workspace_id")
	count, err := DropStaleQueueItems(c.Request.Context(), workspaceID, maxAge)
	if err != nil {
		log.Printf("AdminQueueHandler.DropStale: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to drop stale items"})
		return
	}

	log.Printf("AdminQueueHandler.DropStale: dropped %d items (workspace_id=%s, max_age=%dm)",
		count, workspaceID, maxAge)
	c.JSON(http.StatusOK, gin.H{"dropped": count})
}
