"""Shared helpers for AgentExecutor implementations.

Used by adapter executors that live in template repos (claude-code,
gemini-cli, etc.) post-#87 — this module stays in molecule-runtime
because the helpers are runtime-agnostic, not adapter-specific.
Provides:
- Memory recall/commit (HTTP to platform /memories endpoints)
- Delegation results consumption (atomic file rename)
- Current task heartbeat updates
- System prompt loading from /configs
- A2A instructions text for system prompt injection (MCP and CLI variants)
- Brief task summary extraction (markdown-aware)
- Error message sanitization (exception classes and subprocess categories)
- Shared workspace path constants and the MCP server path resolver
- Attached-file extraction and outbound-file staging (platform-wide chat
  attachments — every runtime routes through these helpers so the
  drag-dropped image / returned report experience is identical)
"""

from __future__ import annotations

import asyncio
import base64
import json
import logging
import mimetypes
import os
import re
import shutil
import subprocess
import uuid as _uuid
from pathlib import Path
from typing import TYPE_CHECKING, Any

import httpx

from builtin_tools.security import _redact_secrets

if TYPE_CHECKING:
    from heartbeat import HeartbeatLoop


logger = logging.getLogger(__name__)


# ========================================================================
# Constants — workspace container layout
# ========================================================================

WORKSPACE_MOUNT = "/workspace"
CONFIG_MOUNT = "/configs"
# Resolved relative to this module so it tracks the wheel install
# location. The hardcoded "/app/a2a_mcp_server.py" was correct under
# the pre-#87 monolithic-template layout, but post-universal-runtime
# the file ships inside the molecule-ai-workspace-runtime wheel at
# site-packages/molecule_runtime/, while /app/ now holds only
# template-specific modules (adapter.py + the runtime-native executor).
# Stale path → Claude Code SDK silently fails to spawn the MCP
# subprocess → list_peers / delegate_task / a2a_send_message all
# disappear from the agent's toolset.
DEFAULT_MCP_SERVER_PATH = str(Path(__file__).parent / "a2a_mcp_server.py")
DEFAULT_DELEGATION_RESULTS_FILE = "/tmp/delegation_results.jsonl"
PLATFORM_HTTP_TIMEOUT_S = 5.0
MEMORY_RECALL_LIMIT = 10
MEMORY_CONTENT_MAX_CHARS = 200
BRIEF_SUMMARY_MAX_LEN = 80


def get_mcp_server_path() -> str:
    """Return the path to the stdio MCP server script.

    Overridable via A2A_MCP_SERVER_PATH for tests and non-default layouts.
    """
    return os.environ.get("A2A_MCP_SERVER_PATH", DEFAULT_MCP_SERVER_PATH)


# ========================================================================
# HTTP client (shared, lazily initialised)
# ========================================================================

_http_client: httpx.AsyncClient | None = None


def get_http_client() -> httpx.AsyncClient:
    """Lazy-init a shared httpx client for platform API calls."""
    global _http_client
    if _http_client is None or _http_client.is_closed:
        _http_client = httpx.AsyncClient(timeout=PLATFORM_HTTP_TIMEOUT_S)
    return _http_client


def reset_http_client_for_tests() -> None:
    """Test helper — drop the shared client so the next call rebuilds it.

    Not for production use. Exposed so tests can guarantee a clean slate
    between cases without touching module internals.
    """
    global _http_client
    _http_client = None


# ========================================================================
# Memory recall + commit
# ========================================================================

async def recall_memories() -> str:
    """Recall recent memories from the platform API.

    Returns a newline-joined bullet list of up to MEMORY_RECALL_LIMIT most recent
    memories, or empty string when the platform is unreachable / not configured
    / returns a non-200 / returns an unexpected payload shape.
    """
    workspace_id = os.environ.get("WORKSPACE_ID", "")
    platform_url = os.environ.get("PLATFORM_URL", "")
    if not workspace_id or not platform_url:
        return ""
    # Fix E (Cycle 5): send auth headers so the WorkspaceAuth middleware
    # (Fix A) allows access once the workspace has a live token on file.
    try:
        from platform_auth import auth_headers as _platform_auth
        _auth = _platform_auth()
    except Exception:
        _auth = {}
    try:
        resp = await get_http_client().get(
            f"{platform_url}/workspaces/{workspace_id}/memories",
            headers=_auth,
        )
        if not 200 <= resp.status_code < 300:
            logger.debug(
                "recall_memories: non-2xx response %s from platform",
                resp.status_code,
            )
            return ""
        data = resp.json()
    except Exception as exc:
        logger.debug("recall_memories: request failed: %s", exc)
        return ""
    if not isinstance(data, list) or not data:
        return ""
    lines = [
        f"- [{m.get('scope', '?')}] {m.get('content', '')}"
        for m in data[-MEMORY_RECALL_LIMIT:]
    ]
    return "\n".join(lines)


