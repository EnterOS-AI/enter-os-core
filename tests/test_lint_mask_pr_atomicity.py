"""Tests for `.gitea/scripts/lint_mask_pr_atomicity.py` — Tier 2d lint.

Structural enforcement of internal#350 Tier 2d: a PR that touches
`.gitea/workflows/ci.yml` and modifies `continue-on-error` OR the
`all-required` sentinel's `needs:` block must EITHER:

  - Touch both atomically in the same PR (preferred), OR
  - Cross-link to the paired PR via `Paired: #NNN` in body OR a commit
    message.

The class this lint exists to prevent: PR#665 (interim
continue-on-error: true on platform-build) + PR#668 (sentinel-exempt)
were designed-as-a-pair but merged solo — #665 landed at 04:47Z, #668
still open at 05:07Z when the watchdog fired. ~20 min of main red.

Test classes (per `feedback_branch_count_before_approving`, every
prod branch enumerated):

  - test_diff_touches_neither_passes              — diff is in ci.yml
    but neither continue-on-error nor all-required.needs is touched.
    PR is exempt. Exit 0.
  - test_diff_touches_both_atomically_passes      — both touched in
    the same PR. Atomic. Exit 0.
  - test_diff_touches_coe_only_no_pair_fails      — continue-on-error
    flipped without sentinel-needs change AND no `Paired: #NNN`
    reference anywhere. Exit 1.
  - test_diff_touches_needs_only_no_pair_fails    — sentinel `needs:`
    changed without `continue-on-error` change AND no pair reference.
    Exit 1.
  - test_diff_touches_coe_only_pair_in_body       — coe changed, no
    needs change, body has `Paired: #668`. Exit 0.
  - test_diff_touches_needs_only_pair_in_commit   — needs changed, no
    coe change, commit message includes `Paired: #665`. Exit 0.
  - test_paired_reference_must_be_numeric         — `Paired: #abc` or
    `Paired: NNNN` (missing `#`) doesn't satisfy the rule. Exit 1.
  - test_ci_yml_unchanged_skips                   — no ci.yml in the
    diff at all (defensive — workflow paths-filter already prevents,
    but the lint should not crash). Exit 0.

The lint receives base SHA + head SHA via env (set by the workflow
from the pull_request payload) and uses `git show` to read both
sides without a separate clone. Tests stub `subprocess.run` to drive
the diff content; the actual git is never invoked.

Run:
    python3 -m pytest tests/test_lint_mask_pr_atomicity.py -v

Dependencies: stdlib + PyYAML (the script reads ci.yml via PyYAML AST
per `feedback_behavior_based_ast_gates`). No network. No live git.
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
    / "lint_mask_pr_atomicity.py"
)


# Minimal ci.yml fixture — only the bits the lint actually parses
# (a job with continue-on-error + the all-required aggregator).
CI_YML_BASE = """
name: CI
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]
jobs:
  platform-build:
    runs-on: ubuntu-latest
    continue-on-error: false
    steps:
      - run: echo build
  canvas-build:
    runs-on: ubuntu-latest
    continue-on-error: false
    steps:
      - run: echo build
  all-required:
    runs-on: ubuntu-latest
    needs:
      - platform-build
      - canvas-build
    if: always()
    steps:
      - run: echo agg
