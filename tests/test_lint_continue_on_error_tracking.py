"""Tests for `.gitea/scripts/lint_continue_on_error_tracking.py` — Tier 2e lint.

Structural enforcement of internal#350 Tier 2e: every
`continue-on-error: true` directive in `.gitea/workflows/*.yml` must be
accompanied by a `# mc#NNNN` or `# internal#NNNN` comment within 2 lines
(above OR below), the referenced issue must be OPEN, and ≤14 days old
counted from `created_at`. Older than 14 days → fail, forces close-or-renew.

The class this lint exists to prevent: Phase-3-masked failures.
`continue-on-error: true` on platform-build had been hiding mc#664-class
regressions for ~3 weeks before #656 surfaced them. A 14-day cap forces
a tracker review cycle, preventing indefinite-mask drift.

Test classes (per `feedback_branch_count_before_approving`):

  - test_coe_false_is_ignored                  — `continue-on-error: false`
    has no tracker requirement. Exit 0.
  - test_coe_true_with_open_recent_mc_passes   — coe true + adjacent
    `# mc#1234` comment, issue open and 5 days old. Exit 0.
  - test_coe_true_with_open_recent_internal    — adjacent `# internal#42`,
    open, 1 day old. Exit 0.
  - test_coe_true_no_comment_fails             — coe true with no
    nearby tracker comment. Exit 1, names the file+line and the
    required tracker shape.
  - test_coe_true_comment_too_far_away_fails   — `# mc#1234` 5 lines
    above the coe directive — outside the 2-line window. Exit 1.
  - test_coe_true_closed_issue_fails           — issue exists but is
    `state=closed`. Exit 1, names the issue.
  - test_coe_true_too_old_issue_fails          — issue open but
    `created_at` is 20 days ago. Exit 1, mentions the age cap.
  - test_coe_true_at_14d_passes                — boundary: exactly 14d
    old. Inclusive. Exit 0.
  - test_coe_true_at_15d_fails                 — boundary: 15d old.
    Exclusive. Exit 1.
  - test_coe_true_api_404_fails                — referenced issue
    doesn't exist (deleted or typo). Exit 1.
  - test_coe_true_api_403_skips                — token-scope issue,
    graceful-degrade per Tier 2a contract: exit 0 with ::error::,
    do NOT red-X every PR over auth.
  - test_two_coe_true_one_violating            — multi-violation
    aggregation: one passes, one fails → exit 1, all violations
    surfaced (not short-circuited).
  - test_coe_true_with_comment_AFTER_directive — comment on the line
    below the directive (within 2 lines) still satisfies. Exit 0.
  - test_coe_value_quoted_string_true_caught   — `continue-on-error: "true"`
    parses to the string "true" via PyYAML which is truthy but NOT
    boolean `True` — the lint catches the IR `True` from
    `continue-on-error: true`, and also flags string `"true"` because
    Gitea's evaluator coerces it.

Stubs:
  - `subprocess.run` is NOT used (this lint reads only files +
    HTTP); `urllib.request.urlopen` IS stubbed via monkeypatch on
    the module-level `api()` to drive issue-API responses.

Run:
    python3 -m pytest tests/test_lint_continue_on_error_tracking.py -v
"""
from __future__ import annotations

import importlib.util
import os
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest


SCRIPT_PATH = (
    Path(__file__).resolve().parent.parent
    / ".gitea"
    / "scripts"
    / "lint_continue_on_error_tracking.py"
)


def _now_iso() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _iso_days_ago(days: int) -> str:
    dt = datetime.now(timezone.utc) - timedelta(days=days)
    return dt.strftime("%Y-%m-%dT%H:%M:%SZ")


def _import_lint():
    spec = importlib.util.spec_from_file_location(
        f"lint_coe_tracking_{os.getpid()}",
        SCRIPT_PATH,
    )
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


@pytest.fixture()
def envset(tmp_path, monkeypatch):
    wf_dir = tmp_path / ".gitea" / "workflows"
    wf_dir.mkdir(parents=True)
    monkeypatch.setenv("WORKFLOWS_DIR", str(wf_dir))
    monkeypatch.setenv("GITEA_TOKEN", "fake-token")
    monkeypatch.setenv("GITEA_HOST", "git.example.test")
    monkeypatch.setenv("REPO", "owner/molecule-core")
    monkeypatch.setenv("INTERNAL_REPO", "owner/internal")
    monkeypatch.setenv("MAX_AGE_DAYS", "14")
    return wf_dir


