"""Tests for workspace memory tools and awareness routing."""

import asyncio
import json
import importlib.util
import sys
from pathlib import Path

import pytest


ROOT = Path(__file__).resolve().parents[1]
TOOLS_DIR = ROOT / "builtin_tools"


def _load_module(module_name: str, file_path: Path):
    spec = importlib.util.spec_from_file_location(module_name, file_path)
    module = importlib.util.module_from_spec(spec)
    assert spec is not None
    assert spec.loader is not None
    sys.modules[module_name] = module
    spec.loader.exec_module(module)
    return module


@pytest.fixture
def memory_modules(monkeypatch):
    """Load the tools package modules from disk for focused unit tests."""
    monkeypatch.setenv("PLATFORM_URL", "http://platform.test")
    monkeypatch.setenv("WORKSPACE_ID", "ws-test")
    monkeypatch.delenv("AWARENESS_URL", raising=False)
    monkeypatch.delenv("AWARENESS_NAMESPACE", raising=False)

    tools_pkg = sys.modules.get("builtin_tools")
    original_tools_memory = sys.modules.pop("builtin_tools.memory", None)
    original_tools_awareness = sys.modules.pop("builtin_tools.awareness_client", None)

    if tools_pkg is not None:
        monkeypatch.setattr(tools_pkg, "__path__", [str(TOOLS_DIR)], raising=False)

    awareness_client = _load_module("builtin_tools.awareness_client", TOOLS_DIR / "awareness_client.py")
    memory = _load_module("builtin_tools.memory", TOOLS_DIR / "memory.py")

    yield memory, awareness_client

    if original_tools_memory is not None:
        sys.modules["builtin_tools.memory"] = original_tools_memory
    else:
        sys.modules.pop("builtin_tools.memory", None)

    if original_tools_awareness is not None:
        sys.modules["builtin_tools.awareness_client"] = original_tools_awareness
    else:
        sys.modules.pop("builtin_tools.awareness_client", None)


class _FakeResponse:
    def __init__(self, status_code, payload):
        self.status_code = status_code
        self._payload = payload
        self.text = str(payload)

    def json(self):
        return self._payload


def test_commit_memory_uses_awareness_client_when_configured(monkeypatch, memory_modules):
    memory, _awareness_client = memory_modules
    captured = {}

    class FakeAsyncClient:
        def __init__(self, timeout):
            captured["timeout"] = timeout

        async def __aenter__(self):
            return self

        async def __aexit__(self, exc_type, exc, tb):
            return None

        async def post(self, url, json, headers=None):
            # Only capture the memories write — _record_memory_activity
            # fires a second /activity post that would overwrite
            # captured["url"] otherwise.
            if "/memories" in url:
                captured["url"] = url
                captured["json"] = json
            return _FakeResponse(201, {"id": "mem-123"})

    monkeypatch.setenv("AWARENESS_URL", "http://awareness.test")
    monkeypatch.setenv("AWARENESS_NAMESPACE", "ws-test")
    monkeypatch.setattr(memory.httpx, "AsyncClient", FakeAsyncClient)

    result = asyncio.run(memory.commit_memory("remember this", "team"))

    assert result == {"success": True, "id": "mem-123", "scope": "TEAM"}
    assert captured["url"] == "http://awareness.test/api/v1/namespaces/ws-test/memories"
    assert captured["json"] == {"content": "remember this", "scope": "TEAM"}


def test_recall_memory_uses_platform_fallback_without_awareness(monkeypatch, memory_modules):
    memory, _awareness_client = memory_modules
    captured = {}

    class FakeAsyncClient:
        def __init__(self, timeout):
            captured["timeout"] = timeout

        async def __aenter__(self):
            return self

        async def __aexit__(self, exc_type, exc, tb):
            return None

        async def get(self, url, params, headers=None):
            captured["url"] = url
            captured["params"] = params
            return _FakeResponse(200, [{"content": "existing"}])

    monkeypatch.setattr(memory.httpx, "AsyncClient", FakeAsyncClient)

    result = asyncio.run(memory.recall_memory("status", "local"))

    assert result == {
        "success": True,
        "count": 1,
        "memories": [{"content": "existing"}],
    }
    assert captured["url"] == "http://platform.test/workspaces/ws-test/memories"
    assert captured["params"] == {"q": "status", "scope": "LOCAL"}


