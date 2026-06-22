"""Tests for `.gitea/scripts/ci-required-drift.py` — RFC internal#219 §4 + §6.

Covers the five drift-finding classes (F1, F1b, F2, F3a, F3b), the happy
path (no drift, no API mutation), and the idempotent path (existing
`[ci-drift]` issue is PATCHed in place, NOT duplicated).

Per the Five-Axis review on PR #112, the test suite must FAIL on the
pre-fix code where `find_open_issue()` returned `None` on transient
HTTP errors (causing the caller to POST a duplicate issue). We exercise
that path explicitly with `test_find_open_issue_raises_on_transient_error`.

Run:
    python3 -m pytest tests/test_ci_required_drift.py -v

Dependencies: stdlib + PyYAML (already required by the script itself).
No network. No live Gitea calls.
"""
from __future__ import annotations

import importlib.util
import json
import os
import textwrap
from pathlib import Path
from unittest import mock

import pytest


# --------------------------------------------------------------------------
# Module-import fixture
# --------------------------------------------------------------------------
# The script reads env vars at import-time (cheap globals, no IO). Tests
# set the env vars BEFORE importing so the module loads under a known
# config, then individual tests monkeypatch the `api()` callable and
# YAML file paths via tmp_path.
SCRIPT_PATH = (
    Path(__file__).resolve().parent.parent
    / ".gitea"
    / "scripts"
    / "ci-required-drift.py"
)


@pytest.fixture(scope="module")
def drift_module():
    """Import the script as a module. Env vars are pre-set so the
    module-level reads pass; tests then patch individual globals as
    needed."""
    env = {
        "GITEA_TOKEN": "fixture-token",
        "GITEA_HOST": "git.example.test",
        "REPO": "owner/repo",
        "BRANCHES": "main staging",
        "SENTINEL_JOB": "all-required",
        "AUDIT_WORKFLOW_PATH": ".gitea/workflows/audit-force-merge.yml",
        "CI_WORKFLOW_PATH": ".gitea/workflows/ci.yml",
        "DRIFT_LABEL": "ci-bp-drift",
    }
    with mock.patch.dict(os.environ, env, clear=False):
        spec = importlib.util.spec_from_file_location(
            "ci_required_drift", SCRIPT_PATH
        )
        m = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(m)
        # Force-set the globals from env (they were captured at import
        # time before our mock.patch.dict took effect on subsequent
        # runs in the same pytest session).
        m.GITEA_TOKEN = env["GITEA_TOKEN"]
        m.GITEA_HOST = env["GITEA_HOST"]
        m.REPO = env["REPO"]
        m.BRANCHES = env["BRANCHES"].split()
        m.SENTINEL_JOB = env["SENTINEL_JOB"]
        m.AUDIT_WORKFLOW_PATH = env["AUDIT_WORKFLOW_PATH"]
        m.CI_WORKFLOW_PATH = env["CI_WORKFLOW_PATH"]
        m.DRIFT_LABEL = env["DRIFT_LABEL"]
        m.OWNER, m.NAME = "owner", "repo"
        m.API = f"https://{env['GITEA_HOST']}/api/v1"
        yield m


# --------------------------------------------------------------------------
# Fixture YAML — minimal but realistic ci.yml + audit-force-merge.yml
# --------------------------------------------------------------------------
def _write_ci_yaml(tmp_path: Path, *, jobs: dict, sentinel_needs: list[str]) -> Path:
    """Write a synthetic ci.yml with the given jobs + sentinel needs."""
    full_jobs = dict(jobs)
    full_jobs["all-required"] = {"runs-on": "ubuntu-latest", "needs": sentinel_needs}
    doc = {"name": "ci", "on": {"pull_request": {}}, "jobs": full_jobs}
    import yaml
    p = tmp_path / "ci.yml"
    p.write_text(yaml.safe_dump(doc), encoding="utf-8")
    return p


