import importlib.util
import json
import sys
from pathlib import Path
from unittest.mock import patch

SCRIPT = Path(__file__).resolve().parents[1] / "ci-required-drift.py"
spec = importlib.util.spec_from_file_location("ci_required_drift", SCRIPT)
drift = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = drift
spec.loader.exec_module(drift)

# Module-level constants are loaded from env at import time; set them
# explicitly so unit tests can import without the full env contract.
drift.SENTINEL_JOB = "all-required"
drift.CI_WORKFLOW_PATH = ".gitea/workflows/ci.yml"
drift.AUDIT_WORKFLOW_PATH = ".gitea/workflows/audit-force-merge.yml"


# ---------------------------------------------------------------------------
# Helper fixtures
# ---------------------------------------------------------------------------

def _make_ci_doc(jobs: dict) -> dict:
    return {"jobs": jobs}


def _make_audit_doc(required_checks: list[str]) -> dict:
    return {
        "jobs": {
            "audit": {
                "steps": [
                    {"env": {"REQUIRED_CHECKS": "\n".join(required_checks)}}
                ]
            }
        }
    }


def _make_audit_doc_json(required_checks_json: dict) -> dict:
    return {
        "jobs": {
            "audit": {
                "steps": [
                    {"env": {"REQUIRED_CHECKS_JSON": json.dumps(required_checks_json)}}
                ]
            }
        }
    }


# ---------------------------------------------------------------------------
# required_checks_env — dual-variant parsing
# ---------------------------------------------------------------------------

def test_required_checks_env_prefers_json_over_legacy():
    doc = {
        "jobs": {
            "audit": {
                "steps": [
                    {
                        "env": {
                            "REQUIRED_CHECKS_JSON": json.dumps(
                                {"main": ["ctx-a"], "staging": ["ctx-b"]}
                            ),
                            "REQUIRED_CHECKS": "ctx-legacy\nctx-old",
                        }
                    }
                ]
            }
        }
    }
    assert drift.required_checks_env(doc, "main") == {"ctx-a"}
    assert drift.required_checks_env(doc, "staging") == {"ctx-b"}


def test_required_checks_env_falls_back_to_legacy():
    doc = _make_audit_doc(["legacy-ctx"])
    assert drift.required_checks_env(doc, "main") == {"legacy-ctx"}


def test_required_checks_env_json_missing_branch_fails():
    doc = _make_audit_doc_json({"staging": ["ctx-b"]})
    try:
        drift.required_checks_env(doc, "main")
    except SystemExit as exc:
        assert exc.code == 3
    else:
        raise AssertionError("expected SystemExit(3)")


def test_required_checks_env_json_malformed_fails():
    doc = {
        "jobs": {
            "audit": {
                "steps": [
                    {"env": {"REQUIRED_CHECKS_JSON": "not-json"}}
                ]
            }
        }
    }
    try:
        drift.required_checks_env(doc, "main")
    except SystemExit as exc:
        assert exc.code == 3
    else:
        raise AssertionError("expected SystemExit(3)")


def test_required_checks_env_json_non_string_item_fails():
    doc = _make_audit_doc_json({"main": ["ctx-a", 123, "ctx-b"]})
    try:
        drift.required_checks_env(doc, "main")
    except SystemExit as exc:
        assert exc.code == 3
    else:
        raise AssertionError("expected SystemExit(3)")


def test_required_checks_env_json_empty_string_item_fails():
    doc = _make_audit_doc_json({"main": ["ctx-a", "   ", "ctx-b"]})
    try:
        drift.required_checks_env(doc, "main")
    except SystemExit as exc:
        assert exc.code == 3
    else:
        raise AssertionError("expected SystemExit(3)")


def test_required_checks_env_json_duplicate_context_fails():
    doc = _make_audit_doc_json({"main": ["ctx-a", "ctx-b", "ctx-a"]})
    try:
        drift.required_checks_env(doc, "main")
    except SystemExit as exc:
        assert exc.code == 3
    else:
        raise AssertionError("expected SystemExit(3)")


# ---------------------------------------------------------------------------
# sentinel_needs
# ---------------------------------------------------------------------------

def test_sentinel_needs_returns_empty_when_absent():
    doc = _make_ci_doc({"all-required": {"runs-on": "ubuntu-latest"}})
    assert drift.sentinel_needs(doc) == set()


def test_sentinel_needs_parses_list():
    doc = _make_ci_doc(
        {"all-required": {"needs": ["platform-build", "canvas-build"]}}
    )
    assert drift.sentinel_needs(doc) == {"platform-build", "canvas-build"}


def test_sentinel_needs_parses_string():
    doc = _make_ci_doc({"all-required": {"needs": "platform-build"}})
    assert drift.sentinel_needs(doc) == {"platform-build"}


# ---------------------------------------------------------------------------
# ci_job_names / ci_jobs_all
# ---------------------------------------------------------------------------

