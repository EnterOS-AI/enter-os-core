"""Bridge between LangGraph agent and A2A protocol, with SSE streaming support.

SSE streaming architecture
--------------------------
The A2A SDK (``DefaultRequestHandler`` + ``EventQueue``) owns the SSE transport
layer.  This executor's job is to push the right event types into the queue as
work progresses:

  1. ``TaskStatusUpdateEvent(state=working)``       — immediately signals start
  2. ``TaskArtifactUpdateEvent(chunk, append=…)``   — one per LLM text token
  3. ``Message(final_text)``                        — terminal event

Client compatibility
--------------------
*Non-streaming* (``message/send``):
    ``ResultAggregator.consume_all()`` processes status/artifact events
    (updating the task in the store) and returns the final ``Message``
    immediately — backward-compatible with ``a2a_client.py`` which reads
    ``data["result"]["parts"][0]["text"]``.

*Streaming* (``message/stream``):
    ``consume_and_emit()`` yields every event above as SSE, letting the client
    render tokens in real time.

LangGraph integration
---------------------
Uses ``agent.astream_events(version="v2")`` to receive ``on_chat_model_stream``
events with ``AIMessageChunk`` payloads.  Text is extracted from both plain
strings (OpenAI / Groq) and Anthropic-style content-block lists.  Non-text
content (tool_use, etc.) is silently skipped.  A fresh ``artifact_id`` is
generated for each new LLM ``run_id`` so tool-call cycles are grouped cleanly.
"""

import functools
import logging
import os
import uuid

from a2a.server.agent_execution import AgentExecutor, RequestContext
from a2a.server.events import EventQueue
from a2a.server.tasks import TaskUpdater
from a2a.types import Part
# KI-009: a2a-sdk v1 renames a2a.utils → a2a.helpers; TextPart removed (Part takes text= directly)
from a2a.helpers import new_text_message
from shared_runtime import (
    extract_history as _extract_history,
    extract_message_text,
    brief_task,
    set_current_task,
)
from executor_helpers import (
    collect_outbound_files,
    extract_attached_files,
)
from builtin_tools.telemetry import (
    A2A_TASK_ID,
    GEN_AI_OPERATION_NAME,
    GEN_AI_REQUEST_MODEL,
    GEN_AI_SYSTEM,
    WORKSPACE_ID_ATTR,
    _incoming_trace_context,
    gen_ai_system_from_model,
    get_tracer,
    record_llm_token_usage,
)

logger = logging.getLogger(__name__)

_WORKSPACE_ID = os.environ.get("WORKSPACE_ID", "unknown")

# LangGraph ReAct cycle budget per turn. Library default is 25; 500 covers
# PM fan-outs (plan → 6 delegations → 6 awaits → 6 results → synthesize ≈
# 30+ steps even before retries). Overridable via LANGGRAPH_RECURSION_LIMIT.
DEFAULT_RECURSION_LIMIT = 500


def _parse_recursion_limit() -> int:
    """Read LANGGRAPH_RECURSION_LIMIT; fall back to DEFAULT_RECURSION_LIMIT
    with a WARNING log on any unparseable or non-positive value."""
    raw = os.environ.get("LANGGRAPH_RECURSION_LIMIT", "")
    if not raw:
        return DEFAULT_RECURSION_LIMIT
    try:
        n = int(raw)
    except ValueError:
        logger.warning(
            "LANGGRAPH_RECURSION_LIMIT=%r is not an integer; using default %d",
            raw, DEFAULT_RECURSION_LIMIT,
        )
        return DEFAULT_RECURSION_LIMIT
    if n <= 0:
        logger.warning(
            "LANGGRAPH_RECURSION_LIMIT=%d is not positive; using default %d",
            n, DEFAULT_RECURSION_LIMIT,
        )
        return DEFAULT_RECURSION_LIMIT
    return n

# ---------------------------------------------------------------------------
# Compliance (OWASP Top 10 for Agentic Apps) — optional, lazy-loaded
# ---------------------------------------------------------------------------

try:
    from builtin_tools.compliance import (
        AgencyTracker,
        ExcessiveAgencyError,
        PromptInjectionError,
        redact_pii as _redact_pii,
        sanitize_input as _sanitize_input,
    )
    _COMPLIANCE_AVAILABLE = True
except ImportError:  # pragma: no cover
    _COMPLIANCE_AVAILABLE = False


