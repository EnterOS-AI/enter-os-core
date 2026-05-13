"""Unit tests for .gitea/scripts/lint_pre_flip_continue_on_error.py.

These tests pin the pure-logic surface (flip detection + per-flip
verdict aggregation) without making real HTTP calls. The end-to-end
git ls-tree + Gitea API path is exercised by running the workflow
against real PRs.

Run locally::

    python3 -m unittest .gitea/scripts/tests/test_lint_pre_flip_continue_on_error.py -v

Mirrors the pattern in scripts/ops/test_check_migration_collisions.py
+ scripts/test_build_runtime_package.py.
"""
from __future__ import annotations

import importlib.util
import os
import sys
import unittest
from pathlib import Path
from unittest import mock

# Load the script as a module without invoking main(). Tests must NOT
# depend on the full runtime env contract (GITEA_TOKEN etc.), so we
# import individual functions and stub the network surface explicitly.
SCRIPT_PATH = Path(__file__).resolve().parent.parent / "lint_pre_flip_continue_on_error.py"
spec = importlib.util.spec_from_file_location("lpfc", SCRIPT_PATH)
lpfc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lpfc)


# --------------------------------------------------------------------------
# Fixtures: minimal valid workflow YAML on each side of a "diff"
# --------------------------------------------------------------------------
CI_YML_BASE = """\
name: CI
on:
  push:
    branches: [main]
jobs:
  platform-build:
    name: Platform (Go)
    runs-on: ubuntu-latest
    continue-on-error: true
    steps:
      - run: echo platform
  canvas-build:
    name: Canvas (Next.js)
    runs-on: ubuntu-latest
    continue-on-error: true
    steps:
      - run: echo canvas
  all-required:
    runs-on: ubuntu-latest
    continue-on-error: true
    needs: [platform-build, canvas-build]
    steps:
      - run: echo ok
"""

CI_YML_HEAD_FLIPPED = """\
name: CI
on:
  push:
    branches: [main]
jobs:
  platform-build:
    name: Platform (Go)
    runs-on: ubuntu-latest
    continue-on-error: false
    steps:
      - run: echo platform
  canvas-build:
    name: Canvas (Next.js)
    runs-on: ubuntu-latest
    continue-on-error: false
    steps:
      - run: echo canvas
  all-required:
    runs-on: ubuntu-latest
    continue-on-error: true
    needs: [platform-build, canvas-build]
    steps:
      - run: echo ok
"""

CI_YML_HEAD_NO_DIFF = CI_YML_BASE  # identical to base, no flip


# --------------------------------------------------------------------------
# 1. CoE coercion (truthy/falsy/quoted/absent)
# --------------------------------------------------------------------------
class TestCoerceCoE(unittest.TestCase):
    def test_python_bool_true(self):
        self.assertTrue(lpfc._coerce_coe(True))

    def test_python_bool_false(self):
        self.assertFalse(lpfc._coerce_coe(False))

    def test_none_is_false(self):
        # GitHub Actions default: absent == false.
        self.assertFalse(lpfc._coerce_coe(None))

    def test_string_true_lowercase(self):
        # Quoted "true" in YAML — Gitea Actions normalizes to True.
        self.assertTrue(lpfc._coerce_coe("true"))

    def test_string_True_titlecase(self):
        self.assertTrue(lpfc._coerce_coe("True"))

    def test_string_yes(self):
        # YAML 1.1 truthy form.
        self.assertTrue(lpfc._coerce_coe("yes"))

    def test_string_false(self):
        self.assertFalse(lpfc._coerce_coe("false"))

    def test_string_random_falsy(self):
        # An unrecognized string is treated as falsy — safer than
        # silently coercing "maybe" to True and false-positiving a
        # flip.
        self.assertFalse(lpfc._coerce_coe("maybe"))


