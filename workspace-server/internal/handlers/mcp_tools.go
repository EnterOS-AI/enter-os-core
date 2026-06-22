package handlers

// mcp_tools.go — MCP bridge tool implementations.
// Each tool* method handles one A2A tool: list_peers, get_workspace_info,
// delegate_task, delegate_task_async, check_task_status, send_message_to_user,
// commit_memory, recall_memory. Also contains URL resolution, SSRF checks,
// and A2A response parsing helpers.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/registry"
	"github.com/google/uuid"
)

// marshalA2ABody marshals the JSON-RPC body for an async A2A dispatch.
// Indirected through a package var so tests can force the (otherwise
// near-impossible) marshal-failure path and assert the early return.
var marshalA2ABody = json.Marshal

// insertMCPDelegationRow writes a delegation activity row so the canvas
// Agent Comms tab can show the task text for MCP-initiated delegations.
// Mirrors insertDelegationRow (delegation.go) for the MCP tool path.
func insertMCPDelegationRow(ctx context.Context, db *sql.DB, workspaceID, targetID, delegationID, task string) error {
	taskJSON, marshalErr := json.Marshal(map[string]interface{}{
		"task":          task,
		"delegation_id": delegationID,
	})
	if marshalErr != nil {
		log.Printf("insertMCPDelegationRow %s: json.Marshal taskJSON failed: %v", delegationID, marshalErr)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, request_body, status)
		VALUES ($1, 'delegation', 'delegate', $2, $3, $4, $5::jsonb, $6)
	`, workspaceID, workspaceID, targetID, "Delegating to "+targetID, string(taskJSON), "pending")
	return err
}

// updateMCPDelegationStatus updates a delegation activity row's status.
// Mirrors updateDelegationStatus (delegation.go) for the MCP tool path.
func updateMCPDelegationStatus(ctx context.Context, db *sql.DB, workspaceID, delegationID, status, errorDetail string) {
	if _, err := db.ExecContext(ctx, `
		UPDATE activity_logs
		SET status = $1, error_detail = CASE WHEN $2 = '' THEN error_detail ELSE $2 END
		WHERE workspace_id = $3
		  AND method = 'delegate'
		  AND request_body->>'delegation_id' = $4
	`, status, errorDetail, workspaceID, delegationID); err != nil {
		log.Printf("MCP Delegation %s: status update failed: %v", delegationID, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool implementations
// ─────────────────────────────────────────────────────────────────────────────

func (h *MCPHandler) toolListPeers(ctx context.Context, workspaceID string) (string, error) {
	var parentID sql.NullString
	err := h.database.QueryRowContext(ctx,
		`SELECT parent_id FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&parentID)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("workspace not found")
	}
	if err != nil {
		return "", fmt.Errorf("lookup failed: %w", err)
	}

	type peer struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Role   string `json:"role"`
		Status string `json:"status"`
		Tier   int    `json:"tier"`
	}

	var peers []peer

	scanPeers := func(rows *sql.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var p peer
			if err := rows.Scan(&p.ID, &p.Name, &p.Role, &p.Status, &p.Tier); err != nil {
				return err
			}
			peers = append(peers, p)
		}
		return rows.Err()
	}

	const cols = `SELECT w.id, w.name, COALESCE(w.role,''), w.status, w.tier`

	// Siblings — workspaces sharing the caller's parent.
	//
	// #1953 cross-tenant isolation: the OLD else-branch returned every
	// workspace with parent_id IS NULL when the caller was itself an org root,
	// i.e. every other tenant's org root (the workspaces table has no org_id
	// column). That leaked peer identities across tenants via MCP list_peers.
	// An org root has no siblings inside its own org, so the org-root caller
	// now gets no siblings; its peers are its children, enumerated below. Only
	// the parent_id-bound branch enumerates siblings, scoped to one tenant.
	if parentID.Valid {
		rows, err := h.database.QueryContext(ctx,
			cols+` FROM workspaces w WHERE w.parent_id = $1 AND w.id != $2 AND w.status != 'removed'`,
			parentID.String, workspaceID)
		if err == nil {
			if scanErr := scanPeers(rows); scanErr != nil {
				log.Printf("MCP toolListPeers: sibling scan error: %v", scanErr)
			}
		}
	}

	// Children
	{
		rows, err := h.database.QueryContext(ctx,
			cols+` FROM workspaces w WHERE w.parent_id = $1 AND w.status != 'removed'`,
			workspaceID)
		if err == nil {
			if scanErr := scanPeers(rows); scanErr != nil {
				log.Printf("MCP toolListPeers: children scan error: %v", scanErr)
			}
		}
	}

	// Parent
	if parentID.Valid {
		rows, err := h.database.QueryContext(ctx,
			cols+` FROM workspaces w WHERE w.id = $1 AND w.status != 'removed'`,
			parentID.String)
		if err == nil {
			if scanErr := scanPeers(rows); scanErr != nil {
				log.Printf("MCP toolListPeers: parent scan error: %v", scanErr)
			}
		}
	}

	if len(peers) == 0 {
		return "No peers found.", nil
	}

	b, marshalErr := json.MarshalIndent(peers, "", "  ")
	if marshalErr != nil {
		log.Printf("toolListPeers: json.MarshalIndent peers failed: %v", marshalErr)
		return "", fmt.Errorf("marshal response: %w", marshalErr)
	}
	return string(b), nil
}

