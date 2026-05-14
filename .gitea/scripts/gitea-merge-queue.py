#!/usr/bin/env python3
"""gitea-merge-queue — conservative serialized merge bot for Gitea.

Gitea 1.22.6 has auto-merge (`pull_auto_merge`) but no GitHub-style merge
queue. This script provides the missing serialized policy in user space:

1. Pick the oldest open PR carrying QUEUE_LABEL.
2. Refuse to act unless main is green.
3. Refuse fork PRs; the queue may only mutate same-repo branches.
4. If the PR branch does not contain current main, call Gitea's
   /pulls/{n}/update endpoint and stop. CI must rerun on the updated head.
5. If the updated PR head has all required contexts green, merge with the
   non-bypass merge actor token.

The script is intentionally one-PR-per-run. Workflow/cron concurrency should
serialize invocations so two green PRs cannot merge against the same main.
"""

from __future__ import annotations

import argparse
import dataclasses
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any


def _env(key: str, *, default: str = "") -> str:
    return os.environ.get(key, default)


GITEA_TOKEN = _env("GITEA_TOKEN")
GITEA_HOST = _env("GITEA_HOST")
REPO = _env("REPO")
WATCH_BRANCH = _env("WATCH_BRANCH", default="main")
QUEUE_LABEL = _env("QUEUE_LABEL", default="merge-queue")
HOLD_LABEL = _env("HOLD_LABEL", default="merge-queue-hold")
UPDATE_STYLE = _env("UPDATE_STYLE", default="merge")
REQUIRED_CONTEXTS_RAW = _env(
    "REQUIRED_CONTEXTS",
    default=(
        "CI / all-required (pull_request),"
        "sop-checklist / all-items-acked (pull_request)"
    ),
)
# Required contexts for push (main/staging) runs. The push CI uses the same
# aggregator names with " (push)" suffix. Checking these explicitly instead of
# the combined state avoids false-pause when non-blocking jobs (e.g. Platform
# Go with continue-on-error: true due to mc#774) have failed — their failures
# pollute the combined state but do not block merges.
PUSH_REQUIRED_CONTEXTS_RAW = _env(
    "PUSH_REQUIRED_CONTEXTS",
    default="CI / all-required (push)",
)

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""


class ApiError(RuntimeError):
    pass


@dataclasses.dataclass(frozen=True)
class MergeDecision:
    ready: bool
    action: str
    reason: str


def _require_runtime_env() -> None:
    for key in ("GITEA_TOKEN", "GITEA_HOST", "REPO", "WATCH_BRANCH", "QUEUE_LABEL"):
        if not os.environ.get(key):
            sys.stderr.write(f"::error::missing required env var: {key}\n")
            sys.exit(2)
    if UPDATE_STYLE not in {"merge", "rebase"}:
        sys.stderr.write("::error::UPDATE_STYLE must be merge or rebase\n")
        sys.exit(2)


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
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        status = exc.code

    if not (200 <= status < 300):
        snippet = raw[:500].decode("utf-8", errors="replace") if raw else ""
        raise ApiError(f"{method} {path} -> HTTP {status}: {snippet}")
    if not raw:
        return status, None
    try:
        return status, json.loads(raw)
    except json.JSONDecodeError as exc:
        if expect_json:
            raise ApiError(f"{method} {path} -> HTTP {status} non-JSON: {exc}") from exc
        return status, {"_raw": raw.decode("utf-8", errors="replace")}


def required_contexts(raw: str) -> list[str]:
    return [part.strip() for part in raw.split(",") if part.strip()]


def push_required_contexts() -> list[str]:
    """Required contexts for push (branch) CI runs. See PUSH_REQUIRED_CONTEXTS_RAW."""
    return required_contexts(PUSH_REQUIRED_CONTEXTS_RAW)


def status_state(status: dict) -> str:
    return str(status.get("status") or status.get("state") or "").lower()


def latest_statuses_by_context(statuses: list[dict]) -> dict[str, dict]:
    # Gitea /statuses endpoint returns entries in ascending id order (oldest
    # first). We need the LAST occurrence of each context, so iterate in
    # reverse to prefer newer entries.
    latest: dict[str, dict] = {}
    for status in reversed(statuses):
        context = status.get("context")
        if isinstance(context, str):
            latest[context] = status  # overwrite: reverse order → newest wins
    return latest


def required_contexts_green(
    latest_statuses: dict[str, dict],
    contexts: list[str],
) -> tuple[bool, list[str]]:
    missing_or_bad: list[str] = []
    for context in contexts:
        status = latest_statuses.get(context)
        state = status_state(status or {})
        if state != "success":
            missing_or_bad.append(f"{context}={state or 'missing'}")
    return not missing_or_bad, missing_or_bad


def label_names(issue: dict) -> set[str]:
    return {
        label["name"]
        for label in issue.get("labels", [])
        if isinstance(label, dict) and isinstance(label.get("name"), str)
    }


