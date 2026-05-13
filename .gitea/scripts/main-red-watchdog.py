#!/usr/bin/env python3
"""main-red-watchdog — Option C of the "main NEVER goes red" directive.

Tracking: molecule-core#420.

What it does (one cron tick):
  1. GET /api/v1/repos/{owner}/{repo}/branches/{watch_branch}
     → current HEAD SHA on the watched branch.
  2. GET /api/v1/repos/{owner}/{repo}/commits/{SHA}/status
     → combined status + per-context statuses.
  3. If combined state is `failure` (or any individual status is
     `failure`): open or PATCH an idempotent
     `[main-red] {repo}: {SHA[:10]}` issue. Body lists each failed
     status context with `target_url` + `description`.
  4. If combined state is `success`: close any open `[main-red]
     {repo}: ...` issue on a previous SHA with a
     "main returned to green at SHA {current_SHA}" comment.
  5. Emit one Loki-shaped JSON line via `logger -t main-red-watchdog`
     so `reference_obs_stack_phase1`'s Vector → Loki path ingests an
     alert event (queryable in Grafana as
     `{tenant="operator-host"} |~ "main-red-watchdog"`).

What it does NOT do:
  - Auto-revert anything. Option B is explicitly rejected per
    `feedback_no_such_thing_as_flakes` + `feedback_fix_root_not_symptom`.
  - Page on its own failures. If api() raises ApiError (transient
    Gitea outage), the workflow run fails LOUDLY by re-raise — exactly
    the contract `feedback_api_helper_must_raise_not_return_dict`
    enforces. Silent fallthrough would re-introduce the duplicate-issue
    regression class.
  - Exit non-zero on RED. The issue IS the alarm; failing the watchdog
    on red would double-page (red workflow + open issue) and create
    silent-loop risk if the watchdog itself flakes.

Idempotency strategy:
  Title is keyed on `{SHA[:10]}` (commit-scoped), NOT just `main`.
  Rationale:
    - A fix-forward changes HEAD → next cron tick sees a new SHA;
      auto-close logic closes the prior `[main-red] OLD_SHA` issue and
      (if the new HEAD is also red, e.g. a different test fails) files
      a fresh `[main-red] NEW_SHA`. Lineage is preserved.
    - A revert that happens to land back on a previously-red SHA
      (rare) would refer to a CLOSED issue; the watchdog never reopens.
      That's a deliberate trade-off — the operator will see the latest
      open issue's `closed` event in the activity feed.

This module is import-safe: tests import individual functions without
invoking main(), so module-level reads use env-with-default and the
runtime contract enforcement lives in `_require_runtime_env()`.

Run locally (dry-run, no API mutation):
    GITEA_TOKEN=... GITEA_HOST=git.moleculesai.app REPO=owner/repo \\
      WATCH_BRANCH=main RED_LABEL=tier:high \\
      python3 .gitea/scripts/main-red-watchdog.py --dry-run
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any


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
RED_LABEL = _env("RED_LABEL", default="tier:high")

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""

# Title prefix — kept short and stable so the idempotency search can
# match by exact title without parsing.
TITLE_PREFIX = "[main-red]"


def _require_runtime_env() -> None:
    """Enforce env contract — called from `main()` only.

    Tests import individual functions without setting the full env
    contract. Mirrors the CP `ci-required-drift.py` pattern so the
    runtime guard is a single chokepoint.
    """
    for key in ("GITEA_TOKEN", "GITEA_HOST", "REPO", "WATCH_BRANCH", "RED_LABEL"):
        if not os.environ.get(key):
            sys.stderr.write(f"::error::missing required env var: {key}\n")
            sys.exit(2)


# --------------------------------------------------------------------------
# Tiny HTTP helper — raises on non-2xx + on JSON-decode-of-expected-JSON.
# --------------------------------------------------------------------------
class ApiError(RuntimeError):
    """Raised when a Gitea API call cannot be trusted to have succeeded.

    Covers non-2xx HTTP status AND 2xx with an unparseable JSON body on
    endpoints documented to return JSON. Callers that swallow this and
    proceed risk e.g. creating duplicate `[main-red]` issues when a
    transient 500 hides an existing match. Per
    `feedback_api_helper_must_raise_not_return_dict`: soft-failure is
    opt-in via `expect_json=False`, never the default.
    """


def api(
    method: str,
    path: str,
    *,
    body: dict | None = None,
    query: dict[str, str] | None = None,
    expect_json: bool = True,
) -> tuple[int, Any]:
    """Tiny HTTP helper around urllib.

    Raises ApiError on any non-2xx response, and on JSON-decode failure
    when `expect_json=True` (the default for read-shaped paths). Mirrors
    the CP ci-required-drift.py contract exactly so behaviour is
    cross-checkable.
    """
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
        raise ApiError(f"{method} {path} → HTTP {status}: {snippet}")

    if not raw:
        return status, None
    try:
        return status, json.loads(raw)
    except json.JSONDecodeError as e:
        if expect_json:
            raise ApiError(
                f"{method} {path} → HTTP {status} but body is not JSON: {e}"
            ) from e
        # Opt-in raw fallthrough for endpoints with known echo-quirks
        # (`feedback_gitea_create_api_unparseable_response`). Caller
        # MUST verify success via a follow-up GET, not by trusting body.
        return status, {"_raw": raw.decode("utf-8", errors="replace")}


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
            {"context": "...", "state": "success|failure|pending|error",
             "target_url": "...", "description": "..."},
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


def is_red(status: dict) -> tuple[bool, list[dict]]:
    """Return (is_red, failed_statuses).

    A commit is "red" if combined state is `failure` OR any individual
    status entry is in {`failure`, `error`}. `pending` and `success`
    do not trip the watchdog — pending means CI is still running, and
    that's the normal state immediately after a merge.

    `failed_statuses` is the list of per-context entries whose own
    `state` is in the red set; useful for the issue body.
    """
    combined = status.get("state")
    statuses = status.get("statuses") or []
    red_states = {"failure", "error"}
    # Schema asymmetry: top-level combined uses `state`, but per-entry
    # items in `statuses[]` use `status` in Gitea 1.22.6. Prefer
    # `status`; fall back to `state` defensively. Verified empirically
    # 2026-05-12 03:42Z. Pre-rev4 code only read `state` from per-entry
    # items → failed[] always empty → render_body always showed the
    # "no per-context entries were in a red state" fallback even when
    # the combined-state correctly flagged red. See
    # `feedback_smoke_test_vendor_truth_not_shape_match`.
    def _entry_state(s: dict) -> str:
        return s.get("status") or s.get("state") or ""

    failed = [
        s for s in statuses
        if isinstance(s, dict) and _entry_state(s) in red_states
    ]
    return (combined in red_states or bool(failed), failed)


# --------------------------------------------------------------------------
# Issue file / update / close
# --------------------------------------------------------------------------
def title_for(sha: str) -> str:
    """Idempotency key — `[main-red] {repo}: {SHA[:10]}`.

    Commit-scoped. A fix-forward to a new SHA produces a new title; the
    prior issue auto-closes via `close_open_red_issues_for_other_shas`.
    """
    return f"{TITLE_PREFIX} {REPO}: {sha[:10]}"


def list_open_red_issues() -> list[dict]:
    """All open issues whose title starts with `[main-red] {repo}: `.

    Per Five-Axis review on CP#112 (`feedback_api_helper_must_raise_not_return_dict`):
    api() raises on non-2xx; we let it propagate. Returning [] on a
    transient 500 would cause auto-close to skip the cleanup AND the
    file-or-update path to POST a duplicate — exactly the regression
    class the helper-raises contract closes.

    Gitea issue search returns at most 50/page; we only need open
    `[main-red]` issues which are by design ≤ 1 at any time per repo,
    so a single page is enough.
    """
    _, results = api(
        "GET",
        f"/repos/{OWNER}/{NAME}/issues",
        query={"state": "open", "type": "issues", "limit": "50"},
    )
    if not isinstance(results, list):
        raise ApiError(
            f"issue search returned non-list body (got {type(results).__name__})"
        )
    prefix = f"{TITLE_PREFIX} {REPO}: "
    return [i for i in results if isinstance(i, dict)
            and isinstance(i.get("title"), str)
            and i["title"].startswith(prefix)]


def find_open_issue_for_sha(sha: str) -> dict | None:
    """Return the existing open `[main-red] {repo}: {SHA[:10]}` issue,
    or None if no such issue is open.

    `None` means "search succeeded, no match" — NOT "search failed".
    api() raises ApiError on any non-2xx; the caller can let that
    propagate so a transient outage fails loudly instead of silently
    duplicating.
    """
    target = title_for(sha)
    for issue in list_open_red_issues():
        if issue.get("title") == target:
            return issue
    return None


def render_body(sha: str, failed: list[dict], debug: dict) -> str:
    """Issue body. Markdown. Mirrors CP#112's render_body shape."""
    lines = [
        f"# Main is RED on `{REPO}` at `{sha[:10]}`",
        "",
        f"Commit: <https://{GITEA_HOST}/{REPO}/commit/{sha}>",
        "",
        "Auto-filed by `.gitea/workflows/main-red-watchdog.yml` (Option C "
        "of the [main-never-red directive]"
        f"(https://{GITEA_HOST}/molecule-ai/molecule-core/issues/420)). "
        "Per `feedback_no_such_thing_as_flakes` + "
        "`feedback_fix_root_not_symptom`: investigate the root cause; do "
        "NOT revert as a reflex. The watchdog itself never reverts.",
        "",
        "## Failed status contexts",
        "",
    ]
    if not failed:
        lines.append(
            "_(Combined state reported `failure`/`error` but no per-context "
            "entries were in a red state. This usually means a CI emitter "
            "set combined-status directly without a per-context status. "
            "Check the most recent workflow run for `main` and trace from "
            "there.)_"
        )
    else:
        for s in failed:
            ctx = s.get("context", "(no context)")
            # Per-entry key is `status` in Gitea 1.22.6, not `state`
            # (see _entry_state in is_red). Fallback for forward-compat.
            state = s.get("status") or s.get("state") or "(no state)"
            url = s.get("target_url") or ""
            desc = (s.get("description") or "").strip()
            entry = f"- **{ctx}** — `{state}`"
            if url:
                entry += f" → [logs]({url})"
            if desc:
                entry += f"\n  - {desc}"
            lines.append(entry)
    lines.extend([
        "",
        "## Resolution path",
        "",
        "1. Read the failed logs (links above).",
        "2. If reproducible locally, fix forward in a PR targeting `main`.",
        "3. If the failure is a real flake — STOP. Per "
        "`feedback_no_such_thing_as_flakes`, intermittent failures are "
        "real bugs. Investigate to root cause; do not mark as flake.",
        "4. If the failure is blocking unrelated work for >1 hour, file a "
        "follow-up issue and assign someone. Do NOT revert without a "
        "human GO per `feedback_prod_apply_needs_hongming_chat_go` "
        "(branch protection is a prod surface).",
        "",
        "## Debug",
        "",
        "```json",
        json.dumps(debug, indent=2, sort_keys=True),
        "```",
        "",
        "_This issue is idempotent: the watchdog runs hourly at `:05` "
        "and edits this body in place. When `main` returns to green, the "
        "watchdog will close this issue automatically with a "
        "\"main returned to green\" comment._",
    ])
    return "\n".join(lines)


