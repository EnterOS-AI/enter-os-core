package handlers

// external_connection.go — copy-paste connection payload shown once to
// the operator when they create a runtime="external" workspace.
//
// The canvas UI surfaces these in a single modal so the operator can
// hand the block to whoever runs their external agent without having
// to piece together workspace_id + platform_url + auth_token + API
// shape from the docs. curl snippet has zero dependencies; Python
// snippet pairs with molecule-sdk-python's A2AServer + RemoteAgentClient.

import (
	"os"

	"github.com/gin-gonic/gin"
)

// externalPlatformURL returns the public URL at which this workspace-
// server instance is reachable by the operator's external agent. This
// is NOT necessarily the caller's Host header (which could be an
// internal CF tunnel hostname). Prefer the EXTERNAL_PLATFORM_URL env
// that Railway/ops sets for the tenant; fall back to the request's
// Host + scheme if unset.
func externalPlatformURL(c *gin.Context) string {
	if v := os.Getenv("EXTERNAL_PLATFORM_URL"); v != "" {
		return v
	}
	scheme := "https"
	if xf := c.Request.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	} else if c.Request.TLS == nil {
		scheme = "http"
	}
	host := c.Request.Host
	if xh := c.Request.Header.Get("X-Forwarded-Host"); xh != "" {
		host = xh
	}
	return scheme + "://" + host
}

// externalCurlTemplate — zero-dependency register snippet. Placeholders:
//   - {{PLATFORM_URL}}, {{WORKSPACE_ID}}   — filled server-side
//   - $WORKSPACE_AUTH_TOKEN                — env var, operator sets
//   - $AGENT_URL                           — env var, operator's public HTTPS endpoint
//
// SSRF filter rejects private IPs at register time, so AGENT_URL must
// resolve to a public host.
//
// Heartbeat loop is NOT included here — curl is fine for one-shot
// register; keeping the workspace alive wants a real loop, so point
// operators at the Python snippet for long-lived setups.
const externalCurlTemplate = `# Replace AGENT_URL with YOUR agent's public HTTPS endpoint, then run:
export WORKSPACE_AUTH_TOKEN="<paste from create response>"
export AGENT_URL="https://your-agent.example.com"

# NOTE on the "Origin" header below: hosted SaaS tenants run behind an
# edge WAF that requires same-origin requests. Without "Origin", paths
# like /workspaces/* silently 404 (rewritten to the canvas Next.js).
# /registry/register is currently allowed without Origin, but setting
# it preemptively keeps your snippet working if the WAF rules expand.
curl -fsS -X POST "{{PLATFORM_URL}}/registry/register" \
  -H "Authorization: Bearer $WORKSPACE_AUTH_TOKEN" \
  -H "Origin: {{PLATFORM_URL}}" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "{{WORKSPACE_ID}}",
    "url": "'"$AGENT_URL"'",
    "agent_card": {
      "name": "My External Agent",
      "description": "",
      "version": "0.1.0"
    }
  }'
`

// externalChannelTemplate — Claude Code channel plugin install + .env. For
// operators whose external agent IS a Claude Code session (laptop or
// remote dev VM); routes the workspace's A2A traffic into the running
// Claude Code session as conversation turns via MCP. The plugin source
// lives at github.com/Molecule-AI/molecule-mcp-claude-channel — polling
// based, no tunnel required (uses /workspaces/:id/activity?since_secs=,
// platform-side support shipped in #2300).
const externalChannelTemplate = `# Claude Code channel — bridges this workspace's A2A traffic into your
# Claude Code session. No tunnel/public URL needed (polling-based).
#
# 1. Save this token + workspace_id, then create ~/.claude/channels/molecule/.env:
mkdir -p ~/.claude/channels/molecule
cat > ~/.claude/channels/molecule/.env <<'EOF'
MOLECULE_PLATFORM_URL={{PLATFORM_URL}}
MOLECULE_WORKSPACE_IDS={{WORKSPACE_ID}}
MOLECULE_WORKSPACE_TOKENS=<paste auth_token from create response>
EOF
chmod 600 ~/.claude/channels/molecule/.env

# 2. Launch Claude Code with the channel enabled:
claude --channels plugin:molecule@Molecule-AI/molecule-mcp-claude-channel

# Inbound A2A messages now surface as conversation turns. Claude's
# replies route back via the reply_to_workspace MCP tool — no extra
# wiring on your side.
#
# Multi-workspace: comma-separate IDs and tokens (same order). See
# https://github.com/Molecule-AI/molecule-mcp-claude-channel for
# pairing flow, push-mode upgrade, and v0.2 roadmap.
`

