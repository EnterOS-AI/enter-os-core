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
      WATCH_BRANCH=main RED_LABEL=ci-bp-drift \\
      python3 .gitea/scripts/main-red-watchdog.py --dry-run
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
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
RED_LABEL = _env("RED_LABEL", default="ci-bp-drift")

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""

# Title prefix — kept short and stable so the idempotency search can
# match by exact title without parsing.
TITLE_PREFIX = "[main-red]"

# Contexts that are scheduled or non-required — their pending/failure
# state should not block stale-issue closeout (mc#1789).
SCHEDULED_CONTEXT_PATTERNS = (
    "Staging SaaS smoke",
    "Continuous synthetic E2E",
    "main-red-watchdog",
    "ci-arm64-advisory",
)

# Settling window (seconds) between initial red detection and the
# pre-file recheck. The recheck filters out the two largest false-
# positive classes seen in mc#1597..1630 (task #394, 2026-05-21):
#   1. HEAD moved on (a new commit landed mid-tick) — the prior red SHA
#      is no longer authoritative; let the next cron tick re-evaluate.
#   2. Combined status recovered on the SAME SHA (transient
#      cancel-cascade rolled forward to success on retry).
# 90s is well below the hourly cron cadence; a real failure that
# persists past it is the one we want surfaced.
# Override with WATCHDOG_RECHECK_DELAY_SECS for tests / local probes
# (the test suite stubs time.sleep to a no-op).
RECHECK_DELAY_SECS = int(_env("WATCHDOG_RECHECK_DELAY_SECS", default="90"))


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
# action_run.status resolver — extensibility hook for task #394.
# --------------------------------------------------------------------------
def _resolve_action_run_status(target_url: str) -> int | None:
    """Resolve the underlying Gitea `action_run.status` integer for the
    run referenced by `target_url`, returning None if the resolver
    cannot reach an authoritative source from the runner.

    Canonical Gitea 1.22.6 enum (per `models/actions/status.go` +
    `reference_gitea_action_status_enum_corrected_2026_05_19`):
        1=Success, 2=Failure, 3=Cancelled, 4=Skipped,
        5=Waiting,  6=Running, 7=Blocked
    Only `status == 2` is a real defect; status=3 is cancel-cascade and
    status=1 is an emission artifact (Gitea wrote a 'failure' commit_status
    row for a run that actually succeeded — observed empirically on
    `publish-canvas-image` jobs at SHAs in mc#1597..1630).

    CURRENT STATE (2026-05-20, verified): Gitea 1.22.6 exposes NO REST
    endpoint for `action_run.status`. Probed:
        /api/v1/repos/{o}/{r}/actions/runs/{id}   → HTTP 404
        /api/v1/repos/{o}/{r}/actions/jobs/{id}   → HTTP 404
        /api/v1/repos/{o}/{r}/actions/tasks/{id}  → HTTP 404
        /swagger.v1.json paths containing 'actions' → secrets+variables+runners only
    The SPA backend (`/{repo}/actions/runs/{id}/jobs/{idx}` POST) requires
    a session CSRF token, unreachable from a runner. The only authoritative
    source today is direct DB access (`mol_action_status` on op-host,
    `docker exec molecule-postgres-1 psql ...`), which the runner cannot
    reach.

    Therefore: this hook returns None on every call. Callers MUST fall
    back to the description-string filter (existing) plus the HEAD
    recheck (this PR). When a future Gitea release (>=1.23 expected) or
    an op-host proxy exposes the endpoint, replace the body of this
    function with an `api(...)` call — the caller contract is stable.

    See also:
        - `reference_chronic_red_sweep_cancelled_vs_failed_filter`
        - `feedback_gitea_status_enum_use_helper_not_raw_int`
    """
    _ = target_url  # noqa: F841 — intentional placeholder
    return None


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


