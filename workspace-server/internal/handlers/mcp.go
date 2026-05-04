package handlers

// mcp.go — MCP bridge protocol handling: JSON-RPC types, handler struct,
// tool definitions, HTTP endpoints (Call, Stream), and RPC dispatch.
// Tool implementations live in mcp_tools.go.
//
// MCP bridge for opencode integration (#800, #809, #810).
//
// Exposes the same 8 A2A tools as workspace/a2a_mcp_server.py but
// served directly from the platform over HTTP so CLI runtimes running
// OUTSIDE workspace containers (opencode, Claude Code on the developer's
// machine) can participate in the A2A mesh.
//
// Routes (registered under wsAuth — bearer token binds to :id):
//
//	GET  /workspaces/:id/mcp/stream  — SSE transport (MCP 2024-11-05 compat)
//	POST /workspaces/:id/mcp         — Streamable HTTP transport (primary)
//
// Security conditions satisfied:
//   C1: WorkspaceAuth middleware rejects requests without a valid bearer token.
//   C2: MCPRateLimiter (120 req/min/token) middleware applied in router.go.
//   C3: commit_memory / recall_memory with scope=GLOBAL return a permission
//       error; send_message_to_user is excluded from tools/list unless
//       MOLECULE_MCP_ALLOW_SEND_MESSAGE=true.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/gin-gonic/gin"
)

// mcpProtocolVersion is the MCP spec version this server implements.
const mcpProtocolVersion = "2024-11-05"

// mcpCallTimeout is the maximum time delegate_task waits for a workspace response.
const mcpCallTimeout = 30 * time.Second

// mcpAsyncCallTimeout is the fire-and-forget A2A call timeout for delegate_task_async.
const mcpAsyncCallTimeout = 8 * time.Second