def test_commit_memory_uses_platform_fallback_without_awareness(monkeypatch, memory_modules):
    memory, _awareness_client = memory_modules
    captured = {}

    class FakeAsyncClient:
        def __init__(self, timeout):
            captured["timeout"] = timeout

        async def __aenter__(self):
            return self

        async def __aexit__(self, exc_type, exc, tb):
            return None

        async def post(self, url, json, headers=None):
            # commit_memory first hits /workspaces/:id/memories (the fix
            # under test), then _record_memory_activity hits /activity as
            # a fire-and-forget follow-up. Filter to only capture the
            # memories call so the subsequent activity post doesn't
            # overwrite captured["url"].
            if "/memories" in url:
                captured["url"] = url
                captured["json"] = json
            return _FakeResponse(201, {"id": "platform-mem"})

    monkeypatch.setattr(memory.httpx, "AsyncClient", FakeAsyncClient)

    result = asyncio.run(memory.commit_memory("remember fallback", "global"))

    assert result == {"success": True, "id": "platform-mem", "scope": "GLOBAL"}
    assert captured["url"] == "http://platform.test/workspaces/ws-test/memories"
    assert captured["json"] == {"content": "remember fallback", "scope": "GLOBAL"}


def test_commit_memory_promoted_packet_logs_skill_promotion(monkeypatch, tmp_path, memory_modules):
    memory, _awareness_client = memory_modules
    captured = {"calls": []}

    class FakeAsyncClient:
        def __init__(self, timeout):
            captured.setdefault("timeouts", []).append(timeout)

        async def __aenter__(self):
            return self

        async def __aexit__(self, exc_type, exc, tb):
            return None

        async def post(self, url, json, headers=None):
            captured["calls"].append((url, json))
            if url.endswith("/memories"):
                return _FakeResponse(201, {"id": "mem-skill"})
            if url.endswith("/activity"):
                return _FakeResponse(200, {"status": "logged"})
            if url.endswith("/registry/heartbeat"):
                return _FakeResponse(200, {"status": "ok"})
            raise AssertionError(f"unexpected URL: {url}")

    monkeypatch.setattr(memory.httpx, "AsyncClient", FakeAsyncClient)

    packet = {
        "title": "Normalize webhook ingress",
        "summary": "Repeated GitHub webhook handling is now a skill candidate",
        "promote_to_skill": True,
        "repetition_signal": {
            "count": 2,
            "workflow": "github webhook ingress",
        },
        "what changed": "The same webhook normalization was done twice cleanly.",
        "why it matters": "It is now stable enough to promote into SKILL.md.",
    }

    result = asyncio.run(memory.commit_memory(json.dumps(packet), "team"))

    assert result == {"success": True, "id": "mem-skill", "scope": "TEAM"}
    # Promoted packets now produce 4 calls (pre-#215-fix the memory-write
    # activity call was silently dropped because the test fake didn't
    # accept a `headers=` kwarg, which changed as the fakes were updated
    # to match the new auth-headers wiring):
    #   [0] POST /memories          — the memory write itself
    #   [1] POST /activity           — memory_write activity row (#125)
    #   [2] POST /activity           — skill_promotion activity row
    #   [3] POST /registry/heartbeat — heartbeat update with promotion task
    assert len(captured["calls"]) == 4
    memory_url, memory_payload = captured["calls"][0]
    memory_activity_url, memory_activity_payload = captured["calls"][1]
    skill_activity_url, skill_activity_payload = captured["calls"][2]
    heartbeat_url, heartbeat_payload = captured["calls"][3]
    assert memory_url == "http://platform.test/workspaces/ws-test/memories"
    assert memory_payload == {"content": json.dumps(packet), "scope": "TEAM"}
    assert memory_activity_url == "http://platform.test/workspaces/ws-test/activity"
    assert memory_activity_payload["activity_type"] == "memory_write"
    assert skill_activity_url == "http://platform.test/workspaces/ws-test/activity"
    assert skill_activity_payload["activity_type"] == "skill_promotion"
    assert skill_activity_payload["method"] == "memory/skill-promotion"
    assert skill_activity_payload["summary"] == "Repeated GitHub webhook handling is now a skill candidate"
    assert skill_activity_payload["metadata"]["promote_to_skill"] is True
    assert skill_activity_payload["metadata"]["memory_id"] == "mem-skill"
    assert skill_activity_payload["metadata"]["repetition_signal"] == packet["repetition_signal"]
    assert heartbeat_url == "http://platform.test/registry/heartbeat"
    assert heartbeat_payload["current_task"] == "Skill promotion: Repeated GitHub webhook handling is now a skill candidate"
    assert heartbeat_payload["active_tasks"] == 1

    assert not (tmp_path / "skills").exists()


