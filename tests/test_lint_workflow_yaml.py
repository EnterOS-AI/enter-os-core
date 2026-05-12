"""Tests for `.gitea/scripts/lint-workflow-yaml.py` — Gitea-1.22.6-hostile shape lint.

Hard-gate (Tier-2) lint that catches workflow YAML shapes Gitea 1.22.6
silently rejects, so they never reach `main`. The six anti-patterns are
documented in saved memory; this test suite is the structural enforcement.

Per-rule positive (anti-pattern present -> exit 1) + negative (clean -> exit 0)
cases, plus a multi-file collision case and an aggregation case.

Run:
    python3 -m pytest tests/test_lint_workflow_yaml.py -v

Dependencies: stdlib + PyYAML. No network.

Cross-links:
- feedback_gitea_workflow_dispatch_inputs_unsupported (rule 1)
- internal task #81 (rule 2 — workflow_run unsupported)
- feedback_workflow_name_with_slash_breaks_parsing (rule 3, if filed)
- feedback_gitea_cross_repo_uses_blocked (rule 5)
- feedback_act_runner_github_server_url (rule 6)
- feedback_smoke_test_vendor_truth_not_shape_match (test-shape rule)
"""
from __future__ import annotations

import subprocess
import sys
import textwrap
from pathlib import Path

import pytest  # noqa: F401  (declares the dep)

REPO_ROOT = Path(__file__).resolve().parents[1]
SCRIPT = REPO_ROOT / ".gitea" / "scripts" / "lint-workflow-yaml.py"


def _run_lint(workflow_dir: Path) -> subprocess.CompletedProcess:
    """Invoke the lint as a subprocess against an isolated workflow dir."""
    return subprocess.run(
        [sys.executable, str(SCRIPT), "--workflow-dir", str(workflow_dir)],
        capture_output=True,
        text=True,
    )


def _write(workflow_dir: Path, name: str, content: str) -> Path:
    """Write a workflow YAML fixture and return its path."""
    workflow_dir.mkdir(parents=True, exist_ok=True)
    p = workflow_dir / name
    p.write_text(textwrap.dedent(content).lstrip())
    return p


# ---------------------------------------------------------------------------
# Rule 1 — workflow_dispatch.inputs (Gitea 1.22.6 parser rejects)
# ---------------------------------------------------------------------------

WD_INPUTS_BAD = """
    name: bad-wd-inputs
    on:
      workflow_dispatch:
        inputs:
          version:
            description: "version"
            required: true
            type: string
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo hi
"""

WD_INPUTS_OK = """
    name: ok-wd-no-inputs
    on:
      workflow_dispatch:
      push:
        branches: [main]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo hi
"""


def test_rule1_workflow_dispatch_inputs_detects_violation(tmp_path):
    _write(tmp_path, "bad.yml", WD_INPUTS_BAD)
    r = _run_lint(tmp_path)
    assert r.returncode == 1
    assert "workflow_dispatch.inputs" in r.stdout
    assert "bad.yml" in r.stdout


def test_rule1_workflow_dispatch_inputs_passes_when_absent(tmp_path):
    _write(tmp_path, "ok.yml", WD_INPUTS_OK)
    r = _run_lint(tmp_path)
    assert r.returncode == 0, f"stdout={r.stdout}\nstderr={r.stderr}"


# ---------------------------------------------------------------------------
# Rule 2 — workflow_run event (not supported on Gitea 1.22.6)
# ---------------------------------------------------------------------------

WF_RUN_BAD = """
    name: bad-workflow-run
    on:
      workflow_run:
        workflows: ["upstream"]
        types: [completed]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo hi
"""

WF_RUN_OK = """
    name: ok-no-workflow-run
    on:
      push:
        branches: [main]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo hi
"""


def test_rule2_workflow_run_event_detects_violation(tmp_path):
    _write(tmp_path, "bad.yml", WF_RUN_BAD)
    r = _run_lint(tmp_path)
    assert r.returncode == 1
    assert "workflow_run" in r.stdout
    assert "bad.yml" in r.stdout


