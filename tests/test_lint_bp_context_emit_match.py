"""Tests for `.gitea/scripts/lint_bp_context_emit_match.py` — Tier 2f lint.

Structural enforcement of internal#350 Tier 2f: BP `status_check_contexts`
and the set of contexts emitted by `.gitea/workflows/*.yml` must agree.

Bidirectional rule:
  (a) BP-only: every context in `branch_protections/<branch>.status_check_contexts`
      must have at least one EMITTER — a workflow `name:` + job `name:` (or job key)
      + `pull_request` (or `push`) event that produces it. A BP context without
      an emitter blocks merges forever (Gitea treats absent-as-pending, NOT
      absent-as-skipped). This is the phantom-required-check class
      (`feedback_phantom_required_check_after_gitea_migration`).

  (b) EMITTER-only: NO automatic flag. The PR#656 case (workflow added a
      sentinel context not yet in BP) is Tier 2g's job — a diff-based PR-time
      lint. Tier 2f runs scheduled and would falsely flag every transitional
      state during a BP rollout. We only flag the BP-empty case in this
      direction as a NOTICE (informational), not as an error.

Tier 2f runs on a daily schedule + workflow_dispatch and files a
`[ci-bp-drift]`-tagged issue on mismatch.

Test classes (per `feedback_branch_count_before_approving`):

  - test_perfect_match_passes              — BP has [X]; workflows emit X.
    Exit 0. No issue filed/edited.
  - test_bp_orphan_context_fails           — BP has [Y] but no workflow
    emits Y. Exit 1. Issue body lists the orphan and the closest
    candidate workflow names (Levenshtein-1 suggestion for typos).
  - test_emitter_orphan_only_warns         — workflow emits Z but BP
    doesn't have it. Exit 0 with ::notice:: (NOT ::error::) because
    Tier 2g handles this at PR time.
  - test_multiple_orphans_aggregated       — two BP orphans surfaced
    together, not short-circuited.
  - test_bp_empty_lints_nothing            — BP has no contexts.
    Exit 0 cleanly.
  - test_api_403_fails_closed              — branch_protections endpoint
    401/403s (auth failure). FAIL CLOSED (exit 2) with ::error::.
  - test_api_transient_fails_closed        — transient/unexpected API
    error. FAIL CLOSED (exit 2).
  - test_api_404_skips_gracefully          — branch has no protection
    (authenticated absent resource). Tolerated skip (exit 0 + warning).
    Exit 0 cleanly.
  - test_context_event_match_required      — BP context says `(push)` and
    workflow only emits on `pull_request`. That's NOT a match — the
    BP-required gate would still wedge. Exit 1.
  - test_workflow_event_mapping_pull_request_target — `pull_request_target`
    in workflow `on:` emits a `(pull_request)` context (Gitea convention).
    Match counts.
  - test_idempotent_issue_filing           — when an issue already exists
    with the canonical title prefix, edit it instead of POSTing a new one
    (idempotency contract — mirrors ci-required-drift).

Run:
    python3 -m pytest tests/test_lint_bp_context_emit_match.py -v
"""
from __future__ import annotations

import importlib.util
import os
from pathlib import Path

import pytest


SCRIPT_PATH = (
    Path(__file__).resolve().parent.parent
    / ".gitea"
    / "scripts"
    / "lint_bp_context_emit_match.py"
)


def _import_lint():
    spec = importlib.util.spec_from_file_location(
        f"lint_bp_emit_{os.getpid()}", SCRIPT_PATH
    )
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


@pytest.fixture()
def envset(tmp_path, monkeypatch):
    wf = tmp_path / ".gitea" / "workflows"
    wf.mkdir(parents=True)
    monkeypatch.setenv("WORKFLOWS_DIR", str(wf))
    monkeypatch.setenv("GITEA_TOKEN", "stub")
    monkeypatch.setenv("GITEA_HOST", "git.example.test")
    monkeypatch.setenv("REPO", "owner/molecule-core")
    monkeypatch.setenv("BRANCH", "main")
    monkeypatch.setenv("DRIFT_LABEL", "ci-bp-drift")
    return wf


def _write_wf(d: Path, name: str, content: str) -> Path:
    p = d / name
    p.write_text(content)
    return p


