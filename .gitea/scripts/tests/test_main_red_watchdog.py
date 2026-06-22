import importlib.util
import sys
from pathlib import Path
from unittest.mock import patch, MagicMock

SCRIPT = Path(__file__).resolve().parents[1] / "main-red-watchdog.py"
spec = importlib.util.spec_from_file_location("main_red_watchdog", SCRIPT)
wd = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = wd
spec.loader.exec_module(wd)

# Module-level constants are loaded from env at import time; set them
# explicitly so unit tests can import without the full env contract.
wd.GITEA_TOKEN = "fake-token"
wd.GITEA_HOST = "git.example.com"
wd.REPO = "molecule-ai/molecule-core"
wd.OWNER = "molecule-ai"
wd.NAME = "molecule-core"
wd.WATCH_BRANCH = "main"
wd.RED_LABEL = "ci-bp-drift"
wd.API = "https://git.example.com/api/v1"


# ---------------------------------------------------------------------------
# _is_scheduled_context
# ---------------------------------------------------------------------------

def test_is_scheduled_context_matches_staging_saas_smoke():
    assert wd._is_scheduled_context("Staging SaaS smoke") is True


def test_is_scheduled_context_matches_case_insensitive():
    assert wd._is_scheduled_context("continuous synthetic e2e") is True


def test_is_scheduled_context_no_match_for_required_ci():
    assert wd._is_scheduled_context("CI / all-required") is False


# ---------------------------------------------------------------------------
# _entry_state
# ---------------------------------------------------------------------------

def test_entry_state_prefers_status_over_state():
    """Gitea 1.22.6 per-entry key is `status`; `state` is fallback."""
    assert wd._entry_state({"status": "failure", "state": "success"}) == "failure"


def test_entry_state_falls_back_to_state():
    assert wd._entry_state({"state": "pending"}) == "pending"


def test_entry_state_empty_when_neither_key_present():
    assert wd._entry_state({"context": "foo"}) == ""


# ---------------------------------------------------------------------------
# is_red
# ---------------------------------------------------------------------------

def test_is_red_combined_failure_no_statuses():
    """Combined failure with empty statuses[] still trips red."""
    red, failed = wd.is_red({"state": "failure", "statuses": []})
    assert red is True
    assert failed == []


def test_is_red_cancel_cascade_filtered():
    """status=3 (cancelled) mapped to failure string must be filtered."""
    status = {
        "state": "failure",
        "statuses": [
            {"context": "CI / build", "status": "failure", "description": "Has been cancelled"},
        ],
    }
    red, failed = wd.is_red(status)
    assert red is False
    assert failed == []


def test_is_red_real_failure_not_filtered():
    """Real failures with different descriptions are kept."""
    status = {
        "state": "failure",
        "statuses": [
            {"context": "CI / build", "status": "failure", "description": "Failing after 12s"},
        ],
    }
    red, failed = wd.is_red(status)
    assert red is True
    assert len(failed) == 1
    assert failed[0]["context"] == "CI / build"


def test_is_red_uses_entry_state_not_top_level_state():
    """Regression: per-entry key is `status`, not `state`."""
    status = {
        "state": "failure",
        "statuses": [
            # Only `status` present; pre-rev4 code read `state` and got None
            {"context": "CI / test", "status": "failure"},
        ],
    }
    red, failed = wd.is_red(status)
    assert red is True
    assert len(failed) == 1


# ---------------------------------------------------------------------------
# list_open_red_issues — pagination (mc#1789)
# ---------------------------------------------------------------------------

def test_list_open_red_issues_exhausts_pagination():
    """Backlog can exceed 50 issues; all pages must be fetched."""
    calls = []

    def fake_api(method, path, **kwargs):
        calls.append((method, path, kwargs))
        query = (kwargs.get("query") or {})
        page = int(query.get("page", "1"))
        limit = int(query.get("limit", "50"))
        # Page 1 returns full limit; page 2 returns partial → break
        if page == 1:
            return 200, [
                {"title": f"[main-red] molecule-ai/molecule-core: sha{i:04d}"}
                for i in range(limit)
            ]
        if page == 2:
            return 200, [
                {"title": "[main-red] molecule-ai/molecule-core: extra1"},
                {"title": "[main-red] molecule-ai/molecule-core: extra2"},
                {"title": " unrelated issue "},  # filtered out
            ]
        return 200, []

    with patch.object(wd, "api", side_effect=fake_api):
        issues = wd.list_open_red_issues()

    assert len(issues) == 52  # 50 + 2 matched
    titles = {i["title"] for i in issues}
    assert "[main-red] molecule-ai/molecule-core: extra1" in titles
    assert "[main-red] molecule-ai/molecule-core: extra2" in titles


def test_list_open_red_issues_single_page():
    """When results < limit, loop breaks after first page."""
    def fake_api(method, path, **kwargs):
        return 200, [
            {"title": "[main-red] molecule-ai/molecule-core: abc123"},
        ]

    with patch.object(wd, "api", side_effect=fake_api):
        issues = wd.list_open_red_issues()

    assert len(issues) == 1


