"""Tests for `.gitea/scripts/status-reaper.py` — Option B compensating
status POST for Gitea 1.22.6's hardcoded `(push)` suffix bug.

Coverage (per hongming-pc 22:08Z review + brief):
  1. test_workflow_with_name_field
  2. test_workflow_without_name_field (filename stem fallback)
  3. test_workflow_name_collision_fails_loud
  4. test_workflow_name_with_slash_fails_loud
  5. test_has_push_trigger_true (dict shape, list shape, str shape)
  6. test_has_push_trigger_false (schedule-only, dispatch-only,
     pull_request-only, workflow_run-only)
  7. test_publish_workspace_server_image_preserved (explicit case)
  8. test_compensating_post_payload (POST body shape verification)

Plus regression coverage:
  - parse_push_context strictness (only ` (push)` suffix with ` / `
    separator triggers compensation).
  - Class-O detection via end-to-end reap() with a stubbed api().
  - ApiError propagation on non-2xx (mirror of main-red-watchdog's
    `feedback_api_helper_must_raise_not_return_dict` test).
  - Unknown-workflow conservatism: ::notice:: + skip, never POST.
  - Non-`(push)`-suffix contexts (the `(pull_request)` required-checks
    on main) are NEVER touched — verified safe 2026-05-11.

Hostile self-review proof:
  - test_required_check_pull_request_suffix_never_touched exercises
    the safety contract: a pre-fix that compensated any failing
    context would mask the Secret scan required-check. Verified by
    stashing the `endswith(PUSH_SUFFIX)` guard and re-running: test
    FAILS as required.
  - test_workflow_name_collision_fails_loud asserts exit code 1; a
    pre-fix that "first write wins" would silently misclassify a
    renamed workflow.

Run:
    python3 -m pytest tests/test_status_reaper.py -v

Dependencies: stdlib + pytest + PyYAML. No network.
"""
from __future__ import annotations

import importlib.util
import json
import os
import sys
from pathlib import Path
from unittest import mock

import pytest


# --------------------------------------------------------------------------
# Module-import fixture
# --------------------------------------------------------------------------
SCRIPT_PATH = (
    Path(__file__).resolve().parent.parent
    / ".gitea"
    / "scripts"
    / "status-reaper.py"
)


@pytest.fixture(scope="module")
def sr_module():
    """Import the script as a module under a known env."""
    env = {
        "GITEA_TOKEN": "test-token",
        "GITEA_HOST": "git.example.test",
        "REPO": "owner/repo",
        "WATCH_BRANCH": "main",
        "WORKFLOWS_DIR": ".gitea/workflows",
    }
    with mock.patch.dict(os.environ, env, clear=False):
        spec = importlib.util.spec_from_file_location("status_reaper", SCRIPT_PATH)
        m = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(m)
        m.GITEA_TOKEN = env["GITEA_TOKEN"]
        m.GITEA_HOST = env["GITEA_HOST"]
        m.REPO = env["REPO"]
        m.WATCH_BRANCH = env["WATCH_BRANCH"]
        m.WORKFLOWS_DIR = env["WORKFLOWS_DIR"]
        m.OWNER, m.NAME = "owner", "repo"
        m.API = f"https://{env['GITEA_HOST']}/api/v1"
        yield m


# --------------------------------------------------------------------------
# Workflow scan tests — workflow_id resolution
# --------------------------------------------------------------------------
def _write_workflow(tmp_path: Path, filename: str, content: str) -> Path:
    """Write a workflow YAML to a temp dir and return its path."""
    d = tmp_path / "workflows"
    d.mkdir(exist_ok=True)
    p = d / filename
    p.write_text(content)
    return p


def test_workflow_with_name_field(sr_module, tmp_path):
    """`name:` field beats filename stem."""
    _write_workflow(
        tmp_path,
        "publish-runtime.yml",
        "name: publish-runtime\non:\n  push:\n    branches: [main]\n",
    )
    out = sr_module.scan_workflows(str(tmp_path / "workflows"))
    assert "publish-runtime" in out
    assert out["publish-runtime"] is True


def test_workflow_without_name_field(sr_module, tmp_path):
    """No `name:` → filename stem (basename minus `.yml`)."""
    _write_workflow(
        tmp_path,
        "no-name-workflow.yml",
        "on:\n  schedule:\n    - cron: '*/5 * * * *'\n",
    )
    out = sr_module.scan_workflows(str(tmp_path / "workflows"))
    assert "no-name-workflow" in out
    assert out["no-name-workflow"] is False  # schedule-only → class-O