def _stub_api(monkeypatch, lint_mod, bp_response, issue_search_response=None, posted_record=None):
    """Stub the module's `api` function.

    bp_response: ("ok", {"status_check_contexts": [...]})
                 or ("forbidden", None) / ("not_found", None)
    issue_search_response: list of issues matching the search query (
                           may be empty; default empty)
    posted_record: dict in which to record any POST/PATCH calls made
                   (so tests can assert idempotency).
    """
    if issue_search_response is None:
        issue_search_response = []
    if posted_record is None:
        posted_record = {}

    def fake_api(method, path, *, body=None, query=None):
        if "branch_protections" in path:
            return bp_response
        if "issues/search" in path or "/issues?" in path or path.endswith("/issues"):
            if method == "GET":
                return ("ok", list(issue_search_response))
            if method == "POST":
                posted_record.setdefault("posts", []).append({"path": path, "body": body})
                return ("ok", {"number": 9001, "html_url": "http://t/9001"})
        if "/issues/" in path and method == "PATCH":
            posted_record.setdefault("patches", []).append({"path": path, "body": body})
            return ("ok", {"number": 9001})
        if "/labels" in path:
            return ("ok", [{"id": 10, "name": "ci-bp-drift"}, {"id": 9, "name": "ci-bp-drift"}])
        return ("ok", {})

    monkeypatch.setattr(lint_mod, "api", fake_api)
    return posted_record


# ---------------------------------------------------------------------------
# Perfect match — both sides agree.
# ---------------------------------------------------------------------------
def test_perfect_match_passes(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "ci.yml",
        "name: CI\non:\n  pull_request:\n    branches: [main]\njobs:\n"
        "  all-required:\n    runs-on: x\n    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_api(
        monkeypatch,
        m,
        ("ok", {"status_check_contexts": ["CI / all-required (pull_request)"]}),
    )
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# BP-only orphan — context with no emitter.
# ---------------------------------------------------------------------------
def test_bp_orphan_context_fails(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "ci.yml",
        "name: CI\non:\n  pull_request:\n    branches: [main]\njobs:\n"
        "  all-required:\n    runs-on: x\n    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_api(
        monkeypatch,
        m,
        ("ok", {"status_check_contexts": [
            "CI / all-required (pull_request)",
            "Ghost workflow / ghost (pull_request)",  # the orphan
        ]}),
    )
    rc = m.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "Ghost workflow" in out or "ghost" in out.lower()


# ---------------------------------------------------------------------------
# Emitter-only direction → notice, not error (Tier 2g territory).
# ---------------------------------------------------------------------------
def test_emitter_orphan_only_warns(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "extra.yml",
        "name: Extra\non:\n  pull_request:\n    branches: [main]\njobs:\n"
        "  extra-job:\n    runs-on: x\n    steps:\n      - run: echo hi\n",
    )
    _write_wf(
        envset,
        "ci.yml",
        "name: CI\non:\n  pull_request:\n    branches: [main]\njobs:\n"
        "  all-required:\n    runs-on: x\n    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_api(
        monkeypatch,
        m,
        ("ok", {"status_check_contexts": ["CI / all-required (pull_request)"]}),
    )
    rc = m.run()
    assert rc == 0
    out = capsys.readouterr().out
    assert "Extra" in out or "extra" in out


# ---------------------------------------------------------------------------
# Multiple BP orphans — all surfaced.
# ---------------------------------------------------------------------------
def test_multiple_orphans_aggregated(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "ci.yml",
        "name: CI\non:\n  pull_request:\n    branches: [main]\njobs:\n"
        "  all-required:\n    runs-on: x\n    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_api(
        monkeypatch,
        m,
        ("ok", {"status_check_contexts": [
            "CI / all-required (pull_request)",
            "Phantom A / a (pull_request)",
            "Phantom B / b (pull_request)",
        ]}),
    )
    rc = m.run()
    assert rc == 1
    out = capsys.readouterr().out
    assert "Phantom A" in out and "Phantom B" in out


