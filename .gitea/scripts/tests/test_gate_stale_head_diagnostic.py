"""Stale-head diagnostic test for #2159.

Deterministically reports whether a PR's HEAD contains the pull_request_review
trigger in qa-review.yml and security-review.yml. If the trigger is absent,
auto-fire on APPROVED review is impossible for that PR.

This is used as a self-diagnostic for future stale-PR situations (PRs opened
before #2157 merged, or branches cut from old bases).

Environment:
  GITEA_HOST  — default: git.moleculesai.app
  GITEA_TOKEN — token with read:repository scope (optional; falls back to local files)
  REPO        — default: molecule-ai/molecule-core
  PR_NUMBER   — required when running against a real PR
"""

import base64
import json
import os
import urllib.error
import urllib.request
from pathlib import Path

import pytest

import yaml

GITEA_HOST = os.environ.get("GITEA_HOST", "git.moleculesai.app")
GITEA_TOKEN = os.environ.get("GITEA_TOKEN", "")
REPO = os.environ.get("REPO", "molecule-ai/molecule-core")
PR_NUMBER = os.environ.get("PR_NUMBER", "")

ROOT = Path(__file__).resolve().parents[2]


def _api(method: str, path: str) -> tuple[int, dict]:
    url = f"https://{GITEA_HOST}/api/v1{path}"
    headers = {"Authorization": f"token {GITEA_TOKEN}"}
    req = urllib.request.Request(url, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        body = exc.read()
        return exc.code, json.loads(body) if body else {}


def _fetch_workflow_from_ref(workflow_name: str, ref: str) -> dict:
    path = f"/repos/{REPO}/contents/.gitea/workflows/{workflow_name}?ref={ref}"
    code, payload = _api("GET", path)
    if code != 200:
        pytest.fail(
            f"GET {path} returned HTTP {code}: {payload}. "
            f"Cannot determine whether PR head contains the trigger."
        )
    raw = base64.b64decode(payload.get("content", "")).decode("utf-8")
    return yaml.safe_load(raw)


def _fetch_workflow_local(workflow_name: str) -> dict:
    p = ROOT / "workflows" / workflow_name
    if not p.exists():
        pytest.fail(f"Local workflow file not found: {p}")
    return yaml.safe_load(p.read_text())


def _has_pull_request_review_trigger(wf: dict) -> bool:
    on = wf.get(True) or wf.get("on") or {}
    if isinstance(on, list):
        return "pull_request_review" in on
    if isinstance(on, dict):
        return "pull_request_review" in on
    if isinstance(on, str):
        return on == "pull_request_review"
    return False


def _diagnose_pr(pr_number: int) -> dict[str, bool]:
    code, pr = _api("GET", f"/repos/{REPO}/pulls/{pr_number}")
    if code != 200:
        pytest.fail(f"GET /pulls/{pr_number} returned HTTP {code}: {pr}")

    head_ref = pr["head"]["ref"]
    head_sha = pr["head"]["sha"]

    results: dict[str, bool] = {}
    for wf_name in ("qa-review.yml", "security-review.yml"):
        wf = _fetch_workflow_from_ref(wf_name, head_sha)
        results[wf_name] = _has_pull_request_review_trigger(wf)

    return {
        "pr_number": pr_number,
        "head_ref": head_ref,
        "head_sha": head_sha,
        "triggers": results,
        "auto_fire_possible": all(results.values()),
    }


def _diagnose_local() -> dict[str, bool]:
    results: dict[str, bool] = {}
    for wf_name in ("qa-review.yml", "security-review.yml"):
        wf = _fetch_workflow_local(wf_name)
        results[wf_name] = _has_pull_request_review_trigger(wf)
    return {
        "pr_number": None,
        "head_ref": "local-checkout",
        "head_sha": None,
        "triggers": results,
        "auto_fire_possible": all(results.values()),
    }


class TestStaleHeadDiagnostic:
    """Test deterministically reports 'auto-fire impossible for this PR' when
    the PR head lacks the pull_request_review trigger.
    """

    def test_local_checkout_has_pull_request_review_trigger(self):
        """Local files (the ones in this checkout) must contain the trigger.

        This is the baseline: if the checkout itself is stale, every PR cut
        from it will also be stale.
        """
        diag = _diagnose_local()
        missing = [n for n, ok in diag["triggers"].items() if not ok]
        if missing:
            pytest.fail(
                f"Local checkout is missing pull_request_review trigger in: {missing}. "
                f"This branch cannot produce PRs that auto-fire."
            )

    @pytest.mark.skipif(not GITEA_TOKEN, reason="GITEA_TOKEN not set")
    @pytest.mark.skipif(not PR_NUMBER, reason="PR_NUMBER not set")
    def test_pr_head_has_pull_request_review_trigger(self):
        """When PR_NUMBER is given, assert the PR head contains the trigger."""
        diag = _diagnose_pr(int(PR_NUMBER))
        if not diag["auto_fire_possible"]:
            missing = [n for n, ok in diag["triggers"].items() if not ok]
            pytest.fail(
                f"Auto-fire impossible for PR #{diag['pr_number']}. "
                f"Head ref={diag['head_ref']} sha={diag['head_sha']}. "
                f"Missing trigger in: {missing}. "
                f"This PR needs /qa-recheck + /security-recheck fallback, or a rebase onto current main."
            )
