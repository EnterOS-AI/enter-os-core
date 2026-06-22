# Codex CLI workspace adapter — app-server design

**Status:** Design draft — pre-implementation
**Owner:** EnterOS (team@enteros.dev)
**Date:** 2026-05-02
**Codex version validated against:** `codex-cli 0.72.0`
**Related:** `docs/integrations/hermes-platform-plugins-upstream-pr.md`,
`molecule-ai-workspace-template-openclaw/packages/openclaw-channel-plugin/`

---

## Goal

Add a EnterOS workspace template for the OpenAI Codex CLI runtime
(`@openai/codex` v0.72+). The template should give Codex agents the
same A2A inbox + mid-session push behavior the other supported
runtimes have:

- **claude-code:** MCP `notifications/claude/channel`
- **OpenClaw:** channel-plugin webhook into the gateway kernel
- **hermes:** `BasePlatformAdapter` (pending upstream PR; polling fallback today)
- **codex (this design):** persistent `codex app-server` stdio JSON-RPC
  client; A2A messages become `turn/start` calls against a long-lived
  thread

Today there is no codex template. The legacy fallback registry entry
at `workspace-server/internal/handlers/runtime_registry.go:83` exists
only to keep old workspaces from crashing — there is no live adapter,
no Dockerfile, nothing in `manifest.json`. This design covers the
fresh build.

---

## Architecture decision: app-server, not `codex exec`

`codex exec --json` is the obvious shape — one CLI subprocess per
A2A message, same anti-pattern OpenClaw used to have and that we are
replacing. It loses session continuity (no shared thread), pays
process-spawn cost on every turn, and gives no path to mid-turn
interruption.

`codex app-server` is a long-running JSON-RPC server over stdio that
holds thread state in memory. The v2 protocol (validated below) gives
us:

- `thread/start` → returns `threadId`
- `turn/start` → input array, threadId required → returns `turnId`
- `turn/interrupt` → cancel a running turn by `(threadId, turnId)`
- Server-pushed notifications: `agent_message_delta`, `turn/started`,
  `turn/completed`, `reasoning_text_delta`,
  `command_execution_output_delta`, `mcp_tool_call_progress`,
  `error_notification`, etc.

A persistent app-server child plus a small async stdio reader gives us
session continuity AND mid-turn injection. Same dual-win shape we got
from migrating OpenClaw away from `openclaw agent`.

### Why not v1?

v1 of the protocol exposes `newConversation` + `sendUserMessage` /
`sendUserTurn` (one-shot per message, no streaming notifications). v2
introduces threads + turns + delta notifications. v2 is the
forward-looking surface; we build against v2 from the start.

---

## RPC sequence

### 1. Boot

```
adapter spawn ▶ codex app-server (stdio NDJSON)
         ◀ ready (process up)
adapter ▶ {"jsonrpc":"2.0","id":1,"method":"initialize",
           "params":{"clientInfo":{"name":"molecule-runtime","version":"…"}}}
adapter ◀ {"id":1,"result":{"userAgent":"codex_cli_rs/0.72.0 …"}}
```

Validated 2026-05-02 against the installed binary — NDJSON framing,
initialize works as shown.

### 2. Thread per workspace session

```
adapter ▶ thread/start
            params: {model, sandboxPolicy, approvalPolicy, cwd,
                     baseInstructions, developerInstructions, …}
adapter ◀ {result: {thread: {threadId: "th_…"}}}
```

`threadId` is cached on the adapter for the workspace's lifetime. On
adapter restart we use `thread/resume` against the persisted ID
(written to disk under `~/.codex/sessions/` by codex itself, but we
also keep our own pointer in workspace state for fast restore).

### 3. A2A message → turn/start

For each inbound A2A message:

```
adapter ▶ turn/start
            params: {threadId, input: [{type:"text", text:"…"}], …}
adapter ◀ {result: {turn: {turnId: "tu_…"}}}

(server pushes notifications)
adapter ◀ turn/started
adapter ◀ agent_message_delta (text chunk)
adapter ◀ agent_message_delta (text chunk)
…
adapter ◀ turn/completed
```

The adapter accumulates `agent_message_delta` chunks into a buffer
keyed by `turnId`, emits them onto the A2A response queue (streamed if
the molecule-runtime contract supports streaming, otherwise assembled
into a single final message on `turn/completed`).

### 4. Mid-turn injection — the load-bearing case

**Default policy: per-thread serialization.** If a turn is already
running when a second A2A message arrives, queue the new message and
fire `turn/start` once the current `turn/completed` lands. This
matches OpenClaw's per-chat sequentializer behavior — the A2A peer
sees their messages handled in order, and we don't need
`turn/interrupt` for the common case.

