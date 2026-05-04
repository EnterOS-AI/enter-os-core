"""Canonical registry of platform tool specs.

Every tool the platform offers to agents (A2A delegation, persistent
memory, broadcast, introspection) is defined ONCE in TOOLS below.
Adapters consume these specs to register the tool in their native
runtime format:

  - a2a_mcp_server.py iterates `TOOLS` to build the MCP TOOLS list +
    dispatches calls to spec.impl. No tool name or description is
    hardcoded there.

  - builtin_tools/{delegation,memory}.py define LangChain `@tool`
    wrappers using `name=` from the spec; the wrapper body just
    calls spec.impl.

  - executor_helpers.get_a2a_instructions(mcp=True) /
    get_hma_instructions() GENERATE the system-prompt doc string from
    `TOOLS` — no hand-maintained instruction text for MCP-capable
    runtimes.

  - executor_helpers._A2A_INSTRUCTIONS_CLI is a SEPARATE hand-maintained
    block for CLI subprocess runtimes (ollama and any other adapter
    that drives a2a via `python3 -m molecule_runtime.a2a_cli ...`). It
    uses different command-shape names than the registry tool names
    (e.g. `peers` vs `list_peers`), so it cannot be auto-generated
    from JSON-schema specs without losing the readable invocation
    syntax. Its tool-coverage alignment with the registry is enforced
    by the `_CLI_A2A_COMMAND_KEYWORDS` mapping in executor_helpers.py
    and the alignment tests in test_platform_tools.py — adding a new
    a2a tool here will fail those tests until the mapping is updated.

Adding a new tool: append a ToolSpec to `TOOLS` below, then update
`_CLI_A2A_COMMAND_KEYWORDS` in executor_helpers.py (set the value to
the CLI subcommand keyword, or to `None` if the tool isn't exposed via
the CLI subprocess interface). The structural alignment tests in
workspace/tests/test_platform_tools.py fail otherwise.

Renaming a tool: change `name` here. Search workspace/ for the old
literal in case any non-adapter consumer (tests, plugin code) hard-coded
it; update those manually. The grep is the audit, the test is the gate.

Removing a tool: delete the entry AND its `_CLI_A2A_COMMAND_KEYWORDS`
key. Adapters stop registering it automatically; doc generators stop
mentioning it.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Literal

from a2a_tools import (
    tool_chat_history,
    tool_check_task_status,
    tool_commit_memory,
    tool_delegate_task,
    tool_delegate_task_async,
    tool_get_workspace_info,
    tool_inbox_peek,
    tool_inbox_pop,
    tool_list_peers,
    tool_recall_memory,
    tool_send_message_to_user,
    tool_wait_for_message,
)

# Section name maps to the heading in the agent-facing system prompt.
# Adding a new section: add a constant + create a corresponding
# generator in executor_helpers (or generalize get_*_instructions).
A2A_SECTION = "a2a"
MEMORY_SECTION = "memory"

Section = Literal["a2a", "memory"]


@dataclass(frozen=True)
class ToolSpec:
    """Runtime-agnostic definition of one platform tool.

    Each adapter (MCP, LangChain, future SDK) consumes the same spec.
    Doc generators consume the same spec. There is no other source
    of truth for tool naming or description.
    """

    name: str
    """The exact name agents see. MUST match every adapter's
    registered name and the literal that appears in agent-facing
    instruction docs. Structural test enforces this."""

    short: str
    """One-line description. Used as the MCP `description` field
    AND as the bullet line in agent-facing instruction docs."""

    when_to_use: str
    """Two-to-three-sentence agent-facing usage guidance — when
    to call this tool, what it returns, what NOT to confuse it
    with. Concatenated into the system prompt below the tool list."""

    input_schema: dict[str, Any]
    """JSON Schema for the tool's input parameters. Consumed
    directly by the MCP server. LangChain derives its schema from
    Python type annotations on the @tool function — alignment is
    pinned by the structural test."""

    impl: Callable[..., Awaitable[str]]
    """The actual coroutine. Both adapters call this; only the
    wrapping differs."""

    section: Section
    """Which agent-prompt section this tool belongs to (controls
    which instruction generator emits it)."""


# ---------------------------------------------------------------------------
# A2A — inter-agent communication & broadcast
# ---------------------------------------------------------------------------

_DELEGATE_TASK = ToolSpec(
    name="delegate_task",
    short=(
        "Delegate a task to a peer workspace via A2A and WAIT for the "
        "response (synchronous)."
    ),
    when_to_use=(
        "Use for QUICK questions and small sub-tasks where you can "
        "afford to wait inline. Returns the peer's response text "
        "directly. For longer-running work (research, multi-minute "
        "jobs) use delegate_task_async + check_task_status instead "
        "so you don't hold this workspace busy waiting."
    ),
    input_schema={
        "type": "object",
        "properties": {
            "workspace_id": {
                "type": "string",
                "description": "Target workspace ID (from list_peers).",
            },
            "task": {
                "type": "string",
                "description": "Task description to send to the peer.",
            },
            "source_workspace_id": {
                "type": "string",
                "description": (
                    "Optional. The registered workspace this delegation "
                    "originates from when the agent is registered to "
                    "multiple workspaces (MOLECULE_WORKSPACES). Auto-"
                    "routes via the peer→source cache when omitted; "
                    "single-workspace operators can ignore it."
                ),
            },
        },
        "required": ["workspace_id", "task"],
    },
    impl=tool_delegate_task,
    section=A2A_SECTION,
)

_DELEGATE_TASK_ASYNC = ToolSpec(
    name="delegate_task_async",
    short=(
        "Send a task to a peer and return immediately with a task_id "
        "(non-blocking)."
    ),
    when_to_use=(
        "Use for long-running work where you want to keep doing other "
        "things while the peer processes. Poll with check_task_status "
        "to retrieve the result. The platform's A2A queue handles "
        "delivery + retries; the peer works independently."
    ),
    input_schema={
        "type": "object",
        "properties": {
            "workspace_id": {
                "type": "string",
                "description": "Target workspace ID (from list_peers).",
            },
            "task": {
                "type": "string",
                "description": "Task description to send to the peer.",
            },
            "source_workspace_id": {
                "type": "string",
                "description": (
                    "Optional. The registered workspace this delegation "
                    "originates from. Auto-routes via the peer→source "
                    "cache when omitted."
                ),
            },
        },
        "required": ["workspace_id", "task"],
    },
    impl=tool_delegate_task_async,
    section=A2A_SECTION,
)

_CHECK_TASK_STATUS = ToolSpec(
    name="check_task_status",
    short=(
        "Poll the status of a task started with delegate_task_async; "
        "returns result when done."
    ),
    when_to_use=(
        "Statuses: pending/in_progress (peer still working — wait), "
        "queued (peer is busy with a prior task — DO NOT retry, the "
        "platform stitches the response when it finishes), completed "
        "(result available), failed (real error — fall back to a "
        "different peer or handle it yourself)."
    ),
    input_schema={
        "type": "object",
        "properties": {
            "workspace_id": {
                "type": "string",
                "description": "Workspace ID the task was sent to.",
            },
            "task_id": {
                "type": "string",
                "description": "task_id returned by delegate_task_async.",
            },
            "source_workspace_id": {
                "type": "string",
                "description": (
                    "Optional. Which registered workspace's delegation "
                    "log to query. Defaults to this workspace."
                ),
            },
        },
        "required": ["workspace_id", "task_id"],
    },
    impl=tool_check_task_status,
    section=A2A_SECTION,
)

_LIST_PEERS = ToolSpec(
    name="list_peers",
    short=(
        "List the workspaces this agent can communicate with — name, "
        "ID, status, role for each."
    ),
    when_to_use=(
        "Call this first when you need to delegate but don't know the "
        "target's ID. Access control is enforced — you only see "
        "siblings, parent, and direct children. With "
        "MOLECULE_WORKSPACES set, peers from every registered workspace "
        "are aggregated and tagged with their source."
    ),
    input_schema={
        "type": "object",
        "properties": {
            "source_workspace_id": {
                "type": "string",
                "description": (
                    "Optional. Restrict to peers of this one registered "
                    "workspace. Omit to aggregate across all workspaces "
                    "an external agent has registered against."
                ),
            },
        },
    },
    impl=tool_list_peers,
    section=A2A_SECTION,
)

_GET_WORKSPACE_INFO = ToolSpec(
    name="get_workspace_info",
    short="Get this workspace's own info — ID, name, role, tier, parent, status.",
    when_to_use=(
        "Use to introspect your own identity (e.g. before reporting "
        "back to the user, or to determine whether you're a tier-0 "
        "root that can write GLOBAL memory)."
    ),
    input_schema={"type": "object", "properties": {}},
    impl=tool_get_workspace_info,
    section=A2A_SECTION,
)

_SEND_MESSAGE_TO_USER = ToolSpec(
    name="send_message_to_user",
    short=(
        "Send a message directly to the user's canvas chat — pushed instantly "
        "via WebSocket. Use this to: (1) acknowledge a task immediately ('Got "
        "it, I'll start working on this'), (2) send interim progress updates "
        "while doing long work, (3) deliver follow-up results after delegation "
        "completes, (4) attach files (zip, pdf, csv, image) for the user to "
        "download via the `attachments` field (NEVER paste file URLs in "
        "`message`). The message appears in the user's chat as if you're "
        "proactively reaching out."
    ),
    when_to_use=(
        "Use proactively across the lifecycle of a task — early to "
        "acknowledge, mid-flight to update, late to deliver. Never paste "
        "file URLs in the message body — always pass absolute paths in "
        "`attachments` so the platform serves them as download chips "
        "(works on SaaS where external file hosts are unreachable)."
    ),
    input_schema={
        "type": "object",
        "properties": {
            "message": {
                "type": "string",
                # The "no URLs in message text" rule is the single biggest
                # cause of bad chat UX: agents drop catbox.moe / file://
                # / temporary upload-host links into the prose, the
                # canvas renders them as plain markdown links the user
                # can't preview, and SaaS deployments often can't even
                # reach those external hosts. Every download MUST go
                # through the structured `attachments` field below.
                "description": (
                    "Caption text for the chat bubble. Required even when sending "
                    "attachments — set to a short label like 'Here's the build:' "
                    "or 'Done — see attached.'\n\n"
                    "DO NOT paste file URLs, download links, or container paths in "
                    "this string. Files MUST go through the `attachments` field, "
                    "which renders as a clickable download chip and works on SaaS "
                    "deployments where external file-host URLs (catbox.moe, file://, "
                    "etc.) are unreachable from the user's browser."
                ),
            },
            "attachments": {
                "type": "array",
                "description": (
                    "REQUIRED for any file delivery. Pass absolute file paths inside "
                    "THIS container (e.g. ['/tmp/build.zip', '/workspace/report.pdf']) "
                    "— the platform uploads each file and returns a download chip "
                    "with the file's icon + name + size in the user's chat. The chip "
                    "works in SaaS deployments because the URL is platform-served, "
                    "not an external host.\n\n"
                    "USE THIS instead of: pasting URLs in `message`, base64-encoding "
                    "in the body, or telling the user to look at a path on disk. "
                    "If the file isn't already on disk, write it first (Bash, Write "
                    "tool, etc.) then pass its path here. 25 MB per file cap."
                ),
                "items": {"type": "string"},
            },
            "workspace_id": {
                "type": "string",
                "description": (
                    "Optional. Set ONLY when this agent is registered in MULTIPLE "
                    "workspaces (external multi-workspace MCP path) — pass the "
                    "`arrival_workspace_id` of the inbound message you're replying "
                    "to so the user sees the reply in the same canvas they typed in. "
                    "Single-workspace agents omit this; the message routes to the "
                    "only registered workspace."
                ),
            },
        },
        "required": ["message"],
    },
    impl=tool_send_message_to_user,
    section=A2A_SECTION,
)


# ---------------------------------------------------------------------------
# Inbox — inbound delivery for the standalone molecule-mcp path.
#
# These tools observe a poller-fed in-memory queue (see workspace/inbox.py).
# They are universally registered so docs + adapters stay aligned, but
# they only return real data in the standalone molecule-mcp runtime;
# in-container runtimes return an informational "not enabled" message
# because their delivery loop is push-based via the canvas WebSocket.
# ---------------------------------------------------------------------------

_WAIT_FOR_MESSAGE = ToolSpec(
    name="wait_for_message",
    short=(
        "Block until the next inbound message (canvas user OR peer "
        "agent) arrives, or until ``timeout_secs`` elapses."
    ),
    when_to_use=(
        "Standalone-runtime ONLY (molecule-mcp wrapper). After "
        "you reply, call this to wait for the next message — forms "
        "the loop ``wait_for_message → respond → wait_for_message``. "
        "Returns the head message non-destructively; call inbox_pop "
        "with the activity_id once you've handled it. In-container "
        "runtimes receive messages via push and should not call this."
    ),
    input_schema={
        "type": "object",
        "properties": {
            "timeout_secs": {
                "type": "number",
                "description": (
                    "Max seconds to block. Capped at 300. "
                    "Default 60."
                ),
            },
        },
    },
    impl=tool_wait_for_message,
    section=A2A_SECTION,
)

_INBOX_PEEK = ToolSpec(
    name="inbox_peek",
    short="List pending inbound messages without removing them.",
    when_to_use=(
        "Standalone-runtime ONLY. Use to inspect what's queued "
        "before deciding which to handle. Non-destructive — pair "
        "with inbox_pop to consume after replying."
    ),
    input_schema={
        "type": "object",
        "properties": {
            "limit": {
                "type": "integer",
                "description": "Max messages to return. Default 10.",
            },
        },
    },
    impl=tool_inbox_peek,
    section=A2A_SECTION,
)

_CHAT_HISTORY = ToolSpec(
    name="chat_history",
    short="Fetch the prior conversation with one peer (both sides, chronological).",
    when_to_use=(
        "Call this when a peer_agent push lands and you need context "
        "from prior turns with that workspace — e.g. \"what task did "
        "this peer assign me last hour?\" or \"what did I tell them?\". "
        "Both sides of the conversation appear in chronological order, "
        "so the agent reads the log top-down. Cheaper than re-deriving "
        "context from memory because the platform already audits every "
        "A2A turn into activity_logs. Pair with `agent_card_url` from "
        "the channel envelope when you also need the peer's "
        "capabilities."
    ),
    input_schema={
        "type": "object",
        "properties": {
            "peer_id": {
                "type": "string",
                "description": (
                    "The peer workspace's UUID — same value you got "
                    "as `peer_id` on the inbound push, or as "
                    "`workspace_id` from `list_peers`."
                ),
            },
            "limit": {
                "type": "integer",
                "description": (
                    "Max rows to return (default 20, capped at 500). "
                    "Default 20 covers \"most recent context\" without "
                    "flooding the conversation window."
                ),
            },
            "before_ts": {
                "type": "string",
                "description": (
                    "Optional RFC3339 timestamp; passes through to the "
                    "server for paging backward through long histories. "
                    "Use the oldest `created_at` from a previous response."
                ),
            },
        },
        "required": ["peer_id"],
    },
    impl=tool_chat_history,
    section=A2A_SECTION,
)

_INBOX_POP = ToolSpec(
    name="inbox_pop",
    short="Remove a handled message from the inbox queue by activity_id.",
    when_to_use=(
        "Standalone-runtime ONLY. Call after you've replied to a "
        "message returned from wait_for_message or inbox_peek to "
        "drop it from the queue. Idempotent — popping a missing "
        "id reports removed=false without erroring."
    ),
    input_schema={
        "type": "object",
        "properties": {
            "activity_id": {
                "type": "string",
                "description": (
                    "activity_id of the message to remove (from "
                    "inbox_peek / wait_for_message output)."
                ),
            },
        },
        "required": ["activity_id"],
    },
    impl=tool_inbox_pop,
    section=A2A_SECTION,
)


# ---------------------------------------------------------------------------
# HMA — hierarchical persistent memory
# ---------------------------------------------------------------------------

_COMMIT_MEMORY = ToolSpec(
    name="commit_memory",
    short="Save a fact to persistent memory; survives across sessions and restarts.",
    when_to_use=(
        "Scopes: LOCAL (private to you, default), TEAM (shared with "
        "parent + siblings), GLOBAL (entire org — only tier-0 root "
        "workspaces can write). Commit decisions, learned facts, and "
        "completed-task summaries so future sessions and teammates "
        "can recall them."
    ),
    input_schema={
        "type": "object",
        "properties": {
            "content": {
                "type": "string",
                "description": "What to remember — be specific.",
            },
            "scope": {
                "type": "string",
                "enum": ["LOCAL", "TEAM", "GLOBAL"],
                "description": "Memory scope (default LOCAL).",
            },
        },
        "required": ["content"],
    },
    impl=tool_commit_memory,
    section=MEMORY_SECTION,
)

_RECALL_MEMORY = ToolSpec(
    name="recall_memory",
    short="Search persistent memory; returns matching LOCAL + TEAM + GLOBAL rows.",
    when_to_use=(
        "Call at the start of new work and when picking up something "
        "you may have done before. Empty query returns ALL accessible "
        "memories — cheap and avoids missing rows that don't match a "
        "narrow keyword. Memory is automatically recalled at session "
        "start; use this to refresh mid-session."
    ),
    input_schema={
        "type": "object",
        "properties": {
            "query": {
                "type": "string",
                "description": "Search query (empty returns all).",
            },
            "scope": {
                "type": "string",
                "enum": ["LOCAL", "TEAM", "GLOBAL", ""],
                "description": "Filter by scope (empty = all accessible).",
            },
        },
    },
    impl=tool_recall_memory,
    section=MEMORY_SECTION,
)


# ---------------------------------------------------------------------------
# Public registry. Keep alphabetically grouped by section for stable
# adapter listings + diff-friendly review.
# ---------------------------------------------------------------------------

TOOLS: list[ToolSpec] = [
    # A2A
    _DELEGATE_TASK,
    _DELEGATE_TASK_ASYNC,
    _CHECK_TASK_STATUS,
    _LIST_PEERS,
    _GET_WORKSPACE_INFO,
    _SEND_MESSAGE_TO_USER,
    # Inbox (standalone-only; in-container returns informational error)
    _WAIT_FOR_MESSAGE,
    _INBOX_PEEK,
    _INBOX_POP,
    _CHAT_HISTORY,
    # HMA
    _COMMIT_MEMORY,
    _RECALL_MEMORY,
]


def a2a_tools() -> list[ToolSpec]:
    """All A2A-section tools, in registration order."""
    return [t for t in TOOLS if t.section == A2A_SECTION]


def memory_tools() -> list[ToolSpec]:
    """All memory-section tools, in registration order."""
    return [t for t in TOOLS if t.section == MEMORY_SECTION]


def by_name(name: str) -> ToolSpec:
    """Look up a spec by its canonical name. Raises KeyError if absent."""
    for t in TOOLS:
        if t.name == name:
            return t
    raise KeyError(f"no platform tool named {name!r}")


def tool_names() -> list[str]:
    """Canonical names in registration order."""
    return [t.name for t in TOOLS]