def _write_audit_yaml(tmp_path: Path, required_checks: list[str]) -> Path:
    """Write a synthetic audit-force-merge.yml with REQUIRED_CHECKS env."""
    block = "\n".join(required_checks)
    text = textwrap.dedent(
        f"""\
        name: audit-force-merge
        on:
          schedule:
            - cron: '*/30 * * * *'
        jobs:
          audit:
            runs-on: ubuntu-latest
            steps:
              - name: Run audit
                env:
                  REQUIRED_CHECKS: |
                    {block.replace(chr(10), chr(10) + '                    ')}
                run: bash .gitea/scripts/audit-force-merge.sh
        """
    )
    p = tmp_path / "audit-force-merge.yml"
    p.write_text(text, encoding="utf-8")
    return p


def _write_audit_yaml_json(tmp_path: Path, required_checks_json: dict) -> Path:
    """Write a synthetic audit-force-merge.yml with REQUIRED_CHECKS_JSON env."""
    block = json.dumps(required_checks_json, indent=2)
    text = textwrap.dedent(
        f"""\
        name: audit-force-merge
        on:
          schedule:
            - cron: '*/30 * * * *'
        jobs:
          audit:
            runs-on: ubuntu-latest
            steps:
              - name: Run audit
                env:
                  REQUIRED_CHECKS_JSON: |
                    {block.replace(chr(10), chr(10) + '                    ')}
                run: bash .gitea/scripts/audit-force-merge.sh
        """
    )
    p = tmp_path / "audit-force-merge.yml"
    p.write_text(text, encoding="utf-8")
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
# Drift-class tests — pure detect_drift() coverage
# --------------------------------------------------------------------------
def _patch_paths(drift_module, monkeypatch, ci_yml: Path, audit_yml: Path):
    monkeypatch.setattr(drift_module, "CI_WORKFLOW_PATH", str(ci_yml))
    monkeypatch.setattr(drift_module, "AUDIT_WORKFLOW_PATH", str(audit_yml))


def test_f1_job_missing_from_sentinel_needs(drift_module, tmp_path, monkeypatch):
    """F1: a job exists in ci.yml but is NOT under sentinel.needs."""
    ci = _write_ci_yaml(
        tmp_path,
        jobs={
            "build": {"runs-on": "ubuntu-latest"},
            "test": {"runs-on": "ubuntu-latest"},  # missing from needs
        },
        sentinel_needs=["build"],
    )
    audit = _write_audit_yaml(tmp_path, ["ci / build (pull_request)"])
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {"status_check_contexts": ["ci / build (pull_request)"]},
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    findings, _ = drift_module.detect_drift("main")
    assert any("F1 —" in f and "test" in f for f in findings), findings


def test_detect_drift_403_fails_closed(drift_module, tmp_path, monkeypatch):
    """AUTH FAILURE on branch_protections (HTTP 401/403) → RAISE (fail
    closed). The token can't read BP, so drift is UNVERIFIABLE; greening
    the hourly cron here would let jobs↔protection drift go silently
    undetected — exactly the regression class this sentinel exists to
    catch. fix/core-ci-fail-closed.
    """
    ci = _write_ci_yaml(
        tmp_path,
        jobs={"build": {"runs-on": "ubuntu-latest"}},
        sentinel_needs=["build"],
    )
    audit = _write_audit_yaml(tmp_path, ["ci / build (pull_request)"])
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            drift_module.ApiError(
                "GET /repos/owner/repo/branch_protections/main → HTTP 403: forbidden"
            )
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)
    with pytest.raises(drift_module.ApiError):
        drift_module.detect_drift("main")


def test_detect_drift_404_skips_branch(drift_module, tmp_path, monkeypatch):
    """Authenticated 404 (branch genuinely has no protection, e.g. staging
    pre-rollout) → tolerated skip: return ([], debug) with
    protection_contexts_skipped True. NOT a fail-open (real read of an
    absent resource with a valid token)."""
    ci = _write_ci_yaml(
        tmp_path,
        jobs={"build": {"runs-on": "ubuntu-latest"}},
        sentinel_needs=["build"],
    )
    audit = _write_audit_yaml(tmp_path, ["ci / build (pull_request)"])
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/staging"): (
            drift_module.ApiError(
                "GET /repos/owner/repo/branch_protections/staging → HTTP 404: not found"
            )
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)
    findings, debug = drift_module.detect_drift("staging")
    assert findings == []
    assert debug.get("protection_contexts_skipped") is True
    assert debug.get("protection_http_status") == 404