def _write_wf(wf_dir: Path, name: str, content: str) -> Path:
    p = wf_dir / name
    p.write_text(content)
    return p


def _stub_issue_api(monkeypatch, lint_mod, responses: dict[str, dict]):
    """Stub the module's `fetch_issue` to drive issue lookups.

    responses keyed by `"<repo-suffix>#NNN"` (e.g. `"mc#1234"`, `"internal#42"`).
    Each value is either:
      - a dict {"state": "open"|"closed", "created_at": "..."} — normal hit
      - the string "404" — issue not found
      - the string "403" — auth denied (token scope)
      - the string "500" — server error
    """

    def fake_fetch(slug_kind: str, num: int):
        key = f"{slug_kind}#{num}"
        r = responses.get(key)
        if r is None:
            # Tests must declare every issue they reference.
            raise AssertionError(f"no test stub for {key}")
        if r == "404":
            return ("not_found", None)
        if r == "403":
            return ("forbidden", None)
        if r == "500":
            return ("error", None)
        return ("ok", r)

    monkeypatch.setattr(lint_mod, "fetch_issue", fake_fetch)


# ---------------------------------------------------------------------------
# continue-on-error: false → no tracker required
# ---------------------------------------------------------------------------
def test_coe_false_is_ignored(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "ok.yml",
        "name: ok\non: [push]\njobs:\n  a:\n    runs-on: x\n    continue-on-error: false\n    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(monkeypatch, m, {})
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# coe true + adjacent OPEN recent mc# tracker → pass
# ---------------------------------------------------------------------------
def test_coe_true_with_open_recent_mc_passes(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "wf.yml",
        "name: w\non: [push]\njobs:\n  a:\n    runs-on: x\n"
        "    # mc#1234 — surfacing flaky test, fix-or-renew\n"
        "    continue-on-error: true\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(
        monkeypatch,
        m,
        {"mc#1234": {"state": "open", "created_at": _iso_days_ago(5)}},
    )
    rc = m.run()
    assert rc == 0


def test_coe_true_with_open_recent_internal(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "wf.yml",
        "name: w\non: [push]\njobs:\n  a:\n    runs-on: x\n"
        "    continue-on-error: true\n"
        "    # internal#42 — phase-3 ladder soak\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(
        monkeypatch,
        m,
        {"internal#42": {"state": "open", "created_at": _iso_days_ago(1)}},
    )
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# coe true + no nearby tracker comment → fail
# ---------------------------------------------------------------------------
def test_coe_true_no_comment_fails(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "bad.yml",
        "name: b\non: [push]\njobs:\n  a:\n    runs-on: x\n"
        "    continue-on-error: true\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(monkeypatch, m, {})
    rc = m.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "bad.yml" in out
    assert "mc#" in out.lower() or "internal#" in out.lower()


# ---------------------------------------------------------------------------
# Comment too far away — outside the 2-line window → fail
# ---------------------------------------------------------------------------
def test_coe_true_comment_too_far_away_fails(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "far.yml",
        "name: f\non: [push]\n"
        "# mc#1234 — referenced too far above\n"
        "jobs:\n"
        "  a:\n"
        "    runs-on: x\n"
        "    name: stage\n"
        "    timeout-minutes: 5\n"
        "    continue-on-error: true\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(
        monkeypatch,
        m,
        {"mc#1234": {"state": "open", "created_at": _iso_days_ago(1)}},
    )
    rc = m.run()
    assert rc == 1


# ---------------------------------------------------------------------------
# Closed issue → fail
# ---------------------------------------------------------------------------
def test_coe_true_closed_issue_fails(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "wf.yml",
        "name: w\non: [push]\njobs:\n  a:\n    runs-on: x\n"
        "    # mc#999\n"
        "    continue-on-error: true\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(
        monkeypatch,
        m,
        {"mc#999": {"state": "closed", "created_at": _iso_days_ago(1)}},
    )
    rc = m.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "999" in out
    assert "closed" in out.lower()


# ---------------------------------------------------------------------------
# Issue is too old (>14d) → fail
# ---------------------------------------------------------------------------
def test_coe_true_too_old_issue_fails(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "wf.yml",
        "name: w\non: [push]\njobs:\n  a:\n    runs-on: x\n"
        "    # mc#7\n"
        "    continue-on-error: true\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(
        monkeypatch,
        m,
        {"mc#7": {"state": "open", "created_at": _iso_days_ago(20)}},
    )
    rc = m.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "20" in out or "14" in out


def test_coe_true_at_14d_passes(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "wf.yml",
        "name: w\non: [push]\njobs:\n  a:\n    runs-on: x\n"
        "    # mc#7\n"
        "    continue-on-error: true\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(
        monkeypatch,
        m,
        {"mc#7": {"state": "open", "created_at": _iso_days_ago(14)}},
    )
    rc = m.run()
    assert rc == 0


def test_coe_true_at_15d_fails(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "wf.yml",
        "name: w\non: [push]\njobs:\n  a:\n    runs-on: x\n"
        "    # mc#7\n"
        "    continue-on-error: true\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(
        monkeypatch,
        m,
        {"mc#7": {"state": "open", "created_at": _iso_days_ago(15)}},
    )
    rc = m.run()
    assert rc == 1


# ---------------------------------------------------------------------------
# 404 (deleted/typo) → fail
# ---------------------------------------------------------------------------
def test_coe_true_api_404_fails(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "wf.yml",
        "name: w\non: [push]\njobs:\n  a:\n    runs-on: x\n"
        "    # mc#9999\n"
        "    continue-on-error: true\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(monkeypatch, m, {"mc#9999": "404"})
    rc = m.run()
    assert rc == 1


# ---------------------------------------------------------------------------
# 403 (token-scope, not lint's fault) → exit 0 with ::error:: per
# Tier 2a graceful-degrade contract.
# ---------------------------------------------------------------------------
def test_coe_true_api_403_skips(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "wf.yml",
        "name: w\non: [push]\njobs:\n  a:\n    runs-on: x\n"
        "    # mc#1\n"
        "    continue-on-error: true\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(monkeypatch, m, {"mc#1": "403"})
    rc = m.run()
    assert rc == 0
    err = capsys.readouterr().err
    assert "403" in err or "scope" in err.lower() or "token" in err.lower()


# ---------------------------------------------------------------------------
# Multi-violation aggregation — all surfaced, not short-circuited
# ---------------------------------------------------------------------------
def test_two_coe_true_one_violating(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "two.yml",
        "name: t\non: [push]\njobs:\n"
        "  good:\n"
        "    runs-on: x\n"
        "    # mc#100\n"
        "    continue-on-error: true\n"
        "    steps:\n      - run: echo a\n"
        "  bad:\n"
        "    runs-on: x\n"
        "    continue-on-error: true\n"
        "    steps:\n      - run: echo b\n",
    )
    m = _import_lint()
    _stub_issue_api(
        monkeypatch,
        m,
        {"mc#100": {"state": "open", "created_at": _iso_days_ago(2)}},
    )
    rc = m.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "bad" in out.lower() or "no tracker" in out.lower()


# ---------------------------------------------------------------------------
# Comment on line AFTER the directive — within 2-line window → pass
# ---------------------------------------------------------------------------
def test_coe_true_with_comment_AFTER_directive(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "after.yml",
        "name: a\non: [push]\njobs:\n  a:\n    runs-on: x\n"
        "    continue-on-error: true  # mc#3\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(
        monkeypatch,
        m,
        {"mc#3": {"state": "open", "created_at": _iso_days_ago(0)}},
    )
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# Quoted string `"true"` — coerced by Gitea evaluator; should be caught
# ---------------------------------------------------------------------------
def test_coe_value_quoted_string_true_caught(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "quoted.yml",
        "name: q\non: [push]\njobs:\n  a:\n    runs-on: x\n"
        "    continue-on-error: \"true\"\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_issue_api(monkeypatch, m, {})
    rc = m.run()
    # No tracker → fail
    assert rc == 1