def test_recall_memory_rejects_invalid_scope(memory_modules):
    memory, _awareness_client = memory_modules

    result = asyncio.run(memory.recall_memory("status", "bad"))

    assert result == {"error": "scope must be LOCAL, TEAM, GLOBAL, or empty"}


# ---------------------------------------------------------------------------
# Additional coverage tests
# ---------------------------------------------------------------------------

@pytest.fixture
def memory_modules_with_mocks(monkeypatch):
    """Load real memory module with full control over audit / telemetry / awareness."""
    import sys
    from types import ModuleType
    from unittest.mock import MagicMock, AsyncMock

    monkeypatch.setenv("PLATFORM_URL", "http://platform.test")
    monkeypatch.setenv("WORKSPACE_ID", "ws-test")
    monkeypatch.delenv("AWARENESS_URL", raising=False)
    monkeypatch.delenv("AWARENESS_NAMESPACE", raising=False)

    # --- audit mock -----------------------------------------------------------
    mock_audit = ModuleType("builtin_tools.audit")
    mock_audit.check_permission = MagicMock(return_value=True)
    mock_audit.get_workspace_roles = MagicMock(return_value=(["operator"], {}))
    mock_audit.log_event = MagicMock(return_value="trace-id")
    monkeypatch.setitem(sys.modules, "builtin_tools.audit", mock_audit)

    # --- telemetry mock -------------------------------------------------------
    mock_telemetry = ModuleType("builtin_tools.telemetry")
    mock_span = MagicMock()
    mock_span.__enter__ = MagicMock(return_value=mock_span)
    mock_span.__exit__ = MagicMock(return_value=False)
    mock_tracer = MagicMock()
    mock_tracer.start_as_current_span = MagicMock(return_value=mock_span)
    mock_telemetry.get_tracer = MagicMock(return_value=mock_tracer)
    mock_telemetry.MEMORY_QUERY = "memory.query"
    mock_telemetry.MEMORY_SCOPE = "memory.scope"
    mock_telemetry.WORKSPACE_ID_ATTR = "workspace.id"
    monkeypatch.setitem(sys.modules, "builtin_tools.telemetry", mock_telemetry)

    # --- awareness_client mock (no client by default) -------------------------
    mock_awareness_mod = ModuleType("builtin_tools.awareness_client")
    mock_awareness_mod.build_awareness_client = MagicMock(return_value=None)
    monkeypatch.setitem(sys.modules, "builtin_tools.awareness_client", mock_awareness_mod)

    # Remove any cached memory module so it re-imports with our mocks
    sys.modules.pop("builtin_tools.memory", None)

    tools_pkg = sys.modules.get("builtin_tools")
    if tools_pkg is not None:
        monkeypatch.setattr(tools_pkg, "__path__", [str(TOOLS_DIR)], raising=False)

    memory = _load_module("builtin_tools.memory_mocked", TOOLS_DIR / "memory.py")
    # Patch module-level constants
    memory.PLATFORM_URL = "http://platform.test"
    memory.WORKSPACE_ID = "ws-test"

    yield memory, mock_audit, mock_awareness_mod

    sys.modules.pop("builtin_tools.memory_mocked", None)


