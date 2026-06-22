"""Tests for `.gitea/scripts/main-red-watchdog.py` — Option C of the
main-never-red directive (tracking: molecule-core#420).

Covers:
  - Happy path: main is green, no issue created.
  - Red detected: issue opened with correct title/body containing each
    failed context.
  - Idempotent: existing `[main-red] {repo}: {SHA[:10]}` issue is
    PATCHed in place, NOT duplicated.
  - Auto-close: when main returns to green, prior `[main-red]` issues
    for other SHAs are closed with a comment.
  - HTTP-failure: api() raises ApiError on non-2xx, NOT silently
    swallowed → `find_open_issue_for_sha` and `list_open_red_issues`
    propagate, blocking the duplicate-write regression class per
    `feedback_api_helper_must_raise_not_return_dict`.
  - --dry-run: no API mutation; rendered title/body to stdout.
  - is_red detector logic across all combined/per-context state
    combinations (failure, error, pending, success).

Hostile self-review proof (`feedback_dev_sop_phase_1_to_4`):
  - `test_find_open_issue_for_sha_raises_on_transient_error` exercises
    the regression class — a pre-fix implementation that returned
    `[]`/None on api() failure would fall through and POST a duplicate.
    Verified by stashing the script's `raise ApiError` and re-running:
    test FAILS as required.
  - `test_file_or_update_patches_existing_issue` asserts NO POST when
    an open issue exists. A pre-fix idempotency bug (always-POST)
    would fail this.

Run:
    python3 -m pytest tests/test_main_red_watchdog.py -v

Dependencies: stdlib + pytest. No network. No live Gitea calls.
"""
from __future__ import annotations

import importlib.util
import json
import os
import urllib.error
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
    / "main-red-watchdog.py"
)


@pytest.fixture(autouse=True)
def _stub_time_sleep(monkeypatch):
    """Autouse: stub time.sleep across every test.

    The watchdog's RECHECK_DELAY_SECS (default 90s) is wired into
    run_once() via time.sleep(). Without this stub, integration-style
    tests that exercise run_once() would each block for 90s — a
    pre-fix `pytest -q` ran in ~0.1s; the unstubbed equivalent took
    >4 minutes (task #394 review evidence). Stubbing here keeps the
    suite fast and deterministic without requiring every red-path test
    to remember the patch.
    """
    monkeypatch.setattr("time.sleep", lambda s: None)


@pytest.fixture(scope="module")
def wd_module():
    """Import the script as a module under a known env."""
    env = {
        "GITEA_TOKEN": "fixture-token",
        "GITEA_HOST": "git.example.test",
        "REPO": "owner/repo",
        "WATCH_BRANCH": "main",
        "RED_LABEL": "ci-bp-drift",
    }
    with mock.patch.dict(os.environ, env, clear=False):
        spec = importlib.util.spec_from_file_location(
            "main_red_watchdog", SCRIPT_PATH
        )
        m = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(m)
        # Force-set globals from env (they were captured at import time
        # before our patch.dict took effect on subsequent runs within
        # the same pytest session — same pattern as CP#112 tests).
        m.GITEA_TOKEN = env["GITEA_TOKEN"]
        m.GITEA_HOST = env["GITEA_HOST"]
        m.REPO = env["REPO"]
        m.WATCH_BRANCH = env["WATCH_BRANCH"]
        m.RED_LABEL = env["RED_LABEL"]
        m.OWNER, m.NAME = "owner", "repo"
        m.API = f"https://{env['GITEA_HOST']}/api/v1"
        yield m


# --------------------------------------------------------------------------
# Stub api() helper — records calls + dispatches by (method, path).
# --------------------------------------------------------------------------
def _make_stub_api(responses: dict):
    """Build a fake `api()` callable.

    `responses` maps (method, path) tuples to either:
      - (status_int, body) → returned as-is
      - Exception instance → raised
    Calls are recorded in `.calls` for assertion.
    """
    class StubApi:
        def __init__(self):
            self.calls: list[tuple] = []

        def __call__(self, method, path, *, body=None, query=None, expect_json=True):
            self.calls.append((method, path, body, query))
            # If we've stored a list for this (method, path), rotate through.
            # This supports tests that need sequential responses for the
            # same endpoint without adding query-param noise.
            key = (method, path)
            r = responses.get(key)
            if isinstance(r, list):
                if not r:
                    raise AssertionError(
                        f"stub sequential responses exhausted for {method} "
                        f"{path} — provisioned {len(r)} entries"
                    )
                return r.pop(0)
            if r is not None:
                if isinstance(r, Exception):
                    raise r
                return r
            raise AssertionError(
                f"unexpected api call: {method} {path} (no stub registered)"
            )

    return StubApi()


# Sample SHA used throughout. 40 chars per Gitea convention.
SHA_RED = "deadbeefcafe1234567890abcdef000011112222"
SHA_GREEN = "ababababcdcdcdcd0000111122223333deadc0de"
SHA_NEW = "aaaabbbbccccddddeeeeffff0000111122223333"


def _branches_response(sha: str) -> dict:
    """Shape Gitea returns from /repos/{o}/{r}/branches/{name}."""
    return {"name": "main", "commit": {"id": sha}}


def _branch_alt(sha: str) -> dict:
    """Identical shape but to a different key path so _make_stub_api
    retains a separate first-response entry from the primary
    _branches_response() path.

    The stub stores only the first response per (method, path) pair.
    Tests that need two distinct responses for the same logical
    GET /branches/main call use _branch_alt for the second lookup so
    the stub returns the correct sequential entry.
    """
    return {"name": "main", "commit": {"id": sha}}


