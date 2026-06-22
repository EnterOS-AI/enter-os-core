"""Live-fire regression test for #2159 — gate auto-fire runtime verification.

Static tests (test_gate_review_auto_fire.py) validate that the workflow YAML
is structurally correct. This test validates the *runtime* path: submitting an
APPROVED review to a PR whose head contains the current gate workflows causes
Gitea Actions to queue the qa-review + security-review workflows and POST the
branch-protection-required (pull_request_target) contexts within a reasonable
window.

Skipped when Gitea API credentials are not available. Intended for:
  - manual developer verification
  - CI jobs provisioned with a service-account token

Environment:
  GITEA_HOST            — default: git.moleculesai.app
  GITEA_TOKEN           — token with read:repository + write:issues (for review POST)
  REPO                  — default: molecule-ai/molecule-core
  LIVEFIRE_PR_NUMBER    — optional; if omitted the test tries to find a
                          suitable open PR automatically, or skips.
  LIVEFIRE_TIMEOUT_SEC  — default: 120
"""

import base64
import json
import os
import re
import time
import urllib.error
import urllib.request
from pathlib import Path

import pytest

import yaml

GITEA_HOST = os.environ.get("GITEA_HOST", "git.moleculesai.app")
GITEA_TOKEN = os.environ.get("GITEA_TOKEN", "")
REPO = os.environ.get("REPO", "molecule-ai/molecule-core")
LIVEFIRE_PR_NUMBER = os.environ.get("LIVEFIRE_PR_NUMBER", "")
LIVEFIRE_TIMEOUT_SEC = int(os.environ.get("LIVEFIRE_TIMEOUT_SEC", "120"))

REQUIRED_CONTEXTS = [
    "qa-review / approved (pull_request_target)",
    "security-review / approved (pull_request_target)",
]

skip_no_token = pytest.mark.skipif(
    not GITEA_TOKEN,
    reason="GITEA_TOKEN not set — live-fire test requires API credentials",
)


def _api(method: str, path: str, body: dict | None = None) -> tuple[int, dict]:
    url = f"https://{GITEA_HOST}/api/v1{path}"
    headers = {
        "Authorization": f"token {GITEA_TOKEN}",
        "Content-Type": "application/json",
    }
    data = json.dumps(body).encode() if body else None
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
            code = resp.status
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        code = exc.code
    payload = json.loads(raw) if raw else {}
    return code, payload


def _get_pr(number: int) -> dict:
    code, pr = _api("GET", f"/repos/{REPO}/pulls/{number}")
    if code != 200:
        pytest.fail(f"GET /pulls/{number} returned HTTP {code}: {pr}")
    return pr


def _list_open_prs() -> list[dict]:
    code, prs = _api("GET", f"/repos/{REPO}/pulls?state=open&limit=50")
    if code != 200:
        pytest.fail(f"GET /pulls?state=open returned HTTP {code}: {prs}")
    return prs


def _pr_has_trigger_in_head(pr: dict) -> bool:
    """Return True if the PR head contains pull_request_review in both workflows."""
    head_sha = pr["head"]["sha"]
    for wf_name in ("qa-review.yml", "security-review.yml"):
        path = f"/repos/{REPO}/contents/.gitea/workflows/{wf_name}?ref={head_sha}"
        code, payload = _api("GET", path)
        if code != 200:
            return False
        raw = base64.b64decode(payload.get("content", "")).decode("utf-8")
        wf = yaml.safe_load(raw)
        on = wf.get(True) or wf.get("on") or {}
        if isinstance(on, str):
            if on != "pull_request_review":
                return False
        elif "pull_request_review" not in on:
            return False
    return True


def _find_suitable_pr() -> dict:
    if LIVEFIRE_PR_NUMBER:
        pr = _get_pr(int(LIVEFIRE_PR_NUMBER))
        if pr.get("state") != "open":
            pytest.skip(f"PR {LIVEFIRE_PR_NUMBER} is not open")
        return pr

    prs = _list_open_prs()
    for pr in prs:
        if _pr_has_trigger_in_head(pr):
            return pr
    pytest.skip("No open PR found whose head contains the pull_request_review trigger")


def _submit_approved_review(pr_number: int) -> dict:
    code, review = _api(
        "POST",
        f"/repos/{REPO}/pulls/{pr_number}/reviews",
        {"body": "Live-fire test APPROVED review", "event": "APPROVED"},
    )
    # 200 = created, 422 = review already exists (idempotent enough for our purposes)
    if code not in (200, 201, 422):
        pytest.fail(f"POST /pulls/{pr_number}/reviews returned HTTP {code}")
    return review