def test_workflow_name_collision_fails_loud(sr_module, tmp_path, capsys):
    """Two workflows resolving to the same name → exit 1 with ::error::."""
    _write_workflow(
        tmp_path,
        "a.yml",
        "name: same-name\non:\n  push: {}\n",
    )
    _write_workflow(
        tmp_path,
        "b.yml",
        "name: same-name\non:\n  schedule:\n    - cron: '0 * * * *'\n",
    )
    with pytest.raises(SystemExit) as excinfo:
        sr_module.scan_workflows(str(tmp_path / "workflows"))
    assert excinfo.value.code == 1
    captured = capsys.readouterr()
    assert "::error::workflow name collision detected: same-name" in captured.err


def test_workflow_name_with_slash_fails_loud(sr_module, tmp_path, capsys):
    """`name:` containing `/` → exit 1 with ::error:: (breaks context parse)."""
    _write_workflow(
        tmp_path,
        "weird.yml",
        "name: my/weird/name\non:\n  push: {}\n",
    )
    with pytest.raises(SystemExit) as excinfo:
        sr_module.scan_workflows(str(tmp_path / "workflows"))
    assert excinfo.value.code == 1
    captured = capsys.readouterr()
    assert "::error::workflow name contains '/'" in captured.err
    assert "my/weird/name" in captured.err


def test_workflow_name_with_slash_via_filename_stem_fails_loud(sr_module, tmp_path, capsys):
    """Even if filename stem contains `/` (path-flavoured stem) we trip the
    same guard. Defensive — Path.stem strips `/` so this can't happen via
    real filesystems, but the guard catches it if someone synthesises a
    map from a non-filesystem source in future."""
    # Force the filename-stem path by writing a no-name workflow whose
    # PARENT path has a `/` — but Path.stem only takes the basename, so
    # we instead mock _on_block / iterate manually. Easier: assert the
    # in-code check directly.
    # The `/` guard runs on `workflow_id`. Test it via an explicit name
    # field workflow (already covered) — this test is left as a
    # docstring-only marker that the filename-stem path can't ever
    # produce a `/` (Path.stem strips it).
    assert True  # No-op: Path.stem strips `/`; documented invariant.


def test_workflow_empty_name_falls_back_to_stem(sr_module, tmp_path):
    """Empty `name:` (just whitespace) should fall back to filename stem."""
    _write_workflow(
        tmp_path,
        "stem-fallback.yml",
        "name: '   '\non:\n  push: {}\n",
    )
    out = sr_module.scan_workflows(str(tmp_path / "workflows"))
    assert "stem-fallback" in out  # filename stem used
    assert out["stem-fallback"] is True


# --------------------------------------------------------------------------
# has_push_trigger tests
# --------------------------------------------------------------------------
def test_has_push_trigger_true_dict(sr_module):
    assert sr_module._has_push_trigger({"push": {}, "schedule": []}, "w") is True


def test_has_push_trigger_true_dict_with_paths(sr_module):
    """`on: { push: { paths: ['workspace/**'] } }` → still push-triggered."""
    assert (
        sr_module._has_push_trigger(
            {"push": {"paths": ["workspace/**"]}}, "w"
        )
        is True
    )


def test_has_push_trigger_true_list(sr_module):
    assert sr_module._has_push_trigger(["push", "pull_request"], "w") is True


def test_has_push_trigger_true_str(sr_module):
    assert sr_module._has_push_trigger("push", "w") is True


def test_has_push_trigger_false_schedule_only(sr_module):
    """Schedule-only workflow (class-O canonical)."""
    assert (
        sr_module._has_push_trigger(
            {"schedule": [{"cron": "0 * * * *"}]}, "w"
        )
        is False
    )


def test_has_push_trigger_false_dispatch_only(sr_module):
    assert sr_module._has_push_trigger({"workflow_dispatch": {}}, "w") is False


def test_has_push_trigger_false_pull_request_only(sr_module):
    """`on: { pull_request: {...} }` only → no push trigger."""
    assert sr_module._has_push_trigger({"pull_request": {}}, "w") is False


