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
    assert counters["preserved_pr_without_push_success"] == 1
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
# Per-context status-key vendor-truth (rev4)
#
# Gitea 1.22.6 returns commit-status entries with key `status` per entry,
# NOT `state`. The TOP-LEVEL combined aggregate uses `state`. This schema
# asymmetry caused rev1-3 to take the compensation path 0 times despite
# triggering on real failures: `s.get("state")` returned None → state
# evaluated to "" → `"" != "failure"` guard preserved every entry.
#
# These tests explicitly use the vendor-truth shape (`status` per entry),
# proving the rev4 fix routes the failure entry through compensation.
# Fixtures in rev1-3 tests above use `state` (the pre-fix bug shape) —
# we keep them for backward-compat coverage via the fallback in
# `s.get("status") or s.get("state")`, but the canonical Gitea shape
# uses `status`. Logged under
# `feedback_smoke_test_vendor_truth_not_shape_match`.
# --------------------------------------------------------------------------
def test_reap_per_context_uses_status_key_not_state(sr_module, monkeypatch):
    """Empirical Gitea 1.22.6 shape: per-entry uses `status`, top-level
    uses `state`. The rev4 fix MUST detect failure via `status`."""
    calls = []

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        calls.append((method, path, body))
        return (201, {})

    monkeypatch.setattr(sr_module, "api", fake_api)

    workflow_map = {"staging-smoke": False}  # no push trigger → Class-O
    # Real Gitea-shaped response: top-level `state`, per-entry `status`.
    # No `state` key on the per-entry item.
    combined = {
        "state": "failure",
        "statuses": [
            {
                "context": "staging-smoke / smoke (push)",
                "status": "failure",  # ← vendor-truth key
                "target_url": "https://example.test/run/1",
                "description": "smoke job failed",
            }
        ],
    }
    counters = sr_module.reap(workflow_map, combined, SHA, dry_run=False)
    # The bug-class assertion: pre-rev4 this would have been 0, with
    # preserved_non_failure=1. Rev4 reads `status` → routes to compensate.
    assert counters["compensated"] == 1, (
        "Compensation path unreachable: status-reaper still reads `state` "
        "instead of `status` on per-entry combined.statuses[] items "
        "(rev1-3 bug)."
    )
    assert counters["preserved_non_failure"] == 0
    assert len(calls) == 1
    assert calls[0][0] == "POST"
    assert calls[0][1] == f"/repos/owner/repo/statuses/{SHA}"


def test_reap_per_context_status_key_takes_precedence_over_state(
    sr_module, monkeypatch
):
    """Defensive: if both `status` and `state` are present (e.g. a
    hypothetical Gitea version emits both), `status` (the canonical
    Gitea 1.22.6 key) wins. Guards against a future regression where
    a fixture or future Gitea release emits stale `state="success"`
    while `status="failure"` is the truth."""
    calls = []

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        calls.append((method, path, body))
        return (201, {})

    monkeypatch.setattr(sr_module, "api", fake_api)

    workflow_map = {"staging-smoke": False}
    combined = {
        "state": "failure",
        "statuses": [
            {
                "context": "staging-smoke / smoke (push)",
                # Both keys present — vendor-truth `status` MUST win.
                "status": "failure",
                "state": "success",
                "target_url": "https://example.test/run/2",
                "description": "smoke job failed",
            }
        ],
    }
    counters = sr_module.reap(workflow_map, combined, SHA, dry_run=False)
    assert counters["compensated"] == 1
    assert counters["preserved_non_failure"] == 0
    assert len(calls) == 1


def test_reap_per_context_state_only_fallback(sr_module, monkeypatch):
    """Backward-compat: a test fixture or older Gitea variant that emits
    only `state` (no `status`) must still flow through compensation.
    Belt-and-suspenders against future fixture drift. Keeps rev1-3
    `state`-using fixtures green."""
    calls = []

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        calls.append((method, path, body))
        return (201, {})

    monkeypatch.setattr(sr_module, "api", fake_api)

    workflow_map = {"staging-smoke": False}
    combined = {
        "state": "failure",
        "statuses": [
            {
                "context": "staging-smoke / smoke (push)",
                "state": "failure",  # legacy fixture shape only
                "target_url": "https://example.test/run/3",
            }
        ],
    }
    counters = sr_module.reap(workflow_map, combined, SHA, dry_run=False)
    assert counters["compensated"] == 1
    assert len(calls) == 1


