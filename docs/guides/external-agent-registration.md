# External Agent Registration Guide

> **In a hurry?** The [External Workspace 5-Minute Quickstart](./external-workspace-quickstart.md) gets you from zero to a live agent on canvas in under 5 minutes. This guide is the comprehensive reference — auth, capabilities, production hardening — for when you need the full picture.

## Overview

An **external agent** (also called a remote agent) is any AI agent that runs
outside the EnterOS platform's Docker network but participates in the
canvas, communicates with other agents via the A2A protocol, and is managed as
a first-class workspace.

Use cases for external agents:

- **Existing infrastructure** -- you already run an agent on your own servers
  and want it to join a EnterOS org without re-deploying inside Docker.
- **Different cloud / region** -- the agent runs on AWS, GCP, Azure, or
  another provider while the platform runs elsewhere.
- **Edge devices** -- agents running on-premises or on edge hardware that
  cannot be containerized by the platform.
- **Third-party services** -- SaaS bots or webhook-driven services that
  expose an A2A-compatible HTTP endpoint.
- **Development / debugging** -- run an agent locally on your laptop while
  pointing it at a shared platform instance.

External workspaces behave identically to platform-provisioned workspaces in
every way except two: the platform does not start or stop a Docker container
for them, and liveness is tracked exclusively through the heartbeat TTL (the
Docker health sweep is skipped).

---

## Prerequisites

| Requirement | Details |
|-------------|---------|
| Running EnterOS platform | Default `http://localhost:8080`. Set `NEXT_PUBLIC_PLATFORM_URL` in canvas accordingly. |
| Publicly reachable HTTP endpoint | Your agent must accept POST requests for incoming A2A messages. If you are behind NAT, use a tunnel (ngrok, Cloudflare Tunnel, etc.). |
| Bearer token storage | The platform issues a 256-bit auth token on first registration. You must persist it -- it is shown only once and cannot be recovered. |

---

## Step-by-Step Registration

### 1. Create an External Workspace

```bash
curl -X POST http://localhost:8080/workspaces \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <admin-token>" \
  -d '{
    "name": "My External Agent",
    "role": "researcher",
    "runtime": "external",
    "external": true,
    "url": "https://my-agent.example.com",
    "tier": 2,
    "parent_id": null
  }'
```

**Response:**

```json
{
  "id": "a1b2c3d4-...",
  "status": "online",
  "external": true
}
```

Notes:

- `POST /workspaces` requires `AdminAuth` (bearer token) when any live admin
  token exists on the platform. On a fresh install with no tokens, it
  bootstraps open.
- `external: true` tells the platform to skip Docker provisioning. The
  workspace goes straight to `online` if a URL is provided.
- `url` is the publicly reachable endpoint where your agent accepts A2A
  messages. It must be an HTTPS or HTTP URL.
- `runtime` should be `"external"` -- the canvas renders a purple "REMOTE"
  badge for this runtime value.
- `tier` defaults to `1` if omitted. Tier has no resource-limit effect on
  external workspaces (no container), but it is stored for organizational
  display.
- `parent_id` is optional. Set it to nest this workspace under an existing
  team/parent workspace.

Save the returned `id` -- you will need it for every subsequent call.

### 2. Register with the Platform

```bash
curl -X POST http://localhost:8080/registry/register \
  -H "Content-Type: application/json" \
  -d '{
    "id": "<workspace-id-from-step-1>",
    "url": "https://my-agent.example.com",
    "agent_card": {
      "name": "My External Agent",
      "description": "Handles research tasks and summarization",
      "skills": ["research", "summarization", "analysis"],
      "runtime": "external"
    }
  }'
```

**Response (first registration):**

```json
{
  "status": "registered",
  "auth_token": "mol_abc123...very-long-token"
}
```

**Critical:** The `auth_token` field is present **only on first
registration**. It is never returned again. Store it securely (environment
variable, secrets manager, etc.). All subsequent authenticated calls require
this token.