func (h *MCPHandler) toolGetWorkspaceInfo(ctx context.Context, workspaceID string) (string, error) {
	var id, name, role, status string
	var tier int
	var parentID sql.NullString

	err := h.database.QueryRowContext(ctx, `
		SELECT id, name, COALESCE(role,''), tier, status, parent_id
		FROM workspaces WHERE id = $1
	`, workspaceID).Scan(&id, &name, &role, &tier, &status, &parentID)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("workspace not found")
	}
	if err != nil {
		return "", fmt.Errorf("lookup failed: %w", err)
	}

	info := map[string]interface{}{
		"id":     id,
		"name":   name,
		"role":   role,
		"tier":   tier,
		"status": status,
	}
	if parentID.Valid {
		info["parent_id"] = parentID.String
	}
	b, marshalErr := json.MarshalIndent(info, "", "  ")
	if marshalErr != nil {
		log.Printf("toolGetWorkspaceInfo %s: json.MarshalIndent info failed: %v", workspaceID, marshalErr)
		return "", fmt.Errorf("marshal response: %w", marshalErr)
	}
	return string(b), nil
}

// buildA2AMessageParts constructs the A2A message parts array from a task string
// and optional attachments. The text part always comes first; attachment parts
// follow in the order provided, with kind derived from MIME type.
func buildA2AMessageParts(task string, attachments []AgentMessageAttachment) []map[string]interface{} {
	parts := []map[string]interface{}{
		// A2A v0.3 Part discriminator is `kind`, NOT `type` (#2251).
		// The receiver's v0.3 Pydantic validator drops a Part keyed
		// `type`, silently losing the task text — the file part below
		// already uses `kind`, this is the matching fix for text.
		{"kind": "text", "text": task},
	}
	for _, att := range attachments {
		kind := kindFromMimeType(att.MimeType)
		filePart := map[string]interface{}{
			"kind": kind,
			"file": map[string]interface{}{
				"uri":       att.URI,
				"mime_type": att.MimeType,
				"name":      att.Name,
			},
		}
		parts = append(parts, filePart)
	}
	return parts
}

func (h *MCPHandler) toolDelegateTask(ctx context.Context, callerID string, args map[string]interface{}, timeout time.Duration) (string, error) {
	targetID, _ := args["workspace_id"].(string)
	task, _ := args["task"].(string)
	if targetID == "" {
		return "", fmt.Errorf("workspace_id is required")
	}
	if task == "" {
		return "", fmt.Errorf("task is required")
	}

	if !registry.CanCommunicate(callerID, targetID) {
		return "", fmt.Errorf("workspace %s is not authorised to communicate with %s", callerID, targetID)
	}

	// Issue #158: write delegation row so canvas Agent Comms tab shows the task text.
	delegationID := uuid.New().String()
	if err := insertMCPDelegationRow(ctx, h.database, callerID, targetID, delegationID, task); err != nil {
		log.Printf("MCP delegate_task: failed to record delegation row: %v", err)
		// Non-fatal: still make the A2A call even if activity log write fails.
	}

	attachments, _ := parseAgentMessageAttachments(args["attachments"])

	a2aBody, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      uuid.New().String(),
		"method":  "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":      "user",
				"parts":     buildA2AMessageParts(task, attachments),
				"messageId": uuid.New().String(),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to build A2A request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	status, body, err := h.proxyA2ARequest(reqCtx, targetID, a2aBody, callerID, true)
	if err != nil {
		updateMCPDelegationStatus(ctx, h.database, callerID, delegationID, "failed", err.Error())
		return "", fmt.Errorf("A2A proxy failed: %w", err)
	}
	if status < 200 || status >= 300 {
		updateMCPDelegationStatus(ctx, h.database, callerID, delegationID, "failed", fmt.Sprintf("A2A proxy returned status %d", status))
		return "", fmt.Errorf("A2A proxy returned status %d", status)
	}
	updateMCPDelegationStatus(ctx, h.database, callerID, delegationID, "dispatched", "")

	return extractA2AText(body), nil
}