# --------------------------------------------------------------------------
# 2. Diff detection — flips, not arbitrary changes
# --------------------------------------------------------------------------
class TestDetectFlips(unittest.TestCase):
    def test_no_flip_in_diff_passes(self):
        # Acceptance test #1: PR doesn't flip continue-on-error → 0 flips.
        flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": CI_YML_HEAD_NO_DIFF},
        )
        self.assertEqual(flips, [])

    def test_flip_detected_in_one_file(self):
        flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": CI_YML_HEAD_FLIPPED},
        )
        # Two jobs flipped: platform-build, canvas-build. all-required
        # is still true on both sides.
        self.assertEqual(len(flips), 2)
        keys = sorted(f["job_key"] for f in flips)
        self.assertEqual(keys, ["canvas-build", "platform-build"])

    def test_context_name_render(self):
        flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": CI_YML_HEAD_FLIPPED},
        )
        platform = next(f for f in flips if f["job_key"] == "platform-build")
        self.assertEqual(platform["context"], "CI / Platform (Go) (push)")
        self.assertEqual(platform["workflow_name"], "CI")

    def test_context_falls_back_to_job_key_when_no_name(self):
        base = "name: WF\njobs:\n  foo:\n    continue-on-error: true\n    runs-on: x\n    steps: []\n"
        head = "name: WF\njobs:\n  foo:\n    continue-on-error: false\n    runs-on: x\n    steps: []\n"
        flips = lpfc.detect_flips({"a.yml": base}, {"a.yml": head})
        self.assertEqual(len(flips), 1)
        self.assertEqual(flips[0]["context"], "WF / foo (push)")

    def test_no_flip_when_only_one_side_has_file(self):
        # Newly added workflow file — head has CoE:false, base has no
        # file. Adding a new workflow with CoE:false is fine; there's
        # nothing to mask.
        flips = lpfc.detect_flips(
            {},  # base has no workflow files
            {".gitea/workflows/new.yml": CI_YML_HEAD_FLIPPED},
        )
        self.assertEqual(flips, [])

    def test_no_flip_when_job_removed(self):
        # Job exists on base, not on head — a removal, not a flip.
        head = """\
name: CI
jobs:
  canvas-build:
    name: Canvas (Next.js)
    continue-on-error: true
    runs-on: ubuntu-latest
    steps: []
"""
        flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": head},
        )
        self.assertEqual(flips, [])

    def test_no_flip_when_job_added_with_false(self):
        # New job on head with CoE:false — no base side; not a flip.
        head_with_new = CI_YML_BASE.replace(
            "  all-required:",
            "  newjob:\n    name: New Job\n    continue-on-error: false\n"
            "    runs-on: x\n    steps: []\n"
            "  all-required:",
        )
        flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": head_with_new},
        )
        self.assertEqual(flips, [])

    def test_yaml_parse_error_warns_not_raises(self):
        # Malformed YAML on head — should warn (stderr) and skip,
        # not raise.
        bad_head = "name: CI\njobs:\n  :::\n"
        # Capture stderr so the test isn't noisy.
        with mock.patch.object(sys, "stderr"):
            flips = lpfc.detect_flips(
                {".gitea/workflows/ci.yml": CI_YML_BASE},
                {".gitea/workflows/ci.yml": bad_head},
            )
        self.assertEqual(flips, [])


# --------------------------------------------------------------------------
# 3. grep_fail_markers — the regex / substring matcher
# --------------------------------------------------------------------------
class TestGrepFailMarkers(unittest.TestCase):
    def test_clean_log_returns_empty(self):
        log = "===== test run starting =====\nPASS\nok  example.com/foo  1.234s\n"
        self.assertEqual(lpfc.grep_fail_markers(log), [])

    def test_go_minus_minus_minus_fail_caught(self):
        log = "ok  example.com/foo  1.234s\n--- FAIL: TestBar (0.01s)\n    bar_test.go:42:\n"
        matches = lpfc.grep_fail_markers(log)
        self.assertEqual(len(matches), 1)
        self.assertIn("FAIL: TestBar", matches[0])

    def test_go_package_fail_caught(self):
        log = "FAIL\texample.com/baz\t1.234s\n"
        matches = lpfc.grep_fail_markers(log)
        self.assertEqual(len(matches), 1)
        self.assertIn("FAIL", matches[0])

    def test_bash_error_directive_caught(self):
        # `lint-curl-status-capture` pattern: a python heredoc inside a
        # bash step that prints `::error::` then sys.exit(1). With
        # continue-on-error:true the job rolls up as success despite
        # this line. THAT's the masking we're trying to catch.
        log = "Running scan...\n::error::Found 3 curl-status-capture pollution site(s):\n"
        matches = lpfc.grep_fail_markers(log)
        self.assertEqual(len(matches), 1)
        self.assertIn("::error::", matches[0])

    def test_caps_matches_at_max_5(self):
        log = "\n".join(["--- FAIL: T%d" % i for i in range(20)])
        matches = lpfc.grep_fail_markers(log)
        self.assertEqual(len(matches), 5)


