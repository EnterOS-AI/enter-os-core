"""Tests for `.gitea/scripts/lint-required-no-paths.py`.

Structural enforcement of `feedback_path_filtered_workflow_cant_be_required`:
no workflow whose status-check context is in `branch_protections/main`
`status_check_contexts` may use `paths:` or `paths-ignore:` filters in its
`on:` block. A path-filtered workflow silently does not fire on a PR whose
diff doesn't touch the filter — Gitea treats that as `pending` forever,
not `skipped`-as-`success`, so the gate degrades to an indefinite block.
Worse, a docs-only PR could never satisfy a required check whose filter
excludes docs paths, and the protected branch becomes unreachable.

Five test classes:
  - test_no_required_workflows_succeeds — empty status_check_contexts → exit 0
  - test_required_workflow_no_paths_passes — required workflow with no
    paths filter → exit 0
  - test_required_workflow_with_paths_filter_fails — required workflow
    with `paths: ['**.go']` → exit 1, error names workflow
  - test_required_workflow_with_paths_ignore_fails — same shape for
    `paths-ignore`
  - test_unknown_required_context_warns_not_fails — context whose
    workflow file is missing → warn, do NOT fail (graceful — could be a
    cross-repo context name or a workflow renamed mid-PR; the lint is for
    paths-filter detection, not orphaned-context detection — that's
    ci-required-drift's job)

Also covers the workflow-name → file-path mapping (parses the
`<workflow_name> / <job_name> (<event>)` context format) and the
multi-event `on:` block edge cases (paths under `on.push` vs `on.pull_request`
vs top-level `on.paths`).

Run:
    python3 -m pytest tests/test_lint_required_no_paths.py -v

Dependencies: stdlib + PyYAML (already required by the script itself).
No network. No live Gitea calls — `api()` is stubbed.
"""
from __future__ import annotations

import importlib.util
import os
import sys
from pathlib import Path
from unittest import mock

import pytest


# --------------------------------------------------------------------------
# Module import fixture — mirror of tests/test_ci_required_drift.py shape
# --------------------------------------------------------------------------
SCRIPT_PATH = (
    Path(__file__).resolve().parent.parent
    / ".gitea"
    / "scripts"
    / "lint-required-no-paths.py"
)


@pytest.fixture()
def lint_module(tmp_path, monkeypatch):
    """Import the script as a module with a clean env per test.

    Tests need a per-test workflows directory under tmp_path; the module
    reads `WORKFLOWS_DIR` from env. Fresh import per test means tests
    cannot leak global state into each other.
    """
    env = {
        "GITEA_TOKEN": "test-token",
        "GITEA_HOST": "git.example.test",
        "REPO": "owner/repo",
        "BRANCH": "main",
        "WORKFLOWS_DIR": str(tmp_path / ".gitea" / "workflows"),
    }
    (tmp_path / ".gitea" / "workflows").mkdir(parents=True)
    monkeypatch.setattr(os, "environ", {**os.environ, **env})
    spec = importlib.util.spec_from_file_location(
        f"lint_required_no_paths_{id(tmp_path)}", SCRIPT_PATH
    )
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    # Force-set the globals from env (they were captured at import time;
    # we mutate them so the per-test tmp_path is what the script reads).
    m.GITEA_TOKEN = env["GITEA_TOKEN"]
    m.GITEA_HOST = env["GITEA_HOST"]
    m.REPO = env["REPO"]
    m.BRANCH = env["BRANCH"]
    m.WORKFLOWS_DIR = env["WORKFLOWS_DIR"]
    m.OWNER, m.NAME = "owner", "repo"
    m.API = f"https://{env['GITEA_HOST']}/api/v1"
    return m


def _write_workflow(workflows_dir: str, filename: str, content: str) -> Path:
    p = Path(workflows_dir) / filename
    p.write_text(content, encoding="utf-8")
    return p


def _make_stub_api(responses: dict):
    """Build a fake `api()` callable.

    `responses` maps (method, path) tuples to either:
      - (status_int, body) → returned as-is
      - Exception instance → raised
    Calls are recorded in `.calls` for later assertion.
    """
    class StubApi:
        def __init__(self):
            self.calls: list[tuple] = []

        def __call__(self, method, path, *, body=None, query=None, expect_json=True):
            self.calls.append((method, path, body, query))
            key = (method, path)
            if key not in responses:
                raise AssertionError(
                    f"unexpected api call: {method} {path} (no stub registered)"
                )
            r = responses[key]
            if isinstance(r, Exception):
                raise r
            return r

    return StubApi()


