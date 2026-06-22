#!/usr/bin/env python3
"""gitea-merge-queue — conservative serialized merge bot for Gitea.

Gitea 1.22.6+ has auto-merge (`pull_auto_merge`) but no GitHub-style merge
queue. This script provides the missing serialized policy in user space:

1. Scan open same-repo PRs that are NOT opted out (auto-discovery, see below),
   oldest-first, skipping drafts, until an ACTIONABLE one is found. A non-ready
   candidate (REQUEST_CHANGES, mergeable!=True, insufficient genuine approvals,
   or red required CI) is SKIPPED so it cannot head-of-line block newer ready
   PRs; the scan continues to the next candidate.
2. Refuse to act unless main's BP-required contexts are green. This is also
   the serialized backstop for direct-merge (see below): after a direct merge,
   main re-runs push CI and this gate PAUSES the queue if main goes red, so no
   merge piles onto an unverified/red main (issue #2358).
3. Refuse fork PRs; the queue may only mutate same-repo branches.
4. DIRECT-MERGE when conflict-free (issue #2358). When Gitea reports the PR
   conflict-free (mergeable is True) and the merge bar below is met, MERGE IT
   DIRECTLY — even if its head does not contain current main. We do NOT call
   /pulls/{n}/update first: branch protection does not require strict
   up-to-date, so behind-main conflict-free PRs merge cleanly, and calling
   /update would trigger Gitea dismiss_stale_approvals (dismissing the genuine
   approvals and forcing a re-review every tick — the rebase-churn bottleneck).
   The /update path is used ONLY when the PR is DEFINITIVELY not mergeable
   (mergeable is literal False) AND its head lacks current main — refreshing the
   branch may resolve a behind-main non-conflict; a real conflict returns HTTP
   409 and the PR is HELD per #2352. mergeable=None/missing (Gitea STILL
   COMPUTING conflict state) is a distinct fail-closed WAIT: never merged AND
   never /update'd — calling /update during the compute window would dismiss the
   PR's genuine approvals (dismiss_stale_approvals) and re-introduce the exact
   rebase-churn this queue eliminates. None is re-checked next tick.
5. Merge ONLY when, on the PR's CURRENT head sha:
     - >= REQUIRED_APPROVALS distinct GENUINE official APPROVED reviews from
       the recognised reviewer set (not stale, not dismissed, commit_id ==
       current head), AND
     - no open official REQUEST_CHANGES on the current head, AND
     - every BP-required status context is green, AND
     - the PR is mergeable (Gitea reports it conflict-free).

Authoritative gates (fail-closed):
  - The REQUIRED status contexts come from BRANCH PROTECTION
    (`status_check_contexts`) PLUS the hardcoded governance checks
    (qa-review, security-review, sop-checklist). If branch protection
    cannot be enumerated, the queue HOLDS (does not merge blindly).
  - NON-required reds (E2E Chat, Staging SaaS, ci-arm64-advisory, any
    continue-on-error job) MUST NOT block. They are reported, never gating.
  - `force_merge=true` is used ONLY when the merge is blocked *solely* by
    missing-but-non-required advisory contexts (required are green + genuine
    approvals present). It is NEVER used to bypass a failing REQUIRED context
    or missing approvals.

Auto-discovery (opt-OUT, label-optional):
  The queue is SELF-SUSTAINING — a ready PR does NOT need a human (or an agent)
  to add the `merge-queue` label first. When AUTO_DISCOVER is on (default), the
  queue enumerates ALL open same-repo PRs and considers any that meets the full
  merge bar (genuine approvals on current head + BP-required green + mergeable +
  no open REQUEST_CHANGES). The merge bar above is UNCHANGED; auto-discovery only
  changes WHICH PRs are considered, not whether they are mergeable.

  This deliberately removes the historical dependency on an agent adding the
  `merge-queue` label — agent Gitea tokens lack `write:issue` (labels are
  issue-scoped), so they could never self-label and the queue stalled. The label
  is now OPTIONAL metadata, not a gate.

  SAFETY is preserved as opt-OUT: any PR carrying an opt-out label
  (OPT_OUT_LABELS — `merge-queue-hold`, `do-not-auto-merge`, `wip`, `draft` by
  default) is skipped (never auto-considered, never merged). Draft PRs
  (draft=true STATE) are also skipped; the literal `draft` LABEL is an
  additional explicit opt-out a human can apply without converting to a draft.
  A human who wants to keep a PR out of autonomous merging just adds one of
  those labels. Setting AUTO_DISCOVER=0 restores the legacy opt-IN behaviour
  (only PRs already carrying QUEUE_LABEL are considered).

Head-of-line (HOL) safety has two complementary layers:
  (a) The queue SCANS THROUGH the FIFO candidate list and skips any non-ready
      PR (REQUEST_CHANGES, mergeable!=True, insufficient genuine approvals, or
      red required CI) instead of locking on the oldest and waiting, so a PR
      that can never become ready without human action does not block newer
      ready PRs.
  (b) For the candidate the scan acts on, two permanent failure modes HOLD the
      PR (apply HOLD_LABEL) and let the scan CONTINUE to the next candidate
      rather than re-selecting the same wedged PR every tick:
        - a permanent permission/4xx merge error (403/404/405), and
        - a persistent branch-update conflict (the /update endpoint returns
          HTTP 409 because the PR branch cannot be merged with main without a
          manual rebase). A conflict will not self-resolve, so retrying it
          every tick would HOL-block every ready PR behind it (issue #2352).

Status-fetch is fail-closed: if the combined status for a sha cannot be
fetched, the PR is skipped this tick (never treated as green).

The script is intentionally one-PR-per-run. Workflow/cron concurrency should
serialize invocations so two green PRs cannot merge against the same main.
"""

from __future__ import annotations

import argparse
import dataclasses
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

# SSOT fail-closed approval predicate (SEV-1 internal#812). review-check.sh
# consumes the same module via _review_check_filter.py — do NOT duplicate
# the predicate here. See _approval_validator.py for the fail-closed contract.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _approval_validator import classify_reviews as _classify_reviews_ssot  # noqa: E402


def _env(key: str, *, default: str = "") -> str:
    return os.environ.get(key, default)


GITEA_TOKEN = _env("GITEA_TOKEN")
GITEA_HOST = _env("GITEA_HOST")
REPO = _env("REPO")
WATCH_BRANCH = _env("WATCH_BRANCH", default="main")
QUEUE_LABEL = _env("QUEUE_LABEL", default="merge-queue")
HOLD_LABEL = _env("HOLD_LABEL", default="merge-queue-hold")
UPDATE_STYLE = _env("UPDATE_STYLE", default="merge")
# Auto-discovery (opt-OUT). When truthy (default), the queue considers ALL open
# same-repo PRs that meet the merge bar, not only PRs already carrying
# QUEUE_LABEL — so the queue is self-sustaining without any human/agent labeling
# (agent tokens lack write:issue and cannot self-label). Set AUTO_DISCOVER=0 to
# restore the legacy opt-IN behaviour (QUEUE_LABEL required to be considered).
AUTO_DISCOVER = _env("AUTO_DISCOVER", default="1").strip().lower() not in {
    "0",
    "false",
    "no",
    "off",
    "",
}
# Opt-OUT labels. A PR carrying ANY of these is skipped (never auto-considered,
# never merged) — the human escape hatch from autonomous merging. HOLD_LABEL is
# always included so the existing hold semantics keep working. `do-not-auto-merge`
# and `wip` let a human keep a PR out of the auto-merge path without removing it.
# `draft` is included as a literal label too: Gitea draft STATE (draft=true) is
# already skipped via _issue_is_draft, but a "draft" LABEL is an additional,
# explicit opt-out signal a human can apply without converting the PR to a draft.
OPT_OUT_LABELS = {
    name.strip()
    for name in _env(
        "OPT_OUT_LABELS",
        default="do-not-auto-merge,wip,draft",
    ).split(",")
    if name.strip()
} | ({HOLD_LABEL} if HOLD_LABEL else set())
# Governance checks that are ALWAYS required for every PR, regardless of
# branch-protection configuration. These are the uniform-gate checks that
# must pass before any PR can merge (SOP tier removal makes them mandatory
# for all PRs, not just tier:medium/tier:high).
#
# Context names use the (pull_request_target) suffix (not pull_request)
# to match the workflow event_type that actually emits them — verified
# live against PR#2419/#2331/etc.: the qa-review/security-review
# workflows run on pull_request_target (their `on:` block uses
# pull_request_target, not pull_request), and sop-checklist's
# all-items-acked job also uses pull_request_target. The previous
# (pull_request) suffix never matched the live emitted contexts,
# which is what was painting ~16 ready PRs red (gate appeared
# "missing" qa-review/security-review even after both passed).
# Verified against the lint-bp-context-emit-match test which already
# asserts (pull_request_target) for these names. No requirement
# dropped; just a name correction.
GOVERNANCE_REQUIRED_CONTEXTS = [
    "qa-review / approved (pull_request_target)",
    "security-review / approved (pull_request_target)",
    "sop-checklist / all-items-acked (pull_request_target)",
]
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

# Recognised official-reviewer set. A merge requires this many DISTINCT genuine
# approvals (not stale/dismissed, on the current head sha) from accounts in
# this set. The set is the real agents-team reviewer roster; founder/CTO-agent
# accounts are intentionally excluded so the queue cannot be satisfied by a
# human/owner approval alone — it must be a genuine peer review.
REVIEWER_SET = {
    name.strip()
    for name in _env(
        "REVIEWER_SET",
        default="agent-reviewer,agent-researcher,agent-reviewer-cr2",
    ).split(",")
    if name.strip()
}
# Default mirrors molecule-core branch protection (required_approvals: 2). The
# authoritative value is read from branch protection at runtime; this is only
# the fallback when BP does not specify one.
REQUIRED_APPROVALS_DEFAULT = int(_env("REQUIRED_APPROVALS", default="2") or "2")