def _get_status_snapshot(sha: str) -> dict[str, dict]:
    """Return mapping context -> {id, updated_at, target_url} for required contexts."""
    code, statuses = _api("GET", f"/repos/{REPO}/statuses/{sha}?limit=100")
    if code != 200:
        return {}
    result: dict[str, dict] = {}
    for st in statuses:
        ctx = st.get("context", "")
        if ctx in REQUIRED_CONTEXTS:
            result[ctx] = {
                "id": st.get("id"),
                "updated_at": st.get("updated_at", st.get("created_at", "")),
                "target_url": st.get("target_url"),
            }
    return result


def _extract_run_id(target_url: str | None) -> str | None:
    """Extract the Actions run_id from a status target_url."""
    if not target_url:
        return None
    m = re.search(r"/actions/runs/(\d+)", target_url)
    return m.group(1) if m else None


def _poll_fresh_statuses(
    sha: str,
    prior_snapshot: dict[str, dict],
    timeout_sec: int = LIVEFIRE_TIMEOUT_SEC,
) -> dict[str, dict]:
    """Poll until required contexts appear fresh (newer timestamp, id, or run)."""
    deadline = time.monotonic() + timeout_sec
    found: dict[str, dict] = {}
    while time.monotonic() < deadline:
        code, statuses = _api("GET", f"/repos/{REPO}/statuses/{sha}?limit=100")
        if code == 200:
            for st in statuses:
                ctx = st.get("context", "")
                if ctx in REQUIRED_CONTEXTS:
                    updated_at = st.get("updated_at", st.get("created_at", ""))
                    status_id = st.get("id")
                    target_url = st.get("target_url")
                    prior = prior_snapshot.get(ctx, {})
                    # Fresh if timestamp changed, id changed, or target_url changed.
                    is_fresh = (
                        ctx not in prior_snapshot
                        or updated_at != prior.get("updated_at", "")
                        or status_id != prior.get("id")
                        or target_url != prior.get("target_url")
                    )
                    if is_fresh:
                        found[ctx] = {
                            "state": st.get("state", st.get("status", "")),
                            "updated_at": updated_at,
                            "id": status_id,
                            "target_url": target_url,
                        }
        if all(ctx in found for ctx in REQUIRED_CONTEXTS):
            return found
        time.sleep(5)
    return found


@skip_no_token
class TestGateAutoFireLive:
    def test_auto_fire_posts_required_contexts(self):
        """Submit APPROVED review; assert BP-required contexts appear fresh within timeout."""
        pr = _find_suitable_pr()
        pr_number = pr["number"]
        head_sha = pr["head"]["sha"]

        # Capture pre-existing status snapshot so we can prove FRESH contexts
        # were posted after the review submission (not stale from a prior run).
        prior_snapshot = _get_status_snapshot(head_sha)
        prior_run_ids = {
            _extract_run_id(s["target_url"])
            for s in prior_snapshot.values()
            if _extract_run_id(s["target_url"])
        }

        review = _submit_approved_review(pr_number)

        found = _poll_fresh_statuses(head_sha, prior_snapshot)

        missing = [ctx for ctx in REQUIRED_CONTEXTS if ctx not in found]
        if missing:
            pytest.fail(
                f"After {LIVEFIRE_TIMEOUT_SEC}s, fresh contexts still missing: {missing}. "
                f"Found: {found}. Prior snapshot: {prior_snapshot}. "
                f"PR #{pr_number} head={head_sha}. "
                f"This indicates the pull_request_review trigger did not fire at runtime."
            )

        # The contexts appeared fresh — that's the proof of auto-fire.
        # We do NOT assert success vs failure; the evaluator decides that.
        # The point of #2159 is that the workflows QUEUE and POST at all.
        for ctx, info in found.items():
            state = info["state"]
            assert state in ("pending", "success", "failure"), (
                f"Unexpected state {state!r} for {ctx}"
            )

            # CR2 Finding 1: prove a NEW workflow run was triggered, not just
            # an in-place status update. Gitea 1.22.6 lacks REST /actions/runs/*
            # endpoints, so we use the run_id embedded in the status target_url
            # as a proxy for distinct run_id.
            run_id = _extract_run_id(info.get("target_url"))
            if run_id and run_id in prior_run_ids:
                pytest.fail(
                    f"Context {ctx!r} has target_url run_id {run_id} which existed "
                    f"BEFORE the review was submitted. This means the status was "
                    f"updated in-place by an existing run, not by a new workflow "
                    f"run triggered from the pull_request_review event."
                )