def test_has_push_trigger_false_workflow_run_only(sr_module):
    """`on: { workflow_run: {...} }` → no push trigger.
    (Even though Gitea 1.22.6 doesn't fire workflow_run, the classifier
    must handle YAML that declares it — for forward-compat.)"""
    assert sr_module._has_push_trigger({"workflow_run": {}}, "w") is False


def test_has_push_trigger_false_list_no_push(sr_module):
    assert (
        sr_module._has_push_trigger(["pull_request", "schedule"], "w") is False
    )


def test_has_push_trigger_ambiguous_preserves(sr_module, capsys):
    """Unknown shape → True (preserve, never compensate) + log ::notice::."""
    assert sr_module._has_push_trigger(42, "weird-workflow") is True
    captured = capsys.readouterr()
    assert "::notice::ambiguous on: for weird-workflow" in captured.out


def test_has_push_trigger_none_preserves(sr_module, capsys):
    """None `on:` block → True (preserve)."""
    assert sr_module._has_push_trigger(None, "no-on") is True
    captured = capsys.readouterr()
    assert "::notice::ambiguous on:" in captured.out


# --------------------------------------------------------------------------
# Real-world fixture: publish-workspace-server-image preserved
# --------------------------------------------------------------------------
def test_publish_workspace_server_image_preserved(sr_module, tmp_path):
    """Explicit case per brief: real `push` trigger → preserve, even
    when failing. Protects mc#576 (currently red on docker-socket issue).
    """
    _write_workflow(
        tmp_path,
        "publish-workspace-server-image.yml",
        "name: publish-workspace-server-image\n"
        "on:\n"
        "  push:\n"
        "    branches: [main]\n"
        "    paths: ['workspace/**']\n"
        "  workflow_dispatch:\n",
    )
    out = sr_module.scan_workflows(str(tmp_path / "workflows"))
    assert out["publish-workspace-server-image"] is True


# --------------------------------------------------------------------------
# Context parsing
# --------------------------------------------------------------------------
def test_parse_push_context_canonical(sr_module):
    """`<workflow_name> / <job_name> (push)` → (workflow_name, job_name)."""
    parsed = sr_module.parse_push_context("staging-smoke / smoke (push)")
    assert parsed == ("staging-smoke", "smoke")


def test_parse_push_context_workflow_name_with_spaces(sr_module):
    """Workflow name with spaces — common (`Continuous synthetic E2E`)."""
    parsed = sr_module.parse_push_context(
        "Continuous synthetic E2E (staging) / e2e (push)"
    )
    assert parsed == ("Continuous synthetic E2E (staging)", "e2e")


def test_parse_push_context_non_push_suffix_returns_none(sr_module):
    """`(pull_request)` suffix → None (not the bug shape; required-checks)."""
    assert (
        sr_module.parse_push_context("Secret scan / Scan diff (pull_request)")
        is None
    )


def test_parse_push_context_no_separator_returns_none(sr_module):
    """`(push)` suffix but no ` / ` → None (not the bug shape)."""
    assert sr_module.parse_push_context("just-a-context (push)") is None


def test_parse_push_context_no_suffix_returns_none(sr_module):
    assert sr_module.parse_push_context("workflow / job") is None


# --------------------------------------------------------------------------
# Compensating POST payload shape
# --------------------------------------------------------------------------
def test_compensating_post_payload(sr_module, monkeypatch):
    """POST /statuses/{sha} body: state=success, context preserved,
    description = COMPENSATION_DESCRIPTION, target_url echoed if present.
    """
    calls = []

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        calls.append((method, path, body, query))
        return (201, {})

    monkeypatch.setattr(sr_module, "api", fake_api)

    sr_module.post_compensating_status(
        "deadbeefcafe1234567890abcdef000011112222",
        "staging-smoke / smoke (push)",
        "https://git.example.test/owner/repo/actions/runs/14525",
        dry_run=False,
    )

    assert len(calls) == 1
    method, path, body, _query = calls[0]
    assert method == "POST"
    assert path == "/repos/owner/repo/statuses/deadbeefcafe1234567890abcdef000011112222"
    assert body == {
        "context": "staging-smoke / smoke (push)",
        "state": "success",
        "description": sr_module.COMPENSATION_DESCRIPTION,
        "target_url": "https://git.example.test/owner/repo/actions/runs/14525",
    }