# --------------------------------------------------------------------------
# Runtime-bump auto-merge exemption (RFC internal#131 PR-A, dispatch 9c2e9c88)
# --------------------------------------------------------------------------
# The bump-bot publishes single-line .runtime-version bumps when a runtime-v
# release lands. These PRs are TRIVIAL by construction (one-file, one-line,
# identical mechanical pattern), and the runtime-smoke CI on the PR is
# designed to be the actual proof — the new runtime has been provisioned +
# run a workspace on it. We trust a curated allowlist of "boring" runtimes
# (currently empty by default; CEO-Asst populates per runtime policy) to
# auto-merge such bumps WITHOUT 2-genuine review — but ONLY if every guard
# and condition below holds. This exists to break the rebase-churn loop
# for runtime-v releases (otherwise every bump queues behind normal review
# cadence and lands a day late).
#
# FAIL-CLOSED: any guard/condition that is unverifiable → NOT exempt. The
# PR routes through the normal 2-genuine path instead. We never auto-merge
# on uncertainty.
RUNTIME_BUMP_BOT_USER = _env("RUNTIME_BUMP_BOT_USER", default="bump-bot")
# Allowlist of runtime names that are eligible for the exemption. Populated
# by the CEO-Asst via env var (comma-separated). Default empty = no runtime
# is eligible; the function is a no-op until the CEO-Asst opts a runtime in.
# RUNTIME_BUMP_EXEMPT_TEMPLATES values must match the runtime identifier in
# .runtime-version (e.g. "claude-code", "hermes", "kimi-cli"), NOT the
# repository name.
RUNTIME_BUMP_EXEMPT_TEMPLATES = {
    name.strip()
    for name in _env("RUNTIME_BUMP_EXEMPT_TEMPLATES", default="").split(",")
    if name.strip()
}
# Subset of branch-protection REQUIRED contexts that prove the PR was
# actually verified by a real runtime-provision/compat smoke (i.e. the CI
# job provisions a workspace on the new runtime and runs a basic task —
# NOT a static build/lint/unit-test job). The PR is GATE-ADEQUATE only if
# at least ONE of these (case-insensitive substring match) appears in the
# required_contexts set the queue loaded from branch protection. Adding
# a new runtime-smoke job to BP without adding the context name here will
# silently disable the exemption for that job — the fail-closed design
# deliberately errs on the side of falling back to 2-genuine.
RUNTIME_PROVISION_SMOKE_CONTEXTS = {
    name.strip().lower()
    for name in _env(
        "RUNTIME_PROVISION_SMOKE_CONTEXTS",
        default=(
            "runtime-provision-smoke,"
            "runtime-compat-smoke,"
            "e2e-runtime-provision"
        ),
    ).split(",")
    if name.strip()
}
# Labels that mark a runtime-bump as breaking. EXCLUDE-BREAKING (condition
# 3) treats any of these as "not eligible", regardless of semver. The
# bump-bot is not expected to add these labels — the set is a safety net
# for the case where a human adds one mid-flight (e.g. "the API changed,
# callers need a rewire" mid-bump).
BREAKING_LABELS = {
    name.strip()
    for name in _env(
        "BREAKING_LABELS",
        default="breaking,breaking-change,requires-consumer-wiring",
    ).split(",")
    if name.strip()
}

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""

# --------------------------------------------------------------------------
# Conductor snapshot (operator-config#158)
# --------------------------------------------------------------------------
# When the conductor tick writes a state snapshot before running the passes,
# both scripts see the SAME observed state instead of re-fetching independently
# and potentially disagreeing within the same tick.
# --------------------------------------------------------------------------


def load_conductor_snapshot() -> dict | None:
    """Load the conductor snapshot if present and fresh.

    The snapshot is written by the conductor wrapper
    (bin/molecule-core-cron-bot.sh conductor) to a path exported as
    CONDUCTOR_SNAPSHOT_FILE. It contains open PRs + per-head combined
    statuses + reviews captured in a single state-read.

    Returns the parsed snapshot dict, or None if absent, unreadable,
    or older than the freshness threshold (10 minutes — twice the */5
    conductor cadence, so a single skipped tick does not invalidate it).
    """
    path = os.environ.get("CONDUCTOR_SNAPSHOT_FILE", "")
    if not path:
        return None
    try:
        with open(path, "r", encoding="utf-8") as f:
            snapshot = json.load(f)
    except (OSError, json.JSONDecodeError) as exc:
        print(f"::notice::conductor snapshot unreadable ({exc}); self-fetching")
        return None

    if not isinstance(snapshot, dict):
        return None

    ts_str = snapshot.get("ts", "")
    if ts_str:
        try:
            from datetime import datetime, timezone

            ts = datetime.strptime(ts_str, "%Y-%m-%dT%H:%M:%SZ").replace(
                tzinfo=timezone.utc
            )
            age_sec = (datetime.now(timezone.utc) - ts).total_seconds()
            if age_sec > 600:  # 10 minutes
                print(
                    f"::notice::conductor snapshot stale ({int(age_sec)}s); "
                    "self-fetching"
                )
                return None
        except ValueError:
            pass  # malformed ts, treat as fresh (conservative)

    return snapshot


class ApiError(RuntimeError):
    pass


class MergePermissionError(ApiError):
    """Merge failed with a permanent permission error (403/404/405).
    The queue should HOLD this PR and move to the next one."""


class BranchUpdateConflictError(ApiError):
    """Updating the PR branch with the base hit a merge-conflict (HTTP 409).

    A true merge-conflict is NOT transient: the branch cannot be auto-updated
    until a human/agent rebases it. The queue should HOLD this PR (apply
    HOLD_LABEL) and advance to the next candidate, exactly like the permission
    path — otherwise the conflicted PR sits at the queue head and is retried
    every tick forever, head-of-line-blocking every ready PR behind it.

    NOTE: distinct from mergeable=None, which is Gitea STILL COMPUTING conflict
    state — that case is handled as a transient WAIT (no hold). This error is
    only raised on an explicit 409 returned by the /update endpoint."""


class BranchProtectionUnavailable(ApiError):
    """Branch protection (the authoritative required-context source) could not
    be enumerated. The queue must HOLD rather than merge with an unverified
    required-context set (fail-closed, no fail-open)."""


@dataclasses.dataclass(frozen=True)
class MergeDecision:
    ready: bool
    action: str
    reason: str
    # When ready is True, force indicates the merge is blocked SOLELY by
    # missing-but-non-required governance contexts (required are green +
    # genuine approvals present), so force_merge=true is justified to bypass
    # ONLY those non-required contexts. Defaults False.
    force: bool = False


@dataclasses.dataclass(frozen=True)
class BranchProtection:
    """The subset of branch protection the queue depends on."""

    required_contexts: list[str]
    required_approvals: int
    block_on_rejected_reviews: bool


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


def api_paginated(
    method: str,
    path: str,
    *,
    query: dict[str, str] | None = None,
    page_size: int = 50,
) -> list[dict]:
    """Fetch all pages of a paginated Gitea list endpoint.

    Gitea paginates with `page` (1-indexed) and `limit`. We loop until a
    page returns fewer than `page_size` items, indicating the end.
    """
    results: list[dict] = []
    page = 1
    while True:
        page_query = dict(query or {})
        page_query["page"] = str(page)
        page_query["limit"] = str(page_size)
        _, body = api(method, path, query=page_query)
        if not isinstance(body, list):
            raise ApiError(f"{path} paginated response not list")
        results.extend(body)
        if len(body) < page_size:
            break
        page += 1
    return results


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


def parse_branch_protection(body: Any) -> BranchProtection:
    """Extract the queue-relevant fields from a branch_protections payload.

    Fail-closed: raises BranchProtectionUnavailable when status checks are
    expected but the required-context list cannot be enumerated. We never fall
    back to a hand-maintained env list as the authoritative required set —
    doing so risks merging when a real required context is red/missing.
    """
    if not isinstance(body, dict):
        raise BranchProtectionUnavailable("branch protection response not an object")
    enable = bool(body.get("enable_status_check"))
    contexts_raw = body.get("status_check_contexts")
    if not enable:
        # Status checks not enforced by BP at all. With no required contexts
        # the queue would gate on approvals only — acceptable, but make it
        # explicit and let the caller decide.
        contexts: list[str] = []
    else:
        if not isinstance(contexts_raw, list):
            raise BranchProtectionUnavailable(
                "enable_status_check is true but status_check_contexts is not a list"
            )
        contexts = [c for c in contexts_raw if isinstance(c, str) and c.strip()]
        if not contexts:
            raise BranchProtectionUnavailable(
                "enable_status_check is true but status_check_contexts is empty"
            )
    approvals = body.get("required_approvals")
    required_approvals = (
        int(approvals) if isinstance(approvals, int) else REQUIRED_APPROVALS_DEFAULT
    )
    return BranchProtection(
        required_contexts=contexts,
        required_approvals=required_approvals,
        block_on_rejected_reviews=bool(body.get("block_on_rejected_reviews")),
    )


def get_branch_protection(branch: str) -> BranchProtection:
    """Fetch branch protection for `branch`; fail-closed if unavailable."""
    try:
        _, body = api("GET", f"/repos/{OWNER}/{NAME}/branch_protections/{branch}")
    except ApiError as exc:
        raise BranchProtectionUnavailable(
            f"could not fetch branch protection for {branch}: {exc}"
        ) from exc
    return parse_branch_protection(body)


def genuine_approvals(
    reviews: list[dict],
    *,
    headsha: str,
    reviewer_set: set[str],
) -> tuple[set[str], list[str]]:
    """Thin wrapper over the SSOT predicate in _approval_validator.py.

    All logic — the per-review commit_id / state / official / dismissed /
    stale contract — lives in _approval_validator.classify_reviews. This
    wrapper exists only to keep the call site (and external readers of
    the symbol) stable. Do NOT add any per-review logic here; if you need
    to change the predicate, edit _approval_validator.py.

    See _approval_validator.py for the full fail-closed contract
    (SEV-1 internal#812). The previous inline implementation had a
    `if isinstance(commit_id, str) and commit_id and headsha:` guard that
    silently accepted reviews with no commit_id; that fail-open surface is
    now closed at the SSOT.
    """
    return _classify_reviews_ssot(
        reviews, headsha=headsha, reviewer_set=reviewer_set
    )