# ---------------------------------------------------------------------------
# commit_memory — RBAC deny
# ---------------------------------------------------------------------------

def test_commit_memory_rbac_deny(memory_modules_with_mocks):
    memory, mock_audit, _ = memory_modules_with_mocks
    mock_audit.check_permission.return_value = False
    mock_audit.get_workspace_roles.return_value = (["read-only"], {})

    result = asyncio.run(memory.commit_memory("secret", "local"))

    assert result["success"] is False
    assert "RBAC" in result["error"]
    assert "memory.write" in result["error"]
    # Denial event logged
    mock_audit.log_event.assert_called()


# ---------------------------------------------------------------------------
# commit_memory — invalid scope
# ---------------------------------------------------------------------------

def test_commit_memory_invalid_scope(memory_modules_with_mocks):
    memory, mock_audit, _ = memory_modules_with_mocks

    result = asyncio.run(memory.commit_memory("content", "INVALID"))

    assert result == {"error": "scope must be LOCAL, TEAM, or GLOBAL"}


# ---------------------------------------------------------------------------
# commit_memory — awareness_client raises
# ---------------------------------------------------------------------------

def test_commit_memory_awareness_client_exception(memory_modules_with_mocks):
    from unittest.mock import AsyncMock, MagicMock
    memory, mock_audit, mock_awareness_mod = memory_modules_with_mocks

    mock_ac = MagicMock()
    mock_ac.commit = AsyncMock(side_effect=RuntimeError("awareness down"))
    # Patch directly on the loaded module since it imported the name at load time
    memory.build_awareness_client = MagicMock(return_value=mock_ac)

    result = asyncio.run(memory.commit_memory("some content", "team"))

    assert result["success"] is False
    assert "awareness down" in result["error"]
    # Failure event must be logged
    log_calls = [str(c) for c in mock_audit.log_event.call_args_list]
    assert any("failure" in call for call in log_calls)


# ---------------------------------------------------------------------------
# commit_memory — httpx 201 success (no awareness_client)
# ---------------------------------------------------------------------------

def test_commit_memory_httpx_201_success(memory_modules_with_mocks):
    memory, mock_audit, _ = memory_modules_with_mocks
    captured = {}

    class FakeAsyncClient:
        def __init__(self, timeout):
            captured["timeout"] = timeout

        async def __aenter__(self):
            return self

        async def __aexit__(self, exc_type, exc, tb):
            return None

        async def post(self, url, json, headers=None):
            # Only capture the /memories call — _record_memory_activity
            # fires /activity after on success and would otherwise
            # overwrite captured["url"].
            if "/memories" in url:
                captured["url"] = url
            return _FakeResponse(201, {"id": "new-mem-1"})

    memory.httpx.AsyncClient = FakeAsyncClient

    result = asyncio.run(memory.commit_memory("hello", "local"))

    assert result == {"success": True, "id": "new-mem-1", "scope": "LOCAL"}
    assert "memories" in captured["url"]


# ---------------------------------------------------------------------------
# commit_memory — httpx non-201
# ---------------------------------------------------------------------------

def test_commit_memory_httpx_non_201(memory_modules_with_mocks):
    memory, mock_audit, _ = memory_modules_with_mocks

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json, headers=None):
            return _FakeResponse(400, {"error": "bad request"})

    memory.httpx.AsyncClient = FakeAsyncClient

    result = asyncio.run(memory.commit_memory("bad content", "local"))

    assert result["success"] is False
    assert "bad request" in result["error"]


# ---------------------------------------------------------------------------
# commit_memory — httpx raises
# ---------------------------------------------------------------------------

def test_commit_memory_httpx_exception(memory_modules_with_mocks):
    memory, mock_audit, _ = memory_modules_with_mocks

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json, headers=None):
            raise ConnectionError("network gone")

    memory.httpx.AsyncClient = FakeAsyncClient

    result = asyncio.run(memory.commit_memory("content", "global"))

    assert result["success"] is False
    assert "network gone" in result["error"]


