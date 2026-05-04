"""Shared fixtures and module mocks for workspace-template tests.

Mocks the a2a SDK modules before any test imports a2a_executor,
since the a2a SDK is a heavy external dependency.
"""

import sys
from types import ModuleType
from unittest.mock import MagicMock


def _make_a2a_mocks():
    """Create mock modules for the a2a SDK with real base classes."""

    # a2a.server.agent_execution needs a real AgentExecutor base class
    agent_execution_mod = ModuleType("a2a.server.agent_execution")

    class AgentExecutor:
        """Stub base class for LangGraphA2AExecutor."""
        pass

    class RequestContext:
        """Stub for type hints."""
        pass

    agent_execution_mod.AgentExecutor = AgentExecutor
    agent_execution_mod.RequestContext = RequestContext

    # a2a.server.events needs a real EventQueue reference
    events_mod = ModuleType("a2a.server.events")

    class EventQueue:
        """Stub for type hints."""
        pass

    events_mod.EventQueue = EventQueue

    # a2a.server.tasks needs a TaskUpdater stub whose async methods are no-ops
    # for status transitions but ROUTE the terminal message back through
    # event_queue.enqueue_event so legacy assertions on enqueue_event keep
    # working. The wrapper preserves identity (the same Message object the
    # executor passed in) so tests inspecting str(event_arg) still see the
    # response text. complete()/failed() also record their last call on the
    # event_queue itself (`_complete_calls`, `_failed_calls`) so the v1
    # contract regression test (#262 follow-on to #2558) can pin the proper
    # path was taken — raw enqueue from executor would NOT touch these.
    tasks_mod = ModuleType("a2a.server.tasks")

    class TaskUpdater:
        """Stub TaskUpdater — terminal helpers route through event_queue."""

        def __init__(self, event_queue, task_id, context_id, *args, **kwargs):
            self.event_queue = event_queue
            self.task_id = task_id
            self.context_id = context_id
            if not hasattr(event_queue, "_complete_calls"):
                event_queue._complete_calls = []
            if not hasattr(event_queue, "_failed_calls"):
                event_queue._failed_calls = []

        async def start_work(self, message=None):
            pass

        async def complete(self, message=None):
            self.event_queue._complete_calls.append(message)
            if message is not None:
                await self.event_queue.enqueue_event(message)

        async def failed(self, message=None):
            self.event_queue._failed_calls.append(message)
            if message is not None:
                await self.event_queue.enqueue_event(message)

        async def add_artifact(
            self, parts, artifact_id=None, name=None, metadata=None,
            append=None, last_chunk=None, extensions=None
        ):
            pass

    tasks_mod.TaskUpdater = TaskUpdater

    # a2a.types needs stubs for Part, Message, Role.
    # v1 Part: flat protobuf with optional text/url/filename/media_type/raw/data fields.
    # v1 Message: has message_id, role, parts, task_id, context_id, etc.
    # Stubs preserve all kwargs so tests can assert on any field.
    types_mod = ModuleType("a2a.types")

    class Part:
        """Stub for A2A Part (v1: flat protobuf with optional fields)."""
        def __init__(self, text=None, root=None, **kwargs):
            self.text = text
            # Preserve every other kwarg as an attribute so tests can
            # assert on Part(url=..., filename=..., media_type=...).
            for k, v in kwargs.items():
                setattr(self, k, v)

    class Message:
        """Stub for A2A Message (v1: protobuf with snake_case fields)."""
        def __init__(self, message_id="", role=0, parts=None, task_id="",
                     context_id="", **kwargs):
            self.message_id = message_id
            self.role = role
            self.parts = list(parts) if parts is not None else []
            self.task_id = task_id
            self.context_id = context_id
            for k, v in kwargs.items():
                setattr(self, k, v)

    class _RoleEnum:
        """Stub for A2A Role enum (v1 protobuf: ROLE_UNSPECIFIED=0, ROLE_USER=1, ROLE_AGENT=2)."""
        ROLE_UNSPECIFIED = 0
        ROLE_USER = 1
        ROLE_AGENT = 2

    types_mod.Part = Part
    types_mod.Message = Message
    types_mod.Role = _RoleEnum

    # v1 Task / TaskStatus / TaskState — used by the executor's "enqueue Task
    # before any TaskStatusUpdateEvent" guard (a2a-sdk ≥ 1.0 contract). The
    # stubs preserve every kwarg so tests can assert on Task(id=..., status=...).
    class TaskStatus:
        def __init__(self, state=None, **kwargs):
            self.state = state
            for k, v in kwargs.items():
                setattr(self, k, v)

    class _TaskStateEnum:
        TASK_STATE_SUBMITTED = 1
        TASK_STATE_WORKING = 2
        TASK_STATE_COMPLETED = 3
        TASK_STATE_CANCELED = 4
        TASK_STATE_FAILED = 5
        TASK_STATE_REJECTED = 6

    class Task:
        def __init__(self, id="", context_id="", status=None, **kwargs):
            self.id = id
            self.context_id = context_id
            self.status = status
            for k, v in kwargs.items():
                setattr(self, k, v)

    types_mod.Task = Task
    types_mod.TaskStatus = TaskStatus
    types_mod.TaskState = _TaskStateEnum

    # v1 AgentCard / AgentSkill / AgentCapabilities / AgentInterface — used
    # by main.py's static-card construction (PR #2756) and by
    # card_helpers.enrich_card_skills's swap path. Stubs preserve kwargs so
    # tests can assert on card.skills[i].name etc., and let card.skills be
    # reassigned in place (the production code's enrichment pattern).
    class AgentSkill:
        def __init__(self, id="", name="", description="", tags=None, examples=None, **kwargs):
            self.id = id
            self.name = name
            self.description = description
            self.tags = list(tags) if tags is not None else []
            self.examples = list(examples) if examples is not None else []
            for k, v in kwargs.items():
                setattr(self, k, v)

    class AgentCapabilities:
        def __init__(self, **kwargs):
            for k, v in kwargs.items():
                setattr(self, k, v)

    class AgentInterface:
        def __init__(self, **kwargs):
            for k, v in kwargs.items():
                setattr(self, k, v)

    class AgentCard:
        def __init__(self, **kwargs):
            self.skills = []
            for k, v in kwargs.items():
                setattr(self, k, v)

    types_mod.AgentSkill = AgentSkill
    types_mod.AgentCapabilities = AgentCapabilities
    types_mod.AgentInterface = AgentInterface
    types_mod.AgentCard = AgentCard

    # a2a.server.routes — used by boot_routes.build_routes (PR #2756 chain
    # / #2761) to mount /.well-known/agent-card.json. The real SDK builds
    # a Starlette route that serializes the card on each request; the stub
    # mirrors that behaviour with json.dumps over the card's __dict__ so
    # TestClient.get("/.well-known/agent-card.json") returns the same
    # shape canvas would see in production.
    routes_mod = ModuleType("a2a.server.routes")

    def _create_agent_card_routes(card):
        from starlette.responses import JSONResponse
        from starlette.routing import Route

        async def _card_handler(_request):
            # Convert the stub AgentCard into a JSON-serialisable dict.
            # Real a2a.types.AgentCard is a Pydantic model with proper
            # serialisation; the stub stores attrs raw, so we walk
            # __dict__ and serialise nested AgentSkill objects too.
            def _to_dict(obj):
                if hasattr(obj, "__dict__"):
                    return {k: _to_dict(v) for k, v in vars(obj).items()}
                if isinstance(obj, list):
                    return [_to_dict(x) for x in obj]
                if isinstance(obj, dict):
                    return {k: _to_dict(v) for k, v in obj.items()}
                return obj

            return JSONResponse(_to_dict(card))

        return [Route("/.well-known/agent-card.json", _card_handler, methods=["GET"])]

    def _create_jsonrpc_routes(request_handler=None, rpc_url="/", **_kwargs):
        from starlette.responses import JSONResponse
        from starlette.routing import Route

        async def _jsonrpc_handler(_request):
            # Stub: real DefaultRequestHandler dispatches to the executor;
            # tests that need real behaviour will use a test-side mock.
            # This stub just returns a JSON-RPC envelope so the not-configured
            # branch's discriminator (`error.data` containing "setup() failed")
            # has something to differ from.
            return JSONResponse({"jsonrpc": "2.0", "result": "stub-jsonrpc-handler"})

        return [Route(rpc_url, _jsonrpc_handler, methods=["POST"])]

    routes_mod.create_agent_card_routes = _create_agent_card_routes
    routes_mod.create_jsonrpc_routes = _create_jsonrpc_routes
    sys.modules["a2a.server.routes"] = routes_mod

    # a2a.server.request_handlers — used by boot_routes' executor branch.
    # DefaultRequestHandler stub takes the same kwargs as the real one;
    # tests that exercise the executor path don't poke at the handler's
    # internals, only that it gets mounted at "/".
    rh_mod = ModuleType("a2a.server.request_handlers")

    class DefaultRequestHandler:
        def __init__(self, agent_executor=None, task_store=None, agent_card=None, **_kwargs):
            self.agent_executor = agent_executor
            self.task_store = task_store
            self.agent_card = agent_card

    rh_mod.DefaultRequestHandler = DefaultRequestHandler
    sys.modules["a2a.server.request_handlers"] = rh_mod

    # InMemoryTaskStore is exposed via a2a.server.tasks (already stubbed
    # above with TaskUpdater). Add it as a no-op class.
    class _InMemoryTaskStore:
        def __init__(self):
            pass

    tasks_mod.InMemoryTaskStore = _InMemoryTaskStore

    # a2a.helpers (v1: moved from a2a.utils, renamed new_agent_text_message
    # → new_text_message). Mock both names — production code only calls
    # new_text_message, but if any test still references the old name it
    # gets the same lambda for backward compat during the rename rollout.
    helpers_mod = ModuleType("a2a.helpers")
    helpers_mod.new_text_message = lambda text, **kwargs: text
    helpers_mod.new_agent_text_message = helpers_mod.new_text_message

    # Register all module paths
    a2a_mod = ModuleType("a2a")
    a2a_server_mod = ModuleType("a2a.server")

    sys.modules["a2a"] = a2a_mod
    sys.modules["a2a.server"] = a2a_server_mod
    sys.modules["a2a.server.agent_execution"] = agent_execution_mod
    sys.modules["a2a.server.events"] = events_mod
    sys.modules["a2a.server.tasks"] = tasks_mod
    sys.modules["a2a.types"] = types_mod
    sys.modules["a2a.helpers"] = helpers_mod