def _entry_state(s: dict) -> str:
    """Per-entry status key in Gitea 1.22.6 is `status`; fall back to `state`."""
    return s.get("status") or s.get("state") or ""


def is_red(status: dict) -> tuple[bool, list[dict]]:
    """Return (is_red, failed_statuses).

    A commit is "red" if combined state is `failure` OR any individual
    status entry is in {`failure`, `error`}. `pending` and `success`
    do not trip the watchdog — pending means CI is still running, and
    that's the normal state immediately after a merge.

    `failed_statuses` is the list of per-context entries whose own
    `state` is in the red set; useful for the issue body.

    Cancel-cascade filter (mc#1564, 2026-05-19):
      Gitea maps BOTH `action_run.status=2 (Failure)` AND
      `action_run.status=3 (Cancelled)` to commit-status string
      `"failure"`. On a busy main with
      `concurrency: cancel-in-progress: true`, every merge burst
      cancels prior in-flight runs (status=3) — those bubble to the
      combined-status `failure` and inflate the watchdog's red%,
      generating phantom `[main-red]` issues (mc#1562/#1552/#1540/...).
      Canonical Gitea 1.22.6 enum per `models/actions/status.go` +
      `reference_gitea_action_status_enum_corrected_2026_05_19`:
          1=Success, 2=Failure, 3=Cancelled, 4=Skipped,
          5=Waiting, 6=Running, 7=Blocked
      We only want status=2 (real defects) to file. At the
      commit-status layer we don't have the integer enum directly
      (only the `failure` rollup string), so we use the description
      string Gitea writes when a run is cancelled — empirically
      `"Has been cancelled"` (verified 2026-05-19 via #1562 body).
      Real failures show `"Failing after Ns"` and are unaffected.
      This is option B from mc#1564 (description-string filter, no
      extra API call). Description-string stability is a soft contract
      with Gitea; if a future release renames it, the cancel-cascade
      entries will simply leak back through (visible-not-silent), and
      we'll either re-pin the string or upgrade to option A (resolve
      the underlying action_run.status integer via target_url).
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
    def _is_cancel_cascade(s: dict) -> bool:
        """status=3 entry per Gitea 1.22.6 description-string contract.
        Match exactly (after strip) — substring match would catch
        legitimate test names like "Has been cancelled by the user
        unexpectedly" in failure logs."""
        desc = (s.get("description") or "").strip()
        return desc == "Has been cancelled"

    failed = [
        s for s in statuses
        if isinstance(s, dict)
        and _entry_state(s) in red_states
        and not _is_cancel_cascade(s)
    ]
    # Combined state alone is no longer sufficient — combined=failure
    # may be 100% cancel-cascade. Drive `red` off the FILTERED list:
    # if every red-shaped per-entry was cancel-cascade, `failed` is
    # empty and we report green. Combined-failure with no per-entry
    # detail (empty `statuses[]`) still trips red — that's the
    # "CI emitter set combined-status directly" edge case from
    # render_body's fallback path; we keep filing on it so the
    # operator sees the breadcrumb.
    combined_red_no_detail = combined in red_states and not statuses
    return (bool(failed) or combined_red_no_detail, failed)


# --------------------------------------------------------------------------
# Issue file / update / close
# --------------------------------------------------------------------------
def title_for(sha: str) -> str:
    """Idempotency key — `[main-red] {repo}: {SHA[:10]}`.

    Commit-scoped. A fix-forward to a new SHA produces a new title; the
    prior issue auto-closes via `close_open_red_issues_for_other_shas`.
    """
    return f"{TITLE_PREFIX} {REPO}: {sha[:10]}"


def _is_scheduled_context(context: str) -> bool:
    """Return True if `context` is a known scheduled/non-required job.

    These contexts run on a schedule and should not block stale-issue
    closeout when main's required CI has recovered (mc#1789).
    """
    return any(pattern.lower() in context.lower() for pattern in SCHEDULED_CONTEXT_PATTERNS)