def test_ci_job_names_excludes_sentinel_and_event_gated():
    doc = _make_ci_doc(
        {
            "platform-build": {},
            "canvas-build": {"if": "github.event_name == 'pull_request'"},
            "main-push": {"if": "github.ref == 'refs/heads/main'"},
            "all-required": {},
        }
    )
    assert drift.ci_job_names(doc) == {"platform-build"}


def test_ci_jobs_all_includes_event_gated():
    doc = _make_ci_doc(
        {
            "platform-build": {},
            "canvas-build": {"if": "github.event_name == 'pull_request'"},
            "all-required": {},
        }
    )
    assert drift.ci_jobs_all(doc) == {"platform-build", "canvas-build"}


# ---------------------------------------------------------------------------
# detect_drift — F1 / F1b with mocked I/O
# ---------------------------------------------------------------------------

SAMPLE_PROTECTION = {
    "status_check_contexts": [
        "CI / all-required (pull_request)",
        "Secret scan / Scan diff for credential-shaped strings (pull_request)",
    ]
}


def test_detect_drift_no_needs_sentinel_skips_f1():
    """Post-#1766 contract: all-required has no needs: → F1 is a false positive."""
    ci = _make_ci_doc(
        {
            "platform-build": {},
            "canvas-build": {},
            "all-required": {},
        }
    )
    audit = _make_audit_doc(
        [
            "CI / all-required (pull_request)",
            "Secret scan / Scan diff for credential-shaped strings (pull_request)",
        ]
    )

    with patch.object(drift, "load_yaml", side_effect=[ci, audit]):
        with patch.object(drift, "api", return_value=(200, SAMPLE_PROTECTION)):
            findings, debug = drift.detect_drift("main")

    assert findings == []
    assert debug["sentinel_needs"] == []


def test_detect_drift_typo_in_needs_triggers_f1b():
    """F1b still catches typos when needs exists."""
    ci = _make_ci_doc(
        {
            "platform-build": {},
            "all-required": {"needs": ["platfom-build"]},  # typo
        }
    )
    audit = _make_audit_doc(["CI / all-required (pull_request)"])

    with patch.object(drift, "load_yaml", side_effect=[ci, audit]):
        with patch.object(drift, "api", return_value=(200, SAMPLE_PROTECTION)):
            findings, _ = drift.detect_drift("main")

    assert any("F1b" in f for f in findings)
    assert any("platfom-build" in f for f in findings)


def test_detect_drift_missing_job_in_needs_triggers_f1():
    """F1 still fires when needs is non-empty and jobs are missing."""
    ci = _make_ci_doc(
        {
            "platform-build": {},
            "canvas-build": {},
            "all-required": {"needs": ["platform-build"]},
        }
    )
    audit = _make_audit_doc(["CI / all-required (pull_request)"])

    with patch.object(drift, "load_yaml", side_effect=[ci, audit]):
        with patch.object(drift, "api", return_value=(200, SAMPLE_PROTECTION)):
            findings, _ = drift.detect_drift("main")

    assert any("F1 —" in f for f in findings)
    assert any("canvas-build" in f for f in findings)
    assert not any("F1b" in f for f in findings)


def test_detect_drift_no_f1_when_needs_empty_even_with_jobs():
    """Explicit regression guard: empty needs + existing jobs = no F1."""
    ci = _make_ci_doc(
        {
            "platform-build": {},
            "canvas-build": {},
            "all-required": {"needs": []},
        }
    )
    audit = _make_audit_doc(["CI / all-required (pull_request)"])

    with patch.object(drift, "load_yaml", side_effect=[ci, audit]):
        with patch.object(drift, "api", return_value=(200, SAMPLE_PROTECTION)):
            findings, _ = drift.detect_drift("main")

    assert not any("F1 —" in f for f in findings)


# ---------------------------------------------------------------------------
# F4 — cross-workflow required-context emitter existence
# (closes the `CI / all-required` name-vs-coverage hole: the sentinel is
#  fail-closed over CI's own jobs but CANNOT cover sibling required
#  workflows — Gitea has no cross-workflow `needs:` — so F4 guarantees each
#  BP-required context still has a live emitting workflow.)
# ---------------------------------------------------------------------------

def test_workflow_emitted_contexts_uses_job_name_over_key():
    """Job `name:` wins over key; missing name falls back to key."""
    doc = {
        "name": "E2E API Smoke Test",
        "jobs": {
            "detect-changes": {},  # no name -> key
            "e2e-api": {"name": "E2E API Smoke Test"},
        },
    }
    got = drift.workflow_emitted_contexts(doc)
    assert got == {
        "E2E API Smoke Test / detect-changes (pull_request)",
        "E2E API Smoke Test / E2E API Smoke Test (pull_request)",
    }


def test_workflow_emitted_contexts_empty_when_no_name():
    """A workflow with no top-level `name:` emits nothing F4 can match."""
    assert drift.workflow_emitted_contexts({"jobs": {"x": {}}}) == set()


