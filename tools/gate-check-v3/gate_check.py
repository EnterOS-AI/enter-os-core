#!/usr/bin/env python3
"""
gate-check-v3 — SOP-6 + CI gate detector for Gitea PRs.

Emits structured verdict + human-readable summary. Designed to run as:
  1. CLI:  python gate_check.py --repo org/repo --pr N
  2. Gitea Actions step: runs this script, captures stdout JSON

Signals (MVP — signals 1,2,3,4,6):
  1. Author-aware agent-tag comment scan
  2. REQUEST_CHANGES reviews state machine
  3. Staleness detection (review.commit_id != PR.head_sha)
  4. Branch divergence / scope-creep guard (base-sha vs target HEAD)
  6. CI required-checks awareness

Exit codes:
  0 — all gates pass (verdict=CLEAR)
  1 — one or more gates blocking (verdict=BLOCKED)
  2 — API error / usage error (verdict=ERROR)
"""

import argparse
import json
import os
import re
import sys
import urllib.request
import urllib.error
from datetime import datetime, timezone

# ── Gitea API client ────────────────────────────────────────────────────────

GITEA_HOST = os.environ.get("GITEA_HOST", "git.moleculesai.app")
GITEA_TOKEN = os.environ.get("GITEA_TOKEN", os.environ.get("GITHUB_TOKEN", ""))
API_BASE = f"https://{GITEA_HOST}/api/v1"

# Timeout in seconds for all HTTP calls. Defence-in-depth: ensures a missing or
# invalid GITEA_TOKEN causes a fast (~15 s) failure rather than an
# indefinite hang. The real fix is provisioning the token; this caps worst-case
# wall-clock on a broken/unreachable Gitea host.
DEFAULT_TIMEOUT = 15