def test_f1b_sentinel_needs_typo(drift_module, tmp_path, monkeypatch):
    """F1b: sentinel.needs lists a job not present in ci.yml (typo).

    Per the prior fix, F1b uses jobs_all (the unfiltered set) so that
    event-gated jobs aren't false-positive typos."""
    ci = _write_ci_yaml(
        tmp_path,
        jobs={"build": {"runs-on": "ubuntu-latest"}},
        sentinel_needs=["build", "bulid"],  # typo'd
    )
    audit = _write_audit_yaml(tmp_path, ["ci / build (pull_request)"])
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {"status_check_contexts": ["ci / build (pull_request)"]},
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    findings, _ = drift_module.detect_drift("main")
    assert any("F1b" in f and "bulid" in f for f in findings), findings


def test_f1b_event_gated_job_not_flagged_as_typo(drift_module, tmp_path, monkeypatch):
    """F1b regression guard: event-gated jobs (with `if: github.event_name`)
    are in jobs_all and must NOT trigger F1b when listed in sentinel.needs.
    They DO trigger F1 if missing — but that's a different finding."""
    ci = _write_ci_yaml(
        tmp_path,
        jobs={
            "build": {"runs-on": "ubuntu-latest"},
            "pr-only": {
                "runs-on": "ubuntu-latest",
                "if": "github.event_name == 'pull_request'",
            },
        },
        sentinel_needs=["build", "pr-only"],  # event-gated, but real
    )
    audit = _write_audit_yaml(
        tmp_path,
        ["ci / build (pull_request)", "ci / pr-only (pull_request)"],
    )
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {
                "status_check_contexts": [
                    "ci / build (pull_request)",
                    "ci / pr-only (pull_request)",
                ]
            },
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    findings, _ = drift_module.detect_drift("main")
    assert not any("F1b" in f for f in findings), findings


def test_f2_protection_has_no_emitter(drift_module, tmp_path, monkeypatch):
    """F2: a `ci / ` prefixed context in protection has no job in ci.yml."""
    ci = _write_ci_yaml(
        tmp_path,
        jobs={"build": {"runs-on": "ubuntu-latest"}},
        sentinel_needs=["build"],
    )
    audit = _write_audit_yaml(tmp_path, ["ci / build (pull_request)"])
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {
                "status_check_contexts": [
                    "ci / build (pull_request)",
                    "ci / removed-job (pull_request)",  # F2
                ]
            },
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    findings, _ = drift_module.detect_drift("main")
    assert any("F2" in f and "removed-job" in f for f in findings), findings


def test_f3a_env_wider_than_protection(drift_module, tmp_path, monkeypatch):
    """F3a: REQUIRED_CHECKS env has a context NOT in protection."""
    ci = _write_ci_yaml(
        tmp_path,
        jobs={"build": {"runs-on": "ubuntu-latest"}},
        sentinel_needs=["build"],
    )
    audit = _write_audit_yaml(
        tmp_path,
        [
            "ci / build (pull_request)",
            "ci / ghost (pull_request)",  # only in env
        ],
    )
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {"status_check_contexts": ["ci / build (pull_request)"]},
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    findings, _ = drift_module.detect_drift("main")
    assert any("F3a" in f and "ghost" in f for f in findings), findings


def test_f3b_protection_wider_than_env(drift_module, tmp_path, monkeypatch):
    """F3b: protection has a context NOT in REQUIRED_CHECKS env."""
    ci = _write_ci_yaml(
        tmp_path,
        jobs={
            "build": {"runs-on": "ubuntu-latest"},
            "test": {"runs-on": "ubuntu-latest"},
        },
        sentinel_needs=["build", "test"],
    )
    audit = _write_audit_yaml(tmp_path, ["ci / build (pull_request)"])
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {
                "status_check_contexts": [
                    "ci / build (pull_request)",
                    "ci / test (pull_request)",  # only in protection
                ]
            },
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    findings, _ = drift_module.detect_drift("main")
    assert any("F3b" in f and "ci / test (pull_request)" in f for f in findings), findings