def list_open_red_issues() -> list[dict]:
    """All open issues whose title starts with `[main-red] {repo}: `.

    Per Five-Axis review on CP#112 (`feedback_api_helper_must_raise_not_return_dict`):
    api() raises on non-2xx; we let it propagate. Returning [] on a
    transient 500 would cause auto-close to skip the cleanup AND the
    file-or-update path to POST a duplicate — exactly the regression
    class the helper-raises contract closes.

    Pagination is exhausted (mc#1789). The old "by design ≤ 1" invariant
    was false — backlog can exceed 50 open issues.
    """
    prefix = f"{TITLE_PREFIX} {REPO}: "
    all_issues: list[dict] = []
    page = 1
    limit = 50
    while True:
        _, results = api(
            "GET",
            f"/repos/{OWNER}/{NAME}/issues",
            query={"state": "open", "type": "issues", "limit": str(limit), "page": str(page)},
        )
        if not isinstance(results, list):
            raise ApiError(
                f"issue search returned non-list body (got {type(results).__name__})"
            )
        matched = [
            i for i in results
            if isinstance(i, dict)
            and isinstance(i.get("title"), str)
            and i["title"].startswith(prefix)
        ]
        all_issues.extend(matched)
        if len(results) < limit:
            break
        page += 1
    return all_issues


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


def close_stale_red_issues(
    current_sha: str,
    current_status: dict,
    *,
    dry_run: bool = False,
) -> int:
    """Close open [main-red] issues whose specific failing contexts have
    all recovered on `current_sha`, even though `main` is still red for
    other reasons (mc#1789).

    When main stays red across consecutive SHAs for *different* causes,
    `close_open_red_issues_for_other_shas` never fires (it only runs when
    main is green). This function prevents stale issues from accumulating
    indefinitely by comparing per-context recovery across SHAs.

    An issue is considered stale when every context that was in a failed
    state on the issue's SHA is now either `success` on the current HEAD
    or absent (workflow removed / renamed). Issues whose original SHA had
    a combined-red-with-no-detail (empty statuses list) are skipped — we
    cannot verify recovery without per-context data.

    Returns the number of issues closed.
    """
    open_red = list_open_red_issues()
    if not open_red:
        return 0

    current_statuses = current_status.get("statuses") or []
    closed = 0

    for issue in open_red:
        title = issue.get("title", "")
        prefix = f"{TITLE_PREFIX} {REPO}: "
        if not title.startswith(prefix):
            continue
        short_sha = title[len(prefix):]
        if short_sha == current_sha[:10]:
            continue

        # Query status for the old SHA. Short SHA should resolve; if it
        # doesn't (GC'd, force-pushed, ambiguous), skip conservatively.
        try:
            old_status = get_combined_status(short_sha)
        except ApiError:
            continue

        old_red, old_failed = is_red(old_status)
        if not old_red:
            # Open issue for a now-green SHA — close it via the normal path.
            num = issue.get("number")
            if isinstance(num, int):
                comment = (
                    f"Commit `{short_sha}` is no longer red. Closing as the "
                    f"failure context has recovered or expired."
                )
                if dry_run:
                    print(
                        f"::notice::[dry-run] would close issue #{num} "
                        f"({title}) — old SHA is now green"
                    )
                    closed += 1
                    continue
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
                print(
                    f"::notice::Closed stale main-red issue #{num} "
                    f"(old SHA {short_sha} is now green)"
                )
                closed += 1
            continue

        if not old_failed:
            # Combined red with no per-context detail — can't verify recovery.
            continue

        # Verify every failed context from the old SHA has recovered.
        all_recovered = True
        recovered_ctxs: list[str] = []
        still_failing_ctxs: list[str] = []
        for s in old_failed:
            ctx = s.get("context", "")
            if not ctx:
                continue
            current_match = None
            for cs in current_statuses:
                if isinstance(cs, dict) and cs.get("context") == ctx:
                    current_match = cs
                    break
            if current_match is None:
                recovered_ctxs.append(ctx)
            elif _entry_state(current_match) == "success":
                recovered_ctxs.append(ctx)
            else:
                all_recovered = False
                still_failing_ctxs.append(ctx)

        if not all_recovered:
            continue

        num = issue.get("number")
        if not isinstance(num, int):
            continue

        comment = (
            f"The failing contexts from this SHA (`{short_sha}`) have "
            f"recovered on current HEAD `{current_sha[:10]}`: "
            f"{', '.join(recovered_ctxs)}. "
            f"Main is still red for other reasons; see the current "
            f"`[main-red]` issue for `{current_sha[:10]}`."
        )
        if dry_run:
            print(
                f"::notice::[dry-run] would close stale issue #{num} "
                f"({title}) — contexts recovered"
            )
            closed += 1
            continue

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
        print(
            f"::notice::Closed stale main-red issue #{num} "
            f"(contexts recovered at {current_sha[:10]})"
        )
        closed += 1

    return closed


