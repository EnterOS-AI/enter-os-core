"""Temporal durable execution wrapper for Molecule AI A2A workspaces.

Architecture
-----------
A co-located Temporal worker runs as an asyncio background task **inside the
same process** as the A2A server.  This means worker activities share the same
memory space as the A2A handler, which lets us bridge non-serialisable objects
(LangGraph agent, EventQueue, RequestContext) through an in-process registry
without having to serialise them through Temporal's state store.

Workflow stages (names mirror the OTEL span names in a2a_executor.py):

  task_receive  →  llm_call  →  task_complete

  task_receive  — durable checkpoint: task acknowledged, queued
  llm_call      — durable checkpoint: LLM execution + SSE streaming (retryable)
  task_complete — durable checkpoint: execution finished, telemetry recorded

Crash-recovery behaviour
------------------------
If the process crashes while ``llm_call`` is running, Temporal retries the
activity on the restarted process.  The in-process registry is empty after a
restart, so the activity detects a registry miss, logs a warning, and returns
an error result.  The SSE client connection is already gone at that point so
no response can be delivered — but the task is permanently recorded in
Temporal's history and will not silently disappear.

Env vars
--------
TEMPORAL_HOST   Temporal gRPC endpoint  (default: ``localhost:7233``)
                Set this to enable durable execution.  Leave unset (or point
                at an unreachable host) to run in direct-execution mode.

Dependencies (optional)
-----------
    temporalio>=1.7.0

Add to requirements.txt to enable.  The module loads and the wrapper class
works without the package installed — all Temporal paths return early with a
graceful fallback to direct execution.
"""

from __future__ import annotations

import asyncio
import dataclasses
import logging
import os
import uuid
from datetime import timedelta
from typing import Any, Optional

import httpx

logger = logging.getLogger(__name__)


def _platform_url() -> str:
    """Return the platform URL, defaulting to host.docker.internal.

    The workspace runtime always runs inside a Docker container, so
    ``localhost`` refers to the container itself, not the platform host.
    The platform API is only reachable via ``host.docker.internal`` from
    within a workspace container, regardless of how the container was started.
    """
    return os.environ.get("PLATFORM_URL", "http://host.docker.internal:8080")


# ─────────────────────────────────────────────────────────────────────────────
# Constants
# ─────────────────────────────────────────────────────────────────────────────

_TASK_QUEUE = "molecule-agent-tasks"
_WORKFLOW_EXECUTION_TIMEOUT = timedelta(minutes=30)
_ACTIVITY_START_TO_CLOSE_TIMEOUT = timedelta(minutes=10)

# ─────────────────────────────────────────────────────────────────────────────
# Checkpoint persistence (non-fatal)
# ─────────────────────────────────────────────────────────────────────────────


async def _fetch_latest_checkpoint(workspace_id: str) -> Optional[dict]:
    """GET /workspaces/:id/checkpoints/latest — returns the most recently
    completed step for this workspace, or None if no checkpoints exist yet.

    Non-fatal: any HTTP error, network failure, or timeout returns None so
    the calling code continues without a resume context.  A 404 (no checkpoints)
    is the expected response for a freshly provisioned workspace.

    Args:
        workspace_id: The workspace to query.

    Reads:
        PLATFORM_URL  Platform base URL (default ``http://host.docker.internal:8080``).
    """
    try:
        from platform_auth import auth_headers as _auth_headers  # type: ignore[import]

        platform_url = _platform_url()
        url = f"{platform_url}/workspaces/{workspace_id}/checkpoints/latest"
        async with httpx.AsyncClient(timeout=5.0) as client:
            resp = await client.get(url, headers=_auth_headers())
            if resp.status_code == 404:
                return None
            resp.raise_for_status()
            return resp.json()
    except Exception as exc:
        logger.debug(
            "Temporal: latest checkpoint fetch skipped workspace=%s: %s "
            "(non-fatal — starting fresh context)",
            workspace_id,
            exc,
        )
        return None