On re-registration (e.g., after your agent restarts), the response contains
only `{"status": "registered"}` -- the original token remains valid.

### 3. Start the Heartbeat Loop

Your agent must send a heartbeat every **30 seconds** to stay online. If the
platform receives no heartbeat for 60 seconds, the workspace transitions to
`offline`.

```bash
curl -X POST http://localhost:8080/registry/heartbeat \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth_token>" \
  -d '{
    "workspace_id": "<workspace-id>",
    "error_rate": 0.0,
    "active_tasks": 0,
    "current_task": "",
    "uptime_seconds": 3600,
    "sample_error": ""
  }'
```

**Response:**

```json
{ "status": "ok" }
```

Heartbeat fields:

| Field | Type | Description |
|-------|------|-------------|
| `workspace_id` | string | Required. Your workspace ID. |
| `error_rate` | float | 0.0 -- 1.0. If > 0.5, workspace enters `degraded` status on the canvas. |
| `active_tasks` | int | Number of tasks currently running. Displayed in the canvas node. |
| `current_task` | string | Human-readable description of current work. Shown in the workspace detail panel. |
| `uptime_seconds` | int | Seconds since your agent started. |
| `sample_error` | string | Most recent error message, if any. Visible in monitoring. |

### 4. Handle Incoming A2A Messages

Your agent must accept `POST` requests at the URL you registered. The
platform (and other agents) send messages in A2A JSON-RPC format:

```json
{
  "jsonrpc": "2.0",
  "method": "message/send",
  "params": {
    "message": {
      "role": "user",
      "parts": [
        { "type": "text", "text": "Research the latest trends in AI safety" }
      ]
    },
    "metadata": {
      "source": "agent",
      "history": []
    }
  },
  "id": "req-abc-123"
}
```

Your endpoint must return a JSON-RPC response:

```json
{
  "jsonrpc": "2.0",
  "result": {
    "message": {
      "role": "agent",
      "parts": [
        { "type": "text", "text": "Here are the latest AI safety trends..." }
      ]
    }
  },
  "id": "req-abc-123"
}
```

For errors, return a JSON-RPC error object:

```json
{
  "jsonrpc": "2.0",
  "error": {
    "code": -32603,
    "message": "Internal error: model rate limited"
  },
  "id": "req-abc-123"
}
```

### 5. Send Messages to Other Agents

Use the A2A proxy to communicate with any workspace your agent is allowed to
reach:

```bash
curl -X POST http://localhost:8080/workspaces/<target-workspace-id>/a2a \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth_token>" \
  -H "X-Workspace-ID: <your-workspace-id>" \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "parts": [
          { "type": "text", "text": "Can you help with this task?" }
        ]
      },
      "metadata": {}
    },
    "id": "req-456"
  }'
```

Both headers are required:

- `Authorization: Bearer <token>` -- your workspace auth token.
- `X-Workspace-ID: <your-id>` -- identifies which workspace is making the
  call. The platform uses this to enforce communication rules.

### 6. Discover Peers

Find out which other workspaces your agent can communicate with:

```bash
# Get your workspace's own info
curl http://localhost:8080/registry/discover/<your-workspace-id> \
  -H "Authorization: Bearer <auth_token>" \
  -H "X-Workspace-ID: <your-workspace-id>"

# List peers (siblings + parent + children)
curl http://localhost:8080/registry/<your-workspace-id>/peers \
  -H "Authorization: Bearer <auth_token>" \
  -H "X-Workspace-ID: <your-workspace-id>"
```

The peers endpoint returns workspaces that your agent is allowed to
communicate with, based on the hierarchy rules below.

---

## Communication Rules

The platform enforces strict hierarchy-based access control via
`CanCommunicate(callerID, targetID)`:

| Relationship | Allowed |
|---|---|
| Same workspace (self-call) | Yes |
| Siblings (same `parent_id`) | Yes |
| Root-level siblings (both `parent_id` is NULL) | Yes |
| Parent to child | Yes |
| Child to parent | Yes |
| Everything else | **Denied** |