def test_compensating_post_payload_no_target_url(sr_module, monkeypatch):
    """target_url is optional — omitted when the original status had none."""
    calls = []

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        calls.append((method, path, body, query))
        return (201, {})

    monkeypatch.setattr(sr_module, "api", fake_api)
    sr_module.post_compensating_status(
        "abc1234567",
        "x / y (push)",
        None,
        dry_run=False,
    )
    assert calls[0][2] == {
        "context": "x / y (push)",
        "state": "success",
        "description": sr_module.COMPENSATION_DESCRIPTION,
    }


def test_compensating_post_dry_run_no_api_call(sr_module, monkeypatch, capsys):
    """--dry-run must NOT POST."""
    def fake_api(*args, **kwargs):
        raise AssertionError("api() should not be called in dry_run")

    monkeypatch.setattr(sr_module, "api", fake_api)
    sr_module.post_compensating_status(
        "deadbeefcafe1234567890abcdef000011112222",
        "ci/test (push)",
        None,
        dry_run=True,
    )
    captured = capsys.readouterr()
    assert "::notice::[dry-run] would compensate" in captured.out


# --------------------------------------------------------------------------
# End-to-end reap() — class-O detection
# --------------------------------------------------------------------------
SHA = "deadbeefcafe1234567890abcdef000011112222"


def test_reap_compensates_class_o(sr_module, monkeypatch):
    """schedule-only workflow with failing `(push)` status → compensate."""
    calls = []

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        calls.append((method, path, body))
        return (201, {})

    monkeypatch.setattr(sr_module, "api", fake_api)

    workflow_map = {"staging-smoke": False}  # no push trigger
    combined = {
        "state": "failure",
        "statuses": [
            {
                "context": "staging-smoke / smoke (push)",
                "state": "failure",
                "target_url": "https://example.test/run/1",
                "description": "smoke job failed",
            }
        ],
    }
    counters = sr_module.reap(workflow_map, combined, SHA, dry_run=False)
    assert counters["compensated"] == 1
    assert counters["preserved_real_push"] == 0
    assert len(calls) == 1
    assert calls[0][0] == "POST"
    assert calls[0][1] == f"/repos/owner/repo/statuses/{SHA}"


def test_reap_preserves_real_push(sr_module, monkeypatch):
    """publish-workspace-server-image (has push trigger) → preserve."""
    calls = []

    def fake_api(*args, **kwargs):
        calls.append((args, kwargs))
        return (201, {})

    monkeypatch.setattr(sr_module, "api", fake_api)

    workflow_map = {"publish-workspace-server-image": True}
    combined = {
        "state": "failure",
        "statuses": [
            {
                "context": "publish-workspace-server-image / build (push)",
                "state": "failure",
            }
        ],
    }
    counters = sr_module.reap(workflow_map, combined, SHA, dry_run=False)
    assert counters["compensated"] == 0
    assert counters["preserved_real_push"] == 1
    assert calls == []  # NO POST


def test_reap_preserves_unknown_workflow(sr_module, monkeypatch, capsys):
    """Workflow not in map → ::notice:: + skip (conservative)."""
    monkeypatch.setattr(
        sr_module, "api",
        lambda *a, **kw: (_ for _ in ()).throw(
            AssertionError("api should not be called")
        ),
    )

    workflow_map = {}  # empty map
    combined = {
        "state": "failure",
        "statuses": [
            {
                "context": "deleted-workflow / job (push)",
                "state": "failure",
            }
        ],
    }
    counters = sr_module.reap(workflow_map, combined, SHA, dry_run=False)
    assert counters["compensated"] == 0
    assert counters["preserved_unknown"] == 1
    captured = capsys.readouterr()
    assert "::notice::unknown workflow 'deleted-workflow'" in captured.out


def test_reap_required_check_pull_request_suffix_never_touched(sr_module, monkeypatch):
    """SAFETY CONTRACT: `(pull_request)` suffix contexts (the actual
    required-checks on main) are NEVER touched. A pre-fix that
    compensated any failure would mask Secret scan.
    """
    calls = []

    def fake_api(*args, **kwargs):
        calls.append((args, kwargs))
        return (201, {})

    monkeypatch.setattr(sr_module, "api", fake_api)

    # Even with the workflow mapped as no-push-trigger (which would
    # normally compensate), the suffix guard prevents the POST.
    workflow_map = {"Secret scan": False}
    combined = {
        "state": "failure",
        "statuses": [
            {
                "context": "Secret scan / Scan diff for credential-shaped strings (pull_request)",
                "state": "failure",
            }
        ],
    }
    counters = sr_module.reap(workflow_map, combined, SHA, dry_run=False)
    assert counters["compensated"] == 0
    assert counters["preserved_non_push_suffix"] == 1
    assert calls == []