def get_pull_reviews(pr_number: int) -> list[dict]:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/reviews")
    if not isinstance(body, list):
        raise ApiError(f"PR #{pr_number} reviews response not list")
    return body


def label_names(issue: dict) -> set[str]:
    return {
        label["name"]
        for label in issue.get("labels", [])
        if isinstance(label, dict) and isinstance(label.get("name"), str)
    }


# --------------------------------------------------------------------------
# Runtime-bump exemption (RFC internal#131 PR-A; spec 9c2e9c88)
# --------------------------------------------------------------------------
# Helpers + the is_runtime_bump_exempt() predicate. Wired into
# evaluate_merge_readiness via the runtime_bump_exempt flag (when True, the
# required-approvals check is skipped — the 2-genuine convention is
# intentionally bypassed for these PRs). The runtime-smoke CI context is
# STILL required (it's a required_contexts check, not an approvals check,
# so the exemption does not weaken the CI gate).
# --------------------------------------------------------------------------

# File touched by a qualifying runtime-bump. The bump-bot writes the new
# version as the ONLY changed line in this file; the line is also the ONLY
# removed line (the old version). Any other file touched, or multiple
# lines, fails the single-line guard and the PR is treated as non-bump.
RUNTIME_VERSION_FILE = ".runtime-version"


def _runtime_version_added_removed_lines(patch: str) -> tuple[int, int, str | None, str | None]:
    """Parse a unified-diff patch string and return the (added, removed) line
    counts AND the values of the single added/removed non-header lines.

    A runtime-bump patch has exactly ONE added line (the new version) and
    ONE removed line (the old version). If either count differs, the PR is
    not a clean single-line bump and is rejected by the guard.

    Header lines (`+++`, `---`) are excluded. Empty lines (a `+` or `-`
    with no content) are treated as a valid line — but a runtime-bump
    would not have one (the version is always a non-empty string).
    """
    added = 0
    removed = 0
    added_value: str | None = None
    removed_value: str | None = None
    for line in patch.splitlines():
        if line.startswith("+++") or line.startswith("---"):
            continue
        if line.startswith("+"):
            added += 1
            # Strip the leading `+` and any trailing newline. Preserve
            # the value verbatim so ==SSOT can compare it to the runtime-v
            # release tag without ambiguity (no trim, no strip).
            added_value = line[1:]
        elif line.startswith("-"):
            removed += 1
            removed_value = line[1:]
    return added, removed, added_value, removed_value


def is_runtime_bump_exempt(
    *,
    pr: dict,
    pr_files: list[dict],
    required_contexts: list[str],
    latest_runtime_v_tag: str | None,
    rc_active: bool,
) -> tuple[bool, str]:
    """Decide whether a PR is a qualifying runtime-bump eligible for the
    auto-merge exemption. Returns (exempt, reason).

    FAIL-CLOSED: every guard/condition that is unverifiable → NOT exempt.
    The PR routes through the normal 2-genuine path. We never auto-merge
    on uncertainty.

    GUARDS (all must hold):
      1. author == bump-bot (RUNTIME_BUMP_BOT_USER)
      2. diff touches EXACTLY .runtime-version AND has exactly 1 added +
         1 removed non-header line in that file
      3. no active release-candidate in progress (rc_active == False)
      4. all required CI contexts green — ENFORCED in evaluate_merge_readiness,
         not here; the exemption skips the approvals check, not the CI gate

    CONDITIONS (all must hold):
      1. ==SSOT: the added value == the latest runtime-v release tag
         (NOT merely "valid semver"; the value must match a real release
         the runtime repo has published)
      2a. GATE-ADEQUACY: the loaded required_contexts set includes a real
         runtime-provision/compat smoke context (a job that provisions +
         runs a workspace on the new runtime — NOT static build/lint)
      2b. GATE-ADEQUACY: the runtime name (parsed from the .runtime-version
         header) is on RUNTIME_BUMP_EXEMPT_TEMPLATES
      3. EXCLUDE-BREAKING: the new version is NOT semver-MAJOR (major>0)
         AND the PR has no breaking label (BREAKING_LABELS)
    """
    # GUARD 1: author must be bump-bot. Fail-closed: a missing/unknown
    # user is NOT bump-bot.
    author = (pr.get("user") or {}).get("login", "")
    if not isinstance(author, str) or author != RUNTIME_BUMP_BOT_USER:
        return False, f"author={author!r} != RUNTIME_BUMP_BOT_USER={RUNTIME_BUMP_BOT_USER!r}"

    # GUARD 2: diff must be EXACTLY .runtime-version, with exactly one
    # added line and one removed line (the old → new version swap).
    runtime_file = None
    other_files = 0
    for entry in pr_files:
        if not isinstance(entry, dict):
            # FAIL-CLOSED: an unparseable file entry is a structural
            # anomaly; we can't prove the diff is single-file. Default
            # to non-exempt.
            return False, "pr_files contains non-dict entry (unverifiable)"
        filename = entry.get("filename", "")
        if filename == RUNTIME_VERSION_FILE:
            if runtime_file is not None:
                # The same file appears twice in the diff listing
                # (shouldn't happen for a real PR but be defensive).
                return False, f"{RUNTIME_VERSION_FILE} appears multiple times in diff"
            runtime_file = entry
        else:
            other_files += 1
    if runtime_file is None:
        return False, f"diff does not touch {RUNTIME_VERSION_FILE}"
    if other_files > 0:
        return False, f"diff touches {other_files} other file(s) besides {RUNTIME_VERSION_FILE}"

    patch = runtime_file.get("patch", "")
    if not isinstance(patch, str) or not patch:
        return False, f"{RUNTIME_VERSION_FILE} diff has no patch text (unverifiable)"

    added, removed, added_value, removed_value = _runtime_version_added_removed_lines(patch)
    if added != 1 or removed != 1:
        return (
            False,
            f"{RUNTIME_VERSION_FILE} diff has {added} added + {removed} removed "
            f"non-header lines (must be exactly 1 each)",
        )
    if not added_value or not removed_value:
        return False, f"{RUNTIME_VERSION_FILE} diff added/removed value is empty"

    # Parse the .runtime-version line into runtime name + version. The
    # format is `<runtime-name>@<version>` (e.g. `claude-code@v1.2.3`).
    # Both pieces are needed: the runtime name for the allowlist, the
    # version for the ==SSOT check and the semver-major check. If the
    # format is not parseable, fail-closed: we cannot determine which
    # runtime the bump is for, so the exemption cannot be safely
    # granted.
    if "@" not in added_value:
        return (
            False,
            f"{RUNTIME_VERSION_FILE} value {added_value!r} is not in "
            f"'<runtime>@<version>' format (cannot determine runtime name "
            f"for allowlist + ==SSOT checks)",
        )
    runtime_name, _, version_part = added_value.partition("@")
    if not runtime_name or not version_part:
        return (
            False,
            f"{RUNTIME_VERSION_FILE} value {added_value!r} has empty "
            f"runtime or version after partition",
        )

    # GUARD 3: no active RC in progress. The caller computes this (it
    # needs to inspect PR list for an active `release-candidate` PR);
    # we trust the boolean, but a missing/None is treated as
    # unverifiable → fail-closed.
    if rc_active is True:
        return False, "active release-candidate PR in progress"

    # CONDITION 1: ==SSOT. The VERSION part of the .runtime-version
    # line must equal the latest runtime-v release tag (NOT the full
    # `<runtime>@<version>` string — the tag is just the version,
    # e.g. `v1.2.3`). None means the caller couldn't determine the
    # latest tag (network error, missing repo, etc.) → fail-closed.
    if not isinstance(latest_runtime_v_tag, str) or not latest_runtime_v_tag:
        return False, "latest runtime-v release tag is unverifiable (==SSOT)"
    if version_part != latest_runtime_v_tag:
        return (
            False,
            f"==SSOT mismatch: .runtime-version version={version_part!r} "
            f"!= latest runtime-v tag={latest_runtime_v_tag!r}",
        )

    # CONDITION 2a: GATE-ADEQUACY (CI side). At least one of the
    # required contexts (case-insensitive substring match against
    # RUNTIME_PROVISION_SMOKE_CONTEXTS) must be in the loaded
    # required_contexts set. Empty required_contexts → fail-closed.
    if not required_contexts:
        return False, "required_contexts is empty (no CI gate to verify against)"
    required_contexts_lower = [c.lower() for c in required_contexts]
    has_runtime_smoke = any(
        any(smoke in ctx for smoke in RUNTIME_PROVISION_SMOKE_CONTEXTS)
        for ctx in required_contexts_lower
    )
    if not has_runtime_smoke:
        return (
            False,
            f"required_contexts has no runtime-provision/compat smoke "
            f"(looked for any of: {sorted(RUNTIME_PROVISION_SMOKE_CONTEXTS)})",
        )

    # CONDITION 2b: GATE-ADEQUACY (template side). The runtime name
    # (parsed above from the .runtime-version header) must be on the
    # allowlist.
    if runtime_name not in RUNTIME_BUMP_EXEMPT_TEMPLATES:
        return (
            False,
            f"runtime {runtime_name!r} not on RUNTIME_BUMP_EXEMPT_TEMPLATES "
            f"allowlist (currently: {sorted(RUNTIME_BUMP_EXEMPT_TEMPLATES)})",
        )

    # CONDITION 3: EXCLUDE-BREAKING. Semver-MAJOR (the major part of
    # the version is > 0) → not eligible. A leading 'v' is tolerated
    # (`v1.2.3` → major=1). Unparseable versions → fail-closed.
    clean_version = version_part.lstrip("v")
    try:
        major_str, _, _ = clean_version.split(".", 2)
        major_int = int(major_str)
    except (ValueError, IndexError):
        return (
            False,
            f"version {version_part!r} is not parseable as semver "
            f"(major.minor.patch)",
        )
    if major_int > 0:
        return (
            False,
            f"semver-MAJOR bump ({version_part}): major={major_int} > 0 → not eligible",
        )

    # CONDITION 3 (cont): breaking label check. The bump-bot is not
    # expected to set these, but a human might have added one
    # mid-flight. Empty pr_labels → trivially passes.
    pr_labels = label_names(pr)
    breaking_present = pr_labels & BREAKING_LABELS
    if breaking_present:
        return (
            False,
            f"PR has breaking label(s): {sorted(breaking_present)}",
        )

    return True, (
        f"runtime-bump exempt: bump-bot single-line .runtime-version "
        f"{removed_value!r}→{added_value!r}; runtime={runtime_name!r}; "
        f"==SSOT; smoke context present; allowlist hit; semver-minor"
    )


