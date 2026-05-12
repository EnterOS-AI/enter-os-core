#!/usr/bin/env python3
"""status-reaper — Option B compensating-status POST for Gitea 1.22.6's
hardcoded `(push)` suffix on default-branch commit statuses.

Tracking: this PR (workflow + script + tests + audit issue). Sibling
bots: internal#327 (publish-runtime-bot), internal#328 (mc-drift-bot).
Upstream RFC: internal#80. Persona provisioned by sub-agent aefaac1b
(2026-05-11 21:39Z; Gitea uid 94, scope=write:repository).

What this script does, per `.gitea/workflows/status-reaper.yml` invocation:

  1. Walk `.gitea/workflows/*.yml`. For each file, build the workflow_id
     using this resolution (per hongming-pc 22:08Z review):
       - If YAML has top-level `name:` → use that.
       - Else → use filename stem (basename minus `.yml`).
     Fail-LOUD on:
       - Two workflows resolving to the SAME identifier (collision).
       - Any identifier containing `/` (it would break context parsing
         downstream — Gitea uses ` / ` as the workflow/job separator).
     Classify each by whether `on:` contains a `push:` trigger.

  2. List the last N (=10) commits on WATCH_BRANCH via
     GET /repos/{o}/{r}/commits?sha={branch}&limit={N}. rev2 sweeps
     N commits per tick instead of HEAD only — schedule workflows
     post `failure` to whatever SHA was HEAD when they COMPLETED, so
     by the next */5 tick main has often moved forward and the red
     gets stranded on a stale commit (Phase 1+2 evidence: rev1 saw
     `compensated:0` every tick across ~6 cycles).

  3. For EACH SHA in the list:
       - GET combined commit status. Per-SHA error isolation
         (refinement #7): if this call raises ApiError or any 5xx,
         LOG `::warning::` + continue to the next SHA. Different from
         the single-HEAD pre-rev2 path where fail-loud was correct;
         the sweep is best-effort across historical commits, so one
         transient blip on a stale SHA must not strand reds on the
         OTHER stale SHAs.
       - If combined.state == "success": skip — cost optimization
         (refinement #2), common case (most commits are green).
       - Otherwise iterate per-context entries. For each entry where:
           state == "failure" AND context.endswith(" (push)")
         Parse context as `<workflow_name> / <job_name> (push)`.
         Look up workflow_name in the trigger map:
           - missing → log ::notice:: and skip (conservative).
           - has_push_trigger=True → preserve (real defect signal).
           - has_push_trigger=False → POST a compensating
             `state=success` status to /statuses/{sha} with the same
             context (Gitea de-dups by context) and a description
             documenting the workaround + this script's path.

  4. Exit 0. Re-running is idempotent — Gitea's commit-status table
     stores the LATEST state-per-context, so the success POST sticks
     even if another tick happens before the runner finishes.

What it does NOT do:
  - Touch any context NOT ending in ` (push)`. The required-checks on
    main (verified 2026-05-11) all have ` (pull_request)` suffixes;
    they CANNOT be reached by this code path.
  - Compensate `error`/`pending` states. Only `failure` — the only one
    Gitea emits for the hardcoded-suffix bug.
  - Write to non-default branches. WATCH_BRANCH is sourced from
    `github.event.repository.default_branch` in the workflow.
  - Mutate workflows or runs. The Actions UI still shows the
    underlying schedule-triggered run as failed; this script edits
    the commit-status surface only.

Halt conditions (script-level — orchestrator-level halts are in the
workflow comments):
  - PyYAML missing → fail-loud at import (no fallback parse).
  - Workflow `name:` collision → exit 1 with ::error:: message.
  - Workflow `name:` containing `/` → exit 1 with ::error:: message.
  - Ambiguous `on:` shape (e.g. neither str/list/dict) → treat as
    "has_push_trigger=True" and log ::notice:: (preserve, never
    compensate the unknown).
  - api() non-2xx → raise ApiError, fail the workflow run loudly so
    a subsequent tick retries (per
    `feedback_api_helper_must_raise_not_return_dict`).

Local dry-run (no network):
    GITEA_TOKEN=... GITEA_HOST=git.moleculesai.app REPO=owner/repo \\
      WATCH_BRANCH=main WORKFLOWS_DIR=.gitea/workflows \\
      python3 .gitea/scripts/status-reaper.py --dry-run
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

import yaml  # PyYAML 6.0.2 — installed by the workflow before this runs.


# --------------------------------------------------------------------------
# Environment
# --------------------------------------------------------------------------
def _env(key: str, *, default: str = "") -> str:
    """Read an env var with a default. Module-import-safe — tests can
    import this script without setting the full env contract."""
    return os.environ.get(key, default)


GITEA_TOKEN = _env("GITEA_TOKEN")
GITEA_HOST = _env("GITEA_HOST")
REPO = _env("REPO")
WATCH_BRANCH = _env("WATCH_BRANCH", default="main")
WORKFLOWS_DIR = _env("WORKFLOWS_DIR", default=".gitea/workflows")

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""

# Compensating-status description prefix. Used as the marker so a human
# auditing commit statuses can tell at a glance that the green was
# synthetic, not a real CI pass. Kept stable; downstream tooling
# (e.g. main-red-watchdog visual diff) MAY key on it.
COMPENSATION_DESCRIPTION = (
    "Compensated by status-reaper (workflow has no push: trigger; "
    "Gitea 1.22.6 hardcoded-suffix bug — see .gitea/scripts/status-reaper.py)"
)

# Context suffix the reaper acts on. Gitea hardcodes this for ALL
# default-branch workflow runs.
PUSH_SUFFIX = " (push)"


def _require_runtime_env() -> None:
    """Enforce env contract — called from `main()` only.

    Tests import individual functions without setting the full env
    contract. Mirrors `main-red-watchdog.py`/`ci-required-drift.py`.
    """
    for key in ("GITEA_TOKEN", "GITEA_HOST", "REPO", "WATCH_BRANCH", "WORKFLOWS_DIR"):
        if not os.environ.get(key):
            sys.stderr.write(f"::error::missing required env var: {key}\n")
            sys.exit(2)


# --------------------------------------------------------------------------
# Tiny HTTP helper — raises on non-2xx + on JSON-decode-of-expected-JSON.
# --------------------------------------------------------------------------
class ApiError(RuntimeError):
    """Raised when a Gitea API call cannot be trusted to have succeeded.

    Per `feedback_api_helper_must_raise_not_return_dict`: soft-failure is
    opt-in via `expect_json=False`, never the default. A pre-fix
    implementation that returned `{}` on non-2xx would skip the
    compensating POST on a transient outage AND silently lose the
    failed-status enumeration, painting main green via omission.
    """


def api(
    method: str,
    path: str,
    *,
    body: dict | None = None,
    query: dict[str, str] | None = None,
    expect_json: bool = True,
) -> tuple[int, Any]:
    """Tiny HTTP helper around urllib. Same contract as
    `main-red-watchdog.py` and `ci-required-drift.py` so behaviour
    is cross-checkable."""
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
# Workflow scan + classification
# --------------------------------------------------------------------------
def _on_block(doc: dict) -> Any:
    """Extract the `on:` block from a parsed YAML doc.

    PyYAML parses bareword `on:` as Python `True` (YAML 1.1 boolean
    spec — `on/off/yes/no` are booleans). The actual key in the dict
    is therefore `True`, NOT the string `"on"`. We accept both for
    forward-compat with YAML 1.2 loaders (which keep it as `"on"`).
    """
    if True in doc:
        return doc[True]
    return doc.get("on")


def _has_push_trigger(on_block: Any, workflow_id: str) -> bool:
    """Return True if `on:` block declares a `push` trigger.

    Accepts the three common shapes:
      - str: `on: push` → True only if == "push"
      - list: `on: [push, pull_request]` → True if "push" in list
      - dict: `on: { push: {...}, schedule: ... }` → True if "push" key

    Defensive: for anything else (including None/empty), return True
    so we preserve rather than over-compensate. Logged via ::notice::.
    """
    if isinstance(on_block, str):
        return on_block == "push"
    if isinstance(on_block, list):
        return "push" in on_block
    if isinstance(on_block, dict):
        return "push" in on_block
    # None or unexpected shape — preserve, log.
    print(
        f"::notice::ambiguous on: for {workflow_id}; preserving "
        f"(value={on_block!r}, type={type(on_block).__name__})"
    )
    return True


def scan_workflows(workflows_dir: str) -> dict[str, bool]:
    """Walk `workflows_dir` and return `{workflow_id: has_push_trigger}`.

    Workflow ID resolution (per hongming-pc 22:08Z review):
      - Top-level `name:` if present.
      - Else filename stem (basename minus `.yml`).

    Fail-LOUD on:
      - Two workflows resolving to the same ID (collision).
      - Any ID containing `/` (would break ` / `-separated context
        parsing on the downstream side).

    Returns a dict for O(1) lookup in the per-status loop.
    """
    path = Path(workflows_dir)
    if not path.is_dir():
        # Workflow dir missing → no workflows to classify. Empty map is
        # safe: per-status loop will hit "unknown workflow; skip" for
        # every entry, which is correct (we cannot tell if a push
        # trigger exists, so we preserve).
        print(f"::warning::workflows dir not found: {workflows_dir}")
        return {}

    out: dict[str, bool] = {}
    sources: dict[str, str] = {}  # workflow_id -> source file (for collision msg)

    for yml in sorted(path.glob("*.yml")):
        try:
            with yml.open() as f:
                doc = yaml.safe_load(f)
        except yaml.YAMLError as e:
            # A malformed YAML in the workflows dir is a real defect
            # (the workflow wouldn't load on Gitea either). Surface it
            # and keep going — the reaper's job is to compensate the
            # OTHER workflows even if one is broken.
            print(f"::warning::yaml parse failed for {yml.name}: {e}; skip")
            continue
        if not isinstance(doc, dict):
            print(f"::warning::workflow {yml.name} not a dict; skip")
            continue

        # Resolve workflow_id.
        name_field = doc.get("name")
        if isinstance(name_field, str) and name_field.strip():
            workflow_id = name_field.strip()
        else:
            workflow_id = yml.stem  # basename minus .yml

        # Halt-loud: `/` in workflow_id breaks ` / ` context parsing.
        if "/" in workflow_id:
            sys.stderr.write(
                f"::error::workflow name contains '/' which breaks "
                f"context parsing: {workflow_id} (file={yml.name})\n"
            )
            sys.exit(1)

        # Halt-loud: ID collision.
        if workflow_id in out:
            sys.stderr.write(
                f"::error::workflow name collision detected: {workflow_id} "
                f"(files: {sources[workflow_id]} + {yml.name})\n"
            )
            sys.exit(1)

        on_block = _on_block(doc)
        out[workflow_id] = _has_push_trigger(on_block, workflow_id)
        sources[workflow_id] = yml.name

    return out


# --------------------------------------------------------------------------
# Gitea reads
# --------------------------------------------------------------------------
def get_head_sha(branch: str) -> str:
    """HEAD SHA of `branch`. Raises ApiError on non-2xx."""
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/branches/{branch}")
    if not isinstance(body, dict):
        raise ApiError(f"branch {branch} response not a JSON object")
    commit = body.get("commit")
    if not isinstance(commit, dict):
        raise ApiError(f"branch {branch} response missing `commit` object")
    sha = commit.get("id") or commit.get("sha")
    if not isinstance(sha, str) or len(sha) < 7:
        raise ApiError(f"branch {branch} response has no usable commit SHA")
    return sha


def get_combined_status(sha: str) -> dict:
    """Combined commit status for `sha`. Gitea returns:
        {
          "state": "success" | "failure" | "pending" | "error",
          "statuses": [
            {"context": "...", "state": "...", "target_url": "...",
             "description": "..."},
            ...
          ],
          ...
        }
    Raises ApiError on non-2xx.
    """
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/commits/{sha}/status")
    if not isinstance(body, dict):
        raise ApiError(f"status for {sha} response not a JSON object")
    return body


# --------------------------------------------------------------------------
# Context parsing
# --------------------------------------------------------------------------
def parse_push_context(context: str) -> tuple[str, str] | None:
    """Parse `<workflow_name> / <job_name> (push)` into
    (workflow_name, job_name).

    Returns None if the context doesn't match the shape (caller skips).
    Strict: requires the trailing ` (push)` and at least one ` / `
    separator. Anything else is left alone.
    """
    if not context.endswith(PUSH_SUFFIX):
        return None
    head = context[: -len(PUSH_SUFFIX)]  # strip " (push)"
    if " / " not in head:
        # No workflow/job separator — not the bug shape we compensate.
        return None
    workflow_name, job_name = head.split(" / ", 1)
    return workflow_name, job_name


# --------------------------------------------------------------------------
# Compensating POST
# --------------------------------------------------------------------------
def post_compensating_status(
    sha: str,
    context: str,
    target_url: str | None,
    *,
    dry_run: bool = False,
) -> None:
    """POST a `state=success` to /repos/{o}/{r}/statuses/{sha} with the
    given context. Gitea de-dups by context (latest write wins).

    Description references this script so the compensation is
    self-documenting on the commit's status view.
    """
    payload: dict[str, Any] = {
        "context": context,
        "state": "success",
        "description": COMPENSATION_DESCRIPTION,
    }
    # Echo the original target_url when present so a human auditing
    # the (now-green) compensated status can still reach the run logs
    # that produced the original red.
    if target_url:
        payload["target_url"] = target_url

    if dry_run:
        print(
            f"::notice::[dry-run] would compensate {context!r} on {sha[:10]} "
            f"with state=success"
        )
        return

    api("POST", f"/repos/{OWNER}/{NAME}/statuses/{sha}", body=payload)
    print(f"::notice::compensated {context!r} on {sha[:10]} (state=success)")


# --------------------------------------------------------------------------
# Main reap loop
# --------------------------------------------------------------------------
def reap(
    workflow_trigger_map: dict[str, bool],
    combined: dict,
    sha: str,
    *,
    dry_run: bool = False,
) -> dict[str, Any]:
    """Walk `combined.statuses[]` and compensate where appropriate.

    Per-SHA worker. The multi-SHA orchestrator (`reap_branch`) calls
    this once per stale main commit each tick.

    Returns counters for observability:
      {compensated, preserved_real_push, preserved_unknown,
       preserved_non_failure, preserved_non_push_suffix,
       preserved_unparseable,
       compensated_contexts: [<context>, ...]}

    `compensated_contexts` is rev2-added so `reap_branch` can build
    `compensated_per_sha` without re-deriving it from the POST stream.
    """
    counters: dict[str, Any] = {
        "compensated": 0,
        "preserved_real_push": 0,
        "preserved_unknown": 0,
        "preserved_non_failure": 0,
        "preserved_non_push_suffix": 0,
        "preserved_unparseable": 0,
        "compensated_contexts": [],
    }

    statuses = combined.get("statuses") or []
    for s in statuses:
        if not isinstance(s, dict):
            continue
        context = s.get("context") or ""
        state = s.get("state") or ""

        # Only `failure` is the bug shape. `error`/`pending`/`success`
        # left alone — they have other meanings.
        if state != "failure":
            counters["preserved_non_failure"] += 1
            continue

        # Only `(push)`-suffix contexts hit the hardcoded-suffix bug.
        # Branch-protection required checks (e.g. `Secret scan / Scan
        # diff (pull_request)`) are NOT reachable from this path.
        if not context.endswith(PUSH_SUFFIX):
            counters["preserved_non_push_suffix"] += 1
            continue

        parsed = parse_push_context(context)
        if parsed is None:
            # Has ` (push)` suffix but missing ` / ` separator — not
            # the bug shape. Preserve.
            counters["preserved_unparseable"] += 1
            continue
        workflow_name, _job_name = parsed

        if workflow_name not in workflow_trigger_map:
            # Real workflow but renamed/deleted/external — we can't
            # tell if it has push trigger. Conservative: preserve.
            print(f"::notice::unknown workflow {workflow_name!r}; skip")
            counters["preserved_unknown"] += 1
            continue

        if workflow_trigger_map[workflow_name]:
            # Real push trigger → real defect signal. Preserve.
            counters["preserved_real_push"] += 1
            continue

        # Class-O: schedule/dispatch/etc.-only workflow with a fake
        # (push) status from Gitea's hardcoded-suffix bug. Compensate.
        post_compensating_status(
            sha, context, s.get("target_url"), dry_run=dry_run
        )
        counters["compensated"] += 1
        counters["compensated_contexts"].append(context)

    return counters


# --------------------------------------------------------------------------
# rev2: multi-SHA sweep over the last N commits on WATCH_BRANCH
# --------------------------------------------------------------------------
# How many main commits to sweep per tick. Sized to cover a burst-merge
# window where multiple PRs land in the 5-min interval between reaper
# ticks. Older reds falling off the window is acceptable — they were
# already stale enough that the schedule-run that posted them has long
# since been overwritten by a real push trigger. See `reference_post_
# suspension_pipeline` for the merge-cadence baseline.
DEFAULT_SWEEP_LIMIT = 10


def list_recent_commit_shas(branch: str, limit: int) -> list[str]:
    """List the most recent `limit` commit SHAs on `branch`, newest
    first.

    Wraps GET /repos/{o}/{r}/commits?sha={branch}&limit={limit}. Gitea
    1.22.6 returns a JSON list of commit objects each with a `sha` key
    (verified via vendor-truth probe 2026-05-11 against
    git.moleculesai.app — `feedback_smoke_test_vendor_truth_not_shape_match`).

    Raises ApiError on non-2xx OR on unexpected response shape. This is
    a HARD halt — without the commit list the sweep can't proceed. (The
    per-SHA error isolation downstream is a different concern: tolerating
    a transient 5xx on ONE commit's status is best-effort; losing the
    commit list itself means we don't even know which commits to try.)
    """
    _, body = api(
        "GET",
        f"/repos/{OWNER}/{NAME}/commits",
        query={"sha": branch, "limit": str(limit)},
    )
    if not isinstance(body, list):
        raise ApiError(
            f"commits listing for {branch} not a JSON array "
            f"(got {type(body).__name__})"
        )
    shas: list[str] = []
    for entry in body:
        if not isinstance(entry, dict):
            continue
        sha = entry.get("sha")
        if isinstance(sha, str) and len(sha) >= 7:
            shas.append(sha)
    if not shas:
        raise ApiError(
            f"commits listing for {branch} returned no usable SHAs"
        )
    return shas


def reap_branch(
    workflow_trigger_map: dict[str, bool],
    branch: str,
    *,
    limit: int = DEFAULT_SWEEP_LIMIT,
    dry_run: bool = False,
) -> dict[str, Any]:
    """Sweep the last `limit` commits on `branch`, applying `reap()`
    to each (with per-SHA error isolation).

    Returns aggregated counters PLUS rev2 observability fields:
      - scanned_shas: how many SHAs we actually iterated
      - compensated_per_sha: {<sha_full>: [<context>, ...]} — only
        SHAs that actually got at least one compensation are included
    """
    shas = list_recent_commit_shas(branch, limit)

    aggregate: dict[str, Any] = {
        "scanned_shas": 0,
        "compensated": 0,
        "preserved_real_push": 0,
        "preserved_unknown": 0,
        "preserved_non_failure": 0,
        "preserved_non_push_suffix": 0,
        "preserved_unparseable": 0,
        "compensated_per_sha": {},
    }

    for sha in shas:
        aggregate["scanned_shas"] += 1

        # Per-SHA error isolation (refinement #7). One transient blip
        # on a historical commit must NOT abort the whole tick — the
        # OTHER stale SHAs may still hold strandable reds.
        try:
            combined = get_combined_status(sha)
        except ApiError as e:
            print(
                f"::warning::get_combined_status({sha[:10]}) failed; "
                f"skipping this SHA: {e}"
            )
            continue

        # Cost optimization (refinement #2): the common case is a green
        # commit. Skip the per-context loop entirely when combined is
        # already success — saves a tight loop over ~20 statuses per SHA
        # on green commits, the dominant majority.
        if combined.get("state") == "success":
            continue

        per_sha = reap(
            workflow_trigger_map, combined, sha, dry_run=dry_run
        )

        # Aggregate scalar counters.
        for key in (
            "compensated",
            "preserved_real_push",
            "preserved_unknown",
            "preserved_non_failure",
            "preserved_non_push_suffix",
            "preserved_unparseable",
        ):
            aggregate[key] += per_sha[key]

        # Record per-SHA compensated contexts (only when non-empty —
        # keep the summary readable when most SHAs are no-ops).
        contexts = per_sha.get("compensated_contexts") or []
        if contexts:
            aggregate["compensated_per_sha"][sha] = list(contexts)

    return aggregate


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Skip the compensating POST; print what would be done.",
    )
    parser.add_argument(
        "--limit",
        type=int,
        default=DEFAULT_SWEEP_LIMIT,
        help=(
            "How many recent commits on WATCH_BRANCH to sweep per tick "
            f"(default: {DEFAULT_SWEEP_LIMIT})."
        ),
    )
    args = parser.parse_args()

    _require_runtime_env()

    workflow_trigger_map = scan_workflows(WORKFLOWS_DIR)
    print(
        f"::notice::scanned {len(workflow_trigger_map)} workflows; "
        f"push-triggered={sum(1 for v in workflow_trigger_map.values() if v)}, "
        f"class-O candidates={sum(1 for v in workflow_trigger_map.values() if not v)}"
    )

    counters = reap_branch(
        workflow_trigger_map,
        WATCH_BRANCH,
        limit=args.limit,
        dry_run=args.dry_run,
    )

    # Observability: print one JSON line summarising the tick. Loki
    # ingestion via the runner's stdout (`source="gitea-actions"`).
    print(
        "status-reaper summary: "
        + json.dumps(
            {
                "branch": WATCH_BRANCH,
                "dry_run": args.dry_run,
                "limit": args.limit,
                **counters,
            },
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