@functools.lru_cache(maxsize=1)
def _get_compliance_cfg():
    """Return ComplianceConfig or None (cached for process lifetime)."""
    try:
        from config import load_config
        return load_config().compliance
    except Exception:
        return None


def _extract_chunk_text(content) -> list[str]:
    """Extract text strings from an LLM streaming chunk's content field.

    Handles both provider content styles:
    - OpenAI / Groq: ``content`` is a plain ``str`` (empty for tool-call chunks).
    - Anthropic:     ``content`` is a list of typed blocks, e.g.
        ``[{"type": "text", "text": "Hello"}, {"type": "tool_use", ...}]``

    Only ``"text"`` blocks are returned; ``tool_use``, ``tool_result``, and
    other non-text blocks are filtered out so raw tool JSON never appears in
    the SSE stream.

    Args:
        content: ``chunk.content`` value from an ``on_chat_model_stream`` event.

    Returns:
        List of non-empty text strings.
    """
    if isinstance(content, str):
        return [content] if content else []
    if isinstance(content, list):
        texts: list[str] = []
        for block in content:
            if isinstance(block, dict) and block.get("type") == "text":
                text = block.get("text", "")
                if text:
                    texts.append(text)
            elif isinstance(block, str) and block:
                texts.append(block)
        return texts
    return []