def _combined_status(state: str, statuses: list[dict] | None = None) -> dict:
    """Shape Gitea returns from /commits/{sha}/status."""
    return {"state": state, "statuses": statuses or []}


# --------------------------------------------------------------------------
# is_red detector
# --------------------------------------------------------------------------
def test_is_red_combined_failure(wd_module):
    red, failed = wd_module.is_red(_combined_status("failure", [
        {"context": "ci/test", "state": "failure"},
    ]))
    assert red is True
    assert len(failed) == 1
    assert failed[0]["context"] == "ci/test"


def test_is_red_combined_error(wd_module):
    """`error` state (CI infra failed) is also red."""
    red, failed = wd_module.is_red(_combined_status("error", [
        {"context": "ci/test", "state": "error"},
    ]))
    assert red is True
    assert failed[0]["state"] == "error"


def test_is_red_combined_success(wd_module):
    red, failed = wd_module.is_red(_combined_status("success", [
        {"context": "ci/test", "state": "success"},
    ]))
    assert red is False
    assert failed == []


def test_is_red_combined_pending(wd_module):
    """Pending = CI still running. Not red, but not green either; the
    main flow handles green vs pending separately."""
    red, failed = wd_module.is_red(_combined_status("pending", [
        {"context": "ci/test", "state": "pending"},
    ]))
    assert red is False
    assert failed == []


def test_is_red_individual_failure_under_pending(wd_module):
    """A single failed context counts as red even if combined is `pending`
    (matrix half-failed, half-still-running). Catches the case where
    Gitea aggregator hasn't rolled up yet."""
    red, failed = wd_module.is_red(_combined_status("pending", [
        {"context": "ci/lint", "state": "success"},
        {"context": "ci/test", "state": "failure"},
        {"context": "ci/build", "state": "pending"},
    ]))
    assert red is True
    assert [s["context"] for s in failed] == ["ci/test"]


def test_is_red_no_statuses(wd_module):
    """No statuses at all (commit pre-CI or never reported) = not red."""
    red, failed = wd_module.is_red(_combined_status("pending", []))
    assert red is False
    assert failed == []


# --------------------------------------------------------------------------
# Per-entry vendor-truth key (rev4) — see status-reaper rev4 sibling
#
# Gitea 1.22.6 returns per-entry items in combined.statuses[] with key
# `status`, not `state`. Pre-rev4 code only read `state` → failed[]
# was always empty → render_body always emitted the fallback "no
# per-context entries were in a red state". These tests use the
# canonical Gitea shape to lock the fix in.
# --------------------------------------------------------------------------
def test_is_red_vendor_truth_status_key_under_pending(wd_module):
    """Real Gitea 1.22.6 shape: per-entry uses `status`. A single failed
    context counts as red even when combined is `pending`. Pre-rev4
    this returned `(False, [])` because `s.get("state")` was None."""
    red, failed = wd_module.is_red({
        "state": "pending",
        "statuses": [
            {"context": "ci/lint", "status": "success"},
            {"context": "ci/test", "status": "failure"},
            {"context": "ci/build", "status": "pending"},
        ],
    })
    assert red is True
    assert [s["context"] for s in failed] == ["ci/test"]


def test_is_red_status_takes_precedence_over_state(wd_module):
    """If both keys present (defensive), `status` (vendor truth) wins."""
    red, failed = wd_module.is_red({
        "state": "pending",
        "statuses": [
            # `status=failure` is truth even though `state=success` is
            # stale. Locking in the precedence prevents a hypothetical
            # future Gitea release that emits both from re-introducing
            # the bug under a different shape.
            {"context": "ci/test", "status": "failure", "state": "success"},
        ],
    })
    assert red is True
    assert len(failed) == 1


def test_is_red_state_only_fallback_still_works(wd_module):
    """Backward-compat: a legacy fixture or future Gitea variant that
    only emits `state` still trips the red detection via the fallback
    chain. Keeps pre-rev4 fixtures green during the rev4 rollout."""
    red, failed = wd_module.is_red({
        "state": "pending",
        "statuses": [
            {"context": "ci/test", "state": "failure"},  # legacy shape
        ],
    })
    assert red is True
    assert len(failed) == 1


# --------------------------------------------------------------------------
# Cancel-cascade filter (mc#1564) — Gitea maps action_run.status=2 (Failure)
# AND status=3 (Cancelled) BOTH to commit-status `"failure"`. We only want
# real failures (status=2) to file. status=3 entries carry description
# `"Has been cancelled"`; real failures carry `"Failing after Ns"`.
# Canonical Gitea 1.22.6 enum (1=Success, 2=Failure, 3=Cancelled, 4=Skipped,
# 5=Waiting, 6=Running, 7=Blocked) per
# `reference_gitea_action_status_enum_corrected_2026_05_19`.
# --------------------------------------------------------------------------
def test_is_red_skips_cancel_cascade_entry(wd_module):
    """status=3 (Cancelled, description='Has been cancelled') must NOT
    count as red. Cancel-cascade from `concurrency: cancel-in-progress`
    on a busy main was generating phantom `[main-red]` issues (mc#1564
    evidence: mc#1562/#1552/#1540 et al). The filter is the durable fix."""
    red, failed = wd_module.is_red({
        "state": "failure",
        "statuses": [
            {"context": "ci/canvas-deploy-reminder",
             "status": "failure",
             "description": "Has been cancelled"},
        ],
    })
    assert red is False, (
        "cancel-cascade entry (description='Has been cancelled', i.e. "
        "Gitea action_run.status=3) must not trip the watchdog"
    )
    assert failed == []