async def commit_memory(content: str) -> None:
    """Save a memory to the platform API. Best-effort, no error propagation."""
    workspace_id = os.environ.get("WORKSPACE_ID", "")
    platform_url = os.environ.get("PLATFORM_URL", "")
    if not workspace_id or not platform_url or not content:
        return
    content = _redact_secrets(content)
    # Fix E (Cycle 5): include auth header so WorkspaceAuth middleware allows access.
    try:
        from platform_auth import auth_headers as _platform_auth
        _auth = _platform_auth()
    except Exception:
        _auth = {}
    try:
        await get_http_client().post(
            f"{platform_url}/workspaces/{workspace_id}/memories",
            json={"content": content, "scope": "LOCAL"},
            headers=_auth,
        )
    except Exception as exc:
        logger.debug("commit_memory: request failed: %s", exc)


# ========================================================================
# Delegation results — written by heartbeat loop, consumed atomically
# ========================================================================

def read_delegation_results() -> str:
    """Read and consume delegation results written by the heartbeat loop.

    Uses atomic rename to prevent races with the heartbeat writer.
    Returns formatted text suitable for prompt injection, or empty string.
    """
    results_file = Path(
        os.environ.get("DELEGATION_RESULTS_FILE", DEFAULT_DELEGATION_RESULTS_FILE)
    )
    if not results_file.exists():
        return ""
    consumed = results_file.with_suffix(".consumed")
    try:
        results_file.rename(consumed)
    except OSError:
        return ""  # File disappeared between exists() and rename()
    try:
        raw = consumed.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return ""
    finally:
        consumed.unlink(missing_ok=True)

    parts: list[str] = []
    for line in raw.strip().split("\n"):
        if not line.strip():
            continue
        try:
            record = json.loads(line)
        except json.JSONDecodeError:
            continue
        status = record.get("status", "?")
        summary = record.get("summary", "")
        preview = record.get("response_preview", "")
        parts.append(f"- [{status}] {summary}")
        if preview:
            parts.append(f"  Response: {preview[:200]}")
    return "\n".join(parts)


# ========================================================================
# Current task heartbeat update
# ========================================================================

async def set_current_task(heartbeat: "HeartbeatLoop | None", task: str) -> None:
    """Update current task on heartbeat and push immediately via platform API.

    Uses increment/decrement instead of binary 0/1 so agents can track
    multiple concurrent tasks (#1408). Pushes immediately on both
    increment and decrement to avoid phantom-busy (#1372).
    """
    if heartbeat is not None:
        if task:
            heartbeat.active_tasks = getattr(heartbeat, "active_tasks", 0) + 1
            heartbeat.current_task = task
        else:
            heartbeat.active_tasks = max(0, getattr(heartbeat, "active_tasks", 0) - 1)
            if heartbeat.active_tasks == 0:
                heartbeat.current_task = ""
    workspace_id = os.environ.get("WORKSPACE_ID", "")
    platform_url = os.environ.get("PLATFORM_URL", "")
    if not (workspace_id and platform_url):
        return
    active = getattr(heartbeat, "active_tasks", 0) if heartbeat is not None else (1 if task else 0)
    cur_task = getattr(heartbeat, "current_task", task or "") if heartbeat is not None else (task or "")
    try:
        try:
            from platform_auth import auth_headers as _auth
            _headers = _auth()
        except Exception:
            _headers = {}
        await get_http_client().post(
            f"{platform_url}/registry/heartbeat",
            json={
                "workspace_id": workspace_id,
                "current_task": cur_task,
                "active_tasks": active,
                "error_rate": 0,
                "sample_error": "",
                "uptime_seconds": 0,
            },
            headers=_headers,
        )
    except Exception as exc:
        logger.debug("set_current_task: heartbeat push failed: %s", exc)


# ========================================================================
# System prompt loading
# ========================================================================

def get_system_prompt(config_path: str, fallback: str | None = None) -> str | None:
    """Read system-prompt.md from the config dir each call (supports hot-reload).

    Falls back to the provided string if the file doesn't exist.
    """
    prompt_file = Path(config_path) / "system-prompt.md"
    if prompt_file.exists():
        return prompt_file.read_text(encoding="utf-8", errors="replace").strip()
    return fallback


# Tool-usage instructions for system-prompt injection. Generated from
# the platform_tools registry — every tool name, description, and usage
# guidance comes from the canonical ToolSpec. Adding/renaming a tool in
# registry.py automatically flows through here.

_A2A_FOOTER = (
    "Always use list_peers first to discover available workspace IDs. "
    "Access control is enforced — you can only reach siblings and parent/children. "
    "If a delegation returns a DELEGATION FAILED message, do NOT forward "
    "the raw error to the user. Instead: (1) try a different peer, "
    "(2) handle the task yourself, or (3) tell the user which peer is "
    "unavailable and provide your own best answer."
)