def test_rule2_workflow_run_event_passes_when_absent(tmp_path):
    _write(tmp_path, "ok.yml", WF_RUN_OK)
    r = _run_lint(tmp_path)
    assert r.returncode == 0, f"stdout={r.stdout}\nstderr={r.stderr}"


# ---------------------------------------------------------------------------
# Rule 3 — name: contains "/" (breaks "<workflow> / <job> (<event>)" parsing)
# ---------------------------------------------------------------------------

NAME_SLASH_BAD = """
    name: ci / build
    on: [push]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo hi
"""

NAME_SLASH_OK = """
    name: ci-build
    on: [push]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo hi
"""


def test_rule3_name_with_slash_detects_violation(tmp_path):
    _write(tmp_path, "bad.yml", NAME_SLASH_BAD)
    r = _run_lint(tmp_path)
    assert r.returncode == 1
    assert "name" in r.stdout.lower()
    assert "/" in r.stdout
    assert "bad.yml" in r.stdout


def test_rule3_name_with_slash_passes_when_absent(tmp_path):
    _write(tmp_path, "ok.yml", NAME_SLASH_OK)
    r = _run_lint(tmp_path)
    assert r.returncode == 0, f"stdout={r.stdout}\nstderr={r.stderr}"


# ---------------------------------------------------------------------------
# Rule 4 — name collision across files (cross-file)
# ---------------------------------------------------------------------------

COLLISION_A = """
    name: shared-name
    on: [push]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo a
"""

COLLISION_B = """
    name: shared-name
    on: [push]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo b
"""

DISTINCT_A = """
    name: name-a
    on: [push]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo a
"""

DISTINCT_B = """
    name: name-b
    on: [push]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo b
"""


def test_rule4_name_collision_across_two_files_detects_violation(tmp_path):
    _write(tmp_path, "a.yml", COLLISION_A)
    _write(tmp_path, "b.yml", COLLISION_B)
    r = _run_lint(tmp_path)
    assert r.returncode == 1
    assert ("collision" in r.stdout.lower()) or ("duplicate" in r.stdout.lower())
    assert "shared-name" in r.stdout


def test_rule4_name_collision_passes_when_names_distinct(tmp_path):
    _write(tmp_path, "a.yml", DISTINCT_A)
    _write(tmp_path, "b.yml", DISTINCT_B)
    r = _run_lint(tmp_path)
    assert r.returncode == 0, f"stdout={r.stdout}\nstderr={r.stderr}"


# ---------------------------------------------------------------------------
# Rule 5 — cross-repo `uses: org/repo/...@ref` (blocked on 1.22.6)
# ---------------------------------------------------------------------------

CROSS_REPO_BAD = """
    name: bad-cross-repo
    on: [push]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - uses: molecule-ai/molecule-ci/.gitea/actions/audit-force-merge@main
"""

# actions/checkout — bare `org/repo@ref` form — allowed. Rule 5 targets
# `org/repo/SUBPATH@ref` cross-repo composite/reusable references because
# only those resolve through `[actions].DEFAULT_ACTIONS_URL`+org-suspended-host.
CROSS_REPO_OK = """
    name: ok-no-cross-repo
    on: [push]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd
          - run: echo hi
"""


def test_rule5_cross_repo_uses_detects_violation(tmp_path):
    _write(tmp_path, "bad.yml", CROSS_REPO_BAD)
    r = _run_lint(tmp_path)
    assert r.returncode == 1
    assert ("cross-repo" in r.stdout.lower()) or ("uses" in r.stdout.lower())
    assert "bad.yml" in r.stdout


def test_rule5_cross_repo_uses_passes_when_only_actions_org(tmp_path):
    _write(tmp_path, "ok.yml", CROSS_REPO_OK)
    r = _run_lint(tmp_path)
    assert r.returncode == 0, f"stdout={r.stdout}\nstderr={r.stderr}"


