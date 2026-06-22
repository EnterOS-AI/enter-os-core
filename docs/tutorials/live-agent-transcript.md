# Live Agent Session Transcript

> **Feature:** `GET /workspaces/:id/transcript` · **Handler:** `workspace-server/internal/handlers/transcript.go` · **PR:** enter-os-core#270

The transcript endpoint surfaces the live agent session log — every turn, tool call, and output — as a single JSON object. Use it to tail a running session, build an observability dashboard, or replay an agent's reasoning.

---

## What the endpoint returns

```
GET /workspaces/:id/transcript
Authorization: Bearer <workspace-token>
```

**Response shape:**

```json
{
  "runtime": "claude-code",
  "supported": true,
  "lines": [
    { "type": "user",        "content": "explain the migration" },
    { "type": "agent",      "content": "Let me check the schema first." },
    { "type": "tool_call",  "content": "GET /db/schema/tables" },
    { "type": "tool_result","content": "12 tables found" },
    { "type": "agent",      "content": "There are 12 tables. Starting with..." }
  ],
  "cursor": 5,
  "more": false
}
```

| Field | Type | Description |
|---|---|---|
| `runtime` | string | Agent runtime name (e.g. `claude-code`) |
| `supported` | bool | `false` means the workspace runtime doesn't expose a transcript |
| `lines` | array | Session log entries in chronological order |
| `cursor` | int | Number of entries — use as `?since=<cursor>` to fetch only new lines |
| `more` | bool | `true` if there are older entries beyond what was returned |

---

## Basic: curl a transcript

```bash
# Get the full current transcript
curl -s https://platform.example.com/workspaces/$WORKSPACE_ID/transcript \
  -H "Authorization: Bearer $WORKSPACE_TOKEN" | jq .
```

**Sample output:**

```json
{
  "runtime": "claude-code",
  "supported": true,
  "lines": [
    { "type": "user", "content": "review PR #1439" },
    { "type": "agent", "content": "I'll start by fetching the PR diff." }
  ],
  "cursor": 2,
  "more": false
}
```

---

## Filter to new lines only (`since` param)

Use `?since=<cursor>` to fetch only entries added since your last poll. This is the building block for any live dashboard or tail command.

```bash
# Poll every 2 seconds for new lines
CURSOR=0
WORKSPACE_ID="ws-abc123"
PLATFORM="https://platform.example.com"
TOKEN="$WORKSPACE_TOKEN"

while true; do
  RESPONSE=$(curl -s "$PLATFORM/workspaces/$WORKSPACE_ID/transcript?since=$CURSOR" \
    -H "Authorization: Bearer $TOKEN")

  NEW_LINES=$(echo "$RESPONSE" | jq -c '.lines')
  CURSOR=$(echo "$RESPONSE" | jq '.cursor')
  MORE=$(echo "$RESPONSE" | jq '.more')

  if [ "$NEW_LINES" != "[]" ]; then
    echo "$NEW_LINES" | jq -r '.[] | "[\(.type)] \(.content)"'
  fi

  # Stop polling if session ended (more=false and lines are complete)
  if [ "$MORE" = "false" ] && [ "$NEW_LINES" = "[]" ]; then
    echo "Session ended."
    break
  fi

  sleep 2
done
```

---

## Limit output size (`limit` param)

The endpoint caps response at 1 MB. For very long sessions, use `?limit=N` to fetch entries in chunks:

```bash
# Fetch last 50 entries
curl -s "https://platform.example.com/workspaces/$WORKSPACE_ID/transcript?limit=50" \
  -H "Authorization: Bearer $WORKSPACE_TOKEN" | jq '.lines[-50:]'
```

---

## Python: async consumer for a live dashboard

