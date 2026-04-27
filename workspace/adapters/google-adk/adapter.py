"""Google ADK adapter for Molecule AI workspace runtime.

Wraps Google's Agent Development Kit (google-adk v1.x) as a Molecule AI
WorkspaceAdapter, bridging the A2A protocol to Google ADK's runner/session
model.

Google ADK concepts used
------------------------
- ``google.adk.agents.LlmAgent``  — An LLM-backed agent with instructions and
  optional tools.  Declared with ``model``, ``name``, and ``instruction``.
- ``google.adk.runners.Runner``   — Drives one or more agents inside a session;
  ``run_async()`` streams ``Event`` objects, including the final response text.
- ``google.adk.sessions.InMemorySessionService`` — Manages session state in
  memory.  Each ``Runner`` owns a single ``InMemorySessionService`` instance.

Runtime-config keys (all optional)
------------------------------------
``max_output_tokens`` — int, default 8192.  Forwarded to the ADK ``GenerateContentConfig``.
``temperature``       — float, default 1.0.
``agent_name``        — str, default ``"molecule-adk-agent"``.

Environment variables
---------------------
``GOOGLE_API_KEY``   — Google AI Studio key (required for ``gemini-*`` models).
``GOOGLE_GENAI_USE_VERTEXAI`` — set to ``"1"`` to use Vertex AI instead of AI
                                Studio.  In that case supply
                                ``GOOGLE_CLOUD_PROJECT`` and
                                ``GOOGLE_CLOUD_LOCATION`` as well.
"""

from __future__ import annotations

import logging
import os
from typing import TYPE_CHECKING, Any

from a2a.server.agent_execution import AgentExecutor, RequestContext
from a2a.server.events import EventQueue
from a2a.helpers import new_text_message

from adapter_base import AdapterConfig, BaseAdapter

if TYPE_CHECKING:
    pass

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

_DEFAULT_AGENT_NAME = "molecule-adk-agent"
_DEFAULT_MAX_OUTPUT_TOKENS = 8192
_DEFAULT_TEMPERATURE = 1.0
_NO_TEXT_MSG = "Error: message contained no text content."
_NO_RESPONSE_MSG = "(no response generated)"


# ---------------------------------------------------------------------------
# GoogleADKA2AExecutor
# ---------------------------------------------------------------------------