# --------------------------------------------------------------------------
# context → (workflow_name, job_name, event) parser
# --------------------------------------------------------------------------
def test_parse_context_standard_shape(lint_module):
    """`<workflow_name> / <job_name> (<event>)` round-trips cleanly."""
    parsed = lint_module.parse_context(
        "Secret scan / Scan diff for credential-shaped strings (pull_request)"
    )
    assert parsed == (
        "Secret scan",
        "Scan diff for credential-shaped strings",
        "pull_request",
    )


def test_parse_context_with_slash_in_job_name(lint_module):
    """Job names CAN contain ' / ' literally in Gitea; the parser must
    split on the LAST ' / ' before the trailing ' (event)' suffix."""
    parsed = lint_module.parse_context(
        "ci / setup / install-deps (pull_request)"
    )
    # Workflow = first segment; job = everything between first ' / ' and
    # the trailing ' (event)'. Pragmatic split: the workflow name is
    # `name:` from the YAML, so multi-slash workflow names are unlikely;
    # treat the first ' / ' as the divider.
    assert parsed[0] == "ci"
    assert parsed[1] == "setup / install-deps"
    assert parsed[2] == "pull_request"


def test_parse_context_unparseable_returns_none(lint_module):
    """Malformed context string → None so the caller can warn-and-skip."""
    assert lint_module.parse_context("garbage no event marker") is None
    assert lint_module.parse_context("") is None


# --------------------------------------------------------------------------
# workflow-name → file resolution
# --------------------------------------------------------------------------
def test_resolve_workflow_file_matches_name_attr(lint_module):
    """Resolution scans workflows/*.yml for a `name:` matching the
    context's workflow_name. Filename is NOT the source of truth — the
    `name:` attribute is, because Gitea's context format uses
    `name:` (not the filename).
    """
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "some-file.yml",
        "name: Secret scan\non:\n  pull_request:\n    types: [opened]\njobs:\n  scan:\n    runs-on: ubuntu-latest\n",
    )
    p = lint_module.resolve_workflow_file("Secret scan")
    assert p is not None
    assert p.name == "some-file.yml"


def test_resolve_workflow_file_returns_none_when_missing(lint_module):
    """No matching `name:` found → None."""
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "other.yml",
        "name: Other\non:\n  pull_request: {}\njobs:\n  x:\n    runs-on: ubuntu-latest\n",
    )
    assert lint_module.resolve_workflow_file("Secret scan") is None


# --------------------------------------------------------------------------
# paths-filter detection
# --------------------------------------------------------------------------
def test_workflow_has_no_paths_filter_clean(lint_module):
    """No paths/paths-ignore → returns empty list (no findings)."""
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "clean.yml",
        "name: Clean\n"
        "on:\n"
        "  pull_request:\n"
        "    types: [opened, synchronize]\n"
        "jobs:\n"
        "  x:\n"
        "    runs-on: ubuntu-latest\n",
    )
    findings = lint_module.detect_paths_filters(
        Path(lint_module.WORKFLOWS_DIR) / "clean.yml"
    )
    assert findings == []


def test_workflow_with_pull_request_paths_filter_detected(lint_module):
    """`on.pull_request.paths` → ONE finding naming pull_request + paths."""
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "bad.yml",
        "name: Bad\n"
        "on:\n"
        "  pull_request:\n"
        "    paths: ['**.go', 'workspace/**']\n"
        "jobs:\n"
        "  x:\n"
        "    runs-on: ubuntu-latest\n",
    )
    findings = lint_module.detect_paths_filters(
        Path(lint_module.WORKFLOWS_DIR) / "bad.yml"
    )
    assert len(findings) == 1
    f = findings[0]
    assert "pull_request" in f
    assert "paths" in f
    assert "**.go" in f or "workspace/**" in f  # filter content surfaced


def test_workflow_with_paths_ignore_filter_detected(lint_module):
    """`on.pull_request.paths-ignore` → finding naming paths-ignore.

    paths-ignore is the SAME class of defect: a docs-only PR (that
    matches the ignore pattern) silently won't fire the workflow, and the
    required context stays pending.
    """
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "bad.yml",
        "name: Bad\n"
        "on:\n"
        "  pull_request:\n"
        "    paths-ignore: ['docs/**']\n"
        "jobs:\n"
        "  x:\n"
        "    runs-on: ubuntu-latest\n",
    )
    findings = lint_module.detect_paths_filters(
        Path(lint_module.WORKFLOWS_DIR) / "bad.yml"
    )
    assert len(findings) == 1
    assert "paths-ignore" in findings[0]