_A2A_INSTRUCTIONS_CLI = """## Inter-Agent Communication
You can delegate tasks to other workspaces using the a2a command:
  python3 -m molecule_runtime.a2a_cli peers                                  # List available peers
  python3 -m molecule_runtime.a2a_cli delegate <workspace_id> <task>          # Sync: wait for response
  python3 -m molecule_runtime.a2a_cli delegate --async <workspace_id> <task>  # Async: return task_id
  python3 -m molecule_runtime.a2a_cli status <workspace_id> <task_id>         # Check async task
  python3 -m molecule_runtime.a2a_cli info                                    # Your workspace info

For quick questions, use sync delegate. For long tasks, use --async + status.
Only delegate to peers listed by the peers command (access control enforced)."""

# Maps every a2a-section registry tool to the substring that MUST appear
# in `_A2A_INSTRUCTIONS_CLI` for CLI-runtime agents to discover it. The
# CLI subprocess interface uses different command-shape names than the
# MCP tool names (e.g. `peers` vs `list_peers`), so this is NOT a
# generated mapping — it's a hand-maintained alignment table.
#
# `None` declares "this MCP tool is intentionally NOT exposed via the
# CLI subprocess interface" — make the decision explicit so adding a
# new registry tool fails the alignment test until the mapping is
# updated. test_platform_tools.py asserts both directions:
#
#   1. every a2a tool in the registry is keyed here (no silent omission)
#   2. every non-None substring actually appears in `_A2A_INSTRUCTIONS_CLI`
#
# Why hand-maintained: the registry is the source of truth for
# MCP-capable runtimes, but the CLI subprocess interface in
# `molecule_runtime.a2a_cli` is a separate surface with its own command
# vocabulary. Auto-generating CLI command lines from JSON-schema specs
# would lose the human-readable invocation syntax (`delegate <ws> <task>`
# vs. `--workspace_id=... --task=...`). The mapping + test gives us
# alignment without forcing a uniform shape.
_CLI_A2A_COMMAND_KEYWORDS: dict[str, str | None] = {
    "list_peers": "peers",
    "delegate_task": "delegate ",          # trailing space disambiguates from "--async" line
    "delegate_task_async": "delegate --async",
    "check_task_status": "status",
    "get_workspace_info": "info",
    # `send_message_to_user` is not exposed via the CLI subprocess
    # interface today — it requires a structured `attachments` field
    # that wouldn't survive a positional-arg shell invocation cleanly.
    # CLI-runtime agents fall back to printing results to stdout (which
    # the runtime forwards to the user) instead. If the a2a_cli ever
    # grows a `say` or `message` subcommand, change `None` to that
    # keyword and the alignment test will start passing.
    "send_message_to_user": None,
    # Inbox tools live in the standalone molecule-mcp wrapper only;
    # CLI-subprocess runtimes have their own delivery loop and never
    # invoke these. The alignment test allows None entries — they
    # appear in registry.TOOLS for adapter consistency without
    # forcing a CLI subcommand.
    "wait_for_message": None,
    "inbox_peek": None,
    "inbox_pop": None,
}


def _validate_cli_a2a_command_keywords() -> None:
    """Keep CLI instruction text aligned with command keyword mapping."""
    missing = [
        (tool_name, keyword)
        for tool_name, keyword in _CLI_A2A_COMMAND_KEYWORDS.items()
        if keyword is not None and keyword not in _A2A_INSTRUCTIONS_CLI
    ]
    if missing:
        details = ", ".join(f"{tool_name}={keyword!r}" for tool_name, keyword in missing)
        raise ValueError(
            "CLI A2A command mapping is out of sync with _A2A_INSTRUCTIONS_CLI: "
            f"{details}"
        )


_validate_cli_a2a_command_keywords()


def _render_section(heading: str, specs, footer: str = "") -> str:
    """Render a section: heading, per-tool bullet, per-tool when_to_use, footer."""
    parts = [heading, ""]
    for spec in specs:
        parts.append(f"- **{spec.name}**: {spec.short}")
    parts.append("")
    for spec in specs:
        parts.append(f"### {spec.name}")
        parts.append(spec.when_to_use)
        parts.append("")
    if footer:
        parts.append(footer)
    return "\n".join(parts).rstrip() + "\n"


def get_capabilities_preamble(mcp: bool = True) -> str:
    """Return a top-of-prompt one-glance summary of platform-native tools.

    Shipped 2026-04-30 (#2332): the dogfooding session surfaced that
    agents weren't using A2A delegation, persistent memory, or
    send_message_to_user — these capabilities WERE documented further
    down in the system prompt (## Inter-Agent Communication, ## HMA),
    but agents tend to read top-down and commit to a plan before
    reaching that section.

    The preamble is the elevator pitch: every tool name + its short
    description in a tight bulleted block, immediately after Platform
    Instructions. The detailed when_to_use docs further down still
    apply — this is "you have these tools; consult the dedicated
    section for usage details."

    Generated from the same `platform_tools.registry` ToolSpecs as the
    detailed sections, so renames/additions in registry.py flow through
    automatically. Returns "" for CLI-runtime agents (mcp=False) — they
    get a different overall prompt shape and the registry's MCP-named
    tools wouldn't match the CLI command vocabulary.
    """
    if not mcp:
        # CLI-runtime agents see _A2A_INSTRUCTIONS_CLI's hand-written
        # command list instead. Skip the preamble to avoid confusing
        # agents with two name vocabularies (MCP tool names vs CLI
        # subcommand keywords).
        return ""

    from platform_tools.registry import a2a_tools, memory_tools

    parts = [
        "## Platform Capabilities",
        "",
        (
            "You have native access to these platform tools. Use them "
            "proactively — they're how multi-agent collaboration, "
            "persistent memory, and user communication actually work. "
            "Detailed usage guidance for each lives in the dedicated "
            "sections below; this preamble is just the inventory."
        ),
        "",
        "**Inter-agent collaboration (A2A):**",
    ]
    for spec in a2a_tools():
        parts.append(f"- `{spec.name}` — {spec.short}")
    parts.append("")
    parts.append("**Persistent memory (HMA):**")
    for spec in memory_tools():
        parts.append(f"- `{spec.name}` — {spec.short}")
    return "\n".join(parts).rstrip() + "\n"


