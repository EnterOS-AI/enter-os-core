package handlers

// external_connection.go — copy-paste connection payload shown once to
// the operator when they create a runtime="external" workspace.
//
// The canvas UI surfaces these in a single modal so the operator can
// hand the block to whoever runs their external agent without having
// to piece together workspace_id + platform_url + auth_token + API
// shape from the docs. curl snippet has zero dependencies; Python
// snippet pairs with molecule-sdk-python's A2AServer + RemoteAgentClient.
//
// BuildExternalConnectionPayload (below) is the single source of truth
// for the payload shape — used by Create (#workspace.go), Rotate
// (#external_rotate.go), and the read-only "show instructions again"
// endpoint. Adding a snippet means adding it here once; the three
// callers pick it up automatically.

import (
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// BuildExternalConnectionPayload assembles the gin.H payload that the
// canvas's ExternalConnectModal consumes. Pure data — caller owns DB
// reads (workspace_id) and token minting (auth_token).
//
// authToken may be empty for the read-only "show instructions again"
// path; the modal masks the field in that case rather than displaying
// an empty string.
func BuildExternalConnectionPayload(platformURL, workspaceID, authToken string) gin.H {
	pURL := strings.TrimSuffix(platformURL, "/")
	stamp := func(tmpl string) string {
		return strings.ReplaceAll(
			strings.ReplaceAll(tmpl, "{{PLATFORM_URL}}", pURL),
			"{{WORKSPACE_ID}}", workspaceID,
		)
	}
	return gin.H{
		"workspace_id":                workspaceID,
		"platform_url":                pURL,
		"auth_token":                  authToken,
		"registry_endpoint":           pURL + "/registry/register",
		"heartbeat_endpoint":          pURL + "/registry/heartbeat",
		"curl_register_template":      stamp(externalCurlTemplate),
		"python_snippet":              stamp(externalPythonTemplate),
		"claude_code_channel_snippet": stamp(externalChannelTemplate),
		"universal_mcp_snippet":       stamp(externalUniversalMcpTemplate),
		"hermes_channel_snippet":      stamp(externalHermesChannelTemplate),
		"codex_snippet":               stamp(externalCodexTemplate),
		"openclaw_snippet":            stamp(externalOpenClawTemplate),
	}
}

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

# Need help?
#   Documentation: https://doc.moleculesai.app/docs/guides/external-agent-registration
#   Common errors:
#     • 401 / 403 on register — WORKSPACE_AUTH_TOKEN must be the value
#       shown at workspace create. Tokens are shown only once.
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
# Prereq: Bun installed (channel plugins are Bun scripts).
#   bun --version    # must print a version number
#
# 1. Inside Claude Code, install the channel plugin from its GitHub repo.
#    The plugin is NOT on Anthropic's default allowlist, so a one-time
#    marketplace-add is needed before install:
#
#      /plugin marketplace add Molecule-AI/molecule-mcp-claude-channel
#      /plugin install molecule@molecule-mcp-claude-channel
#
#    Then either run /reload-plugins or restart Claude Code so the
#    plugin is registered.
#
# 2. Create the per-watched-workspace config file:
mkdir -p ~/.claude/channels/molecule
cat > ~/.claude/channels/molecule/.env <<'EOF'
MOLECULE_PLATFORM_URL={{PLATFORM_URL}}
MOLECULE_WORKSPACE_IDS={{WORKSPACE_ID}}
MOLECULE_WORKSPACE_TOKENS=<paste auth_token from create response>
EOF
chmod 600 ~/.claude/channels/molecule/.env

# 3. Launch Claude Code with the channel enabled. Custom (non-Anthropic-
#    allowlisted) channels need the --dangerously-load-development-channels
#    flag to opt in — without it, you'll see "not on the approved channels
#    allowlist" on startup.
claude --dangerously-load-development-channels \
  --channels plugin:molecule@molecule-mcp-claude-channel

# You should see on stderr:
#   molecule channel: connected — watching 1 workspace(s) at {{PLATFORM_URL}}
#
# Inbound A2A messages now surface as conversation turns. Claude's
# replies route back via the reply_to_workspace MCP tool — no extra
# wiring on your side.
#
# Common errors:
#   "plugin not installed"            → Step 1 didn't run; run /plugin install
#                                       inside Claude Code, then /reload-plugins.
#   "not on approved channels allowlist" → Add --dangerously-load-development-channels
#                                       to the launch command (Step 3).
#   "config-missing"                  → ~/.claude/channels/molecule/.env not
#                                       readable; re-run Step 2 and check chmod.
#
# Team/Enterprise orgs: the --dangerously-load-development-channels flag is
# blocked by managed settings. Your admin must set channelsEnabled=true and
# add the plugin to allowedChannelPlugins in claude.ai admin settings.
#
# Multi-workspace: comma-separate IDs and tokens (same order). See
# https://github.com/Molecule-AI/molecule-mcp-claude-channel for
# pairing flow, push-mode upgrade, and v0.2 roadmap.

# Need help?
#   Documentation: https://doc.moleculesai.app/docs/guides/claude-code-channel-plugin
#   Common errors:
#     • "plugin not installed" — run /plugin marketplace add then
#       /plugin install lines above; /reload-plugins or restart.
#     • "not on the approved channels allowlist" — custom channels need
#       --dangerously-load-development-channels; team/enterprise orgs
#       need admin to set channelsEnabled + allowedChannelPlugins.
#     • "Inbound messages not arriving" — stderr should show
#       "molecule channel: connected — watching N workspace(s)";
#       verify ~/.claude/channels/molecule/.env has PLATFORM_URL + token.
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

# Requires Python >= 3.11. On 3.10 or older pip says
# "Could not find a version that satisfies the requirement
# (from versions: none)" — the wheel's requires_python pin filters
# the only available artifact before pip even attempts install.
# Upgrade the interpreter (brew install python@3.12 / apt install
# python3.12 / etc.) or use a 3.11+ venv.

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

# Need help?
#   Where to install: https://pypi.org/project/molecule-ai-workspace-runtime/
#   Documentation: https://doc.moleculesai.app/docs/guides/mcp-server-setup
#   Common errors:
#     • "Tools not appearing in your agent" — run ` + "`claude mcp list`" + ` (or
#       your runtime's equivalent) and confirm the molecule entry. If
#       missing, re-run the ` + "`claude mcp add`" + ` line above.
#     • "ConnectionRefused / DNS error on first call" — PLATFORM_URL must
#       include the scheme (https://) and have NO trailing slash. Verify
#       with: curl ${PLATFORM_URL}/healthz
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

# Need help?
#   Where to install: https://pypi.org/project/molecule-ai-workspace-runtime/
#   Documentation: https://doc.moleculesai.app/docs/guides/external-agent-registration
#   Common errors:
#     • 401 from /heartbeat — AUTH_TOKEN expired or wrong workspace_id.
#       Tokens shown only once at create time; re-create to get a fresh one.
#     • AGENT_URL not reachable from platform — public HTTPS URL required
#       for inbound A2A. Use ngrok or Cloudflare Tunnel if behind NAT.
`

// externalHermesChannelTemplate — install snippet for operators whose
// external agent IS a hermes-agent session. Routes the workspace's
// A2A traffic into the running hermes gateway as platform messages
// via the molecule-channel plugin.
//
// The plugin (Molecule-AI/hermes-channel-molecule) is a hermes
// platform adapter that:
//   1. Spawns ``python -m molecule_runtime.a2a_mcp_server`` as a
//      stdio MCP subprocess (separate from any hermes-side MCP
//      client connection).
//   2. Long-polls ``wait_for_message`` on the platform's inbox.
//   3. Dispatches each inbound activity into the hermes gateway as a
//      MessageEvent — same code path Telegram/Discord use.
//   4. Outbound replies route via ``send_message_to_user`` (canvas
//      user) or ``delegate_task`` (peer agent) MCP tool calls.
//
// Result: hermes gets push parity with Claude Code / codex / openclaw —
// canvas messages and peer A2A arrive as conversation turns mid-session,
// not just at the start of a new ``hermes`` invocation.
//
// Plugin uses the upstream ``register_platform`` API shipped by
// NousResearch/hermes-agent#17751 (merged 2026-04-30) and falls back
// to the legacy ``register_platform_adapter`` shape on older forks —
// same wheel installs cleanly on stock or patched hermes-agent.
const externalHermesChannelTemplate = `# Hermes channel — bridges this workspace's A2A traffic into your
# hermes-agent session. No tunnel/public URL needed (long-poll based,
# same shape as the Claude Code channel).
#
# Prereq: a hermes-agent install on the target machine. Latest builds
# (post #17751) ship the platform-plugin API natively; older ones are
# also supported via the plugin's dual-mode fallback.
#
# 1. Install the runtime + plugin:
pip install molecule-ai-workspace-runtime
pip install 'git+https://github.com/Molecule-AI/hermes-channel-molecule.git'

# 2. Export the workspace credentials:
export MOLECULE_WORKSPACE_ID={{WORKSPACE_ID}}
export MOLECULE_PLATFORM_URL={{PLATFORM_URL}}
export MOLECULE_WORKSPACE_TOKEN="<paste from create response>"

# 3. Edit ~/.hermes/config.yaml — under your existing top-level
#    gateway: block, add a plugin_platforms entry:
#
#      gateway:
#        # ...your existing gateway settings...
#        plugin_platforms:
#          molecule:
#            enabled: true
#
#    If you don't yet have a gateway: block, create one with just
#    that plugin_platforms entry. Don't append blindly — YAML
#    rejects duplicate top-level keys, so a second gateway: block
#    will silently break hermes config loading.

# 4. Restart the hermes gateway:
hermes gateway --replace

# Inbound canvas messages + peer A2A now arrive as MessageEvents —
# same dispatch path Telegram/Discord/Slack use. The agent replies via
# send_message_to_user / delegate_task MCP tool calls (already wired
# by the plugin's molecule_runtime MCP subprocess).
#
# Source + issue tracker:
# https://github.com/Molecule-AI/hermes-channel-molecule

# Need help?
#   Documentation: https://doc.moleculesai.app/docs/guides/external-agent-registration
#   Common errors:
#     • Gateway start failure — tail ~/.hermes/gateway.log. YAML
#       duplicate-key in config.yaml is the most common cause; the
#       gateway: block must appear exactly once.
#     • Plugin not discovered after install — pip show hermes-channel-molecule
#       to confirm install. Some hermes builds need ` + "`hermes plugin reload`" + `
#       before the new platform_plugins entry takes effect.
`

// externalCodexTemplate — for operators whose external agent is a
// codex CLI (@openai/codex) session. Wires the molecule_runtime A2A
// MCP server into codex's config.toml so the agent can call
// list_peers / delegate_task / send_message_to_user / commit_memory,
// AND surfaces the codex-channel-molecule bridge daemon for inbound
// push parity.
//
// Push parity:
//   - Outbound (codex calls platform tools) — works via the wired
//     MCP server (step 2 below).
//   - Inbound (canvas messages and peer-initiated tasks wake the
//     codex agent) — works via codex-channel-molecule (step 3),
//     which long-polls the platform inbox and runs `codex exec
//     --resume <session>` per inbound message. Each turn is a fresh
//     subprocess but per-thread session continuity is preserved on
//     disk so conversation context survives.
//
// Long-term: when openai/codex#17543 lands (codex MCP runtime routes
// inbound notifications/* into the active session as Op::UserInput),
// the bridge daemon becomes redundant — the wired MCP server in
// step 2 will deliver push natively. Until then, run both.
const externalCodexTemplate = `# Codex external setup — outbound tools (MCP) + inbound push (bridge).
# For operators whose external agent is a codex CLI (@openai/codex)
# session.

# 1. Install codex CLI, the workspace runtime, and the bridge daemon:
npm install -g @openai/codex@latest
pip install molecule-ai-workspace-runtime
pip install codex-channel-molecule

# 2. Wire the molecule MCP server into codex's config.toml — this is
#    the OUTBOUND path (codex calls list_peers / delegate_task /
#    send_message_to_user / commit_memory).
#
#    Don't append blindly — TOML rejects duplicate
#    [mcp_servers.molecule] tables, so re-running on an existing
#    config will break codex parsing. If [mcp_servers.molecule]
#    already exists (e.g. you set this up before), replace the
#    existing block instead of appending.

mkdir -p ~/.codex
# (then open ~/.codex/config.toml in your editor and paste:)
#
# [mcp_servers.molecule]
# command = "molecule-mcp"
# args = []
# startup_timeout_sec = 30
#
# [mcp_servers.molecule.env]
# WORKSPACE_ID = "{{WORKSPACE_ID}}"
# PLATFORM_URL = "{{PLATFORM_URL}}"
# MOLECULE_WORKSPACE_TOKEN = "<paste from create response>"
#
# Use the "molecule-mcp" console-script wrapper (NOT
# "python3 -m molecule_runtime.a2a_mcp_server"). The wrapper is what
# keeps the workspace ALIVE on the canvas: it POSTs /registry/register
# at startup and runs a 20s heartbeat thread alongside the MCP stdio
# loop. The bare a2a_mcp_server module exposes tools but does NOT
# heartbeat — pointing codex at it leaves the canvas showing this
# workspace as awaiting_agent (OFFLINE) within 60-90s even while
# tools work.

# 3. Run the bridge daemon as a durable background process — this
#    is the INBOUND path. Long-polls the platform inbox and runs
#    "codex exec --resume <session>" per inbound canvas/peer message,
#    routes the assistant reply back via send_message_to_user /
#    delegate_task. Per-thread session continuity persisted to
#    ~/.codex-channel-molecule/sessions.json so conversation context
#    survives daemon restarts.
#
#    Same env-var contract as the MCP server above.
#
#    Without this daemon, codex still works for outbound calls but
#    canvas messages won't wake an idle session — codex's MCP runtime
#    doesn't yet route notifications/* into the chat loop (tracked
#    upstream at openai/codex#17543; when that lands, the bridge
#    becomes redundant).

WORKSPACE_ID="{{WORKSPACE_ID}}" \
PLATFORM_URL="{{PLATFORM_URL}}" \
MOLECULE_WORKSPACE_TOKEN="<paste from create response>" \
nohup codex-channel-molecule > ~/.codex-channel-molecule/daemon.log 2>&1 &
disown

# 4. Run codex itself for interactive use — molecule tools are
#    available to the agent, and the bridge wakes a non-interactive
#    codex turn for any inbound canvas/peer message:
codex

# Need help?
#   Documentation: https://doc.moleculesai.app/docs/guides/mcp-server-setup
#   Common errors:
#     • [mcp_servers.molecule] not loaded — codex must be ≥ 0.57.
#       Check with ` + "`codex --version`" + `; upgrade via npm install -g @openai/codex@latest.
#     • TOML parse error after re-running setup — TOML rejects duplicate
#       [mcp_servers.molecule] tables. Open ~/.codex/config.toml and
#       remove the old block before pasting the new one.
#     • Canvas messages don't wake codex — step 3 (codex-channel-molecule
#       bridge daemon) is required for inbound push. Check
#       pgrep -f codex-channel-molecule and tail ~/.codex-channel-molecule/daemon.log.
`

// externalOpenClawTemplate — for operators whose external agent is an
// openclaw session. Wires the molecule MCP server via openclaw's
// `mcp set` config + starts the openclaw gateway on loopback.
//
// Like the codex tab, this is outbound-only. Full push parity on an
// external openclaw would need a sessions.steer bridge daemon (the
// equivalent of hermes-channel-molecule for openclaw). Tracked
// separately; outbound tools is the first cut.
const externalOpenClawTemplate = `# OpenClaw MCP config — outbound tool path. For operators whose
# external agent is an openclaw session.
#
# This wires the molecule platform's A2A MCP server into openclaw's
# gateway so the agent can call list_peers / delegate_task /
# send_message_to_user / commit_memory. Inbound A2A push into a
# running openclaw run is not wired here yet — the platform-side
# openclaw template (template-openclaw) implements the full
# sessions.steer push path; an external setup would need the same
# bridge daemon the template uses. For inbound delivery on an
# external machine today, pair with the Python SDK tab.

# 1. Install openclaw CLI + the workspace runtime wheel:
npm install -g openclaw@latest
pip install molecule-ai-workspace-runtime

# 2. Onboard openclaw against your model provider (one-time setup).
#    --non-interactive needs an explicit --provider + --model so it
#    doesn't prompt; pick what matches your API key. Skip step 2 if
#    you've already onboarded on this host.
#
#    openclaw onboard --non-interactive \
#      --provider openai \
#      --model gpt-5

# 3. Wire the molecule MCP server. {{WORKSPACE_ID}} + {{PLATFORM_URL}}
# are stamped server-side; paste the auth token before running.
#
# Use the "molecule-mcp" console-script wrapper (NOT
# "python3 -m molecule_runtime.a2a_mcp_server"). The wrapper is what
# keeps the workspace ALIVE on the canvas: it POSTs /registry/register
# at startup and runs a 20s heartbeat thread alongside the MCP stdio
# loop. The bare a2a_mcp_server module exposes tools but does NOT
# heartbeat — pointing openclaw at it leaves the canvas showing this
# workspace as awaiting_agent (OFFLINE) within 60-90s even while
# tools work.
WORKSPACE_TOKEN="<paste from create response>"
openclaw mcp set molecule "$(cat <<EOF
{
  "command": "molecule-mcp",
  "args": [],
  "env": {
    "WORKSPACE_ID": "{{WORKSPACE_ID}}",
    "PLATFORM_URL": "{{PLATFORM_URL}}",
    "MOLECULE_WORKSPACE_TOKEN": "$WORKSPACE_TOKEN"
  }
}
EOF
)"

# 4. Start the openclaw gateway as a durable background process.
#    A bare '&' dies when the terminal closes; nohup + log file keeps
#    the gateway alive across logout. For systemd-managed hosts,
#    register a unit instead.
nohup openclaw gateway --dev --port 18789 --bind loopback \
  > ~/.openclaw/gateway.log 2>&1 &
disown

# 5. Run an agent turn — molecule tools are now available:
openclaw agent --message "list my peers"

# Need help?
#   Documentation: https://doc.moleculesai.app/docs/guides/mcp-server-setup
#   Common errors:
#     • Gateway not starting — tail ~/.openclaw/gateway.log. The loopback
#       bind requires :18789 to be free; check with ` + "`lsof -iTCP:18789`" + `.
#     • ` + "`openclaw mcp set`" + ` rejected — the heredoc generates JSON;
#       verify with ` + "`jq < ~/.openclaw/mcp/molecule.json`" + ` and re-run
#       ` + "`openclaw mcp set`" + ` if the file is malformed.
`
