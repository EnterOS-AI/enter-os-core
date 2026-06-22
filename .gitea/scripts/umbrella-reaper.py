#!/usr/bin/env python3
"""umbrella-reaper — auto-recovery for stale CI umbrella statuses on PRs.

Tracking: molecule-core#1780.

Sibling to status-reaper.py (default-branch push-suffix compensation),
but scoped to pull_request umbrellas instead of main-branch contexts.

What this script does, per `.gitea/workflows/umbrella-reaper.yml` invocation:

  1. List open PRs via GET /repos/{o}/{r}/pulls?state=open&limit={N}.
  2. For EACH PR:
     - GET combined commit status for PR head SHA.
     - Look for the umbrella context (default: "CI / all-required (pull_request)").
     - If umbrella state is "failure":
         - Verify ALL required sub-job contexts are "success".
         - If yes → POST compensating success to /statuses/{sha} with the
           same umbrella context and an honest description.
         - If any required sub-job is NOT success → skip (umbrella correctly
           reflects reality; do NOT lie).
     - If umbrella state is "success" or "pending" → skip.
  3. Exit 0. Re-running is idempotent — Gitea de-dups by context.

What it does NOT do:
  - Touch non-umbrella contexts.
  - Compensate when ANY required sub-job is missing, pending, failure, or
    cancelled. Only the "all sub-jobs green, umbrella stale" race.
  - Merge PRs. It only posts a status; branch protection still requires
    human approval.
  - Run on closed PRs.

Halt conditions:
  - Missing required env vars → exit 1 with ::error:: message.
  - API 5xx on PR list → fail-loud (can't assess state).
  - API 5xx on an individual PR's status → ::warning:: + continue to next PR.
"""
from __future__ import annotations

import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


def _load_required_sub_jobs_from_ci_yml(workflows_dir: str) -> list[str]:
    """Parse ci.yml and extract the all-required sentinel's sub-job contexts.

    Derives sub-jobs from the authoritative `needs:` list plus each job's
    display `name:`. This is more robust than parsing the `run:` block, which
    has changed shape several times (Python f-string list, inline shell,
    extracted shell script).

    Raises RuntimeError if ci.yml is missing, has no all-required job, or the
    sub-jobs cannot be derived.
    """
    ci_path = Path(workflows_dir) / "ci.yml"
    if not ci_path.exists():
        raise RuntimeError(f"ci.yml not found at {ci_path}")

    # PyYAML is installed by the workflow (same as status-reaper.py).
    import yaml

    with ci_path.open() as f:
        doc = yaml.safe_load(f)

    jobs = doc.get("jobs", {})
    all_required = jobs.get("all-required")
    if not isinstance(all_required, dict):
        raise RuntimeError("ci.yml missing 'all-required' job")

    # Determine event suffix from the umbrella context we are watching.
    if UMBRELLA_CONTEXT.endswith(" (pull_request)"):
        suffix = "(pull_request)"
    elif UMBRELLA_CONTEXT.endswith(" (push)"):
        suffix = "(push)"
    else:
        m = re.search(r" \(([^)]+)\)$", UMBRELLA_CONTEXT)
        suffix = m.group(1) if m else "pull_request"

    needs = all_required.get("needs", [])
    if isinstance(needs, str):
        needs = [needs]
    if not needs:
        raise RuntimeError("all-required job has no needs: cannot derive sub-jobs")

    contexts = []
    for key in needs:
        job = jobs.get(key)
        if not isinstance(job, dict):
            raise RuntimeError(f"all-required needs unknown job {key!r}")
        name = job.get("name")
        if not name:
            raise RuntimeError(f"job {key!r} has no name: cannot build status context")
        contexts.append(f"CI / {name} {suffix}")

    return contexts


# --------------------------------------------------------------------------
# Environment
# --------------------------------------------------------------------------
def _env(key: str, *, default: str = "") -> str:
    return os.environ.get(key, default)