def _make_langchain_mocks():
    """Create mock modules for langchain_core so coordinator.py can be imported."""
    langchain_core_mod = ModuleType("langchain_core")
    langchain_core_tools_mod = ModuleType("langchain_core.tools")
    # Make @tool a no-op decorator
    langchain_core_tools_mod.tool = lambda f: f

    sys.modules["langchain_core"] = langchain_core_mod
    sys.modules["langchain_core.tools"] = langchain_core_tools_mod


def _make_tools_mocks():
    """Create mock modules for tools.* so adapters can be imported in tests."""
    tools_mod = ModuleType("builtin_tools")
    tools_mod.__path__ = []  # Make it a proper package

    tools_delegation_mod = ModuleType("builtin_tools.delegation")
    tools_delegation_mod.delegate_task = MagicMock()
    tools_delegation_mod.delegate_task.name = "delegate_task"
    tools_delegation_mod.delegate_task_async = MagicMock()
    tools_delegation_mod.delegate_task_async.name = "delegate_task_async"
    tools_delegation_mod.check_task_status = MagicMock()
    tools_delegation_mod.check_task_status.name = "check_task_status"

    tools_approval_mod = ModuleType("builtin_tools.approval")
    tools_approval_mod.request_approval = MagicMock()
    tools_approval_mod.request_approval.name = "request_approval"

    tools_memory_mod = ModuleType("builtin_tools.memory")
    tools_memory_mod.commit_memory = MagicMock()
    tools_memory_mod.commit_memory.name = "commit_memory"
    tools_memory_mod.recall_memory = MagicMock()
    tools_memory_mod.recall_memory.name = "recall_memory"

    tools_sandbox_mod = ModuleType("builtin_tools.sandbox")
    tools_sandbox_mod.run_code = MagicMock()
    tools_sandbox_mod.run_code.name = "run_code"

    tools_a2a_mod = ModuleType("builtin_tools.a2a_tools")
    tools_a2a_mod.delegate_task = MagicMock()
    tools_a2a_mod.list_peers = MagicMock()
    tools_a2a_mod.get_peers_summary = MagicMock()

    tools_awareness_mod = ModuleType("builtin_tools.awareness_client")
    tools_awareness_mod.get_awareness_config = MagicMock(return_value=None)

    # tools.telemetry — provide constants and no-op callables used by a2a_executor
    from contextvars import ContextVar
    tools_telemetry_mod = ModuleType("builtin_tools.telemetry")
    tools_telemetry_mod.GEN_AI_SYSTEM = "gen_ai.system"
    tools_telemetry_mod.GEN_AI_REQUEST_MODEL = "gen_ai.request.model"
    tools_telemetry_mod.GEN_AI_OPERATION_NAME = "gen_ai.operation.name"
    tools_telemetry_mod.GEN_AI_USAGE_INPUT_TOKENS = "gen_ai.usage.input_tokens"
    tools_telemetry_mod.GEN_AI_USAGE_OUTPUT_TOKENS = "gen_ai.usage.output_tokens"
    tools_telemetry_mod.GEN_AI_RESPONSE_FINISH_REASONS = "gen_ai.response.finish_reasons"
    tools_telemetry_mod.WORKSPACE_ID_ATTR = "workspace.id"
    tools_telemetry_mod.A2A_TASK_ID = "a2a.task_id"
    tools_telemetry_mod.A2A_SOURCE_WORKSPACE = "a2a.source_workspace_id"
    tools_telemetry_mod.A2A_TARGET_WORKSPACE = "a2a.target_workspace_id"
    tools_telemetry_mod.MEMORY_SCOPE = "memory.scope"
    tools_telemetry_mod.MEMORY_QUERY = "memory.query"
    tools_telemetry_mod._incoming_trace_context = ContextVar("otel_incoming_trace_context", default=None)
    tools_telemetry_mod.get_tracer = MagicMock(return_value=MagicMock())
    tools_telemetry_mod.setup_telemetry = MagicMock()
    tools_telemetry_mod.make_trace_middleware = MagicMock(side_effect=lambda app: app)
    tools_telemetry_mod.inject_trace_headers = MagicMock(side_effect=lambda h: h)
    tools_telemetry_mod.extract_trace_context = MagicMock(return_value=None)
    tools_telemetry_mod.get_current_traceparent = MagicMock(return_value=None)
    tools_telemetry_mod.gen_ai_system_from_model = lambda m: m.split(":")[0] if ":" in m else "unknown"
    tools_telemetry_mod.record_llm_token_usage = MagicMock()

    # tools.audit — provide RBAC helpers and log_event as no-ops
    tools_audit_mod = ModuleType("builtin_tools.audit")
    tools_audit_mod.log_event = MagicMock(return_value="mock-trace-id")
    tools_audit_mod.check_permission = MagicMock(return_value=True)
    tools_audit_mod.get_workspace_roles = MagicMock(return_value=(["operator"], {}))
    tools_audit_mod.ROLE_PERMISSIONS = {
        "admin": {"delegate", "approve", "memory.read", "memory.write"},
        "operator": {"delegate", "approve", "memory.read", "memory.write"},
        "read-only": {"memory.read"},
    }

    # tools.hitl — lightweight stubs for the HITL tools
    tools_hitl_mod = ModuleType("builtin_tools.hitl")
    tools_hitl_mod.pause_task = MagicMock()
    tools_hitl_mod.pause_task.name = "pause_task"
    tools_hitl_mod.resume_task = MagicMock()
    tools_hitl_mod.resume_task.name = "resume_task"
    tools_hitl_mod.list_paused_tasks = MagicMock()
    tools_hitl_mod.list_paused_tasks.name = "list_paused_tasks"
    tools_hitl_mod.requires_approval = MagicMock(side_effect=lambda *a, **kw: (lambda f: f))
    tools_hitl_mod.pause_registry = MagicMock()

    # builtin_tools.security — load the real module so _redact_secrets is
    # available to executor_helpers, a2a_tools, and any other module that
    # imports from it.  The module is pure-Python with no external deps.
    import importlib.util as _ilu
    import os as _os
    _sec_path = _os.path.join(
        _os.path.dirname(_os.path.dirname(_os.path.abspath(__file__))),
        "builtin_tools", "security.py",
    )
    _sec_spec = _ilu.spec_from_file_location("builtin_tools.security", _sec_path)
    _sec_mod = _ilu.module_from_spec(_sec_spec)
    _sec_spec.loader.exec_module(_sec_mod)

    sys.modules["builtin_tools"] = tools_mod
    sys.modules["builtin_tools.delegation"] = tools_delegation_mod
    sys.modules["builtin_tools.approval"] = tools_approval_mod
    sys.modules["builtin_tools.memory"] = tools_memory_mod
    sys.modules["builtin_tools.sandbox"] = tools_sandbox_mod
    sys.modules["builtin_tools.a2a_tools"] = tools_a2a_mod
    sys.modules["builtin_tools.awareness_client"] = tools_awareness_mod
    sys.modules["builtin_tools.telemetry"] = tools_telemetry_mod
    sys.modules["builtin_tools.audit"] = tools_audit_mod
    sys.modules["builtin_tools.hitl"] = tools_hitl_mod
    sys.modules["builtin_tools.security"] = _sec_mod


