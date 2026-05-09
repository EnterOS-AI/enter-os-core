package handlers

// mcp_tools.go — MCP bridge tool implementations.
// Each tool* method handles one A2A tool: list_peers, get_workspace_info,
// delegate_task, delegate_task_async, check_task_status, send_message_to_user,
// commit_memory, recall_memory. Also contains URL resolution, SSRF checks,
// and A2A response parsing helpers.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/registry"
	"github.com/google/uuid"
)
// insertMCPDelegationRow writes a delegation activity row so the canvas
// Agent Comms tab can show the task text for MCP-initiated delegations.
// Mirrors insertDelegationRow (delegation.go) for the MCP tool path.
func insertMCPDelegationRow(ctx context.Context, db *sql.DB, workspaceID, targetID, delegationID, task string) error {
	taskJSON, _ := json.Marshal(map[string]interface{}{
		"task":          task,
		"delegation_id": delegationID,
	})
	_, err := db.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, request_body, status)
		VALUES ($1, 'delegation', 'delegate', $2, $3, $4, $5::jsonb, 'pending')
	`, workspaceID, workspaceID, targetID, "Delegating to "+targetID, string(taskJSON))
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

	// Siblings
	if parentID.Valid {
		rows, err := h.database.QueryContext(ctx,
			cols+` FROM workspaces w WHERE w.parent_id = $1 AND w.id != $2 AND w.status != 'removed'`,
			parentID.String, workspaceID)
		if err == nil {
			_ = scanPeers(rows)
		}
	} else {
		rows, err := h.database.QueryContext(ctx,
			cols+` FROM workspaces w WHERE w.parent_id IS NULL AND w.id != $1 AND w.status != 'removed'`,
			workspaceID)
		if err == nil {
			_ = scanPeers(rows)
		}
	}

	// Children
	{
		rows, err := h.database.QueryContext(ctx,
			cols+` FROM workspaces w WHERE w.parent_id = $1 AND w.status != 'removed'`,
			workspaceID)
		if err == nil {
			_ = scanPeers(rows)
		}
	}

	// Parent
	if parentID.Valid {
		rows, err := h.database.QueryContext(ctx,
			cols+` FROM workspaces w WHERE w.id = $1 AND w.status != 'removed'`,
			parentID.String)
		if err == nil {
			_ = scanPeers(rows)
		}
	}

	if len(peers) == 0 {
		return "No peers found.", nil
	}

	b, _ := json.MarshalIndent(peers, "", "  ")
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
	b, _ := json.MarshalIndent(info, "", "  ")
	return string(b), nil
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

	agentURL, err := mcpResolveURL(ctx, h.database, targetID)
	if err != nil {
		return "", err
	}
	// SSRF defence: reject private/metadata URLs before making outbound call.
	if err := isSafeURL(agentURL); err != nil {
		return "", fmt.Errorf("invalid workspace URL: %w", err)
	}

	a2aBody, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      uuid.New().String(),
		"method":  "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":      "user",
				"parts":     []map[string]interface{}{{"type": "text", "text": task}},
				"messageId": uuid.New().String(),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to build A2A request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", agentURL+"/a2a", bytes.NewReader(a2aBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// X-Workspace-ID identifies this caller to the A2A proxy. The /workspaces/:id/a2a
	// endpoint is intentionally outside WorkspaceAuth (agents do not hold bearer tokens
	// to peer workspaces). Access control is enforced by CanCommunicate above, which
	// already validated callerID → targetID before this request is constructed.
	// callerID was authenticated by WorkspaceAuth on the MCP bridge entry point,
	// so this header reflects a verified caller identity, not a spoofable value.
	httpReq.Header.Set("X-Workspace-ID", callerID)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		updateMCPDelegationStatus(ctx, h.database, callerID, delegationID, "failed", err.Error())
		return "", fmt.Errorf("A2A call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A 200/500 from the peer still means the call was dispatched — only
	// network errors are truly "failed". Status 'dispatched' is correct for
	// any HTTP response (peer's A2A layer handles the actual processing).
	updateMCPDelegationStatus(ctx, h.database, callerID, delegationID, "dispatched", "")

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

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
	// Insert with 'dispatched' status since the goroutine won't update it.
	if err := insertMCPDelegationRow(ctx, h.database, callerID, targetID, delegationID, task); err != nil {
		log.Printf("MCP delegate_task_async: failed to record delegation row: %v", err)
		// Non-fatal: still fire the A2A call.
	} else {
		updateMCPDelegationStatus(ctx, h.database, callerID, delegationID, "dispatched", "")
	}

	// Fire and forget in a detached goroutine. Use a background context so
	// the call is not cancelled when the HTTP request completes.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), mcpAsyncCallTimeout)
		defer cancel()

		agentURL, err := mcpResolveURL(bgCtx, h.database, targetID)
		if err != nil {
			log.Printf("MCPHandler.delegate_task_async: resolve URL for %s: %v", targetID, err)
			return
		}
		// SSRF defence: reject private/metadata URLs before making outbound call.
		if err := isSafeURL(agentURL); err != nil {
			log.Printf("MCPHandler.delegate_task_async: unsafe URL for %s: %v", targetID, err)
			return
		}

		a2aBody, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      delegationID,
			"method":  "message/send",
			"params": map[string]interface{}{
				"message": map[string]interface{}{
					"role":      "user",
					"parts":     []map[string]interface{}{{"type": "text", "text": task}},
					"messageId": uuid.New().String(),
				},
			},
		})

		httpReq, err := http.NewRequestWithContext(bgCtx, "POST", agentURL+"/a2a", bytes.NewReader(a2aBody))
		if err != nil {
			log.Printf("MCPHandler.delegate_task_async: create request: %v", err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("X-Workspace-ID", callerID)

		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			log.Printf("MCPHandler.delegate_task_async: A2A call to %s: %v", targetID, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		// Drain response so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
	}()

	return fmt.Sprintf(`{"task_id":%q,"status":"dispatched","target_id":%q}`, delegationID, targetID), nil
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
		"status":    status.String,
		"target_id": targetID,
	}
	if errorDetail.Valid && errorDetail.String != "" {
		result["error"] = errorDetail.String
	}
	if len(responseBody) > 0 {
		result["result"] = extractA2AText(responseBody)
	}
	b, _ := json.MarshalIndent(result, "", "  ")
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
	// activity.go:Notify is what produced the reno-stars data-loss
	// regression; both paths now route through the same writer.
	//
	// MCP send_message_to_user does not currently surface attachments
	// (the tool args don't accept them); pass nil. If a future tool
	// schema adds an attachments arg, build []AgentMessageAttachment
	// and pass through.
	writer := NewAgentMessageWriter(h.database, h.broadcaster)
	if err := writer.Send(ctx, workspaceID, message, nil); err != nil {
		if errors.Is(err, ErrWorkspaceNotFound) {
			return "", fmt.Errorf("workspace not found")
		}
		return "", err
	}
	return "Message sent.", nil
}


func (h *MCPHandler) toolCommitMemory(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	// PR-6 (RFC #2728) compat shim: when the v2 plugin is wired
	// (MEMORY_PLUGIN_URL set), translate legacy scope→namespace and
	// delegate. Otherwise fall through to the legacy DB path so
	// operators who haven't enabled the plugin yet keep working.
	if h.memoryV2Available() == nil {
		return h.commitMemoryLegacyShim(ctx, workspaceID, args)
	}

	content, _ := args["content"].(string)
	scope, _ := args["scope"].(string)
	if content == "" {
		return "", fmt.Errorf("content is required")
	}
	if scope == "" {
		scope = "LOCAL"
	}

	// C3: GLOBAL scope is blocked on the MCP bridge.
	if scope == "GLOBAL" {
		return "", fmt.Errorf("GLOBAL scope is not permitted via the MCP bridge — use LOCAL or TEAM")
	}
	if scope != "LOCAL" && scope != "TEAM" {
		return "", fmt.Errorf("scope must be LOCAL or TEAM")
	}

	memoryID := uuid.New().String()
	// SAFE-T1201 (#838): scrub known credential patterns before persistence so
	// plain-text API keys pulled in via tool responses can't land in the
	// memories table (and leak into shared TEAM scope). Reuses redactSecrets
	// already shipped for the HTTP path in PR #881 — this was the MCP-bridge
	// sibling the original fix missed. Runs on every write regardless of scope.
	content, _ = redactSecrets(workspaceID, content)
	_, err := h.database.ExecContext(ctx, `
		INSERT INTO agent_memories (id, workspace_id, content, scope, namespace)
		VALUES ($1, $2, $3, $4, $5)
	`, memoryID, workspaceID, content, scope, workspaceID)
	if err != nil {
		log.Printf("MCPHandler.commit_memory workspace=%s: %v", workspaceID, err)
		return "", fmt.Errorf("failed to save memory")
	}

	return fmt.Sprintf(`{"id":%q,"scope":%q}`, memoryID, scope), nil
}

func (h *MCPHandler) toolRecallMemory(ctx context.Context, workspaceID string, args map[string]interface{}) (string, error) {
	// PR-6 (RFC #2728) compat shim: when the v2 plugin is wired,
	// route through it. Otherwise fall through to legacy DB path.
	if h.memoryV2Available() == nil {
		return h.recallMemoryLegacyShim(ctx, workspaceID, args)
	}

	query, _ := args["query"].(string)
	scope, _ := args["scope"].(string)

	// C3: GLOBAL scope is blocked on the MCP bridge.
	if scope == "GLOBAL" {
		return "", fmt.Errorf("GLOBAL scope is not permitted via the MCP bridge — use LOCAL, TEAM, or empty")
	}

	var rows *sql.Rows
	var err error

	switch scope {
	case "LOCAL":
		rows, err = h.database.QueryContext(ctx, `
			SELECT id, content, scope, created_at
			FROM agent_memories
			WHERE workspace_id = $1 AND scope = 'LOCAL'
			  AND ($2 = '' OR content ILIKE '%' || $2 || '%')
			ORDER BY created_at DESC LIMIT 50
		`, workspaceID, query)
	case "TEAM":
		// Team scope: parent + all siblings.
		rows, err = h.database.QueryContext(ctx, `
			SELECT m.id, m.content, m.scope, m.created_at
			FROM agent_memories m
			JOIN workspaces w ON w.id = m.workspace_id
			WHERE m.scope = 'TEAM'
			  AND w.status != 'removed'
			  AND (w.id = $1 OR w.parent_id = (SELECT parent_id FROM workspaces WHERE id = $1 AND parent_id IS NOT NULL))
			  AND ($2 = '' OR m.content ILIKE '%' || $2 || '%')
			ORDER BY m.created_at DESC LIMIT 50
		`, workspaceID, query)
	default:
		// Empty scope → LOCAL only for the MCP bridge (GLOBAL excluded per C3).
		rows, err = h.database.QueryContext(ctx, `
			SELECT id, content, scope, created_at
			FROM agent_memories
			WHERE workspace_id = $1 AND scope IN ('LOCAL', 'TEAM')
			  AND ($2 = '' OR content ILIKE '%' || $2 || '%')
			ORDER BY created_at DESC LIMIT 50
		`, workspaceID, query)
	}
	if err != nil {
		return "", fmt.Errorf("memory search failed: %w", err)
	}
	defer rows.Close()

	type memEntry struct {
		ID        string `json:"id"`
		Content   string `json:"content"`
		Scope     string `json:"scope"`
		CreatedAt string `json:"created_at"`
	}
	var results []memEntry
	for rows.Next() {
		var e memEntry
		if err := rows.Scan(&e.ID, &e.Content, &e.Scope, &e.CreatedAt); err != nil {
			continue
		}
		results = append(results, e)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("memory scan error: %w", err)
	}

	if len(results) == 0 {
		return "No memories found.", nil
	}
	b, _ := json.MarshalIndent(results, "", "  ")
	return string(b), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// mcpResolveURL returns a routable URL for a workspace's A2A server.
//
// Resolution order:
//  1. Docker-internal URL cache (set by provisioner; correct when platform is in Docker)
//  2. Redis URL cache
//  3. DB `url` column fallback, with 127.0.0.1→Docker bridge rewrite when in Docker
//
// SECURITY (F1083 / #1130): all three paths run the returned URL through
// validateAgentURL to block SSRF targets (private IPs, loopback, cloud metadata).
func mcpResolveURL(ctx context.Context, database *sql.DB, workspaceID string) (string, error) {
	if platformInDocker {
		if url, err := db.GetCachedInternalURL(ctx, workspaceID); err == nil && url != "" {
			if err := validateAgentURL(url); err != nil {
				return "", fmt.Errorf("workspace %s: forbidden URL from internal cache: %w", workspaceID, err)
			}
			return url, nil
		}
	}
	if url, err := db.GetCachedURL(ctx, workspaceID); err == nil && url != "" {
		if platformInDocker && strings.HasPrefix(url, "http://127.0.0.1:") {
			return provisioner.InternalURL(workspaceID), nil
		}
		if err := validateAgentURL(url); err != nil {
			return "", fmt.Errorf("workspace %s: forbidden URL from Redis cache: %w", workspaceID, err)
		}
		return url, nil
	}

	var urlStr sql.NullString
	var status string
	if err := database.QueryRowContext(ctx,
		`SELECT url, status FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&urlStr, &status); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("workspace %s not found", workspaceID)
		}
		return "", fmt.Errorf("workspace lookup failed: %w", err)
	}
	if !urlStr.Valid || urlStr.String == "" {
		return "", fmt.Errorf("workspace %s has no URL (status: %s)", workspaceID, status)
	}
	if platformInDocker && strings.HasPrefix(urlStr.String, "http://127.0.0.1:") {
		return provisioner.InternalURL(workspaceID), nil
	}
	if err := validateAgentURL(urlStr.String); err != nil {
		return "", fmt.Errorf("workspace %s: forbidden URL from DB: %w", workspaceID, err)
	}
	return urlStr.String, nil
}

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
	b, _ := json.Marshal(result)
	return string(b)
}