GITEA_TOKEN = _env("GITEA_TOKEN")
GITEA_HOST = _env("GITEA_HOST")
REPO = _env("REPO")
DRY_RUN = _env("DRY_RUN", default="").lower() in ("1", "true", "yes")

# The umbrella context to watch. Must match the branch-protection name
# exactly (Gitea de-dups by context string).
UMBRELLA_CONTEXT = _env("UMBRELLA_CONTEXT", default="CI / all-required (pull_request)")

# Required sub-job contexts. The umbrella is only compensated when ALL of
# these are "success" on the same SHA. Order does not matter.
#
# Derive from ci.yml at runtime to prevent drift (CR2 blocker #1).
# The env var REQUIRED_SUB_JOBS overrides derivation for emergency
# tuning or local testing.
_REQUIRED_SUB_JOBS_OVERRIDE = _env("REQUIRED_SUB_JOBS")
if _REQUIRED_SUB_JOBS_OVERRIDE:
    REQUIRED_SUB_JOBS = [
        ctx.strip()
        for ctx in _REQUIRED_SUB_JOBS_OVERRIDE.split(";")
        if ctx.strip()
    ]
else:
    try:
        REQUIRED_SUB_JOBS = _load_required_sub_jobs_from_ci_yml(".gitea/workflows")
    except Exception as exc:
        sys.stderr.write(
            f"::error::Failed to derive REQUIRED_SUB_JOBS from ci.yml: {exc}\n"
        )
        sys.exit(1)

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""
PR_LIMIT = int(_env("PR_LIMIT", default="50"))


def _require_runtime_env() -> None:
    for key in ("GITEA_TOKEN", "GITEA_HOST", "REPO"):
        if not os.environ.get(key):
            sys.stderr.write(f"::error::missing required env var: {key}\n")
            sys.exit(1)


# --------------------------------------------------------------------------
# Tiny HTTP helper
# --------------------------------------------------------------------------
class ApiError(RuntimeError):
    pass


def api(
    method: str,
    path: str,
    *,
    body: dict | None = None,
    query: dict[str, str] | None = None,
    expect_json: bool = True,
) -> tuple[int, Any]:
    url = f"{API}{path}"
    if query:
        url = f"{url}?{urllib.parse.urlencode(query)}"
    data = None
    headers = {
        "Authorization": f"token {GITEA_TOKEN}",
        "Accept": "application/json",
    }
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, method=method, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read()
        status = e.code

    if not (200 <= status < 300):
        snippet = raw[:500].decode("utf-8", errors="replace") if raw else ""
        raise ApiError(f"{method} {path} -> HTTP {status}: {snippet}")

    if not raw:
        return status, None
    try:
        return status, json.loads(raw)
    except json.JSONDecodeError as e:
        if expect_json:
            raise ApiError(
                f"{method} {path} -> HTTP {status} but body is not JSON: {e}"
            ) from e
        return status, {"_raw": raw.decode("utf-8", errors="replace")}


# --------------------------------------------------------------------------
# Gitea reads / writes
# --------------------------------------------------------------------------
def list_open_prs(limit: int = 50) -> list[dict]:
    """Paginate through all open PR pages. Fail closed on non-list responses."""
    all_prs: list[dict] = []
    page = 1
    while True:
        _, body = api(
            "GET",
            f"/repos/{OWNER}/{NAME}/pulls",
            query={"state": "open", "limit": str(limit), "page": str(page)},
        )
        if not isinstance(body, list):
            raise ApiError(f"PR list page {page} response is not a JSON array")
        if not body:
            break
        all_prs.extend(body)
        if len(body) < limit:
            break
        page += 1
    return all_prs


def get_combined_status(sha: str) -> dict:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/commits/{sha}/status")
    if not isinstance(body, dict):
        raise ApiError(f"status for {sha} response is not a JSON object")
    return body