# Install mocks before any test collection imports a2a_executor
if "a2a" not in sys.modules:
    _make_a2a_mocks()

# Note: the claude_agent_sdk stub was removed alongside
# workspace/claude_sdk_executor.py (#87 Phase 2). The executor + its
# tests now live in the claude-code template repo, where the real SDK
# IS installed via Dockerfile, so no stub is needed.

if "langchain_core" not in sys.modules:
    _make_langchain_mocks()

if "builtin_tools" not in sys.modules or not hasattr(sys.modules.get("builtin_tools"), "__path__"):
    _make_tools_mocks()

# Mock additional modules needed by _common_setup in base.py
if "plugins" not in sys.modules:
    plugins_mod = ModuleType("plugins")
    plugins_mod.load_plugins = MagicMock()
    sys.modules["plugins"] = plugins_mod

if "skill_loader" not in sys.modules:
    # Add workspace-template to path so real skills.loader can be imported
    import importlib.util
    _ws_root = str(MagicMock.__module__).replace("unittest.mock", "")  # just a trick to get path
    import os as _os
    _ws_root = _os.path.dirname(_os.path.dirname(_os.path.abspath(__file__)))
    if _ws_root not in sys.path:
        sys.path.insert(0, _ws_root)
    # Import real skills module so LoadedSkill/SkillMetadata are available
    skills_mod = ModuleType("skill_loader")
    skills_mod.__path__ = [_os.path.join(_ws_root, "skill_loader")]
    sys.modules["skill_loader"] = skills_mod
    _spec = importlib.util.spec_from_file_location("skill_loader.loader", _os.path.join(_ws_root, "skill_loader", "loader.py"))
    _loader_mod = importlib.util.module_from_spec(_spec)
    sys.modules["skill_loader.loader"] = _loader_mod
    _spec.loader.exec_module(_loader_mod)