# ---------------------------------------------------------------------------
# Rule 6 — GITHUB_SERVER_URL heuristic (warn-not-fail per halt-condition 3)
# ---------------------------------------------------------------------------

GH_API_REF_NO_SERVER = """
    name: warn-server-url
    on: [push]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: curl https://api.github.com/repos/foo/bar
"""

GH_API_REF_WITH_SERVER = """
    name: ok-server-url-set
    on: [push]
    env:
      GITHUB_SERVER_URL: https://git.moleculesai.app
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: curl https://api.github.com/repos/foo/bar
"""


def test_rule6_github_server_url_missing_is_warning_not_fatal(tmp_path):
    """Heuristic rule — emits warning but does NOT exit 1.

    Per halt-condition 3: heuristic may false-positive (current main has 3:
    OCI label + jq-release URL refs). Downgrade to warn-not-fail.
    """
    _write(tmp_path, "warn.yml", GH_API_REF_NO_SERVER)
    r = _run_lint(tmp_path)
    assert r.returncode == 0
    combined = (r.stdout + r.stderr).lower()
    assert ("github_server_url" in combined) or ("::warning" in combined)


def test_rule6_github_server_url_present_no_warning(tmp_path):
    _write(tmp_path, "ok.yml", GH_API_REF_WITH_SERVER)
    r = _run_lint(tmp_path)
    assert r.returncode == 0
    # No warning emitted (server URL is set)
    assert "::warning" not in r.stdout


# ---------------------------------------------------------------------------
# Aggregation — single file with multiple anti-patterns
# ---------------------------------------------------------------------------

MULTI_VIOLATIONS = """
    name: ci / multi
    on:
      workflow_dispatch:
        inputs:
          v:
            type: string
      workflow_run:
        workflows: [up]
        types: [completed]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - uses: molecule-ai/molecule-ci/.gitea/actions/x@main
"""


def test_all_violations_aggregated_single_file(tmp_path):
    _write(tmp_path, "multi.yml", MULTI_VIOLATIONS)
    r = _run_lint(tmp_path)
    assert r.returncode == 1
    out = r.stdout
    # All four FATAL rules should be reported (1, 2, 3, 5)
    assert "workflow_dispatch.inputs" in out
    assert "workflow_run" in out
    assert "/" in out  # rule 3 surfaces the slash
    assert ("cross-repo" in out.lower()) or ("uses" in out.lower())


# ---------------------------------------------------------------------------
# Empty-dir / no-workflows edge case
# ---------------------------------------------------------------------------

def test_no_workflows_exits_zero(tmp_path):
    r = _run_lint(tmp_path)
    assert r.returncode == 0


# ---------------------------------------------------------------------------
# Vendor-truth: rule 1 catches the exact 2026-05-11 publish-runtime.yml shape
# ---------------------------------------------------------------------------

# The exact YAML shape from feedback_gitea_workflow_dispatch_inputs_unsupported
# that caused publish-runtime-v1.0.0 to silently freeze PyPI at 0.1.129 for ~24h.
PUBLISH_RUNTIME_VENDOR_TRUTH = """
    name: publish-runtime
    on:
      push:
        tags: ['runtime-v*']
      workflow_dispatch:
        inputs:
          version:
            description: "Version to publish (e.g. 0.1.6). Required for manual dispatch."
            required: true
            type: string
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo hi
"""


def test_rule1_catches_2026_05_11_publish_runtime_regression(tmp_path):
    """Vendor-truth fixture: the exact YAML shape that froze PyPI for 24h."""
    _write(tmp_path, "publish-runtime.yml", PUBLISH_RUNTIME_VENDOR_TRUTH)
    r = _run_lint(tmp_path)
    assert r.returncode == 1, (
        "Lint must catch the 2026-05-11 publish-runtime regression "
        f"(memory: feedback_gitea_workflow_dispatch_inputs_unsupported)."
        f"\nstdout={r.stdout}"
    )
