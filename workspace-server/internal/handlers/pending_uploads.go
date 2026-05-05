// pending_uploads.go — endpoints the workspace polls to fetch and ack
// chat-upload files staged on the platform side for poll-mode delivery.
//
// Companion to chat_files.go Upload's poll-mode branch:
//
//   Canvas POST /workspaces/:id/chat/uploads
//        ↓ (poll-mode workspace)
//   Platform: chat_files.uploadPollMode
//        ↓ writes pending_uploads row + activity_logs(type=chat_upload_receive)
//   Workspace inbox poller picks up activity row
//        ↓
//   Workspace GETs   /workspaces/:id/pending-uploads/:fid/content  ← this file
//        ↓ writes file to /workspace/.molecule/chat-uploads
//   Workspace POSTs  /workspaces/:id/pending-uploads/:fid/ack       ← this file
//        ↓ row marked acked; Phase 3 sweep deletes
//
// Auth: same wsAuth middleware that gates the activity poll endpoint —
// the workspace's per-workspace platform_token. Only the target workspace
// can read OR ack its own pending uploads. The handler enforces that
// :id == file.workspace_id even though the URL param matches; defence in
// depth against a token leak letting one workspace pull another's bytes.

package handlers

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/pendinguploads"
)

// PendingUploadsHandler serves the workspace-side fetch + ack endpoints.
// Holds a Storage so tests can inject an in-memory implementation
// without going through Postgres (sqlmock-based unit tests cover the
// Postgres impl in internal/pendinguploads/storage_test.go).
type PendingUploadsHandler struct {
	storage pendinguploads.Storage
}

// NewPendingUploadsHandler constructs the handler with a concrete
// Storage. Production wires up pendinguploads.NewPostgres(db.DB).
func NewPendingUploadsHandler(storage pendinguploads.Storage) *PendingUploadsHandler {
	return &PendingUploadsHandler{storage: storage}
}

// GetContent handles GET /workspaces/:id/pending-uploads/:file_id/content.
//
// Returns the file bytes with the original mimetype and a
// Content-Disposition that names the original (sanitized) filename so
// the workspace's fetcher writes it under the expected name. Stamps
// fetched_at on the row best-effort — the read response is already
// flushed to the network before the MarkFetched call so a sweep race
// can't break the workspace's fetch.
//
// 404 on:
//   - file_id not found
//   - file_id belongs to a different workspace (cross-workspace bleed
//     protection)
//   - row already acked (workspace's bug — should not re-fetch after ack)
//   - row past expires_at (Phase 3 sweep would delete shortly anyway)
func (h *PendingUploadsHandler) GetContent(c *gin.Context) {
	workspaceID := c.Param("id")
	if err := validateWorkspaceID(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}
	fileIDStr := c.Param("file_id")
	fileID, err := uuid.Parse(fileIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file_id"})
		return
	}

	rec, err := h.storage.Get(c.Request.Context(), fileID)
	if errors.Is(err, pendinguploads.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "pending upload not found, expired, or already acked"})
		return
	}
	if err != nil {
		log.Printf("pending_uploads GetContent: storage.Get(%s) failed: %v", fileID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "storage error"})
		return
	}

	// Cross-workspace bleed protection: a token leak from workspace A
	// must not let it read workspace B's pending uploads even with the
	// correct file_id. wsAuth already pinned the caller to :id; reject
	// if the row's workspace_id doesn't match.
	if rec.WorkspaceID.String() != workspaceID {
		log.Printf("pending_uploads GetContent: workspace mismatch — caller=%s row=%s file_id=%s",
			workspaceID, rec.WorkspaceID, fileID)
		c.JSON(http.StatusNotFound, gin.H{"error": "pending upload not found"})
		return
	}

	// Stream the bytes. Set the original mimetype if known; fall back
	// to application/octet-stream so curl / browser clients still get
	// a valid response. Content-Disposition uses the workspace-side
	// filename so the fetcher writes it under the expected name.
	mimetype := rec.Mimetype
	if mimetype == "" {
		mimetype = "application/octet-stream"
	}
	c.Header("Content-Type", mimetype)
	c.Header("Content-Disposition", contentDispositionAttachment(rec.Filename))
	c.Header("Content-Length", strconv.FormatInt(rec.SizeBytes, 10))
	c.Status(http.StatusOK)
	if _, err := c.Writer.Write(rec.Content); err != nil {
		// Connection closed mid-stream — log and bail; we cannot
		// re-emit headers at this point. The workspace's HTTP client
		// will see the truncated body and retry on next poll.
		log.Printf("pending_uploads GetContent: write failed for %s: %v", fileID, err)
		return
	}

	// Best-effort fetched_at stamp. After-the-fact so the GET response
	// completes regardless of the UPDATE outcome — a Phase 3 sweep
	// race that nukes the row between Get and MarkFetched must not
	// break the workspace's fetch.
	if err := h.storage.MarkFetched(c.Request.Context(), fileID); err != nil {
		log.Printf("pending_uploads GetContent: mark_fetched(%s) failed: %v", fileID, err)
	}
}

// Ack handles POST /workspaces/:id/pending-uploads/:file_id/ack.
//
// Marks the row as handed-off; Phase 3 sweep deletes acked rows after
// a retention window. Idempotent — workspace at-least-once retries on
// a flaky network return success without moving the timestamp.
func (h *PendingUploadsHandler) Ack(c *gin.Context) {
	workspaceID := c.Param("id")
	if err := validateWorkspaceID(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}
	fileIDStr := c.Param("file_id")
	fileID, err := uuid.Parse(fileIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file_id"})
		return
	}

	// Cross-workspace bleed protection: do a lookup BEFORE Ack so
	// a token leak can't ack a row owned by a different workspace.
	// We don't expose this distinction in the response (404 either
	// way) — the workspace can't tell whether it ack'd a non-existent
	// row vs. one it didn't own, and that's fine for the contract.
	rec, err := h.storage.Get(c.Request.Context(), fileID)
	if errors.Is(err, pendinguploads.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "pending upload not found, expired, or already acked"})
		return
	}
	if err != nil {
		log.Printf("pending_uploads Ack: storage.Get(%s) failed: %v", fileID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "storage error"})
		return
	}
	if rec.WorkspaceID.String() != workspaceID {
		log.Printf("pending_uploads Ack: workspace mismatch — caller=%s row=%s file_id=%s",
			workspaceID, rec.WorkspaceID, fileID)
		c.JSON(http.StatusNotFound, gin.H{"error": "pending upload not found"})
		return
	}

	if err := h.storage.Ack(c.Request.Context(), fileID); err != nil {
		if errors.Is(err, pendinguploads.ErrNotFound) {
			// Race window: the row passed Get but failed Ack — sweep
			// raced with us between the two queries. Treat as success
			// (the workspace's intent was honored, the row is gone).
			c.JSON(http.StatusOK, gin.H{"acked": true, "raced": true})
			return
		}
		log.Printf("pending_uploads Ack: storage.Ack(%s) failed: %v", fileID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "storage error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"acked": true})
}

