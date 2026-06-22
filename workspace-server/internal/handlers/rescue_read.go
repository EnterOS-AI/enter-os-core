package handlers

// rescue_read.go — GET /workspaces/:id/rescue (RFC internal#742 Part 3).
//
// Serves the LATEST post-mortem rescue bundle captured for a
// boot-failed/terminated workspace, so "why won't my agent boot" is
// answerable WITHOUT a live instance. Powers the future canvas
// "Why did this fail?" panel.
//
// Read-path: the bundle is read from the queryable rescue_bundles table
// (internal/rescuestore), NOT from obs/Loki. Part 2 ships the bundle via
// internal/audit (Loki-only); reading from Loki would require obs read
// creds the tenant deliberately lacks. Part 3 persists the
// already-redacted bundle on capture and serves it here — see the
// migration header for the full rationale.
//
// Auth/scoping: registered on the WorkspaceAuth-guarded /workspaces/:id
// group (same gate as /files/* and /exec), so the caller must hold a
// valid per-workspace or org bearer token for :id. TenantGuard already
// 404s cross-org requests at the routing layer; on top of that the store
// read is org-scoped by MOLECULE_ORG_ID, so a row written under a
// different org is never returned (defense in depth).
//
// Redaction: the stored sections were already scrubbed at capture time
// (Part 2's SAFE-T1201 secret-scan). This handler returns them verbatim
// — it never re-ships or re-derives secrets.

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/rescuestore"
	"github.com/gin-gonic/gin"
)

// maxResponseSections bounds how many sections the read response
// returns. The fixed capture set is small (6), so this is a backstop
// against a future capture set growth or a hand-written row — keeps the
// JSON response bounded regardless of what's stored. Per-section content
// is already clamped at persist time (rescuestore.maxSectionBytes).
const maxResponseSections = 64

// RescueReadHandler serves GET /workspaces/:id/rescue. The store is
// injected so tests fake it; production wires a Postgres store over
// db.DB (see NewRescueReadHandler).
type RescueReadHandler struct {
	store rescuestore.Store
}

// NewRescueReadHandler builds the handler over the package db.DB. db.DB
// is nil in some unit-test binaries; the handler tolerates that by
// returning 503 rather than nil-deref (the store guards nil db).
func NewRescueReadHandler() *RescueReadHandler {
	return &RescueReadHandler{store: rescuestore.NewPostgres(db.DB)}
}

// WithStore overrides the store (test seam). Returns the handler for
// chaining.
func (h *RescueReadHandler) WithStore(s rescuestore.Store) *RescueReadHandler {
	h.store = s
	return h
}

// rescueSection is one labelled chunk in the read response.
type rescueSection struct {
	Name     string `json:"name"`
	Content  string `json:"content"`
	Redacted bool   `json:"redacted"`
}

// rescueReadResponse is the JSON shape returned for a found bundle.
// `sections` is an ordered array (capture reading order), not a map, so
// the order config→logs→state→env is preserved for the canvas panel.
type rescueReadResponse struct {
	WorkspaceID string          `json:"workspace_id"`
	CapturedAt  time.Time       `json:"captured_at"`
	Reason      string          `json:"reason"`
	InstanceID  string          `json:"instance_id"`
	Sections    []rescueSection `json:"sections"`
	// Truncated is true when the stored bundle had more sections than
	// maxResponseSections and the response was capped.
	Truncated bool `json:"truncated,omitempty"`
}

// GetRescue handles GET /workspaces/:id/rescue.
//
//	200 — latest rescue bundle for the workspace (org-scoped).
//	404 — no rescue bundle on file for this workspace (or wrong org).
//	503 — store/datastore unavailable.
func (h *RescueReadHandler) GetRescue(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	if h.store == nil {
		log.Printf("GetRescue: store not configured for ws=%s", workspaceID)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "rescue store unavailable",
			"code":  "platform_unavailable",
		})
		return
	}

	// org_id is the tenant's configured org (one tenant = one org).
	// Fail closed: an empty org_id disables org isolation and must not
	// reach the store (#2020).
	orgID := os.Getenv("MOLECULE_ORG_ID")
	if orgID == "" {
		log.Printf("GetRescue: missing MOLECULE_ORG_ID for ws=%s", workspaceID)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "rescue org not configured",
			"code":  "platform_misconfigured",
		})
		return
	}

	stored, err := h.store.GetLatest(ctx, workspaceID, orgID)
	if err != nil {
		// Per the Store contract a missing bundle is (nil, nil), NOT an
		// error — so any error here is a genuine datastore fault → 503,
		// never a masquerading 404 that would hide an outage.
		log.Printf("GetRescue: store query failed for ws=%s: %v", workspaceID, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "rescue store query failed",
			"code":  "platform_unavailable",
		})
		return
	}
	if stored == nil {
		// No bundle captured (workspace never boot-failed, or its grace
		// window lapsed). 404 — existence-non-inferring; a workspace in a
		// sibling org reaches the same 404 via the org filter.
		c.JSON(http.StatusNotFound, gin.H{"error": "no rescue bundle for this workspace"})
		return
	}

	resp := buildRescueResponse(workspaceID, stored)
	c.JSON(http.StatusOK, resp)
}

// buildRescueResponse maps a stored bundle to the read response, bounding
// the section count. Split out so the mapping/limit is unit-testable.
func buildRescueResponse(workspaceID string, stored *rescuestore.StoredBundle) rescueReadResponse {
	secs := stored.Bundle.Sections
	truncated := false
	if len(secs) > maxResponseSections {
		secs = secs[:maxResponseSections]
		truncated = true
	}
	out := make([]rescueSection, 0, len(secs))
	for _, s := range secs {
		// rescue.Section and rescueSection are field-identical; the
		// explicit conversion keeps the handler's JSON shape independent
		// of the leaf package's struct (which could gain non-response
		// fields later).
		out = append(out, rescueSection(s))
	}
	return rescueReadResponse{
		WorkspaceID: workspaceID,
		CapturedAt:  stored.CapturedAt,
		Reason:      stored.Bundle.Reason,
		InstanceID:  stored.Bundle.InstanceID,
		Sections:    out,
		Truncated:   truncated,
	}
}