def choose_next_queued_issue(
    issues: list[dict],
    *,
    queue_label: str,
    hold_label: str = "",
) -> dict | None:
    candidates = []
    for issue in issues:
        labels = label_names(issue)
        if queue_label not in labels:
            continue
        if hold_label and hold_label in labels:
            continue
        if "pull_request" not in issue:
            continue
        candidates.append(issue)
    candidates.sort(key=lambda issue: (issue.get("created_at") or "", int(issue["number"])))
    return candidates[0] if candidates else None


def pr_contains_base_sha(commits: list[dict], base_sha: str) -> bool:
    for commit in commits:
        sha = commit.get("sha") or commit.get("id")
        if sha == base_sha:
            return True
    return False


def pr_has_current_base(pr: dict, commits: list[dict], main_sha: str) -> bool:
    if pr.get("merge_base") == main_sha:
        return True
    return pr_contains_base_sha(commits, main_sha)


def evaluate_merge_readiness(
    *,
    main_status: dict,
    pr_status: dict,
    required_contexts: list[str],
    pr_has_current_base: bool,
) -> MergeDecision:
    # Check push-required contexts explicitly instead of combined state.
    # Combined state can be "failure" due to non-blocking jobs
    # (continue-on-error: true) that don't actually gate merges.
    # CI / all-required (push) is the authoritative gate — it respects
    # continue-on-error and correctly aggregates all blocking failures.
    main_latest = latest_statuses_by_context(main_status.get("statuses") or [])
    main_ok, main_bad = required_contexts_green(main_latest, push_required_contexts())
    if not main_ok:
        return MergeDecision(False, "pause", "main required contexts not green: " + ", ".join(main_bad))
    if not pr_has_current_base:
        return MergeDecision(False, "update", "PR head does not contain current main")

    # Check explicit required contexts instead of combined state. Combined state
    # can be "failure" due to non-blocking jobs with continue-on-error: true
    # (e.g. publish-runtime-autobump/pr-validate, qa-review on stale tokens).
    # The required_contexts list is the authoritative gate — it includes only
    # the checks that actually block merges.
    latest = latest_statuses_by_context(pr_status.get("statuses") or [])
    ok, missing_or_bad = required_contexts_green(latest, required_contexts)
    if not ok:
        return MergeDecision(False, "wait", "required contexts not green: " + ", ".join(missing_or_bad))
    return MergeDecision(True, "merge", "ready")


def get_branch_head(branch: str) -> str:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/branches/{branch}")
    commit = body.get("commit") if isinstance(body, dict) else None
    sha = commit.get("id") if isinstance(commit, dict) else None
    if not isinstance(sha, str) or len(sha) < 7:
        raise ApiError(f"branch {branch} response missing commit id")
    return sha


def get_combined_status(sha: str) -> dict:
    """Combined status + all individual statuses for `sha`.

    The /status endpoint caps the `statuses` array at 30 entries (Gitea
    default page size), so we fetch the full list via /statuses with a
    higher limit. The combined `state` still comes from /status.
    """
    _, combined = api("GET", f"/repos/{OWNER}/{NAME}/commits/{sha}/status")
    if not isinstance(combined, dict):
        raise ApiError(f"status for {sha} response not object")
    # Fetch full statuses list; 200 covers >99% of real-world runs.
    # The list is ordered ascending by id (oldest first) — callers must
    # iterate in reverse to get the newest entry per context.
    # Best-effort: large repos (main with 550+ statuses) may time out.
    # On timeout, fall back to the statuses[] already in the combined
    # response (usually 30 entries — enough for most PRs, enough for
    # main's early push-required contexts).
    try:
        _, all_statuses = api(
            "GET",
            f"/repos/{OWNER}/{NAME}/commits/{sha}/statuses",
            query={"limit": "50"},
        )
        if isinstance(all_statuses, list):
            combined["statuses"] = all_statuses
    except (ApiError, urllib.error.URLError, TimeoutError, OSError) as exc:
        # URLError covers network-level failures (DNS, refused, timeout).
        # TimeoutError and OSError cover socket-level timeouts.
        sys.stderr.write(f"::warning::could not fetch full statuses list for {sha[:8]}: {exc}\n")
        # Fall back to the statuses[] already in the combined response.
        pass
    return combined


def list_queued_issues() -> list[dict]:
    _, body = api(
        "GET",
        f"/repos/{OWNER}/{NAME}/issues",
        query={
            "state": "open",
            "type": "pulls",
            "labels": QUEUE_LABEL,
            "limit": "50",
        },
    )
    if not isinstance(body, list):
        raise ApiError("queued issues response not list")
    return body


def get_pull(pr_number: int) -> dict:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}")
    if not isinstance(body, dict):
        raise ApiError(f"PR #{pr_number} response not object")
    return body