**Opt-in policy: interrupt-and-rerun.** For workspaces that prefer
"latest message wins" semantics (rare; configurable), the adapter
fires `turn/interrupt` with `(threadId, currentTurnId)`, waits for
`turn/completed` (with cancelled status), then `turn/start` with the
combined context: previous user message + agent's partial response so
far + new message, so the agent has full context of what got
interrupted. Off by default.

### 5. Shutdown

```
adapter ▶ {"method":"shutdown"} (if v2 exposes one; otherwise SIGTERM)
adapter ▶ close stdio
adapter ▶ wait(child, timeout=5s); on timeout SIGKILL
```

---

## File layout (new template repo)

```
molecule-ai-workspace-template-codex/
├── adapter.py        # BaseAdapter shell, thin (~50 LOC)
├── executor.py       # AppServerProxyExecutor — the RPC client (~300 LOC)
├── app_server.py     # AppServerProcess — stdio child + NDJSON reader (~150 LOC)
├── config.yaml
├── Dockerfile        # node:20 + npm i -g @openai/codex@0.72
├── start.sh          # boots adapter; codex app-server is spawned per session by executor
├── requirements.txt
├── README.md
└── tests/
    ├── test_app_server.py     # mocks stdio; tests framing, request/notification dispatch
    └── test_executor.py       # mocks AppServerProcess; tests turn lifecycle, interrupt
```

Modeled on the hermes template (which is the closest existing shape:
adapter.py + executor.py separation; daemon proxy via local IPC). The
extra `app_server.py` exists because the JSON-RPC client + child
process management is non-trivial enough to warrant its own module
with its own tests.

---

## Executor skeleton

```python
# executor.py — A2A → codex app-server bridge

class CodexAppServerExecutor(AgentExecutor):
    """Holds one app-server child + thread, dispatches A2A turns as turn/start RPCs."""

    def __init__(self, config: AdapterConfig):
        self._config = config
        self._app_server: AppServerProcess | None = None
        self._thread_id: str | None = None
        self._turn_lock = asyncio.Lock()  # serialize per-thread by default

    async def _ensure_thread(self) -> str:
        if self._app_server is None:
            self._app_server = await AppServerProcess.start()
            await self._app_server.initialize(client_info={
                "name": "molecule-runtime",
                "version": MOLECULE_RUNTIME_VERSION,
            })
        if self._thread_id is None:
            resp = await self._app_server.request("thread/start", {
                "model": self._config.model or None,
                "developerInstructions": self._config.system_prompt or None,
                # other policy fields (sandbox, approval) — EnterOS defaults
            })
            self._thread_id = resp["thread"]["threadId"]
        return self._thread_id

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        prompt = extract_message_text(context.message) or ""
        if not prompt.strip():
            await event_queue.enqueue_event(new_agent_text_message("(empty prompt)"))
            return

        async with self._turn_lock:  # per-thread serialization
            thread_id = await self._ensure_thread()

            # Subscribe to delta notifications BEFORE starting the turn so we
            # don't race the first agent_message_delta.
            buffer: list[str] = []
            done = asyncio.Event()
            error: Exception | None = None

            def on_notification(method: str, params: dict) -> None:
                nonlocal error
                if method == "agent_message_delta":
                    buffer.append(params.get("delta", ""))
                elif method == "turn/completed":
                    done.set()
                elif method == "error_notification":
                    error = RuntimeError(params.get("message", "unknown app-server error"))
                    done.set()

            unsub = self._app_server.subscribe(on_notification)
            try:
                resp = await self._app_server.request("turn/start", {
                    "threadId": thread_id,
                    "input": [{"type": "text", "text": prompt}],
                })
                turn_id = resp["turn"]["turnId"]
                await asyncio.wait_for(done.wait(), timeout=_TURN_TIMEOUT)
            finally:
                unsub()

            if error:
                await event_queue.enqueue_event(
                    new_agent_text_message(f"[codex error] {error}"))
                return
            await event_queue.enqueue_event(new_agent_text_message("".join(buffer)))

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        # When the molecule-runtime cancels a request, fire turn/interrupt
        # against the currently-running turn. Best-effort — racing
        # turn/completed is fine, app-server returns a noop in that case.
        if self._app_server and self._thread_id and self._current_turn_id:
            await self._app_server.request("turn/interrupt", {
                "threadId": self._thread_id,
                "turnId": self._current_turn_id,
            })
```

The `AppServerProcess` class encapsulates: stdio child management,
NDJSON line reader/writer, request-id correlation, notification
subscriber registry, and graceful shutdown. Standard async stdio
JSON-RPC client — nothing exotic.