func (h *MCPHandler) toolDelegateTaskAsync(ctx context.Context, callerID string, args map[string]interface{}) (string, error) {
	targetID, _ := args["workspace_id"].(string)
	task, _ := args["task"].(string)
	if targetID == "" {
		return "", fmt.Errorf("workspace_id is required")
	}
	if task == "" {
		return "", fmt.Errorf("task is required")
	}

	if !registry.CanCommunicate(callerID, targetID) {
		return "", fmt.Errorf("workspace %s is not authorised to communicate with %s", callerID, targetID)
	}

	delegationID := uuid.New().String()

	// Issue #158: write delegation row so canvas Agent Comms tab shows the task text.
	// Insert with 'queued' status; goroutine updates to delivered or failed.
	if err := insertMCPDelegationRow(ctx, h.database, callerID, targetID, delegationID, task); err != nil {
		log.Printf("MCP delegate_task_async: failed to record delegation row: %v", err)
		// Non-fatal: still fire the A2A call.
	} else {
		updateMCPDelegationStatus(ctx, h.database, callerID, delegationID, "queued", "")
	}

	// Fire and forget in a detached goroutine. Use a background context so
	// the call is not cancelled when the HTTP request completes.
	// RFC internal#524 Layer 1: globalGoAsync — the detached call reads db.DB
	// through the platform A2A proxy and must be drained by drainTestAsync
	// before any t.Cleanup-driven db.DB swap.
	globalGoAsync(func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), mcpAsyncCallTimeout)
		defer cancel()

		attachments, _ := parseAgentMessageAttachments(args["attachments"])

		a2aBody, marshalErr := marshalA2ABody(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      delegationID,
			"method":  "message/send",
			"params": map[string]interface{}{
				"message": map[string]interface{}{
					"role":      "user",
					"parts":     buildA2AMessageParts(task, attachments),
					"messageId": uuid.New().String(),
				},
			},
		})
		if marshalErr != nil {
			log.Printf("toolDelegateTask %s: json.Marshal a2aBody failed: %v", delegationID, marshalErr)
			// Bail out: proceeding would call proxyA2ARequest with a
			// nil/empty body, dispatching a malformed A2A request.
			updateMCPDelegationStatus(bgCtx, h.database, callerID, delegationID, "failed", fmt.Sprintf("marshal_error: %v", marshalErr))
			return
		}

		status, _, err := h.proxyA2ARequest(bgCtx, targetID, a2aBody, callerID, true)
		if err != nil || status < 200 || status >= 300 {
			var errorDetail string
			if err != nil {
				log.Printf("MCPHandler.delegate_task_async: A2A proxy to %s: %v", targetID, err)
				errorDetail = fmt.Sprintf("target_offline: %v", err)
			} else {
				log.Printf("MCPHandler.delegate_task_async: A2A proxy to %s returned status %d", targetID, status)
				errorDetail = fmt.Sprintf("http_status: %d", status)
			}
			updateMCPDelegationStatus(bgCtx, h.database, callerID, delegationID, "failed", errorDetail)
			return
		}

		updateMCPDelegationStatus(bgCtx, h.database, callerID, delegationID, "delivered", "")
	})

	return fmt.Sprintf(`{"task_id":%q,"status":"queued","target_id":%q}`, delegationID, targetID), nil
}

func (h *MCPHandler) toolCheckTaskStatus(ctx context.Context, callerID string, args map[string]interface{}) (string, error) {
	targetID, _ := args["workspace_id"].(string)
	taskID, _ := args["task_id"].(string)
	if targetID == "" {
		return "", fmt.Errorf("workspace_id is required")
	}
	if taskID == "" {
		return "", fmt.Errorf("task_id is required")
	}

	var status, errorDetail sql.NullString
	var responseBody []byte

	err := h.database.QueryRowContext(ctx, `
		SELECT status, error_detail, response_body
		FROM activity_logs
		WHERE workspace_id = $1
		  AND target_id = $2
		  AND request_body->>'delegation_id' = $3
		ORDER BY created_at DESC
		LIMIT 1
	`, callerID, targetID, taskID).Scan(&status, &errorDetail, &responseBody)
	if err == sql.ErrNoRows {
		return fmt.Sprintf(`{"task_id":%q,"status":"not_found","note":"task not tracked or not yet dispatched"}`, taskID), nil
	}
	if err != nil {
		return "", fmt.Errorf("status lookup failed: %w", err)
	}

	result := map[string]interface{}{
		"task_id":   taskID,
		"target_id": targetID,
	}
	if status.Valid {
		result["status"] = status.String
	} else {
		result["status"] = "unknown"
	}
	if errorDetail.Valid && errorDetail.String != "" {
		result["error"] = errorDetail.String
	}
	if len(responseBody) > 0 {
		result["result"] = extractA2AText(responseBody)
	}
	b, marshalErr := json.MarshalIndent(result, "", "  ")
	if marshalErr != nil {
		log.Printf("toolCheckTaskStatus: json.MarshalIndent result failed: %v", marshalErr)
		return "", fmt.Errorf("marshal response: %w", marshalErr)
	}
	return string(b), nil
}