Canvas requests (no `X-Workspace-ID` header) and system callers
(`webhook:*`, `system:*`, `test:*` prefixes) bypass this check.

---

## Multiple Workspaces From One Local MCP Bridge

The standalone runtime package includes `molecule-mcp`, a local MCP bridge for
external agents such as Claude Code, Codex, Hermes, and other tools that run
outside the platform container fleet. One local bridge can serve multiple
external workspaces by setting `MOLECULE_WORKSPACES`:

```json
[
  {
    "id": "workspace-id-local-to-hongming-org",
    "token": "...",
    "platform_url": "https://hongming.moleculesai.app"
  },
  {
    "id": "different-workspace-id-local-to-agents-team-org",
    "token": "...",
    "platform_url": "https://agents-team.moleculesai.app"
  }
]
```

`platform_url` is the tenant routing key. The bridge registers, heartbeats,
polls inboxes, and sends outbound A2A calls against the URL attached to the
workspace that is doing the work.

Do not add `org_id` to this config. The tenant already comes from
`platform_url`, and the bearer token is issued by that tenant. Workspace IDs
also do not need to be shared across orgs; each tenant can return its own
workspace ID and token for the same local agent process.

---

## Canvas Appearance

External workspaces appear on the canvas with a purple **REMOTE** badge
instead of the usual runtime pill (e.g., "LANGGRAPH", "CLAUDE-CODE").

They support all standard canvas features:

- Drag-and-drop positioning (persisted via `PATCH /workspaces/:id`)
- Nesting into team nodes (set `parent_id` on create or move via API)
- Real-time status updates -- heartbeat data (active tasks, current task,
  error rate) is broadcast to canvas clients via WebSocket
- Chat panel -- "My Chat" tab sends A2A messages to the agent; "Agent Comms"
  tab shows inter-agent traffic
- Config and secrets management via the detail panel

---

## Example: Python Implementation

A minimal external agent using `requests` and `flask`:

```python
"""
Minimal EnterOS external agent.

pip install flask requests
"""

import os
import sys
import time
import threading
import requests
from flask import Flask, request, jsonify

PLATFORM_URL = os.environ.get("PLATFORM_URL", "http://localhost:8080")
AGENT_URL = os.environ.get("AGENT_URL")  # e.g. "https://my-agent.ngrok.io"
ADMIN_TOKEN = os.environ.get("ADMIN_TOKEN", "")  # for POST /workspaces
AGENT_NAME = os.environ.get("AGENT_NAME", "Python External Agent")

app = Flask(__name__)

# --- State ---
workspace_id = None
auth_token = None
start_time = time.time()


# --- Step 4: Handle incoming A2A messages ---
@app.route("/", methods=["POST"])
def handle_a2a():
    payload = request.get_json(force=True)
    method = payload.get("method", "")
    req_id = payload.get("id", "unknown")

    if method == "message/send":
        text = ""
        parts = (
            payload.get("params", {}).get("message", {}).get("parts", [])
        )
        for part in parts:
            if part.get("type") == "text":
                text += part.get("text", "")

        # --- Your agent logic here ---
        reply = f"Received: {text}. Processing complete."

        return jsonify({
            "jsonrpc": "2.0",
            "result": {
                "message": {
                    "role": "agent",
                    "parts": [{"type": "text", "text": reply}],
                }
            },
            "id": req_id,
        })

    return jsonify({
        "jsonrpc": "2.0",
        "error": {"code": -32601, "message": f"Unknown method: {method}"},
        "id": req_id,
    })


# --- Step 3: Heartbeat loop ---
def heartbeat_loop():
    while True:
        try:
            requests.post(
                f"{PLATFORM_URL}/registry/heartbeat",
                json={
                    "workspace_id": workspace_id,
                    "error_rate": 0.0,
                    "active_tasks": 0,
                    "current_task": "",
                    "uptime_seconds": int(time.time() - start_time),
                },
                headers={"Authorization": f"Bearer {auth_token}"},
                timeout=10,
            )
        except Exception as e:
            print(f"Heartbeat failed: {e}", file=sys.stderr)
        time.sleep(30)


def register():
    global workspace_id, auth_token

    if not AGENT_URL:
        print("ERROR: Set AGENT_URL to your publicly reachable endpoint", file=sys.stderr)
        sys.exit(1)

    # Step 1: Create external workspace
    headers = {"Content-Type": "application/json"}
    if ADMIN_TOKEN:
        headers["Authorization"] = f"Bearer {ADMIN_TOKEN}"

    resp = requests.post(
        f"{PLATFORM_URL}/workspaces",
        json={
            "name": AGENT_NAME,
            "runtime": "external",
            "external": True,
            "url": AGENT_URL,
            "tier": 2,
        },
        headers=headers,
        timeout=10,
    )
    resp.raise_for_status()
    workspace_id = resp.json()["id"]
    print(f"Created workspace: {workspace_id}")

    # Step 2: Register with the platform
    resp = requests.post(
        f"{PLATFORM_URL}/registry/register",
        json={
            "id": workspace_id,
            "url": AGENT_URL,
            "agent_card": {
                "name": AGENT_NAME,
                "description": "A minimal Python external agent",
                "skills": ["echo"],
                "runtime": "external",
            },
        },
        timeout=10,
    )
    resp.raise_for_status()
    data = resp.json()
    auth_token = data.get("auth_token")
    if auth_token:
        print(f"Auth token received (save this!): {auth_token[:12]}...")
    else:
        print("No new token issued (re-registration). Use your saved token.")
        auth_token = os.environ.get("AUTH_TOKEN")
        if not auth_token:
            print("ERROR: Set AUTH_TOKEN for re-registration", file=sys.stderr)
            sys.exit(1)

    # Start heartbeat
    t = threading.Thread(target=heartbeat_loop, daemon=True)
    t.start()
    print("Heartbeat loop started (every 30s)")


if __name__ == "__main__":
    register()
    print(f"Listening on {AGENT_URL}")
    app.run(host="0.0.0.0", port=int(os.environ.get("PORT", 5000)))
```

Run it:

```bash
export PLATFORM_URL=http://localhost:8080
export AGENT_URL=https://my-agent.ngrok.io
export ADMIN_TOKEN=your-admin-bearer-token
python external_agent.py
```

---

## Example: Node.js Implementation

A minimal external agent using the built-in `fetch` and `express`:

```javascript
/**
 * Minimal EnterOS external agent.
 *
 * npm install express
 * Node.js 18+ required (native fetch).
 */

const express = require("express");

const PLATFORM_URL = process.env.PLATFORM_URL || "http://localhost:8080";
const AGENT_URL = process.env.AGENT_URL; // e.g. "https://my-agent.ngrok.io"
const ADMIN_TOKEN = process.env.ADMIN_TOKEN || "";
const AGENT_NAME = process.env.AGENT_NAME || "Node External Agent";
const PORT = parseInt(process.env.PORT || "5000", 10);

let workspaceId = null;
let authToken = null;
const startTime = Date.now();

// --- Step 4: Handle incoming A2A messages ---
const app = express();
app.use(express.json());

app.post("/", (req, res) => {
  const { method, id: reqId, params } = req.body;

  if (method === "message/send") {
    const parts = params?.message?.parts || [];
    const text = parts
      .filter((p) => p.type === "text")
      .map((p) => p.text)
      .join("");

    // --- Your agent logic here ---
    const reply = `Received: ${text}. Processing complete.`;

    return res.json({
      jsonrpc: "2.0",
      result: {
        message: {
          role: "agent",
          parts: [{ type: "text", text: reply }],
        },
      },
      id: reqId,
    });
  }

  res.json({
    jsonrpc: "2.0",
    error: { code: -32601, message: `Unknown method: ${method}` },
    id: reqId,
  });
});

// --- Step 3: Heartbeat loop ---
function startHeartbeat() {
  setInterval(async () => {
    try {
      await fetch(`${PLATFORM_URL}/registry/heartbeat`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${authToken}`,
        },
        body: JSON.stringify({
          workspace_id: workspaceId,
          error_rate: 0.0,
          active_tasks: 0,
          current_task: "",
          uptime_seconds: Math.floor((Date.now() - startTime) / 1000),
        }),
      });
    } catch (err) {
      console.error("Heartbeat failed:", err.message);
    }
  }, 30_000);
}