def _parse_bump_pr_title(title: str) -> tuple[str, str] | None:
    """Parse a bump-bot PR title into (runtime_name, version).

    Returns None if the title doesn't match a recognized bump-bot
    format. The caller treats a None return as "singleton bucket, no
    dedup possible" (fail-safe — a non-matching title is never
    spuriously grouped with anything else).

    Recognized formats (per the 9c2e9c88 spec):
      - "runtime:<runtime>-<version>"  (no leading 'v' on version)
      - "<runtime> bump to <version>"  (version may or may not have leading 'v')
      - "runtime: <runtime> bump to <version>"  (hybrid: prefix + verb form)

    Examples:
      "runtime:claude-code-v1.2.3"          -> ("claude-code", "v1.2.3")
      "claude-code bump to v1.2.3"         -> ("claude-code", "v1.2.3")
      "runtime: claude-code bump to v1.2.3"-> ("claude-code", "v1.2.3")
      "kimi-cli bump to v0.5.0"            -> ("kimi-cli", "v0.5.0")
      "fix typo in docs"                    -> None
    """
    if not isinstance(title, str) or not title:
        return None
    # The "runtime:" prefix is optional — strip it if present so we can
    # use a single regex for the two body forms.
    body = title[len("runtime:"):].lstrip() if title.startswith("runtime:") else title
    # Format 1: "<runtime>-<version>" — runtime name is the substring
    # before the last "-v<MAJOR>.<MINOR>.<PATCH>" suffix.
    m = re.search(r"-v\d+\.\d+\.\d+\s*$", body)
    if m:
        runtime_name = body[: m.start()].strip()
        version = body[m.start() + 1 :].strip()
        if runtime_name and version:
            return (runtime_name, version)
    # Format 2: "<runtime> bump to <version>"
    m = re.match(
        r"^(?P<runtime>\S+)\s+bump to\s+(?P<version>v?\d+\.\d+\.\d+)\s*$",
        body,
    )
    if m:
        runtime_name = m.group("runtime").strip()
        version = m.group("version").strip()
        # Normalize version to leading "v" so the dedup key matches across
        # formats (e.g. "1.2.3" from format 1 == "v1.2.3" from format 2).
        if not version.startswith("v"):
            version = "v" + version
        if runtime_name and version:
            return (runtime_name, version)
    return None


def dup_close_superseded_bump_prs(
    prs: list[dict],
    *,
    close_fn: "callable",
    comment_fn: "callable",
    dry_run: bool = False,
) -> list[int]:
    """Close all but the newest open auto-bump PR per (runtime_name, version)
    tuple. Returns the list of PR numbers that were closed.

    Rationale: when the bump-bot republishes a bump (e.g. CI was red on the
    first push, so the bot repushed with the same version), or when a new
    version lands while the old one is still open, multiple bump PRs can
    race for the same (runtime, version) slot. The exemption would auto-merge
    the OLDEST one first (FIFO) — but the OLDEST one has stale CI, and
    merging it would skip the new smoke run. We close the older ones (and
    post a comment) so the auto-merge path only ever sees the freshest.

    "Newest" = latest `updated_at` then highest `number` (both stable Gitea
    fields). PRs not authored by the bump-bot, or PRs whose diff is not a
    clean single-line .runtime-version bump, are ignored (not eligible for
    closure via this path — they may be hand-edits).

    `close_fn(pr_number, *, dry_run)` and `comment_fn(pr_number, body, *, dry_run)`
    are injected so the function is unit-testable without a live Gitea.
    """
    # Bucket PRs by (runtime, version). Skip PRs that aren't
    # single-line .runtime-version bumps authored by bump-bot.
    #
    # Title-based parsing: bump-bot PR titles follow one of two stable
    # formats per the 9c2e9c88 spec:
    #   - "runtime:<runtime>-<version>"  (e.g. "runtime:claude-code-v1.2.3")
    #   - "<runtime> bump to <version>"  (e.g. "claude-code bump to v1.2.3")
    # We parse the title to extract (runtime, version) explicitly so the
    # bucket key matches the documented dedup semantic ("newest per
    # (runtime, version)"). A title that doesn't match either format is
    # treated as its own single-PR bucket — the per-PR close guard
    # (`if len(group) <= 1: continue`) makes that a no-op. This is
    # fail-safe: a non-standard title is never spuriously closed.
    buckets: dict[tuple[str, str], list[dict]] = {}
    for pr in prs:
        if not isinstance(pr, dict):
            continue
        author = (pr.get("user") or {}).get("login", "")
        if author != RUNTIME_BUMP_BOT_USER:
            continue
        number = int(pr.get("number", 0))
        if not number:
            continue
        runtime_v = _parse_bump_pr_title(pr.get("title", ""))
        # Group by (runtime, version); non-matching titles get a unique
        # sentinel key so they end up in a singleton bucket (no-op).
        bucket_key = runtime_v if runtime_v is not None else (str(number), "")
        buckets.setdefault(bucket_key, []).append(pr)

    closed: list[int] = []
    for _key, group in buckets.items():
        if len(group) <= 1:
            continue
        # Sort newest-first by (updated_at desc, number desc).
        group_sorted = sorted(
            group,
            key=lambda p: (
                p.get("updated_at", ""),
                int(p.get("number", 0)),
            ),
            reverse=True,
        )
        keep = group_sorted[0]
        for stale in group_sorted[1:]:
            stale_number = int(stale.get("number", 0))
            keep_number = int(keep.get("number", 0))
            body = (
                f"Closing as superseded by #{keep_number} "
                f"(newer bump-bot PR for the same runtime-version slot; "
                f"the auto-merge exemption auto-merged the freshest to "
                f"avoid merging stale CI)."
            )
            try:
                comment_fn(stale_number, body, dry_run=dry_run)
            except ApiError:
                # Comment is best-effort; the close is what matters.
                pass
            try:
                close_fn(stale_number, dry_run=dry_run)
            except ApiError as exc:
                # Don't crash the tick on a single close failure; log
                # and continue. The other dupes in the same group
                # still need closing.
                sys.stderr.write(
                    f"::warning::dup-close failed for PR #{stale_number}: {exc}\n"
                )
                continue
            closed.append(stale_number)
    return closed


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


def _issue_is_draft(issue: dict) -> bool:
    """True if the issue/PR is a draft.

    The /issues listing exposes draft state under the `pull_request` sub-object
    (`{"draft": true}`); some Gitea versions also surface a top-level `draft`.
    Either is honoured. Drafts are never auto-considered for merging.
    """
    pr = issue.get("pull_request")
    if isinstance(pr, dict) and pr.get("draft") is True:
        return True
    return issue.get("draft") is True


def choose_candidate_issues(
    issues: list[dict],
    *,
    queue_label: str,
    opt_out_labels: set[str],
    auto_discover: bool,
) -> list[dict]:
    """All open PRs eligible for a merge attempt this tick, oldest-first.

    This is the auto-discovery selector. It does NOT change the merge bar — it
    only changes WHICH PRs are considered:

      - auto_discover=True (default): every open same-repo PR is a candidate,
        EXCEPT those carrying an opt-out label or marked draft. The QUEUE_LABEL
        is optional metadata, not a gate, so a ready PR reaches the queue with no
        human/agent labeling (the write:issue gap is removed).
      - auto_discover=False: legacy opt-IN — only PRs carrying queue_label are
        candidates (still skipping opt-out labels and drafts).

    Opt-out is the safety escape hatch: any opt_out_labels member present skips
    the PR entirely (never considered, never merged). Ordering is oldest-first
    (created_at, then number) to preserve the serialized FIFO ordering.

    Returns the FULL ordered list (not just the head) so process_once can SCAN
    THROUGH non-ready candidates instead of locking on the oldest. A non-ready
    auto-discovered PR (e.g. one with REQUEST_CHANGES or mergeable=false, which
    can never become ready without human action) must NOT head-of-line block the
    newer ready PRs behind it — the readiness check happens per-candidate in
    process_once, and a `wait` candidate is skipped to the next one.
    """
    candidates = []
    for issue in issues:
        if "pull_request" not in issue:
            continue
        labels = label_names(issue)
        if opt_out_labels & labels:
            continue  # opt-out: human kept this PR out of autonomous merging
        if _issue_is_draft(issue):
            continue  # drafts are never auto-merged
        if not auto_discover and queue_label not in labels:
            continue  # legacy opt-IN: require the queue label
        candidates.append(issue)
    candidates.sort(key=lambda issue: (issue.get("created_at") or "", int(issue["number"])))
    return candidates


def choose_next_candidate_issue(
    issues: list[dict],
    *,
    queue_label: str,
    opt_out_labels: set[str],
    auto_discover: bool,
) -> dict | None:
    """The oldest eligible candidate, or None. Thin head-of-list wrapper around
    choose_candidate_issues; retained for callers/tests that only want the head.
    process_once uses the full list (choose_candidate_issues) so it can scan past
    non-ready PRs rather than HOL-block on the oldest."""
    candidates = choose_candidate_issues(
        issues,
        queue_label=queue_label,
        opt_out_labels=opt_out_labels,
        auto_discover=auto_discover,
    )
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