async def _save_checkpoint(
    workspace_id: str,
    workflow_id: str,
    step_name: str,
    step_index: int,
    payload: Optional[dict] = None,
) -> None:
    """POST a step checkpoint to the platform.

    Non-fatal: any HTTP error, network failure, or timeout is logged as a
    WARNING and silently swallowed so the calling activity always continues.
    Checkpoint loss is survivable; aborting a workflow on a transient DB or
    network blip is not.

    Args:
        workspace_id:  The workspace whose token is used for auth.
        workflow_id:   Unique ID for this workflow execution (task_id).
        step_name:     Temporal activity stage name
                       (``task_receive`` / ``llm_call`` / ``task_complete``).
        step_index:    0-based stage index matching the platform schema.
        payload:       Optional JSON-serialisable dict stored as JSONB.

    Reads:
        PLATFORM_URL   Platform base URL (default ``http://host.docker.internal:8080``).
    """
    try:
        from platform_auth import auth_headers as _auth_headers  # type: ignore[import]

        platform_url = _platform_url()
        url = f"{platform_url}/workspaces/{workspace_id}/checkpoints"
        body: dict = {
            "workflow_id": workflow_id,
            "step_name": step_name,
            "step_index": step_index,
        }
        if payload is not None:
            body["payload"] = payload

        async with httpx.AsyncClient(timeout=5.0) as client:
            resp = await client.post(url, json=body, headers=_auth_headers())
            resp.raise_for_status()

        logger.debug(
            "Temporal: checkpoint saved workspace=%s wf=%s step=%s idx=%d",
            workspace_id,
            workflow_id,
            step_name,
            step_index,
        )
    except Exception as exc:
        # Non-fatal: workflow continues regardless of checkpoint outcome.
        logger.warning(
            "Temporal: checkpoint failed workspace=%s wf=%s step=%s: %s "
            "(non-fatal — workflow continues)",
            workspace_id,
            workflow_id,
            step_name,
            exc,
        )


# ─────────────────────────────────────────────────────────────────────────────
# Serialisable data models
# These are the only objects that cross the Temporal serialisation boundary.
# ─────────────────────────────────────────────────────────────────────────────


@dataclasses.dataclass
class AgentTaskInput:
    """Serialisable snapshot of an incoming A2A task.

    All fields must be JSON-representable so that Temporal can persist them in
    its workflow history (used for crash recovery and replay).
    """

    task_id: str
    context_id: str
    user_input: str
    model: str
    workspace_id: str
    history: list  # [[role, content], ...] — tuples converted to lists


@dataclasses.dataclass
class LLMResult:
    """Serialisable execution result passed from ``llm_call`` to ``task_complete``."""

    final_text: str
    success: bool
    error: str = ""


# ─────────────────────────────────────────────────────────────────────────────
# In-process registry
#
# Maps task_id → {executor, context, event_queue, final_text}
# Activities look up non-serialisable objects here.  The registry is
# populated by TemporalWorkflowWrapper.run() before the workflow starts and
# cleaned up in the finally block when the workflow completes.
# ─────────────────────────────────────────────────────────────────────────────

_task_registry: dict[str, dict[str, Any]] = {}


# ─────────────────────────────────────────────────────────────────────────────
# Temporal workflow + activities
# Loaded only when the temporalio package is installed.  The surrounding
# try/except ensures the module imports cleanly without the package.
# ─────────────────────────────────────────────────────────────────────────────

_TEMPORAL_AVAILABLE = False

