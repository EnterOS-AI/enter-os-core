"""Tests for `.gitea/scripts/lint_required_context_exists_in_bp.py` — Tier 2g lint.

Structural enforcement of internal#350 Tier 2g: when a PR adds a NEW
commit-status emission (a workflow's `name:` + a new job-key/name pair
that didn't exist on the base side), the PR must EITHER:

  (a) Include a `# bp-required: yes` directive comment on the workflow
      AND the new context must already be in
      `branch_protections/<branch>.status_check_contexts`, OR

  (b) Include a `# bp-required: pending #NNN` directive (acknowledged
      asymmetry with a tracking issue), OR

  (c) Include a `# bp-exempt: <reason>` directive (informational job,
      not intended to be a required gate).

Default (no directive on a new emitter) = FAIL.

The class this prevents
-----------------------
PR#656 added `CI / all-required (pull_request)` as a sentinel context
that workflows emit, but BP did NOT list it — so when `platform-build`
failed, `all-required` failed, but BP let the PR merge anyway. Cascade
to mc#664. With Tier 2g, PR#656 would have been blocked until either
the BP PATCH ran alongside OR the author marked the emission with a
`bp-required: pending #NNN` directive.

Test classes (per `feedback_branch_count_before_approving`):

  - test_no_new_emissions_skips                   — diff doesn't add any
    new emitter; pass.
  - test_new_emission_with_bp_required_yes_in_bp  — directive set AND
    BP lists the context; pass.
  - test_new_emission_with_bp_required_yes_not_in_bp — directive set
    BUT BP doesn't list; fail.
  - test_new_emission_with_bp_required_pending    — `# bp-required:
    pending #800` directive references an open tracker; pass.
  - test_new_emission_with_bp_exempt              — `# bp-exempt:
    informational` directive; pass.
  - test_new_emission_no_directive_fails          — no directive on a
    new emission; fail with the 3-option fix-hint.
  - test_modified_workflow_with_new_job_is_new    — pre-existing
    workflow gains a new job with a new name → counted as new
    emission. Apply rule.
  - test_modified_workflow_job_renamed_is_new     — same workflow,
    same job-key, but job `name:` changed → counted as new emission
    (the OLD context name disappears; the NEW one needs validation).
  - test_unrelated_workflow_edit_is_not_new       — edit a comment in
    an existing emitter; no new context introduced; pass.
  - test_api_403_fails_closed                     — BP read 401/403 auth
    failure → FAIL CLOSED (exit 2)
  - test_api_transient_fails_closed               — transient → exit 2
  - test_api_404_skips_gracefully                 — authenticated 404 → exit 0
    with stderr ::error::.
  - test_directive_must_be_in_workflow_yml        — directive in PR
    body alone is NOT sufficient; the comment must live in the
    workflow file so future scheduled Tier 2f runs can see it.

Run:
    python3 -m pytest tests/test_lint_required_context_exists_in_bp.py -v
"""
from __future__ import annotations

import importlib.util
import os
import subprocess
from pathlib import Path

import pytest


SCRIPT_PATH = (
    Path(__file__).resolve().parent.parent
    / ".gitea"
    / "scripts"
    / "lint_required_context_exists_in_bp.py"
)


def _import_lint():
    spec = importlib.util.spec_from_file_location(
        f"lint_required_ctx_in_bp_{os.getpid()}", SCRIPT_PATH
    )
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


# Sample workflows used across multiple tests.
WF_CI_BASE = """name: CI
on:
  pull_request:
    branches: [main]
jobs:
  all-required:
    runs-on: x
    steps:
      - run: echo hi
"""

# CI with a new job added.
WF_CI_NEW_JOB = """name: CI
on:
  pull_request:
    branches: [main]
jobs:
  all-required:
    runs-on: x
    steps:
      - run: echo hi
  brand-new:
    runs-on: x
    steps:
      - run: echo new
"""