// --- Steps 1 & 2: Create + register ---
async function register() {
  if (!AGENT_URL) {
    console.error("ERROR: Set AGENT_URL to your publicly reachable endpoint");
    process.exit(1);
  }

  // Step 1: Create external workspace
  const headers = { "Content-Type": "application/json" };
  if (ADMIN_TOKEN) headers["Authorization"] = `Bearer ${ADMIN_TOKEN}`;

  const createResp = await fetch(`${PLATFORM_URL}/workspaces`, {
    method: "POST",
    headers,
    body: JSON.stringify({
      name: AGENT_NAME,
      runtime: "external",
      external: true,
      url: AGENT_URL,
      tier: 2,
    }),
  });
  if (!createResp.ok) throw new Error(`Create failed: ${createResp.status}`);
  const createData = await createResp.json();
  workspaceId = createData.id;
  console.log(`Created workspace: ${workspaceId}`);

  // Step 2: Register
  const regResp = await fetch(`${PLATFORM_URL}/registry/register`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      id: workspaceId,
      url: AGENT_URL,
      agent_card: {
        name: AGENT_NAME,
        description: "A minimal Node.js external agent",
        skills: ["echo"],
        runtime: "external",
      },
    }),
  });
  if (!regResp.ok) throw new Error(`Register failed: ${regResp.status}`);
  const regData = await regResp.json();

  authToken = regData.auth_token;
  if (authToken) {
    console.log(`Auth token received (save this!): ${authToken.slice(0, 12)}...`);
  } else {
    console.log("No new token issued (re-registration). Using saved token.");
    authToken = process.env.AUTH_TOKEN;
    if (!authToken) {
      console.error("ERROR: Set AUTH_TOKEN for re-registration");
      process.exit(1);
    }
  }

  startHeartbeat();
  console.log("Heartbeat loop started (every 30s)");
}

register().then(() => {
  app.listen(PORT, () => console.log(`Listening on port ${PORT}`));
});
```

Run it:

```bash
export PLATFORM_URL=http://localhost:8080
export AGENT_URL=https://my-agent.ngrok.io
export ADMIN_TOKEN=your-admin-bearer-token
node external_agent.js
```

---

## Lifecycle

External workspaces follow a simplified version of the standard lifecycle:

```
provisioning --> online (on create with URL, or on register)
                   |
                   v
              degraded (error_rate > 0.5 in heartbeat)
                   |
                   v
               online (error_rate recovers)
                   |
                   v
              offline (heartbeat TTL expires after 60s)
                   |
                   v
              removed (DELETE /workspaces/:id)
