# External Workspace — 5-Minute Quickstart

Run an agent on your laptop, a home server, a cloud VM, or any machine with internet — and have it show up on a Molecule AI canvas alongside platform-provisioned agents. This guide gets you from zero to a working agent in under 5 minutes.

> **Looking for the operator-focused reference?** See [External Agent Registration](./external-agent-registration.md) for full capability + auth details, or [Remote Workspaces FAQ](./remote-workspaces-faq.md) for hardening + production notes. This doc is the fast path.

---

## What is an "external workspace"?

A workspace whose agent code lives outside Molecule's infrastructure. The platform treats it as a first-class participant — canvas node, A2A routing, delegation, memory, channels — but doesn't manage its lifecycle (no Docker, no EC2 launched for you).

You're responsible for:
1. Running an HTTP server that speaks A2A JSON-RPC
2. Exposing it at a URL the platform can reach
3. Registering it with your tenant

Everything else — message routing, canvas rendering, peer discovery, memory access — works the same as a platform-native agent.

---

## Prerequisites

| You need | Notes |
|---|---|
| A Molecule AI tenant | Your own hosted instance (e.g. `you.moleculesai.app`) or self-hosted |
| Tenant admin token | Available in the admin UI, or via `molecli ws list` |
| Outbound HTTPS | No inbound ports needed if you use a tunnel (next step) |
| Any language with an HTTP server | Python / Node.js / Go / Rust — anything that can POST+GET JSON |

---

## Step 1 — Write the agent (Python example, ~40 lines)

```python
# agent.py
import time
from fastapi import FastAPI, Request

app = FastAPI()

@app.get("/health")
def health():
    return {"status": "ok"}

@app.post("/")
async def a2a(request: Request):
    body = await request.json()

    # Extract user text from A2A JSON-RPC message/send
    user_text = ""
    try:
        for part in body["params"]["message"]["parts"]:
            if part.get("kind") == "text":
                user_text = part["text"]
                break
    except (KeyError, TypeError):
        pass

    # Your logic goes here — echo for now
    reply = f"You said: {user_text}"

    return {
        "jsonrpc": "2.0",
        "id": body.get("id"),
        "result": {
            "kind": "message",
            "messageId": f"agent-{int(time.time() * 1000)}",
            "role": "agent",
            "parts": [{"kind": "text", "text": reply}],
        },
    }
```

```bash
pip install fastapi uvicorn
uvicorn agent:app --host 127.0.0.1 --port 9876
```

Test locally:
```bash
curl -X POST http://127.0.0.1:9876/ \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"message/send","id":"1","params":{"message":{"role":"user","messageId":"m1","parts":[{"kind":"text","text":"hello"}]}}}'
```

Should return a JSON body with `"text":"You said: hello"`.

---

## Step 2 — Expose it to the internet

Pick one:

### Option A — Cloudflare quick tunnel (no account, ephemeral)
```bash
cloudflared tunnel --url http://127.0.0.1:9876
```
Copy the printed `https://*.trycloudflare.com` URL. Regenerates on every restart; fine for demos.

### Option B — ngrok (account, persistent during session)
```bash
ngrok http 9876
```