def test_happy_path_no_drift(drift_module, tmp_path, monkeypatch):
    """Happy path: ci.yml ↔ protection ↔ audit env all in alignment."""
    ci = _write_ci_yaml(
        tmp_path,
        jobs={
            "build": {"runs-on": "ubuntu-latest"},
            "test": {"runs-on": "ubuntu-latest"},
        },
        sentinel_needs=["build", "test"],
    )
    audit = _write_audit_yaml(
        tmp_path,
        [
            "ci / build (pull_request)",
            "ci / test (pull_request)",
            "ci / all-required (pull_request)",
        ],
    )
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {
                "status_check_contexts": [
                    "ci / build (pull_request)",
                    "ci / test (pull_request)",
                    "ci / all-required (pull_request)",
                ]
            },
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    findings, _ = drift_module.detect_drift("main")
    assert findings == [], findings


# --------------------------------------------------------------------------
# REQUIRED_CHECKS_JSON variant drift tests
# --------------------------------------------------------------------------
def test_f3a_env_wider_than_protection_json_variant(drift_module, tmp_path, monkeypatch):
    """F3a: REQUIRED_CHECKS_JSON env has a context NOT in protection."""
    ci = _write_ci_yaml(
        tmp_path,
        jobs={"build": {"runs-on": "ubuntu-latest"}},
        sentinel_needs=["build"],
    )
    audit = _write_audit_yaml_json(
        tmp_path,
        {"main": ["ci / build (pull_request)", "ci / ghost (pull_request)"]},
    )
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {"status_check_contexts": ["ci / build (pull_request)"]},
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    findings, _ = drift_module.detect_drift("main")
    assert any("F3a" in f and "ghost" in f for f in findings), findings


def test_f3b_protection_wider_than_env_json_variant(drift_module, tmp_path, monkeypatch):
    """F3b: protection has a context NOT in REQUIRED_CHECKS_JSON env."""
    ci = _write_ci_yaml(
        tmp_path,
        jobs={
            "build": {"runs-on": "ubuntu-latest"},
            "test": {"runs-on": "ubuntu-latest"},
        },
        sentinel_needs=["build", "test"],
    )
    audit = _write_audit_yaml_json(
        tmp_path,
        {"main": ["ci / build (pull_request)"]},
    )
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {
                "status_check_contexts": [
                    "ci / build (pull_request)",
                    "ci / test (pull_request)",
                ]
            },
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    findings, _ = drift_module.detect_drift("main")
    assert any("F3b" in f and "ci / test (pull_request)" in f for f in findings), findings


def test_happy_path_no_drift_json_variant(drift_module, tmp_path, monkeypatch):
    """Happy path with REQUIRED_CHECKS_JSON: all aligned."""
    ci = _write_ci_yaml(
        tmp_path,
        jobs={
            "build": {"runs-on": "ubuntu-latest"},
            "test": {"runs-on": "ubuntu-latest"},
        },
        sentinel_needs=["build", "test"],
    )
    audit = _write_audit_yaml_json(
        tmp_path,
        {
            "main": [
                "ci / build (pull_request)",
                "ci / test (pull_request)",
                "ci / all-required (pull_request)",
            ]
        },
    )
    _patch_paths(drift_module, monkeypatch, ci, audit)

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/branch_protections/main"): (
            200,
            {
                "status_check_contexts": [
                    "ci / build (pull_request)",
                    "ci / test (pull_request)",
                    "ci / all-required (pull_request)",
                ]
            },
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    findings, _ = drift_module.detect_drift("main")
    assert findings == [], findings


# --------------------------------------------------------------------------
# MUST-FIX 1: find_open_issue must raise on transient HTTP errors
# --------------------------------------------------------------------------
def test_find_open_issue_returns_none_on_no_match(drift_module, monkeypatch):
    """Search succeeded, no match → return None (the OK path)."""
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/issues"): (200, []),
    })
    monkeypatch.setattr(drift_module, "api", stub)
    assert drift_module.find_open_issue("[ci-drift] foo") is None


def test_find_open_issue_returns_match(drift_module, monkeypatch):
    """Search succeeded, matching issue exists → return it."""
    issue = {"number": 42, "title": "[ci-drift] foo"}
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/issues"): (200, [issue]),
    })
    monkeypatch.setattr(drift_module, "api", stub)
    assert drift_module.find_open_issue("[ci-drift] foo") == issue