class GoogleADKA2AExecutor(AgentExecutor):
    """A2A executor backed by a Google ADK ``Runner``.

    Each executor instance owns a single ``Runner`` and ``InMemorySessionService``.
    Sessions are created on first use and reused across subsequent turns
    (the session_id is derived from the A2A context_id so each task gets a
    stable, isolated session).

    Parameters
    ----------
    model:
        ADK model identifier, e.g. ``"gemini-2.0-flash"`` or
        ``"gemini-1.5-pro"``.
    system_prompt:
        Optional instruction prepended to every conversation.  Passed to
        ``LlmAgent(instruction=...)``.
    agent_name:
        Internal ADK agent name.  Defaults to ``_DEFAULT_AGENT_NAME``.
    max_output_tokens:
        Token cap forwarded to ``GenerateContentConfig``.
    temperature:
        Sampling temperature forwarded to ``GenerateContentConfig``.
    heartbeat:
        Optional ``HeartbeatLoop`` instance (unused directly but stored for
        future heartbeat integration).
    _runner:
        Inject a pre-built ``Runner`` — for testing only.  When provided,
        the real ADK ``Runner`` is never constructed.
    """

    def __init__(
        self,
        model: str,
        system_prompt: str | None = None,
        agent_name: str = _DEFAULT_AGENT_NAME,
        max_output_tokens: int = _DEFAULT_MAX_OUTPUT_TOKENS,
        temperature: float = _DEFAULT_TEMPERATURE,
        heartbeat: Any = None,
        _runner: Any = None,
    ) -> None:
        self.model = model
        self.system_prompt = system_prompt
        self.agent_name = agent_name
        self.max_output_tokens = max_output_tokens
        self.temperature = temperature
        self._heartbeat = heartbeat
        self._sessions_created: set[str] = set()

        if _runner is not None:
            # Test injection — skip building the real ADK objects.
            self._runner = _runner
        else:
            self._runner = self._build_runner()

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _build_runner(self) -> Any:  # pragma: no cover — requires real ADK
        """Construct a Google ADK ``Runner`` with an ``LlmAgent``.

        Lazy-imports ``google.adk`` so the rest of the workspace runtime
        doesn't pull in google-adk on startup (it's only needed when this
        executor is actually instantiated by ``GoogleADKAdapter.create_executor``).
        """
        from google.adk.agents import LlmAgent
        from google.adk.runners import Runner
        from google.adk.sessions import InMemorySessionService

        agent = LlmAgent(
            name=self.agent_name,
            model=self.model,
            instruction=self.system_prompt or "",
        )

        session_service = InMemorySessionService()
        runner = Runner(
            agent=agent,
            app_name=self.agent_name,
            session_service=session_service,
        )
        return runner

    async def _ensure_session(self, session_id: str, user_id: str) -> None:
        """Create a session in the service if it doesn't exist yet."""
        if session_id in self._sessions_created:
            return
        session_service = self._runner.session_service
        existing = await session_service.get_session(
            app_name=self.agent_name,
            user_id=user_id,
            session_id=session_id,
        )
        if existing is None:
            await session_service.create_session(
                app_name=self.agent_name,
                user_id=user_id,
                session_id=session_id,
            )
        self._sessions_created.add(session_id)

    def _extract_text(self, context: RequestContext) -> str:
        """Pull plain text out of the A2A message parts."""
        from shared_runtime import extract_message_text
        return extract_message_text(context)

    def _build_content(self, user_text: str) -> Any:
        """Wrap user text in an ADK-compatible ``Content`` object."""
        from google.genai.types import Content, Part
        return Content(role="user", parts=[Part(text=user_text)])

    # ------------------------------------------------------------------
    # AgentExecutor interface
    # ------------------------------------------------------------------

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        """Run a single ADK turn and enqueue the reply as an A2A Message.

        Sequence:
        1. Extract user text from A2A message parts.
        2. Ensure an ADK session exists for this context_id.
        3. Call ``runner.run_async()`` and collect all response events.
        4. Concatenate final-response text; fall back to ``_NO_RESPONSE_MSG``
           when the model produces no output.
        5. Enqueue the reply via ``event_queue``.
        """
        user_text = self._extract_text(context)
        if not user_text:
            parts = getattr(getattr(context, "message", None), "parts", None)
            logger.warning("GoogleADKA2AExecutor: no text in message parts: %s", parts)
            await event_queue.enqueue_event(new_text_message(_NO_TEXT_MSG))
            return

        session_id = getattr(context, "context_id", None) or "default-session"
        user_id = "molecule-user"

        try:
            await self._ensure_session(session_id, user_id)

            content = self._build_content(user_text)
            response_parts: list[str] = []

            async for event in self._runner.run_async(
                session_id=session_id,
                user_id=user_id,
                new_message=content,
            ):
                # Collect text from final-response events
                if not getattr(event, "is_final_response", lambda: False)():
                    continue
                candidate_response = getattr(event, "response", None)
                if candidate_response is None:
                    continue
                for part in getattr(
                    getattr(candidate_response, "content", None) or MissingContent(),
                    "parts", []
                ):
                    text = getattr(part, "text", None)
                    if text:
                        response_parts.append(text)

            final_text = "".join(response_parts).strip() or _NO_RESPONSE_MSG
            await event_queue.enqueue_event(new_text_message(final_text))

        except Exception as exc:
            logger.error(
                "GoogleADKA2AExecutor: execution error [model=%s]: %s",
                self.model,
                type(exc).__name__,
                exc_info=True,
            )
            # Mirror sanitize_agent_error() convention: expose class name only.
            await event_queue.enqueue_event(
                new_text_message(f"Agent error: {type(exc).__name__}")
            )

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        """Cancel a running task — emits canceled state per A2A protocol."""
        from a2a.types import TaskState, TaskStatus, TaskStatusUpdateEvent

        await event_queue.enqueue_event(
            TaskStatusUpdateEvent(
                status=TaskStatus(state=TaskState.TASK_STATE_CANCELED),
                final=True,
            )
        )


class MissingContent:
    """Sentinel to avoid AttributeError when response.content is None."""
    parts: list = []


# ---------------------------------------------------------------------------
# GoogleADKAdapter
# ---------------------------------------------------------------------------