def _non_required_red_present(
    latest: dict[str, dict],
    required_contexts: list[str],
) -> bool:
    """True if any NON-required context is non-success.

    Such reds are the governance/SOP/advisory checks Gitea may still treat as
    "missing required context" at merge time even though branch protection does
    not require them. Their presence is what justifies force_merge=true (we
    have already verified every REQUIRED context is green and approvals are
    genuine, so force only bypasses these non-required reds).
    """
    required = set(required_contexts)
    for context, status in latest.items():
        if context in required:
            continue
        if status_state(status) != "success":
            return True
    return False


def evaluate_merge_readiness(
    *,
    main_status: dict,
    pr_status: dict,
    required_contexts: list[str],
    required_approvals: int,
    approvers: set[str],
    request_changes: list[str],
    pr_has_current_base: bool,
    mergeable: bool | None,
    pr_labels: set[str] | None = None,
    runtime_bump_exempt: bool = False,
) -> MergeDecision:
    # 1) Main's push-required contexts must be green. Combined state can be
    #    "failure" due to non-blocking jobs (continue-on-error: true) that do
    #    not gate merges, so check the explicit required set, not combined.
    #
    #    This main-green gate is ALSO the serialized backstop that makes the
    #    direct-merge (no update) path safe (issue #2358): after a direct merge
    #    of a behind-main PR, main re-runs its push CI; if a semantic main-break
    #    slips through (PR green standalone but broken when combined with newer
    #    main), main's required contexts go red and this gate PAUSES the queue —
    #    no further merge piles onto an unverified/red main until it is green.
    main_latest = latest_statuses_by_context(main_status.get("statuses") or [])
    main_ok, main_bad = required_contexts_green(main_latest, push_required_contexts())
    if not main_ok:
        return MergeDecision(False, "pause", "main required contexts not green: " + ", ".join(main_bad))

    # 2) No open official REQUEST_CHANGES on the current head.
    if request_changes:
        return MergeDecision(
            False, "wait",
            "open REQUEST_CHANGES on current head from: " + ", ".join(sorted(request_changes)),
        )

    # 3) Enough distinct genuine official approvals on the current head.
    #    The runtime-bump exemption (RFC internal#131) bypasses this check
    #    for single-line .runtime-version bumps authored by bump-bot when
    #    the runtime-smoke CI is present in the required_contexts. The
    #    exemption does NOT bypass any CI gate (step 4 still requires
    #    every required context to be green) and does NOT bypass main's
    #    push-required gate (step 1). It ONLY bypasses the human-approvals
    #    bar, which a bot cannot satisfy by construction. Required
    #    approvals is forced to 0 for the check below when the exemption
    #    holds.
    effective_required_approvals = 0 if runtime_bump_exempt else required_approvals
    if len(approvers) < effective_required_approvals:
        return MergeDecision(
            False, "wait",
            f"insufficient genuine approvals on current head: have "
            f"{len(approvers)} ({', '.join(sorted(approvers)) or 'none'}), "
            f"need {effective_required_approvals}",
        )

    # 4) Every REQUIRED status context must be green. This includes both
    #    branch-protection-required contexts AND the hardcoded governance checks
    #    (qa-review, security-review, sop-checklist). NON-required reds (E2E
    #    Chat, Staging SaaS, ci-arm64-advisory, continue-on-error jobs) are NOT
    #    consulted here and must not block.
    latest = latest_statuses_by_context(pr_status.get("statuses") or [])
    ok, missing_or_bad = required_contexts_green(latest, required_contexts)
    if not ok:
        return MergeDecision(False, "wait", "required contexts not green: " + ", ".join(missing_or_bad))

    # 5) DIRECT-MERGE when conflict-free (issue #2358 — throughput fix).
    #    If Gitea reports the PR conflict-free (mergeable is True), MERGE IT
    #    DIRECTLY even if its head does not yet contain current main. Branch
    #    protection does NOT require strict up-to-date, so a behind-main but
    #    conflict-free PR merges cleanly. We deliberately do NOT call
    #    /pulls/{n}/update first: update triggers Gitea dismiss_stale_approvals,
    #    which would dismiss the PR's genuine approvals and force a full
    #    re-review every tick — the rebase-churn bottleneck that collapsed
    #    throughput to ~0/hr with dozens of mergeable PRs open.
    #
    #    The merge bar is UNCHANGED: we only reach here with main green +
    #    >= required genuine approvals on the current head + no open
    #    REQUEST_CHANGES + every BP-required context green. The trade-off is
    #    that the PR's CI ran on a possibly-behind base, so a SEMANTIC main-break
    #    is caught by POST-merge main CI (step 1's pause backstop) rather than
    #    pre-merge. force_merge is used ONLY for missing-but-non-required
    #    governance reds (required are green + approvals genuine), never to
    #    bypass a failing required context or an approval shortfall.
    if mergeable is True:
        force = _non_required_red_present(latest, required_contexts)
        return MergeDecision(True, "merge", "ready", force=force)

    # 6) NOT (yet) mergeable. TRI-STATE, fail-closed — never merge on an unknown.
    #    We MUST distinguish "still computing" (None/missing) from a "definitive
    #    conflict" (False); collapsing them would route a behind-main but
    #    STILL-COMPUTING PR into the /update path, whose dismiss_stale_approvals
    #    is the rebase-churn this change eliminates.
    #
    #    mergeable is None  → Gitea has NOT finished computing conflict state.
    #    WAIT: do nothing this tick — never /update (would dismiss genuine
    #    approvals during the compute window → churn), never merge. Re-check next
    #    tick once Gitea reports a decisive True/False.
    if mergeable is None:
        return MergeDecision(
            False, "wait",
            "PR mergeability is still being computed (mergeable=None) — waiting",
        )

    # mergeable is False → DEFINITIVE not-mergeable. If the head also does not
    #    contain current main, try the /update path to refresh the branch (this
    #    may resolve a behind-main non-conflict; a real conflict returns HTTP 409
    #    and process_once HOLDs the PR per #2352). If the head already contains
    #    current main yet Gitea still reports not-mergeable, there is nothing the
    #    queue can do (genuine conflict against current main) — WAIT.
    if not pr_has_current_base:
        return MergeDecision(False, "update", "PR not mergeable and head does not contain current main")
    return MergeDecision(False, "wait", "PR is not mergeable (conflicts)")


def get_branch_head(branch: str) -> str:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/branches/{branch}")
    commit = body.get("commit") if isinstance(body, dict) else None
    sha = commit.get("id") if isinstance(commit, dict) else None
    if not isinstance(sha, str) or len(sha) < 7:
        raise ApiError(f"branch {branch} response missing commit id")
    return sha


def _snapshot_status_for_sha(sha: str) -> dict | None:
    """Return a Gitea-shaped combined-status dict from the conductor snapshot
    if the SHA matches an open PR head, else None."""
    snapshot = load_conductor_snapshot()
    if snapshot is None:
        return None
    for pr in (snapshot.get("prs") or []):
        if pr.get("head_sha") == sha:
            statuses = pr.get("statuses") or []
            return {
                "state": pr.get("combined_state", "unknown"),
                "statuses": [
                    {"context": s.get("context"), "status": s.get("status")}
                    for s in statuses
                    if isinstance(s, dict)
                ],
            }
    return None


def get_combined_status(sha: str) -> dict:
    """Combined status + all individual statuses for `sha`.

    Uses the conductor snapshot when available (same tick, same observed
    state as the merge-queue pass), otherwise self-fetches via API.
    """
    snapshot_status = _snapshot_status_for_sha(sha)
    if snapshot_status is not None:
        return snapshot_status

    _, combined = api("GET", f"/repos/{OWNER}/{NAME}/commits/{sha}/status")
    if not isinstance(combined, dict):
        raise ApiError(f"status for {sha} response not object")
    combined_statuses: list[dict] = combined.get("statuses") or []
    all_statuses = api_paginated(
        "GET",
        f"/repos/{OWNER}/{NAME}/commits/{sha}/statuses",
    )
    # Build latest per context: process combined (ascending→reverse=newest
    # first), then fill gaps from all_statuses (already newest-first).
    latest: dict[str, dict] = {}
    for status in reversed(sorted(combined_statuses, key=lambda s: s.get("id") or 0)):
        ctx = status.get("context")
        if isinstance(ctx, str) and ctx not in latest:
            latest[ctx] = status
    for status in all_statuses:
        ctx = status.get("context")
        if isinstance(ctx, str) and ctx not in latest:
            latest[ctx] = status
    combined["statuses"] = list(latest.values())
    return combined


def _snapshot_pr_to_issue(pr_entry: dict) -> dict:
    """Normalise a conductor-snapshot PR entry into the shape the queue
    expects from /issues (number, title, labels, pull_request sub-dict)."""
    return {
        "number": pr_entry.get("number"),
        "title": pr_entry.get("title"),
        "labels": [{"name": n} for n in (pr_entry.get("labels") or [])],
        "pull_request": {"draft": False},
        "created_at": "",
    }


def list_queued_issues() -> list[dict]:
    snapshot = load_conductor_snapshot()
    if snapshot is not None:
        prs = snapshot.get("prs") or []
        return [
            _snapshot_pr_to_issue(p)
            for p in prs
            if QUEUE_LABEL in (p.get("labels") or [])
        ]
    return api_paginated(
        "GET",
        f"/repos/{OWNER}/{NAME}/issues",
        query={
            "state": "open",
            "type": "pulls",
            "label": QUEUE_LABEL,
        },
    )