def get_a2a_instructions(mcp: bool = True) -> str:
    """Return inter-agent communication instructions for system-prompt injection.

    Generated from the platform_tools registry. Pass `mcp=True` (default)
    for MCP-capable runtimes (claude-code, hermes, langchain, crewai).
    Pass `mcp=False` for CLI-only runtimes (ollama, custom subprocess
    runtimes that don't speak MCP) — those get a static block describing
    the molecule_runtime.a2a_cli subprocess interface instead.
    """
    if not mcp:
        return _A2A_INSTRUCTIONS_CLI
    from platform_tools.registry import a2a_tools
    return _render_section(
        "## Inter-Agent Communication",
        a2a_tools(),
        footer=_A2A_FOOTER,
    )


def get_hma_instructions() -> str:
    """Return HMA persistent-memory instructions for system-prompt injection.

    Generated from the platform_tools registry.
    """
    from platform_tools.registry import memory_tools
    return _render_section(
        "## Hierarchical Memory (HMA)",
        memory_tools(),
        footer=(
            "Memory is automatically recalled at the start of each new "
            "session. Use commit_memory proactively during work so future "
            "sessions and teammates can recall what you learned."
        ),
    )


# ========================================================================
# Misc text helpers
# ========================================================================

_MARKDOWN_FENCE = "```"
_MARKDOWN_HR = "---"


_BRIEF_SUMMARY_MIN_LEN = 4  # 1 char + 3-char ellipsis


def brief_summary(text: str, max_len: int = BRIEF_SUMMARY_MAX_LEN) -> str:
    """Extract a one-line task summary for the canvas card display.

    Strips markdown headers (#, ##, ###), bold/italic markers (**, __),
    and skips code fences and horizontal rules. Returns the first meaningful
    line, truncated with an ellipsis when it exceeds `max_len`.

    `max_len` is clamped to at least 4 (one real character plus a 3-char
    ellipsis) so degenerate callers can't produce negative slice indices.
    """
    max_len = max(max_len, _BRIEF_SUMMARY_MIN_LEN)
    for raw_line in text.split("\n"):
        line = raw_line.strip()
        while line.startswith("#"):
            line = line[1:]
        line = line.strip()
        if not line or line.startswith(_MARKDOWN_FENCE) or line == _MARKDOWN_HR:
            continue
        line = line.replace("**", "").replace("__", "")
        if len(line) > max_len:
            return line[: max_len - 3] + "..."
        return line
    return text[:max_len]


def extract_message_text(message: Any) -> str:
    """Extract text from an A2A message (handles both .text and .root.text patterns)."""
    parts = getattr(message, "parts", None) or []
    text_parts: list[str] = []
    for part in parts:
        text = getattr(part, "text", None)
        if text:
            text_parts.append(text)
            continue
        root = getattr(part, "root", None)
        if root is not None:
            root_text = getattr(root, "text", None)
            if root_text:
                text_parts.append(root_text)
    return " ".join(text_parts).strip()


# Word-boundary patterns for subprocess stderr classification. Using word
# boundaries avoids false positives like "author" matching "auth" or
# "generate" matching "rate".
_RATE_LIMIT_RE = re.compile(r"\brate\b|\b429\b|\boverloaded\b", re.IGNORECASE)
_AUTH_RE = re.compile(r"\bauth(?:entication|orization)?\b|\bapi[_-]?key\b", re.IGNORECASE)
_SESSION_RE = re.compile(r"\bsession\b|\bno conversation found\b", re.IGNORECASE)


def classify_subprocess_error(stderr_text: str, exit_code: int | None) -> str:
    """Map a subprocess stderr blob to a short, user-safe category tag.

    The full stderr goes to the workspace logs via `logger.error`; only the
    category is surfaced to the user to avoid leaking tokens, internal paths,
    or stack traces in the chat UI. Used with `sanitize_agent_error` to
    produce a user-facing message for subprocess failures.
    """
    if _RATE_LIMIT_RE.search(stderr_text):
        return "rate_limited"
    if _AUTH_RE.search(stderr_text):
        return "auth_failed"
    if _SESSION_RE.search(stderr_text):
        return "session_error"
    if exit_code is not None and exit_code != 0:
        return f"exit_{exit_code}"
    return "subprocess_error"