# --------------------------------------------------------------------------
# 4. verify_flip — single-flip verdict assembly (network surface stubbed)
# --------------------------------------------------------------------------
def _stub_status(context: str, state: str, target_url: str = "/owner/repo/actions/runs/1/jobs/0") -> dict:
    """Build a single-context combined-status response."""
    return {
        "state": state,
        "statuses": [
            {"context": context, "status": state, "target_url": target_url, "description": ""}
        ],
    }


FLIP_FIXTURE = {
    "workflow_path": ".gitea/workflows/ci.yml",
    "workflow_name": "CI",
    "job_key": "platform-build",
    "job_name": "Platform (Go)",
    "context": "CI / Platform (Go) (push)",
}


class TestVerifyFlip(unittest.TestCase):
    def test_flip_with_clean_history_passes(self):
        # Acceptance test #2: flip detected, last 5 runs clean → exit 0.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1", "sha2", "sha3"]):
            with mock.patch.object(
                lpfc, "combined_status",
                side_effect=[_stub_status(FLIP_FIXTURE["context"], "success") for _ in range(3)],
            ):
                with mock.patch.object(lpfc, "fetch_log", return_value="ok  example.com/foo  1s\nPASS\n"):
                    verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(verdict["masked_runs"], [])
        self.assertEqual(verdict["checked_commits"], 3)
        self.assertEqual(verdict["warnings"], [])

    def test_flip_with_recent_fail_blocks(self):
        # Acceptance test #3: flip detected, recent run has --- FAIL → exit 1.
        # Setup: 3 commits, the most recent run's log shows --- FAIL
        # but the STATUS is success (Quirk #10 mask). That's the
        # masked_runs case.
        log_with_fail = "ok  example.com/foo  1s\n--- FAIL: TestSqlmock (0.01s)\n    sqlmock_test.go:42:\n"
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1", "sha2", "sha3"]):
            with mock.patch.object(
                lpfc, "combined_status",
                side_effect=[_stub_status(FLIP_FIXTURE["context"], "success") for _ in range(3)],
            ):
                with mock.patch.object(lpfc, "fetch_log", side_effect=[log_with_fail, "PASS\n", "PASS\n"]):
                    verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(len(verdict["masked_runs"]), 1)
        self.assertEqual(verdict["masked_runs"][0]["sha"], "sha1")
        self.assertTrue(any("TestSqlmock" in s for s in verdict["masked_runs"][0]["samples"]))
        self.assertEqual(verdict["fail_runs"], [])

    def test_red_status_alone_blocks(self):
        # Status itself is `failure` — block without needing log
        # markers. (Belt-and-braces: even with a clean log, a `failure`
        # status means the job's exit code was non-zero.)
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value=_stub_status(FLIP_FIXTURE["context"], "failure"),
            ):
                with mock.patch.object(lpfc, "fetch_log", return_value="some unrelated text\n"):
                    verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(len(verdict["fail_runs"]), 1)
        self.assertEqual(verdict["fail_runs"][0]["status"], "failure")

    def test_unreadable_log_warns_not_blocks(self):
        # Acceptance test #5: log fetch 404 (None) → warn, not block.
        # Status is `success`, log is None — we can't tell, so we warn
        # and allow.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value=_stub_status(FLIP_FIXTURE["context"], "success"),
            ):
                with mock.patch.object(lpfc, "fetch_log", return_value=None):
                    verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(verdict["masked_runs"], [])
        self.assertTrue(any("log unavailable" in w for w in verdict["warnings"]))

    def test_unreadable_log_with_failure_status_still_blocks(self):
        # Edge case: log fetch fails BUT the status itself is `failure`.
        # We can still block — the status alone is sufficient signal,
        # we don't need the log to confirm.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value=_stub_status(FLIP_FIXTURE["context"], "failure"),
            ):
                with mock.patch.object(lpfc, "fetch_log", return_value=None):
                    verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(len(verdict["fail_runs"]), 1)
        self.assertIn("log unavailable", verdict["fail_runs"][0]["samples"][0])

    def test_zero_runs_history_warns_allows(self):
        # No commits with a matching context — newly added workflow.
        # Allow with warning.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1", "sha2"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value={"state": "success", "statuses": []},  # no matching context
            ):
                verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["checked_commits"], 0)
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(verdict["masked_runs"], [])
        self.assertTrue(any("no runs of" in w for w in verdict["warnings"]))

    def test_zero_commits_warns_allows(self):
        # Empty branch (newly created repo, e.g.). Allow with warning.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=[]):
            verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["checked_commits"], 0)
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(verdict["masked_runs"], [])
        self.assertTrue(any("no recent commits" in w for w in verdict["warnings"]))


