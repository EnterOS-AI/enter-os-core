package handlers

// llm_billing_mode_handler.go — workspace-server admin routes that read /
// write the per-workspace billing mode override (internal#691). These are
// the per-tenant routes that CP's new /cp/admin/workspaces/:id/llm-billing-mode
// proxies to; the canvas hits them via the CP route, not directly.
//
// Route shape:
//
//   GET  /admin/workspaces/:id/llm-billing-mode
//     -> 200 BillingModeResolution
//     -> 400 on malformed UUID
//     -> 500 on DB error (response still includes a safe_default the caller
//             can fall through to — the resolver always returns a valid mode
//             even on error, per the default-closed contract)
//
//   PUT  /admin/workspaces/:id/llm-billing-mode
//     body: {"mode": "byok" | "platform_managed" | "disabled" | null}
//     -> 200 BillingModeResolution (post-write)
//     -> 400 on bad UUID / unknown mode / malformed body / missing "mode" key
//     -> 404 when the workspace row doesn't exist
//
// Auth: mounted under wsAdmin (middleware.AdminAuth) — admin_token required.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// GetWorkspaceLLMBillingMode handles GET /admin/workspaces/:id/llm-billing-mode.
//
// Resolution DERIVES the provider from the workspace's stored (runtime, model)
// via the registry — per-workspace only, no org-level billing mode (retired
// 2026-06-12). The returned resolution matches what the provision-time strip
// gate computes (same SSOT resolver), so operators see the real platform-vs-byok
// decision + the derived provider in ProviderSelection.
func GetWorkspaceLLMBillingMode(c *gin.Context) {
	workspaceID := strings.TrimSpace(c.Param("id"))
	if !uuidRegex.MatchString(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace id"})
		return
	}
	res, err := ResolveLLMBillingMode(c.Request.Context(), workspaceID)
	if err != nil {
		// Resolver returns a safe default-closed mode alongside the error;
		// surface the error so the operator sees the DB issue, but the
		// response still has a usable mode field for the caller to fall
		// through to without a separate fail-closed branch.
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":        "resolve workspace billing mode failed",
			"detail":       err.Error(),
			"safe_default": res.ResolvedMode,
			"workspace_id": res.WorkspaceID,
		})
		return
	}
	c.JSON(http.StatusOK, res)
}

// PutWorkspaceLLMBillingMode handles PUT /admin/workspaces/:id/llm-billing-mode.
//
// Body shape: {"mode": "byok" | "platform_managed" | "disabled" | null}
// where null clears the override (workspace inherits the org default again).
// Omitting "mode" entirely is a 400 — callers must be explicit about whether
// they want to set or clear, so a typo'd field name can't silently no-op.
//
// On success returns the post-write resolution so the canvas can re-render
// without a follow-up GET.
func PutWorkspaceLLMBillingMode(c *gin.Context) {
	workspaceID := strings.TrimSpace(c.Param("id"))
	if !uuidRegex.MatchString(workspaceID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace id"})
		return
	}

	// Read raw body so we can distinguish three cases:
	//   {"mode": "byok"}     → set override
	//   {"mode": null}       → clear override
	//   {}                   → 400 (caller must be explicit)
	// json.RawMessage zero length ⇔ key absent; raw "null" ⇔ explicit clear;
	// raw quoted string ⇔ set.
	raw, readErr := io.ReadAll(c.Request.Body)
	if readErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body", "detail": readErr.Error()})
		return
	}
	var body struct {
		Mode json.RawMessage `json:"mode"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json", "detail": err.Error()})
		return
	}
	if len(body.Mode) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing required field 'mode' (use null to clear override)"})
		return
	}

	var writeErr error
	if string(body.Mode) == "null" {
		writeErr = SetWorkspaceLLMBillingMode(c.Request.Context(), workspaceID, "")
	} else {
		var modeStr string
		if err := json.Unmarshal(body.Mode, &modeStr); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be a string or null", "detail": err.Error()})
			return
		}
		modeStr = strings.TrimSpace(modeStr)
		if modeStr == "" {
			// Empty string is ambiguous (could be "clear" or "user error");
			// reject as 400 so the caller picks null explicitly.
			c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be one of platform_managed, byok, disabled, or null to clear"})
			return
		}
		writeErr = SetWorkspaceLLMBillingMode(c.Request.Context(), workspaceID, modeStr)
	}

	if errors.Is(writeErr, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if writeErr != nil {
		// Validation errors from SetWorkspaceLLMBillingMode (unknown mode
		// string) come back as a plain error; map to 400.
		if strings.HasPrefix(writeErr.Error(), "unknown billing mode") {
			c.JSON(http.StatusBadRequest, gin.H{"error": writeErr.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "set workspace billing mode failed", "detail": writeErr.Error()})
		return
	}

	// Read back the resolution so the response reflects post-write state.
	res, resolveErr := ResolveLLMBillingMode(c.Request.Context(), workspaceID)
	if resolveErr != nil {
		// Write succeeded but readback failed — still return 200 with the
		// best-effort resolution; the safe default is set even on error.
		c.JSON(http.StatusOK, gin.H{
			"workspace_id":   workspaceID,
			"resolved_mode":  res.ResolvedMode,
			"readback_error": resolveErr.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, res)
}