def test_is_red_keeps_real_failure_entry(wd_module):
    """status=2 (Failure, description='Failing after Ns') IS red.
    Companion to the cancel-cascade filter — we must not over-filter."""
    red, failed = wd_module.is_red({
        "state": "failure",
        "statuses": [
            {"context": "ci/test",
             "status": "failure",
             "description": "Failing after 12s"},
        ],
    })
    assert red is True
    assert len(failed) == 1
    assert failed[0]["context"] == "ci/test"


def test_is_red_mixed_cancel_and_real_failure(wd_module):
    """Real-world shape (mc#1562 body, verified 2026-05-19): combined
    `failure` with a mix of 'Failing after Ns' and 'Has been cancelled'
    entries. The watchdog must file (real failures present) AND the
    failed[] list must contain ONLY the real failures — cancel-cascade
    noise is filtered out of the issue body."""
    red, failed = wd_module.is_red({
        "state": "failure",
        "statuses": [
            {"context": "ci/test", "status": "failure",
             "description": "Failing after 1m49s"},
            {"context": "ci/canvas-deploy-reminder", "status": "failure",
             "description": "Has been cancelled"},
            {"context": "ci/lint", "status": "failure",
             "description": "Failing after 8s"},
        ],
    })
    assert red is True
    assert [s["context"] for s in failed] == ["ci/test", "ci/lint"], (
        "cancel-cascade entry should be filtered out of failed[] body"
    )


def test_is_red_all_entries_cancelled_is_green(wd_module):
    """Pure cancel-cascade (every red-shaped entry is status=3) = green.
    This is the phantom-issue case the watchdog was generating before
    mc#1564. With the filter, no issue files."""
    red, failed = wd_module.is_red({
        "state": "failure",
        "statuses": [
            {"context": "ci/a", "status": "failure",
             "description": "Has been cancelled"},
            {"context": "ci/b", "status": "failure",
             "description": "Has been cancelled"},
        ],
    })
    assert red is False
    assert failed == []


def test_is_red_combined_failure_no_per_entry_still_red(wd_module):
    """Edge case: combined=failure with empty statuses[] — preserved
    from rev4 behaviour. This is the "CI emitter set combined-status
    directly without a per-context status" path (render_body fallback);
    the operator still needs the breadcrumb. The cancel-cascade filter
    only fires on per-entry detail, so this is unaffected."""
    red, failed = wd_module.is_red({"state": "failure", "statuses": []})
    assert red is True
    assert failed == []


def test_is_red_cancel_cascade_filter_exact_match_only(wd_module):
    """The cancel-cascade filter matches description EXACTLY (after
    strip) — substring would over-match (e.g. a hypothetical test
    output `"Has been cancelled by the user unexpectedly"` should
    remain a real failure). Locks down the contract."""
    red, failed = wd_module.is_red({
        "state": "failure",
        "statuses": [
            {"context": "ci/edge",
             "status": "failure",
             "description": "Has been cancelled by the user unexpectedly"},
        ],
    })
    assert red is True
    assert len(failed) == 1


def test_render_body_uses_status_key_for_per_entry_state(wd_module):
    """render_body must surface the per-entry `status` value in the
    issue body. Pre-rev4 it read `state` (always None on real Gitea) →
    every issue body said `(no state)`, defeating the diagnostic."""
    failed = [
        {"context": "ci/test", "status": "failure",
         "target_url": "https://example.test/run/1",
         "description": "broke"},
    ]
    body = wd_module.render_body("deadbeefcafe1234", failed, {})
    assert "`failure`" in body, (
        "render_body did not surface per-entry status — likely still "
        "reading `state` key only (rev1-3 bug)."
    )
    assert "(no state)" not in body


# --------------------------------------------------------------------------
# Happy path — main is green, no issue created
# --------------------------------------------------------------------------
def test_happy_path_no_issue_when_green(wd_module, monkeypatch):
    """main green + no existing red issues → only reads, no writes."""
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branches/main"): (200, _branches_response(SHA_GREEN)),
        ("GET", f"/repos/owner/repo/commits/{SHA_GREEN}/status"): (
            200, _combined_status("success", [
                {"context": "ci/test", "state": "success"},
            ]),
        ),
        ("GET", "/repos/owner/repo/issues"): (200, []),  # no open red issues
    })
    monkeypatch.setattr(wd_module, "api", stub)

    rc = wd_module.run_once(dry_run=False)
    assert rc == 0
    methods = [c[0] for c in stub.calls]
    assert "POST" not in methods, f"unexpected POST: {stub.calls}"
    assert "PATCH" not in methods, f"unexpected PATCH: {stub.calls}"


# --------------------------------------------------------------------------
# Red detected → issue opened with correct title + body
# --------------------------------------------------------------------------
def test_red_detected_opens_issue(wd_module, monkeypatch):
    """When main is red and no issue is open, POST a new one with the
    correct title; body lists each failed context."""
    failed_ctx = [
        {
            "context": "ci/test",
            "state": "failure",
            "target_url": "https://ci.example/run/42",
            "description": "1 test failed",
        },
        {
            "context": "ci/lint",
            "state": "error",
            "target_url": "https://ci.example/run/43",
            "description": "runner crashed",
        },
    ]
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branches/main"): (200, _branches_response(SHA_RED)),
        ("GET", f"/repos/owner/repo/commits/{SHA_RED}/status"): (
            200, _combined_status("failure", failed_ctx),
        ),
        ("GET", "/repos/owner/repo/issues"): (200, []),  # no existing issue
        ("POST", "/repos/owner/repo/issues"): (201, {"number": 555}),
        ("GET", "/repos/owner/repo/labels"): (
            200, [{"id": 9, "name": "ci-bp-drift"}],
        ),
        ("POST", "/repos/owner/repo/issues/555/labels"): (200, []),
    })
    monkeypatch.setattr(wd_module, "api", stub)

    wd_module.run_once(dry_run=False)

    # Find the POST call to create the issue and inspect its body.
    post_calls = [c for c in stub.calls if c[0] == "POST" and c[1] == "/repos/owner/repo/issues"]
    assert len(post_calls) == 1, post_calls
    posted_body = post_calls[0][2]
    expected_title = f"[main-red] owner/repo: {SHA_RED[:10]}"
    assert posted_body["title"] == expected_title
    body_text = posted_body["body"]
    assert "ci/test" in body_text
    assert "ci/lint" in body_text
    assert "1 test failed" in body_text
    assert "runner crashed" in body_text
    assert SHA_RED[:10] in body_text
    # Label apply attempted on the happy path:
    assert ("POST", "/repos/owner/repo/issues/555/labels") in [
        (c[0], c[1]) for c in stub.calls
    ]