def close_open_red_issues_for_other_shas(
    current_sha: str,
    *,
    dry_run: bool = False,
    close_same_sha: bool = False,
) -> int:
    """When main is green at current_sha, close any open `[main-red]`
    issues whose title references a different SHA. Returns the number
    of issues closed.

    Lineage note: we only close issues whose title prefix matches; if
    a human renamed the issue or added a suffix this won't touch it.
    That's intentional — manual editorial state takes precedence.

    Args:
        close_same_sha: set True when the caller already knows main is
            green at current_sha (e.g. recovery block) and wants to close
            the open issue for THIS SHA too. Defaults False so the
            green-path callers never accidentally close an issue they just
            filed on the same tick.
    """
    target_title = title_for(current_sha)
    open_red = list_open_red_issues()
    closed = 0
    for issue in open_red:
        if issue.get("title") == target_title:
            if not close_same_sha:
                # Same SHA — caller should not have invoked this if main is
                # green. Skip defensively (guards against green-path callers
                # that accidentally pass the SHA they just filed for).
                continue
            # close_same_sha=True: close even this SHA's issue (recovery path)
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
        # HEAD recheck (task #394 — guards mc#1597..1630 false-positive
        # cluster). After the initial detection, wait RECHECK_DELAY_SECS
        # (default 90s; tests stub time.sleep) and re-evaluate:
        #
        #   1. Re-fetch HEAD SHA. If HEAD moved, a new commit landed
        #      mid-tick — the prior red SHA is no longer authoritative
        #      and the next cron run will re-evaluate against the new
        #      HEAD. Skip-file.
        #
        #   2. If HEAD unchanged, re-fetch the combined status. If it
        #      recovered (combined state no longer in {failure,error}
        #      after the cancel-cascade filter), a transient retry
        #      rolled the run forward. Skip-file.
        #
        # Both paths emit a Loki event distinguishable from the real
        # `main_red_detected` so obs queries can track filter activity.
        # The settling window is well below the hourly cron cadence —
        # genuine failures persist past it and are surfaced normally.
        time.sleep(RECHECK_DELAY_SECS)

        recheck_sha = get_head_sha(WATCH_BRANCH)
        if recheck_sha != sha:
            emit_loki_event("main_red_skipped_head_drift", sha, [])
            print(
                f"::notice::skip-file (HEAD moved): initial red at "
                f"{sha[:10]} but HEAD is now {recheck_sha[:10]} on "
                f"{WATCH_BRANCH}; next cron tick will re-evaluate."
            )
            # HEAD drifted — close any stale main-red issue for the prior SHA
            # before returning, so we don't leave stale open issues when main
            # is no longer pointing at the red commit.
            close_open_red_issues_for_other_shas(recheck_sha, dry_run=dry_run)
            return 0

        recheck_status = get_combined_status(sha)
        recheck_red, recheck_failed = is_red(recheck_status)
        if not recheck_red:
            emit_loki_event("main_red_skipped_recovered", sha, [])
            print(
                f"::notice::skip-file (recovered after settling): "
                f"combined state at {sha[:10]} flipped to "
                f"{recheck_status.get('state')!r} on recheck; "
                f"initial red was a transient cancel-cascade."
            )
            # CI recovered on the same SHA — close any stale main-red issue
            # that was filed on a prior tick for this SHA.
            close_open_red_issues_for_other_shas(sha, dry_run=dry_run, close_same_sha=True)
            return 0

        # Still red after settling — file/update. Use the recheck data
        # as authoritative so the issue body reflects the latest state.
        failed = recheck_failed
        debug["recheck_combined_state"] = recheck_status.get("state")
        debug["recheck_failed_contexts"] = [
            s.get("context") for s in failed
        ]

        failed_ctxs = [s.get("context") for s in failed if s.get("context")]
        emit_loki_event("main_red_detected", sha, failed_ctxs)
        print(f"::warning::main is RED at {sha[:10]} on {WATCH_BRANCH}: "
              f"{len(failed)} failed context(s)")
        file_or_update_red(sha, failed, debug, dry_run=dry_run)
        stale_closed = close_stale_red_issues(sha, recheck_status, dry_run=dry_run)
        if stale_closed:
            emit_loki_event("main_red_stale_closed", sha, [])
            print(
                f"::notice::Closed {stale_closed} stale main-red issue(s) "
                f"whose contexts recovered at {sha[:10]}"
            )
    else:
        # Green or pending-with-no-real-failures. Close stale issues
        # from earlier SHAs when required CI has recovered.
        #
        # mc#1789: main often sits at combined `pending` because
        # scheduled/non-required contexts (Staging SaaS smoke,
        # Continuous synthetic E2E, main-red-watchdog itself,
        # ci-arm64-advisory) are still running. We close stale issues
        # as long as no *non-scheduled* context has failed and no
        # *non-scheduled* context is still pending — i.e. required CI
        # is effectively green.
        #
        # The success-only gate is preserved for the canonical green
        # path; the extended check below only fires when combined is
        # `pending` but all required work is done.
        combined_state = status.get("state")
        if combined_state == "success":
            should_close = True
            close_reason = "GREEN"
        else:
            statuses = status.get("statuses") or []
            non_scheduled_pending = [
                s for s in statuses
                if isinstance(s, dict)
                and (_entry_state(s) == "pending")
                and not _is_scheduled_context(s.get("context", ""))
            ]
            non_scheduled_failed = [
                s for s in statuses
                if isinstance(s, dict)
                and (_entry_state(s) in {"failure", "error"})
                and not _is_scheduled_context(s.get("context", ""))
            ]
            # Cancel-cascade already filtered by is_red(); red=False
            # here means no real failures. We additionally check that
            # no non-scheduled context is still pending.
            should_close = not non_scheduled_pending and not non_scheduled_failed
            close_reason = "pending-but-required-green"

        if should_close:
            closed = close_open_red_issues_for_other_shas(sha, dry_run=dry_run)
            if closed:
                emit_loki_event(
                    "main_returned_to_green", sha,
                    [],
                )
            print(
                f"::notice::main is {close_reason} at {sha[:10]} on {WATCH_BRANCH} "
                f"(closed {closed} stale issue(s))"
            )
        else:
            print(
                f"::notice::main has pending-or-failed required CI at {sha[:10]} "
                f"on {WATCH_BRANCH} (combined state={combined_state!r}; no action)"
            )
    return 0


def main(argv: list[str] | None = None) -> int:
    args = _parse_args(argv)
    _require_runtime_env()
    return run_once(dry_run=args.dry_run)


if __name__ == "__main__":
    sys.exit(main())