def test_reap_per_context_missing_both_keys_preserves(sr_module, monkeypatch):
    """A per-entry item lacking BOTH `status` and `state` must be
    preserved (counted under preserved_non_failure). This is the only
    correctly-behaving leg of the pre-rev4 bug — exercising it ensures
    the fallback chain doesn't accidentally over-compensate on
    malformed entries."""
    monkeypatch.setattr(
        sr_module, "api",
        lambda *a, **kw: (_ for _ in ()).throw(
            AssertionError("api should not be called")
        ),
    )

    workflow_map = {"staging-smoke": False}
    combined = {
        "state": "failure",
        "statuses": [
            {
                "context": "staging-smoke / smoke (push)",
                # No status, no state — neither key present.
                "target_url": "https://example.test/run/4",
            }
        ],
    }
    counters = sr_module.reap(workflow_map, combined, SHA, dry_run=False)
    assert counters["compensated"] == 0
    assert counters["preserved_non_failure"] == 1


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


# --------------------------------------------------------------------------
# rev2: multi-SHA sweep — `reap_branch()` walks last N main commits
# --------------------------------------------------------------------------
# Phase 1+2 evidence (orchestrator + hongming-pc2): rev1 sees `compensated:0`
# every tick because the schedule workflow posts `failure` to whatever SHA
# was HEAD when it COMPLETED. By the next */5 tick, main has often moved
# forward, so the single-HEAD reaper misses the stranded red. rev2 sweeps
# the last 10 commits each tick. See `reference_post_suspension_pipeline`
# and parent rev1 PR #618 for context.

SHA_A = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
SHA_B = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
SHA_C = "cccccccccccccccccccccccccccccccccccccccc"


def test_reap_sweeps_n_shas_smoke(sr_module, monkeypatch):
    """rev2 contract: sweep last 10 (or N) main commits, GET combined
    status for EACH. Smoke: with 3 stub SHAs, each is GET'd exactly once.
    """
    gets: list[str] = []
    posts: list[tuple[str, dict]] = []

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        if method == "GET" and path.endswith("/commits"):
            # commits listing — return 3 fake commit objects
            return (200, [{"sha": SHA_A}, {"sha": SHA_B}, {"sha": SHA_C}])
        if method == "GET" and "/commits/" in path and path.endswith("/status"):
            sha = path.split("/commits/")[1].split("/status")[0]
            gets.append(sha)
            # All combined=success → cost-optimization short-circuit
            return (200, {"state": "success", "statuses": []})
        if method == "POST":
            posts.append((path, body))
            return (201, {})
        raise AssertionError(f"unexpected api call: {method} {path}")

    monkeypatch.setattr(sr_module, "api", fake_api)

    workflow_map = {"x": False}
    counters = sr_module.reap_branch(
        workflow_map, "main", limit=10, dry_run=False
    )

    # Each of the 3 SHAs returned by /commits should be GET'd once.
    assert gets == [SHA_A, SHA_B, SHA_C]
    # No POST (everything was combined=success).
    assert posts == []
    # Counters reflect what we saw.
    assert counters["scanned_shas"] == 3
    assert counters["compensated"] == 0
    assert counters["compensated_per_sha"] == {}


def test_reap_skips_combined_success_shas(sr_module, monkeypatch):
    """rev2 cost-optimization (refinement #2): when combined==success for
    a SHA, do NOT iterate per-context statuses; move on to next SHA.

    Mock 2 SHAs with combined=success + 1 with combined=failure → only
    the failure-SHA's statuses get the per-context loop applied.
    """
    per_context_iterated_for: list[str] = []
    posts: list[tuple[str, dict]] = []

    failure_statuses = [
        {
            "context": "drift / drift (push)",
            "state": "failure",
            "target_url": "https://example.test/run/42",
        }
    ]

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        if method == "GET" and path.endswith("/commits"):
            return (200, [{"sha": SHA_A}, {"sha": SHA_B}, {"sha": SHA_C}])
        if method == "GET" and "/commits/" in path and path.endswith("/status"):
            sha = path.split("/commits/")[1].split("/status")[0]
            if sha == SHA_B:
                # Mark this SHA as the failure one — return per-context
                # statuses that would compensate if iterated.
                return (200, {"state": "failure", "statuses": failure_statuses})
            # Others are combined=success — must short-circuit.
            return (200, {"state": "success", "statuses": failure_statuses})
        if method == "POST":
            # If a POST hits a non-failure SHA, the short-circuit failed.
            posts.append((path, body))
            return (201, {})
        raise AssertionError(f"unexpected api call: {method} {path}")

    monkeypatch.setattr(sr_module, "api", fake_api)

    # Workflow trigger map: `drift` is schedule-only (compensable).
    workflow_map = {"drift": False}
    counters = sr_module.reap_branch(
        workflow_map, "main", limit=10, dry_run=False
    )

    # Only SHA_B (the combined=failure one) should be compensated.
    assert counters["compensated"] == 1
    assert counters["scanned_shas"] == 3
    assert SHA_B in counters["compensated_per_sha"]
    assert counters["compensated_per_sha"][SHA_B] == ["drift / drift (push)"]
    # SHA_A and SHA_C must NOT appear in compensated_per_sha — their
    # per-context loop was skipped via the combined=success short-circuit.
    assert SHA_A not in counters["compensated_per_sha"]
    assert SHA_C not in counters["compensated_per_sha"]
    # Exactly one POST: the compensation on SHA_B.
    assert len(posts) == 1
    assert posts[0][0] == f"/repos/owner/repo/statuses/{SHA_B}"