### Option C — Real server with TLS
Deploy the same Python script to a VM (Fly, Railway, DigitalOcean, anywhere) behind a TLS terminator (Caddy, nginx, or the platform's native TLS).

---

## Step 3 — Register the workspace

Replace `<TENANT>`, `<ADMIN_TOKEN>`, `<ORG_ID>`, and `<YOUR_URL>` with your values.

```bash
curl -X POST https://<TENANT>/workspaces \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "X-Molecule-Org-Id: <ORG_ID>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "My Laptop Agent",
    "runtime": "external",
    "external": true,
    "url": "<YOUR_URL>",
    "tier": 2
  }'
```

Response:
```json
{"external":true,"id":"abc-123-...","status":"online"}
```

The `id` field is your workspace ID — remember it.

---

## Step 4 — Chat with it

1. Open your Molecule canvas at `https://<TENANT>`
2. You'll see a new workspace node named "My Laptop Agent" with status `online`
3. Click it → Chat tab → type "hello"
4. Watch your terminal's uvicorn log — you'll see the incoming POST
5. The reply appears in the canvas chat

🎉 **You have an external agent running on Molecule.** Everything from here is iteration on that agent's handler code.

---

## Common gotchas

| Problem | Fix |
|---|---|
| "Failed to send message — agent may be unreachable" | The tenant couldn't POST to your URL. Verify `curl https://<your-tunnel>/health` returns 200 from another machine. |
| Response takes > 30s | Canvas times out around 30s. Keep initial implementations simple. For long-running work, return a placeholder and use [polling mode](#next-step-polling-mode-preview) (once available). |
| Agent duplicated in chat | Known canvas bug where WebSocket + HTTP responses both render. Fixed in [PR #1517](https://git.moleculesai.app/molecule-ai/molecule-core/pull/1517). |
| Agent replies but canvas shows "Agent unreachable" | Check the tenant can reach your URL. Cloudflare quick tunnels rotate — the URL in your canvas may point at a dead tunnel after restart. |
| Getting 404 when POSTing to tenant | Add `X-Molecule-Org-Id` header. The tenant's security layer 404s unmatched origin requests by design. |

---

## What you can do from the agent

Your agent has the same capability surface as a platform-native one. From inside your handler you can make outbound calls to the tenant API:

```python
import httpx

TENANT = "https://you.moleculesai.app"
TOKEN = "..."  # your workspace_auth_token from registration

def call_peer(workspace_id: str, text: str) -> str:
    """Message another agent (parent, child, sibling)."""
    resp = httpx.post(
        f"{TENANT}/workspaces/{workspace_id}/a2a",
        headers={"Authorization": f"Bearer {TOKEN}"},
        json={
            "jsonrpc": "2.0",
            "method": "message/send",
            "id": "1",
            "params": {"message": {
                "role": "user", "messageId": "1",
                "parts": [{"kind": "text", "text": text}]
            }}
        },
        timeout=30,
    )
    return resp.json()["result"]["parts"][0]["text"]
```

Similarly available: `delegate_to_workspace`, `commit_memory`, `search_memory`, `request_approval`, `peers`, `discover`. See the [A2A protocol reference](../api-protocol/communication-rules.md) for the full endpoint list.

---

## Production upgrade path

The quickstart leaves you with an ephemeral demo. For real use:

1. **Deploy to a real host**: Fly Machine / Railway / anywhere with a stable URL + TLS.
2. **Use a named Cloudflare tunnel**: survives restarts, gets you a consistent subdomain.
3. **Authenticate outbound calls correctly**: store the `workspace_auth_token` (returned when you register via `/registry/register`; see the [full registration doc](./external-agent-registration.md)) and send it as `Authorization: Bearer ...` on every outbound call to the tenant.
4. **Add an LLM**: swap the echo handler for `anthropic` / `openai` / `ollama` / your model of choice.
5. **Handle long-running work**: use the (upcoming) polling mode transport so you don't need a publicly reachable URL at all.

---

## Next step: polling mode (preview)

Push mode (this guide) works today but requires an inbound-reachable URL — which forces tunnels or public IPs. A polling-mode transport is in design:

```
[Canvas] --A2A--> [Platform] <--polls-- [Your laptop]
                  [inbox queue]     -->replies
```

Your agent makes only outbound HTTPS calls to the platform, pulling messages from an inbox queue and posting replies back. Works behind any NAT/firewall, tolerates offline laptops, no tunnel needed.

See the [design doc](https://git.moleculesai.app/molecule-ai/internal/src/branch/main/product/external-workspaces-polling.md) (internal) and the implementation tracking issue (search `polling+mode` on the [molecule-core issue tracker](https://git.moleculesai.app/molecule-ai/molecule-core/issues)).

---

## Examples

- **This quickstart's code**: [gist](https://gist.github.com/molecule-ai/external-workspace-quickstart) (forked for your language of choice)
- **LLM-backed example**: `molecule-ai/examples/external-claude-agent` — a working agent that proxies to Anthropic's API
- **Scheduled cron example**: `molecule-ai/examples/external-cron-agent` — fires timed outbound messages without needing inbound

---

## Troubleshooting

Run this diagnostic checklist before filing an issue:

```bash
# 1. Is your agent serving locally?
curl http://127.0.0.1:9876/health

# 2. Is the tunnel up?
curl https://<your-tunnel-url>/health

# 3. Can the tenant reach you? (from tenant shell or your laptop)
curl -X POST https://<your-tunnel-url>/ \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"message/send","id":"x","params":{"message":{"role":"user","messageId":"m","parts":[{"kind":"text","text":"hi"}]}}}'

# 4. Is the workspace registered correctly?
curl -H "Authorization: Bearer <ADMIN_TOKEN>" -H "X-Molecule-Org-Id: <ORG_ID>" \
     https://<TENANT>/workspaces/<WS_ID>
```

If all four pass and canvas still shows your agent as unreachable, see the [remote workspaces FAQ](./remote-workspaces-faq.md).

---

## Feedback

This is a new path. Tell us what broke:
- Open an issue: https://git.moleculesai.app/molecule-ai/molecule-core/issues/new?labels=external-workspace
- Join #external-workspaces on our Slack
- Submit a PR improving this doc if something tripped you up — the faster we can make the quickstart, the more developers we bring in

---

*Last updated 2026-04-21*
