package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
)

const (
	displayControlDefaultTTLSeconds = 300
	displayControlMinTTLSeconds     = 30
	displayControlMaxTTLSeconds     = 3600
)

type workspaceDisplayControlResponse struct {
	Controller   string    `json:"controller"`
	ControlledBy string    `json:"controlled_by,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	SessionURL   string    `json:"session_url,omitempty"`
}

type workspaceDisplayControlNoneResponse struct {
	Controller string `json:"controller"`
}

type acquireDisplayControlRequest struct {
	Controller string `json:"controller"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type releaseDisplayControlRequest struct {
	Force bool `json:"force"`
}

// DisplayControl handles GET /workspaces/:id/display/control.
func (h *WorkspaceHandler) DisplayControl(c *gin.Context) {
	lock, found, err := h.loadActiveDisplayControl(c, c.Param("id"))
	if err != nil {
		log.Printf("DisplayControl: load lock for %s failed: %v", c.Param("id"), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display control"})
		return
	}
	if !found {
		c.JSON(http.StatusOK, workspaceDisplayControlNoneResponse{Controller: "none"})
		return
	}
	c.JSON(http.StatusOK, lock)
}

// AcquireDisplayControl handles POST /workspaces/:id/display/control/acquire.
func (h *WorkspaceHandler) AcquireDisplayControl(c *gin.Context) {
	var req acquireDisplayControlRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid display control request"})
			return
		}
	}
	if req.Controller == "" {
		req.Controller = "user"
	}
	if req.Controller != "user" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "browser callers may only acquire user display control"})
		return
	}
	if req.TTLSeconds == 0 {
		req.TTLSeconds = displayControlDefaultTTLSeconds
	}
	if req.TTLSeconds < displayControlMinTTLSeconds || req.TTLSeconds > displayControlMaxTTLSeconds {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ttl_seconds must be between 30 and 3600"})
		return
	}
	if ok := h.displayControlEnabled(c, c.Param("id")); !ok {
		return
	}

	controlledBy, ok := displayControlActor(c)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "display control requires admin-token or org-token auth"})
		return
	}
	if displaySessionSigningSecret() == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "display session signing secret is not configured"})
		return
	}
	workspaceID := c.Param("id")
	startedAt := time.Now()
	emitDisplayControlEvent(c.Request.Context(), "display.control.acquire.started", workspaceID, map[string]any{
		"controller":    req.Controller,
		"controlled_by": controlledBy,
		"ttl_seconds":   req.TTLSeconds,
	})
	var lock workspaceDisplayControlResponse
	err := db.DB.QueryRowContext(c.Request.Context(), `
INSERT INTO workspace_display_control_locks
    (workspace_id, controller, controlled_by, expires_at)
VALUES
    ($1, $2, $3, now() + ($4 * interval '1 second'))
ON CONFLICT (workspace_id) DO UPDATE
SET controller = EXCLUDED.controller,
    controlled_by = EXCLUDED.controlled_by,
    expires_at = EXCLUDED.expires_at,
    updated_at = now()
WHERE workspace_display_control_locks.expires_at <= now()
   OR workspace_display_control_locks.controlled_by = EXCLUDED.controlled_by
RETURNING controller, controlled_by, expires_at`,
		workspaceID, req.Controller, controlledBy, req.TTLSeconds,
	).Scan(&lock.Controller, &lock.ControlledBy, &lock.ExpiresAt)
	if err == nil {
		lock.SessionURL = signedDisplaySessionURL(workspaceID, lock.ControlledBy, lock.ExpiresAt)
		emitDisplayControlEvent(c.Request.Context(), "display.control.acquire.completed", workspaceID, map[string]any{
			"controller":    lock.Controller,
			"controlled_by": lock.ControlledBy,
			"ttl_seconds":   req.TTLSeconds,
			"duration_ms":   time.Since(startedAt).Milliseconds(),
		})
		c.JSON(http.StatusOK, lock)
		return
	}
	if err == sql.ErrNoRows {
		current, found, loadErr := h.loadActiveDisplayControl(c, workspaceID)
		if loadErr != nil {
			log.Printf("AcquireDisplayControl: load active lock for %s failed: %v", workspaceID, loadErr)
			emitDisplayControlEvent(c.Request.Context(), "display.control.acquire.failed", workspaceID, map[string]any{
				"controlled_by": controlledBy,
				"duration_ms":   time.Since(startedAt).Milliseconds(),
				"error":         loadErr.Error(),
			})
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display control"})
			return
		}
		emitDisplayControlEvent(c.Request.Context(), "display.control.acquire.failed", workspaceID, map[string]any{
			"controlled_by": controlledBy,
			"duration_ms":   time.Since(startedAt).Milliseconds(),
			"error":         "display control already held",
		})
		if !found {
			c.JSON(http.StatusConflict, gin.H{
				"error":   "display control already held",
				"current": workspaceDisplayControlNoneResponse{Controller: "none"},
			})
			return
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":   "display control already held",
			"current": current,
		})
		return
	}
	log.Printf("AcquireDisplayControl: acquire lock for %s failed: %v", workspaceID, err)
	emitDisplayControlEvent(c.Request.Context(), "display.control.acquire.failed", workspaceID, map[string]any{
		"controlled_by": controlledBy,
		"duration_ms":   time.Since(startedAt).Milliseconds(),
		"error":         err.Error(),
	})
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to acquire display control"})
}