# ---------------------------------------------------------------------------
# commit_memory — result.success=False (platform returned error payload)
# ---------------------------------------------------------------------------

def test_commit_memory_result_failure(memory_modules_with_mocks):
    memory, mock_audit, _ = memory_modules_with_mocks

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json, headers=None):
            return _FakeResponse(400, {"error": "storage full"})

    memory.httpx.AsyncClient = FakeAsyncClient

    result = asyncio.run(memory.commit_memory("data", "team"))

    assert result["success"] is False
    # failure event should be logged
    log_calls = [str(c) for c in mock_audit.log_event.call_args_list]
    assert any("failure" in call for call in log_calls)


# ---------------------------------------------------------------------------
# recall_memory — RBAC deny
# ---------------------------------------------------------------------------

def test_recall_memory_rbac_deny(memory_modules_with_mocks):
    memory, mock_audit, _ = memory_modules_with_mocks
    mock_audit.check_permission.return_value = False
    mock_audit.get_workspace_roles.return_value = (["read-only-special"], {})

    result = asyncio.run(memory.recall_memory("find something", "local"))

    assert result["success"] is False
    assert "RBAC" in result["error"]
    assert "memory.read" in result["error"]


# ---------------------------------------------------------------------------
# recall_memory — invalid scope
# ---------------------------------------------------------------------------

def test_recall_memory_invalid_scope(memory_modules_with_mocks):
    memory, _mock_audit, _ = memory_modules_with_mocks

    result = asyncio.run(memory.recall_memory("q", "BAD"))

    assert result == {"error": "scope must be LOCAL, TEAM, GLOBAL, or empty"}


# ---------------------------------------------------------------------------
# recall_memory — awareness_client success
# ---------------------------------------------------------------------------

def test_recall_memory_awareness_client_success(memory_modules_with_mocks):
    from unittest.mock import AsyncMock, MagicMock
    memory, mock_audit, mock_awareness_mod = memory_modules_with_mocks

    mock_ac = MagicMock()
    mock_ac.search = AsyncMock(return_value={
        "success": True,
        "count": 2,
        "memories": [{"content": "a"}, {"content": "b"}],
    })
    # Patch directly on the loaded module since it imported the name at load time
    memory.build_awareness_client = MagicMock(return_value=mock_ac)

    result = asyncio.run(memory.recall_memory("find", "team"))

    assert result["success"] is True
    assert result["count"] == 2
    assert len(result["memories"]) == 2


# ---------------------------------------------------------------------------
# recall_memory — awareness_client raises
# ---------------------------------------------------------------------------

def test_recall_memory_awareness_client_exception(memory_modules_with_mocks):
    from unittest.mock import AsyncMock, MagicMock
    memory, mock_audit, mock_awareness_mod = memory_modules_with_mocks

    mock_ac = MagicMock()
    mock_ac.search = AsyncMock(side_effect=RuntimeError("awareness search failed"))
    # Patch directly on the loaded module since it imported the name at load time
    memory.build_awareness_client = MagicMock(return_value=mock_ac)

    result = asyncio.run(memory.recall_memory("query", "local"))

    assert result["success"] is False
    assert "awareness search failed" in result["error"]
    log_calls = [str(c) for c in mock_audit.log_event.call_args_list]
    assert any("failure" in call for call in log_calls)


# ---------------------------------------------------------------------------
# recall_memory — httpx 200 success (no awareness_client)
# ---------------------------------------------------------------------------

def test_recall_memory_httpx_200_success(memory_modules_with_mocks):
    memory, _mock_audit, _ = memory_modules_with_mocks

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def get(self, url, params, headers=None):
            return _FakeResponse(200, [{"content": "result1"}, {"content": "result2"}])

    memory.httpx.AsyncClient = FakeAsyncClient

    result = asyncio.run(memory.recall_memory("find", "global"))

    assert result["success"] is True
    assert result["count"] == 2
    assert result["memories"] == [{"content": "result1"}, {"content": "result2"}]


# ---------------------------------------------------------------------------
# recall_memory — httpx non-200
# ---------------------------------------------------------------------------