def list_candidate_issues(*, auto_discover: bool) -> list[dict]:
    """Open PR issues eligible for consideration this tick.

    With auto_discover=True (default) this enumerates ALL open PRs (no label
    filter) so the queue is self-sustaining — a ready PR is considered without
    any human/agent first adding QUEUE_LABEL. With auto_discover=False it falls
    back to the legacy label-filtered listing (opt-IN). Opt-out filtering and
    draft-skipping happen in choose_next_candidate_issue, not here.
    """
    snapshot = load_conductor_snapshot()
    if snapshot is not None:
        prs = snapshot.get("prs") or []
        return [_snapshot_pr_to_issue(p) for p in prs]
    if not auto_discover:
        return list_queued_issues()
    return api_paginated(
        "GET",
        f"/repos/{OWNER}/{NAME}/issues",
        query={
            "state": "open",
            "type": "pulls",
        },
    )


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
    try:
        api(
            "POST",
            f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/update",
            query={"style": UPDATE_STYLE},
            expect_json=False,
        )
    except ApiError as exc:
        # Gitea returns HTTP 409 when the base cannot be merged into the PR
        # branch because of a real conflict. The queue cannot auto-resolve a
        # conflict, so re-raise as BranchUpdateConflictError; process_once HOLDs
        # the PR and advances (HOL guard) instead of retrying it forever.
        # Match the HTTP STATUS token ("-> HTTP 409") specifically, not a bare
        # "409" substring — the PR number or path can itself contain "409"
        # (e.g. /pulls/1409/update) and must not be misread as a conflict.
        if "-> HTTP 409" in str(exc):
            raise BranchUpdateConflictError(str(exc)) from exc
        raise  # re-raise other ApiErrors unchanged


def add_label_by_name(pr_number: int, label_name: str, *, dry_run: bool) -> None:
    """Apply an existing repo label (by name) to a PR/issue.

    Used to HOLD a wedged PR so the queue advances. Resolves the label id from
    the repo label set; if the label does not exist, raises ApiError (the
    caller decides whether that is fatal).
    """
    print(f"::notice::applying label '{label_name}' to PR #{pr_number}")
    if dry_run:
        return
    _, labels = api("GET", f"/repos/{OWNER}/{NAME}/labels", query={"limit": "100"})
    label_id = None
    if isinstance(labels, list):
        for label in labels:
            if isinstance(label, dict) and label.get("name") == label_name:
                label_id = label.get("id")
                break
    if label_id is None:
        raise ApiError(f"label '{label_name}' not found in repo {OWNER}/{NAME}")
    api(
        "POST",
        f"/repos/{OWNER}/{NAME}/issues/{pr_number}/labels",
        body={"labels": [label_id]},
    )


def close_pull(pr_number: int, *, dry_run: bool) -> None:
    """Close a PR (issues endpoint; PRs are issues in Gitea's data model).

    Used by dup_close_superseded_bump_prs() to retire older bump-bot
    PRs that have been superseded by a fresh bump. Network/HTTP
    errors propagate as ApiError so the caller can decide whether
    to log-and-continue or fail the tick.

    The body is intentionally empty (Gitea does not require a close
    reason; the explanatory comment is posted separately).
    """
    if dry_run:
        print(f"::notice::(dry-run) would close PR #{pr_number}")
        return
    api("PATCH", f"/repos/{OWNER}/{NAME}/issues/{pr_number}", body={"state": "closed"})


def hold_pr(pr_number: int, hold_note: str, *, dry_run: bool) -> None:
    """Apply HOLD_LABEL to a wedged PR so the queue advances past it.

    choose_next_queued_issue skips HOLD_LABEL-bearing PRs, so this is the HOL
    guard: a PR the queue cannot make progress on (permanent permission error
    or unresolvable branch-update conflict) is held and a human/agent fixes it,
    rather than the queue re-selecting it every tick forever. If the label
    cannot be applied we still post the explanatory comment so the wedge is at
    least visible — but we never loop on the PR.
    """
    try:
        add_label_by_name(pr_number, HOLD_LABEL, dry_run=dry_run)
    except ApiError as label_exc:
        sys.stderr.write(
            f"::error::could not apply HOLD_LABEL to PR #{pr_number}: {label_exc}\n"
        )
        hold_note += (
            f"\n\n(NOTE: could not apply the hold label automatically: "
            f"{label_exc}. Please add `{HOLD_LABEL}` manually.)"
        )
    post_comment(pr_number, hold_note, dry_run=dry_run)