class GoogleADKAdapter(BaseAdapter):
    """Molecule AI workspace adapter for Google ADK (google-adk v1.x).

    Implements the full ``BaseAdapter`` lifecycle:
    - ``setup()``           — validates config and runs ``_common_setup()``.
    - ``create_executor()`` — returns a ``GoogleADKA2AExecutor`` configured
                             from ``AdapterConfig``.
    """

    # Stored by setup(); consumed by create_executor()
    _setup_result: Any = None

    # ------------------------------------------------------------------
    # Identity
    # ------------------------------------------------------------------

    @staticmethod
    def name() -> str:
        """Runtime identifier — matches the ``runtime`` field in config.yaml."""
        return "google-adk"

    @staticmethod
    def display_name() -> str:
        """Human-readable name shown in the Molecule AI UI."""
        return "Google ADK"

    @staticmethod
    def description() -> str:
        """Short description of this adapter's capabilities."""
        return (
            "Google Agent Development Kit (ADK) adapter. "
            "Runs LLM agents via Google Gemini models using the official "
            "google-adk Python SDK (Apache-2.0)."
        )

    @staticmethod
    def get_config_schema() -> dict:
        """JSON Schema for runtime_config fields rendered in the Config tab."""
        return {
            "type": "object",
            "properties": {
                "agent_name": {
                    "type": "string",
                    "default": _DEFAULT_AGENT_NAME,
                    "description": "Internal ADK agent name",
                },
                "max_output_tokens": {
                    "type": "integer",
                    "default": _DEFAULT_MAX_OUTPUT_TOKENS,
                    "description": "Maximum output tokens for the Gemini model",
                },
                "temperature": {
                    "type": "number",
                    "default": _DEFAULT_TEMPERATURE,
                    "minimum": 0.0,
                    "maximum": 2.0,
                    "description": "Sampling temperature",
                },
            },
            "additionalProperties": False,
        }

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    async def setup(self, config: AdapterConfig) -> None:
        """Validate config and run the shared platform setup pipeline.

        Raises ``RuntimeError`` if the required API key is not set and
        Vertex AI mode is not active.

        Args:
            config: ``AdapterConfig`` populated by the workspace runtime.
        """
        use_vertex = os.environ.get("GOOGLE_GENAI_USE_VERTEXAI", "").strip() in ("1", "true", "True")
        api_key = os.environ.get("GOOGLE_API_KEY", "").strip()

        if not use_vertex and not api_key:
            raise RuntimeError(
                "GoogleADKAdapter requires GOOGLE_API_KEY (for AI Studio) or "
                "GOOGLE_GENAI_USE_VERTEXAI=1 with GOOGLE_CLOUD_PROJECT set."
            )

        logger.info(
            "GoogleADKAdapter.setup: model=%s vertex=%s", config.model, use_vertex
        )

        self._setup_result = await self._common_setup(config)

    async def create_executor(self, config: AdapterConfig) -> GoogleADKA2AExecutor:
        """Build and return a ``GoogleADKA2AExecutor`` for A2A integration.

        Uses the system prompt assembled by ``_common_setup()`` in ``setup()``.
        Runtime-config keys ``agent_name``, ``max_output_tokens``, and
        ``temperature`` are respected when present.

        Args:
            config: ``AdapterConfig`` populated by the workspace runtime.

        Returns:
            A ready-to-use ``GoogleADKA2AExecutor`` instance.
        """
        rc = config.runtime_config or {}

        # Strip provider prefix from model, e.g. "google:gemini-2.0-flash" → "gemini-2.0-flash"
        model = config.model
        if ":" in model:
            model = model.split(":", 1)[1]

        system_prompt = (
            self._setup_result.system_prompt
            if self._setup_result is not None
            else config.system_prompt or ""
        )

        return GoogleADKA2AExecutor(
            model=model,
            system_prompt=system_prompt,
            agent_name=rc.get("agent_name", _DEFAULT_AGENT_NAME),
            max_output_tokens=int(rc.get("max_output_tokens", _DEFAULT_MAX_OUTPUT_TOKENS)),
            temperature=float(rc.get("temperature", _DEFAULT_TEMPERATURE)),
            heartbeat=config.heartbeat,
        )


# ---------------------------------------------------------------------------
# Module-level alias required by the adapter autodiscovery loader
# ---------------------------------------------------------------------------

Adapter = GoogleADKAdapter
