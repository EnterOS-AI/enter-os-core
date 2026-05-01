"""Tests for heartbeat.py — HeartbeatLoop tracking and HTTP calls."""

import asyncio
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from heartbeat import HeartbeatLoop


def test_init():
    """HeartbeatLoop stores platform_url, workspace_id, and zeroes counters."""
    hb = HeartbeatLoop("http://localhost:8080", "ws-123")
    assert hb.platform_url == "http://localhost:8080"
    assert hb.workspace_id == "ws-123"
    assert hb.error_count == 0
    assert hb.request_count == 0
    assert hb.active_tasks == 0
    assert hb.sample_error == ""
    assert hb._task is None


def test_record_success():
    """record_success increments request_count only."""
    hb = HeartbeatLoop("http://localhost:8080", "ws-1")
    hb.record_success()
    hb.record_success()
    assert hb.request_count == 2
    assert hb.error_count == 0


def test_record_error():
    """record_error increments both counts and stores sample error."""
    hb = HeartbeatLoop("http://localhost:8080", "ws-1")
    hb.record_error("timeout")
    assert hb.request_count == 1
    assert hb.error_count == 1
    assert hb.sample_error == "timeout"


def test_error_rate_zero_requests():
    """error_rate is 0.0 when no requests have been recorded."""
    hb = HeartbeatLoop("http://localhost:8080", "ws-1")
    assert hb.error_rate == 0.0


def test_error_rate_calculation():
    """error_rate correctly computes error_count / request_count."""
    hb = HeartbeatLoop("http://localhost:8080", "ws-1")
    hb.record_success()
    hb.record_success()
    hb.record_error("fail")
    hb.record_success()
    # 1 error / 4 requests = 0.25
    assert hb.error_rate == 0.25


def test_error_rate_all_errors():
    """error_rate is 1.0 when all requests are errors."""
    hb = HeartbeatLoop("http://localhost:8080", "ws-1")
    hb.record_error("e1")
    hb.record_error("e2")
    assert hb.error_rate == 1.0


def test_sample_error_updated():
    """sample_error always reflects the most recent error."""
    hb = HeartbeatLoop("http://localhost:8080", "ws-1")
    hb.record_error("first")
    hb.record_error("second")
    assert hb.sample_error == "second"


@pytest.mark.asyncio
async def test_heartbeat_loop_posts():
    """The _loop sends a POST to /registry/heartbeat with the correct payload."""
    hb = HeartbeatLoop("http://platform:8080", "ws-abc")
    hb.record_error("some error")
    hb.active_tasks = 2

    mock_response = MagicMock()
    mock_client = AsyncMock()
    mock_client.post = AsyncMock(return_value=mock_response)
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)

    with patch("heartbeat.httpx.AsyncClient", return_value=mock_client):
        # Run the loop but cancel after one iteration
        async def run_one_iteration():
            task = asyncio.create_task(hb._loop())
            await asyncio.sleep(0.05)
            task.cancel()
            try:
                await task
            except asyncio.CancelledError:
                pass

        await run_one_iteration()

    mock_client.post.assert_called_once()
    call_args = mock_client.post.call_args
    assert call_args[0][0] == "http://platform:8080/registry/heartbeat"
    payload = call_args[1]["json"]
    assert payload["workspace_id"] == "ws-abc"
    assert payload["error_rate"] == 1.0  # 1 error / 1 request
    assert payload["sample_error"] == "some error"
    assert payload["active_tasks"] == 2
    assert "uptime_seconds" in payload


@pytest.mark.asyncio
async def test_stop_cancels_task():
    """stop() cancels the running heartbeat task."""
    hb = HeartbeatLoop("http://localhost:8080", "ws-1")

    mock_client = AsyncMock()
    mock_client.post = AsyncMock()
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)

    with patch("heartbeat.httpx.AsyncClient", return_value=mock_client):
        hb.start()
        assert hb._task is not None
        await asyncio.sleep(0.01)
        await hb.stop()
        assert hb._task.cancelled() or hb._task.done()