def emit_loki_event(event_type: str, sha: str, failed_contexts: list[str]) -> None:
    """Emit a JSON line to syslog tag `main-red-watchdog` for
    `reference_obs_stack_phase1` (Vector → Loki).

    Best-effort: if `logger` isn't on PATH (e.g. local dev macOS without
    util-linux logger), print to stderr instead. The Gitea Actions
    Ubuntu runner has util-linux preinstalled.

    Loki labels: the workflow runs on the Ubuntu runner where Vector is
    NOT configured (Vector lives on the operator host + tenants per
    `reference_obs_stack_phase1`). The Loki line is still emitted as
    stdout JSON so the workflow log itself is parseable; treat the
    syslog call as belt-and-braces for the cases where this script is
    invoked from a host that DOES have Vector (e.g. operator-host cron
    fallback in a follow-up PR).
    """
    payload = {
        "event_type": event_type,
        "repo": REPO,
        "sha": sha,
        "failed_contexts": failed_contexts,
    }
    line = json.dumps(payload, sort_keys=True)
    # Always print to stdout so the workflow log captures it (machine-
    # readable; `gitea run logs` + Loki ingestion via the operator-host
    # journald → Vector → Loki path will see this from runners that
    # forward stdout). Loki query:
    #   {source="gitea-actions"} |~ "main_red_detected"
    print(f"main-red-watchdog event: {line}")
    # Best-effort syslog tag so a future "run from operator-host cron"
    # path picks it up directly via the existing Vector pipeline.
    if shutil.which("logger"):
        try:
            subprocess.run(
                ["logger", "-t", "main-red-watchdog", line],
                check=False,
                timeout=5,
            )
        except (OSError, subprocess.SubprocessError) as e:
            sys.stderr.write(f"::warning::logger call failed: {e}\n")


