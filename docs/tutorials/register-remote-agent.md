# Register a Remote Agent on Molecule AI

Remote agents let you connect AI agents running on *any* infrastructure — your laptop, a cloud VM, a CI/CD pipeline, or an on-premise server — to a single Molecule AI canvas. Your agent keeps running wherever it lives; the canvas gives you fleet-wide visibility, secret management, and cross-network A2A messaging from one place.

This tutorial walks through the full registration flow: creating an external workspace, obtaining a bearer token, setting up the heartbeat, and verifying the agent appears on your canvas.

> **Prerequisites:** A running Molecule AI platform (self-hosted or cloud), `ADMIN_TOKEN` (or an org-scoped key with admin scope), and an agent binary that can make HTTP calls.

## How remote agents work

Molecule AI's remote agent system has three parts:

1. **External workspace** — a workspace record with `runtime: "external"` and `external: true`. It holds metadata (agent name, URL, agent card) but does not provision a container.
2. **Bearer token** — the credential your remote agent uses to authenticate to the platform on every call. Issued once at registration; stored by the agent.
3. **Heartbeat loop** — the agent sends a `POST /registry/heartbeat` every 30 seconds to stay visible on the canvas.

```
Your infra (laptop / VM / CI)          Molecule AI Platform
         │                                     │
         │  POST /workspaces  (create external workspace)
         │────────────────────────────────────►│
         │                                     │
         │  POST /registry/register  (get bearer token)
         │────────────────────────────────────►│
         │  ← auth_token
         │                                     │
         │  POST /registry/heartbeat  (every 30s)
         │────────────────────────────────────►│ Canvas shows purple REMOTE badge
         │                                     │
         │  GET /secrets  (fetch workspace secrets)
         │  POST /a2a     (A2A messaging)
         │────────────────────────────────────►│
```

## Step-by-step registration

### Step 1: Create an external workspace

```bash
ADMIN_TOKEN="your-admin-token-or-org-key"
PLATFORM_URL="https://platform.moleculesai.app"
AGENT_URL="https://your-agent.example.com"  # must be reachable from the platform

WORKSPACE=$(curl -s -X POST "${PLATFORM_URL}/workspaces" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "CI Agent",
    "runtime": "external",
    "external": true,
    "url": "https://your-agent.example.com"
  }')

WORKSPACE_ID=$(echo $WORKSPACE | jq -r '.id')
echo "Workspace ID: ${WORKSPACE_ID}"
```

The `runtime: "external"` flag tells the platform this workspace is agent-managed, not container-provisioned. The `url` field is the address the platform uses to reach your agent (for A2A routing and health checks).

Save the workspace ID — you'll use it in the next step.

### Step 2: Register the agent and receive a bearer token

```bash
REG=$(curl -s -X POST "${PLATFORM_URL}/registry/register" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{
    \"id\": \"${WORKSPACE_ID}\",
    \"url\": \"https://your-agent.example.com\",
    \"agent_card\": {
      \"name\": \"CI Agent\",
      \"runtime\": \"external\",
      \"version\": \"1.0\"
    }
  }")

AUTH_TOKEN=$(echo $REG | jq -r '.auth_token')
echo "Auth token: ${AUTH_TOKEN}"

# IMPORTANT: the auth_token is shown once. Store it securely.
# If lost, revoke and re-register.
```

The response looks like:

```json
{
  "auth_token": "rtok_01HZX... truncated ...",
  "workspace_id": "ws_01HZX...",
  "org_id": "org_01HZX...",
  "expires_at": null
}
```

Store `auth_token` in your agent's environment — **it's shown only once**. If you lose it, create a new external workspace and re-register.

### Step 3: Pull secrets on demand

Your agent fetches workspace secrets via the platform API using its bearer token. Secrets are never injected as environment variables for remote agents — the agent pulls them explicitly:

```bash
curl -s "${PLATFORM_URL}/workspaces/${WORKSPACE_ID}/secrets" \
  -H "Authorization: Bearer ${AUTH_TOKEN}"
```

```json
{
  "secrets": {
    "OPENAI_API_KEY": "sk-...",
    "GITHUB_TOKEN": "ghs_..."
  }
}
```

This keeps secrets out of environment blocks and allows rotation without restarting the agent. Call this on agent boot and re-call whenever your agent refreshes its credential cache.

### Step 4: Start the heartbeat loop

The heartbeat keeps your agent visible on the canvas and reports runtime state to the platform. Send it every **30 seconds**:

```python
import requests, time

AUTH_TOKEN = "rtok_01HZX..."
WORKSPACE_ID = "ws_01HZX..."
PLATFORM_URL = "https://platform.moleculesai.app"

while True:
    resp = requests.post(
        f"{PLATFORM_URL}/registry/heartbeat",
        headers={"Authorization": f"Bearer {AUTH_TOKEN}"},
        json={
            "workspace_id": WORKSPACE_ID,
            "active_tasks": 0,          # number of tasks currently being processed
            "current_task": None,       # optional: short description of what the agent is doing
            "uptime_seconds": 0,        # optional: seconds since agent started
            "error_rate": 0.0,         # optional: fraction of requests that errored in the last period
            "runtime_state": "idle",    # one of: idle, working, paused, error
        },
    )
    if resp.status_code != 200:
        print(f"Heartbeat failed: {resp.status_code} {resp.text}")
    time.sleep(30)
```

If the platform misses three consecutive heartbeats (90 seconds), it marks the agent as `offline` on the canvas. The agent can resume by sending a heartbeat at any time — the canvas updates immediately.

> **Tip:** Use the SDK's `run_heartbeat_loop()` method instead of writing the loop manually. It handles the timing and includes an optional `task_supplier` callable so the heartbeat reports live `active_tasks` and `current_task` automatically:
>
> ```python
> from molecule_agent import RemoteAgentClient
>
> client = RemoteAgentClient(
>     platform_url=PLATFORM_URL,
>     workspace_id=WORKSPACE_ID,
>     auth_token=AUTH_TOKEN,
> )
>
> def task_status():
>     return {"active_tasks": client.get_active_task_count(), "current_task": client.get_current_task_name()}
>
> client.run_heartbeat_loop(task_supplier=task_status)
> ```

### Step 5: Send and receive A2A messages

Remote agents use the standard A2A protocol. Use the SDK's `fetch_inbound()` method to poll for inbound tasks:

```python
from molecule_agent import RemoteAgentClient

client = RemoteAgentClient(
    platform_url=PLATFORM_URL,
    workspace_id=WORKSPACE_ID,
    auth_token=AUTH_TOKEN,
)

# Poll for inbound tasks (call this in your agent's main loop)
task = client.fetch_inbound(timeout_seconds=30)
if task:
    print(f"Received task: {task}")
    # Process task and send response via client.send_result(...)
```

The SDK handles the `X-Workspace-ID` header automatically. Remote agents send from their own workspace; orchestrators can address specific agents by workspace ID.

### Step 6: Verify the agent appears on the canvas

Open your Molecule AI canvas, navigate to **Workspaces**, and look for your agent. Remote agents show a **purple REMOTE badge** next to their name so you can distinguish them from container-provisioned workspaces at a glance.

If the badge is grey instead of purple, the heartbeat is not reaching the platform. Check:
- The agent's outbound HTTPS can reach `platform.moleculesai.app`
- The heartbeat loop is running and not crashing silently
- The `auth_token` matches the workspace ID

## Agent code: minimal Python example

Here's a minimal agent that registers, starts the heartbeat, and can receive A2A tasks:

```python
import requests, time, threading, json

PLATFORM_URL = "https://platform.moleculesai.app"
ADMIN_TOKEN = "your-admin-token"  # used only during registration
AGENT_URL = "https://your-agent.example.com"  # must be HTTPS and reachable

# Step 1: Create external workspace
workspace = requests.post(
    f"{PLATFORM_URL}/workspaces",
    headers={"Authorization": f"Bearer {ADMIN_TOKEN}"},
    json={"name": "CI Agent", "runtime": "external", "external": True, "url": AGENT_URL},
).json()
WORKSPACE_ID = workspace["id"]

# Step 2: Register and get bearer token
reg = requests.post(
    f"{PLATFORM_URL}/registry/register",
    headers={"Authorization": f"Bearer {ADMIN_TOKEN}"},
    json={
        "id": WORKSPACE_ID,
        "url": AGENT_URL,
        "agent_card": {"name": "CI Agent", "runtime": "external"},
    },
).json()
AUTH_TOKEN = reg["auth_token"]

# Step 3: Fetch secrets on boot
secrets = requests.get(
    f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/secrets",
    headers={"Authorization": f"Bearer {AUTH_TOKEN}"},
).json()
# Store secrets in your agent's credential store

# Step 4: Heartbeat loop (runs in background)
def heartbeat_loop():
    while True:
        requests.post(
            f"{PLATFORM_URL}/registry/heartbeat",
            headers={"Authorization": f"Bearer {AUTH_TOKEN}"},
            json={"workspace_id": WORKSPACE_ID},
        )
        time.sleep(30)

threading.Thread(target=heartbeat_loop, daemon=True).start()

# Step 5: Poll for A2A tasks
print(f"Registered. Workspace ID: {WORKSPACE_ID}")
print("Heartbeat running in background.")
```

## Self-hosted agents

For agents on private networks or air-gapped infrastructure, the platform must be able to reach `AGENT_URL` for A2A delivery. If your agent is behind a NAT or firewall:

- Use a tunnel (Cloudflare Tunnel, ngrok, frp) to expose the agent on a public HTTPS URL
- Ensure the URL resolves and the agent's HTTP server handles `POST /a2a` requests
- Check that your firewall allows outbound HTTPS to `PLATFORM_URL`

For air-gapped deployments without internet access, contact your Molecule AI sales team for on-premise deployment options.

## Revoking and re-registering

To rotate the agent's bearer token:

1. **Revoke the workspace** (canvas UI or `DELETE /workspaces/{id}`) — this invalidates the current token
2. Re-run Step 1 and Step 2 above with a new workspace name
3. Update your agent's `AUTH_TOKEN` with the new value

To revoke without deleting the workspace record, use `DELETE /workspaces/{id}/tokens` if your platform version supports it.

## Remote agents vs. Docker workspaces

| | Remote Agent | Docker Workspace |
|---|---|---|
| Infrastructure | Your own (laptop, VM, bare metal) | Platform-provisioned containers |
| Token issuance | Manual via `/registry/register` | Automatic on container boot |
| Secrets | Pulled on demand via API | Injected as env vars at startup |
| Heartbeat | Your code sends it every 30s | Platform sends it from the container |
| Canvas badge | Purple REMOTE | Standard (no badge) |
| Tear-down | Revoke token + stop agent | `DELETE /workspaces/{id}` |
| Best for | CI/CD agents, laptops, on-prem | Cloud VMs managed by the platform |

## What's next

- [Agent Card reference](../agent-runtime/agent-card.md) — publish your agent's capabilities so orchestrators can discover and route tasks
- [A2A protocol reference](../api-protocol/a2a-protocol.md) — full message format, error codes, and streaming
- [Registry and heartbeat reference](../api-protocol/registry-and-heartbeat.md) — heartbeat interval, offline detection, and error handling
- [Remote workspaces blog post](../blog/2026-04-20-remote-workspaces/index.md) — the product announcement with fleet visibility context

> **Molecule AI is open source.** Remote agent support is in `molecule-core/registry/` on `main`.