def sanitize_agent_error(
    exc: BaseException | None = None,
    category: str | None = None,
) -> str:
    """Render an agent-side failure into a user-safe error message.

    Either pass an exception (class name is used as the tag) or an explicit
    category string (e.g. from `classify_subprocess_error`). If both are
    given, `category` wins. If neither, the tag defaults to "unknown".

    The message body is deliberately dropped — exception messages and
    subprocess stderr frequently leak stack traces, paths, tokens, and
    API keys. Full detail is available in the workspace logs via
    `logger.exception()` / `logger.error()`.
    """
    if category:
        tag = category
    elif exc is not None:
        tag = type(exc).__name__
    else:
        tag = "unknown"
    return f"Agent error ({tag}) — see workspace logs for details."


# ========================================================================
# Auto-push hook — push unpushed commits and open PR after task completion
# ========================================================================

# Resolve git/gh from PATH so the runtime works regardless of which
# image the workspace is on. Some templates ship a /usr/local/bin/{git,gh}
# wrapper with GH_TOKEN baked in (preferred — picks up auth automatically);
# other templates have plain /usr/bin/git installed by apt. Hardcoding
# /usr/local/bin/git crashed every auto-push attempt on the latter image
# class with `FileNotFoundError: '/usr/local/bin/git'` (issue #2289).
# `shutil.which` finds the wrapper first if it's earlier in PATH, so the
# GH_TOKEN injection still wins where it exists.
_GIT = shutil.which("git") or "/usr/bin/git"
_GH = shutil.which("gh") or "/usr/bin/gh"
_PROTECTED_BRANCHES = frozenset({"staging", "main", "master"})


def _run_git(args: list[str], cwd: str, timeout: int = 30) -> subprocess.CompletedProcess:
    """Run a git/gh command with bounded timeout. Never raises on failure."""
    return subprocess.run(
        args,
        cwd=cwd,
        capture_output=True,
        text=True,
        timeout=timeout,
    )


def _auto_push_and_pr_sync(cwd: str) -> None:
    """Synchronous implementation of the auto-push hook.

    1. Check if we're in a git repo with unpushed commits on a feature branch.
    2. Push the branch.
    3. Open a PR against staging if one doesn't already exist.

    Designed to be called from a background thread — never raises, logs all
    errors. Uses the git/gh wrappers at /usr/local/bin/ which have GH_TOKEN
    baked in.
    """
    try:
        # --- Guard: is this a git repo? ---
        probe = _run_git([_GIT, "rev-parse", "--is-inside-work-tree"], cwd)
        if probe.returncode != 0:
            return

        # --- Guard: get current branch ---
        branch_result = _run_git(
            [_GIT, "rev-parse", "--abbrev-ref", "HEAD"], cwd
        )
        if branch_result.returncode != 0:
            return
        branch = branch_result.stdout.strip()
        if not branch or branch in _PROTECTED_BRANCHES or branch == "HEAD":
            return

        # --- Guard: any unpushed commits? ---
        log_result = _run_git(
            [_GIT, "log", "origin/staging..HEAD", "--oneline"], cwd
        )
        if log_result.returncode != 0 or not log_result.stdout.strip():
            # No unpushed commits (or origin/staging doesn't exist).
            return

        unpushed_lines = log_result.stdout.strip().splitlines()
        logger.info(
            "auto-push: %d unpushed commit(s) on branch '%s', pushing...",
            len(unpushed_lines),
            branch,
        )

        # --- Push ---
        push_result = _run_git(
            [_GIT, "push", "origin", branch], cwd, timeout=60
        )
        if push_result.returncode != 0:
            logger.warning(
                "auto-push: git push failed (exit %d): %s",
                push_result.returncode,
                (push_result.stderr or push_result.stdout)[:500],
            )
            return

        logger.info("auto-push: pushed branch '%s' successfully", branch)

        # --- Check if PR already exists ---
        pr_list = _run_git(
            [_GH, "pr", "list", "--head", branch, "--json", "number"], cwd
        )
        if pr_list.returncode != 0:
            logger.warning(
                "auto-push: gh pr list failed (exit %d): %s",
                pr_list.returncode,
                (pr_list.stderr or pr_list.stdout)[:500],
            )
            return

        existing_prs = json.loads(pr_list.stdout.strip() or "[]")
        if existing_prs:
            logger.info(
                "auto-push: PR already exists for branch '%s' (#%s), skipping create",
                branch,
                existing_prs[0].get("number", "?"),
            )
            return

        # --- Get first commit message for PR title ---
        first_commit = _run_git(
            [_GIT, "log", "origin/staging..HEAD", "--reverse",
             "--format=%s", "-1"],
            cwd,
        )
        pr_title = first_commit.stdout.strip() if first_commit.returncode == 0 else branch
        # Truncate to 256 chars (GitHub limit)
        if len(pr_title) > 256:
            pr_title = pr_title[:253] + "..."

        # --- Create PR ---
        pr_create = _run_git(
            [
                _GH, "pr", "create",
                "--base", "staging",
                "--title", pr_title,
                "--body", "Auto-created by workspace agent",
            ],
            cwd,
            timeout=60,
        )
        if pr_create.returncode != 0:
            logger.warning(
                "auto-push: gh pr create failed (exit %d): %s",
                pr_create.returncode,
                (pr_create.stderr or pr_create.stdout)[:500],
            )
        else:
            pr_url = pr_create.stdout.strip()
            logger.info("auto-push: created PR %s", pr_url)

    except subprocess.TimeoutExpired:
        logger.warning("auto-push: command timed out, skipping")
    except Exception:
        logger.exception("auto-push: unexpected error (non-fatal)")


