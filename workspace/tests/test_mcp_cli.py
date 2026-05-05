"""Tests for workspace/mcp_cli.py — the molecule-mcp console-script
entry-point validator.

The wrapper exists to surface a friendly missing-env error before
a2a_client.py:22's module-level RuntimeError fires. Regressions here
ship a poor first-run UX to every external-runtime operator.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

import mcp_cli
import mcp_heartbeat


@pytest.fixture(autouse=True)
def _isolate(monkeypatch, tmp_path):
    """Each test starts with no Molecule env vars set + a fresh
    CONFIGS_DIR pointing at an empty tmpdir. The heartbeat thread is
    disabled by default so happy-path tests don't spawn a background
    POST loop against a fake URL — individual tests opt back in via
    monkeypatch.delenv when they want to assert heartbeat behavior."""
    for var in ("WORKSPACE_ID", "PLATFORM_URL", "MOLECULE_WORKSPACE_TOKEN"):
        monkeypatch.delenv(var, raising=False)
    monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
    monkeypatch.setenv("MOLECULE_MCP_DISABLE_HEARTBEAT", "1")
    yield


def _run_main_capturing_exit(capsys) -> tuple[int, str]:
    """Call mcp_cli.main and return (exit_code, stderr).

    main() is supposed to sys.exit on missing env. Any non-exit return
    means it tried to run the real MCP loop, which we don't want in a
    unit test (and which would also fail because we never set the
    mandatory env).
    """
    with pytest.raises(SystemExit) as exc_info:
        mcp_cli.main()
    captured = capsys.readouterr()
    code = exc_info.value.code if isinstance(exc_info.value.code, int) else 1
    return code, captured.err


def test_missing_workspace_id_exits_with_message(capsys):
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2, f"expected exit code 2, got {code}"
    assert "WORKSPACE_ID" in err
    assert "PLATFORM_URL" in err  # also missing
    assert "MOLECULE_WORKSPACE_TOKEN" in err  # also missing


def test_only_workspace_id_missing(capsys, monkeypatch):
    monkeypatch.setenv("PLATFORM_URL", "http://localhost:8080")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok")
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2
    # Only WORKSPACE_ID should appear in the "currently missing" list.
    assert "Currently missing: WORKSPACE_ID" in err


def test_only_platform_url_missing(capsys, monkeypatch):
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok")
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2
    assert "Currently missing: PLATFORM_URL" in err


def test_only_token_missing(capsys, monkeypatch):
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "http://localhost:8080")
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2
    assert "MOLECULE_WORKSPACE_TOKEN" in err


def test_token_file_satisfies_token_requirement(capsys, monkeypatch, tmp_path):
    """Token from CONFIGS_DIR/.auth_token must be accepted (in-container
    path)."""
    (tmp_path / ".auth_token").write_text("file-token")
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "http://localhost:8080")
    # No MOLECULE_WORKSPACE_TOKEN — but file exists. Validation should
    # pass; we then short-circuit before importing the heavy module by
    # patching the import to a no-op spy.

    spy_called: dict[str, bool] = {"called": False}

    def fake_cli_main():
        spy_called["called"] = True

    # Patch the heavy import to avoid actually running the MCP server.
    # mcp_cli does the import lazily inside main(), so we monkeypatch
    # sys.modules to inject a fake a2a_mcp_server.
    import types
    fake_module = types.ModuleType("a2a_mcp_server")
    fake_module.cli_main = fake_cli_main
    monkeypatch.setitem(sys.modules, "a2a_mcp_server", fake_module)

    mcp_cli.main()  # should NOT exit
    assert spy_called["called"], "expected cli_main to be invoked when env+file are valid"


def test_env_token_satisfies_token_requirement(capsys, monkeypatch):
    """Token from env must be accepted (external-runtime path)."""
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "http://localhost:8080")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "env-token")

    spy_called: dict[str, bool] = {"called": False}

    def fake_cli_main():
        spy_called["called"] = True

    import types
    fake_module = types.ModuleType("a2a_mcp_server")
    fake_module.cli_main = fake_cli_main
    monkeypatch.setitem(sys.modules, "a2a_mcp_server", fake_module)

    mcp_cli.main()
    assert spy_called["called"]


def test_whitespace_only_env_treated_as_missing(capsys, monkeypatch):
    """An accidentally-empty env var (WORKSPACE_ID="   ") must NOT be
    considered set — otherwise the error would surface deep inside an
    HTTP call instead of in this validator."""
    monkeypatch.setenv("WORKSPACE_ID", "   ")
    monkeypatch.setenv("PLATFORM_URL", "http://localhost:8080")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok")
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2
    assert "WORKSPACE_ID" in err


def test_help_lists_canvas_tokens_tab_pointer(capsys):
    """Operator must know WHERE to get a token. The help mentions the
    canvas Tokens tab so they can self-recover without asking on
    Slack."""
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2
    assert "Tokens tab" in err or "canvas" in err.lower()


# ==================== Standalone register + heartbeat ====================
# molecule-mcp must be a single-process standalone runtime: it registers
# the workspace at startup AND continuously heartbeats so the platform
# healthsweep doesn't flip status back to awaiting_agent. Without these,
# the operator sees "OFFLINE — Restart" in the canvas within ~60s of
# launching the agent, which was the bug that motivated this PR.


def test_register_called_at_startup(monkeypatch):
    """When env is valid and heartbeat enabled, register fires once
    before the MCP loop starts."""
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "https://test.moleculesai.app")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok")
    monkeypatch.delenv("MOLECULE_MCP_DISABLE_HEARTBEAT", raising=False)

    register_calls: list[tuple[str, str, str]] = []

    def fake_register(platform_url, workspace_id, token):
        register_calls.append((platform_url, workspace_id, token))

    def fake_start_thread(*_args, **_kwargs):
        # Return a dummy thread-shaped object so the caller's reference
        # is harmless. Real thread spawning is asserted separately.
        class _Stub:
            def join(self): pass
        return _Stub()

    monkeypatch.setattr(mcp_cli, "_platform_register", fake_register)
    monkeypatch.setattr(mcp_cli, "_start_heartbeat_thread", fake_start_thread)

    spy_called: dict[str, bool] = {"called": False}

    def fake_cli_main():
        spy_called["called"] = True

    import types
    fake_module = types.ModuleType("a2a_mcp_server")
    fake_module.cli_main = fake_cli_main
    monkeypatch.setitem(sys.modules, "a2a_mcp_server", fake_module)

    mcp_cli.main()

    assert register_calls == [
        ("https://test.moleculesai.app", "00000000-0000-0000-0000-000000000000", "tok"),
    ]
    assert spy_called["called"], "MCP loop must run AFTER register"


def test_heartbeat_thread_started(monkeypatch):
    """The heartbeat daemon thread must start before the MCP loop runs."""
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "https://test.moleculesai.app")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok")
    monkeypatch.delenv("MOLECULE_MCP_DISABLE_HEARTBEAT", raising=False)

    monkeypatch.setattr(mcp_cli, "_platform_register", lambda *a, **k: None)

    thread_started: dict[str, bool] = {"started": False}

    def fake_start_thread(platform_url, workspace_id, token):
        thread_started["started"] = True
        thread_started["args"] = (platform_url, workspace_id, token)
        class _Stub:
            def join(self): pass
        return _Stub()

    monkeypatch.setattr(mcp_cli, "_start_heartbeat_thread", fake_start_thread)

    import types
    fake_module = types.ModuleType("a2a_mcp_server")
    fake_module.cli_main = lambda: None
    monkeypatch.setitem(sys.modules, "a2a_mcp_server", fake_module)

    mcp_cli.main()

    assert thread_started["started"], "heartbeat thread must be spawned"
    assert thread_started["args"][1] == "00000000-0000-0000-0000-000000000000"
    assert thread_started["args"][2] == "tok"


def test_heartbeat_disable_env_skips_both(monkeypatch):
    """MOLECULE_MCP_DISABLE_HEARTBEAT=1 (the test fixture default + the
    in-container escape hatch) must skip BOTH register and heartbeat,
    so the in-container heartbeat loop in heartbeat.py doesn't compete
    with this thread."""
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "https://test.moleculesai.app")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok")
    # MOLECULE_MCP_DISABLE_HEARTBEAT=1 is set by the autouse fixture.

    register_called: dict[str, bool] = {"called": False}
    thread_started: dict[str, bool] = {"started": False}

    monkeypatch.setattr(
        mcp_cli, "_platform_register",
        lambda *a, **k: register_called.update(called=True),
    )
    monkeypatch.setattr(
        mcp_cli, "_start_heartbeat_thread",
        lambda *a, **k: thread_started.update(started=True),
    )

    import types
    fake_module = types.ModuleType("a2a_mcp_server")
    fake_module.cli_main = lambda: None
    monkeypatch.setitem(sys.modules, "a2a_mcp_server", fake_module)

    mcp_cli.main()

    assert register_called["called"] is False, "disable env must skip register"
    assert thread_started["started"] is False, "disable env must skip heartbeat thread"


def test_token_resolved_from_env_when_no_file(monkeypatch):
    """Operator without a /configs volume — token comes from env var."""
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "https://test.moleculesai.app")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "env-token")
    monkeypatch.delenv("MOLECULE_MCP_DISABLE_HEARTBEAT", raising=False)

    captured_token: dict[str, str] = {}

    def fake_register(platform_url, workspace_id, token):
        captured_token["t"] = token

    monkeypatch.setattr(mcp_cli, "_platform_register", fake_register)
    monkeypatch.setattr(mcp_cli, "_start_heartbeat_thread", lambda *a, **k: None)

    import types
    fake_module = types.ModuleType("a2a_mcp_server")
    fake_module.cli_main = lambda: None
    monkeypatch.setitem(sys.modules, "a2a_mcp_server", fake_module)

    mcp_cli.main()

    assert captured_token["t"] == "env-token"


def test_token_resolved_from_file_when_no_env(monkeypatch, tmp_path):
    """In-container parity: token comes from /configs/.auth_token when
    env is unset. Mirrors platform_auth.get_token resolution order."""
    (tmp_path / ".auth_token").write_text("file-token")
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "https://test.moleculesai.app")
    monkeypatch.delenv("MOLECULE_WORKSPACE_TOKEN", raising=False)
    monkeypatch.delenv("MOLECULE_MCP_DISABLE_HEARTBEAT", raising=False)

    captured_token: dict[str, str] = {}

    def fake_register(platform_url, workspace_id, token):
        captured_token["t"] = token

    monkeypatch.setattr(mcp_cli, "_platform_register", fake_register)
    monkeypatch.setattr(mcp_cli, "_start_heartbeat_thread", lambda *a, **k: None)

    import types
    fake_module = types.ModuleType("a2a_mcp_server")
    fake_module.cli_main = lambda: None
    monkeypatch.setitem(sys.modules, "a2a_mcp_server", fake_module)

    mcp_cli.main()

    assert captured_token["t"] == "file-token"


def test_register_401_exits_with_actionable_error(monkeypatch, capsys):
    """Bad token at startup must hard-fail. Otherwise the operator
    sees no error in their MCP client (which spawns the binary in a
    subprocess), the heartbeat thread silently 401's forever, and
    every tool call also 401's — needle-in-haystack debugging.
    Hard-exiting prints a clear pointer to the canvas Tokens tab."""

    class FakeResp:
        status_code = 401
        text = "invalid workspace auth token"

    class FakeClient:
        def __init__(self, **_kwargs): pass
        def __enter__(self): return self
        def __exit__(self, *_a): return False
        def post(self, *_a, **_kw): return FakeResp()

    import types
    fake_httpx = types.ModuleType("httpx")
    fake_httpx.Client = FakeClient
    monkeypatch.setitem(sys.modules, "httpx", fake_httpx)

    with pytest.raises(SystemExit) as exc_info:
        mcp_cli._platform_register(
            "https://test.moleculesai.app",
            "ws-bad-token",
            "wrong-token",
        )
    assert exc_info.value.code == 3
    err = capsys.readouterr().err
    assert "401" in err
    assert "ws-bad-token" in err
    assert "Tokens tab" in err or "canvas" in err.lower()


def test_register_403_also_exits(monkeypatch, capsys):
    """403 is the C18 hijack-prevention rejection — same operator
    action (regenerate token) as 401."""

    class FakeResp:
        status_code = 403
        text = "C18: live tokens exist; bearer didn't match"

    class FakeClient:
        def __init__(self, **_kwargs): pass
        def __enter__(self): return self
        def __exit__(self, *_a): return False
        def post(self, *_a, **_kw): return FakeResp()

    import types
    fake_httpx = types.ModuleType("httpx")
    fake_httpx.Client = FakeClient
    monkeypatch.setitem(sys.modules, "httpx", fake_httpx)

    with pytest.raises(SystemExit) as exc_info:
        mcp_cli._platform_register(
            "https://test.moleculesai.app",
            "ws-hijack",
            "stolen-token",
        )
    assert exc_info.value.code == 3


def test_register_500_does_not_exit(monkeypatch):
    """Transient platform errors (500, 503) must NOT hard-fail —
    those clear on retry and the heartbeat thread will surface
    persistent failures via warning logs."""

    class FakeResp:
        status_code = 503
        text = "service unavailable"

    class FakeClient:
        def __init__(self, **_kwargs): pass
        def __enter__(self): return self
        def __exit__(self, *_a): return False
        def post(self, *_a, **_kw): return FakeResp()

    import types
    fake_httpx = types.ModuleType("httpx")
    fake_httpx.Client = FakeClient
    monkeypatch.setitem(sys.modules, "httpx", fake_httpx)

    # Should return cleanly, no SystemExit raised
    mcp_cli._platform_register(
        "https://test.moleculesai.app",
        "ws-ok",
        "tok",
    )


def test_register_payload_shape(monkeypatch):
    """The register POST body must use the field names the workspace-
    server expects (id/url/agent_card/delivery_mode), and must include
    the Origin header for the SaaS edge WAF."""
    captured: dict[str, object] = {}

    class FakeResp:
        status_code = 200
        text = ""

    class FakeClient:
        def __init__(self, **_kwargs): pass
        def __enter__(self): return self
        def __exit__(self, *_a): return False
        def post(self, url, json=None, headers=None):
            captured["url"] = url
            captured["json"] = json
            captured["headers"] = headers
            return FakeResp()

    import types
    fake_httpx = types.ModuleType("httpx")
    fake_httpx.Client = FakeClient
    monkeypatch.setitem(sys.modules, "httpx", fake_httpx)

    mcp_cli._platform_register(
        "https://test.moleculesai.app",
        "ws-abc",
        "tok",
    )

    assert captured["url"] == "https://test.moleculesai.app/registry/register"
    body = captured["json"]
    assert body["id"] == "ws-abc"
    assert body["delivery_mode"] == "poll"
    assert body["url"] == ""
    assert "agent_card" in body
    headers = captured["headers"]
    assert headers["Authorization"] == "Bearer tok"
    assert headers["Origin"] == "https://test.moleculesai.app"


# ============== Agent card env vars (capability discovery) ==============
# External runtimes register with hardcoded agent_card.name and skills=[].
# Both the canvas SkillsTab and the list_peers tool surface skills to
# users + peer agents for routing — empty skills means peers route blind.
# MOLECULE_AGENT_NAME / DESCRIPTION / SKILLS env vars let the operator
# declare identity + capabilities without code changes. Defaults are
# strict-superset: unset env vars = previous hardcoded behaviour.


def test_build_agent_card_defaults_match_previous_behavior(monkeypatch):
    """Strict-superset: when no env vars are set, the agent_card shape
    matches the previous hardcoded value exactly. No silent regression
    for operators who haven't set the new vars."""
    for var in ("MOLECULE_AGENT_NAME", "MOLECULE_AGENT_DESCRIPTION", "MOLECULE_AGENT_SKILLS"):
        monkeypatch.delenv(var, raising=False)

    card = mcp_cli._build_agent_card("8dad3e29-c32a-4ec7-9ea7-94fe2d2d98ec")

    assert card == {"name": "molecule-mcp-8dad3e29", "skills": []}