def test_recall_memory_httpx_non_200(memory_modules_with_mocks):
    memory, mock_audit, _ = memory_modules_with_mocks

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def get(self, url, params, headers=None):
            return _FakeResponse(500, {"error": "server error"})

    memory.httpx.AsyncClient = FakeAsyncClient

    result = asyncio.run(memory.recall_memory("q", ""))

    assert result["success"] is False
    assert "server error" in result["error"]


# ---------------------------------------------------------------------------
# recall_memory — httpx raises
# ---------------------------------------------------------------------------

def test_recall_memory_httpx_exception(memory_modules_with_mocks):
    memory, mock_audit, _ = memory_modules_with_mocks

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def get(self, url, params, headers=None):
            raise TimeoutError("request timed out")

    memory.httpx.AsyncClient = FakeAsyncClient

    result = asyncio.run(memory.recall_memory("query", "local"))

    assert result["success"] is False
    assert "request timed out" in result["error"]


# ---------------------------------------------------------------------------
# _parse_promotion_packet
# ---------------------------------------------------------------------------

def test_parse_promotion_packet_not_json(memory_modules_with_mocks):
    memory, _, _ = memory_modules_with_mocks

    result = memory._parse_promotion_packet("this is not JSON at all")
    assert result is None


def test_parse_promotion_packet_no_promote_key(memory_modules_with_mocks):
    memory, _, _ = memory_modules_with_mocks

    result = memory._parse_promotion_packet('{"title": "something", "summary": "no promote key"}')
    assert result is None


def test_parse_promotion_packet_valid(memory_modules_with_mocks):
    memory, _, _ = memory_modules_with_mocks

    packet = {
        "title": "My skill",
        "summary": "Does something useful",
        "promote_to_skill": True,
    }
    result = memory._parse_promotion_packet(json.dumps(packet))
    assert result is not None
    assert result["promote_to_skill"] is True
    assert result["title"] == "My skill"


# ---------------------------------------------------------------------------
# _maybe_log_skill_promotion
# ---------------------------------------------------------------------------

def test_maybe_log_skill_promotion_no_packet(memory_modules_with_mocks):
    """Non-promotion content → _maybe_log_skill_promotion returns without HTTP calls."""
    memory, _, _ = memory_modules_with_mocks
    http_called = []

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json, headers=None):
            http_called.append(url)

    memory.httpx.AsyncClient = FakeAsyncClient

    asyncio.run(memory._maybe_log_skill_promotion(
        "plain text content", "LOCAL", {"success": True, "id": "m1"}
    ))

    assert http_called == []


def test_commit_memory_awareness_exception_span_record_fails(memory_modules_with_mocks):
    """awareness_client.commit raises + span.record_exception also raises: error still returned."""
    from unittest.mock import AsyncMock, MagicMock
    memory, mock_audit, mock_awareness_mod = memory_modules_with_mocks

    # Get the span mock from the telemetry module loaded in sys.modules
    mock_telemetry = sys.modules.get("builtin_tools.telemetry")
    mock_span = mock_telemetry.get_tracer.return_value.start_as_current_span.return_value.__enter__.return_value
    mock_span.record_exception = MagicMock(side_effect=RuntimeError("span broken"))

    # Make awareness_client raise
    mock_ac = MagicMock()
    mock_ac.commit = AsyncMock(side_effect=RuntimeError("awareness down"))
    memory.build_awareness_client = MagicMock(return_value=mock_ac)

    result = asyncio.run(memory.commit_memory("test content", "local"))
    assert result["success"] is False  # error propagated despite span failure


def test_recall_memory_awareness_exception_span_record_fails(memory_modules_with_mocks):
    """awareness_client.search raises + span.record_exception also raises: error still returned."""
    from unittest.mock import AsyncMock, MagicMock
    memory, mock_audit, mock_awareness_mod = memory_modules_with_mocks

    mock_telemetry = sys.modules.get("builtin_tools.telemetry")
    mock_span = mock_telemetry.get_tracer.return_value.start_as_current_span.return_value.__enter__.return_value
    mock_span.record_exception = MagicMock(side_effect=RuntimeError("span broken"))

    mock_ac = MagicMock()
    mock_ac.search = AsyncMock(side_effect=RuntimeError("awareness down"))
    memory.build_awareness_client = MagicMock(return_value=mock_ac)

    result = asyncio.run(memory.recall_memory("test", "local"))
    assert result["success"] is False