async def auto_push_hook(cwd: str | None = None) -> None:
    """Post-execution hook: push unpushed commits and open a PR.

    Runs the git/gh subprocess work in a background thread via
    asyncio.to_thread so it never blocks the agent's event loop.
    Catches all exceptions — the agent must never crash due to this hook.
    """
    if cwd is None:
        cwd = WORKSPACE_MOUNT
    if not os.path.isdir(cwd):
        return
    try:
        await asyncio.to_thread(_auto_push_and_pr_sync, cwd)
    except Exception:
        logger.exception("auto_push_hook: failed (non-fatal)")


# ========================================================================
# Chat attachments — platform-level support for drag-drop uploads and
# agent-returned files. Every runtime executor routes inbound file parts
# through ``extract_attached_files`` + ``build_user_content_with_files``
# and post-processes replies through ``collect_outbound_files`` so a file
# attached in the canvas shows up correctly across hermes, claude-code,
# langgraph, CLI runtimes, etc. Living here (not in any one executor)
# keeps the attachment contract in one place — match canvas/ChatTab.tsx
# and workspace-server/internal/handlers/chat_files.go, and every runtime
# benefits at once.
# ========================================================================

# Matches CHAT_UPLOAD_DIR in workspace-server/internal/handlers/chat_files.go.
# The canvas uploads files here; outbound files get staged here so the
# download endpoint (which whitelists this directory) can serve them.
CHAT_UPLOADS_DIR = f"{WORKSPACE_MOUNT}/.molecule/chat-uploads"


def ensure_workspace_writable() -> None:
    """Make /workspace (and the chat-uploads dir) writable by whoever the
    agent will run as.

    Docker's default for a new named volume is root-owned 755 — that
    bricks the agent→user "write a file, hand it to the user" flow for
    every template whose agent runs under a non-root user (hermes uses
    `agent`, most others use some dedicated UID too). Each Dockerfile
    solving this individually was the anti-pattern; this helper belongs
    to the platform so every runtime picks up the fix by calling into
    ``molecule_runtime`` during boot.

    Runs best-effort: if molecule-runtime itself started as non-root
    (rare, but possible in some CP configurations), the chmod silently
    no-ops — the template's own start.sh is expected to have already
    handled perms in that case. We prefer silent degradation to a hard
    boot failure because misconfigured perms are recoverable (user gets
    a clear "permission denied" from the agent) but an uncatchable
    exception here would wedge the whole workspace in `provisioning`.
    """
    # 777 matches the intent: one container, one tenant, anyone in the
    # container can read/write workspace files. Cross-tenant isolation
    # happens at the Docker boundary, not inside the volume.
    for path in (WORKSPACE_MOUNT, CHAT_UPLOADS_DIR):
        try:
            os.makedirs(path, exist_ok=True)
            os.chmod(path, 0o777)
        except PermissionError:
            logger.info(
                "ensure_workspace_writable: lacking root (non-fatal) for %s", path
            )
        except OSError as exc:
            logger.warning(
                "ensure_workspace_writable: %s for %s", exc, path
            )

# Cap image inlining so a 25MB PNG doesn't blow past provider context
# limits. Images larger than this fall back to a path mention only —
# the agent can still read them via file_read / bash tools.
MAX_INLINE_ATTACHMENT_BYTES = 8 * 1024 * 1024

# Absolute /workspace/... paths the agent may mention in its reply.
# Leading boundary prevents matching the middle of URLs like
# https://example.com/workspace/foo while allowing markdown emphasis
# wrappers (**, *, _, `, (, [) so "**/workspace/x.pdf**" still matches.
# Trailing '.' is stripped post-capture (see collect_outbound_files).
_WORKSPACE_PATH_RE = re.compile(
    r"(?:^|[\s`\"'*_(\[])(/workspace/[A-Za-z0-9_./\-]+)"
)
_UNSAFE_NAME_RE = re.compile(r"[^A-Za-z0-9._\-]")