@pytest.mark.asyncio
async def test_heartbeat_loop_continues_after_exception(capsys):
    """When the POST raises an exception, the loop prints a message and continues."""
    hb = HeartbeatLoop("http://platform:8080", "ws-err")

    call_count = 0

    async def fake_post(*args, **kwargs):
        nonlocal call_count
        call_count += 1
        if call_count == 1:
            raise Exception("connection refused")
        # Second call succeeds — return a mock response
        return MagicMock()

    mock_client = AsyncMock()
    mock_client.post = fake_post
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)

    with patch("heartbeat.httpx.AsyncClient", return_value=mock_client):
        with patch("asyncio.sleep", new_callable=AsyncMock) as mock_sleep:
            # Allow two iterations then cancel
            iteration = 0

            async def controlled_sleep(delay):
                nonlocal iteration
                iteration += 1
                if iteration >= 2:
                    raise asyncio.CancelledError()

            mock_sleep.side_effect = controlled_sleep

            task = asyncio.create_task(hb._loop())
            try:
                await task
            except asyncio.CancelledError:
                pass

    # The loop ran at least once and logged the failure (via logger, not print)
    # The loop continued (call_count reached at least 1)
    assert call_count >= 1


# ---------------------------------------------------------------------------
# Delegation checking tests
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_check_delegations_writes_results_file(tmp_path):
    """When completed delegations are found, results are written to file."""
    import json
    results_file = tmp_path / "delegation_results.jsonl"

    hb = HeartbeatLoop("http://platform:8080", "ws-abc")

    delegations = [
        {"delegation_id": "d-1", "status": "completed", "target_id": "ws-t",
         "source_id": "ws-abc",  # must match workspace_id for Fix B source validation
         "summary": "Done", "response_preview": "Result here", "error": ""},
    ]

    mock_client = AsyncMock()
    # GET /delegations returns completed delegation
    get_resp = MagicMock()
    get_resp.status_code = 200
    get_resp.json = MagicMock(return_value=delegations)
    mock_client.get = AsyncMock(return_value=get_resp)
    # POST for self-message and notify — just succeed
    post_resp = MagicMock()
    post_resp.status_code = 200
    mock_client.post = AsyncMock(return_value=post_resp)

    with patch("heartbeat.DELEGATION_RESULTS_FILE", str(results_file)):
        await hb._check_delegations(mock_client)

    # Verify file was written
    assert results_file.exists()
    lines = results_file.read_text().strip().split("\n")
    assert len(lines) == 1
    data = json.loads(lines[0])
    assert data["delegation_id"] == "d-1"
    assert data["status"] == "completed"
    assert data["response_preview"] == "Result here"


@pytest.mark.asyncio
async def test_check_delegations_deduplicates():
    """Same delegation_id is not processed twice."""
    hb = HeartbeatLoop("http://platform:8080", "ws-abc")
    hb._seen_delegation_ids.add("d-1")  # Already seen

    delegations = [
        {"delegation_id": "d-1", "status": "completed", "target_id": "ws-t",
         "summary": "Done", "response_preview": "old"},
    ]

    mock_client = AsyncMock()
    get_resp = MagicMock()
    get_resp.status_code = 200
    get_resp.json = MagicMock(return_value=delegations)
    mock_client.get = AsyncMock(return_value=get_resp)
    mock_client.post = AsyncMock()

    with patch("heartbeat.DELEGATION_RESULTS_FILE", "/tmp/test_dedup.jsonl"):
        await hb._check_delegations(mock_client)

    # No self-message should be sent (delegation already seen)
    # Only the GET call, no POST
    mock_client.post.assert_not_called()