def test_commit_memory_httpx_exception_span_record_fails(memory_modules_with_mocks):
    """httpx raises in commit_memory + span.record_exception also raises: error still returned."""
    from unittest.mock import MagicMock
    memory, mock_audit, mock_awareness_mod = memory_modules_with_mocks

    mock_telemetry = sys.modules.get("builtin_tools.telemetry")
    mock_span = mock_telemetry.get_tracer.return_value.start_as_current_span.return_value.__enter__.return_value
    mock_span.record_exception = MagicMock(side_effect=RuntimeError("span broken"))

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json, headers=None):
            raise ConnectionError("network gone")

    memory.httpx.AsyncClient = FakeAsyncClient

    result = asyncio.run(memory.commit_memory("content", "global"))
    assert result["success"] is False


def test_recall_memory_httpx_exception_span_record_fails(memory_modules_with_mocks):
    """httpx raises in recall_memory + span.record_exception also raises: error still returned."""
    from unittest.mock import MagicMock
    memory, mock_audit, mock_awareness_mod = memory_modules_with_mocks

    mock_telemetry = sys.modules.get("builtin_tools.telemetry")
    mock_span = mock_telemetry.get_tracer.return_value.start_as_current_span.return_value.__enter__.return_value
    mock_span.record_exception = MagicMock(side_effect=RuntimeError("span broken"))

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def get(self, url, params, headers=None):
            raise TimeoutError("request timed out")

    memory.httpx.AsyncClient = FakeAsyncClient

    result = asyncio.run(memory.recall_memory("query", "local"))
    assert result["success"] is False


def test_parse_promotion_packet_invalid_json(memory_modules_with_mocks):
    """Lines 322-323: content starts with { but is invalid JSON → JSONDecodeError → None."""
    memory, _, _ = memory_modules_with_mocks
    result = memory._parse_promotion_packet("{bad: json}")
    assert result is None


def test_parse_promotion_packet_invalid_json_2(memory_modules_with_mocks):
    """Lines 322-323: another invalid JSON starting with { — missing closing brace."""
    memory, _, _ = memory_modules_with_mocks
    result = memory._parse_promotion_packet("{not valid json at all }")
    assert result is None


def test_maybe_log_skill_promotion_no_workspace_id(memory_modules_with_mocks):
    """Empty WORKSPACE_ID → returns early without HTTP calls."""
    memory, _, _ = memory_modules_with_mocks
    memory.WORKSPACE_ID = ""

    http_called = []

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json, headers=None):
            http_called.append(url)

    memory.httpx.AsyncClient = FakeAsyncClient

    packet = json.dumps({"promote_to_skill": True, "summary": "test"})
    asyncio.run(memory._maybe_log_skill_promotion(packet, "TEAM", {"success": True, "id": "m2"}))

    assert http_called == []


# ---------------------------------------------------------------------------
# _record_memory_activity (#125)
# ---------------------------------------------------------------------------

def test_record_memory_activity_posts_to_activity_endpoint(memory_modules_with_mocks):
    """Successful memory write surfaces as an activity row with scope tag."""
    memory, _, _ = memory_modules_with_mocks
    captured = []

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json=None, headers=None):
            captured.append({"url": url, "json": json, "headers": headers})

    memory.httpx.AsyncClient = FakeAsyncClient
    memory.WORKSPACE_ID = "ws-test"
    memory.PLATFORM_URL = "http://platform.test"

    asyncio.run(memory._record_memory_activity("LOCAL", "remember this fact", "mem-id-42"))

    assert len(captured) == 1
    call = captured[0]
    assert call["url"] == "http://platform.test/workspaces/ws-test/activity"
    assert call["json"]["activity_type"] == "memory_write"
    assert call["json"]["status"] == "ok"
    # target_id column is UUID-typed and reserved for workspace refs; the
    # memory id is encoded in the summary instead so it stays searchable.
    assert "target_id" not in call["json"]
    assert "mem-id-42" in call["json"]["summary"]
    assert call["json"]["summary"].startswith("[LOCAL]")
    assert "remember this fact" in call["json"]["summary"]