# --------------------------------------------------------------------------
# 5. Multiple-flip aggregation in main()
# --------------------------------------------------------------------------
class TestMainAggregation(unittest.TestCase):
    """Tests that `main()` aggregates multiple flips and exits 1 when
    ANY one of them has a masked or red recent run. Acceptance test #4.

    We stub at the verify_flip + workflows_at_sha + _require_runtime_env
    boundary so we don't need real git or HTTP.
    """

    def setUp(self):
        # The actual env values are irrelevant — _require_runtime_env
        # is stubbed out — but the module reads OWNER/NAME at import
        # time. Patch the runtime env contract to a no-op for the
        # duration of each test.
        self._patches = [
            mock.patch.object(lpfc, "_require_runtime_env", return_value=None),
            mock.patch.object(lpfc, "BASE_REF", "main"),
            mock.patch.object(lpfc, "BASE_SHA", "deadbeefcafe"),
            mock.patch.object(lpfc, "HEAD_SHA", "feedfaceabad"),
            mock.patch.object(lpfc, "RECENT_COMMITS_N", 5),
        ]
        for p in self._patches:
            p.start()
        self.addCleanup(lambda: [p.stop() for p in self._patches])

    def test_multiple_flips_aggregated_one_bad_blocks(self):
        # PR flips 3 jobs; 1 has a recent fail → exit 1, naming that job.
        flips = [
            {"workflow_path": ".gitea/workflows/ci.yml", "workflow_name": "CI",
             "job_key": "platform-build", "job_name": "Platform (Go)",
             "context": "CI / Platform (Go) (push)"},
            {"workflow_path": ".gitea/workflows/ci.yml", "workflow_name": "CI",
             "job_key": "canvas-build", "job_name": "Canvas (Next.js)",
             "context": "CI / Canvas (Next.js) (push)"},
            {"workflow_path": ".gitea/workflows/ci.yml", "workflow_name": "CI",
             "job_key": "python-lint", "job_name": "Python Lint & Test",
             "context": "CI / Python Lint & Test (push)"},
        ]
        clean = {"flip": flips[0], "checked_commits": 5, "masked_runs": [],
                 "fail_runs": [], "warnings": []}
        bad = {"flip": flips[1], "checked_commits": 5,
               "masked_runs": [{"sha": "abc1234567", "status": "success",
                                "target_url": "/x/y/actions/runs/1/jobs/0",
                                "samples": ["--- FAIL: TestSqlmock"]}],
               "fail_runs": [], "warnings": []}
        also_clean = {"flip": flips[2], "checked_commits": 5, "masked_runs": [],
                      "fail_runs": [], "warnings": []}

        with mock.patch.object(lpfc, "workflows_at_sha", return_value={}):
            with mock.patch.object(lpfc, "detect_flips", return_value=flips):
                with mock.patch.object(lpfc, "verify_flip",
                                       side_effect=[clean, bad, also_clean]):
                    # Capture stdout to assert on naming.
                    captured = []
                    with mock.patch("builtins.print", side_effect=lambda *a, **k: captured.append(" ".join(str(x) for x in a))):
                        rc = lpfc.main([])
        self.assertEqual(rc, 1)
        # The blocking error message must name the failing job.
        joined = "\n".join(captured)
        self.assertIn("canvas-build", joined)
        # And it must mention the empirical class so a reviewer can
        # cross-link the right RFC.
        self.assertTrue("mc#664" in joined or "PR#656" in joined)

    def test_no_flips_in_diff_exits_zero(self):
        # Acceptance test #1 at main() level: empty flips → exit 0.
        with mock.patch.object(lpfc, "workflows_at_sha", return_value={}):
            with mock.patch.object(lpfc, "detect_flips", return_value=[]):
                rc = lpfc.main([])
        self.assertEqual(rc, 0)

    def test_all_flips_clean_exits_zero(self):
        flips = [{"workflow_path": ".gitea/workflows/ci.yml", "workflow_name": "CI",
                  "job_key": "platform-build", "job_name": "Platform (Go)",
                  "context": "CI / Platform (Go) (push)"}]
        clean = {"flip": flips[0], "checked_commits": 5, "masked_runs": [],
                 "fail_runs": [], "warnings": []}
        with mock.patch.object(lpfc, "workflows_at_sha", return_value={}):
            with mock.patch.object(lpfc, "detect_flips", return_value=flips):
                with mock.patch.object(lpfc, "verify_flip", return_value=clean):
                    rc = lpfc.main([])
        self.assertEqual(rc, 0)

    def test_dry_run_forces_exit_zero_even_with_bad_flip(self):
        # --dry-run never fails, even when verification finds masked runs.
        flips = [{"workflow_path": ".gitea/workflows/ci.yml", "workflow_name": "CI",
                  "job_key": "platform-build", "job_name": "Platform (Go)",
                  "context": "CI / Platform (Go) (push)"}]
        bad = {"flip": flips[0], "checked_commits": 5,
               "masked_runs": [{"sha": "abc1234567", "status": "success",
                                "target_url": "/x/y/actions/runs/1/jobs/0",
                                "samples": ["--- FAIL: TestSqlmock"]}],
               "fail_runs": [], "warnings": []}
        with mock.patch.object(lpfc, "workflows_at_sha", return_value={}):
            with mock.patch.object(lpfc, "detect_flips", return_value=flips):
                with mock.patch.object(lpfc, "verify_flip", return_value=bad):
                    rc = lpfc.main(["--dry-run"])
        self.assertEqual(rc, 0)


# --------------------------------------------------------------------------
# 6. Context-name rendering (the format Gitea Actions actually emits)
# --------------------------------------------------------------------------
class TestContextName(unittest.TestCase):
    def test_push_event(self):
        self.assertEqual(
            lpfc.context_name("CI", "Platform (Go)", "push"),
            "CI / Platform (Go) (push)",
        )

    def test_pull_request_event(self):
        self.assertEqual(
            lpfc.context_name("CI", "Platform (Go)", "pull_request"),
            "CI / Platform (Go) (pull_request)",
        )

    def test_workflow_name_falls_back_to_filename(self):
        # No top-level `name:` → falls back to filename minus extension.
        doc = {"jobs": {"foo": {"continue-on-error": True}}}
        self.assertEqual(
            lpfc.workflow_name(doc, fallback="my-workflow"),
            "my-workflow",
        )


if __name__ == "__main__":
    unittest.main()