// ─────────────────────────────────────────────────────────────────────────────
// JSON-RPC 2.0 types
// ─────────────────────────────────────────────────────────────────────────────

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      interface{}  `json:"id"`
	Result  interface{}  `json:"result,omitempty"`
	Error   *mcpRPCError `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpTool is a tool descriptor returned in tools/list responses.
type mcpTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Handler
// ─────────────────────────────────────────────────────────────────────────────

// MCPHandler serves the MCP bridge endpoints for the workspace identified by :id.
type MCPHandler struct {
	database    *sql.DB
	broadcaster *events.Broadcaster

	// memv2 is the v2 memory plugin wiring (RFC #2728). nil-safe:
	// every v2 tool calls memoryV2Available() first and returns a
	// clear error rather than crashing when the operator hasn't set
	// MEMORY_PLUGIN_URL.
	memv2 *memoryV2Deps
}

// NewMCPHandler wires the handler to db and broadcaster.
// Pass db.DB and the platform broadcaster at router-setup time.
func NewMCPHandler(database *sql.DB, broadcaster *events.Broadcaster) *MCPHandler {
	return &MCPHandler{database: database, broadcaster: broadcaster}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool definitions (mirrors workspace/a2a_mcp_server.py TOOLS list)
// ─────────────────────────────────────────────────────────────────────────────

var mcpAllTools = []mcpTool{
	{
		Name:        "delegate_task",
		Description: "Delegate a task to another workspace via A2A protocol and WAIT for the response. Use for quick tasks. The target must be a peer (sibling or parent/child). Use list_peers to find available targets.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"workspace_id": map[string]interface{}{
					"type":        "string",
					"description": "Target workspace ID (from list_peers)",
				},
				"task": map[string]interface{}{
					"type":        "string",
					"description": "The task description to send to the target workspace",
				},
			},
			"required": []string{"workspace_id", "task"},
		},
	},
	{
		Name:        "delegate_task_async",
		Description: "Send a task to another workspace with a short timeout (fire-and-forget). Returns immediately with a task_id — use check_task_status to poll for results.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"workspace_id": map[string]interface{}{
					"type":        "string",
					"description": "Target workspace ID (from list_peers)",
				},
				"task": map[string]interface{}{
					"type":        "string",
					"description": "The task description to send to the target workspace",
				},
			},
			"required": []string{"workspace_id", "task"},
		},
	},
	{
		Name:        "check_task_status",
		Description: "Check the status of a previously submitted async task. Returns status (dispatched/success/failed) and result when available.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"workspace_id": map[string]interface{}{
					"type":        "string",
					"description": "The workspace ID the task was sent to",
				},
				"task_id": map[string]interface{}{
					"type":        "string",
					"description": "The task_id returned by delegate_task_async",
				},
			},
			"required": []string{"workspace_id", "task_id"},
		},
	},
	{
		Name:        "list_peers",
		Description: "List all workspaces this agent can communicate with (siblings and parent/children). Returns name, ID, status, and role for each peer.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
	{
		Name:        "get_workspace_info",
		Description: "Get this workspace's own info — ID, name, role, tier, parent, status.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
	{
		Name:        "send_message_to_user",
		Description: "Send a message directly to the user's canvas chat — pushed instantly via WebSocket. Use this to acknowledge tasks, send progress updates, or deliver follow-up results.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"message": map[string]interface{}{
					"type":        "string",
					"description": "The message to send to the user",
				},
			},
			"required": []string{"message"},
		},
	},
	{
		Name:        "commit_memory",
		Description: "Save important information to persistent memory. Scope LOCAL (this workspace only) and TEAM (parent + siblings) are supported. GLOBAL scope is not available via the MCP bridge.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"content": map[string]interface{}{
					"type":        "string",
					"description": "The information to remember",
				},
				"scope": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"LOCAL", "TEAM"},
					"description": "Memory scope (LOCAL or TEAM — GLOBAL is blocked on the MCP bridge)",
				},
			},
			"required": []string{"content"},
		},
	},
	{
		Name:        "recall_memory",
		Description: "Search persistent memory for previously saved information. Returns all matching memories. GLOBAL scope is not available via the MCP bridge.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Search query (empty returns all memories)",
				},
				"scope": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"LOCAL", "TEAM", ""},
					"description": "Filter by scope (empty returns LOCAL + TEAM; GLOBAL is blocked)",
				},
			},
		},
	},

	// ─────────────────────────────────────────────────────────────────
	// v2 memory tools (RFC #2728). Coexist with legacy commit_memory /
	// recall_memory; PR-6 aliases the legacy names. Surface here so
	// agents calling tools/list see them when MEMORY_PLUGIN_URL is
	// configured (handlers no-op cleanly when it isn't).
	// ─────────────────────────────────────────────────────────────────
	{
		Name:        "commit_memory_v2",
		Description: "Save a memory to a namespace. Defaults to your own workspace. Use list_writable_namespaces to discover what else you can write to. Server applies SAFE-T1201 redaction before storage.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"content":    map[string]interface{}{"type": "string"},
				"namespace":  map[string]interface{}{"type": "string"},
				"kind":       map[string]interface{}{"type": "string", "enum": []string{"fact", "summary", "checkpoint"}},
				"expires_at": map[string]interface{}{"type": "string", "description": "RFC3339"},
				"pin":        map[string]interface{}{"type": "boolean"},
			},
			"required": []string{"content"},
		},
	},
	{
		Name:        "search_memory",
		Description: "Search memories across one or more namespaces. Empty namespaces = search everything readable. Server applies ACL intersection before querying.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query":      map[string]interface{}{"type": "string"},
				"namespaces": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				"kinds":      map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "enum": []string{"fact", "summary", "checkpoint"}}},
				"limit":      map[string]interface{}{"type": "integer"},
			},
		},
	},
	{
		Name:        "commit_summary",
		Description: "Save an end-of-session summary. Same shape as commit_memory_v2 but kind=summary and a 30-day default TTL.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"content":    map[string]interface{}{"type": "string"},
				"namespace":  map[string]interface{}{"type": "string"},
				"expires_at": map[string]interface{}{"type": "string"},
			},
			"required": []string{"content"},
		},
	},
	{
		Name:        "list_writable_namespaces",
		Description: "List the namespaces this workspace can write to.",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "list_readable_namespaces",
		Description: "List the namespaces this workspace can read from.",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "forget_memory",
		Description: "Delete a memory by id. Only memories in namespaces you can write to can be forgotten.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"memory_id": map[string]interface{}{"type": "string"},
				"namespace": map[string]interface{}{"type": "string"},
			},
			"required": []string{"memory_id"},
		},
	},
}

// mcpToolList returns the filtered tool list for this MCP bridge.
// C3: send_message_to_user is excluded unless MOLECULE_MCP_ALLOW_SEND_MESSAGE=true.
func mcpToolList() []mcpTool {
	allowSend := os.Getenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE") == "true"
	var out []mcpTool
	for _, t := range mcpAllTools {
		if t.Name == "send_message_to_user" && !allowSend {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP handlers
// ─────────────────────────────────────────────────────────────────────────────

// Call handles POST /workspaces/:id/mcp — Streamable HTTP transport.
//
// Accepts a JSON-RPC 2.0 request and returns a JSON-RPC 2.0 response.
// WorkspaceAuth on the wsAuth group ensures the bearer token is valid for :id
// before this handler runs.
func (h *MCPHandler) Call(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var req mcpRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, mcpResponse{
			JSONRPC: "2.0",
			Error:   &mcpRPCError{Code: -32700, Message: "parse error: " + err.Error()},
		})
		return
	}

	resp := h.dispatchRPC(ctx, workspaceID, req)
	c.JSON(http.StatusOK, resp)
}

// Stream handles GET /workspaces/:id/mcp/stream — SSE transport (backwards compat).
//
// Implements the MCP 2024-11-05 SSE transport:
//  1. Sends an `endpoint` event pointing to the POST endpoint.
//  2. Keeps the connection alive with periodic ping comments.
//
// Clients should POST JSON-RPC requests to the endpoint URL returned in the
// event. The Streamable HTTP POST endpoint is the primary transport for new
// integrations.
func (h *MCPHandler) Stream(c *gin.Context) {
	workspaceID := c.Param("id")

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	// MCP 2024-11-05 SSE transport: the first event must be "endpoint" with
	// the URL clients should use for JSON-RPC POSTs.
	endpointURL := "/workspaces/" + workspaceID + "/mcp"
	fmt.Fprintf(c.Writer, "event: endpoint\ndata: %s\n\n", endpointURL)
	flusher.Flush()

	ctx := c.Request.Context()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprintf(c.Writer, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// JSON-RPC dispatch
// ─────────────────────────────────────────────────────────────────────────────

func (h *MCPHandler) dispatchRPC(ctx context.Context, workspaceID string, req mcpRequest) mcpResponse {
	base := mcpResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		base.Result = map[string]interface{}{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{"listChanged": false},
			},
			"serverInfo": map[string]string{
				"name":    "molecule-a2a",
				"version": "1.0.0",
			},
		}

	case "notifications/initialized":
		// No response required for notifications — return empty result.
		base.Result = nil

	case "tools/list":
		base.Result = map[string]interface{}{
			"tools": mcpToolList(),
		}

	case "tools/call":
		var params struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			base.Error = &mcpRPCError{Code: -32602, Message: "invalid params: " + err.Error()}
			return base
		}
		text, err := h.dispatch(ctx, workspaceID, params.Name, params.Arguments)
		if err != nil {
			base.Error = &mcpRPCError{Code: -32000, Message: err.Error()}
			return base
		}
		base.Result = map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": text},
			},
		}

	default:
		base.Error = &mcpRPCError{Code: -32601, Message: "method not found: " + req.Method}
	}

	return base
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool dispatch
// ─────────────────────────────────────────────────────────────────────────────

// Dispatch is the public entry point external code (tests, future
// out-of-package callers) uses to invoke a tool by name. Forwards
// to the unexported dispatch so existing in-package call sites
// stay unchanged.
func (h *MCPHandler) Dispatch(ctx context.Context, workspaceID, toolName string, args map[string]interface{}) (string, error) {
	return h.dispatch(ctx, workspaceID, toolName, args)
}

func (h *MCPHandler) dispatch(ctx context.Context, workspaceID, toolName string, args map[string]interface{}) (string, error) {
	switch toolName {
	case "list_peers":
		return h.toolListPeers(ctx, workspaceID)
	case "get_workspace_info":
		return h.toolGetWorkspaceInfo(ctx, workspaceID)
	case "delegate_task":
		return h.toolDelegateTask(ctx, workspaceID, args, mcpCallTimeout)
	case "delegate_task_async":
		return h.toolDelegateTaskAsync(ctx, workspaceID, args)
	case "check_task_status":
		return h.toolCheckTaskStatus(ctx, workspaceID, args)
	case "send_message_to_user":
		return h.toolSendMessageToUser(ctx, workspaceID, args)
	case "commit_memory":
		return h.toolCommitMemory(ctx, workspaceID, args)
	case "recall_memory":
		return h.toolRecallMemory(ctx, workspaceID, args)

	// v2 memory tools (RFC #2728). PR-6 will alias the legacy names to
	// these; until then they are independent surfaces.
	case "commit_memory_v2":
		return h.toolCommitMemoryV2(ctx, workspaceID, args)
	case "search_memory":
		return h.toolSearchMemory(ctx, workspaceID, args)
	case "commit_summary":
		return h.toolCommitSummary(ctx, workspaceID, args)
	case "list_writable_namespaces":
		return h.toolListWritableNamespaces(ctx, workspaceID, args)
	case "list_readable_namespaces":
		return h.toolListReadableNamespaces(ctx, workspaceID, args)
	case "forget_memory":
		return h.toolForgetMemory(ctx, workspaceID, args)

	default:
		return "", fmt.Errorf("unknown tool: %s", toolName)
	}
}