def test_build_agent_card_name_from_env(monkeypatch):
    """MOLECULE_AGENT_NAME overrides the auto-generated default so
    operators can give the canvas card a human-readable label."""
    monkeypatch.setenv("MOLECULE_AGENT_NAME", "Research Assistant")
    monkeypatch.delenv("MOLECULE_AGENT_DESCRIPTION", raising=False)
    monkeypatch.delenv("MOLECULE_AGENT_SKILLS", raising=False)

    card = mcp_cli._build_agent_card("8dad3e29-c32a-4ec7-9ea7-94fe2d2d98ec")

    assert card["name"] == "Research Assistant"


def test_build_agent_card_skills_csv_to_objects(monkeypatch):
    """MOLECULE_AGENT_SKILLS is comma-separated names; each gets
    expanded to {'name': ...} — the minimum shape that satisfies both
    shared_runtime.summarize_peers (s['name']) AND canvas SkillsTab
    (id falls back to name)."""
    monkeypatch.delenv("MOLECULE_AGENT_NAME", raising=False)
    monkeypatch.setenv("MOLECULE_AGENT_SKILLS", "research,code-review,memory-curation")

    card = mcp_cli._build_agent_card("ws-1")

    assert card["skills"] == [
        {"name": "research"},
        {"name": "code-review"},
        {"name": "memory-curation"},
    ]