def resolve_attachment_uri(uri: str) -> str | None:
    """Resolve a canvas-issued attachment URI to an in-container path.

    Accepted shapes (matches canvas uploads.ts + chat_files.go):
      - ``workspace:/workspace/.molecule/chat-uploads/<name>``  (canonical)
      - ``file:///workspace/...``                               (legacy)
      - ``/workspace/...``                                      (bare)

    Anything resolving outside ``/workspace`` is refused. ``Path.resolve``
    collapses ``..`` segments so a crafted ``workspace:/workspace/../etc/passwd``
    returns None instead of leaking the real filesystem.
    """
    if not uri:
        return None
    path: str | None = None
    if uri.startswith("workspace:"):
        path = uri[len("workspace:"):]
    elif uri.startswith("file://"):
        path = uri[len("file://"):]
    elif uri.startswith("/"):
        path = uri
    if not path:
        return None
    try:
        resolved = str(Path(path).resolve())
    except (OSError, RuntimeError):
        return None
    if not (resolved == WORKSPACE_MOUNT or resolved.startswith(WORKSPACE_MOUNT + "/")):
        return None
    return resolved


def extract_attached_files(message: Any) -> list[dict[str, str]]:
    """Pull ``{name, mime_type, path}`` dicts out of an A2A message.

    Handles the discriminated-union shape ``part.root.file`` that a2a-sdk
    produces via Pydantic RootModel, and the flatter ``part.file`` shape
    hand-built callers sometimes emit. Non-file parts and files with
    unresolvable URIs are skipped — the caller sees an empty list rather
    than a mix of valid and broken entries.
    """
    if message is None:
        return []
    parts = getattr(message, "parts", None) or []
    out: list[dict[str, str]] = []
    for part in parts:
        root = getattr(part, "root", part)
        if getattr(root, "kind", None) != "file":
            continue
        f = getattr(root, "file", None)
        if f is None:
            continue
        uri = getattr(f, "uri", "") or ""
        name = getattr(f, "name", "") or ""
        mime = getattr(f, "mimeType", None) or getattr(f, "mime_type", None) or ""
        path = resolve_attachment_uri(uri)
        if not path or not os.path.isfile(path):
            logger.warning("skipping attached file with unresolvable uri=%r", uri)
            continue
        out.append({"name": name, "mime_type": mime, "path": path})
    return out


def _read_as_data_url(path: str, mime_type: str) -> str | None:
    """Return ``data:<mime>;base64,<...>`` or None if too large / unreadable."""
    try:
        size = os.path.getsize(path)
    except OSError:
        return None
    if size > MAX_INLINE_ATTACHMENT_BYTES:
        logger.info(
            "attachment %s too large to inline (%d bytes > cap)", path, size
        )
        return None
    try:
        with open(path, "rb") as fh:
            b64 = base64.b64encode(fh.read()).decode("ascii")
    except OSError as exc:
        logger.warning("failed to read attachment %s: %s", path, exc)
        return None
    return f"data:{mime_type or 'application/octet-stream'};base64,{b64}"


def build_user_content_with_files(
    user_text: str, attached: list[dict[str, str]]
) -> Any:
    """Combine text + attachments into an OpenAI-compat ``content`` field.

    - No attachments → plain string (preserves simple shape for non-vision
      models).
    - Any image attachment → list-of-parts with text + image_url entries
      (multi-modal; vision-capable models see the image bytes). Skipped
      when ``MOLECULE_DISABLE_IMAGE_INLINING`` is truthy — some provider/
      model combos (e.g. MiniMax's hermes-agent adapter as of 2026-04)
      claim vision support but hang indefinitely on image payloads, and
      the caller may prefer manifest-only so the agent can still use its
      file_read tool instead of stalling the whole request.
    - Non-image attachments → manifest appended to the text so the agent
      knows the filenames + absolute paths and can inspect via its
      file_read / bash tools.

    This is the platform's one-line fix for "agent didn't know I attached
    a file": any executor that calls it gets attachment awareness for
    free, regardless of which LLM provider is behind it.
    """
    if not attached:
        return user_text

    manifest_lines = [
        f"- {f['name']} ({f['mime_type'] or 'unknown type'}) at {f['path']}"
        for f in attached
    ]
    manifest = "Attached files:\n" + "\n".join(manifest_lines)
    combined = f"{user_text}\n\n{manifest}" if user_text else manifest

    disable_inline = os.environ.get("MOLECULE_DISABLE_IMAGE_INLINING", "").lower() in (
        "1", "true", "yes", "on",
    )
    if disable_inline or not any(
        (f["mime_type"] or "").startswith("image/") for f in attached
    ):
        return combined

    content: list[dict[str, Any]] = [{"type": "text", "text": combined}]
    for f in attached:
        mt = f["mime_type"] or ""
        if not mt.startswith("image/"):
            continue
        data_url = _read_as_data_url(f["path"], mt)
        if data_url is not None:
            content.append({"type": "image_url", "image_url": {"url": data_url}})
    return content


def _sanitize_attachment_name(name: str) -> str:
    cleaned = _UNSAFE_NAME_RE.sub("_", name) or "file"
    return cleaned[:100]