```

Key differences from platform-managed workspaces:

- **No Docker health sweep** -- external workspaces are invisible to the
  Docker API. Only the Redis heartbeat TTL determines liveness.
- **No auto-restart** -- when an external workspace goes offline, the
  platform does not attempt to restart it. Your agent is responsible for
  re-registering and resuming heartbeats.
- **Pause/resume** -- pausing an external workspace (`POST
  /workspaces/:id/pause`) sets status to `paused` and the heartbeat monitor
  skips it. Resuming (`POST /workspaces/:id/resume`) returns it to
  `provisioning`; your agent must re-register to go back online.

---

## Security

### Auth Token

- Tokens are 256-bit cryptographically random values.
- The raw token is returned **once** at first registration. The platform
  stores only the SHA-256 hash in the `workspace_auth_tokens` table.
- Lost tokens cannot be recovered. If you lose your token, delete the
  workspace and create a new one (or wait for a future token-reset API).
- Tokens are automatically revoked when a workspace is deleted.

### Required Headers

| Endpoint | Required Headers |
|----------|-----------------|
| `POST /registry/heartbeat` | `Authorization: Bearer <token>` |
| `POST /registry/update-card` | `Authorization: Bearer <token>` |
| `POST /workspaces/:target/a2a` | `Authorization: Bearer <token>`, `X-Workspace-ID: <your-id>` |
| `GET /registry/discover/:id` | `Authorization: Bearer <token>`, `X-Workspace-ID: <your-id>` |
| `GET /registry/:id/peers` | `Authorization: Bearer <token>`, `X-Workspace-ID: <your-id>` |

### Legacy / Bootstrap Behavior

Workspaces that registered before the token system existed (Phase 30.1) are
grandfathered -- their requests pass through without a token until their next
`/registry/register` call issues one. After that, the token is enforced on
every subsequent call.

---

## Updating Your Agent Card

If your agent's capabilities change, update the card without re-registering:

```bash
curl -X POST http://localhost:8080/registry/update-card \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth_token>" \
  -d '{
    "workspace_id": "<workspace-id>",
    "agent_card": {
      "name": "My External Agent v2",
      "description": "Now with summarization and translation",
      "skills": ["research", "summarization", "translation"],
      "runtime": "external"
    }
  }'
```

---

## Troubleshooting

### Workspace shows "offline" on the canvas

**Cause:** Heartbeat has not been received in the last 60 seconds.

**Fix:** Verify your heartbeat loop is running, sending to the correct
`workspace_id`, and including `Authorization: Bearer <token>`. Check network
connectivity between your agent and the platform. Inspect platform logs for
401 errors.

### 401 Unauthorized on heartbeat

**Cause:** Missing or invalid auth token.

**Fix:** Ensure you are sending the exact token returned at first
registration. Tokens are case-sensitive. If you lost the token, you must
delete the workspace and re-create it.

### Cannot send A2A messages (403 or communication denied)

**Cause:** The `CanCommunicate` check failed -- your workspace is not a
sibling, parent, or child of the target.

**Fix:** Check the hierarchy. Use `GET /registry/<your-id>/peers` to see
which workspaces you can reach. If needed, move your workspace under the
correct parent via `PATCH /workspaces/:id` with `{"parent_id": "..."}`.

### Agent card not showing on canvas

**Cause:** Registration was not called, or the `agent_card` JSON was
malformed.

**Fix:** Call `POST /registry/register` again with a valid `agent_card`
object. The `name`, `description`, and `skills` fields are used for display
on the canvas.

### Messages not reaching your agent

**Cause:** The URL you registered is not reachable from the platform.

**Fix:**
1. Confirm the URL is correct: `curl -X POST <your-url> -d '{}'`
2. If running locally, use a tunnel (ngrok, Cloudflare Tunnel) and register
   the tunnel URL.
3. Check that your agent's HTTP server is binding to `0.0.0.0`, not just
   `127.0.0.1`.
4. Verify firewall rules allow inbound traffic on your agent's port.

### Workspace stuck in "provisioning"

**Cause:** The `external: true` flag was not set on create, so the platform
tried to provision a Docker container.

**Fix:** Delete the workspace and re-create it with `"external": true` in the
payload.

### Re-registration does not return a token

**Expected behavior.** The token is issued only on first registration. On
re-registration, use the token you saved from the first time. If you need to
start fresh, delete the workspace and create a new one.

### Heartbeat succeeds but status stays "degraded"

**Cause:** Your heartbeat is reporting `error_rate` > 0.5.

**Fix:** Lower the `error_rate` field in your heartbeat payload. The
workspace recovers to `online` automatically once the rate drops below 0.5.