def test_all_emitted_contexts_unions_workflow_dir(tmp_path):
    """all_emitted_contexts globs *.yml and unions their emitter sets."""
    wf = tmp_path / "wf"
    wf.mkdir()
    (wf / "a.yml").write_text(
        "name: CI\njobs:\n  all-required:\n    runs-on: x\n", encoding="utf-8"
    )
    (wf / "b.yml").write_text(
        "name: Handlers Postgres Integration\n"
        "jobs:\n  integration:\n    name: Handlers Postgres Integration\n"
        "    runs-on: x\n",
        encoding="utf-8",
    )
    got = drift.all_emitted_contexts(str(wf))
    assert "CI / all-required (pull_request)" in got
    assert "Handlers Postgres Integration / Handlers Postgres Integration (pull_request)" in got


def test_all_emitted_contexts_skips_unparseable(tmp_path):
    """A single broken sibling workflow must not blind F4 to the rest."""
    wf = tmp_path / "wf"
    wf.mkdir()
    (wf / "good.yml").write_text("name: CI\njobs:\n  j:\n    runs-on: x\n", encoding="utf-8")
    (wf / "bad.yml").write_text("name: [unterminated\n  : : :\n", encoding="utf-8")
    got = drift.all_emitted_contexts(str(wf))
    assert "CI / j (pull_request)" in got


# A BP fixture that includes the two cross-workflow required contexts.
_BP_WITH_SIBLINGS = {
    "status_check_contexts": [
        "CI / all-required (pull_request)",
        "E2E API Smoke Test / E2E API Smoke Test (pull_request)",
        "Handlers Postgres Integration / Handlers Postgres Integration (pull_request)",
    ]
}

# The matching set of repo-wide emitted contexts (what a correct repo produces).
_EMITTED_OK = {
    "CI / all-required (pull_request)",
    "E2E API Smoke Test / E2E API Smoke Test (pull_request)",
    "Handlers Postgres Integration / Handlers Postgres Integration (pull_request)",
}


def test_detect_drift_f4_silent_when_all_contexts_emitted():
    """No F4 when every BP context has a live emitting workflow."""
    ci = _make_ci_doc({"all-required": {}})
    audit = _make_audit_doc(sorted(_BP_WITH_SIBLINGS["status_check_contexts"]))
    with patch.object(drift, "load_yaml", side_effect=[ci, audit]):
        with patch.object(drift, "api", return_value=(200, _BP_WITH_SIBLINGS)):
            with patch.object(drift, "all_emitted_contexts", return_value=set(_EMITTED_OK)):
                findings, debug = drift.detect_drift("main")
    assert not any("F4 —" in f for f in findings)
    assert debug["repo_emitted_contexts"] == sorted(_EMITTED_OK)


def test_detect_drift_f4_fires_on_stale_cross_workflow_context():
    """The core gate-hole regression: BP requires a cross-workflow context
    (e.g. a renamed/deleted sibling workflow) that NO workflow emits.
    F4 must fire — this is the inverse-of-F2 hole that makes a red PR look
    mergeable if BP is ever trimmed/renamed around `CI / all-required`."""
    ci = _make_ci_doc({"all-required": {}})
    audit = _make_audit_doc(sorted(_BP_WITH_SIBLINGS["status_check_contexts"]))
    # Handlers workflow got renamed -> its OLD BP context now has no emitter.
    emitted_after_rename = {
        "CI / all-required (pull_request)",
        "E2E API Smoke Test / E2E API Smoke Test (pull_request)",
        # Handlers context absent (renamed away)
    }
    with patch.object(drift, "load_yaml", side_effect=[ci, audit]):
        with patch.object(drift, "api", return_value=(200, _BP_WITH_SIBLINGS)):
            with patch.object(drift, "all_emitted_contexts", return_value=emitted_after_rename):
                findings, _ = drift.detect_drift("main")
    assert any("F4 —" in f for f in findings)
    assert any("Handlers Postgres Integration" in f for f in findings)


def test_detect_drift_f4_catches_all_required_only_trim():
    """If BP is trimmed to JUST `CI / all-required` but E2E/Handlers are
    still real workflows, F4 does NOT fire (no stale context) — but F3b
    (env vs BP) / operator policy must keep them required. This asserts F4
    does not false-positive on a correctly-emitted lone context."""
    bp = {"status_check_contexts": ["CI / all-required (pull_request)"]}
    ci = _make_ci_doc({"all-required": {}})
    audit = _make_audit_doc(["CI / all-required (pull_request)"])
    with patch.object(drift, "load_yaml", side_effect=[ci, audit]):
        with patch.object(drift, "api", return_value=(200, bp)):
            with patch.object(drift, "all_emitted_contexts", return_value=set(_EMITTED_OK)):
                findings, _ = drift.detect_drift("main")
    assert not any("F4 —" in f for f in findings)