def merge_pull(pr_number: int, *, dry_run: bool, force: bool = False) -> None:
    payload: dict[str, Any] = {
        "Do": "merge",
        "MergeTitleField": f"Merge PR #{pr_number} via Gitea merge queue",
        "MergeMessageField": (
            "Serialized merge by gitea-merge-queue after current-main, "
            "genuine approvals, and required CI checks were green."
        ),
    }
    if force:
        # force_merge bypasses ONLY missing-but-non-required governance
        # contexts. The caller has already verified required contexts are green
        # and genuine approvals are present, so this never bypasses a failing
        # required context or an approval shortfall.
        payload["force_merge"] = True
    print(f"::notice::merging PR #{pr_number}{' (force_merge: non-required reds)' if force else ''}")
    if dry_run:
        return
    try:
        api("POST", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/merge", body=payload, expect_json=False)
    except ApiError as exc:
        # Re-raise permission-like errors so process_once can HOLD this PR.
        # 403 = no push access, 404 = repo/pr not found, 405 = not allowed.
        msg = str(exc)
        for code in ("403", "404", "405"):
            if code in msg:
                raise MergePermissionError(msg) from exc
        raise  # re-raise other ApiErrors unchanged


def process_once(*, dry_run: bool = False) -> int:
    # Required status contexts come from BRANCH PROTECTION, not a hand-kept env
    # list. Fail-closed: if BP cannot be enumerated, HOLD the whole tick rather
    # than merge against an unverified required set.
    try:
        bp = get_branch_protection(WATCH_BRANCH)
    except BranchProtectionUnavailable as exc:
        sys.stderr.write(
            f"::error::queue held: branch protection for {WATCH_BRANCH} "
            f"unavailable (fail-closed): {exc}\n"
        )
        return 0
    # Uniform gate: governance checks are ALWAYS required, even if branch
    # protection does not enumerate them. Deduplicate against BP list.
    contexts = list(dict.fromkeys(bp.required_contexts + GOVERNANCE_REQUIRED_CONTEXTS))
    required_approvals = bp.required_approvals
    print(
        f"::notice::queue policy from branch protection: "
        f"required_approvals={required_approvals} "
        f"required_contexts={contexts or '[none]'}"
    )

    main_sha = get_branch_head(WATCH_BRANCH)
    main_status = get_combined_status(main_sha)
    # Check push-required contexts explicitly instead of combined state.
    # See evaluate_merge_readiness for rationale.
    main_latest = latest_statuses_by_context(main_status.get("statuses") or [])
    main_ok, main_bad = required_contexts_green(main_latest, push_required_contexts())
    if not main_ok:
        print(f"::notice::queue paused: {WATCH_BRANCH}@{main_sha[:8]} required contexts not green: {', '.join(main_bad)}")
        return 0

    candidates = choose_candidate_issues(
        list_candidate_issues(auto_discover=AUTO_DISCOVER),
        queue_label=QUEUE_LABEL,
        opt_out_labels=OPT_OUT_LABELS,
        auto_discover=AUTO_DISCOVER,
    )
    if not candidates:
        print(
            "::notice::no merge candidates "
            f"(auto_discover={'on' if AUTO_DISCOVER else 'off'})"
        )
        return 0

    # Runtime-bump dup-close (RFC internal#131 PR-A). If multiple bump-bot
    # PRs are open for the same (runtime, version) slot (e.g. the bot
    # repushed after a red CI run), the OLDEST has stale CI. The exemption
    # would auto-merge the first one the FIFO scan reaches — but we want
    # the FRESHEST to win (its smoke run is the relevant one). Close all
    # but the freshest per group so the auto-merge path only ever sees
    # the fresh bump. Best-effort: a single close failure logs and
    # continues (does NOT abort the tick).
    try:
        closed_dups = dup_close_superseded_bump_prs(
            candidates,
            close_fn=close_pull,
            comment_fn=post_comment,
            dry_run=dry_run,
        )
        if closed_dups:
            print(
                f"::notice::dup-close retired {len(closed_dups)} superseded "
                f"bump PR(s): {closed_dups}"
            )
    except ApiError as exc:
        # Defensive: dup_close_superseded_bump_prs catches per-close
        # ApiError internally; an outer raise means something more
        # fundamental is wrong. Log and continue — dup-close is an
        # optimization, not a gate.
        sys.stderr.write(
            f"::warning::dup-close encountered unexpected error: {exc}\n"
        )

    # HOL fix: SCAN THROUGH the FIFO candidate list until a PR we can ACT on is
    # found, instead of locking on the oldest and waiting. A non-ready candidate
    # (decision.action == "wait": REQUEST_CHANGES, mergeable!=True, insufficient
    # genuine approvals, or red required CI) is SKIPPED — it must NOT head-of-line
    # block the newer ready PRs behind it. The merge bar is unchanged: a skipped
    # PR is never merged, and the first ACTIONABLE candidate (an "update" that
    # advances a stale branch, or a fully-ready "merge") terminates the scan.
    #
    # `update` is treated as actionable, not skippable: a PR whose head merely
    # lacks current main is in a legitimate in-progress state (updating it +
    # rerunning CI moves it toward ready), unlike a PR that can never become
    # ready without a human (RC / conflict), which is a `wait` and gets skipped.
    for issue in candidates:
        decision, ctx = _evaluate_candidate(
            issue,
            main_sha=main_sha,
            main_status=main_status,
            required_contexts=contexts,
            required_approvals=required_approvals,
            dry_run=dry_run,
        )
        if decision is None:
            continue  # not merge-eligible (not-open / opted-out / fork / wrong base)
        pr_number = ctx["pr_number"]
        print(f"::notice::PR #{pr_number} decision={decision.action}: {decision.reason}")
        if decision.action == "wait":
            # Non-ready: skip to the next candidate (no HOL block, no merge).
            continue
        if decision.action == "update":
            try:
                update_pull(pr_number, dry_run=dry_run)
            except BranchUpdateConflictError as exc:
                # The branch cannot be updated with main because of a real
                # conflict (HTTP 409 from /update). This is the #2352 HOL guard:
                # a conflict will not self-resolve without a human/agent rebase,
                # so re-attempting the update every tick would head-of-line block
                # every ready PR behind it. HOLD this PR (apply HOLD_LABEL, which
                # is an opt-out label so later ticks skip it) and CONTINUE the
                # scan so a newer ready PR can still merge this tick. Fail-closed:
                # a held PR is skipped, never merged.
                sys.stderr.write(
                    f"::error::branch-update conflict for PR #{pr_number}: {exc}\n"
                )
                hold_note = (
                    "merge-queue: could not update this branch with "
                    f"`{WATCH_BRANCH}` — the update returned a merge conflict "
                    f"(HTTP 409) that the queue cannot auto-resolve ({exc}). "
                    f"Applied `{HOLD_LABEL}` to unblock the queue (HOL guard). "
                    f"Fix: rebase/merge `{WATCH_BRANCH}` into this branch and "
                    f"resolve the conflicts, then remove `{HOLD_LABEL}` to requeue."
                )
                hold_pr(pr_number, hold_note, dry_run=dry_run)
                continue  # held — keep scanning for a mergeable candidate
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
            try:
                merge_pull(pr_number, dry_run=dry_run, force=decision.force)
            except MergePermissionError as exc:
                # Permanent merge failure (HTTP 403/404/405). HOLD this PR by
                # applying HOLD_LABEL (it becomes an opt-out label, so subsequent
                # ticks skip it) and CONTINUE scanning so the queue still advances
                # to the next ready PR this tick rather than stalling.
                sys.stderr.write(f"::error::merge permission error for PR #{pr_number}: {exc}\n")
                hold_note = (
                    "merge-queue: merge failed with a permanent permission error "
                    f"({exc}). No available token has Can-merge permission for this "
                    f"PR. Applied `{HOLD_LABEL}` to unblock the queue (HOL guard). "
                    f"Fix: grant Can-merge to the queue token, then remove "
                    f"`{HOLD_LABEL}` to requeue."
                )
                try:
                    add_label_by_name(pr_number, HOLD_LABEL, dry_run=dry_run)
                except ApiError as label_exc:
                    # If we cannot even apply the hold label, fall back to a comment
                    # so the wedge is at least visible; do NOT loop on this PR.
                    sys.stderr.write(
                        f"::error::could not apply HOLD_LABEL to PR #{pr_number}: {label_exc}\n"
                    )
                    hold_note += (
                        f"\n\n(NOTE: could not apply the hold label automatically: "
                        f"{label_exc}. Please add `{HOLD_LABEL}` manually.)"
                    )
                post_comment(pr_number, hold_note, dry_run=dry_run)
                continue  # held — keep scanning for a mergeable candidate
            return 0
    return 0


def get_pull_diff_files(pr_number: int) -> list[dict]:
    """Fetch the list of files changed in a PR with their unified-diff patch.

    Gitea endpoint: GET /repos/{owner}/{name}/pulls/{pr}/files. Returns a
    list of `{filename, status, additions, deletions, changes, patch}` dicts.
    The `patch` field is the unified diff (may be None/empty for binary
    files or files beyond the Gitea diff-size cap).

    Used by is_runtime_bump_exempt() to inspect the .runtime-version diff
    (the only file a qualifying bump touches). Network/HTTP errors are
    caught and converted to ApiError by the api() helper; the caller
    treats an empty list as "no diff info available" (fail-closed: the
    exemption check will see an empty pr_files and reject the PR).

    Fail-safe: in addition to ApiError, any other exception (a malformed
    URL, a urllib transport error, a JSON decode failure, etc.) is
    swallowed and an empty list returned. is_runtime_bump_exempt() will
    then reject the PR (no .runtime-version found), so the PR takes the
    normal 2-genuine path. The empty list is the fail-closed sentinel.
    """
    try:
        _, body = api("GET", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/files")
    except (ApiError, ValueError, TypeError) as exc:
        # ValueError: malformed URL (urllib raised this in test monkeypatches
        # that don't set a real host). TypeError: rare but defensive. ApiError:
        # the documented HTTP failure mode. All three mean "diff not
        # fetchable" → fail-closed empty list.
        sys.stderr.write(
            f"::warning::get_pull_diff_files failed for PR #{pr_number}: {exc}\n"
        )
        return []
    if not isinstance(body, list):
        return []
    return body


def latest_runtime_v_tag_for(pr: dict) -> str | None:
    """Query the runtime repo for the latest runtime-v release tag matching
    the .runtime-version bump on `pr`. Returns the tag string, or None if
    it can't be determined (network error, no matching repo, no tags, etc.).

    FAIL-CLOSED contract: returning None is enough to fail the ==SSOT
    condition in is_runtime_bump_exempt — the PR will not be exempted.

    The lookup strategy:
      1. Read the runtime name from the pr's head branch name
         (bump-bot uses branch names like `runtime-bump/claude-code/v1.2.3`
         per the 9c2e9c88 spec; if the branch doesn't match, return None).
      2. Query the runtime repo's tags list
         (GET /repos/molecule-ai/molecule-ai-runtime-{name}/tags).
      3. Return the most recent tag that is exactly a `vMAJOR.MINOR.PATCH`
         version (so a v0.x.y dev tag doesn't accidentally satisfy).

    To keep the implementation minimal and testable, the runtime repo name
    is derived from a convention: the runtime name (extracted from the
    branch) maps to `molecule-ai/molecule-ai-runtime-<name>`. If the
    convention doesn't hold for a given runtime, the operator can set
    RUNTIME_BUMP_REPO_TEMPLATE env var to override (e.g.
    `org/repo-{name}-runtime`).
    """
    repo_template = _env(
        "RUNTIME_BUMP_REPO_TEMPLATE",
        default="molecule-ai/molecule-ai-runtime-{name}",
    )
    # Extract the runtime name from the branch. The branch format is
    # `runtime-bump/<runtime-name>/v<version>` per the 9c2e9c88 spec.
    head = pr.get("head") or {}
    branch = head.get("ref", "") if isinstance(head, dict) else ""
    if not isinstance(branch, str) or not branch.startswith("runtime-bump/"):
        return None
    parts = branch.split("/")
    if len(parts) != 3:
        return None
    runtime_name = parts[1]
    if not runtime_name:
        return None
    runtime_repo = repo_template.format(name=runtime_name)
    try:
        owner, name = runtime_repo.split("/", 1)
    except ValueError:
        return None
    try:
        _, tags = api("GET", f"/repos/{owner}/{name}/tags", query={"limit": "20"})
    except ApiError:
        return None
    if not isinstance(tags, list):
        return None
    # Pick the most recent tag that looks like a clean semver `vX.Y.Z`
    # (filter out pre-release / build-metadata / non-version tags).
    semver_re = re.compile(r"^v\d+\.\d+\.\d+$")
    for tag in tags:
        if not isinstance(tag, dict):
            continue
        tag_name = tag.get("name", "")
        if isinstance(tag_name, str) and semver_re.match(tag_name):
            return tag_name
    return None


def _is_active_release_candidate(prs: list[dict], *, pr_number: int) -> bool:
    """True if any open PR (other than `pr_number`) carries a label
    indicating a release-candidate is in progress.

    The label name is taken from RELEASE_CANDIDATE_LABELS env var
    (comma-separated, default "release-candidate,rc"). An open PR
    carrying ANY of these labels blocks the runtime-bump exemption —
    the bump would race with the RC and potentially land before the
    RC smoke completes.
    """
    rc_labels = {
        name.strip()
        for name in _env(
            "RELEASE_CANDIDATE_LABELS",
            default="release-candidate,rc",
        ).split(",")
        if name.strip()
    }
    for other in prs:
        if not isinstance(other, dict):
            continue
        try:
            other_number = int(other.get("number", 0))
        except (TypeError, ValueError):
            continue
        if other_number == pr_number:
            continue  # don't count the PR being evaluated
        if other.get("state") != "open":
            continue
        other_labels = label_names(other)
        if other_labels & rc_labels:
            return True
    return False


def _early_skip_reason(
    pr: dict,
    *,
    watch_branch: str,
) -> tuple[str | None, str | None]:
    """Return an observable skip reason for PRs that are not merge-eligible.

    Returns (reason, pr_comment). When reason is non-None the PR should be
    skipped by the queue. pr_comment is the body to post on the PR; None means
    the skip is silent w.r.t. PR comments (the reason is still emitted as a
    workflow notice by the caller). This keeps the early-skip decision pure and
    unit-testable while preserving the observable-vs-silent distinction.
    """
    if pr.get("state") != "open":
        return "not open", None
    if OPT_OUT_LABELS & label_names(pr):
        return "opt-out label", None
    if pr.get("draft") is True:
        return "draft", None
    if pr.get("base", {}).get("ref") != watch_branch:
        # Silent w.r.t. PR comments: stacked PRs whose base is not main are
        # re-evaluated every conductor tick; posting a comment each time floods
        # the PR. The caller still emits a workflow notice so the skip is
        # observable in logs.
        return f"base is not `{watch_branch}`", None
    if pr.get("head", {}).get("repo_id") != pr.get("base", {}).get("repo_id"):
        return "fork PR", "merge-queue: skipped; fork PRs are not supported by the serialized queue."
    return None, None


def _evaluate_candidate(
    issue: dict,
    *,
    main_sha: str,
    main_status: dict,
    required_contexts: list[str],
    required_approvals: int,
    dry_run: bool,
) -> tuple[MergeDecision | None, dict]:
    """Evaluate a single auto-discovered candidate against the full merge bar.

    Returns (decision, ctx) where ctx carries {"pr_number"}. A None decision
    means the PR is not merge-eligible at all (not open / opted-out / draft /
    fork / wrong base) and the caller should skip to the next candidate; for
    fork / wrong-base the explanatory comment is posted here before returning.

    The merge bar is UNCHANGED from the single-PR path — this only factors the
    per-PR evaluation out so process_once can scan multiple candidates. A failed
    status fetch still raises (fail-closed): it propagates to the caller so the
    PR is never treated as green.
    """
    pr_number = int(issue["number"])
    ctx = {"pr_number": pr_number}
    pr = get_pull(pr_number)
    reason, pr_comment = _early_skip_reason(pr, watch_branch=WATCH_BRANCH)
    if reason is not None:
        # Observable in workflow logs (LOUD relative to silent disappearance),
        # but avoid PR-comment noise for the high-volume skip classes.
        print(f"::notice::PR #{pr_number} {reason}; skipping")
        if pr_comment is not None:
            post_comment(pr_number, pr_comment, dry_run=dry_run)
        return None, ctx

    head_sha = pr.get("head", {}).get("sha")
    if not isinstance(head_sha, str) or len(head_sha) < 7:
        raise ApiError(f"PR #{pr_number} missing head sha")
    commits = get_pull_commits(pr_number)
    current_base = pr_has_current_base(pr, commits, main_sha)
    # Fail-closed: a failed status fetch raises here and propagates (the PR is
    # never treated as green).
    pr_status = get_combined_status(head_sha)
    pr_labels = label_names(pr)
    # FAIL-CLOSED, TRI-STATE: Gitea returns mergeable=None (or omits the field)
    # while it is still COMPUTING conflict state, mergeable=False for a definitive
    # conflict, and mergeable=True only when it has proven the PR conflict-free.
    # We preserve all THREE states (do NOT collapse None/missing into False):
    #   - True            → direct-merge eligible (step 5).
    #   - None / missing  → still computing → WAIT (never merge, never update,
    #                       never dismiss approvals); re-check next tick.
    #   - False           → definitive conflict → the update/hold path (step 6).
    # Collapsing None→False would route a behind-main but STILL-COMPUTING PR into
    # the /update path, which triggers dismiss_stale_approvals — the exact
    # rebase-churn this change eliminates. Normalize only to the literal True /
    # False / None set (some Gitea versions omit the key entirely → None).
    raw_mergeable = pr.get("mergeable")
    mergeable: bool | None = raw_mergeable if isinstance(raw_mergeable, bool) else None

    reviews = get_pull_reviews(pr_number)
    approvers, request_changes = genuine_approvals(
        reviews, headsha=head_sha, reviewer_set=REVIEWER_SET
    )

    # Runtime-bump exemption (RFC internal#131). Compute the exempt flag here
    # so it flows into evaluate_merge_readiness. The check itself lives in
    # is_runtime_bump_exempt() and is fail-closed — any unverifiable guard
    # or condition returns (False, reason), in which case the PR takes the
    # normal 2-genuine path. The PR is exempt ONLY if every guard + every
    # condition holds, AND an active release-candidate is NOT in progress.
    #
    # Fail-safe wrap: each helper (get_pull_diff_files,
    # latest_runtime_v_tag_for, _is_active_release_candidate) issues its own
    # API calls. A network blip or a partial outage must not abort the
    # whole tick; on error we default to NOT exempt (the PR takes the
    # normal 2-genuine path). This is consistent with the fail-closed
    # design: when in doubt, require 2-genuine.
    runtime_bump_exempt = False
    exempt_reason = "exemption check not run"
    try:
        pr_files = get_pull_diff_files(pr_number)
        latest_tag = latest_runtime_v_tag_for(pr)
        try:
            rc_prs = list_candidate_issues(auto_discover=False)
        except (ApiError, ValueError, TypeError) as exc:
            # list_candidate_issues can also hit the network via
            # api_paginated; if it errors here, treat as "no active RC"
            # (the conservative read — the PR may be exempted if every
            # other condition holds, but only when the call succeeds
            # and returns a clean list). Logging only — fail-closed
            # elsewhere (empty pr_files / None latest_tag → not exempt).
            sys.stderr.write(
                f"::warning::list_candidate_issues for RC check failed on "
                f"PR #{pr_number}: {exc}\n"
            )
            rc_prs = []
        rc_active = _is_active_release_candidate(prs=rc_prs, pr_number=pr_number)
        runtime_bump_exempt, exempt_reason = is_runtime_bump_exempt(
            pr=pr,
            pr_files=pr_files,
            required_contexts=required_contexts,
            latest_runtime_v_tag=latest_tag,
            rc_active=rc_active,
        )
    except (ApiError, ValueError, TypeError) as exc:
        # API error fetching diff / tag / RC status → fail-closed:
        # treat as not exempt, continue with normal 2-genuine path.
        runtime_bump_exempt = False
        exempt_reason = f"exemption check errored (fail-closed): {exc}"
    if runtime_bump_exempt:
        # Observable so an audit can confirm WHY the approvals gate was
        # bypassed. The reason is also returned via the MergeDecision
        # field if a merge decision is taken.
        print(f"::notice::PR #{pr_number} runtime-bump exempt: {exempt_reason}")
    else:
        # Only log "not exempt" at debug level (we don't want to spam
        # the workflow log for the common case). The decision.reason
        # returned by evaluate_merge_readiness will carry the real
        # "insufficient genuine approvals" message on the normal path.
        pass

    decision = evaluate_merge_readiness(
        main_status=main_status,
        pr_status=pr_status,
        required_contexts=required_contexts,
        required_approvals=required_approvals,
        approvers=approvers,
        request_changes=request_changes,
        pr_has_current_base=current_base,
        mergeable=mergeable,
        pr_labels=pr_labels,
        runtime_bump_exempt=runtime_bump_exempt,
    )
    return decision, ctx


@dataclasses.dataclass(frozen=True)
class ReadinessEntry:
    """One candidate's readiness state."""

    pr_number: int
    decision: MergeDecision | None
    reason: str


def enumerate_readiness(*, dry_run: bool = False) -> list[ReadinessEntry]:
    """Evaluate ALL candidates and return their readiness states.

    Fail-closed: if branch protection cannot be fetched, raise
    BranchProtectionUnavailable (caller must handle). Unlike
    process_once, this does NOT stop at the first actionable candidate;
    it evaluates every eligible PR and returns the full list so a
    post-batch summary can be printed.
    """
    bp = get_branch_protection(WATCH_BRANCH)
    # Uniform gate: governance checks are ALWAYS required, even if branch
    # protection does not enumerate them. Deduplicate against BP list.
    contexts = list(dict.fromkeys(bp.required_contexts + GOVERNANCE_REQUIRED_CONTEXTS))
    required_approvals = bp.required_approvals

    main_sha = get_branch_head(WATCH_BRANCH)
    main_status = get_combined_status(main_sha)
    main_latest = latest_statuses_by_context(main_status.get("statuses") or [])
    main_ok, main_bad = required_contexts_green(main_latest, push_required_contexts())

    candidates = choose_candidate_issues(
        list_candidate_issues(auto_discover=AUTO_DISCOVER),
        queue_label=QUEUE_LABEL,
        opt_out_labels=OPT_OUT_LABELS,
        auto_discover=AUTO_DISCOVER,
    )

    entries: list[ReadinessEntry] = []
    for issue in candidates:
        pr_number = int(issue["number"])
        try:
            decision, ctx = _evaluate_candidate(
                issue,
                main_sha=main_sha,
                main_status=main_status,
                required_contexts=contexts,
                required_approvals=required_approvals,
                dry_run=dry_run,
            )
        except ApiError as exc:
            # Fail-closed per candidate: an unreadable PR is recorded as
            # unverifiable, not skipped silently.
            entries.append(
                ReadinessEntry(
                    pr_number=pr_number,
                    decision=None,
                    reason=f"unverifiable (API error: {exc})",
                )
            )
            continue
        if decision is None:
            entries.append(
                ReadinessEntry(
                    pr_number=pr_number,
                    decision=None,
                    reason="not merge-eligible (opt-out/draft/fork/wrong-base)",
                )
            )
            continue
        entries.append(
            ReadinessEntry(
                pr_number=pr_number,
                decision=decision,
                reason=decision.reason,
            )
        )
    return entries


def print_post_batch_summary(entries: list[ReadinessEntry]) -> None:
    """Print a structured summary of all candidates' readiness.

    Emits ::notice:: lines for machine parsing and a human-readable
    block for operator visibility.
    """
    ready = [e for e in entries if e.decision and e.decision.ready]
    waiting = [e for e in entries if e.decision and not e.decision.ready]
    ineligible = [e for e in entries if e.decision is None]

    print("::group::merge-queue readiness summary")
    print(f"total_candidates={len(entries)}")
    print(f"ready={len(ready)}")
    print(f"waiting={len(waiting)}")
    print(f"ineligible/unverifiable={len(ineligible)}")
    print("")
    for e in entries:
        state = "ready" if e.decision and e.decision.ready else (
            "waiting" if e.decision else "ineligible"
        )
        action = e.decision.action if e.decision else "n/a"
        print(f"PR #{e.pr_number}: state={state} action={action} reason={e.reason}")
    print("::endgroup::")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument(
        "--enumerate",
        action="store_true",
        help="Evaluate all candidates and print a readiness summary without merging.",
    )
    args = parser.parse_args()
    _require_runtime_env()
    try:
        if args.enumerate:
            entries = enumerate_readiness(dry_run=args.dry_run)
            print_post_batch_summary(entries)
            return 0
        return process_once(dry_run=args.dry_run)
    except ApiError as exc:
        # FAIL-CLOSED: API errors are not "transient success" — they mean
        # the queue could not evaluate merge state. Returning 0 hides
        # persistent infra issues (auth drift, endpoint outages) from
        # operators. Return 1 so the cron job surfaces red and paging fires.
        sys.stderr.write(f"::error::queue API error: {exc}\n")
        return 1
    except urllib.error.URLError as exc:
        sys.stderr.write(f"::error::queue network error: {exc}\n")
        return 1
    except TimeoutError as exc:
        sys.stderr.write(f"::error::queue timeout: {exc}\n")
        return 1


if __name__ == "__main__":
    sys.exit(main())