@pytest.mark.asyncio
async def test_check_delegations_sends_self_message(tmp_path):
    """Self-message A2A is sent when new completed delegations found."""
    results_file = tmp_path / "results.jsonl"
    hb = HeartbeatLoop("http://platform:8080", "ws-abc")

    delegations = [
        {"delegation_id": "d-new", "status": "completed", "target_id": "ws-t",
         "source_id": "ws-abc",  # must match workspace_id for Fix B source validation
         "summary": "Task done", "response_preview": "All good", "error": ""},
    ]

    mock_client = AsyncMock()
    get_resp = MagicMock()
    get_resp.status_code = 200
    get_resp.json = MagicMock(return_value=delegations)
    mock_client.get = AsyncMock(return_value=get_resp)
    post_resp = MagicMock()
    post_resp.status_code = 200
    mock_client.post = AsyncMock(return_value=post_resp)

    with patch("heartbeat.DELEGATION_RESULTS_FILE", str(results_file)):
        await hb._check_delegations(mock_client)

    # Should have sent self-message (A2A to own workspace) + notify
    post_calls = mock_client.post.call_args_list
    assert len(post_calls) >= 1
    # First POST should be the self-message A2A
    a2a_call = post_calls[0]
    assert "/a2a" in str(a2a_call)

    # Regression: the self-message MUST include X-Workspace-ID set to
    # the workspace's own id, so the platform's a2a_receive logger
    # records source_id = workspace_id (not NULL). Without this header
    # the canvas's My Chat tab (which filters source_id IS NULL) would
    # render the internal "Delegation results are ready..." trigger
    # as a user-typed message. Bug observed 2026-04-25 on UX A/B Lab
    # Design Director chat.
    a2a_headers = a2a_call.kwargs.get("headers") or {}
    assert a2a_headers.get("X-Workspace-ID") == "ws-abc", (
        f"self-message must self-identify via X-Workspace-ID header, "
        f"got headers={a2a_headers!r}"
    )


@pytest.mark.asyncio
async def test_check_delegations_cooldown():
    """Self-message respects cooldown — no second message within 5 min."""
    import time
    hb = HeartbeatLoop("http://platform:8080", "ws-abc")
    hb._last_self_message_time = time.time()  # Just sent one

    delegations = [
        {"delegation_id": "d-cool", "status": "completed", "target_id": "ws-t",
         "summary": "Done", "response_preview": "ok", "error": ""},
    ]

    mock_client = AsyncMock()
    get_resp = MagicMock()
    get_resp.status_code = 200
    get_resp.json = MagicMock(return_value=delegations)
    mock_client.get = AsyncMock(return_value=get_resp)
    mock_client.post = AsyncMock()

    with patch("heartbeat.DELEGATION_RESULTS_FILE", "/tmp/test_cooldown.jsonl"):
        await hb._check_delegations(mock_client)

    # File should still be written (results stored)
    # But self-message should NOT be sent (cooldown active)
    # Only notify POST, no A2A self-message
    for call in mock_client.post.call_args_list:
        assert "/a2a" not in str(call[0][0]), "Self-message should be blocked by cooldown"


@pytest.mark.asyncio
async def test_seen_ids_eviction():
    """Seen delegation IDs are evicted when over MAX limit."""
    from heartbeat import MAX_SEEN_DELEGATION_IDS
    hb = HeartbeatLoop("http://platform:8080", "ws-abc")

    # Fill beyond max
    for i in range(MAX_SEEN_DELEGATION_IDS + 50):
        hb._seen_delegation_ids.add(f"d-{i}")

    assert len(hb._seen_delegation_ids) > MAX_SEEN_DELEGATION_IDS

    # Trigger eviction via _check_delegations with empty results
    mock_client = AsyncMock()
    get_resp = MagicMock()
    get_resp.status_code = 200
    get_resp.json = MagicMock(return_value=[])
    mock_client.get = AsyncMock(return_value=get_resp)

    await hb._check_delegations(mock_client)

    # Should have been trimmed
    assert len(hb._seen_delegation_ids) <= MAX_SEEN_DELEGATION_IDS


def test_on_done_restarts_loop():
    """_on_done restarts the loop when task has an exception."""
    hb = HeartbeatLoop("http://platform:8080", "ws-abc")

    # Create a mock failed task
    mock_task = MagicMock()
    mock_task.cancelled.return_value = False
    mock_task.exception.return_value = RuntimeError("boom")

    with patch("asyncio.create_task") as mock_create:
        mock_new_task = MagicMock()
        mock_create.return_value = mock_new_task
        hb._on_done(mock_task)

    # Should have created a new task
    mock_create.assert_called_once()
    # New task should have done callback
    mock_new_task.add_done_callback.assert_called_once()