# ---------------------------------------------------------------------------
# run_once — close logic (mc#1789)
# ---------------------------------------------------------------------------

def test_run_once_green_closes_stale_issues(monkeypatch):
    """Combined success → close stale issues."""
    monkeypatch.setattr(wd, "get_head_sha", lambda b: "abc123")
    monkeypatch.setattr(wd, "get_combined_status", lambda s: {"state": "success", "statuses": []})
    monkeypatch.setattr(wd, "is_red", lambda s: (False, []))

    closed = []

    def capture_close(current_sha, *, dry_run=False, close_same_sha=False):
        closed.append(current_sha)
        return 1

    monkeypatch.setattr(wd, "close_open_red_issues_for_other_shas", capture_close)
    monkeypatch.setattr(wd, "emit_loki_event", lambda *a, **k: None)

    assert wd.run_once(dry_run=True) == 0
    assert closed == ["abc123"]


def test_run_once_pending_scheduled_only_closes_stale_issues(monkeypatch):
    """Combined pending, but only scheduled contexts pending → close stale."""
    monkeypatch.setattr(wd, "get_head_sha", lambda b: "abc123")
    monkeypatch.setattr(
        wd, "get_combined_status",
        lambda s: {
            "state": "pending",
            "statuses": [
                {"context": "CI / all-required", "status": "success"},
                {"context": "Staging SaaS smoke", "status": "pending"},
            ],
        }
    )
    monkeypatch.setattr(wd, "is_red", lambda s: (False, []))

    closed = []

    def capture_close(current_sha, *, dry_run=False, close_same_sha=False):
        closed.append(current_sha)
        return 1

    monkeypatch.setattr(wd, "close_open_red_issues_for_other_shas", capture_close)
    monkeypatch.setattr(wd, "emit_loki_event", lambda *a, **k: None)

    assert wd.run_once(dry_run=True) == 0
    assert closed == ["abc123"]


def test_run_once_pending_required_does_not_close(monkeypatch):
    """Combined pending with a real required context still pending → no close."""
    monkeypatch.setattr(wd, "get_head_sha", lambda b: "abc123")
    monkeypatch.setattr(
        wd, "get_combined_status",
        lambda s: {
            "state": "pending",
            "statuses": [
                {"context": "CI / all-required", "status": "pending"},
                {"context": "Staging SaaS smoke", "status": "success"},
            ],
        }
    )
    monkeypatch.setattr(wd, "is_red", lambda s: (False, []))

    closed = []

    def capture_close(current_sha, *, dry_run=False, close_same_sha=False):
        closed.append(current_sha)
        return 0

    monkeypatch.setattr(wd, "close_open_red_issues_for_other_shas", capture_close)
    monkeypatch.setattr(wd, "emit_loki_event", lambda *a, **k: None)

    assert wd.run_once(dry_run=True) == 0
    assert closed == []


def test_run_once_failure_does_not_close(monkeypatch):
    """Real failure in non-scheduled context → no close."""
    monkeypatch.setattr(wd, "get_head_sha", lambda b: "abc123")
    monkeypatch.setattr(
        wd, "get_combined_status",
        lambda s: {
            "state": "failure",
            "statuses": [
                {"context": "CI / all-required", "status": "failure"},
            ],
        }
    )
    # is_red will return True, so we enter the red path, not the green close path
    monkeypatch.setattr(wd, "is_red", lambda s: (True, s.get("statuses", [])))
    monkeypatch.setattr(wd, "time", MagicMock(sleep=lambda x: None))
    monkeypatch.setattr(wd, "emit_loki_event", lambda *a, **k: None)

    filed = []

    def capture_file(sha, failed, debug, *, dry_run=False):
        filed.append(sha)

    monkeypatch.setattr(wd, "file_or_update_red", capture_file)
    monkeypatch.setattr(wd, "close_open_red_issues_for_other_shas", lambda *a, **k: 0)
    monkeypatch.setattr(wd, "close_stale_red_issues", lambda *a, **k: 0)

    assert wd.run_once(dry_run=True) == 0
    assert filed == ["abc123"]


# ---------------------------------------------------------------------------
# title_for / find_open_issue_for_sha
# ---------------------------------------------------------------------------

def test_title_for_uses_short_sha():
    assert wd.title_for("abcdef123456") == "[main-red] molecule-ai/molecule-core: abcdef1234"


def test_find_open_issue_for_sha_matches_exact_title(monkeypatch):
    fake_issue = {"title": "[main-red] molecule-ai/molecule-core: abc1234567", "number": 42}
    monkeypatch.setattr(wd, "list_open_red_issues", lambda: [fake_issue])
    assert wd.find_open_issue_for_sha("abc1234567") == fake_issue


def test_find_open_issue_for_sha_returns_none_when_no_match(monkeypatch):
    monkeypatch.setattr(wd, "list_open_red_issues", lambda: [])
    assert wd.find_open_issue_for_sha("abc123") is None