def post_status(sha: str, context: str, description: str) -> None:
    payload = {
        "context": context,
        "state": "success",
        "description": description,
    }
    if DRY_RUN:
        print(f"[DRY-RUN] Would POST /statuses/{sha}: {json.dumps(payload)}")
        return
    api("POST", f"/repos/{OWNER}/{NAME}/statuses/{sha}", body=payload)


# --------------------------------------------------------------------------
# Core logic
# --------------------------------------------------------------------------
def _entry_state(s: dict) -> str:
    return s.get("status") or s.get("state") or ""


def process_pr(pr: dict) -> bool:
    """Process a single PR. Returns True if the tick succeeded for this PR
    (including no-op skips), False if a compensating POST failed.
    """
    num = pr.get("number")
    sha = pr.get("head", {}).get("sha")
    if not sha:
        print(f"::warning::PR #{num}: missing head.sha; skipping")
        return True

    try:
        status = get_combined_status(sha)
    except ApiError as e:
        print(f"::error::PR #{num}: status fetch failed: {e}")
        return False

    statuses = status.get("statuses")
    if not isinstance(statuses, list):
        print(f"::error::PR #{num}: combined status missing 'statuses' array")
        return False
    umbrella_entry = None
    subjob_states: dict[str, str] = {}

    for s in statuses:
        if not isinstance(s, dict):
            continue
        ctx = s.get("context", "")
        state = _entry_state(s)
        if ctx == UMBRELLA_CONTEXT:
            umbrella_entry = s
        if ctx in REQUIRED_SUB_JOBS:
            subjob_states[ctx] = state

    if umbrella_entry is None:
        print(f"::notice::PR #{num}: no umbrella context '{UMBRELLA_CONTEXT}'; skipping")
        return True

    umbrella_state = _entry_state(umbrella_entry)
    if umbrella_state != "failure":
        print(f"::notice::PR #{num}: umbrella is '{umbrella_state}'; skipping")
        return True

    # Verify ALL required sub-jobs are present and success
    missing = [ctx for ctx in REQUIRED_SUB_JOBS if ctx not in subjob_states]
    if missing:
        print(
            f"::notice::PR #{num}: umbrella=failure, but missing sub-jobs: {missing}; "
            "skipping (sub-jobs may still be running)"
        )
        return True

    not_success = [ctx for ctx in REQUIRED_SUB_JOBS if subjob_states[ctx] != "success"]
    if not_success:
        print(
            f"::notice::PR #{num}: umbrella=failure, but sub-jobs not all success: "
            f"{[(ctx, subjob_states[ctx]) for ctx in not_success]}; skipping"
        )
        return True

    # All checks pass — post compensating status
    desc = (
        "Compensating status: all required sub-jobs verified success; "
        "umbrella stale due to commit-status propagation race. "
        f"Auto-posted by umbrella-reaper for PR #{num}."
    )
    try:
        post_status(sha, UMBRELLA_CONTEXT, desc)
        print(f"::notice::PR #{num}: posted compensating success for {UMBRELLA_CONTEXT}")
        return True
    except ApiError as e:
        print(f"::error::PR #{num}: failed to post compensating status: {e}")
        return False


def main() -> int:
    _require_runtime_env()

    # Drift guard: ci.yml derivation already happened at module load, but
    # we sanity-check it is non-empty so the loop below doesn't trivially
    # no-op because of a parse bug.
    if not REQUIRED_SUB_JOBS:
        sys.stderr.write("::error::REQUIRED_SUB_JOBS is empty; bailing out\n")
        return 1

    prs = list_open_prs(limit=PR_LIMIT)
    print(f"::notice::Scanning {len(prs)} open PRs for stale umbrella statuses")
    compensated = 0
    failed = 0
    for pr in prs:
        ok = process_pr(pr)
        if not ok:
            failed += 1
    print(f"::notice::umbrella-reaper complete (failed POSTs={failed})")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