# --------------------------------------------------------------------------
# Idempotent: existing issue is PATCHed, not duplicated
# --------------------------------------------------------------------------
def test_idempotent_existing_issue_patched_not_duplicated(wd_module, monkeypatch):
    """When an open `[main-red] {repo}: {SHA[:10]}` issue already exists
    for the current SHA, file_or_update_red PATCHes it. No POST."""
    existing_title = f"[main-red] owner/repo: {SHA_RED[:10]}"
    failed_ctx = [
        {"context": "ci/test", "state": "failure",
         "target_url": "https://x/y", "description": "boom"},
    ]
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branches/main"): (200, _branches_response(SHA_RED)),
        ("GET", f"/repos/owner/repo/commits/{SHA_RED}/status"): (
            200, _combined_status("failure", failed_ctx),
        ),
        ("GET", "/repos/owner/repo/issues"): (
            200, [{"number": 7, "title": existing_title}],
        ),
        ("PATCH", "/repos/owner/repo/issues/7"): (200, {"number": 7}),
    })
    monkeypatch.setattr(wd_module, "api", stub)

    wd_module.run_once(dry_run=False)

    methods_paths = [(c[0], c[1]) for c in stub.calls]
    assert ("PATCH", "/repos/owner/repo/issues/7") in methods_paths, stub.calls
    assert ("POST", "/repos/owner/repo/issues") not in methods_paths, (
        f"expected NO POST when issue exists (idempotent), got: {stub.calls}"
    )


# --------------------------------------------------------------------------
# Auto-close: main green at NEW_SHA → close issue for OLD_SHA
# --------------------------------------------------------------------------
def test_auto_close_when_main_returns_to_green(wd_module, monkeypatch):
    """main green at SHA_GREEN with an open `[main-red]` issue for
    SHA_RED → close the old issue with a 'returned to green' comment."""
    old_title = f"[main-red] owner/repo: {SHA_RED[:10]}"
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branches/main"): (200, _branches_response(SHA_GREEN)),
        ("GET", f"/repos/owner/repo/commits/{SHA_GREEN}/status"): (
            200, _combined_status("success", [
                {"context": "ci/test", "state": "success"},
            ]),
        ),
        ("GET", "/repos/owner/repo/issues"): (
            200, [{"number": 7, "title": old_title}],
        ),
        ("POST", "/repos/owner/repo/issues/7/comments"): (201, {"id": 100}),
        ("PATCH", "/repos/owner/repo/issues/7"): (200, {"number": 7, "state": "closed"}),
    })
    monkeypatch.setattr(wd_module, "api", stub)

    wd_module.run_once(dry_run=False)

    methods_paths = [(c[0], c[1]) for c in stub.calls]
    # Comment posted with reference to the new SHA
    assert ("POST", "/repos/owner/repo/issues/7/comments") in methods_paths
    comment_calls = [
        c for c in stub.calls
        if c[0] == "POST" and c[1] == "/repos/owner/repo/issues/7/comments"
    ]
    assert SHA_GREEN in comment_calls[0][2]["body"]
    # Issue closed via PATCH state=closed
    patch_calls = [
        c for c in stub.calls
        if c[0] == "PATCH" and c[1] == "/repos/owner/repo/issues/7"
    ]
    assert patch_calls[0][2] == {"state": "closed"}


def test_auto_close_skips_when_main_pending(wd_module, monkeypatch):
    """main pending (CI still running) at NEW_SHA → leave old issue alone.
    Pending could resolve to red, so closing prematurely would lose the
    breadcrumb of the prior red."""
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branches/main"): (200, _branches_response(SHA_GREEN)),
        ("GET", f"/repos/owner/repo/commits/{SHA_GREEN}/status"): (
            200, _combined_status("pending", [
                {"context": "ci/test", "state": "pending"},
            ]),
        ),
    })
    monkeypatch.setattr(wd_module, "api", stub)

    wd_module.run_once(dry_run=False)

    # No close-related calls
    methods_paths = [(c[0], c[1]) for c in stub.calls]
    assert ("PATCH", "/repos/owner/repo/issues/7") not in methods_paths
    assert ("GET", "/repos/owner/repo/issues") not in methods_paths