---

## Open questions to resolve before implementation

1. **MoleculeRuntime streaming contract.** Does our A2A executor
   contract support emitting incremental events (so the user sees
   partial responses as the agent streams), or do we always assemble
   on `turn/completed`? If streaming is supported, we want to forward
   each `agent_message_delta` as an A2A event for parity with hermes
   gateway streaming. (Cross-reference: hermes adapter currently
   doesn't stream either — `executor.py:122` sets `stream=False` —
   so non-streaming is the safe v1 baseline.)

2. **Sandbox policy default.** Codex defaults to `read-only` for safety
   in CLI mode; for workspace use we need write access to the
   workspace tree. Pick a sensible default in `thread/start` —
   probably `workspace-write` scoped to the workspace cwd.

3. **Approval policy default.** Codex's `--ask-for-approval` modes
   (`untrusted`, `on-failure`, `never`). Workspace agents need
   `never` (they can't prompt a human). Confirm this is exposed via
   `approvalPolicy` in `thread/start`.

4. **Auth — login flow.** Codex supports `login api-key` (env
   `OPENAI_API_KEY`) and `login chatgpt` (interactive OAuth). For
   workspace use we mandate API key. Document this in the template's
   README and surface it as a required env in config.yaml.

5. **MCP server passthrough.** Codex's own `mcp_servers` config lets
   the agent call out to MCP servers as a CLIENT. Should the workspace
   adapter automatically wire `~/.codex/config.toml` so the agent can
   reach the molecule MCP server (chat_history, recall_memory,
   delegate_task)? Almost certainly yes — but verify the env-var
   substitution pattern works in TOML.

6. **Thread persistence across workspace restarts.** Codex stores
   sessions on disk under `~/.codex/sessions/`. The adapter should
   persist the threadId in workspace state so a restart resumes the
   thread (`thread/resume`) rather than starting fresh. This matches
   the existing molecule-runtime convention for session continuity.

7. **Token usage / cost reporting.** v2 emits
   `ThreadTokenUsageUpdatedNotification`. Plumb this into our usage
   tracking — same path the other runtimes use.

8. **MCP push notifications inbound.** Earlier research established
   that codex's own MCP server mode does NOT support
   `notifications/*` for push. So the path for unsolicited mid-session
   A2A messages is NOT "codex's MCP client receives notifications from
   our MCP server" — it's "molecule-runtime polls inbox via
   `wait_for_message`, and on each polled message fires `turn/start`
   on the existing thread." The "MCP native" framing here is satisfied
   not by codex receiving MCP push, but by the persistent thread +
   turn/start delivering the same UX (session continuity + queued or
   interrupted handling of new messages mid-thread).

---

## Why this design satisfies "MCP native push parity"

User goal: every runtime delivers A2A inbox messages with the same
quality of experience as claude-code's MCP `notifications/claude/channel`.

claude-code path: MCP server pushes notification → claude-code SDK
injects synthetic user turn into running session.

Codex path: molecule-runtime polls inbox (universal poll path) →
adapter fires `turn/start` on the existing app-server thread → codex
processes the message in-thread with full context. The "push" happens
at the molecule-runtime ↔ adapter boundary; the "native" part is that
codex's own session model handles it as an in-thread turn, not as a
fresh subprocess.

For mid-turn arrivals: the per-thread serialization (or opt-in
interrupt) gives us behavior equivalent to OpenClaw's per-chat
sequentializer. Equivalent UX to claude-code's mid-session
notification injection in practice — one is a kernel-level interrupt,
the other is a queue-then-dispatch, but the user-visible behavior
("the agent processes my message after the current turn finishes") is
identical.

---

## Sequencing

This is post-demo work. Order:

1. **Spec the executor lifecycle** — pin down the open questions
   above (especially #1 streaming, #5 MCP passthrough, #6 thread
   persistence) before any code lands.
2. **Implement `AppServerProcess`** with thorough unit tests against a
   mock stdio. This is the riskiest module (concurrency around
   request-id correlation + notification dispatch); land it first
   with high coverage.
3. **Implement `CodexAppServerExecutor`** on top.
4. **Build the template repo skeleton** (Dockerfile, config.yaml,
   start.sh, README) once the Python side runs locally.
5. **Add codex to `manifest.json`** and the runtime registry.
6. **End-to-end verify** per `feedback_close_on_user_visible_not_merge`
   — boot a real workspace, send A2A messages, observe streamed
   responses + thread continuity + queued mid-turn handling.

Estimated total: 3-5 engineering days for v1, plus E2E hardening.