def get_pull_commits(pr_number: int) -> list[dict]:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/commits")
    if not isinstance(body, list):
        raise ApiError(f"PR #{pr_number} commits response not list")
    return body


def post_comment(pr_number: int, body: str, *, dry_run: bool) -> None:
    print(f"::notice::comment PR #{pr_number}: {body.splitlines()[0][:160]}")
    if dry_run:
        return
    api("POST", f"/repos/{OWNER}/{NAME}/issues/{pr_number}/comments", body={"body": body})


def update_pull(pr_number: int, *, dry_run: bool) -> None:
    print(f"::notice::updating PR #{pr_number} with base branch via style={UPDATE_STYLE}")
    if dry_run:
        return
    api(
        "POST",
        f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/update",
        query={"style": UPDATE_STYLE},
        expect_json=False,
    )


def merge_pull(pr_number: int, *, dry_run: bool) -> None:
    payload = {
        "Do": "merge",
        "MergeTitleField": f"Merge PR #{pr_number} via Gitea merge queue",
        "MergeMessageField": (
            "Serialized merge by gitea-merge-queue after current-main, "
            "SOP, and required CI checks were green."
        ),
    }
    print(f"::notice::merging PR #{pr_number}")
    if dry_run:
        return
    api("POST", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/merge", body=payload, expect_json=False)


def process_once(*, dry_run: bool = False) -> int:
    contexts = required_contexts(REQUIRED_CONTEXTS_RAW)
    main_sha = get_branch_head(WATCH_BRANCH)
    main_status = get_combined_status(main_sha)
    # Check push-required contexts explicitly instead of combined state.
    # See evaluate_merge_readiness for rationale.
    main_latest = latest_statuses_by_context(main_status.get("statuses") or [])
    main_ok, main_bad = required_contexts_green(main_latest, push_required_contexts())
    if not main_ok:
        print(f"::notice::queue paused: {WATCH_BRANCH}@{main_sha[:8]} required contexts not green: {', '.join(main_bad)}")
        return 0

    issue = choose_next_queued_issue(
        list_queued_issues(),
        queue_label=QUEUE_LABEL,
        hold_label=HOLD_LABEL,
    )
    if not issue:
        print("::notice::merge queue empty")
        return 0

    pr_number = int(issue["number"])
    pr = get_pull(pr_number)
    if pr.get("state") != "open":
        print(f"::notice::PR #{pr_number} is not open; skipping")
        return 0
    if pr.get("base", {}).get("ref") != WATCH_BRANCH:
        post_comment(pr_number, f"merge-queue: skipped; base branch is not `{WATCH_BRANCH}`.", dry_run=dry_run)
        return 0
    if pr.get("head", {}).get("repo_id") != pr.get("base", {}).get("repo_id"):
        post_comment(pr_number, "merge-queue: skipped; fork PRs are not supported by the serialized queue.", dry_run=dry_run)
        return 0

    head_sha = pr.get("head", {}).get("sha")
    if not isinstance(head_sha, str) or len(head_sha) < 7:
        raise ApiError(f"PR #{pr_number} missing head sha")
    commits = get_pull_commits(pr_number)
    current_base = pr_has_current_base(pr, commits, main_sha)
    pr_status = get_combined_status(head_sha)
    decision = evaluate_merge_readiness(
        main_status=main_status,
        pr_status=pr_status,
        required_contexts=contexts,
        pr_has_current_base=current_base,
    )

    print(f"::notice::PR #{pr_number} decision={decision.action}: {decision.reason}")
    if decision.action == "update":
        update_pull(pr_number, dry_run=dry_run)
        post_comment(
            pr_number,
            (
                f"merge-queue: updated this branch with `{WATCH_BRANCH}` at "
                f"`{main_sha[:12]}`. Waiting for CI on the refreshed head."
            ),
            dry_run=dry_run,
        )
        return 0
    if decision.ready:
        latest_main_sha = get_branch_head(WATCH_BRANCH)
        if latest_main_sha != main_sha:
            print(
                f"::notice::main moved {main_sha[:8]} -> {latest_main_sha[:8]}; "
                "deferring to next tick"
            )
            return 0
        merge_pull(pr_number, dry_run=dry_run)
        return 0
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    _require_runtime_env()
    try:
        return process_once(dry_run=args.dry_run)
    except ApiError as exc:
        # API errors (401/403/404/500) are transient for a queue tick —
        # log and exit 0 so the workflow is not marked failed and the next
        # tick can retry. Returning non-zero would permanently fail the
        # workflow run, blocking future ticks.
        sys.stderr.write(f"::error::queue API error: {exc}\n")
        return 0
    except urllib.error.URLError as exc:
        sys.stderr.write(f"::error::queue network error: {exc}\n")
        return 0
    except TimeoutError as exc:
        sys.stderr.write(f"::error::queue timeout: {exc}\n")
        return 0


if __name__ == "__main__":
    sys.exit(main())