def test_build_agent_card_skills_strips_whitespace_and_empty(monkeypatch):
    """Real-world env vars often have stray whitespace from copy-paste
    or shell quoting. Strip each entry; drop empty ones."""
    monkeypatch.setenv(
        "MOLECULE_AGENT_SKILLS", " research , , code-review ,, "
    )

    card = mcp_cli._build_agent_card("ws-1")

    assert card["skills"] == [{"name": "research"}, {"name": "code-review"}]


def test_build_agent_card_description_only_set_when_present(monkeypatch):
    """description is omitted from the card when env var is unset —
    keeps the wire payload minimal and matches the platform's
    'absent field = use default' contract."""
    monkeypatch.delenv("MOLECULE_AGENT_DESCRIPTION", raising=False)

    card = mcp_cli._build_agent_card("ws-1")

    assert "description" not in card

    monkeypatch.setenv("MOLECULE_AGENT_DESCRIPTION", "Researches things")
    card2 = mcp_cli._build_agent_card("ws-1")
    assert card2["description"] == "Researches things"


def test_build_agent_card_whitespace_only_name_falls_back_to_default(monkeypatch):
    """An accidentally-empty MOLECULE_AGENT_NAME (e.g. operator set
    the var but forgot to fill the value) falls back to the auto-
    generated default, matching the WORKSPACE_ID whitespace handling
    in main()."""
    monkeypatch.setenv("MOLECULE_AGENT_NAME", "   ")

    card = mcp_cli._build_agent_card("8dad3e29-c32a-4ec7-9ea7-94fe2d2d98ec")

    assert card["name"] == "molecule-mcp-8dad3e29"