def test_record_memory_activity_truncates_long_content(memory_modules_with_mocks):
    """Content longer than 80 chars is truncated with ellipsis to keep
    activity_logs readable."""
    memory, _, _ = memory_modules_with_mocks
    captured = []

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json=None, headers=None):
            captured.append(json)

    memory.httpx.AsyncClient = FakeAsyncClient
    memory.WORKSPACE_ID = "ws-test"
    memory.PLATFORM_URL = "http://platform.test"

    long_content = "x" * 200
    asyncio.run(memory._record_memory_activity("TEAM", long_content, "mid"))

    summary = captured[0]["summary"]
    assert summary.startswith("[TEAM]")
    # Content is truncated with ellipsis; suffix has memory id appended.
    assert "…" in summary
    assert summary.endswith("(id=mid)")
    # 80 char body of x's between the scope tag and the ellipsis.
    body = summary[len("[TEAM] "):summary.index("…")]
    assert len(body) == 80
    assert body == "x" * 80


def test_record_memory_activity_strips_newlines_in_summary(memory_modules_with_mocks):
    """Multi-line content should appear single-line in activity summary."""
    memory, _, _ = memory_modules_with_mocks
    captured = []

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json=None, headers=None):
            captured.append(json)

    memory.httpx.AsyncClient = FakeAsyncClient
    memory.WORKSPACE_ID = "ws-test"
    memory.PLATFORM_URL = "http://platform.test"

    asyncio.run(memory._record_memory_activity("LOCAL", "line one\nline two", None))

    assert "\n" not in captured[0]["summary"]
    assert "line one line two" in captured[0]["summary"]


def test_record_memory_activity_skips_when_workspace_or_url_missing(memory_modules_with_mocks):
    """Defensive: empty WORKSPACE_ID or PLATFORM_URL → no HTTP call."""
    memory, _, _ = memory_modules_with_mocks
    captured = []

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json=None, headers=None):
            captured.append(url)

    memory.httpx.AsyncClient = FakeAsyncClient

    memory.WORKSPACE_ID = ""
    memory.PLATFORM_URL = "http://platform.test"
    asyncio.run(memory._record_memory_activity("LOCAL", "x", "id"))

    memory.WORKSPACE_ID = "ws-test"
    memory.PLATFORM_URL = ""
    asyncio.run(memory._record_memory_activity("LOCAL", "x", "id"))

    assert captured == []


def test_record_memory_activity_swallows_post_failure(memory_modules_with_mocks):
    """Activity log is observability — must never raise into the tool path."""
    memory, _, _ = memory_modules_with_mocks

    class ExplodingClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json=None, headers=None):
            raise ConnectionError("platform down")

    memory.httpx.AsyncClient = ExplodingClient
    memory.WORKSPACE_ID = "ws-test"
    memory.PLATFORM_URL = "http://platform.test"

    # Must not raise
    asyncio.run(memory._record_memory_activity("LOCAL", "x", "id"))


def test_record_memory_activity_omits_target_id_when_none(memory_modules_with_mocks):
    """Memory writes without an id (rare error paths) still log activity."""
    memory, _, _ = memory_modules_with_mocks
    captured = []

    class FakeAsyncClient:
        def __init__(self, timeout): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return None
        async def post(self, url, json=None, headers=None):
            captured.append(json)

    memory.httpx.AsyncClient = FakeAsyncClient
    memory.WORKSPACE_ID = "ws-test"
    memory.PLATFORM_URL = "http://platform.test"

    asyncio.run(memory._record_memory_activity("GLOBAL", "fact", None))

    assert "target_id" not in captured[0]