def test_workflow_with_push_paths_filter_detected(lint_module):
    """`on.push.paths` → also a finding. A required check on a PR is
    typically `(pull_request)`-event, but a workflow may ALSO have a
    push trigger; a paths filter on the push side affects the same
    workflow file, and a future PR might add `paths:` to the wrong
    event-branch and trip the gate. Surface all paths-filter sites.
    """
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "bad.yml",
        "name: Bad\n"
        "on:\n"
        "  pull_request:\n"
        "    types: [opened]\n"
        "  push:\n"
        "    branches: [main]\n"
        "    paths: ['**.py']\n"
        "jobs:\n"
        "  x:\n"
        "    runs-on: ubuntu-latest\n",
    )
    findings = lint_module.detect_paths_filters(
        Path(lint_module.WORKFLOWS_DIR) / "bad.yml"
    )
    assert len(findings) == 1
    assert "push" in findings[0]
    assert "paths" in findings[0]


def test_workflow_with_both_paths_and_paths_ignore_two_findings(lint_module):
    """Both filters under one event → two findings (one per offending
    key). Test ensures the detector doesn't short-circuit after the
    first."""
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "bad.yml",
        "name: Bad\n"
        "on:\n"
        "  pull_request:\n"
        "    paths: ['**.go']\n"
        "    paths-ignore: ['docs/**']\n"
        "jobs:\n"
        "  x:\n"
        "    runs-on: ubuntu-latest\n",
    )
    findings = lint_module.detect_paths_filters(
        Path(lint_module.WORKFLOWS_DIR) / "bad.yml"
    )
    assert len(findings) == 2


def test_workflow_with_on_shorthand_string_passes(lint_module):
    """`on: pull_request` (string shorthand, no sub-keys) cannot have a
    paths filter — detector treats it as clean."""
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "clean.yml",
        "name: Clean\non: pull_request\njobs:\n  x:\n    runs-on: ubuntu-latest\n",
    )
    findings = lint_module.detect_paths_filters(
        Path(lint_module.WORKFLOWS_DIR) / "clean.yml"
    )
    assert findings == []


def test_workflow_with_on_list_shorthand_passes(lint_module):
    """`on: [pull_request, push]` (list shorthand) cannot carry filters
    either — clean."""
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "clean.yml",
        "name: Clean\non: [pull_request, push]\njobs:\n  x:\n    runs-on: ubuntu-latest\n",
    )
    findings = lint_module.detect_paths_filters(
        Path(lint_module.WORKFLOWS_DIR) / "clean.yml"
    )
    assert findings == []


def test_workflow_on_event_with_null_value_passes(lint_module):
    """`pull_request:` with no body (None / null) is event-shorthand —
    no filter possible."""
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "clean.yml",
        "name: Clean\non:\n  pull_request:\n  push:\n    branches: [main]\njobs:\n  x:\n    runs-on: ubuntu-latest\n",
    )
    findings = lint_module.detect_paths_filters(
        Path(lint_module.WORKFLOWS_DIR) / "clean.yml"
    )
    assert findings == []


# --------------------------------------------------------------------------
# End-to-end lint (main) — required-checks fan-out
# --------------------------------------------------------------------------
def test_no_required_workflows_succeeds(lint_module, monkeypatch, capsys):
    """Empty status_check_contexts → exit 0, no findings reported."""
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {"status_check_contexts": []},
        ),
    })
    monkeypatch.setattr(lint_module, "api", stub)
    rc = lint_module.run()
    assert rc == 0
    out = capsys.readouterr().out
    assert "no required contexts" in out.lower() or "0 required" in out.lower()


def test_required_workflow_no_paths_passes(lint_module, monkeypatch, capsys):
    """A required workflow with no paths filter → exit 0."""
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "secret-scan.yml",
        "name: Secret scan\non:\n  pull_request:\n    types: [opened]\njobs:\n  scan:\n    runs-on: ubuntu-latest\n",
    )
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {
                "status_check_contexts": [
                    "Secret scan / scan (pull_request)",
                ]
            },
        ),
    })
    monkeypatch.setattr(lint_module, "api", stub)
    rc = lint_module.run()
    assert rc == 0