WF_CI_NEW_JOB_BP_YES = """name: CI
on:
  pull_request:
    branches: [main]
jobs:
  all-required:
    runs-on: x
    steps:
      - run: echo hi
  # bp-required: yes
  brand-new:
    runs-on: x
    steps:
      - run: echo new
"""

WF_CI_NEW_JOB_BP_PENDING = """name: CI
on:
  pull_request:
    branches: [main]
jobs:
  all-required:
    runs-on: x
    steps:
      - run: echo hi
  # bp-required: pending #800
  brand-new:
    runs-on: x
    steps:
      - run: echo new
"""

WF_CI_NEW_JOB_BP_EXEMPT = """name: CI
on:
  pull_request:
    branches: [main]
jobs:
  all-required:
    runs-on: x
    steps:
      - run: echo hi
  # bp-exempt: informational sticker, not a gate
  brand-new:
    runs-on: x
    steps:
      - run: echo new
"""

# Same WF, job rename only (CI/all-required → CI/sentinel).
WF_CI_JOB_RENAMED = """name: CI
on:
  pull_request:
    branches: [main]
jobs:
  all-required:
    runs-on: x
    name: sentinel
    steps:
      - run: echo hi
"""

# Comment-only edit — should NOT count as new emission.
WF_CI_COMMENT_ONLY = """# a fresh comment line
name: CI
on:
  pull_request:
    branches: [main]
jobs:
  all-required:
    runs-on: x
    steps:
      - run: echo hi
"""


def _stub_git_and_api(
    monkeypatch,
    lint_mod,
    base_files: dict[str, str | None],
    head_files: dict[str, str | None],
    bp_response,
):
    """Stub `subprocess.run` for git, and `lint_mod.api` for HTTP."""

    def fake_run(cmd, *args, **kwargs):
        if not isinstance(cmd, list):
            raise AssertionError(f"unexpected cmd: {cmd!r}")
        if cmd[:2] == ["git", "show"] and ":" in cmd[2]:
            sha, path = cmd[2].split(":", 1)
            side = base_files if "base" in sha else head_files
            content = side.get(path)
            if content is None:
                return subprocess.CompletedProcess(cmd, 128, "", "fatal: path not in tree")
            return subprocess.CompletedProcess(cmd, 0, content, "")
        if cmd[:2] == ["git", "diff"]:
            # Names of files that changed (any side has differing contents
            # from the other, or only appears on one side).
            all_paths = set(base_files) | set(head_files)
            changed = sorted(p for p in all_paths if base_files.get(p) != head_files.get(p))
            return subprocess.CompletedProcess(cmd, 0, "\n".join(changed) + "\n", "")
        raise AssertionError(f"unexpected cmd: {cmd!r}")

    monkeypatch.setattr(subprocess, "run", fake_run)

    def fake_api(method, path, *, body=None, query=None):
        if "branch_protections" in path:
            return bp_response
        return ("ok", {})

    monkeypatch.setattr(lint_mod, "api", fake_api)


@pytest.fixture()
def env(monkeypatch):
    monkeypatch.setenv("BASE_SHA", "base-x")
    monkeypatch.setenv("HEAD_SHA", "head-x")
    monkeypatch.setenv("GITEA_TOKEN", "stub")
    monkeypatch.setenv("GITEA_HOST", "git.example.test")
    monkeypatch.setenv("REPO", "owner/molecule-core")
    monkeypatch.setenv("BRANCH", "main")
    monkeypatch.setenv("WORKFLOWS_DIR", ".gitea/workflows")
    return monkeypatch


# ---------------------------------------------------------------------------
# No new emissions — pass.
# ---------------------------------------------------------------------------
def test_no_new_emissions_skips(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        bp_response=("ok", {"status_check_contexts": []}),
    )
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# New emission + bp-required: yes + in BP → pass.
# ---------------------------------------------------------------------------
def test_new_emission_with_bp_required_yes_in_bp(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_NEW_JOB_BP_YES},
        bp_response=(
            "ok",
            {"status_check_contexts": ["CI / brand-new (pull_request)"]},
        ),
    )
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# bp-required: yes but NOT in BP → fail.
# ---------------------------------------------------------------------------
def test_new_emission_with_bp_required_yes_not_in_bp(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_NEW_JOB_BP_YES},
        bp_response=("ok", {"status_check_contexts": []}),
    )
    rc = m.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "brand-new" in out