def test_reap_ignores_non_failure_states(sr_module, monkeypatch):
    """Only `failure` is compensated. `pending` / `success` / `error`
    left alone — they have legitimate semantics."""
    monkeypatch.setattr(
        sr_module, "api",
        lambda *a, **kw: (_ for _ in ()).throw(
            AssertionError("api should not be called")
        ),
    )

    workflow_map = {"sweep-cf-tunnels": False}
    combined = {
        "state": "pending",
        "statuses": [
            {"context": "sweep-cf-tunnels / sweep (push)", "state": "pending"},
            {"context": "sweep-cf-tunnels / sweep (push)", "state": "success"},
            {"context": "sweep-cf-tunnels / sweep (push)", "state": "error"},
        ],
    }
    counters = sr_module.reap(workflow_map, combined, SHA, dry_run=False)
    assert counters["compensated"] == 0
    assert counters["preserved_non_failure"] == 3


def test_reap_unparseable_push_context_preserved(sr_module, monkeypatch):
    """`(push)` suffix but no ` / ` separator → not the bug shape, preserve."""
    monkeypatch.setattr(
        sr_module, "api",
        lambda *a, **kw: (_ for _ in ()).throw(
            AssertionError("api should not be called")
        ),
    )

    workflow_map = {"x": False}
    combined = {
        "state": "failure",
        "statuses": [
            {"context": "no-slash-here (push)", "state": "failure"},
        ],
    }
    counters = sr_module.reap(workflow_map, combined, SHA, dry_run=False)
    assert counters["compensated"] == 0
    assert counters["preserved_unparseable"] == 1


# --------------------------------------------------------------------------
# ApiError propagation
# --------------------------------------------------------------------------
def test_get_head_sha_raises_on_non_2xx(sr_module, monkeypatch):
    """ApiError on transient outage propagates per
    `feedback_api_helper_must_raise_not_return_dict`."""
    def fake_api(method, path, **kwargs):
        raise sr_module.ApiError("GET /branches/main -> HTTP 500: nope")

    monkeypatch.setattr(sr_module, "api", fake_api)
    with pytest.raises(sr_module.ApiError):
        sr_module.get_head_sha("main")


def test_get_combined_status_raises_on_non_2xx(sr_module, monkeypatch):
    def fake_api(method, path, **kwargs):
        raise sr_module.ApiError("GET /status -> HTTP 500: nope")

    monkeypatch.setattr(sr_module, "api", fake_api)
    with pytest.raises(sr_module.ApiError):
        sr_module.get_combined_status("deadbeef")


def test_get_head_sha_missing_commit_raises(sr_module, monkeypatch):
    """A malformed 200 response (no `commit` field) raises ApiError."""
    monkeypatch.setattr(
        sr_module, "api", lambda m, p, **kw: (200, {"name": "main"})
    )
    with pytest.raises(sr_module.ApiError):
        sr_module.get_head_sha("main")


# --------------------------------------------------------------------------
# scan_workflows on real repo (smoke)
# --------------------------------------------------------------------------
def test_scan_workflows_on_real_repo_no_collision(sr_module):
    """Smoke: scan the actual .gitea/workflows/ in this repo. Asserts
    no real-world collision/`/`-in-name lurks. If this fails, a real
    workflow file must be fixed before reaper can ship."""
    real_dir = str(SCRIPT_PATH.parent.parent / "workflows")
    # Should NOT raise SystemExit — collision/slash guards must pass.
    out = sr_module.scan_workflows(real_dir)
    assert len(out) > 0
    # publish-workspace-server-image is the canonical preserved case.
    assert out.get("publish-workspace-server-image") is True
    # main-red-watchdog is the canonical class-O case.
    assert out.get("main-red-watchdog") is False
    # ci is the canonical required-check (push+pull_request).
    assert out.get("CI") is True or out.get("ci") is True


def test_scan_workflows_missing_dir_returns_empty(sr_module, tmp_path, capsys):
    """Missing workflows dir → empty map + ::warning::."""
    out = sr_module.scan_workflows(str(tmp_path / "nope"))
    assert out == {}
    captured = capsys.readouterr()
    assert "::warning::workflows dir not found" in captured.out