try:
    from temporalio import activity, workflow
    from temporalio.client import Client
    from temporalio.worker import Worker

    _TEMPORAL_AVAILABLE = True

    # ── Activities ────────────────────────────────────────────────────────── #

    @activity.defn(name="task_receive")
    async def task_receive_activity(inp: AgentTaskInput) -> dict:
        """Durable checkpoint: task received and queued for LLM execution.

        Mirrors the *task_receive* OTEL span opened in
        ``LangGraphA2AExecutor._core_execute()``.  This activity is lightweight —
        it validates that the in-process registry entry exists and logs receipt.
        The actual A2A "working" signal (``updater.start_work()``) is emitted
        inside ``_core_execute()`` so that SSE timing is preserved.

        Saves a step checkpoint after completing.  Checkpoint failure is
        non-fatal — the activity returns normally regardless.
        """
        logger.info(
            "Temporal[task_receive] task_id=%s context_id=%s workspace=%s model=%s",
            inp.task_id,
            inp.context_id,
            inp.workspace_id,
            inp.model,
        )
        if inp.task_id not in _task_registry:
            logger.warning(
                "Temporal[task_receive] task_id=%s not found in registry "
                "(crash recovery path — no SSE client connection available)",
                inp.task_id,
            )
            try:
                await _save_checkpoint(
                    inp.workspace_id, inp.task_id, "task_receive", 0,
                    {"task_id": inp.task_id, "status": "registry_miss"},
                )
            except Exception as _ckpt_exc:  # pragma: no cover
                logger.warning("task_receive checkpoint swallowed: %s", _ckpt_exc)
            return {"task_id": inp.task_id, "status": "registry_miss"}

        try:
            await _save_checkpoint(
                inp.workspace_id, inp.task_id, "task_receive", 0,
                {"task_id": inp.task_id, "status": "received"},
            )
        except Exception as _ckpt_exc:  # pragma: no cover
            logger.warning("task_receive checkpoint swallowed: %s", _ckpt_exc)
        return {"task_id": inp.task_id, "status": "received"}

    @activity.defn(name="llm_call")
    async def llm_call_activity(inp: AgentTaskInput) -> LLMResult:
        """Durable checkpoint: LLM execution with streaming to the event_queue.

        Mirrors the *llm_call* OTEL span in ``LangGraphA2AExecutor._core_execute()``.
        Calls ``executor._core_execute()`` which handles the full execution pipeline:
        SSE streaming, OTEL sub-spans, final message emission, and heartbeat updates.

        On crash recovery (empty registry): logs a warning and returns an error
        result.  Temporal records the failure and will retry if configured to do so.
        The original SSE client connection is gone after a crash, so no response
        can be delivered, but the task is durably recorded in Temporal's history.
        """
        logger.info("Temporal[llm_call] task_id=%s", inp.task_id)

        entry = _task_registry.get(inp.task_id)
        if entry is None:
            msg = (
                f"task_id={inp.task_id} not in registry — "
                "process likely restarted; original SSE client connection is gone"
            )
            logger.warning("Temporal[llm_call] registry miss: %s", msg)
            miss_result = LLMResult(final_text="", success=False, error=msg)
            try:
                await _save_checkpoint(
                    inp.workspace_id, inp.task_id, "llm_call", 1,
                    {"success": False, "error": msg},
                )
            except Exception as _ckpt_exc:  # pragma: no cover
                logger.warning("llm_call checkpoint swallowed: %s", _ckpt_exc)
            return miss_result

        try:
            executor = entry["executor"]
            context = entry["context"]
            event_queue = entry["event_queue"]

            # _core_execute() is the renamed body of the original execute().
            # It handles: OTEL spans, SSE streaming, final message, heartbeat.
            final_text = await executor._core_execute(context, event_queue)

            # Cache for task_complete observability
            entry["final_text"] = final_text or ""
            result = LLMResult(final_text=final_text or "", success=True)

        except Exception as exc:
            logger.error(
                "Temporal[llm_call] task_id=%s execution error: %s",
                inp.task_id,
                exc,
                exc_info=True,
            )
            result = LLMResult(final_text="", success=False, error=str(exc))

        try:
            await _save_checkpoint(
                inp.workspace_id, inp.task_id, "llm_call", 1,
                {"success": result.success, "error": result.error or None},
            )
        except Exception as _ckpt_exc:  # pragma: no cover
            logger.warning("llm_call checkpoint swallowed: %s", _ckpt_exc)
        return result

    @activity.defn(name="task_complete")
    async def task_complete_activity(result: LLMResult) -> None:
        """Durable checkpoint: task execution finished.

        Mirrors the *task_complete* OTEL span in ``LangGraphA2AExecutor._core_execute()``.
        This activity records the outcome for Temporal observability.  The actual
        OTEL task_complete span fires inside ``_core_execute()``; this activity
        provides a durable, queryable record in Temporal's workflow history.

        Saves a step checkpoint.  Checkpoint failure is non-fatal.
        The ``workspace_id`` and ``task_id`` are not available in this activity
        (only the ``LLMResult`` is passed from ``llm_call``), so the checkpoint
        is skipped here — ``llm_call`` already captured the final outcome.
        """
        if result.success:
            logger.info(
                "Temporal[task_complete] success=True final_text_len=%d",
                len(result.final_text),
            )
        else:
            logger.warning(
                "Temporal[task_complete] success=False error=%r",
                result.error,
            )

    # ── Workflow ──────────────────────────────────────────────────────────── #

    @workflow.defn
    class MoleculeAIAgentWorkflow:
        """Durable Temporal workflow for Molecule AI A2A agent task execution.

        Sequences three activities that mirror the OTEL span hierarchy in
        ``LangGraphA2AExecutor._core_execute()``:

            task_receive  →  llm_call  →  task_complete

        Each activity is a durable checkpoint: if the process crashes between
        activities, Temporal resumes from the last completed checkpoint on
        restart.  If an activity fails (exception or timeout), Temporal can
        retry it according to the configured retry policy.
        """

        @workflow.run
        async def run(self, inp: AgentTaskInput) -> LLMResult:
            opts: dict[str, Any] = {
                "start_to_close_timeout": _ACTIVITY_START_TO_CLOSE_TIMEOUT,
            }

            # Stage 1 — acknowledge receipt (lightweight checkpoint)
            await workflow.execute_activity(task_receive_activity, inp, **opts)

            # Stage 2 — LLM execution (main work; retryable on crash/timeout)
            result: LLMResult = await workflow.execute_activity(
                llm_call_activity, inp, **opts
            )

            # Stage 3 — record completion (lightweight checkpoint)
            await workflow.execute_activity(task_complete_activity, result, **opts)

            return result