def file_or_update_red(
    sha: str,
    failed: list[dict],
    debug: dict,
    *,
    dry_run: bool = False,
) -> None:
    """Open a new `[main-red] {repo}: {SHA[:10]}` issue, or PATCH the
    existing one's body. Idempotent by title."""
    title = title_for(sha)
    body = render_body(sha, failed, debug)

    if dry_run:
        print(f"::notice::[dry-run] would file/update main-red issue for {sha[:10]}")
        print("::group::[dry-run] title")
        print(title)
        print("::endgroup::")
        print("::group::[dry-run] body")
        print(body)
        print("::endgroup::")
        return

    existing = find_open_issue_for_sha(sha)
    if existing:
        num = existing["number"]
        api("PATCH", f"/repos/{OWNER}/{NAME}/issues/{num}", body={"body": body})
        print(f"::notice::Updated existing main-red issue #{num} for {sha[:10]}")
        return

    _, created = api(
        "POST",
        f"/repos/{OWNER}/{NAME}/issues",
        body={"title": title, "body": body, "labels": []},
    )
    if not isinstance(created, dict):
        raise ApiError("POST issue response not a JSON object")
    new_num = created.get("number")
    print(f"::warning::Filed new main-red issue #{new_num} for {sha[:10]}")

    # Apply RED_LABEL by id. Gitea's add-labels endpoint takes IDs, not
    # names (`feedback_gitea_label_delete_by_id` — same rule for add).
    # Best-effort: label failure is logged but does not fail the run.
    try:
        _, labels = api("GET", f"/repos/{OWNER}/{NAME}/labels")
    except ApiError as e:
        sys.stderr.write(f"::warning::could not list labels: {e}\n")
        return
    label_id = None
    if isinstance(labels, list):
        for lbl in labels:
            if isinstance(lbl, dict) and lbl.get("name") == RED_LABEL:
                label_id = lbl.get("id")
                break
    if label_id is not None and new_num:
        try:
            api(
                "POST",
                f"/repos/{OWNER}/{NAME}/issues/{new_num}/labels",
                body={"labels": [label_id]},
            )
        except ApiError as e:
            sys.stderr.write(
                f"::warning::could not apply label '{RED_LABEL}' to #{new_num}: {e}\n"
            )
    else:
        sys.stderr.write(f"::warning::label '{RED_LABEL}' not found on repo\n")