func (h *MCPHandler) toolSendMessageToUser(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	message, _ := args["message"].(string)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	// Check send_message_to_user is enabled (C3).
	if os.Getenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE") != "true" {
		return "", fmt.Errorf("send_message_to_user is not enabled on this MCP bridge (set MOLECULE_MCP_ALLOW_SEND_MESSAGE=true)")
	}

	// Single source of truth for chat-bearing agent → user messages —
	// see agent_message_writer.go for the contract. The pre-RFC-#2945
	// duplication of broadcast + INSERT logic between this handler and
	// activity.go:Notify is what produced the acme-demo data-loss
	// regression; both paths now route through the same writer.
	//
	attachments, err := parseAgentMessageAttachments(args["attachments"])
	if err != nil {
		return "", err
	}
	writer := NewAgentMessageWriter(h.database, h.broadcaster)
	if err := writer.Send(ctx, workspaceID, message, attachments); err != nil {
		if errors.Is(err, ErrWorkspaceNotFound) {
			return "", fmt.Errorf("workspace not found")
		}
		return "", err
	}
	return "Message sent.", nil
}

// toolRequestUserAction implements request_user_action — the agent raises a
// tracked ask for the human user (it appears in the concierge Tasks list).
// Mirrors the user_tasks REST Create handler. Unlike send_message_to_user it
// is not gated behind MOLECULE_MCP_ALLOW_SEND_MESSAGE — raising an ask is
// always allowed.
func (h *MCPHandler) toolRequestUserAction(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	title, _ := args["title"].(string)
	if title == "" {
		return "", fmt.Errorf("title is required")
	}
	detail, _ := args["detail"].(string)

	// SSOT for user-task persistence + validation + broadcast — see
	// user_task_store.go. Pre-consolidation this hand-wrote the same INSERT
	// and USER_TASK_REQUESTED broadcast the REST Create handler did.
	if _, err := h.userTaskStore().Create(ctx, workspaceID, title, detail); err != nil {
		return "", fmt.Errorf("failed to create user task: %w", err)
	}

	return "Asked the user: " + title, nil
}

// toolListUserTasks implements list_user_tasks — the asks THIS workspace
// raised, with status. Returns a JSON array string.
func (h *MCPHandler) toolListUserTasks(ctx context.Context, workspaceID string) (string, error) {
	rows, err := h.userTaskStore().List(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("failed to list user tasks: %w", err)
	}

	// The MCP surface returns a slimmer shape than the REST list (no
	// resolved_at / resolved_by). Project the store rows down so the
	// existing tool output stays stable.
	type ut struct {
		ID        string  `json:"id"`
		Title     string  `json:"title"`
		Detail    *string `json:"detail"`
		Status    string  `json:"status"`
		CreatedAt string  `json:"created_at"`
	}
	tasks := make([]ut, 0, len(rows))
	for _, r := range rows {
		tasks = append(tasks, ut{
			ID:        r.ID,
			Title:     r.Title,
			Detail:    r.Detail,
			Status:    r.Status,
			CreatedAt: r.CreatedAt,
		})
	}
	out, err := json.Marshal(tasks)
	if err != nil {
		return "", fmt.Errorf("failed to encode user tasks: %w", err)
	}
	return string(out), nil
}