def test_register_payload_uses_built_agent_card(monkeypatch):
    """End-to-end: env vars flow through _platform_register's payload
    so the platform sees the operator's declared identity, not the
    hardcoded default."""
    monkeypatch.setenv("MOLECULE_AGENT_NAME", "Research Bot")
    monkeypatch.setenv("MOLECULE_AGENT_SKILLS", "research,analysis")

    captured: dict[str, object] = {}

    class FakeResp:
        status_code = 200
        text = ""

    class FakeClient:
        def __init__(self, **_kwargs): pass
        def __enter__(self): return self
        def __exit__(self, *_a): return False
        def post(self, url, json=None, headers=None):
            captured["json"] = json
            return FakeResp()

    import types
    fake_httpx = types.ModuleType("httpx")
    fake_httpx.Client = FakeClient
    monkeypatch.setitem(sys.modules, "httpx", fake_httpx)

    mcp_cli._platform_register("https://test.moleculesai.app", "ws-1", "tok")

    body = captured["json"]
    assert body["agent_card"]["name"] == "Research Bot"
    assert body["agent_card"]["skills"] == [
        {"name": "research"},
        {"name": "analysis"},
    ]


def test_heartbeat_loop_posts_to_correct_endpoint(monkeypatch):
    """Heartbeat thread must POST to /registry/heartbeat with the
    workspace_id + Origin/Authorization headers."""
    captured: dict[str, object] = {}

    class FakeResp:
        status_code = 200
        text = ""

    class FakeClient:
        def __init__(self, **_kwargs): pass
        def __enter__(self): return self
        def __exit__(self, *_a): return False
        def post(self, url, json=None, headers=None):
            captured["url"] = url
            captured["json"] = json
            captured["headers"] = headers
            return FakeResp()

    import types
    fake_httpx = types.ModuleType("httpx")
    fake_httpx.Client = FakeClient
    monkeypatch.setitem(sys.modules, "httpx", fake_httpx)

    # Patch sleep so the loop exits after one tick (raise to break out).
    sleep_calls: list[float] = []

    def fake_sleep(seconds):
        sleep_calls.append(seconds)
        raise SystemExit  # break out of the infinite loop

    monkeypatch.setattr("time.sleep", fake_sleep)

    with pytest.raises(SystemExit):
        mcp_cli._heartbeat_loop(
            "https://test.moleculesai.app",
            "ws-abc",
            "tok",
            interval=20.0,
        )

    assert captured["url"] == "https://test.moleculesai.app/registry/heartbeat"
    assert captured["json"]["workspace_id"] == "ws-abc"
    assert captured["headers"]["Authorization"] == "Bearer tok"
    assert captured["headers"]["Origin"] == "https://test.moleculesai.app"
    assert sleep_calls == [20.0], "heartbeat must sleep the configured interval"