def close_open_red_issues_for_other_shas(
    current_sha: str,
    *,
    dry_run: bool = False,
) -> int:
    """When main is green at current_sha, close any open `[main-red]`
    issues whose title references a different SHA. Returns the number
    of issues closed.

    Lineage note: we only close issues whose title prefix matches; if
    a human renamed the issue or added a suffix this won't touch it.
    That's intentional — manual editorial state takes precedence.
    """
    target_title = title_for(current_sha)
    open_red = list_open_red_issues()
    closed = 0
    for issue in open_red:
        if issue.get("title") == target_title:
            # Same SHA — caller should not have invoked this if main is
            # green. Skip defensively.
            continue
        num = issue.get("number")
        if not isinstance(num, int):
            continue
        comment = (
            f"`main` returned to green at SHA `{current_sha}` "
            f"(<https://{GITEA_HOST}/{REPO}/commit/{current_sha}>). "
            "Closing automatically. If the underlying root cause is "
            "not yet understood, reopen this issue and file a "
            "postmortem — green-by-flake is still a bug per "
            "`feedback_no_such_thing_as_flakes`."
        )
        if dry_run:
            print(f"::notice::[dry-run] would close issue #{num} ({issue.get('title')})")
            closed += 1
            continue
        # Comment first, then close. Order matters: a closed issue can
        # still receive comments, but the activity-feed ordering reads
        # better with the explanation arriving just before the close.
        api(
            "POST",
            f"/repos/{OWNER}/{NAME}/issues/{num}/comments",
            body={"body": comment},
        )
        api(
            "PATCH",
            f"/repos/{OWNER}/{NAME}/issues/{num}",
            body={"state": "closed"},
        )
        print(f"::notice::Closed main-red issue #{num} (green at {current_sha[:10]})")
        closed += 1
    return closed


# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------
def _parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        prog="main-red-watchdog",
        description="Detect post-merge CI red on the watched branch and "
        "file an idempotent issue. Option C of the main-never-red directive.",
    )
    p.add_argument(
        "--dry-run",
        action="store_true",
        help="Detect + print the would-be issue title/body to stdout; do "
        "NOT POST/PATCH/close any issues. Useful for local testing.",
    )
    return p.parse_args(argv)


def run_once(*, dry_run: bool = False) -> int:
    """One watchdog tick. Returns 0 on green or red-issue-filed; lets
    ApiError propagate on transient outage (workflow run fails loudly,
    which is correct per the helper-raises contract)."""
    sha = get_head_sha(WATCH_BRANCH)
    status = get_combined_status(sha)
    red, failed = is_red(status)

    debug = {
        "branch": WATCH_BRANCH,
        "sha": sha,
        "combined_state": status.get("state"),
        "failed_contexts": [s.get("context") for s in failed],
        "all_contexts": [
            # Per-entry key is `status` in Gitea 1.22.6, not `state`.
            # Pre-rev4 debug output reported `state: None` for every
            # context, making run logs useless for triage.
            {"context": s.get("context"),
             "state": s.get("status") or s.get("state")}
            for s in (status.get("statuses") or [])
            if isinstance(s, dict)
        ],
    }

    if red:
        failed_ctxs = [s.get("context") for s in failed if s.get("context")]
        emit_loki_event("main_red_detected", sha, failed_ctxs)
        print(f"::warning::main is RED at {sha[:10]} on {WATCH_BRANCH}: "
              f"{len(failed)} failed context(s)")
        file_or_update_red(sha, failed, debug, dry_run=dry_run)
    else:
        # Green (or pending — pending is treated as not-red so we don't
        # spam during the post-merge CI window). Close any stale issues
        # from earlier SHAs only when we're actually green; pending
        # means CI hasn't finished and the prior issue might still be
        # accurate.
        if status.get("state") == "success":
            closed = close_open_red_issues_for_other_shas(sha, dry_run=dry_run)
            if closed:
                emit_loki_event(
                    "main_returned_to_green", sha,
                    [],
                )
            print(f"::notice::main is GREEN at {sha[:10]} on {WATCH_BRANCH} "
                  f"(closed {closed} stale issue(s))")
        else:
            print(f"::notice::main is PENDING at {sha[:10]} on {WATCH_BRANCH} "
                  f"(combined state={status.get('state')!r}; no action)")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = _parse_args(argv)
    _require_runtime_env()
    return run_once(dry_run=args.dry_run)


if __name__ == "__main__":
    sys.exit(main())