class LangGraphA2AExecutor(AgentExecutor):
    """Bridges LangGraph agent to A2A event model with SSE streaming support.

    Always uses ``agent.astream_events()`` so that:
    - Streaming clients (``message/stream``) receive token-level SSE events.
    - Non-streaming clients (``message/send``) receive the final ``Message``
      collected from the same stream — no duplicate LLM call, full compat.
    """

    def __init__(self, agent, heartbeat=None, model: str = "unknown"):
        self.agent = agent  # Compiled LangGraph graph (create_react_agent output)
        self._heartbeat = heartbeat
        self._model = model  # e.g. "anthropic:claude-sonnet-4-6"

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        """Execute a task from an A2A request with SSE streaming.

        Routes through the Temporal durable workflow when a global
        ``TemporalWorkflowWrapper`` is initialised and connected to Temporal;
        otherwise falls back to ``_core_execute()`` (direct path).

        Event emission sequence:
          1. TaskStatusUpdateEvent(working)           — immediate start signal
          2. TaskArtifactUpdateEvent chunks           — token-by-token via astream_events
          3. Message(final_text)                      — terminal; non-streaming clients
                                                        return on this; streaming clients
                                                        also receive it as the last SSE event.
        """
        # ── Optional Temporal durable execution wrapper ──────────────────────
        # When a TemporalWorkflowWrapper is active this routes execution through
        # a MoleculeAIAgentWorkflow (task_receive → llm_call → task_complete).
        # Falls back silently to _core_execute() on any error or if Temporal
        # is unavailable, so the client always receives a response.
        try:
            from builtin_tools.temporal_workflow import get_wrapper as _get_temporal_wrapper

            _tw = _get_temporal_wrapper()
            if _tw is not None and _tw.is_available():
                return await _tw.run(self, context, event_queue)
        except Exception:
            pass  # Never let the wrapper path crash the executor

        await self._core_execute(context, event_queue)

    async def _core_execute(self, context: RequestContext, event_queue: EventQueue) -> str:
        """Core execution pipeline — called directly or from a Temporal activity.

        This is the original ``execute()`` body, extracted so that the Temporal
        ``llm_call`` activity can invoke it without re-entering the wrapper
        check and causing infinite recursion.

        Returns the final response text (empty string on empty input or error).

        Event emission sequence:
          1. TaskStatusUpdateEvent(working)           — immediate start signal
          2. TaskArtifactUpdateEvent chunks           — token-by-token via astream_events
          3. Message(final_text)                      — terminal event
        """
        user_input = extract_message_text(context)
        # Pull attached files from A2A message parts (kind: "file") and
        # append a manifest to the prompt so the agent knows they exist.
        # LangGraph tools (filesystem, bash, skills) can then open the
        # files by path — without this the agent silently ignores the
        # attachments and replies "I'm not sure what you're referring to".
        _attached_files = extract_attached_files(getattr(context, "message", None))
        if _attached_files:
            _manifest = "\n\nAttached files:\n" + "\n".join(
                f"- {f['name']} ({f['mime_type'] or 'unknown type'}) at {f['path']}"
                for f in _attached_files
            )
            user_input = (user_input + _manifest) if user_input else _manifest.lstrip()
        if not user_input:
            parts = getattr(getattr(context, "message", None), "parts", None)
            logger.warning("A2A execute: no text content in message parts: %s", parts)
            await event_queue.enqueue_event(
                new_text_message("Error: message contained no text content.")
            )
            return ""

        # ── OA-01: Prompt injection check (OWASP Agentic Top 10) ────────────
        _compliance_cfg = _get_compliance_cfg() if _COMPLIANCE_AVAILABLE else None
        if _COMPLIANCE_AVAILABLE and _compliance_cfg and _compliance_cfg.mode == "owasp_agentic":
            try:
                user_input = _sanitize_input(
                    user_input,
                    prompt_injection_mode=_compliance_cfg.prompt_injection,
                    context_id=context.context_id or "",
                )
            except PromptInjectionError as exc:
                await event_queue.enqueue_event(
                    new_text_message(f"Request blocked: {exc}")
                )
                return ""

        logger.info("A2A execute: user_input=%s", user_input[:200])

        # ── OTEL: task_receive span ──────────────────────────────────────────
        parent_ctx = _incoming_trace_context.get()
        tracer = get_tracer()

        _result: str = ""  # captured inside the span for return after it closes

        with tracer.start_as_current_span("task_receive", context=parent_ctx) as task_span:
            task_span.set_attribute(WORKSPACE_ID_ATTR, _WORKSPACE_ID)
            task_span.set_attribute(A2A_TASK_ID, context.context_id or "")
            task_span.set_attribute("a2a.input_preview", user_input[:256])

            # Resolve IDs — the RequestContextBuilder always sets them, but
            # we generate fallbacks for safety (e.g. in unit tests).
            task_id = context.task_id or str(uuid.uuid4())
            context_id = context.context_id or str(uuid.uuid4())

            updater = TaskUpdater(event_queue, task_id, context_id)

            try:
                # set_current_task INSIDE the try so active_tasks is always
                # decremented by the finally block even if CancelledError hits
                # during the heartbeat HTTP push. Moving it outside the try
                # created a window where cancellation left active_tasks stuck
                # at 1, permanently blocking queue drain. (#2026)
                await set_current_task(self._heartbeat, brief_task(user_input))
                messages = _extract_history(context)
                if messages:
                    logger.info("A2A execute: injecting %d history messages", len(messages))
                messages.append(("human", user_input))

                # Recursion limit: see DEFAULT_RECURSION_LIMIT and
                # _parse_recursion_limit() at module top. Re-read on every
                # call so the env var can be hot-changed between requests.
                recursion_limit = _parse_recursion_limit()
                run_config = {
                    "configurable": {"thread_id": context_id},
                    "run_name": f"a2a-{context_id[:8]}",
                    "recursion_limit": recursion_limit,
                }

                # ── OTEL: llm_call span ──────────────────────────────────────
                with tracer.start_as_current_span("llm_call") as llm_span:
                    llm_span.set_attribute(GEN_AI_OPERATION_NAME, "chat")
                    llm_span.set_attribute(GEN_AI_SYSTEM, gen_ai_system_from_model(self._model))
                    llm_span.set_attribute(GEN_AI_REQUEST_MODEL, self._model)
                    llm_span.set_attribute(WORKSPACE_ID_ATTR, _WORKSPACE_ID)

                    # ── Step 1: signal "working" to streaming clients ─────────
                    await updater.start_work()

                    # ── Step 2: stream tokens via LangGraph astream_events ────
                    # Each "on_chat_model_stream" event carries an AIMessageChunk.
                    # We emit one TaskArtifactUpdateEvent per text chunk so SSE
                    # clients can render tokens in real time.
                    # artifact_id resets on each new LLM run_id so agent→tool→agent
                    # cycles each get their own artifact slot.

                    artifact_id = str(uuid.uuid4())
                    has_streamed = False   # True after first chunk for current artifact
                    current_run_id = None  # Detects new LLM call in a ReAct cycle
                    accumulated: list[str] = []    # All text for the final Message
                    last_ai_message = None          # Saved for token-usage telemetry

                    # ── OA-03: Excessive agency tracker ──────────────────────
                    _agency = (
                        AgencyTracker(
                            max_tool_calls=_compliance_cfg.max_tool_calls_per_task,
                            max_duration_seconds=float(_compliance_cfg.max_task_duration_seconds),
                        )
                        if _COMPLIANCE_AVAILABLE and _compliance_cfg and _compliance_cfg.mode == "owasp_agentic"
                        else None
                    )

                    # ── Tool trace: collect every tool invocation for
                    # platform-level observability ────────────────────
                    # Keyed by run_id so parallel tool calls (LangGraph
                    # supports them) pair start→end correctly. Capped at
                    # MAX_TOOL_TRACE entries to prevent runaway loops from
                    # ballooning the JSONB payload.
                    MAX_TOOL_TRACE = 200
                    tool_trace: list[dict] = []
                    tool_trace_by_run: dict[str, dict] = {}

                    async for event in self.agent.astream_events(
                        {"messages": messages},
                        config=run_config,
                        version="v2",
                    ):
                        kind = event.get("event", "")

                        if kind == "on_chat_model_stream":
                            run_id = event.get("run_id", "")
                            if run_id and run_id != current_run_id:
                                # New LLM run started — fresh artifact slot
                                current_run_id = run_id
                                artifact_id = str(uuid.uuid4())
                                has_streamed = False

                            chunk = event.get("data", {}).get("chunk")
                            if chunk is not None:
                                texts = _extract_chunk_text(chunk.content)
                                for text in texts:
                                    await updater.add_artifact(
                                        parts=[Part(text=text)],  # v1: TextPart removed, Part takes text= directly
                                        artifact_id=artifact_id,
                                        append=has_streamed,  # False=first, True=append
                                        last_chunk=False,
                                    )
                                    has_streamed = True
                                    accumulated.append(text)

                        elif kind == "on_tool_start":
                            tool_name = event.get("name", "?")
                            tool_input = event.get("data", {}).get("input", "")
                            tool_run_id = event.get("run_id", "")
                            logger.debug("SSE: tool start — %s", tool_name)
                            if len(tool_trace) < MAX_TOOL_TRACE:
                                entry = {
                                    "tool": tool_name,
                                    "input": str(tool_input)[:500] if tool_input else "",
                                }
                                tool_trace.append(entry)
                                if tool_run_id:
                                    tool_trace_by_run[tool_run_id] = entry
                            if _agency is not None:
                                _agency.on_tool_call(
                                    tool_name=tool_name,
                                    context_id=context_id,
                                )

                        elif kind == "on_tool_end":
                            tool_end_name = event.get("name", "?")
                            tool_output = event.get("data", {}).get("output", "")
                            tool_run_id = event.get("run_id", "")
                            logger.debug("SSE: tool end — %s", tool_end_name)
                            # Pair via run_id so parallel tool calls don't clobber each other.
                            entry = tool_trace_by_run.get(tool_run_id) if tool_run_id else None
                            if entry is not None:
                                entry["output_preview"] = str(tool_output)[:300] if tool_output else ""

                        elif kind == "on_chat_model_end":
                            # Capture the last completed AIMessage for token telemetry
                            output = event.get("data", {}).get("output")
                            if output is not None:
                                last_ai_message = output

                    # Record token usage from the last completed LLM call
                    if last_ai_message is not None:
                        record_llm_token_usage(llm_span, {"messages": [last_ai_message]})

                # Build final text from all accumulated streaming tokens
                final_text = "".join(accumulated).strip() or "(no response generated)"
                logger.info("A2A execute: response length=%d chars", len(final_text))

                # ── OA-02 / OA-06: Output PII redaction ──────────────────────
                if _COMPLIANCE_AVAILABLE and _compliance_cfg and _compliance_cfg.mode == "owasp_agentic":
                    final_text, _pii_types = _redact_pii(final_text)
                    if _pii_types:
                        from builtin_tools.audit import log_event as _audit_log
                        _audit_log(
                            event_type="compliance",
                            action="pii.redact",
                            resource="task_output",
                            outcome="redacted",
                            pii_types=_pii_types,
                            context_id=context_id,
                        )

                # ── OTEL: task_complete span ─────────────────────────────────
                with tracer.start_as_current_span("task_complete") as done_span:
                    done_span.set_attribute(WORKSPACE_ID_ATTR, _WORKSPACE_ID)
                    done_span.set_attribute(A2A_TASK_ID, context_id)
                    done_span.set_attribute("task.has_response", bool(accumulated))
                    done_span.set_attribute("task.response_length", len(final_text))

                # ── Step 3: emit final Message ────────────────────────────────
                # Non-streaming: ResultAggregator.consume_all() returns this
                #   immediately as the response (a2a_client.py reads .parts[0].text).
                # Streaming: yielded as the last SSE event in the stream.
                #
                # If the reply mentions /workspace/... paths, stage each one
                # and emit as FileParts alongside the text so the canvas can
                # render a download button. Same contract the hermes executor
                # uses — every runtime going through this code path (langgraph,
                # deepagents, future ReAct variants) inherits it.
                _outbound = collect_outbound_files(final_text)
                if _outbound:
                    # NOTE: do NOT re-import `Part` here. It is already imported
                    # at module scope (line 42). A function-scope `from a2a.types
                    # import ... Part ...` would mark `Part` as a local name
                    # throughout this function under Python's scoping rules,
                    # making the earlier `Part(text=text)` call (line ~358, inside
                    # the astream_events loop) raise UnboundLocalError because
                    # the local binding is not yet in scope at that point.
                    #
                    # a2a-sdk 1.x flattened the Part shape: 0.x used
                    # `Part(root=TextPart(text=...))` / `Part(root=FilePart(file=
                    # FileWithUri(uri=..., name=..., mimeType=...)))` (Pydantic
                    # discriminated-union style). 1.x's Part is a single proto
                    # message with flat fields: text, url, filename, media_type,
                    # raw, data, metadata. TextPart/FilePart/FileWithUri were
                    # removed. Same for Message: messageId/taskId/contextId
                    # camelCase became message_id/task_id/context_id.
                    from a2a.types import Message, Role
                    _parts: list[Part] = [Part(text=final_text)] if final_text else []
                    for f in _outbound:
                        _parts.append(Part(
                            url="workspace:" + f["path"],
                            filename=f["name"],
                            media_type=f["mime_type"],
                        ))
                    msg = Message(
                        message_id=uuid.uuid4().hex,
                        # 1.x Role is a protobuf enum: ROLE_UNSPECIFIED,
                        # ROLE_USER, ROLE_AGENT. Old `Role.agent` (Pydantic
                        # lowercase enum) doesn't exist anymore.
                        role=Role.ROLE_AGENT,
                        parts=_parts,
                        task_id=task_id,
                        context_id=context_id,
                    )
                else:
                    msg = new_text_message(final_text, task_id=task_id, context_id=context_id)
                # Attach tool_trace via metadata when supported. Guarded with
                # hasattr because some test mocks return a plain string here.
                if tool_trace and hasattr(msg, "metadata"):
                    try:
                        msg.metadata = {"tool_trace": tool_trace}
                    except (AttributeError, TypeError):
                        # `new_text_message()` returns a plain string in
                        # MagicMock paths in tests, where assignment to
                        # .metadata raises despite hasattr being true (the
                        # mock has the attribute as a property). Suppression
                        # is intentional — production Message objects always
                        # accept the assignment. See #1787 + commit dcbcf19
                        # for the original test-mock motivation.
                        logger.debug("metadata attach skipped (non-Message return from new_text_message)")
                await event_queue.enqueue_event(msg)
                _result = final_text

            except Exception as e:
                logger.error("A2A execute error: %s", e, exc_info=True)
                try:
                    task_span.record_exception(e)
                    from opentelemetry.trace import StatusCode
                    task_span.set_status(StatusCode.ERROR, str(e))
                except Exception:
                    pass
                # Emit a Message so both streaming and non-streaming clients
                # receive an error response rather than hanging.
                await event_queue.enqueue_event(
                    new_text_message(
                        f"Agent error: {e}", task_id=task_id, context_id=context_id
                    )
                )
            finally:
                await set_current_task(self._heartbeat, "")

        return _result

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        """Cancel a running task — emits canceled state to comply with A2A protocol."""
        from a2a.types import TaskStatus, TaskState, TaskStatusUpdateEvent
        await event_queue.enqueue_event(
            TaskStatusUpdateEvent(
                status=TaskStatus(state=TaskState.TASK_STATE_CANCELED),  # v1: TaskState uses SCREAMING_SNAKE_CASE
                final=True,
            )
        )