# ============== Heartbeat persists platform_inbound_secret (2026-04-30) ==============
# Heartbeat loop must persist the platform_inbound_secret returned by
# the platform. Without this, a workspace that lazy-healed the secret
# on the platform side recovers only on a runtime restart — chat upload
# 401-forever. Pairs with the server-side
# TestHeartbeatHandler_DeliversPlatformInboundSecret pin.


def test_heartbeat_persists_inbound_secret_from_response(monkeypatch, tmp_path):
    """Heartbeat 200 with platform_inbound_secret in body → save_inbound_secret called."""

    class FakeResp:
        status_code = 200
        text = ""

        def json(self):
            return {"status": "ok", "platform_inbound_secret": "fresh-secret"}

    saved: list[str] = []
    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", saved.append)

    mcp_cli._persist_inbound_secret_from_heartbeat(FakeResp())

    assert saved == ["fresh-secret"], (
        "expected save_inbound_secret called once with the platform's secret"
    )


def test_heartbeat_persist_skips_when_secret_absent(monkeypatch):
    """Heartbeat 200 without platform_inbound_secret → no persist call."""

    class FakeResp:
        def json(self):
            return {"status": "ok"}

    saved: list[str] = []
    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", saved.append)

    mcp_cli._persist_inbound_secret_from_heartbeat(FakeResp())

    assert saved == [], "no secret in body → must NOT call save_inbound_secret"