def api_get(path: str) -> dict | list:
    url = f"{API_BASE}{path}"
    req = urllib.request.Request(
        url,
        headers={
            "Authorization": f"token {GITEA_TOKEN}",
            "Accept": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=DEFAULT_TIMEOUT) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        body = e.read().decode(errors="replace")
        raise GiteaError(f"GET {url} → {e.code}: {body[:300]}")


def api_list(path: str, per_page: int = 100) -> list:
    """Paginate a list endpoint using Link headers (Gitea/GitHub convention)."""
    results = []
    page = 1
    while True:
        paged_path = f"{path}?per_page={per_page}&page={page}"
        result = api_get(paged_path)
        if isinstance(result, list):
            results.extend(result)
            if len(result) < per_page:
                break
            page += 1
        else:
            # Some endpoints return an object with a data/items key
            data = result.get("data", result.get("items", result))
            if isinstance(data, list):
                results.extend(data)
            break
        # Safety cap to avoid runaway pagination
        if page > 20:
            break
    return results


class GiteaError(Exception):
    pass


# ── Signal 1: Author-aware agent-tag comment scan ─────────────────────────────
# Matches: [core-{role}-agent] VERDICT in comment body.
# Must be authored by the agent whose role is tagged.
# Scans BOTH issue comments (/issues/{N}/comments) and PR comments
# (/pulls/{N}/comments) since agents post on both.

# Matches [core-{role}-agent] VERDICT anywhere in the comment body.
AGENT_TAG_RE = re.compile(
    r"\[core-([a-z]+)-agent\]\s+(APPROVED|N/?A|CHANGES_REQUESTED|COMMENT|BLOCKED|ACK)\b",
)

# Map agent role → canonical login (from workspace registry)
AGENT_LOGIN_MAP = {
    "qa": "core-qa",
    "security": "core-security",
    "uiux": "core-uiux",
    "lead": "core-lead",
    "devops": "core-devops",
    "be": "core-be",
    "fe": "core-fe",
    "offsec": "core-offsec",
}

# Map alternate Gitea logins → canonical logins for gate matching.
# infra-sre is the engineers/core-devops agent (same team, same work).
# Without this alias, infra-sre comments/reviews never satisfy the engineers gate.
LOGIN_ALIASES = {
    "infra-sre": "core-devops",
}

POSITIVE_VERDICTS = {"APPROVED", "N/A", "ACK"}

# Uniform required-agent set (SOP-6 tier removal, CTO 2026-06-07).
# ALL of the following must APPROVE (AND gate, strict).
REQUIRED_AGENTS = {
    "managers": "core-lead",
    "engineers": "core-devops",
    "qa": "core-qa",
    "security": "core-security",
}


def signal_1_comment_scan(pr_number: int, repo: str) -> dict:
    """
    Scan issue + PR comments AND reviews for agent-tag policy gates.
    Matches tag AND author. All REQUIRED_AGENTS must positively ACK.
    Returns: {signal, results, verdict}
    """
    owner, name = repo.split("/", 1)

    relevant_roles = REQUIRED_AGENTS

    # Build reverse map: login -> (group, agent_key)
    login_to_group = {}
    for group, login in relevant_roles.items():
        for role, role_login in AGENT_LOGIN_MAP.items():
            if role_login == login:
                login_to_group[role_login] = (group, f"core-{role}")

    # Collect all agent-tag matches from comments
    comments = []
    try:
        comments.extend(api_list(f"/repos/{owner}/{name}/issues/{pr_number}/comments"))
    except GiteaError:
        pass
    try:
        comments.extend(api_list(f"/repos/{owner}/{name}/pulls/{pr_number}/comments"))
    except GiteaError:
        pass

    # Collect APPROVED reviews from agent logins (resolving LOGIN_ALIASES)
    try:
        reviews = api_list(f"/repos/{owner}/{name}/pulls/{pr_number}/reviews")
        for r in reviews:
            login = (r.get("user") or {}).get("login", "")
            canonical = LOGIN_ALIASES.get(login, login)
            if canonical in login_to_group and r.get("state") == "APPROVED":
                comments.append(
                    {
                        "id": f"review-{r['id']}",
                        "user": {"login": canonical},
                        "body": f"[{canonical}-agent] APPROVED",
                        "created_at": r.get("submitted_at") or r.get("created_at", ""),
                        "source": "review",
                    }
                )
    except GiteaError:
        pass

    # Find latest verdict per agent login
    findings = {}
    for login, (group, agent_key) in login_to_group.items():
        matches = []
        for c in comments:
            body = c.get("body", "") or ""
            user_login = (c.get("user") or {}).get("login", "")
            # Resolve LOGIN_ALIASES so alternate logins satisfy the canonical gate
            user_login = LOGIN_ALIASES.get(user_login, user_login)
            if user_login != login:
                continue
            for m in AGENT_TAG_RE.finditer(body):
                tag_role, verdict = m.group(1), m.group(2)
                # Match the role part of the login (e.g. "core-devops" → "devops")
                login_role = login.replace("core-", "")
                if tag_role == login_role:
                    matches.append(
                        {
                            "comment_id": c["id"],
                            "verdict": verdict,
                            "user": user_login,
                            "created_at": c["created_at"],
                            "source": c.get("source", "comment"),
                        }
                    )
        latest = max(matches, key=lambda x: x["created_at"], default=None) if matches else None
        findings[agent_key] = {
            "group": group,
            "found": latest,
            "verdict": latest["verdict"] if latest else "MISSING",
        }

    # Uniform AND gate: ALL required agents must be positive.
    verdicts = [f["verdict"] for f in findings.values()]
    if not verdicts:
        gate_verdict = "N/A"
    elif all(v in POSITIVE_VERDICTS for v in verdicts):
        gate_verdict = "CLEAR"
    elif any(v in ("BLOCKED", "CHANGES_REQUESTED", "COMMENT") for v in verdicts):
        gate_verdict = "BLOCKED"
    else:
        gate_verdict = "INCOMPLETE"

    return {"signal": "agent_tag_comments", "results": findings, "verdict": gate_verdict}


# ── Signal 2: REQUEST_CHANGES reviews state machine ────────────────────────────

def signal_2_reviews(pr_number: int, repo: str) -> dict:
    """
    Check /pulls/{N}/reviews for active REQUEST_CHANGES with dismissed=false.
    This is the layer that empirically blocks Gitea merges.
    Returns: {blocking_reviews: [...], verdict}
    """
    owner, name = repo.split("/", 1)
    reviews = api_list(f"/repos/{owner}/{name}/pulls/{pr_number}/reviews")

    blocking = []
    for r in reviews:
        if (
            r.get("state") == "REQUEST_CHANGES"
            and not r.get("dismissed", False)
            and r.get("official") is not False
        ):
            login = (r.get("user") or {}).get("login", "")
            if not login:
                continue
            blocking.append(
                {
                    "review_id": r["id"],
                    "user": login,
                    "commit_id": r.get("commit_id", ""),
                    "created_at": r.get("submitted_at") or r.get("created_at", ""),
                }
            )
    return {
        "signal": "request_changes_reviews",
        "blocking_reviews": blocking,
        "verdict": "BLOCKED" if blocking else "CLEAR",
    }


# ── Signal 3: Staleness detection ────────────────────────────────────────────

WORKING_DAY_SECONDS = 9 * 3600  # SOP-12: 1 working day threshold


def signal_3_staleness(pr_number: int, repo: str) -> dict:
    """
    Flag reviews where review.commit_id != PR.head_sha AND
    time_since_review > 1 working day. Per SOP-12 (internal#282).
    Returns: {stale_reviews: [...], verdict}
    """
    owner, name = repo.split("/", 1)

    # Get PR head sha
    pr = api_get(f"/repos/{owner}/{name}/pulls/{pr_number}")
    head_sha = pr["head"]["sha"]

    reviews = api_list(f"/repos/{owner}/{name}/pulls/{pr_number}/reviews")

    stale = []
    now = datetime.now(timezone.utc)
    for r in reviews:
        review_commit = r.get("commit_id", "")
        if review_commit and review_commit != head_sha:
            # Review predates current head
            try:
                created = datetime.fromisoformat(r["created_at"].replace("Z", "+00:00"))
            except (KeyError, ValueError):
                continue
            age_seconds = (now - created).total_seconds()
            if age_seconds > WORKING_DAY_SECONDS:
                stale.append(
                    {
                        "review_id": r["id"],
                        "user": r["user"]["login"],
                        "review_commit": review_commit,
                        "pr_head": head_sha,
                        "age_hours": round(age_seconds / 3600, 1),
                        "created_at": r.get("submitted_at") or r.get("created_at", ""),
                    }
                )
    return {
        "signal": "stale_reviews",
        "stale_reviews": stale,
        "verdict": "STALE-RC" if stale else "CLEAR",
    }


# ── Signal 4: Branch divergence / scope-creep guard ─────────────────────────
# Detects stale PR branches where the base SHA has drifted behind target HEAD.
# Distinguishes files that are "inherited" from base divergence (already on
# target via prior commits) from genuinely new PR work. Prevents misattribution
# of scope creep when branches are stale (molecule-core#365).


def _commits_and_files_behind(
    owner: str, name: str, base_sha: str, target_branch: str
) -> tuple[int | None, set[str]]:
    """Paginate target-branch commits from HEAD back to base_sha.
    Return (commits_behind_count, set of filenames changed in those commits).
    Safety-capped at 20 pages (~1000 commits) to avoid runaway pagination.
    """
    commits_behind = 0
    target_files: set[str] = set()
    page = 1
    max_pages = 20
    per_page = 50

    while page <= max_pages:
        try:
            commits = api_get(
                f"/repos/{owner}/{name}/commits?sha={target_branch}&page={page}&limit={per_page}"
            )
        except GiteaError:
            return (None, target_files)

        if not isinstance(commits, list):
            return (None, target_files)

        for c in commits:
            if c.get("sha") == base_sha:
                return (commits_behind, target_files)
            commits_behind += 1
            for f in c.get("files", []):
                fname = f.get("filename") or f.get("name", "")
                if fname:
                    target_files.add(fname)

        if len(commits) < per_page:
            break
        page += 1

    return (commits_behind if commits_behind > 0 else None, target_files)


def signal_4_branch_divergence(
    pr_number: int, repo: str, pr_data: dict | None = None
) -> dict:
    """
    Compare PR.base.sha to current target-branch HEAD.
    If diverged, show "inherited from base divergence" vs "actual new work"
    file fractions using the commits API.
    Returns: {signal, verdict, diverged, commits_behind, inherited_fraction, ...}
    """
    owner, name = repo.split("/", 1)

    if pr_data is None:
        pr_data = api_get(f"/repos/{owner}/{name}/pulls/{pr_number}")

    base_sha = pr_data["base"]["sha"]
    target_branch = pr_data["base"]["ref"]

    try:
        branch_info = api_get(f"/repos/{owner}/{name}/branches/{target_branch}")
        target_head = branch_info["commit"]["id"]
    except GiteaError as e:
        return {"signal": "branch_divergence", "verdict": "N/A", "error": str(e)}

    if base_sha == target_head:
        return {
            "signal": "branch_divergence",
            "verdict": "CLEAR",
            "diverged": False,
            "commits_behind": 0,
            "pr_files_count": 0,
            "inherited_files": [],
            "new_work_files": [],
            "inherited_fraction": 0.0,
        }

    # Branch is diverged — count commits behind and collect files changed on
    # target since the PR's base snapshot.
    commits_behind, target_files = _commits_and_files_behind(
        owner, name, base_sha, target_branch
    )

    # Get PR files
    try:
        pr_files_data = api_list(f"/repos/{owner}/{name}/pulls/{pr_number}/files")
        pr_files = {
            f.get("filename") or f.get("name", "") for f in pr_files_data
        }
        pr_files.discard("")
    except GiteaError:
        pr_files = set()

    inherited_files = sorted(pr_files & target_files)
    new_work_files = sorted(pr_files - target_files)
    total = len(pr_files)
    inherited_fraction = len(inherited_files) / total if total else 0.0

    # Verdict: WARNING if significant divergence.
    # Thresholds: >50 % inherited files, or >5 commits behind with any inherited files.
    if inherited_fraction > 0.5 or (
        commits_behind and commits_behind > 5 and inherited_files
    ):
        verdict = "WARNING"
    else:
        verdict = "CLEAR"

    return {
        "signal": "branch_divergence",
        "verdict": verdict,
        "diverged": True,
        "base_sha": base_sha,
        "target_head": target_head,
        "commits_behind": commits_behind,
        "pr_files_count": total,
        "inherited_files": inherited_files,
        "new_work_files": new_work_files,
        "inherited_fraction": round(inherited_fraction, 2),
    }


# ── Signal 6: CI required-checks awareness ───────────────────────────────────

# Governance checks that are ALWAYS required for every PR, regardless of
# branch-protection configuration. These are the uniform-gate checks that
# must pass before any PR can merge (SOP tier removal makes them mandatory
# for all PRs, not just tier:medium/tier:high).
GOVERNANCE_REQUIRED_CONTEXTS = [
    "qa-review / approved (pull_request_target)",
    "security-review / approved (pull_request_target)",
    "sop-checklist / all-items-acked (pull_request_target)",
]


# ── Signal 7: destructive-diff guard (core#2875) ────────────────────────────

# Detects a stale-or-destructive PR: a branch that has either
# (a) inherited massive destructive changes from its base (the
# pre-stale state of the base has since been heavily edited on the
# target branch, so the PR is now reverting large swaths of current
# main), OR
# (b) grown the destructive diff itself (files_changed >= 200 OR
# net_deleted_lines >= 5000).
#
# Real example (the RCA that motivated this guard): PR #1100 was an
# all-required-green stale branch whose current head reverted 481
# files / -55k lines of current main. The all-required status
# check was green but the diff was destructive. This signal blocks
# that case at the gate level so it can never silently merge.
#
# Two-tier heuristic per Researcher's #2875 proposal:
#   - BLOCK: high-confidence destructive condition (the destructive
#     diff + the branch-diverged / stale state) AND not explicitly
#     opted out via a refactor / migration / generated label.
#   - WARN: moderate drift (commits_behind > 10, or any of the
#     high-confidence thresholds halved) — operator should review.
#   - CLEAR: no destructive pattern detected.
#
# All PR-file count + line-count comes from the Gitea PR-files API
# (paginated; sum of `additions` + `deletions`). Refactor/migration
# exemptions are read from PR labels (no PR-body parsing).
REFACTOR_EXEMPT_LABELS = {"refactor", "migration", "generated", "vendor"}


def _pr_diff_stats(pr_number: int, repo: str) -> dict:
    """Fetch PR file changes + sum to (files_changed, added_lines, deleted_lines).

    Returns an empty dict on API error — the caller treats missing data as
    'cannot compute' (which falls into the WARN tier, not BLOCK, to avoid
    false-positive blocks on transient API failures).
    """
    try:
        files = api_list(f"/repos/{repo}/pulls/{pr_number}/files", per_page=200)
    except GiteaError as e:
        return {"error": str(e)}

    files_changed = 0
    added = 0
    deleted = 0
    for f in files:
        # The Gitea PR-files API uses `additions` and `deletions` per file.
        # Some older versions use `additions`/`deletions`; normalize both.
        files_changed += 1
        added += int(f.get("additions", 0) or 0)
        deleted += int(f.get("deletions", 0) or 0)
    return {
        "files_changed": files_changed,
        "added_lines": added,
        "deleted_lines": deleted,
        "net_deleted_lines": max(0, deleted - added),
    }


def _label_appliers(pr_number: int, repo: str) -> dict[str, set[str]]:
    """Fetch the issue timeline and return a mapping from lowercase label
    name to the set of logins that applied that label.

    Fail-closed: if the timeline API is unreachable or returns unexpected
    data, returns an empty mapping so no label exemption can be proven.
    """
    owner, name = repo.split("/", 1)
    try:
        events = api_list(f"/repos/{owner}/{name}/issues/{pr_number}/timeline")
    except GiteaError:
        return {}
    appliers: dict[str, set[str]] = {}
    for event in events:
        if event.get("type") != "label":
            continue
        # Gitea encodes label ADD as body="1" and label REMOVE as body="".
        # Only ADD events count as applying the label; counting removals would
        # let a non-author who *removed* an exempt label enable an author who
        # re-added it — inverting the self-exemption guard (core#2884).
        if (event.get("body") or "") != "1":
            continue
        label = event.get("label") or {}
        label_name = (label.get("name") or "").lower()
        user = (event.get("user") or {}).get("login", "")
        if not label_name or not user:
            continue
        appliers.setdefault(label_name, set()).add(user)
    return appliers


def _pr_has_refactor_exemption(pr_data: dict, pr_number: int, repo: str) -> bool:
    """True iff the PR has a label in REFACTOR_EXEMPT_LABELS (e.g. 'refactor',
    'migration', 'generated', 'vendor') that opts it out of the destructive
    BLOCK, AND that label was applied by someone other than the PR author.

    Defense-in-depth against self-exemption (core#2884): a PR author with
    label-write permission cannot attach an exempt label to their own
    destructive diff and downgrade a BLOCK to WARN. The exemption is still
    LABEL-based (not PR-body-marker) because labels are the canonical signal
    already understood by the rest of the gate stack.

    Refactor-exempt PRs still get the WARN tier (not CLEAR) so operators
    can see the destructive diff size — they just don't get a BLOCK.
    """
    author = (pr_data.get("user") or {}).get("login", "")
    appliers = _label_appliers(pr_number, repo)
    for label in pr_data.get("labels", []) or []:
        name = (label.get("name") or "").lower()
        if name not in REFACTOR_EXEMPT_LABELS:
            continue
        # Require proof that a non-author applied this label. If we cannot
        # determine who applied it (timeline missing / API error), fail
        # closed and do not honor the exemption.
        label_appliers = appliers.get(name, set())
        if any(login != author for login in label_appliers):
            return True
    return False


def signal_7_destructive_diff_guard(
    pr_number: int, repo: str, pr_data: dict | None = None
) -> dict:
    """core#2875 — BLOCK a PR when its destructive diff + branch-diverged
    state match a high-confidence destructive pattern (a stale branch
    that would silently revert large swaths of current main).

    Computes:
      - diff stats (files_changed, deleted_lines, net_deleted_lines)
        from the PR-files API.
      - branch divergence (base.sha vs current target-branch HEAD) and
        commits_behind via signal_4's helper.
      - refactor exemption via PR labels applied by a non-author (core#2884
        defense-in-depth: author-self-applied exempt labels are ignored).

    Verdict:
      - BLOCK  when (files>=200 OR net_deleted>=5000 OR deleted>=10000)
                AND (diverged OR commits_behind>20)
                AND no refactor exemption.
      - WARN   when (files>=50 OR net_deleted>=1000 OR deleted>=2000)
                OR commits_behind>10.
                (WARN also surfaces on API errors so transient fetches don't
                silently pass.)
      - CLEAR  otherwise.
    """
    if pr_data is None:
        owner, name = repo.split("/", 1)
        pr_data = api_get(f"/repos/{owner}/{name}/pulls/{pr_number}")

    # Branch divergence: reuse the signal_4 helper's data
    # (no need to re-derive diverged + commits_behind here).
    sig4 = signal_4_branch_divergence(pr_number, repo, pr_data=pr_data)
    diverged = bool(sig4.get("diverged", False))
    commits_behind = int(sig4.get("commits_behind", 0) or 0)

    # Diff stats from the PR-files API.
    stats = _pr_diff_stats(pr_number, repo)
    if "error" in stats:
        # Cannot compute — surface as WARN (transient API failure
        # shouldn't silently pass a destructive PR, but also shouldn't
        # BLOCK on a missing-files transient).
        return {
            "signal": "destructive_diff_guard",
            "verdict": "WARNING",
            "reason": "could not fetch PR files API: " + stats["error"],
            "diverged": diverged,
            "commits_behind": commits_behind,
        }

    files_changed = stats["files_changed"]
    deleted_lines = stats["deleted_lines"]
    net_deleted = stats["net_deleted_lines"]
    has_refactor_exemption = _pr_has_refactor_exemption(pr_data, pr_number, repo)

    # High-confidence destructive condition:
    #   - any of the destructive diff thresholds
    #   - AND (branch is diverged OR has been sitting behind >20 commits)
    #   - AND the PR is not explicitly opted out via a refactor/migration label.
    destructive_diff = (
        files_changed >= 200
        or net_deleted >= 5000
        or deleted_lines >= 10000
    )
    is_stale = diverged or commits_behind > 20
    if destructive_diff and is_stale and not has_refactor_exemption:
        return {
            "signal": "destructive_diff_guard",
            "verdict": "BLOCKED",
            "diverged": diverged,
            "commits_behind": commits_behind,
            "files_changed": files_changed,
            "added_lines": stats["added_lines"],
            "deleted_lines": deleted_lines,
            "net_deleted_lines": net_deleted,
            "refactor_exemption": False,
            "reason": (
                f"destructive diff (files={files_changed}, net_deleted={net_deleted}, "
                f"deleted={deleted_lines}) + stale branch "
                f"(diverged={diverged}, commits_behind={commits_behind})"
            ),
        }

    # Moderate-drift WARN: any of the moderate thresholds
    # (operator should review but the PR isn't blocked).
    moderate = (
        files_changed >= 50
        or net_deleted >= 1000
        or deleted_lines >= 2000
        or commits_behind > 10
    )
    if moderate:
        return {
            "signal": "destructive_diff_guard",
            "verdict": "WARNING",
            "diverged": diverged,
            "commits_behind": commits_behind,
            "files_changed": files_changed,
            "added_lines": stats["added_lines"],
            "deleted_lines": deleted_lines,
            "net_deleted_lines": net_deleted,
            "refactor_exemption": has_refactor_exemption,
        }

    return {
        "signal": "destructive_diff_guard",
        "verdict": "CLEAR",
        "diverged": diverged,
        "commits_behind": commits_behind,
        "files_changed": files_changed,
        "added_lines": stats["added_lines"],
        "deleted_lines": deleted_lines,
        "net_deleted_lines": net_deleted,
        "refactor_exemption": has_refactor_exemption,
    }


def signal_6_ci(pr_number: int, repo: str, branch: str | None = None, pr_data: dict | None = None) -> dict:
    """
    Query combined CI status for PR head commit.
    Find required status checks on target branch.
    Surface any failing required check as primary blocker.
    """
    owner, name = repo.split("/", 1)

    # Re-use PR data if already fetched by caller; otherwise fetch once.
    if pr_data is None:
        pr_data = api_get(f"/repos/{owner}/{name}/pulls/{pr_number}")
    head_sha = pr_data["head"]["sha"]
    # Fall back to PR's actual base branch when no explicit branch is given
    branch = branch or pr_data.get("base", {}).get("ref", "main")

    # Combined status of PR head
    combined = api_get(f"/repos/{owner}/{name}/commits/{head_sha}/status")
    ci_state = combined.get("state", "null")

    # Individual check statuses
    # Gitea Actions uses "status" (pending/success/failure) not "state" for
    # individual check entries. "state" is null for pending runs.
    # Exclude our own prior status to prevent self-referential failure loops.
    # Gitea /commits/<sha>/statuses is non-monotonic by id, so collapse by
    # max(id) to guarantee the latest result for each context wins.
    latest_by_context: dict[str, dict] = {}
    for s in combined.get("statuses") or []:
        ctx = s["context"]
        if "gate-check" in ctx.lower():
            continue
        existing = latest_by_context.get(ctx)
        if existing is None or s.get("id", 0) > existing.get("id", 0):
            latest_by_context[ctx] = s
    check_statuses = {
        ctx: s.get("status", "pending") for ctx, s in latest_by_context.items()
    }

    # Try to get branch protection for required checks
    required_checks = []
    try:
        protection = api_get(f"/repos/{owner}/{name}/branches/{branch}/protection")
        for check in protection.get("required_status_checks", {}).get("checks", []):
            required_checks.append(check["context"])
    except GiteaError:
        pass  # No protection or no read access
    # Uniform gate: governance checks are ALWAYS required, even if branch
    # protection does not enumerate them. Deduplicate against BP list.
    required_checks = list(dict.fromkeys(required_checks + GOVERNANCE_REQUIRED_CONTEXTS))

    failing_required = []
    passing_required = []
    pending_required = []
    for ctx in required_checks:
        state = check_statuses.get(ctx, "null")
        if state == "failure":
            failing_required.append(ctx)
        elif state in ("success", "neutral"):
            passing_required.append(ctx)
        else:
            pending_required.append(ctx)

    # NOTE: do NOT use ci_state (combined_state) as a fallback verdict driver.
    # The combined_state is computed over ALL statuses including this
    # gate-check's own prior result. Using it as a fallback creates a
    # self-referential loop: gate-check posts failure → combined_state
    # becomes failure → script re-blocks → posts failure again.
    # The check_statuses dict already excludes gate-check (Bug-1 fix from
    # PR #547).
    #
    # Fail-closed: any required check that is missing, pending, or failing
    # blocks the gate. Only return CLEAR when every required check is
    # explicitly success/neutral.
    if failing_required:
        verdict = "CI_FAIL"
    elif pending_required:
        verdict = "CI_PENDING"
    else:
        verdict = "CLEAR"

    return {
        "signal": "ci_checks",
        "combined_state": ci_state,
        "required_checks": required_checks,
        "failing_required": failing_required,
        "passing_required": passing_required,
        "pending_required": pending_required,
        "all_check_statuses": check_statuses,
        "verdict": verdict,
    }


# ── Gate evaluation ───────────────────────────────────────────────────────────

VERDICT_ORDER = {"ERROR": 0, "CI_FAIL": 1, "BLOCKED": 2, "STALE-RC": 3, "CI_PENDING": 4, "N/A": 5, "WARNING": 6, "CLEAR": 7}


def compute_verdict(gates: list[dict]) -> tuple[str, list[dict]]:
    """Compute overall verdict from gate results. Worst gate wins."""
    worst = "CLEAR"
    blockers = []
    for g in gates:
        v = g.get("verdict", "N/A")
        if VERDICT_ORDER.get(v, 99) < VERDICT_ORDER.get(worst, 0):
            worst = v
        if v in ("BLOCKED", "CI_FAIL", "STALE-RC", "ERROR"):
            blockers.append(g)
    return worst, blockers


def format_gate_verdict(v: str) -> tuple[str, str]:
    """Return (icon, label) for a gate verdict."""
    if v in ("APPROVED", "CLEAR"):
        return "✅", v
    if v in ("BLOCKED", "CI_FAIL", "ERROR"):
        return "❌", v
    return "⚠️", v


def format_comment(repo: str, pr_number: int, verdict: str, gates: list[dict], blockers: list[dict]) -> str:
    """Format human-readable Gitea PR comment."""
    gate_labels = {
        "agent_tag_comments": "Agent-tag gates",
        "request_changes_reviews": "REQUEST_CHANGES reviews",
        "stale_reviews": "Staleness check",
        "branch_divergence": "Branch divergence / scope-creep guard",
        "ci_checks": "CI required checks",
    }

    lines = [f"[gate-check-v3] STATUS: **{verdict}**", ""]

    # Per-gate summary
    for g in gates:
        sig = g.get("signal", "?")
        label = gate_labels.get(sig, sig)
        v = g.get("verdict", "N/A")
        icon, _ = format_gate_verdict(v)
        lines.append(f"{icon} **{label}**: {v}")

    # Gate-specific detail
    if blockers:
        lines.append("")
        lines.append("### Blockers")
        for b in blockers:
            sig = b.get("signal", "?")
            if sig == "request_changes_reviews":
                for r in b.get("blocking_reviews", []):
                    lines.append(f"  - @{r['user']} requested changes (review id={r['review_id']})")
            elif sig == "ci_checks":
                combined = b.get("combined_state", "?")
                lines.append(f"  - CI combined state: **{combined}**")
                for c in b.get("failing_required", []):
                    lines.append(f"    - required check failing: **{c}**")
                for c in b.get("all_check_statuses", {}).items():
                    ctx, state = c
                    lines.append(f"    - {ctx}: {state}")
            elif sig == "stale_reviews":
                for r in b.get("stale_reviews", []):
                    lines.append(
                        f"  - @{r['user']} stale (commit={r.get('review_commit','?')[:7]}, age={r.get('age_hours','?')}h)"
                    )
            elif sig == "branch_divergence":
                if b.get("diverged"):
                    lines.append(
                        f"  - Branch is {b.get('commits_behind', '?')} commits behind target "
                        f"({b.get('target_head', '?')[:7]})"
                    )
                    frac = b.get("inherited_fraction", 0)
                    lines.append(
                        f"  - {frac * 100:.0f}% of PR files inherited from base divergence "
                        f"({len(b.get('inherited_files', []))}/{b.get('pr_files_count', 0)} files)"
                    )
                    for f in b.get("inherited_files", [])[:5]:
                        lines.append(f"    - inherited: `{f}`")
                    if len(b.get("inherited_files", [])) > 5:
                        lines.append(
                            f"    - ... and {len(b.get('inherited_files', [])) - 5} more"
                        )
                else:
                    lines.append("  - Branch is up to date with target")
            elif sig == "agent_tag_comments":
                for agent, res in b.get("results", {}).items():
                    v = res.get("verdict", "MISSING")
                    icon, _ = format_gate_verdict(v)
                    if v == "MISSING":
                        lines.append(f"  {icon} {agent}: no agent-tag comment found")
                    else:
                        lines.append(f"  {icon} {agent}: {v}")

    lines.append("")
    lines.append(f"_gate-check-v3 · repo={repo} · pr={pr_number}_")
    return "\n".join(lines)


# ── Main ─────────────────────────────────────────────────────────────────────

def run(repo: str, pr_number: int, post_comment: bool = False) -> dict:
    try:
        # Fetch PR once to get base ref for signal_6_ci
        owner, name = repo.split("/", 1)
        pr = api_get(f"/repos/{owner}/{name}/pulls/{pr_number}")
        base_ref = pr.get("base", {}).get("ref", "main")
        default_branch = os.environ.get("DEFAULT_BRANCH", "main")
        if base_ref != default_branch:
            result = {
                "verdict": "CLEAR",
                "repo": repo,
                "pr": pr_number,
                "skipped": True,
                "reason": (
                    f"PR targets {base_ref}, not protected default branch "
                    f"{default_branch}"
                ),
                "timestamp": datetime.now(timezone.utc).isoformat(),
            }
            print(json.dumps(result, indent=2))
            return result

        gates = [
            signal_1_comment_scan(pr_number, repo),
            signal_2_reviews(pr_number, repo),
            signal_3_staleness(pr_number, repo),
            signal_4_branch_divergence(pr_number, repo, pr_data=pr),
            signal_6_ci(pr_number, repo, branch=base_ref, pr_data=pr),
            signal_7_destructive_diff_guard(pr_number, repo, pr_data=pr),
        ]
        verdict, blockers = compute_verdict(gates)

        result = {
            "verdict": verdict,
            "repo": repo,
            "pr": pr_number,
            "gates": gates,
            "blockers": blockers,
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }

        # Print human-readable to stdout for Gitea Actions log
        print(json.dumps(result, indent=2))

        # Optionally post comment
        if post_comment:
            owner, name = repo.split("/", 1)
            comment_body = format_comment(repo, pr_number, verdict, gates, blockers)
            headers = {
                "Authorization": f"token {GITEA_TOKEN}",
                "Content-Type": "application/json",
                "Accept": "application/json",
            }
            # Check if a gate-check comment already exists to avoid spamming
            existing = api_list(f"/repos/{owner}/{name}/issues/{pr_number}/comments")
            our_comments = [c for c in existing if "[gate-check-v3]" in (c.get("body") or "")]
            try:
                if our_comments:
                    # Update latest
                    comment_id = our_comments[-1]["id"]
                    url = f"{API_BASE}/repos/{owner}/{name}/issues/comments/{comment_id}"
                    req = urllib.request.Request(url, data=json.dumps({"body": comment_body}).encode(), headers=headers, method="PATCH")
                    with urllib.request.urlopen(req, timeout=DEFAULT_TIMEOUT) as r:
                        r.read()
                else:
                    url = f"{API_BASE}/repos/{owner}/{name}/issues/{pr_number}/comments"
                    req = urllib.request.Request(url, data=json.dumps({"body": comment_body}).encode(), headers=headers, method="POST")
                    with urllib.request.urlopen(req, timeout=DEFAULT_TIMEOUT) as r:
                        r.read()
            except urllib.error.HTTPError as e:
                if e.code == 403:
                    print(f"WARN: --post-comment 403 (token scope) — verdict={verdict}; skipping comment-post", file=sys.stderr)
                else:
                    raise

        return result

    except GiteaError as e:
        result = {"verdict": "ERROR", "error": str(e), "repo": repo, "pr": pr_number}
        print(json.dumps(result, indent=2), file=sys.stderr)
        return result


def main() -> int:
    parser = argparse.ArgumentParser(description="gate-check-v3 — PR gate detector")
    parser.add_argument("--repo", required=True, help="org/repo (e.g. molecule-ai/molecule-core)")
    parser.add_argument("--pr", type=int, required=True, help="PR number")
    parser.add_argument("--post-comment", action="store_true", help="Post/update comment on PR")
    args = parser.parse_args()

    result = run(args.repo, args.pr, post_comment=args.post_comment)
    verdict = result.get("verdict", "ERROR")

    if verdict == "ERROR":
        return 2
    elif verdict in ("BLOCKED", "CI_FAIL", "STALE-RC", "ERROR"):
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