except ImportError:
    # temporalio not installed — the wrapper class below will gracefully fall
    # back to direct execution for every call.
    logger.debug(
        "Temporal: temporalio package not installed — "
        "durable execution disabled (add temporalio>=1.7.0 to requirements.txt)"
    )


# ─────────────────────────────────────────────────────────────────────────────
# TemporalWorkflowWrapper
# ─────────────────────────────────────────────────────────────────────────────


class TemporalWorkflowWrapper:
    """Wraps ``LangGraphA2AExecutor.execute()`` with Temporal durable execution.

    The wrapper intercepts each ``execute()`` call and routes it through a
    ``MoleculeAIAgentWorkflow`` Temporal workflow.  If Temporal is unavailable
    for any reason, execution falls back transparently to the direct path
    (``executor._core_execute()``), so the A2A server never crashes due to
    Temporal issues.

    Lifecycle
    ---------
    1. ``create_wrapper()`` — instantiate and register the global singleton.
    2. ``await wrapper.start()`` — connect to Temporal, launch the background
       worker.  No-op (with a log warning) if Temporal is unreachable.
    3. Normal operation — ``wrapper.run()`` is called from ``execute()``.
    4. ``await wrapper.stop()`` — cancel the background worker task on shutdown.

    Co-located worker pattern
    -------------------------
    The Temporal worker runs as an asyncio background task in the **same event
    loop** as the A2A server.  This means:
    - No separate worker process to manage.
    - Activities share the process's memory (registry access works).
    - Worker and server share the same asyncio event loop.

    Env vars
    --------
    ``TEMPORAL_HOST``  Temporal gRPC address, e.g. ``localhost:7233`` or
                       ``temporal.internal:7233``.  Defaults to
                       ``localhost:7233``.  If Temporal is not reachable at
                       this address, the wrapper falls back to direct execution.
    """

    def __init__(self) -> None:
        self._host: str = os.environ.get("TEMPORAL_HOST", "localhost:7233")
        self._client: Optional[Any] = None
        self._worker: Optional[Any] = None
        self._worker_task: Optional[asyncio.Task] = None  # type: ignore[type-arg]
        self._available: bool = False

    # ── Lifecycle ─────────────────────────────────────────────────────────── #

    async def start(self) -> None:
        """Connect to Temporal and start the co-located background worker.

        Safe to call multiple times (idempotent after first success).
        Never raises — logs a warning and returns on any failure.
        """
        if not _TEMPORAL_AVAILABLE:
            logger.info(
                "Temporal: temporalio package not installed — "
                "all tasks will use direct execution. "
                "To enable durable execution: pip install temporalio>=1.7.0"
            )
            return

        if self._available:
            return  # already started

        # Connect to the Temporal server
        try:
            self._client = await Client.connect(self._host)  # type: ignore[name-defined]
            logger.info("Temporal: connected to %s", self._host)
        except Exception as exc:
            logger.warning(
                "Temporal: cannot connect to %s (%s) — "
                "all tasks will use direct execution (no durable state)",
                self._host,
                exc,
            )
            return

        # Start the worker as an asyncio background task
        try:
            self._worker = Worker(  # type: ignore[name-defined]
                self._client,
                task_queue=_TASK_QUEUE,
                workflows=[MoleculeAIAgentWorkflow],  # type: ignore[name-defined]
                activities=[
                    task_receive_activity,  # type: ignore[name-defined]
                    llm_call_activity,  # type: ignore[name-defined]
                    task_complete_activity,  # type: ignore[name-defined]
                ],
            )
            self._worker_task = asyncio.create_task(
                self._worker.run(),
                name="temporal-worker",
            )
            self._available = True
            logger.info(
                "Temporal: co-located worker started on task queue '%s'",
                _TASK_QUEUE,
            )
        except Exception as exc:
            logger.warning(
                "Temporal: worker initialisation failed (%s) — "
                "falling back to direct execution",
                exc,
            )

    async def stop(self) -> None:
        """Gracefully stop the Temporal worker background task."""
        self._available = False
        if self._worker_task and not self._worker_task.done():
            self._worker_task.cancel()
            try:
                await self._worker_task
            except (asyncio.CancelledError, Exception):
                pass
        logger.info("Temporal: worker stopped")

    # ── Public API ────────────────────────────────────────────────────────── #

    def is_available(self) -> bool:
        """Return ``True`` if Temporal is connected and the worker is running."""
        return self._available

    async def run(
        self,
        executor: Any,
        context: Any,
        event_queue: Any,
    ) -> None:
        """Route one A2A task execution through a Temporal durable workflow.

        Steps
        -----
        1. Build a serialisable ``AgentTaskInput`` from the A2A request context.
        2. Store non-serialisable state (executor, context, event_queue) in
           the in-process ``_task_registry`` keyed by task_id.
        3. Submit and await ``MoleculeAIAgentWorkflow`` on the Temporal server.
        4. Clean up the registry entry (always, via ``finally``).

        Falls back to ``executor._core_execute()`` if:
        - Temporal is not available (``is_available()`` is False).
        - Input extraction fails.
        - The workflow raises any exception.

        This guarantees that the A2A client always receives a response even
        when Temporal is misconfigured or temporarily unreachable.
        """
        if not self._available or self._client is None:
            # Temporal unavailable — silent direct fallback
            await executor._core_execute(context, event_queue)
            return

        task_id = getattr(context, "task_id", None) or str(uuid.uuid4())
        context_id = getattr(context, "context_id", None) or str(uuid.uuid4())

        # Build serialisable AgentTaskInput
        try:
            from adapters.shared_runtime import (
                extract_history as _extract_history,
                extract_message_text,
            )

            user_input = extract_message_text(context) or ""
            raw_history = _extract_history(context)
            # Convert (role, content) tuples → [role, content] lists (JSON-safe)
            history: list = [list(pair) for pair in raw_history]
        except Exception as exc:
            logger.warning(
                "Temporal: failed to extract serialisable task input (%s) — "
                "falling back to direct execution",
                exc,
            )
            await executor._core_execute(context, event_queue)
            return

        workspace_id_env = os.environ.get("WORKSPACE_ID", "unknown")

        # Issue #837: query the latest checkpoint for this workspace.
        # If a previous workflow crashed mid-step, inject the last known
        # step into the history so the agent is aware of its prior state.
        # Non-fatal: a missing or 404 response means starting fresh.
        last_ckpt = await _fetch_latest_checkpoint(workspace_id_env)
        if last_ckpt:
            step_name = last_ckpt.get("step_name", "unknown")
            workflow_id_ckpt = last_ckpt.get("workflow_id", "")
            completed_at = last_ckpt.get("completed_at", "")
            ckpt_note = (
                f"[SYSTEM: This workspace was previously executing workflow "
                f"'{workflow_id_ckpt}'. The last recorded step was '{step_name}' "
                f"(completed at {completed_at}). If the current task is a "
                f"continuation of that workflow, resume from this point. "
                f"Otherwise ignore this context and start fresh.]"
            )
            # Prepend as a synthetic context entry so the agent sees it at the
            # start of its history — before any user messages for this task.
            history = [["system", ckpt_note]] + history
            logger.info(
                "Temporal: injecting checkpoint context task_id=%s last_step=%s wf=%s",
                task_id,
                step_name,
                workflow_id_ckpt,
            )

        inp = AgentTaskInput(
            task_id=task_id,
            context_id=context_id,
            user_input=user_input,
            model=getattr(executor, "_model", "unknown"),
            workspace_id=workspace_id_env,
            history=history,
        )

        # Register non-serialisable in-process state for activities to access
        _task_registry[task_id] = {
            "executor": executor,
            "context": context,
            "event_queue": event_queue,
            "final_text": "",
        }

        try:
            logger.info(
                "Temporal: starting workflow molecule-%s on queue '%s'",
                task_id,
                _TASK_QUEUE,
            )
            await self._client.execute_workflow(
                MoleculeAIAgentWorkflow.run,  # type: ignore[name-defined]
                inp,
                id=f"molecule-{task_id}",
                task_queue=_TASK_QUEUE,
                execution_timeout=_WORKFLOW_EXECUTION_TIMEOUT,
            )
        except Exception as exc:
            logger.error(
                "Temporal: workflow molecule-%s failed (%s) — "
                "falling back to direct execution so client receives a response",
                task_id,
                exc,
                exc_info=True,
            )
            # Direct fallback ensures the SSE client is never left hanging
            await executor._core_execute(context, event_queue)
        finally:
            _task_registry.pop(task_id, None)