def test_heartbeat_persist_skips_on_empty_secret(monkeypatch):
    """Heartbeat 200 with empty-string platform_inbound_secret → no persist."""

    class FakeResp:
        def json(self):
            return {"status": "ok", "platform_inbound_secret": ""}

    saved: list[str] = []
    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", saved.append)

    mcp_cli._persist_inbound_secret_from_heartbeat(FakeResp())

    assert saved == [], "empty secret string → must NOT call save_inbound_secret"


def test_heartbeat_persist_swallows_non_json_body(monkeypatch):
    """Heartbeat with unparseable body must not raise — logs + returns."""

    class FakeResp:
        def json(self):
            raise ValueError("not json")

    saved: list[str] = []
    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", saved.append)

    # Must not raise; non-JSON body is treated as "no secret to deliver".
    mcp_cli._persist_inbound_secret_from_heartbeat(FakeResp())
    assert saved == []


def test_heartbeat_persist_handles_non_dict_body(monkeypatch):
    """Heartbeat returning a list (not a dict) is silently ignored."""

    class FakeResp:
        def json(self):
            return ["unexpected", "list"]

    saved: list[str] = []
    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", saved.append)

    mcp_cli._persist_inbound_secret_from_heartbeat(FakeResp())
    assert saved == []


def test_heartbeat_persist_swallows_save_exceptions(monkeypatch, caplog):
    """save_inbound_secret raising must not crash the heartbeat loop."""

    class FakeResp:
        def json(self):
            return {"platform_inbound_secret": "x"}

    def boom(_secret):
        raise OSError("disk full")

    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", boom)

    # Must not raise — heartbeat liveness > secret persistence.
    mcp_cli._persist_inbound_secret_from_heartbeat(FakeResp())