"""

# Same as base but with continue-on-error flipped on platform-build.
CI_YML_COE_FLIPPED = CI_YML_BASE.replace(
    "  platform-build:\n    runs-on: ubuntu-latest\n    continue-on-error: false",
    "  platform-build:\n    runs-on: ubuntu-latest\n    continue-on-error: true",
)

# Same as base but with canvas-build dropped from all-required.needs.
CI_YML_NEEDS_CHANGED = CI_YML_BASE.replace(
    "    needs:\n      - platform-build\n      - canvas-build",
    "    needs:\n      - platform-build",
)

# Both changed at once.
CI_YML_BOTH = CI_YML_COE_FLIPPED.replace(
    "    needs:\n      - platform-build\n      - canvas-build",
    "    needs:\n      - platform-build",
)


def _import_lint(monkeypatch):
    """Import the lint module under a fresh name per test."""
    spec = importlib.util.spec_from_file_location(
        f"lint_mask_pr_atomicity_{os.getpid()}_{id(monkeypatch)}",
        SCRIPT_PATH,
    )
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


def _stub_git(base_yml: str | None, head_yml: str | None, commits: list[str]):
    """Build a fake `subprocess.run` that emulates git show + log.

    base_yml / head_yml: contents the lint sees at base/head SHA.
        Pass `None` to simulate "path didn't exist on that side" (git
        show returns exit code 128 — file-not-in-tree).
    commits: list of commit messages on the PR (head's ancestry up to
        the base merge-base). The lint runs
        `git log --format=%B base..head` to find Paired: refs.
    """

    def fake_run(cmd, *args, **kwargs):
        if not isinstance(cmd, list):
            raise AssertionError(f"unexpected non-list cmd: {cmd!r}")
        # `git show <sha>:<path>`
        if cmd[:2] == ["git", "show"] and len(cmd) >= 3 and ":" in cmd[2]:
            sha, path = cmd[2].split(":", 1)
            if "base" in sha or "BASE" in sha:
                content = base_yml
            else:
                content = head_yml
            if content is None:
                return subprocess.CompletedProcess(
                    cmd, returncode=128, stdout="", stderr="fatal: path not in tree"
                )
            return subprocess.CompletedProcess(
                cmd, returncode=0, stdout=content, stderr=""
            )
        # `git log --format=%B base..head -- .`
        if cmd[:2] == ["git", "log"]:
            body = "\n\n--commit-boundary--\n\n".join(commits)
            return subprocess.CompletedProcess(
                cmd, returncode=0, stdout=body, stderr=""
            )
        # `git diff --name-only base..head`
        if cmd[:2] == ["git", "diff"]:
            # If either side had ci.yml, it's in the diff; else not.
            paths = []
            if (base_yml or "") != (head_yml or ""):
                paths.append(".gitea/workflows/ci.yml")
            return subprocess.CompletedProcess(
                cmd, returncode=0, stdout="\n".join(paths) + "\n", stderr=""
            )
        raise AssertionError(f"unexpected git invocation: {cmd!r}")

    return fake_run


@pytest.fixture()
def env(monkeypatch):
    monkeypatch.setenv("BASE_SHA", "base-sha-1")
    monkeypatch.setenv("HEAD_SHA", "head-sha-1")
    monkeypatch.setenv("PR_BODY", "")
    monkeypatch.setenv("CI_WORKFLOW_PATH", ".gitea/workflows/ci.yml")
    monkeypatch.setenv("SENTINEL_JOB_KEY", "all-required")
    return monkeypatch


# ---------------------------------------------------------------------------
# Diff in ci.yml but neither rule predicate triggered → pass
# ---------------------------------------------------------------------------
def test_diff_touches_neither_passes(env, monkeypatch, capsys):
    # Add a comment-only change (no coe flip, no needs change).
    base = CI_YML_BASE
    head = "# a harmless comment\n" + CI_YML_BASE
    monkeypatch.setattr(
        subprocess, "run", _stub_git(base, head, ["chore: comment"])
    )
    m = _import_lint(monkeypatch)
    rc = m.run()
    assert rc == 0
    out = capsys.readouterr().out
    assert "no atomicity risk" in out.lower() or "ok" in out.lower()


# ---------------------------------------------------------------------------
# Diff touches BOTH coe and sentinel.needs in the same PR → atomic, pass
# ---------------------------------------------------------------------------
def test_diff_touches_both_atomically_passes(env, monkeypatch, capsys):
    monkeypatch.setattr(
        subprocess,
        "run",
        _stub_git(CI_YML_BASE, CI_YML_BOTH, ["fix(ci): atomic flip"]),
    )
    m = _import_lint(monkeypatch)
    rc = m.run()
    assert rc == 0
    out = capsys.readouterr().out
    assert "atomic" in out.lower()


# ---------------------------------------------------------------------------
# Diff touches ONLY continue-on-error, no pair reference → fail
# ---------------------------------------------------------------------------
def test_diff_touches_coe_only_no_pair_fails(env, monkeypatch, capsys):
    monkeypatch.setattr(
        subprocess,
        "run",
        _stub_git(
            CI_YML_BASE,
            CI_YML_COE_FLIPPED,
            ["fix(ci): flip coe on platform-build"],
        ),
    )
    m = _import_lint(monkeypatch)
    rc = m.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "paired" in out.lower() or "atomicity" in out.lower()
    # Actionable failure: must name what is missing.
    assert "continue-on-error" in out.lower()


# ---------------------------------------------------------------------------
# Diff touches ONLY sentinel.needs, no pair reference → fail
# ---------------------------------------------------------------------------
def test_diff_touches_needs_only_no_pair_fails(env, monkeypatch, capsys):
    monkeypatch.setattr(
        subprocess,
        "run",
        _stub_git(
            CI_YML_BASE,
            CI_YML_NEEDS_CHANGED,
            ["fix(ci): drop canvas-build from sentinel"],
        ),
    )
    m = _import_lint(monkeypatch)
    rc = m.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "paired" in out.lower() or "atomicity" in out.lower()
    assert "needs" in out.lower() or "sentinel" in out.lower()


# ---------------------------------------------------------------------------
# COE-only flip with `Paired: #668` in PR body → pass
# ---------------------------------------------------------------------------
def test_diff_touches_coe_only_pair_in_body(env, monkeypatch, capsys):
    monkeypatch.setenv("PR_BODY", "Interim coe flip. Paired: #668")
    monkeypatch.setattr(
        subprocess,
        "run",
        _stub_git(
            CI_YML_BASE,
            CI_YML_COE_FLIPPED,
            ["fix(ci): flip coe on platform-build"],
        ),
    )
    m = _import_lint(monkeypatch)
    rc = m.run()
    assert rc == 0
    out = capsys.readouterr().out
    assert "paired" in out.lower()
    assert "668" in out


# ---------------------------------------------------------------------------
# Needs-only flip with `Paired: #665` in a commit message → pass
# ---------------------------------------------------------------------------
def test_diff_touches_needs_only_pair_in_commit(env, monkeypatch, capsys):
    monkeypatch.setattr(
        subprocess,
        "run",
        _stub_git(
            CI_YML_BASE,
            CI_YML_NEEDS_CHANGED,
            [
                "fix(ci): drop canvas-build from sentinel\n\nPaired: #665",
            ],
        ),
    )
    m = _import_lint(monkeypatch)
    rc = m.run()
    assert rc == 0
    out = capsys.readouterr().out
    assert "paired" in out.lower()
    assert "665" in out


# ---------------------------------------------------------------------------
# `Paired: #abc` is not a valid issue/PR ref — fail
# ---------------------------------------------------------------------------
def test_paired_reference_must_be_numeric(env, monkeypatch, capsys):
    monkeypatch.setenv("PR_BODY", "Paired: #abc")
    monkeypatch.setattr(
        subprocess,
        "run",
        _stub_git(
            CI_YML_BASE,
            CI_YML_COE_FLIPPED,
            ["fix(ci): flip coe"],
        ),
    )
    m = _import_lint(monkeypatch)
    rc = m.run()
    assert rc == 1


# ---------------------------------------------------------------------------
# Defensive: ci.yml not in diff at all → skip cleanly
# ---------------------------------------------------------------------------
def test_ci_yml_unchanged_skips(env, monkeypatch, capsys):
    monkeypatch.setattr(
        subprocess, "run", _stub_git(CI_YML_BASE, CI_YML_BASE, ["chore: noop"])
    )
    m = _import_lint(monkeypatch)
    rc = m.run()
    assert rc == 0
    out = capsys.readouterr().out
    assert "ci.yml" in out.lower() or "not in" in out.lower() or "skip" in out.lower()


# ---------------------------------------------------------------------------
# Cross-cutting: file ADDED on head side (no base) — coe inferred as
# "newly added with coe=true". Should NOT trigger the lint (it's a new
# file, not a flip — Tier 2e covers tracking-issue for new coe=true).
# ---------------------------------------------------------------------------
def test_ci_yml_newly_added_passes(env, monkeypatch, capsys):
    monkeypatch.setattr(
        subprocess,
        "run",
        _stub_git(None, CI_YML_COE_FLIPPED, ["feat(ci): add ci.yml"]),
    )
    m = _import_lint(monkeypatch)
    rc = m.run()
    assert rc == 0