def test_find_open_issue_raises_on_transient_error(drift_module, monkeypatch):
    """Search FAILED (HTTP 500) → raise ApiError, do NOT return None.

    This is the regression class from PR #112's Five-Axis review:
    returning None caused file_or_update() to take the else branch and
    POST a duplicate issue. The fix is for api() to raise; tests pin
    that contract by exercising the failure path explicitly.
    """
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/issues"): drift_module.ApiError(
            "GET /repos/owner/repo/issues → HTTP 500: gateway timeout"
        ),
    })
    monkeypatch.setattr(drift_module, "api", stub)
    with pytest.raises(drift_module.ApiError):
        drift_module.find_open_issue("[ci-drift] foo")


# --------------------------------------------------------------------------
# Pagination: search beyond page 1 so an existing issue on any page is found
# --------------------------------------------------------------------------
def test_find_open_issue_paginates_to_page_2(drift_module, monkeypatch):
    """Issue exists on page 2 → paginate and find it."""
    target = {"number": 99, "title": "[ci-drift] foo"}
    filler = [{"number": i, "title": f"other-{i}"} for i in range(1, 51)]

    class PaginatedStub:
        def __init__(self):
            self.calls = []

        def __call__(self, method, path, *, body=None, query=None, expect_json=True):
            self.calls.append((method, path, body, query))
            page = int((query or {}).get("page", "1"))
            if page == 1:
                return 200, filler
            if page == 2:
                return 200, [target]
            return 200, []

    stub = PaginatedStub()
    monkeypatch.setattr(drift_module, "api", stub)
    assert drift_module.find_open_issue("[ci-drift] foo") == target
    assert len(stub.calls) == 2


def test_find_open_issue_stops_at_last_page(drift_module, monkeypatch):
    """No match across pages → stop when a page has <50 results."""
    filler = [{"number": i, "title": f"other-{i}"} for i in range(1, 51)]

    class PaginatedStub:
        def __init__(self):
            self.calls = []

        def __call__(self, method, path, *, body=None, query=None, expect_json=True):
            self.calls.append((method, path, body, query))
            page = int((query or {}).get("page", "1"))
            if page == 1:
                return 200, filler
            return 200, []

    stub = PaginatedStub()
    monkeypatch.setattr(drift_module, "api", stub)
    assert drift_module.find_open_issue("[ci-drift] foo") is None
    assert len(stub.calls) == 2


# --------------------------------------------------------------------------
# Idempotent path: existing issue is PATCHed, NOT duplicated
# --------------------------------------------------------------------------
def test_file_or_update_patches_existing_issue(drift_module, monkeypatch):
    """When an open `[ci-drift]` issue exists, file_or_update PATCHes it
    and does NOT POST a duplicate."""
    title = drift_module.title_for("main")
    issue = {"number": 7, "title": title}

    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/issues"): (200, [issue]),
        ("PATCH", "/repos/owner/repo/issues/7"): (200, {"number": 7}),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    drift_module.file_or_update(
        "main",
        ["F2 — ci / removed-job (pull_request) has no emitter"],
        {"branch": "main"},
    )

    methods = [c[0] for c in stub.calls]
    assert "PATCH" in methods, stub.calls
    assert "POST" not in methods, (
        f"expected NO POST when issue exists (idempotent path), got: {stub.calls}"
    )


def test_file_or_update_posts_new_issue_when_none_exists(drift_module, monkeypatch):
    """When no open `[ci-drift]` issue exists, file_or_update POSTs one."""
    stub = _make_stub_api({
        ("GET", "/repos/owner/repo/issues"): (200, []),
        ("POST", "/repos/owner/repo/issues"): (201, {"number": 99}),
        ("GET", "/repos/owner/repo/labels"): (200, [{"id": 10, "name": "ci-bp-drift"}]),
        ("POST", "/repos/owner/repo/issues/99/labels"): (200, []),
    })
    monkeypatch.setattr(drift_module, "api", stub)

    drift_module.file_or_update(
        "main",
        ["F2 — ci / removed-job (pull_request) has no emitter"],
        {"branch": "main"},
    )

    methods_paths = [(c[0], c[1]) for c in stub.calls]
    assert ("POST", "/repos/owner/repo/issues") in methods_paths, stub.calls
    # Label apply is best-effort but should be attempted on the happy path:
    assert ("POST", "/repos/owner/repo/issues/99/labels") in methods_paths, stub.calls