# --------------------------------------------------------------------------
# Stale-issue cleanup on transient / head-drift (internal#1789)
# --------------------------------------------------------------------------
def test_head_drift_closes_stale_issue_for_prior_sha(wd_module, monkeypatch):
    """Initial red at SHA_RED. Before recheck, main is force-pushed to
    SHA_NEW (different commit). watchdog must close the stale SHA_RED
    issue before returning — otherwise stale open issues accumulate
    when main is force-pushed during a red window."""
    stub = _make_stub_api({
        # Initial check: branch SHA_RED, status failure
        ("GET", "/repos/owner/repo/branches/main"): [
            (200, _branches_response(SHA_RED)),
            (200, _branch_alt(SHA_NEW)),      # recheck branch call → HEAD moved
            (200, _branch_alt(SHA_NEW)),      # close path branch call
        ],
        ("GET", f"/repos/owner/repo/commits/{SHA_RED}/status"): [
            (200, _combined_status("failure", [
                {"context": "ci/test", "status": "failure", "description": "broke"},
            ])),
            (200, _combined_status("success", [    # recheck: CI result arrived
                {"context": "ci/test", "status": "success"},
            ])),
        ],
        ("GET", f"/repos/owner/repo/commits/{SHA_NEW}/status"): [
            (200, _combined_status("success", [
                {"context": "ci/test", "status": "success"},
            ])),
        ],
        # close_open_red_issues_for_other_shas(SHA_NEW): issue for SHA_RED found
        ("GET", "/repos/owner/repo/issues"): [
            (200, [{"number": 9, "title": f"[main-red] owner/repo: {SHA_RED[:10]}"}]),
        ],
        ("POST", "/repos/owner/repo/issues/9/comments"): (201, {"id": 200}),
        ("PATCH", "/repos/owner/repo/issues/9"): (200, {"number": 9, "state": "closed"}),
    })
    monkeypatch.setattr(wd_module, "api", stub)
    rc = wd_module.run_once(dry_run=False)
    assert rc == 0
    methods_paths = [(c[0], c[1]) for c in stub.calls]
    assert ("PATCH", "/repos/owner/repo/issues/9") in methods_paths, \
        "head-drift should close the stale SHA_RED issue"


def test_recovery_on_same_sha_closes_issue_filed_on_prior_tick(wd_module, monkeypatch):
    """Same SHA shows red on initial check, but CI recovers before recheck
    completes. watchdog must close the issue that was filed on an earlier
    tick for this same SHA — otherwise stale open issues accumulate when CI
    recovers within the settling window."""
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branches/main"): (200, _branches_response(SHA_RED)),
        # Sequential: initial check → failure, recheck (≥2nd call) → success.
        # Using a list so Python dict keeps a single key (avoids overwrite).
        ("GET", f"/repos/owner/repo/commits/{SHA_RED}/status"): [
            (200, _combined_status("failure", [
                {"context": "ci/test", "status": "failure", "description": "broke"},
            ])),
            (200, _combined_status("success", [
                {"context": "ci/test", "state": "success"},
            ])),
        ],
        # List open red issues → find stale issue for this SHA
        ("GET", "/repos/owner/repo/issues"): (
            200, [{"number": 11, "title": f"[main-red] owner/repo: {SHA_RED[:10]}"}],
        ),
        ("POST", "/repos/owner/repo/issues/11/comments"): (201, {"id": 300}),
        ("PATCH", "/repos/owner/repo/issues/11"): (200, {"number": 11, "state": "closed"}),
    })
    monkeypatch.setattr(wd_module, "api", stub)
    rc = wd_module.run_once(dry_run=False)
    assert rc == 0
    methods_paths = [(c[0], c[1]) for c in stub.calls]
    assert ("PATCH", "/repos/owner/repo/issues/11") in methods_paths, \
        "recovery-on-same-SHA should close the stale issue"


# --------------------------------------------------------------------------
# HTTP-failure / api() raises — duplicate-write regression guard
# --------------------------------------------------------------------------
def test_find_open_issue_for_sha_raises_on_transient_error(wd_module, monkeypatch):
    """When the issue-search GET fails (transient 500),
    find_open_issue_for_sha must propagate ApiError, NOT return None.

    REGRESSION CLASS PROOF: a pre-fix implementation that returned
    `None` on api() failure would cause file_or_update_red to take the
    POST branch and create a duplicate issue. This test FAILS on that
    pre-fix code. Verified by temporarily replacing the script's
    `raise ApiError` with `return [], None` and rerunning — this case
    flips red.
    """
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/issues"): wd_module.ApiError(
            "GET /repos/owner/repo/issues → HTTP 500: gateway timeout"
        ),
    })
    monkeypatch.setattr(wd_module, "api", stub)
    with pytest.raises(wd_module.ApiError):
        wd_module.find_open_issue_for_sha(SHA_RED)


def test_list_open_red_issues_raises_on_transient_error(wd_module, monkeypatch):
    """Same contract for list_open_red_issues — close path must not
    silently skip on transient error."""
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/issues"): wd_module.ApiError(
            "GET /repos/owner/repo/issues → HTTP 502: bad gateway"
        ),
    })
    monkeypatch.setattr(wd_module, "api", stub)
    with pytest.raises(wd_module.ApiError):
        wd_module.list_open_red_issues()


def test_run_once_propagates_api_error_loudly(wd_module, monkeypatch):
    """Transient outage on branches read → ApiError propagates through
    run_once. The workflow run fails LOUDLY (correct behaviour); silent
    fallthrough would hide that the watchdog is broken."""
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branches/main"): wd_module.ApiError(
            "GET /repos/owner/repo/branches/main → HTTP 503: service unavailable"
        ),
    })
    monkeypatch.setattr(wd_module, "api", stub)
    with pytest.raises(wd_module.ApiError):
        wd_module.run_once(dry_run=False)