// toolUpdateUserTask implements update_user_task — edit a task this workspace
// raised (title / detail / status). Scoped by workspace_id.
func (h *MCPHandler) toolUpdateUserTask(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	taskID, _ := args["user_task_id"].(string)
	if taskID == "" {
		return "", fmt.Errorf("user_task_id is required")
	}
	var title, detail, status *string
	if v, ok := args["title"].(string); ok && v != "" {
		title = &v
	}
	if v, ok := args["detail"].(string); ok && v != "" {
		detail = &v
	}
	if v, ok := args["status"].(string); ok && v != "" {
		status = &v
	}

	// SSOT for the COALESCE update + status-enum validation — see
	// user_task_store.go.
	if err := h.userTaskStore().Update(ctx, workspaceID, taskID, title, detail, status); err != nil {
		if errors.Is(err, ErrInvalidUserTaskStatus) {
			return "", fmt.Errorf("status must be 'pending', 'done' or 'dismissed'")
		}
		if errors.Is(err, ErrUserTaskNotFound) {
			return "", fmt.Errorf("user task not found")
		}
		return "", fmt.Errorf("failed to update user task: %w", err)
	}
	return "User task updated.", nil
}

// toolDeleteUserTask implements delete_user_task — remove a task this
// workspace raised. Scoped by workspace_id.
func (h *MCPHandler) toolDeleteUserTask(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	taskID, _ := args["user_task_id"].(string)
	if taskID == "" {
		return "", fmt.Errorf("user_task_id is required")
	}
	if err := h.userTaskStore().Delete(ctx, workspaceID, taskID); err != nil {
		if errors.Is(err, ErrUserTaskNotFound) {
			return "", fmt.Errorf("user task not found")
		}
		return "", fmt.Errorf("failed to delete user task: %w", err)
	}
	return "User task deleted.", nil
}

func parseAgentMessageAttachments(raw interface{}) ([]AgentMessageAttachment, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("attachments must be an array")
	}
	if len(items) == 0 {
		return nil, nil
	}
	attachments := make([]AgentMessageAttachment, 0, len(items))
	for i, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("attachment[%d]: must be an object", i)
		}
		uri, _ := m["uri"].(string)
		name, _ := m["name"].(string)
		if uri == "" || name == "" {
			return nil, fmt.Errorf("attachment[%d]: uri and name are required", i)
		}
		att := AgentMessageAttachment{
			URI:  uri,
			Name: name,
		}
		if mimeType, ok := m["mimeType"].(string); ok {
			att.MimeType = mimeType
		}
		if size, ok := numericInt64(m["size"]); ok {
			att.Size = size
		}
		attachments = append(attachments, att)
	}
	return attachments, nil
}

func numericInt64(raw interface{}) (int64, bool) {
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	default:
		return 0, false
	}
}

func (h *MCPHandler) toolCommitMemory(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	// Issue #1733 — v2 memory plugin is now the only path. The legacy
	// SQL fallback on `agent_memories` is gone; an unconfigured plugin
	// returns a clear error to the agent rather than silently writing
	// into a stale table no one reads.
	if err := h.memoryV2Available(); err != nil {
		return "", err
	}
	return h.commitMemoryLegacyShim(ctx, workspaceID, args)
}

func (h *MCPHandler) toolRecallMemory(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	// Issue #1733 — v2 memory plugin is now the only path. Same shape
	// as toolCommitMemory: an unconfigured plugin is an error, not a
	// quiet read from a frozen v1 table.
	if err := h.memoryV2Available(); err != nil {
		return "", err
	}
	return h.recallMemoryLegacyShim(ctx, workspaceID, args)
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// extractA2AText extracts human-readable text from an A2A JSON-RPC response body.
// Falls back to the raw JSON when no text part can be found.
func extractA2AText(body []byte) string {
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return string(body)
	}

	// Propagate A2A errors.
	if errObj, ok := resp["error"].(map[string]interface{}); ok {
		if msg, ok := errObj["message"].(string); ok {
			return "[error] " + msg
		}
	}

	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		return string(body)
	}

	// Format 1: result.artifacts[0].parts[0].text
	if artifacts, ok := result["artifacts"].([]interface{}); ok && len(artifacts) > 0 {
		if art, ok := artifacts[0].(map[string]interface{}); ok {
			if parts, ok := art["parts"].([]interface{}); ok && len(parts) > 0 {
				if part, ok := parts[0].(map[string]interface{}); ok {
					if text, ok := part["text"].(string); ok && text != "" {
						return text
					}
				}
			}
		}
	}

	// Format 2: result.message.parts[0].text
	if msg, ok := result["message"].(map[string]interface{}); ok {
		if parts, ok := msg["parts"].([]interface{}); ok && len(parts) > 0 {
			if part, ok := parts[0].(map[string]interface{}); ok {
				if text, ok := part["text"].(string); ok && text != "" {
					return text
				}
			}
		}
	}

	// Fallback: marshal result as JSON.
	b, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		log.Printf("extractA2AText: json.Marshal result failed: %v", marshalErr)
	}
	return string(b)
}