def test_required_workflow_with_paths_filter_fails(
    lint_module, monkeypatch, capsys
):
    """A required workflow that has `paths:` filter → exit 1 + error
    names the offending workflow + the filter."""
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "secret-scan.yml",
        "name: Secret scan\n"
        "on:\n"
        "  pull_request:\n"
        "    paths: ['**.go']\n"
        "jobs:\n"
        "  scan:\n"
        "    runs-on: ubuntu-latest\n",
    )
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {"status_check_contexts": ["Secret scan / scan (pull_request)"]},
        ),
    })
    monkeypatch.setattr(lint_module, "api", stub)
    rc = lint_module.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "secret-scan.yml" in out
    assert "Secret scan" in out
    assert "paths" in out
    assert "::error::" in out


def test_required_workflow_with_paths_ignore_fails(
    lint_module, monkeypatch, capsys
):
    """Same defect class for `paths-ignore` — exit 1, named."""
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "sop-tier-check.yml",
        "name: sop-tier-check\n"
        "on:\n"
        "  pull_request_target:\n"
        "    paths-ignore: ['docs/**']\n"
        "jobs:\n"
        "  tier-check:\n"
        "    runs-on: ubuntu-latest\n",
    )
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {
                "status_check_contexts": [
                    "sop-tier-check / tier-check (pull_request_target)"
                ]
            },
        ),
    })
    monkeypatch.setattr(lint_module, "api", stub)
    rc = lint_module.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "sop-tier-check.yml" in out
    assert "paths-ignore" in out


def test_unknown_required_context_warns_not_fails(
    lint_module, monkeypatch, capsys
):
    """Required context with no matching workflow file → warn, don't
    fail. This is gracefully bounded — the lint's mandate is paths-filter
    detection, not orphaned-context detection (`ci-required-drift` is the
    canonical detector for that).
    """
    # No workflows written → all required contexts will be unresolved.
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {
                "status_check_contexts": [
                    "Mystery / job (pull_request)",
                ]
            },
        ),
    })
    monkeypatch.setattr(lint_module, "api", stub)
    rc = lint_module.run()
    assert rc == 0  # warn-not-fail
    out = capsys.readouterr().out
    assert "::warning::" in out
    assert "Mystery" in out


def test_multi_required_one_bad_one_good_fails(
    lint_module, monkeypatch, capsys
):
    """Two required contexts; one workflow is bad. Lint still fails
    (one defect is enough) and the error names ONLY the bad workflow."""
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "good.yml",
        "name: Good\non:\n  pull_request:\n    types: [opened]\njobs:\n  x:\n    runs-on: ubuntu-latest\n",
    )
    _write_workflow(
        lint_module.WORKFLOWS_DIR,
        "bad.yml",
        "name: Bad\n"
        "on:\n"
        "  pull_request:\n"
        "    paths: ['src/**']\n"
        "jobs:\n  x:\n    runs-on: ubuntu-latest\n",
    )
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {
                "status_check_contexts": [
                    "Good / x (pull_request)",
                    "Bad / x (pull_request)",
                ]
            },
        ),
    })
    monkeypatch.setattr(lint_module, "api", stub)
    rc = lint_module.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "bad.yml" in out
    # `good.yml` should NOT show up in the error block — only the bad one.
    # (It may appear as a "checked" notice; assert it's not flagged as bad.)
    assert "::error::" in out
    error_lines = [ln for ln in out.split("\n") if ln.startswith("::error::") or "paths" in ln.lower() and "good" in ln.lower()]
    # The good workflow must not appear under an ::error:: line referencing paths.
    for ln in error_lines:
        if ln.startswith("::error::"):
            # The error line itself shouldn't name good.yml as offending.
            assert "good.yml" not in ln


def test_protection_403_treated_as_skip(lint_module, monkeypatch, capsys):
    """If the token can't read branch_protections (HTTP 403), exit 0
    with a clear ::error::-but-non-fatal note. Same scope-fallback shape
    as ci-required-drift.py per the precedent.

    Rationale: if the lint workflow itself can't read protection, the PR
    can't make THIS state worse (a paths-filter PR was already addable
    without the lint). Better to surface a token-scope problem loudly
    than to red-X every PR until the token is fixed.
    """
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            lint_module.ApiError(
                "GET /repos/owner/repo/branch_protections/main → HTTP 403: forbidden"
            )
        ),
    })
    monkeypatch.setattr(lint_module, "api", stub)
    rc = lint_module.run()
    assert rc == 0
    err = capsys.readouterr().err
    assert "::error::" in err
    assert "403" in err