def test_heartbeat_loop_calls_persist_on_success(monkeypatch):
    """End-to-end: heartbeat loop on 200 invokes the persist helper."""
    saw: list[object] = []

    def fake_persist(resp):
        saw.append(resp)

    # Patch on mcp_heartbeat — that's where heartbeat_loop's internal
    # name resolution looks up persist_inbound_secret_from_heartbeat
    # after the RFC #2873 iter 3 split. The mcp_cli._persist_…_from_heartbeat
    # back-compat re-export still exists, but patching it here would not
    # affect the loop body.
    monkeypatch.setattr(
        mcp_heartbeat, "persist_inbound_secret_from_heartbeat", fake_persist
    )

    class FakeResp:
        status_code = 200
        text = ""

    class FakeClient:
        def __init__(self, **_kwargs):
            pass

        def __enter__(self):
            return self

        def __exit__(self, *_a):
            return False

        def post(self, *_a, **_k):
            return FakeResp()

    import types

    fake_httpx = types.ModuleType("httpx")
    fake_httpx.Client = FakeClient
    monkeypatch.setitem(sys.modules, "httpx", fake_httpx)

    def fake_sleep(_):
        raise SystemExit

    monkeypatch.setattr("time.sleep", fake_sleep)

    with pytest.raises(SystemExit):
        mcp_cli._heartbeat_loop(
            "https://test.moleculesai.app",
            "ws-abc",
            "tok",
            interval=20.0,
        )

    assert len(saw) == 1, "persist helper must be called once per successful heartbeat"


def test_heartbeat_loop_skips_persist_on_4xx(monkeypatch):
    """Heartbeat 4xx error path must NOT invoke persist (no body to trust)."""
    saw: list[object] = []
    monkeypatch.setattr(
        mcp_heartbeat,
        "persist_inbound_secret_from_heartbeat",
        lambda r: saw.append(r),
    )

    class FakeResp:
        status_code = 401
        text = "unauthorized"

    class FakeClient:
        def __init__(self, **_kwargs):
            pass

        def __enter__(self):
            return self

        def __exit__(self, *_a):
            return False

        def post(self, *_a, **_k):
            return FakeResp()

    import types

    fake_httpx = types.ModuleType("httpx")
    fake_httpx.Client = FakeClient
    monkeypatch.setitem(sys.modules, "httpx", fake_httpx)

    def fake_sleep(_):
        raise SystemExit

    monkeypatch.setattr("time.sleep", fake_sleep)

    with pytest.raises(SystemExit):
        mcp_cli._heartbeat_loop(
            "https://test.moleculesai.app",
            "ws-abc",
            "tok",
            interval=20.0,
        )

    assert saw == [], "4xx response must NOT trigger persist call"


# ============== Heartbeat auth-failure escalation (2026-05-01) ==============
# When a workspace is deleted server-side (DELETE /workspaces/:id), the
# platform revokes the workspace's auth token. The heartbeat starts
# 401-ing. The previous behavior just logged WARNING on every tick — a
# user tailing logs might miss it, and there was no actionable signal
# anywhere. Escalate after a small number of consecutive auth failures
# so the operator gets a clear "token revoked, re-onboard" message and
# isn't left to puzzle out why their MCP tools 401.
#
# Pairs with the register-time 401 hard-fail path that already exists
# at mcp_cli.py:104-111.


def _multi_iter_runner(monkeypatch, response_status_codes):
    """Run _heartbeat_loop for ``len(response_status_codes)`` iterations.

    Each call to FakeClient.post returns a response with the next status
    code from ``response_status_codes``. After all responses are consumed,
    the next sleep raises SystemExit to break the loop.
    """
    import types

    iterations = {"count": 0}
    target = len(response_status_codes)

    class FakeResp:
        def __init__(self, status_code):
            self.status_code = status_code
            self.text = "" if status_code < 400 else '{"error":"invalid workspace auth token"}'

        def json(self):
            if self.status_code >= 400:
                return {"error": "invalid workspace auth token"}
            return {"status": "ok"}

    class FakeClient:
        def __init__(self, **_kw): pass
        def __enter__(self): return self
        def __exit__(self, *_a): return False
        def post(self, *_a, **_kw):
            i = iterations["count"]
            sc = response_status_codes[i] if i < len(response_status_codes) else 200
            return FakeResp(sc)

    fake_httpx = types.ModuleType("httpx")
    fake_httpx.Client = FakeClient
    monkeypatch.setitem(sys.modules, "httpx", fake_httpx)

    def fake_sleep(_):
        iterations["count"] += 1
        if iterations["count"] >= target:
            raise SystemExit

    monkeypatch.setattr("time.sleep", fake_sleep)

    with pytest.raises(SystemExit):
        mcp_cli._heartbeat_loop(
            "https://test.moleculesai.app",
            "ws-deleted-12345678",
            "stale-token",
            interval=20.0,
        )