# --------------------------------------------------------------------------
# api() helper: raises on non-2xx
# --------------------------------------------------------------------------
def test_api_raises_on_non_2xx(wd_module, monkeypatch):
    """api() must raise ApiError on HTTP 500. This pins the
    `feedback_api_helper_must_raise_not_return_dict` contract — the
    duplicate-issue regression class depends on it."""

    def fake_urlopen(req, timeout=30):
        raise urllib.error.HTTPError(
            req.full_url, 500, "Internal Server Error", {}, None,  # type: ignore
        )

    monkeypatch.setattr(wd_module.urllib.request, "urlopen", fake_urlopen)

    with pytest.raises(wd_module.ApiError) as excinfo:
        wd_module.api("GET", "/repos/owner/repo/issues")
    assert "HTTP 500" in str(excinfo.value)


def test_api_raises_on_json_decode_when_expected(wd_module, monkeypatch):
    """api(expect_json=True) raises ApiError if body is not valid JSON.
    Closes the `{"_raw": ...}` fallthrough that callers misinterpret."""

    class FakeResp:
        status = 200

        def read(self):
            return b"not-json\n\n"

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    def fake_urlopen(req, timeout=30):
        return FakeResp()

    monkeypatch.setattr(wd_module.urllib.request, "urlopen", fake_urlopen)

    with pytest.raises(wd_module.ApiError):
        wd_module.api("GET", "/repos/owner/repo/issues")


def test_api_allows_raw_when_expect_json_false(wd_module, monkeypatch):
    """expect_json=False returns `{_raw: ...}` for known-quirky endpoints
    per `feedback_gitea_create_api_unparseable_response`. Opt-in."""

    class FakeResp:
        status = 201

        def read(self):
            return b"not-json-but-created\n"

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    def fake_urlopen(req, timeout=30):
        return FakeResp()

    monkeypatch.setattr(wd_module.urllib.request, "urlopen", fake_urlopen)
    status, body = wd_module.api(
        "POST", "/repos/owner/repo/issues", expect_json=False,
    )
    assert status == 201
    assert "_raw" in body


# --------------------------------------------------------------------------
# --dry-run flag — no side effects
# --------------------------------------------------------------------------
def test_dry_run_skips_writes(wd_module, monkeypatch, capsys):
    """--dry-run: detector runs, would-be title/body printed, but no
    POST/PATCH/comment calls are issued."""
    failed_ctx = [
        {"context": "ci/test", "state": "failure",
         "target_url": "https://x/y", "description": "boom"},
    ]
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branches/main"): (200, _branches_response(SHA_RED)),
        ("GET", f"/repos/owner/repo/commits/{SHA_RED}/status"): (
            200, _combined_status("failure", failed_ctx),
        ),
        ("GET", "/repos/owner/repo/issues"): (200, []),
    })
    monkeypatch.setattr(wd_module, "api", stub)

    wd_module.run_once(dry_run=True)

    methods = [c[0] for c in stub.calls]
    assert "POST" not in methods, f"dry-run made writes: {stub.calls}"
    assert "PATCH" not in methods, f"dry-run made writes: {stub.calls}"
    captured = capsys.readouterr()
    assert "[dry-run]" in captured.out
    assert "[main-red]" in captured.out  # title rendered


def test_dry_run_flag_parsed(wd_module):
    """--dry-run wired into argparse."""
    ns = wd_module._parse_args(["--dry-run"])
    assert ns.dry_run is True
    ns = wd_module._parse_args([])
    assert ns.dry_run is False


# --------------------------------------------------------------------------
# Title format
# --------------------------------------------------------------------------
def test_title_format_uses_short_sha(wd_module):
    """Title is `[main-red] {repo}: {SHA[:10]}` — stable idempotency key."""
    t = wd_module.title_for(SHA_RED)
    assert t == f"[main-red] owner/repo: {SHA_RED[:10]}"
    # exactly 10 chars of SHA
    assert SHA_RED[:10] in t
    assert SHA_RED[:11] not in t


def test_list_open_red_issues_filters_by_prefix(wd_module, monkeypatch):
    """list_open_red_issues only returns issues whose title starts with
    the expected prefix — unrelated open issues are not touched."""
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/issues"): (200, [
            {"number": 1, "title": f"[main-red] owner/repo: {SHA_RED[:10]}"},
            {"number": 2, "title": "Some unrelated bug"},
            {"number": 3, "title": "[ci-drift] owner/repo: divergence"},
            {"number": 4, "title": f"[main-red] owner/repo: {SHA_GREEN[:10]}"},
        ]),
    })
    monkeypatch.setattr(wd_module, "api", stub)
    out = wd_module.list_open_red_issues()
    assert [i["number"] for i in out] == [1, 4]


# --------------------------------------------------------------------------
# get_head_sha / get_combined_status data-shape guards
# --------------------------------------------------------------------------
def test_get_head_sha_raises_on_malformed_response(wd_module, monkeypatch):
    """If Gitea returns a body without `commit.id`, raise ApiError —
    do NOT proceed to file an issue with a bogus SHA."""
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branches/main"): (
            200, {"name": "main"},  # no commit object
        ),
    })
    monkeypatch.setattr(wd_module, "api", stub)
    with pytest.raises(wd_module.ApiError):
        wd_module.get_head_sha("main")


def test_get_head_sha_accepts_sha_field(wd_module, monkeypatch):
    """Older Gitea versions may return `commit.sha` instead of `commit.id`.
    Accept either — the watchdog must be tolerant to a documented shape
    variance."""
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branches/main"): (
            200, {"name": "main", "commit": {"sha": SHA_RED}},
        ),
    })
    monkeypatch.setattr(wd_module, "api", stub)
    assert wd_module.get_head_sha("main") == SHA_RED