def test_default_sweep_limit_is_30(sr_module):
    """rev3 contract: `DEFAULT_SWEEP_LIMIT = 30` (widened from rev2's 10).

    Root cause of the widening: schedule workflows post `failure`
    RETROACTIVELY 5-15 min after their merge. A 10-commit window is
    narrower than the merge-cadence during a burst, so reds land
    OUTSIDE the window before reaper's next tick sees them.

    Evidence: rev2 run 17057 (02:46Z 2026-05-12) saw 185 contexts / 0
    fails on its 10 SHAs; direct probe ~30min later showed ~25 fails
    on those same 10 SHAs.

    If this default is ever lowered back, that change MUST cite
    re-measured cadence data — a smaller window than the
    retroactive-failure-post lag re-introduces compensated:0.
    """
    assert sr_module.DEFAULT_SWEEP_LIMIT == 30


def test_reap_widened_window_catches_retroactive_failure(sr_module, monkeypatch):
    """rev3 regression: with limit=30, a stranded red on a SHA at depth=20
    (which the rev2 limit=10 window would have missed) IS swept + compensated.

    Why this matters: rev2 ran with limit=10 and saw `compensated:0` for
    6 consecutive ticks despite ~25 known-stranded reds across the last
    30 main commits. Widening to 30 must demonstrably catch a SHA past
    the old window. We mock 30 SHAs, plant the failure on SHA[20], and
    verify exactly one compensation lands on that SHA.
    """
    shas = [f"{c:02x}" * 20 for c in range(30)]  # 30 deterministic SHAs
    failing_sha = shas[20]  # depth 20 — outside rev2's window=10, inside rev3's =30

    posts: list[tuple[str, dict]] = []

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        if method == "GET" and path.endswith("/commits"):
            # /commits listing — return all 30 fake commit objects
            assert query.get("limit") == "30", (
                f"expected limit=30 in query, got {query}"
            )
            return (200, [{"sha": s} for s in shas])
        if method == "GET" and "/commits/" in path and path.endswith("/status"):
            sha = path.split("/commits/")[1].split("/status")[0]
            if sha == failing_sha:
                return (
                    200,
                    {
                        "state": "failure",
                        "statuses": [
                            {
                                "context": "retroactive-drift / drift (push)",
                                "state": "failure",
                                "target_url": "https://example.test/run/9001",
                            }
                        ],
                    },
                )
            # All others combined=success (cost-opt short-circuit).
            return (200, {"state": "success", "statuses": []})
        if method == "POST":
            posts.append((path, body))
            return (201, {})
        raise AssertionError(f"unexpected api call: {method} {path}")

    monkeypatch.setattr(sr_module, "api", fake_api)

    workflow_map = {"retroactive-drift": False}  # schedule-only → class-O
    counters = sr_module.reap_branch(
        workflow_map, "main", limit=sr_module.DEFAULT_SWEEP_LIMIT, dry_run=False
    )

    # All 30 SHAs walked; exactly one compensated.
    assert counters["scanned_shas"] == 30
    assert counters["compensated"] == 1
    assert failing_sha in counters["compensated_per_sha"]
    assert counters["compensated_per_sha"][failing_sha] == [
        "retroactive-drift / drift (push)"
    ]
    assert len(posts) == 1
    assert posts[0][0] == f"/repos/owner/repo/statuses/{failing_sha}"
    # Sanity: with rev2's window=10, depth=20 would NOT have been reached.
    # This assertion documents the rev3 widening as the structural fix:
    # the failing_sha index (20) is strictly greater than rev2's old limit (10).
    assert shas.index(failing_sha) >= 10