// externalUniversalMcpTemplate — runtime-agnostic standalone path.
// Ships as the `molecule-mcp` console script in the
// molecule-ai-workspace-runtime PyPI wheel (workspace/mcp_cli.py).
// Any MCP-aware runtime (Claude Code, hermes, codex, third-party)
// registers it once and gets the same 8 universal tools that
// container-bound runtimes use today: delegate_task, list_peers,
// send_message_to_user, commit_memory, etc.
//
// Standalone: the binary itself handles register-on-startup +
// continuous heartbeats (daemon thread, 20s cadence). No separate
// SDK or channel process needed to keep the workspace online. The
// only thing it does NOT yet do is poll inbound A2A messages — for
// runtimes that need their agent to react to canvas messages or
// peer-initiated tasks, pair with the Claude Code channel tab
// (poll-based inbound delivery into a Claude Code session) or the
// Python SDK tab (push-mode inbound + heartbeat).
//
// Origin/WAF: handled automatically by platform_auth.auth_headers()
// in the wheel — operator doesn't need to configure anything.
const externalUniversalMcpTemplate = `# Universal MCP — standalone register + heartbeat + outbound platform tools
# for any MCP-aware runtime (Claude Code, hermes, codex, etc.).
# Pair with the Claude Code or Python SDK tab if your runtime needs
# inbound A2A delivery (canvas messages → agent conversation turns).

# 1. Install the workspace runtime wheel:
pip install molecule-ai-workspace-runtime

# 2. Wire molecule-mcp into your agent's MCP config. Claude Code:
claude mcp add molecule -s user -- env \
  WORKSPACE_ID={{WORKSPACE_ID}} \
  PLATFORM_URL={{PLATFORM_URL}} \
  MOLECULE_WORKSPACE_TOKEN="<paste from create response>" \
  molecule-mcp

# molecule-mcp registers the workspace + heartbeats every 20s in a
# daemon thread, then runs the MCP stdio loop. Same env-var contract
# works with hermes-agent, codex, or any MCP stdio runtime. Tools
# exposed: delegate_task, delegate_task_async, check_task_status,
# list_peers, get_workspace_info, send_message_to_user,
# commit_memory, recall_memory.
#
# Origin/WAF handling is built into the wheel — no manual headers
# needed when calling tools through the MCP server.
`

// externalPythonTemplate uses molecule-sdk-python's RemoteAgentClient +
// A2AServer (PR #13 in that repo). Until the SDK cuts a v0.y release
// to PyPI the snippet pins git+main.
const externalPythonTemplate = `# pip install 'git+https://github.com/Molecule-AI/molecule-sdk-python.git@main'

import asyncio
from molecule_agent import RemoteAgentClient, A2AServer

WORKSPACE_ID  = "{{WORKSPACE_ID}}"
PLATFORM_URL  = "{{PLATFORM_URL}}"
AUTH_TOKEN    = "<paste from create response>"
INBOUND_URL   = "https://your-agent.example.com/a2a/inbound"  # your public HTTPS endpoint

async def handle(request: dict) -> dict:
    # request has parts, message, task_id, idempotency_key
    text = "".join(p.get("text", "") for p in request.get("parts", []) if p.get("type") == "text")
    return {"parts": [{"type": "text", "text": f"echo: {text}"}]}

async def main():
    client = RemoteAgentClient(
        workspace_id=WORKSPACE_ID,
        platform_url=PLATFORM_URL,
        auth_token=AUTH_TOKEN,
    )
    server = A2AServer(
        agent_id=client.workspace_id,
        inbound_url=INBOUND_URL,
        message_handler=handle,
    )
    server.start_in_background()
    client.reported_url = INBOUND_URL
    client.register()                         # one-shot announcement
    await client.run_heartbeat_loop_async()   # keeps the workspace online

if __name__ == "__main__":
    asyncio.run(main())
`