# --------------------------------------------------------------------------
# Loki event emitter (best-effort, must not raise)
# --------------------------------------------------------------------------
def test_emit_loki_event_prints_json_line(wd_module, capsys, monkeypatch):
    """emit_loki_event always prints a JSON line to stdout (for workflow
    log capture) regardless of whether `logger` is installed."""
    # Force logger-not-found path to make the test deterministic.
    monkeypatch.setattr(wd_module.shutil, "which", lambda name: None)
    wd_module.emit_loki_event("main_red_detected", SHA_RED, ["ci/test"])
    captured = capsys.readouterr()
    assert "main-red-watchdog event:" in captured.out
    # Find the JSON payload after the prefix and verify it parses
    line = [ln for ln in captured.out.splitlines() if "main-red-watchdog event:" in ln][0]
    payload = json.loads(line.split("main-red-watchdog event:", 1)[1].strip())
    assert payload["event_type"] == "main_red_detected"
    assert payload["repo"] == "owner/repo"
    assert payload["sha"] == SHA_RED
    assert payload["failed_contexts"] == ["ci/test"]


def test_emit_loki_event_survives_logger_failure(wd_module, monkeypatch, capsys):
    """If `logger` is present but the subprocess call raises, the event
    emitter must NOT raise — emission is best-effort by contract."""
    monkeypatch.setattr(wd_module.shutil, "which", lambda name: "/usr/bin/logger")

    def boom(*a, **kw):
        raise OSError("logger pipe failed")
    monkeypatch.setattr(wd_module.subprocess, "run", boom)

    # Must not raise:
    wd_module.emit_loki_event("main_red_detected", SHA_RED, ["ci/test"])
    captured = capsys.readouterr()
    assert "logger call failed" in captured.err


# --------------------------------------------------------------------------
# Runtime env guard
# --------------------------------------------------------------------------
def test_require_runtime_env_exits_when_missing(wd_module, monkeypatch):
    """_require_runtime_env() exits with code 2 when any required env
    var is missing. Caught at main() entry, before any side-effecting
    API call."""
    monkeypatch.delenv("GITEA_TOKEN", raising=False)
    with pytest.raises(SystemExit) as excinfo:
        wd_module._require_runtime_env()
    assert excinfo.value.code == 2


# --------------------------------------------------------------------------
# Action-run status filter + HEAD-recheck (task #394, mc#1597..1630)
#
# The existing cancel-cascade filter matched description=='Has been
# cancelled' EXACTLY, but a 7-day DB sweep on 2026-05-20 showed that
# only 76/702 (~11%) of action_run.status=3 (Cancelled) entries carry
# that string — 89% are written as 'Failing after Ns', indistinguishable
# from real action_run.status=2 (Failure) at the commit_status layer.
#
# Gitea 1.22.6 has NO REST endpoint exposing action_run.status, so the
# canonical filter (status=2 only) cannot run from a Gitea Actions
# runner. The next-best signal is the HEAD-recheck: re-fetch HEAD SHA
# (or its combined status) right before filing. If HEAD moved on or
# combined state recovered, the prior "red" was a transient
# cancel-cascade and we skip-file.
#
# References:
#   - reference_chronic_red_sweep_cancelled_vs_failed_filter
#   - feedback_gitea_status_enum_use_helper_not_raw_int
#   - reference_gitea_action_status_enum_corrected_2026_05_19
#   - triage evidence 2026-05-21 04:55 (6 cancellation + 1 emission
#     artifact across mc#1597,1605,1609,1613,1626,1627,1630)
# --------------------------------------------------------------------------
def test_head_recheck_skips_file_when_head_moved(wd_module, monkeypatch, capsys):
    """When initial tick sees red at SHA_A but HEAD has since moved to
    SHA_B (next commit landed mid-tick), the watchdog must NOT file.
    Re-evaluation happens on the next cron tick against the new SHA.

    REGRESSION CLASS: this guards mc#1597..#1630 — 7 false-positives
    filed in 24h because cancel-cascade fired commit_status=failure
    rows on SHAs that were already superseded by new merges."""
    SHA_A = SHA_RED
    SHA_B = SHA_GREEN
    failed_ctx = [
        {"context": "ci/test", "status": "failure",
         "target_url": "/r/runs/100/jobs/0",
         "description": "Failing after 12s"},
    ]
    # First branches read returns SHA_A; the second (recheck) returns SHA_B
    # → watchdog detects HEAD drift and skip-files.
    branches_responses = iter([
        (200, _branches_response(SHA_A)),
        (200, _branches_response(SHA_B)),
    ])

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        if method == "GET" and path == "/repos/owner/repo/branches/main":
            return next(branches_responses)
        if method == "GET" and path == f"/repos/owner/repo/commits/{SHA_A}/status":
            return (200, _combined_status("failure", failed_ctx))
        if method == "POST" and path == "/repos/owner/repo/issues":
            raise AssertionError(
                "watchdog filed a phantom issue despite HEAD moving away "
                "from the red SHA (regression: mc#1597..1630)"
            )
        if method == "GET" and path == "/repos/owner/repo/issues":
            return (200, [])
        raise AssertionError(f"unexpected api call: {method} {path}")

    # Settling delay is no-op'd by the _stub_time_sleep autouse fixture.
    monkeypatch.setattr(wd_module, "api", fake_api)
    wd_module.run_once(dry_run=False)
    captured = capsys.readouterr()
    assert "head drift" in captured.out.lower() or "head moved" in captured.out.lower(), (
        f"expected a notice about HEAD drift, got: {captured.out!r}"
    )