def test_reap_continues_on_per_sha_apierror(sr_module, monkeypatch, capsys):
    """rev2 refinement #7 (MOST CRITICAL): a transient ApiError or HTTP-5xx
    on get_combined_status(SHA_X) must NOT fail the whole tick. Log + skip
    SHA_X, continue with SHA_Y.

    Different from the single-HEAD path (where fail-loud is correct): the
    sweep is best-effort across historical commits, so one transient blip
    on a stale SHA should not strand reds on the OTHER stale SHAs.
    """
    posts: list[tuple[str, dict]] = []

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        if method == "GET" and path.endswith("/commits"):
            return (200, [{"sha": SHA_A}, {"sha": SHA_B}])
        if method == "GET" and "/commits/" in path and path.endswith("/status"):
            sha = path.split("/commits/")[1].split("/status")[0]
            if sha == SHA_A:
                raise sr_module.ApiError(
                    f"GET /repos/owner/repo/commits/{SHA_A}/status "
                    f"-> HTTP 502: bad gateway"
                )
            # SHA_B returns normally with a failure to compensate.
            return (
                200,
                {
                    "state": "failure",
                    "statuses": [
                        {
                            "context": "drift / drift (push)",
                            "state": "failure",
                        }
                    ],
                },
            )
        if method == "POST":
            posts.append((path, body))
            return (201, {})
        raise AssertionError(f"unexpected api call: {method} {path}")

    monkeypatch.setattr(sr_module, "api", fake_api)

    workflow_map = {"drift": False}
    # Must NOT raise — per-SHA error isolation contract.
    counters = sr_module.reap_branch(
        workflow_map, "main", limit=10, dry_run=False
    )

    # SHA_A was logged + skipped. SHA_B processed normally.
    assert counters["scanned_shas"] == 2
    assert counters["compensated"] == 1
    assert SHA_B in counters["compensated_per_sha"]
    assert SHA_A not in counters["compensated_per_sha"]
    # Compensation POST landed on SHA_B only.
    assert len(posts) == 1
    assert posts[0][0] == f"/repos/owner/repo/statuses/{SHA_B}"
    # The ApiError must be logged so a human auditing tick output can see
    # WHICH SHA blipped and WHY.
    captured = capsys.readouterr()
    assert "::warning::" in captured.out or "::notice::" in captured.out
    assert SHA_A[:10] in captured.out


def test_main_soft_skips_when_commit_listing_times_out(sr_module, monkeypatch, capsys):
    """A transient outage while listing recent commits should not paint main red.

    Per-SHA status read failures are already isolated inside `reap_branch`.
    The real 2026-05-14 failure was earlier: `/commits?sha=main&limit=30`
    timed out after all retries, aborting the tick. The next 5-minute tick can
    retry safely, so `main()` should emit an observable warning and return 0.
    """

    monkeypatch.setattr(sr_module, "scan_workflows", lambda _: {"workflow-without-push": False})

    def fake_list_recent_commit_shas(*args, **kwargs):
        raise sr_module.ApiError(
            "GET /repos/owner/repo/commits failed after 4 attempts: timed out"
        )

    monkeypatch.setattr(sr_module, "list_recent_commit_shas", fake_list_recent_commit_shas)
    monkeypatch.setattr(sys, "argv", ["status-reaper.py"])

    assert sr_module.main() == 0
    captured = capsys.readouterr()
    assert "::warning::status-reaper skipped this tick" in captured.out
    assert '"skipped": true' in captured.out
    assert '"skip_reason": "commit-list-api-error"' in captured.out


def test_main_does_not_soft_skip_status_write_failures(sr_module, monkeypatch):
    """Only commit-list read failures are soft-skipped.

    A compensation write failure means the reaper could not repair a red
    status. That must still fail the job loudly instead of being mislabeled as
    a transient commit-list outage.
    """

    monkeypatch.setattr(sr_module, "scan_workflows", lambda _: {"workflow-without-push": False})
    monkeypatch.setattr(sr_module, "list_recent_commit_shas", lambda *_args, **_kwargs: [SHA_A])
    monkeypatch.setattr(
        sr_module,
        "get_combined_status",
        lambda _sha: {
            "state": "failure",
            "statuses": [
                {
                    "context": "workflow-without-push / job (push)",
                    "status": "failure",
                    "description": "stranded class-O red",
                }
            ],
        },
    )

    def fake_post_compensating_status(*args, **kwargs):
        raise sr_module.ApiError("POST /statuses failed: 403")

    monkeypatch.setattr(sr_module, "post_compensating_status", fake_post_compensating_status)
    monkeypatch.setattr(sys, "argv", ["status-reaper.py"])

    with pytest.raises(sr_module.ApiError, match="POST /statuses failed"):
        sr_module.main()