if "coordinator" not in sys.modules:
    # Try importing real coordinator first
    try:
        import coordinator as _coord  # noqa: F401
    except (ImportError, RuntimeError):
        coordinator_mod = ModuleType("coordinator")
        coordinator_mod.get_children = MagicMock()
        coordinator_mod.get_parent_context = MagicMock()
        coordinator_mod.build_children_description = MagicMock()
        coordinator_mod.route_task_to_team = MagicMock()
        coordinator_mod.route_task_to_team.name = "route_task_to_team"
        sys.modules["coordinator"] = coordinator_mod

# Don't mock prompt or coordinator if they can be imported from the workspace-template dir
# test_prompt.py and test_coordinator.py need the real modules



# ─── runtime_wedge cross-test isolation ─────────────────────────────────
#
# `runtime_wedge` carries module-scope state via the `_DEFAULT` instance
# (workspace/runtime_wedge.py). Any test that calls `mark_wedged` and
# doesn't clean up leaks a sticky wedge into every later test in the
# same pytest process. Smoke tests (test_smoke_mode.py) that read
# `is_wedged()` would then fail-via-leak instead of assessing the code
# under test.
#
# Autouse fixture is scoped to the workspace/tests/ tree (this conftest
# is at workspace/tests/conftest.py), so it runs for every test that
# touches the runtime — without each test having to opt in. The
# import is deferred to fixture-call time so the fixture also works
# in environments where runtime_wedge isn't yet importable (matches
# the fail-open posture that smoke_mode + heartbeat take at the
# consumer side).
import pytest as _pytest  # alias to avoid colliding with any existing `pytest` name


@_pytest.fixture(autouse=True)
def _reset_runtime_wedge_between_tests():
    """Reset the universal runtime_wedge flag before AND after every
    workspace test so module-scope state can't leak across tests.

    A test that calls `mark_wedged` without cleanup would otherwise
    contaminate the next test's `is_wedged()` read — and because the
    flag is sticky-first-write-wins, the later test couldn't even
    overwrite the leaked reason. Two-sided reset (yield + cleanup)
    means an early failure also doesn't poison the rest of the run.
    """
    try:
        from runtime_wedge import reset_for_test
    except (ImportError, ModuleNotFoundError):
        # No runtime_wedge installed — nothing to reset. Yield as a
        # no-op so the fixture still runs the test.
        yield
        return
    reset_for_test()
    yield
    reset_for_test()