def test_head_recheck_skips_file_when_recheck_status_recovered(
    wd_module, monkeypatch, capsys,
):
    """When initial tick sees red at SHA, but the post-settling recheck
    on the SAME SHA shows combined status recovered (e.g. transient
    cancel-cascade rolled forward to success on retry), skip-file.

    This catches the mid-flight cancel-cascade window — the second
    largest false-positive cluster in mc#1597..1630."""
    failed_ctx_initial = [
        {"context": "ci/test", "status": "failure",
         "target_url": "/r/runs/100/jobs/0",
         "description": "Failing after 12s"},
    ]
    recovered_ctx = [
        {"context": "ci/test", "status": "success",
         "target_url": "/r/runs/100/jobs/0",
         "description": "Successful in 30s"},
    ]
    # Same SHA across both branch reads; status flips from failure→success
    # between the two combined-status reads.
    status_responses = iter([
        (200, _combined_status("failure", failed_ctx_initial)),
        (200, _combined_status("success", recovered_ctx)),
    ])

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        if method == "GET" and path == "/repos/owner/repo/branches/main":
            return (200, _branches_response(SHA_RED))
        if method == "GET" and path == f"/repos/owner/repo/commits/{SHA_RED}/status":
            return next(status_responses)
        if method == "POST" and path == "/repos/owner/repo/issues":
            raise AssertionError(
                "watchdog filed a phantom issue despite combined status "
                "recovering on recheck (mid-flight cancel-cascade window)"
            )
        if method == "GET" and path == "/repos/owner/repo/issues":
            return (200, [])
        raise AssertionError(f"unexpected api call: {method} {path}")

    monkeypatch.setattr(wd_module, "api", fake_api)
    wd_module.run_once(dry_run=False)
    captured = capsys.readouterr()
    assert "recovered" in captured.out.lower() or "settled" in captured.out.lower(), (
        f"expected a notice about post-settling recovery, got: {captured.out!r}"
    )


def test_head_recheck_files_when_still_red_after_settling(
    wd_module, monkeypatch,
):
    """When BOTH the initial detection AND the post-settling recheck
    show the same SHA still red, file the issue. This is the genuine-
    failure path the watchdog is designed to surface.

    Locks the over-filter: a future change that always-skips after
    recheck would dismiss real failures."""
    failed_ctx = [
        {"context": "ci/test", "status": "failure",
         "target_url": "/r/runs/100/jobs/0",
         "description": "Failing after 12s"},
    ]
    post_filed = {"value": False}

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        if method == "GET" and path == "/repos/owner/repo/branches/main":
            return (200, _branches_response(SHA_RED))
        if method == "GET" and path == f"/repos/owner/repo/commits/{SHA_RED}/status":
            return (200, _combined_status("failure", failed_ctx))
        if method == "GET" and path == "/repos/owner/repo/issues":
            return (200, [])
        if method == "GET" and path == "/repos/owner/repo/labels":
            return (200, [{"id": 9, "name": "ci-bp-drift"}])
        if method == "POST" and path == "/repos/owner/repo/issues":
            post_filed["value"] = True
            return (201, {"number": 999})
        if method == "POST" and path == "/repos/owner/repo/issues/999/labels":
            return (200, [])
        raise AssertionError(f"unexpected api call: {method} {path}")

    monkeypatch.setattr(wd_module, "api", fake_api)
    wd_module.run_once(dry_run=False)
    assert post_filed["value"], (
        "genuine-failure path was skip-filed — head-recheck over-filter "
        "regression (would suppress all real main-red alarms)"
    )


def test_head_recheck_skips_when_initial_was_only_cancel_cascade(
    wd_module, monkeypatch,
):
    """Belt-and-braces: combined-status failure caused exclusively by
    description='Has been cancelled' entries should still be filtered
    by the EXISTING cancel-cascade filter — head-recheck must not
    accidentally bypass it. Regression guard for the existing mc#1564
    fix."""
    failed_ctx = [
        {"context": "ci/test", "status": "failure",
         "description": "Has been cancelled"},
    ]

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        if method == "GET" and path == "/repos/owner/repo/branches/main":
            return (200, _branches_response(SHA_RED))
        if method == "GET" and path == f"/repos/owner/repo/commits/{SHA_RED}/status":
            return (200, _combined_status("failure", failed_ctx))
        if method == "POST" and path == "/repos/owner/repo/issues":
            raise AssertionError(
                "cancel-cascade-only entry must be filtered before any "
                "head-recheck logic runs"
            )
        if method == "GET" and path == "/repos/owner/repo/issues":
            return (200, [])
        # No commit-status recheck should happen because is_red() returned False
        raise AssertionError(f"unexpected api call: {method} {path}")

    monkeypatch.setattr(wd_module, "api", fake_api)
    wd_module.run_once(dry_run=False)
    # success: no AssertionError raised, no POST


def test_resolve_action_run_status_returns_none_on_no_endpoint(wd_module):
    """The action_run.status REST endpoint does NOT exist in Gitea
    1.22.6 (verified empirically 2026-05-20 — /api/v1/.../actions/runs/N
    returns HTTP 404 across all probe variants). The resolver must
    return None gracefully so callers fall back to the description-
    string + head-recheck heuristics.

    This pins the extensibility hook: when a future Gitea release (or
    an op-host proxy) exposes the endpoint, the resolver implementation
    can be swapped in without touching the caller contract."""
    # The function exists and is callable
    assert hasattr(wd_module, "_resolve_action_run_status")
    # A typical target_url shape from real Gitea commit_status rows:
    target_url = "/molecule-ai/molecule-core/actions/runs/75020/jobs/0"
    # Return None when no endpoint available
    out = wd_module._resolve_action_run_status(target_url)
    assert out is None, (
        "resolver must return None when the action_run.status endpoint "
        "isn't reachable — callers depend on the None-fallback path"
    )