def test_heartbeat_single_401_logs_warning_not_error(monkeypatch, caplog):
    """One 401 alone is not enough to declare the token dead — could be a
    transient platform blip. Log at WARNING; don't shout."""
    import logging

    caplog.set_level(logging.WARNING, logger="mcp_heartbeat")

    _multi_iter_runner(monkeypatch, [401])

    auth_records = [r for r in caplog.records if "401" in r.message
                    or "auth" in r.message.lower()
                    or "revoked" in r.message.lower()]
    # At least the WARNING-level mention of HTTP 401 must appear.
    assert any(r.levelno == logging.WARNING for r in auth_records), (
        f"expected at least one WARNING about 401, got: "
        f"{[(r.levelname, r.message) for r in auth_records]}"
    )
    # Crucially, NOT escalated to ERROR yet — only one failure.
    assert not any(r.levelno >= logging.ERROR for r in auth_records), (
        "single 401 must not escalate to ERROR — premature alarm"
    )


def test_heartbeat_three_consecutive_401s_escalates_to_error(monkeypatch, caplog):
    """Token-revoked is the canonical failure mode after a workspace is
    deleted server-side. After 3 consecutive 401s the operator gets a
    LOUD ERROR with re-onboard guidance — not buried at WARNING."""
    import logging

    caplog.set_level(logging.WARNING, logger="mcp_heartbeat")

    _multi_iter_runner(monkeypatch, [401, 401, 401])

    error_records = [r for r in caplog.records if r.levelno >= logging.ERROR]
    assert error_records, (
        f"expected ERROR after 3 consecutive 401s, got only: "
        f"{[(r.levelname, r.message[:80]) for r in caplog.records]}"
    )
    # The message must be actionable — operator needs to know what to do.
    msg = " ".join(r.message for r in error_records).lower()
    assert "revoked" in msg or "deleted" in msg, (
        f"ERROR must explain WHY (token revoked / workspace deleted), got: {msg}"
    )
    assert "regenerate" in msg or "re-onboard" in msg or "tokens" in msg, (
        f"ERROR must point at the canvas Tokens tab so operator knows how to recover, got: {msg}"
    )
    # The workspace_id should appear so the operator knows which one is dead.
    assert "ws-deleted" in msg, f"ERROR must name the dead workspace_id, got: {msg}"


def test_heartbeat_403_treated_same_as_401(monkeypatch, caplog):
    """403 Forbidden is the other auth-failure shape (token valid but
    not authorized for this workspace). Same escalation path."""
    import logging

    caplog.set_level(logging.WARNING, logger="mcp_heartbeat")

    _multi_iter_runner(monkeypatch, [403, 403, 403])

    error_records = [r for r in caplog.records if r.levelno >= logging.ERROR]
    assert error_records, "expected ERROR after 3 consecutive 403s"


def test_heartbeat_recovery_resets_consecutive_counter(monkeypatch, caplog):
    """If the platform comes back to 200 in the middle of an outage,
    the auth-failure counter must reset. A subsequent isolated 401
    later should NOT immediately escalate."""
    import logging

    caplog.set_level(logging.WARNING, logger="mcp_heartbeat")

    # Two 401s, then 200, then one 401. If counter resets correctly,
    # the final 401 is "1 consecutive" and should NOT escalate.
    _multi_iter_runner(monkeypatch, [401, 401, 200, 401])

    error_records = [r for r in caplog.records if r.levelno >= logging.ERROR]
    assert not error_records, (
        f"recovered (200) → reset counter → final isolated 401 must NOT "
        f"escalate. Got ERRORs: {[r.message[:80] for r in error_records]}"
    )


def test_heartbeat_500_does_not_increment_auth_counter(monkeypatch, caplog):
    """5xx is a server-side blip, not auth. Three consecutive 500s
    must NOT trigger the 'token revoked' escalation — that would be
    misleading the operator."""
    import logging

    caplog.set_level(logging.WARNING, logger="mcp_heartbeat")

    _multi_iter_runner(monkeypatch, [500, 500, 500])

    error_records = [r for r in caplog.records if r.levelno >= logging.ERROR]
    revoked_errors = [r for r in error_records if "revoked" in r.message.lower()]
    assert not revoked_errors, (
        f"5xx must NOT be classified as auth failure — would mislead operator. "
        f"Got 'revoked' ERRORs: {[r.message[:80] for r in revoked_errors]}"
    )