# ---------------------------------------------------------------------------
# BP has zero contexts → nothing to lint, pass.
# ---------------------------------------------------------------------------
def test_bp_empty_lints_nothing(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "ci.yml",
        "name: CI\non:\n  pull_request:\n    branches: [main]\njobs:\n"
        "  all-required:\n    runs-on: x\n    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_api(monkeypatch, m, ("ok", {"status_check_contexts": []}))
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# API 403 — AUTH FAILURE → FAIL CLOSED (exit 2). This is a HARD gate on a
# protected context; a token that can't read BP must NOT green the lint.
# ---------------------------------------------------------------------------
def test_api_403_fails_closed(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "ci.yml",
        "name: CI\non:\n  pull_request:\n    branches: [main]\njobs:\n"
        "  j:\n    runs-on: x\n    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_api(monkeypatch, m, ("forbidden", None))
    rc = m.run()
    assert rc == 2
    err = capsys.readouterr().err
    assert "403" in err or "scope" in err.lower() or "token" in err.lower()


# ---------------------------------------------------------------------------
# API transient/unexpected error → FAIL CLOSED (exit 2).
# ---------------------------------------------------------------------------
def test_api_transient_fails_closed(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "ci.yml",
        "name: CI\non:\n  pull_request:\n    branches: [main]\njobs:\n"
        "  j:\n    runs-on: x\n    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_api(monkeypatch, m, ("error", None))
    rc = m.run()
    assert rc == 2


# ---------------------------------------------------------------------------
# API 404 — authenticated absent resource (branch has no protection) →
# tolerated graceful skip (exit 0 with ::warning::), NOT a fail-open.
# ---------------------------------------------------------------------------
def test_api_404_skips_gracefully(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "ci.yml",
        "name: CI\non:\n  pull_request:\n    branches: [main]\njobs:\n"
        "  j:\n    runs-on: x\n    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_api(monkeypatch, m, ("not_found", None))
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# Event-suffix match strict: BP says (push), workflow emits (pull_request)
# only. Mismatch — flag.
# ---------------------------------------------------------------------------
def test_context_event_match_required(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "ci.yml",
        "name: CI\non:\n  pull_request:\n    branches: [main]\njobs:\n"
        "  all-required:\n    runs-on: x\n    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_api(
        monkeypatch,
        m,
        ("ok", {"status_check_contexts": ["CI / all-required (push)"]}),
    )
    rc = m.run()
    assert rc == 1


# ---------------------------------------------------------------------------
# `pull_request_target` in workflow `on:` emits a `(pull_request)` context
# (Gitea convention — verified empirically on molecule-core).
# ---------------------------------------------------------------------------
def test_workflow_event_mapping_pull_request_target(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "secret.yml",
        "name: Secret scan\non:\n  pull_request_target:\n    branches: [main]\njobs:\n"
        "  scan:\n    runs-on: x\n    name: Scan diff for credential-shaped strings\n"
        "    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    _stub_api(
        monkeypatch,
        m,
        ("ok", {"status_check_contexts": [
            "Secret scan / Scan diff for credential-shaped strings (pull_request)",
        ]}),
    )
    rc = m.run()
    assert rc == 0


# ---------------------------------------------------------------------------
# Idempotency — existing open issue is PATCHed, not duplicated.
# ---------------------------------------------------------------------------
def test_idempotent_issue_filing(envset, monkeypatch, capsys):
    _write_wf(
        envset,
        "ci.yml",
        "name: CI\non:\n  pull_request:\n    branches: [main]\njobs:\n"
        "  all-required:\n    runs-on: x\n    steps:\n      - run: echo hi\n",
    )
    m = _import_lint()
    posted = _stub_api(
        monkeypatch,
        m,
        ("ok", {"status_check_contexts": [
            "CI / all-required (pull_request)",
            "Ghost / g (pull_request)",
        ]}),
        issue_search_response=[
            {
                "number": 4242,
                "title": "[ci-bp-drift] owner/molecule-core/main: BP→emitter mismatch",
                "state": "open",
                "html_url": "http://t/4242",
            }
        ],
    )
    rc = m.run()
    assert rc == 1
    # Should have PATCHed, not POSTed a new one.
    assert posted.get("patches"), f"expected PATCH on existing issue; got {posted!r}"
    assert not posted.get("posts"), f"expected no POSTs; got {posted!r}"
