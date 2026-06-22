package orgtoken

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// AuditAction enumerates the lifecycle events we persist for org API tokens.
type AuditAction string

const (
	AuditActionMint         AuditAction = "mint"
	AuditActionRevoke       AuditAction = "revoke"
	AuditActionValidateFail AuditAction = "validate_fail"
)

// AuditEvent is a single row in org_token_audit_logs.
type AuditEvent struct {
	ID         string          `json:"id"`
	TokenID    *string         `json:"token_id,omitempty"`
	Action     AuditAction     `json:"action"`
	Actor      string          `json:"actor"`
	OrgID      *string         `json:"org_id,omitempty"`
	IPAddress  *string         `json:"ip_address,omitempty"`
	UserAgent  *string         `json:"user_agent,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	CreatedAt  string          `json:"created_at"`
}

// AuditLogRequestContext carries HTTP request-derived metadata for an audit log
// row. It is intentionally small so the orgtoken package stays decoupled from
// gin and HTTP details.
type AuditLogRequestContext struct {
	IPAddress string
	UserAgent string
}

// AuditLogRequestContextFromGin extracts client metadata from a gin context.
func AuditLogRequestContextFromGin(c *gin.Context) AuditLogRequestContext {
	if c == nil || c.Request == nil {
		return AuditLogRequestContext{}
	}
	return AuditLogRequestContext{
		IPAddress: c.ClientIP(),
		UserAgent: c.Request.UserAgent(),
	}
}

// AuditLogRequestContextFromHTTP extracts client metadata from an http.Request.
func AuditLogRequestContextFromHTTP(r *http.Request) AuditLogRequestContext {
	if r == nil {
		return AuditLogRequestContext{}
	}
	ip := r.RemoteAddr
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		ip = xf
	}
	return AuditLogRequestContext{
		IPAddress: ip,
		UserAgent: r.UserAgent(),
	}
}

// LogAuditEvent writes an org_token_audit_logs row. Best-effort: failures are
// logged and swallowed so an audit-log INSERT hiccup cannot break token
// operations.
func LogAuditEvent(ctx context.Context, db *sql.DB, tokenID *string, action AuditAction, actor, orgID string, reqCtx AuditLogRequestContext, metadata map[string]any) {
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		metaJSON = []byte("{}")
	}

	var tokID, oid interface{}
	if tokenID != nil && *tokenID != "" {
		tokID = *tokenID
	} else {
		tokID = nil
	}
	if orgID != "" {
		oid = orgID
	} else {
		oid = nil
	}

	var ip, ua interface{}
	if reqCtx.IPAddress != "" {
		ip = reqCtx.IPAddress
	}
	if reqCtx.UserAgent != "" {
		ua = reqCtx.UserAgent
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO org_token_audit_logs (token_id, action, actor, org_id, ip_address, user_agent, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, tokID, string(action), actor, oid, ip, ua, metaJSON)
	if err != nil {
		log.Printf("orgtoken: failed to write audit log (action=%s actor=%s): %v", action, actor, err)
	}
}

// LogMint records a successful token mint.
func LogMint(ctx context.Context, db *sql.DB, tokenID, actor, orgID string, reqCtx AuditLogRequestContext, metadata map[string]any) {
	LogAuditEvent(ctx, db, &tokenID, AuditActionMint, actor, orgID, reqCtx, metadata)
}

// LogRevoke records a token revocation.
func LogRevoke(ctx context.Context, db *sql.DB, tokenID, actor, orgID string, reqCtx AuditLogRequestContext, metadata map[string]any) {
	LogAuditEvent(ctx, db, &tokenID, AuditActionRevoke, actor, orgID, reqCtx, metadata)
}

// LogValidateFail records a failed token validation. tokenID is nil when the
// presented token did not match any live row; prefix can be logged as metadata
// for correlation with the UI revoke list.
func LogValidateFail(ctx context.Context, db *sql.DB, actor, orgID string, reqCtx AuditLogRequestContext, metadata map[string]any) {
	LogAuditEvent(ctx, db, nil, AuditActionValidateFail, actor, orgID, reqCtx, metadata)
}

// ListAuditEvents returns the most recent audit events for a token, newest
// first. Used by admin/ops endpoints (not yet wired).
func ListAuditEvents(ctx context.Context, db *sql.DB, tokenID string, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, token_id, action, actor, org_id, ip_address, user_agent, metadata, created_at
		FROM org_token_audit_logs
		WHERE token_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, tokenID, limit)
	if err != nil {
		return nil, fmt.Errorf("orgtoken: list audit events: %w", err)
	}
	defer rows.Close()

	out := []AuditEvent{}
	for rows.Next() {
		var e AuditEvent
		var tokID, oid, ip, ua sql.NullString
		var meta []byte
		if err := rows.Scan(&e.ID, &tokID, &e.Action, &e.Actor, &oid, &ip, &ua, &meta, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("orgtoken: scan audit event: %w", err)
		}
		if tokID.Valid {
			e.TokenID = &tokID.String
		}
		if oid.Valid {
			e.OrgID = &oid.String
		}
		if ip.Valid {
			e.IPAddress = &ip.String
		}
		if ua.Valid {
			e.UserAgent = &ua.String
		}
		if len(meta) > 0 {
			e.Metadata = meta
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