```python
import asyncio, httpx, json

PLATFORM  = "https://platform.example.com"
WORKSPACE  = "ws-abc123"   # your workspace ID
TOKEN      = "ws_tok_..."  # workspace-scoped bearer token

async def tail_transcript():
    cursor = 0
    async with httpx.AsyncClient(timeout=30.0) as client:
        while True:
            resp = await client.get(
                f"{PLATFORM}/workspaces/{WORKSPACE}/transcript",
                params={"since": cursor},
                headers={"Authorization": f"Bearer {TOKEN}"},
            )
            resp.raise_for_status()
            data = resp.json()

            # Print each new line as it arrives
            for line in data["lines"]:
                ts = line.get("type", "").upper().ljust(12)
                content = line.get("content", "")
                print(f"[{ts}] {content}")

            if not data["supported"]:
                print("[transcript] runtime does not support transcript streaming")
                break

            if not data["more"] and not data["lines"]:
                # Session still active but no new lines — just re-poll
                await asyncio.sleep(2)
                continue

            # Update cursor to the last seen entry + 1
            cursor = data["cursor"]

            if not data["more"]:
                print("[transcript] session ended cleanly")
                break

            # Small delay before next poll to avoid tight loop
            await asyncio.sleep(1)

if __name__ == "__main__":
    asyncio.run(tail_transcript())
```

**Install dependencies:**

```bash
pip install httpx
```

---

## Parse into structured events

The `type` field lets you filter by event type for richer dashboards:

```python
def format_line(line: dict) -> str:
    t = line.get("type", "")
    c = line.get("content", "")

    if t == "tool_call":
        return f"  🔧 TOOL: {c}"
    if t == "tool_result":
        return f"  → RESULT: {c[:80]}{'...' if len(c) > 80 else ''}"
    if t == "agent":
        return f"  🤖 {c[:120]}{'...' if len(c) > 120 else ''}"
    if t == "user":
        return f"  👤 {c}"
    return f"  ? {c}"

for line in lines:
    print(format_line(line))
```

**Sample formatted output:**

```
  👤 review PR #1439
  🤖 I'll start by fetching the PR diff.
  🔧 TOOL: GET /db/schema/tables
  → RESULT: 12 tables found
  🤖 There are 12 tables in the schema...
```

---

## Embed in an observability dashboard

The transcript pairs with Canvas and the audit log for full observability:

| Signal | What it gives you | Endpoint |
|---|---|---|
| **Live session log** | Every turn and tool call, streaming | `GET /workspaces/:id/transcript` |
| **Memory state** | What the agent knows | `GET /workspaces/:id/memories` |
| **Audit trail** | Who did what, when, with which key | `GET /admin/orgs/:id/audit-logs` |

```python
# Fetch both transcript and memory state in parallel
async def workspace_snapshot():
    async with httpx.AsyncClient() as client:
        transcript, memories = await asyncio.gather(
            client.get(f"{PLATFORM}/workspaces/{WORKSPACE}/transcript",
                       headers={"Authorization": f"Bearer {TOKEN}"}),
            client.get(f"{PLATFORM}/workspaces/{WORKSPACE}/memories",
                       params={"scope": "LOCAL"},
                       headers={"Authorization": f"Bearer {TOKEN}"}),
        )
        return {
            "session_log": transcript.json()["lines"],
            "memory_entries": memories.json()["entries"],
            "cursor": transcript.json()["cursor"],
        }
```

---

## Error codes

| Status | Meaning |
|---|---|
| `200 OK` | Transcript returned |
| `400 Bad Request` | Invalid workspace ID or query params |
| `401 Unauthorized` | Missing or invalid bearer token |
| `404 Not Found` | Workspace does not exist |
| `502 Bad Gateway` | Workspace agent unreachable (offline or crashed) |
| `503 Service Unavailable` | Workspace registered but has no agent URL on file |

---

## Security notes

- The platform validates the workspace URL (from `agent_card->>'url'`) before proxying to prevent SSRF — blocklist covers cloud metadata IPs, link-local addresses, and non-HTTP schemes.
- The bearer token is forwarded to the workspace endpoint. Workspace auth (PRs #287, #328) secures it.
- Response is capped at 1 MB to prevent a runaway session from saturating the caller.

---

## Related

- [Memory Inspector Panel](https://docs.moleculeai.app/docs/blog/memory-inspector-panel) — what the agent knows during the session
- [Audit Trail API](../blog/audit-chain-verification) — who accessed what
- [Workspace Runtime](../agent-runtime/workspace-runtime.md) — runtime environment model
- `workspace-server/internal/handlers/transcript.go` — handler source