# ---------------------------------------------------------------------------
# bp-required: pending #NNN → pass.
# ---------------------------------------------------------------------------
def test_new_emission_with_bp_required_pending(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_NEW_JOB_BP_PENDING},
        bp_response=("ok", {"status_check_contexts": []}),
    )
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# bp-exempt → pass.
# ---------------------------------------------------------------------------
def test_new_emission_with_bp_exempt(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_NEW_JOB_BP_EXEMPT},
        bp_response=("ok", {"status_check_contexts": []}),
    )
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# New emission, no directive → fail with 3-option fix hint.
# ---------------------------------------------------------------------------
def test_new_emission_no_directive_fails(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_NEW_JOB},
        bp_response=("ok", {"status_check_contexts": []}),
    )
    rc = m.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "brand-new" in out
    assert "bp-required" in out
    assert "bp-exempt" in out


# ---------------------------------------------------------------------------
# Pre-existing workflow gains a new job → counted as new emission.
# ---------------------------------------------------------------------------
def test_modified_workflow_with_new_job_is_new(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_NEW_JOB},
        bp_response=("ok", {"status_check_contexts": []}),
    )
    rc = m.run()
    # No directive → fail
    assert rc == 1


# ---------------------------------------------------------------------------
# Same workflow, same job-key, but job `name:` changed → new context.
# ---------------------------------------------------------------------------
def test_modified_workflow_job_renamed_is_new(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_JOB_RENAMED},
        bp_response=("ok", {"status_check_contexts": []}),
    )
    rc = m.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "sentinel" in out


# ---------------------------------------------------------------------------
# Comment-only edit → no new emission.
# ---------------------------------------------------------------------------
def test_unrelated_workflow_edit_is_not_new(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_COMMENT_ONLY},
        bp_response=("ok", {"status_check_contexts": []}),
    )
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# BP API 401/403 = AUTH FAILURE → FAIL CLOSED (exit 2). A new emission can't
# be verified against BP if the token can't read BP — must not green.
# ---------------------------------------------------------------------------
def test_api_403_fails_closed(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_NEW_JOB},
        bp_response=("forbidden", None),
    )
    rc = m.run()
    assert rc == 2
    err = capsys.readouterr().err
    assert "403" in err or "scope" in err.lower() or "token" in err.lower()


# ---------------------------------------------------------------------------
# BP API transient/unexpected error → FAIL CLOSED (exit 2).
# ---------------------------------------------------------------------------
def test_api_transient_fails_closed(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_NEW_JOB},
        bp_response=("error", None),
    )
    rc = m.run()
    assert rc == 2


# ---------------------------------------------------------------------------
# BP API authenticated 404 (branch genuinely unprotected) → tolerated
# graceful skip (exit 0 with ::warning::), NOT a fail-open.
# ---------------------------------------------------------------------------
def test_api_404_skips_gracefully(env, monkeypatch, capsys):
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_NEW_JOB},
        bp_response=("not_found", None),
    )
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# Directive must be in the workflow YML, not PR body.
# ---------------------------------------------------------------------------
def test_directive_must_be_in_workflow_yml(env, monkeypatch, capsys):
    monkeypatch = env
    monkeypatch.setenv("PR_BODY", "bp-required: yes — see comment above")
    m = _import_lint()
    _stub_git_and_api(
        monkeypatch,
        m,
        base_files={".gitea/workflows/ci.yml": WF_CI_BASE},
        head_files={".gitea/workflows/ci.yml": WF_CI_NEW_JOB},
        bp_response=("ok", {"status_check_contexts": []}),
    )
    rc = m.run()
    # Even though PR body claims, the workflow itself lacks the directive.
    assert rc == 1