def _guess_mime(path: str) -> str:
    mt, _ = mimetypes.guess_type(path)
    return mt or "application/octet-stream"


def stage_outbound_file(src_path: str) -> dict[str, str] | None:
    """Copy ``src_path`` into ``CHAT_UPLOADS_DIR`` (unless already there)
    and return ``{name, mime_type, path}`` so the caller can attach it to
    the A2A reply.

    Files already in the chat-uploads directory are attached as-is;
    anything elsewhere under /workspace gets a uuid-prefixed copy so
    basenames can't collide with existing uploads and the original
    workspace layout stays untouched. Returns None on I/O failure.
    """
    try:
        os.makedirs(CHAT_UPLOADS_DIR, exist_ok=True)
    except OSError as exc:
        logger.warning("cannot ensure chat-uploads dir: %s", exc)
        return None
    name = os.path.basename(src_path)
    mime = _guess_mime(src_path)
    if os.path.dirname(src_path) == CHAT_UPLOADS_DIR:
        return {"name": name, "mime_type": mime, "path": src_path}
    try:
        stored = f"{_uuid.uuid4().hex[:16]}-{_sanitize_attachment_name(name)}"
        dst = os.path.join(CHAT_UPLOADS_DIR, stored)
        with open(src_path, "rb") as fin, open(dst, "wb") as fout:
            fout.write(fin.read())
    except OSError as exc:
        logger.warning("failed to stage %s → chat-uploads: %s", src_path, exc)
        return None
    return {"name": name, "mime_type": mime, "path": dst}


def collect_outbound_files(reply_text: str) -> list[dict[str, str]]:
    """Detect /workspace/... paths the agent mentioned in its reply and
    stage each one so it can be returned to the canvas as a file part.

    Each unique, readable file goes through ``stage_outbound_file`` — the
    download endpoint only serves files from whitelisted directories, so
    a reply referencing /workspace/private/secret.pem still can't be
    exfiltrated via the chat download link unless we've explicitly
    copied it under the chat-uploads dir.
    """
    if not reply_text:
        return []
    seen: set[str] = set()
    out: list[dict[str, str]] = []
    for match in _WORKSPACE_PATH_RE.finditer(reply_text):
        # Trim trailing sentence punctuation that the character class
        # greedily swallowed — "wrote /workspace/x.txt." would otherwise
        # resolve to "x.txt." which doesn't exist.
        raw = match.group(1).rstrip(".")
        resolved = resolve_attachment_uri(raw)
        if not resolved or resolved in seen or not os.path.isfile(resolved):
            continue
        seen.add(resolved)
        staged = stage_outbound_file(resolved)
        if staged is not None:
            out.append(staged)
    return out


def new_response_message(
    context: Any,
    text: str = "",
    files: list[dict[str, str]] | None = None,
) -> Any:
    """Build an A2A v1 protobuf response Message with task/context correlation.

    Adapter executors should use this instead of ``a2a.helpers.new_text_message``
    (which omits ``task_id`` / ``context_id``) so the platform's a2a proxy can
    reliably correlate the response to the originating task. Mirrors the shape
    used by ``workspace/a2a_executor.py``'s own response construction so all
    runtime paths produce the same Message envelope.

    Args:
        context: The ``RequestContext`` from the inbound A2A request. Reads
            ``context.task_id`` and ``context.context_id``; both fall back to
            fresh UUIDs when ``None`` (RequestContextBuilder always sets them
            in production; the fallback exists for unit tests).
        text: Response text. Empty string omits the text Part — useful when
            replying with files only.
        files: Optional list of ``{"path": ..., "name": ..., "mime_type": ...}``
            dicts (e.g. the output of :func:`collect_outbound_files`). Each
            becomes a Part with ``url="workspace:<path>"``, ``filename``, and
            ``media_type`` set.

    Returns:
        A v1 protobuf ``a2a.types.Message`` ready to pass to
        ``event_queue.enqueue_event(...)``.

    Why this exists: a2a-sdk v1 replaced the v0 Pydantic discriminated-union
    types (``Part(root=TextPart(...))`` / ``Part(root=FilePart(file=
    FileWithUri(...)))``) with a flat protobuf Part struct. Templates that
    were written against v0 + then auto-renamed have shipped without
    ``task_id``/``context_id`` correlation; this helper centralizes the
    canonical pattern.
    """
    # Lazy import: a2a.types is provided by a2a-sdk which is a runtime
    # dependency every adapter image already has. Importing here keeps the
    # module load path lean for callers that don't construct messages.
    from a2a.types import Message, Part, Role

    parts: list = [Part(text=text)] if text else []
    for f in files or []:
        parts.append(Part(
            url="workspace:" + f["path"],
            filename=f["name"],
            media_type=f["mime_type"],
        ))
    return Message(
        message_id=_uuid.uuid4().hex,
        role=Role.ROLE_AGENT,
        parts=parts,
        task_id=getattr(context, "task_id", None) or _uuid.uuid4().hex,
        context_id=getattr(context, "context_id", None) or _uuid.uuid4().hex,
    )