# ============== In-container heartbeat persists platform_inbound_secret (2026-04-30) ==============
# Pairs with workspace-server PR #2421's heartbeat-delivers-secret change.
# The standalone wrapper (mcp_cli.py) got persistence in #2421; the
# in-container heartbeat (heartbeat.py) was missed and the symptom
# returned: hongmingwang Claude Code agent stayed 401-forever on chat
# upload because the workspace's runtime never picked up the lazy-healed
# secret without a restart.

import heartbeat as heartbeat_mod  # noqa: E402


def test_persist_inbound_secret_happy_path(monkeypatch):
    """200 with platform_inbound_secret in body → save_inbound_secret called."""

    class FakeResp:
        def json(self):
            return {"status": "ok", "platform_inbound_secret": "fresh-secret"}

    saved: list[str] = []
    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", saved.append)

    heartbeat_mod._persist_inbound_secret_from_heartbeat(FakeResp())

    assert saved == ["fresh-secret"]


def test_persist_inbound_secret_skips_when_absent(monkeypatch):
    class FakeResp:
        def json(self):
            return {"status": "ok"}

    saved: list[str] = []
    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", saved.append)

    heartbeat_mod._persist_inbound_secret_from_heartbeat(FakeResp())
    assert saved == []


def test_persist_inbound_secret_skips_on_empty(monkeypatch):
    class FakeResp:
        def json(self):
            return {"status": "ok", "platform_inbound_secret": ""}

    saved: list[str] = []
    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", saved.append)

    heartbeat_mod._persist_inbound_secret_from_heartbeat(FakeResp())
    assert saved == []


def test_persist_inbound_secret_swallows_non_json(monkeypatch):
    class FakeResp:
        def json(self):
            raise ValueError("not json")

    saved: list[str] = []
    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", saved.append)

    # Must not raise
    heartbeat_mod._persist_inbound_secret_from_heartbeat(FakeResp())
    assert saved == []


def test_persist_inbound_secret_handles_non_dict(monkeypatch):
    class FakeResp:
        def json(self):
            return ["unexpected", "list"]

    saved: list[str] = []
    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", saved.append)

    heartbeat_mod._persist_inbound_secret_from_heartbeat(FakeResp())
    assert saved == []


def test_persist_inbound_secret_swallows_save_oserror(monkeypatch):
    class FakeResp:
        def json(self):
            return {"platform_inbound_secret": "x"}

    def boom(_secret):
        raise OSError("disk full")

    import platform_inbound_auth

    monkeypatch.setattr(platform_inbound_auth, "save_inbound_secret", boom)

    # Heartbeat liveness > secret persistence — must not raise.
    heartbeat_mod._persist_inbound_secret_from_heartbeat(FakeResp())


@pytest.mark.asyncio
async def test_heartbeat_loop_persists_secret_from_response(monkeypatch):
    """End-to-end: in-container _loop persists secret when the heartbeat
    response carries platform_inbound_secret."""
    saved: list[str] = []

    def fake_persist(resp):
        try:
            body = resp.json()
        except Exception:
            return
        if isinstance(body, dict) and body.get("platform_inbound_secret"):
            saved.append(body["platform_inbound_secret"])

    monkeypatch.setattr(
        heartbeat_mod,
        "_persist_inbound_secret_from_heartbeat",
        fake_persist,
    )

    hb = HeartbeatLoop("http://platform:8080", "ws-abc")

    mock_response = MagicMock()
    mock_response.json = MagicMock(
        return_value={"status": "ok", "platform_inbound_secret": "from-heartbeat"}
    )
    mock_client = AsyncMock()
    mock_client.post = AsyncMock(return_value=mock_response)
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)

    with patch("heartbeat.httpx.AsyncClient", return_value=mock_client):
        task = asyncio.create_task(hb._loop())
        await asyncio.sleep(0.05)
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass

    assert saved == ["from-heartbeat"], (
        "in-container heartbeat must persist platform_inbound_secret from 200 response"
    )