# --------------------------------------------------------------------------
# --dry-run flag
# --------------------------------------------------------------------------
def test_dry_run_skips_all_api_writes(drift_module, monkeypatch, capsys):
    """--dry-run: detector still runs, but no GET/POST/PATCH issue calls."""
    stub = _make_stub_api({})  # any api call would assert
    monkeypatch.setattr(drift_module, "api", stub)

    drift_module.file_or_update(
        "main",
        ["F2 — ci / removed-job (pull_request) has no emitter"],
        {"branch": "main"},
        dry_run=True,
    )

    assert stub.calls == [], f"dry-run must not call api(), got: {stub.calls}"
    captured = capsys.readouterr()
    assert "[dry-run]" in captured.out
    assert "[ci-drift]" in captured.out  # title rendered to stdout


def test_dry_run_flag_parsed(drift_module):
    """--dry-run is wired into argparse."""
    ns = drift_module._parse_args(["--dry-run"])
    assert ns.dry_run is True
    ns = drift_module._parse_args([])
    assert ns.dry_run is False


# --------------------------------------------------------------------------
# api() helper: raises on non-2xx + on JSON-decode failure when expected
# --------------------------------------------------------------------------
def test_api_raises_on_non_2xx(drift_module, monkeypatch):
    """api() must raise ApiError on HTTP 500 — the duplicate-issue
    regression class from PR #112's review depends on this."""
    class FakeHTTPError(Exception):
        def __init__(self):
            self.code = 500
        def read(self):
            return b"internal server error"

    def fake_urlopen(req, timeout=30):
        import urllib.error
        raise urllib.error.HTTPError(
            req.full_url, 500, "Internal Server Error", {}, None  # type: ignore
        )

    monkeypatch.setattr(drift_module.urllib.request, "urlopen", fake_urlopen)

    with pytest.raises(drift_module.ApiError) as excinfo:
        drift_module.api("GET", "/repos/owner/repo/issues")
    assert "HTTP 500" in str(excinfo.value)


def test_api_raises_api_error_on_url_error(drift_module, monkeypatch):
    """api() must map URLError (network/DNS/connection-refused) to ApiError.

    A Gitea brown-out surfaces as urllib.error.URLError and previously
    crashed the cron with an unhandled stack trace. Failing soft via
    ApiError lets the hourly retry recover instead of spamming duplicates.
    """

    def fake_urlopen(req, timeout=30):
        import urllib.error
        raise urllib.error.URLError("connection refused during brown-out")

    monkeypatch.setattr(drift_module.urllib.request, "urlopen", fake_urlopen)

    with pytest.raises(drift_module.ApiError) as excinfo:
        drift_module.api("GET", "/repos/owner/repo/issues")
    assert "network error" in str(excinfo.value)


def test_api_raises_on_json_decode_when_expected(drift_module, monkeypatch):
    """api(expect_json=True) raises ApiError if body is not valid JSON.

    This closes the prior `{"_raw": ...}` fallthrough that callers
    could misinterpret as "JSON response with one key called _raw".
    """
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

    monkeypatch.setattr(drift_module.urllib.request, "urlopen", fake_urlopen)

    with pytest.raises(drift_module.ApiError):
        drift_module.api("GET", "/repos/owner/repo/issues")


def test_api_allows_raw_when_expect_json_false(drift_module, monkeypatch):
    """api(expect_json=False) returns the `_raw` fallthrough for endpoints
    with known echo-quirks (Gitea create responses). Reserved opt-in."""
    class FakeResp:
        status = 201
        def read(self):
            return b"not-json-but-create-succeeded\n"
        def __enter__(self):
            return self
        def __exit__(self, *a):
            return False

    def fake_urlopen(req, timeout=30):
        return FakeResp()

    monkeypatch.setattr(drift_module.urllib.request, "urlopen", fake_urlopen)
    status, body = drift_module.api(
        "POST", "/repos/owner/repo/issues", expect_json=False
    )
    assert status == 201
    assert "_raw" in body