// ReleaseDisplayControl handles POST /workspaces/:id/display/control/release.
func (h *WorkspaceHandler) ReleaseDisplayControl(c *gin.Context) {
	var req releaseDisplayControlRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid display control release request"})
			return
		}
	}
	if req.Force {
		if !displayControlIsAdminToken(c) {
			c.JSON(http.StatusForbidden, gin.H{"error": "force release requires admin-token auth"})
			return
		}
	}

	controlledBy, ok := displayControlActor(c)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "display control requires admin-token or org-token auth"})
		return
	}
	workspaceID := c.Param("id")
	startedAt := time.Now()
	emitDisplayControlEvent(c.Request.Context(), "display.control.release.started", workspaceID, map[string]any{
		"controlled_by": controlledBy,
		"force":         req.Force,
	})
	query := `DELETE FROM workspace_display_control_locks WHERE workspace_id = $1 AND controlled_by = $2`
	args := []interface{}{workspaceID, controlledBy}
	if req.Force {
		query = `DELETE FROM workspace_display_control_locks WHERE workspace_id = $1`
		args = []interface{}{workspaceID}
	}
	result, err := db.DB.ExecContext(c.Request.Context(), query, args...)
	if err != nil {
		log.Printf("ReleaseDisplayControl: release lock for %s failed: %v", workspaceID, err)
		emitDisplayControlEvent(c.Request.Context(), "display.control.release.failed", workspaceID, map[string]any{
			"controlled_by": controlledBy,
			"duration_ms":   time.Since(startedAt).Milliseconds(),
			"error":         err.Error(),
			"force":         req.Force,
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to release display control"})
		return
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("ReleaseDisplayControl: rows affected for %s failed: %v", workspaceID, err)
		emitDisplayControlEvent(c.Request.Context(), "display.control.release.failed", workspaceID, map[string]any{
			"controlled_by": controlledBy,
			"duration_ms":   time.Since(startedAt).Milliseconds(),
			"error":         err.Error(),
			"force":         req.Force,
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to release display control"})
		return
	}
	if rowsAffected == 0 {
		current, found, loadErr := h.loadActiveDisplayControl(c, workspaceID)
		if loadErr != nil {
			log.Printf("ReleaseDisplayControl: load active lock for %s failed: %v", workspaceID, loadErr)
			emitDisplayControlEvent(c.Request.Context(), "display.control.release.failed", workspaceID, map[string]any{
				"controlled_by": controlledBy,
				"duration_ms":   time.Since(startedAt).Milliseconds(),
				"error":         loadErr.Error(),
				"force":         req.Force,
			})
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display control"})
			return
		}
		if !found {
			emitDisplayControlEvent(c.Request.Context(), "display.control.release.completed", workspaceID, map[string]any{
				"controlled_by": controlledBy,
				"duration_ms":   time.Since(startedAt).Milliseconds(),
				"force":         req.Force,
				"rows_affected": rowsAffected,
			})
			c.JSON(http.StatusOK, workspaceDisplayControlNoneResponse{Controller: "none"})
			return
		}
		emitDisplayControlEvent(c.Request.Context(), "display.control.release.failed", workspaceID, map[string]any{
			"controlled_by": controlledBy,
			"duration_ms":   time.Since(startedAt).Milliseconds(),
			"error":         "display control held by another caller",
			"force":         req.Force,
		})
		c.JSON(http.StatusConflict, gin.H{
			"error":   "display control held by another caller",
			"current": current,
		})
		return
	}
	emitDisplayControlEvent(c.Request.Context(), "display.control.release.completed", workspaceID, map[string]any{
		"controlled_by": controlledBy,
		"duration_ms":   time.Since(startedAt).Milliseconds(),
		"force":         req.Force,
		"rows_affected": rowsAffected,
	})
	c.JSON(http.StatusOK, workspaceDisplayControlNoneResponse{Controller: "none"})
}

func (h *WorkspaceHandler) loadActiveDisplayControl(c *gin.Context, workspaceID string) (workspaceDisplayControlResponse, bool, error) {
	var lock workspaceDisplayControlResponse
	err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = $1 AND expires_at > now()`,
		workspaceID,
	).Scan(&lock.Controller, &lock.ControlledBy, &lock.ExpiresAt)
	if err == nil {
		return lock, true, nil
	}
	if err == sql.ErrNoRows {
		return workspaceDisplayControlResponse{}, false, nil
	}
	return workspaceDisplayControlResponse{}, false, err
}

func (h *WorkspaceHandler) displayControlEnabled(c *gin.Context, workspaceID string) bool {
	var raw string
	err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT COALESCE(compute, '{}'::jsonb) FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&raw)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return false
		}
		log.Printf("displayControlEnabled: load compute for %s failed: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display config"})
		return false
	}
	compute, err := parseWorkspaceDisplayCompute(workspaceID, raw)
	if err != nil {
		log.Printf("displayControlEnabled: invalid display config for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid display config"})
		return false
	}
	if compute.Display.Mode == "" || compute.Display.Mode == "none" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "display not enabled"})
		return false
	}
	return true
}

func parseWorkspaceDisplayCompute(workspaceID, raw string) (models.WorkspaceCompute, error) {
	var compute models.WorkspaceCompute
	if raw == "" || raw == "{}" {
		return compute, nil
	}
	if err := json.Unmarshal([]byte(raw), &compute); err != nil {
		return models.WorkspaceCompute{}, fmt.Errorf("invalid compute JSON for %s: %w", workspaceID, err)
	}
	if err := validateWorkspaceDisplayConfig(compute.Display); err != nil {
		return models.WorkspaceCompute{}, err
	}
	return compute, nil
}

func displayControlActor(c *gin.Context) (string, bool) {
	if v, ok := c.Get("org_token_prefix"); ok {
		if s, ok := v.(string); ok && s != "" {
			return actorOrgTokenPrefix + s, true
		}
	}
	if v, ok := c.Get("cp_session_actor"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s, true
		}
	}
	if displayControlIsAdminToken(c) {
		return actorAdminToken, true
	}
	return "", false
}

func displayControlIsAdminToken(c *gin.Context) bool {
	adminSecret := os.Getenv("ADMIN_TOKEN")
	if adminSecret == "" {
		return false
	}
	tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
	return subtle.ConstantTimeCompare([]byte(tok), []byte(adminSecret)) == 1
}

func emitDisplayControlEvent(ctx context.Context, eventType string, workspaceID string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		log.Printf("emitDisplayControlEvent: marshal %s payload failed: %v", eventType, err)
		return
	}
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO structure_events (event_type, workspace_id, payload, created_at)
		VALUES ($1, $2, $3::jsonb, now())
	`, eventType, workspaceID, string(payloadJSON)); err != nil {
		log.Printf("emitDisplayControlEvent: insert %s failed: %v", eventType, err)
	}
}

func signedDisplaySessionURL(workspaceID, controlledBy string, expiresAt time.Time) string {
	token := signDisplaySessionToken(workspaceID, controlledBy, expiresAt)
	if token == "" {
		return ""
	}
	return fmt.Sprintf("/workspaces/%s/display/session/websockify#token=%s", url.PathEscape(workspaceID), token)
}

func signDisplaySessionToken(workspaceID, controlledBy string, expiresAt time.Time) string {
	secret := displaySessionSigningSecret()
	if secret == "" || workspaceID == "" || controlledBy == "" || expiresAt.IsZero() {
		return ""
	}
	payload := strings.Join([]string{workspaceID, controlledBy, strconv.FormatInt(expiresAt.Unix(), 10)}, "|")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validateDisplaySessionToken(token, workspaceID, controlledBy string, expiresAt time.Time) bool {
	secret := displaySessionSigningSecret()
	parts := strings.Split(token, ".")
	if secret == "" || len(parts) != 2 || workspaceID == "" || controlledBy == "" || expiresAt.IsZero() || time.Now().After(expiresAt) {
		return false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	payload := string(payloadBytes)
	wantPayload := strings.Join([]string{workspaceID, controlledBy, strconv.FormatInt(expiresAt.Unix(), 10)}, "|")
	if subtle.ConstantTimeCompare([]byte(payload), []byte(wantPayload)) != 1 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return hmac.Equal(sig, mac.Sum(nil))
}

func displaySessionSigningSecret() string {
	return os.Getenv("DISPLAY_SESSION_SIGNING_SECRET")
}