# ─────────────────────────────────────────────────────────────────────────────
# Module-level singleton helpers
# Used by a2a_executor.py and main.py
# ─────────────────────────────────────────────────────────────────────────────

_global_wrapper: Optional[TemporalWorkflowWrapper] = None


def get_wrapper() -> Optional[TemporalWorkflowWrapper]:
    """Return the global ``TemporalWorkflowWrapper``, or ``None`` if not set.

    Called from ``LangGraphA2AExecutor.execute()`` on every request.
    Returns ``None`` before ``create_wrapper()`` is called (direct-execution mode).
    """
    return _global_wrapper


def create_wrapper() -> TemporalWorkflowWrapper:
    """Create (or return the existing) global ``TemporalWorkflowWrapper``.

    Idempotent — safe to call multiple times.  Call ``await wrapper.start()``
    after this to connect to Temporal and launch the background worker.

    Example (in main.py)::

        from builtin_tools.temporal_workflow import create_wrapper as create_temporal_wrapper
        temporal_wrapper = create_temporal_wrapper()
        await temporal_wrapper.start()          # connects + starts worker
        try:
            await server.serve()
        finally:
            await temporal_wrapper.stop()
    """
    global _global_wrapper
    if _global_wrapper is None:
        _global_wrapper = TemporalWorkflowWrapper()
    return _global_wrapper
