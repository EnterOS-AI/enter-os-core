# Tool Trace + Platform Instructions Demo

Two platform-level features merged in PR #1686:

- **Tool Trace** — every A2A response includes a `tool_trace` list in `Message.metadata`, stored in `activity_logs.tool_trace` JSONB. Verifies agent claims ("I checked X") against actual tool calls.
- **Platform Instructions** — admin-configurable instruction text (global/workspace scope) injected into every agent's system prompt at startup and periodically refreshed.

This demo covers all four scenarios in ~90 seconds.

---

## Prerequisites

```bash
# Platform URL and workspace token from environment
PLATFORM_URL="${PLATFORM_URL:-https://platform.molecule.ai}"
WORKSPACE_TOKEN="${MOLECULE_WORKSPACE_TOKEN}"
```

---

## Scenario 1: Admin creates a global instruction (API)

Admin creates a global instruction that applies to all workspaces. The token is the platform admin token.

```bash
curl -s -X POST "$PLATFORM_URL/instructions" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "scope": "global",
    "title": "No shell commands in user-facing agents",
    "content": "Agents must NOT execute shell commands for users. Use file read/write tools or MCP tools only. Shell commands are only permitted in internal provisioning scripts.",
    "priority": 10
  }' | jq .
```

**Expected response:**
```json
{
  "id": "a1b2c3d4-...",
  "scope": "global",
  "title": "No shell commands in user-facing agents",
  "content": "...",
  "priority": 10,
  "enabled": true,
  "created_at": "2026-04-23T12:00:00Z",
  "updated_at": "2026-04-23T12:00:00Z"
}
```

---

## Scenario 2: Admin creates a workspace-scoped instruction

Admin targets an instruction at a specific workspace — used to enforce per-workspace operational rules.

```bash
WORKSPACE_ID="your-workspace-id"
curl -s -X POST "$PLATFORM_URL/instructions" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"scope\": \"workspace\",
    \"scope_target\": \"$WORKSPACE_ID\",
    \"title\": \"Use dark theme by default\",
    \"content\": \"When generating UI components, default to the dark theme unless the user explicitly requests light mode. Import styles from /styles/dark.css.\",
    \"priority\": 5
  }" | jq .
```

**Expected response:**
```json
{
  "id": "b2c3d4e5-...",
  "scope": "workspace",
  "scope_target": "your-workspace-id",
  "title": "Use dark theme by default",
  "priority": 5,
  "enabled": true,
  ...
}
```

---

## Scenario 3: Agent fetches its instruction set at startup

When a workspace boots, the runtime calls `GET /workspaces/:id/instructions/resolve` using the workspace token. The response is injected as the first section of the system prompt, ahead of all other content. The agent cannot override these instructions — they take highest precedence.

```bash
WORKSPACE_ID="your-workspace-id"
curl -s "$PLATFORM_URL/workspaces/$WORKSPACE_ID/instructions/resolve" \
  -H "X-Workspace-ID: $WORKSPACE_ID" \
  -H "Authorization: Bearer $MOLECULE_WORKSPACE_TOKEN" | jq .
```

**Expected response:**
```json
{
  "workspace_id": "your-workspace-id",
  "instructions": "# Platform Instructions\n\n> No shell commands in user-facing agents\n...\n> Use dark theme by default\n..."
}
```

The resolved `instructions` string is prepended directly to the system prompt in `workspace/prompt.py` (`get_platform_instructions()` → `build_system_prompt()` with `platform_instructions` parameter).

---

## Scenario 4: Admin lists all active instructions

```bash
curl -s "$PLATFORM_URL/instructions?scope=global" \
  -H "Authorization: Bearer $ADMIN_TOKEN" | jq .
```

**Expected response:**
```json
[
  {
    "id": "a1b2c3d4-...",
    "scope": "global",
    "title": "No shell commands in user-facing agents",
    "priority": 10,
    "enabled": true,
    ...
  }
]
```

---

## Scenario 5: Query activity logs with tool traces

After an A2A call, the platform stores `tool_trace` entries. Query a workspace's activity logs to see which tools an agent actually invoked — useful for debugging and compliance.

```bash
WORKSPACE_ID="your-workspace-id"
curl -s "$PLATFORM_URL/workspaces/$WORKSPACE_ID/activity?limit=5" \
  -H "Authorization: Bearer $ADMIN_TOKEN" | jq '.[] | {
    id, activity_type, created_at,
    tool_trace: .tool_trace | if . then . else null end
  }'
```

**Expected response:**
```json
[
  {
    "id": "log-123",
    "activity_type": "a2a_call",
    "created_at": "2026-04-23T12:01:00Z",
    "tool_trace": [
      {
        "tool": "mcp__files__read",
        "input": {"path": "config.yaml"},
        "output_preview": "api_version: v2, region: us-east-1, ..."
      },
      {
        "tool": "mcp__httpx__get",
        "input": {"url": "https://api.example.com/status"},
        "output_preview": "{\"status\": \"ok\", \"latency_ms\": 42}"
      }
    ]
  }
]
```

Each `tool_trace` entry records the tool name, the input arguments (sanitized), and a preview of the output (truncated at 200 chars). Parallel tool calls are captured via shared `run_id`.

---

## How it works

### Tool Trace

```
A2A request → agent executes tools → parallel run_id pairs start/end events
→ A2A response metadata.tool_trace = [{name, input, output_preview}, ...]
→ activity_logs INSERT with tool_trace JSONB column
→ admin queries /workspaces/:id/activity
```

Key code:
- `workspace-server/internal/handlers/activity.go` — stores + returns tool_trace
- `workspace-server/migrations/039_activity_tool_trace.up.sql` — adds column + GIN index
- `workspace/a2a_executor.py` — extracts and sends tool_trace in A2A response metadata

### Platform Instructions

```
Admin: POST /instructions → platform_instructions table
Admin: GET /instructions?scope=global → list all
Agent boot: GET /workspaces/:id/instructions/resolve → resolved string
→ workspace/prompt.py: build_system_prompt(..., platform_instructions)
→ injected as # Platform Instructions section (highest precedence)
→ refreshed periodically while agent runs
```

Key code:
- `workspace-server/internal/handlers/instructions.go` — CRUD endpoints
- `workspace-server/migrations/040_platform_instructions.up.sql` — table + index
- `workspace/prompt.py` — `get_platform_instructions()` + prepends to system prompt

### Security: instruction content is capped at 8192 chars

The `maxInstructionContentLen` constant and the `CHECK (length(content) <= 8192)` table constraint prevent oversized instructions from being prepended to every agent's system prompt and causing token-budget DoS.

---

## Screencast outline

| Moment | What's on screen | Narration |
|--------|-----------------|-----------|
| 1 | Admin POST global instruction via curl | "Admins create platform-wide instructions in seconds — global scope applies to every workspace automatically." |
| 2 | Admin POST workspace-scoped instruction | "Or target a specific workspace — great for onboarding rules or per-project operational policies." |
| 3 | Workspace boot log showing instructions fetched | "Every workspace fetches its resolved instructions at startup — global plus workspace scope, merged into one string." |
| 4 | System prompt (first section = # Platform Instructions) | "The instructions are injected as the first section of the system prompt, so they take highest precedence — agents cannot override them." |
| 5 | Activity log query showing tool_trace entries | "After every A2A call, the platform stores which tools were actually invoked — admins can verify agent claims and debug unexpected behavior." |

**Total screencast:** ~90 seconds

**TTS narration script** is in `narration.txt`.