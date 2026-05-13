#!/usr/bin/env python3
"""
gate-check-v3 — SOP-6 + CI gate detector for Gitea PRs.

Emits structured verdict + human-readable summary. Designed to run as:
  1. CLI:  python gate_check.py --repo org/repo --pr N
  2. Gitea Actions step: runs this script, captures stdout JSON

Signals (MVP — signals 1,2,3,6):
  1. Author-aware agent-tag comment scan
  2. REQUEST_CHANGES reviews state machine
  3. Staleness detection (review.commit_id != PR.head_sha)
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
import time
import urllib.request
import urllib.error
from datetime import datetime, timezone
from typing import Any, Optional

# ── Gitea API client ────────────────────────────────────────────────────────

GITEA_HOST = os.environ.get("GITEA_HOST", "git.moleculesai.app")
GITEA_TOKEN = os.environ.get("GITEA_TOKEN", os.environ.get("GITHUB_TOKEN", ""))
API_BASE = f"https://{GITEA_HOST}/api/v1"

# Timeout in seconds for all HTTP calls. Defence-in-depth: ensures a missing or
# invalid SOP_TIER_CHECK_TOKEN causes a fast (~15 s) failure rather than an
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

# SOP-6 tier → required agent groups
# tier:low    → engineers,managers,ceo (OR: any one suffices)
# tier:medium → managers AND engineers AND qa,security (AND)
# tier:high   → ceo (OR, but single)
# "?" = teams not yet created; treated as optional for MVP
TIER_AGENTS = {
    "tier:low":    {"managers": "core-lead", "engineers": "core-devops", "ceo": "ceo"},
    "tier:medium": {"managers": "core-lead", "engineers": "core-devops", "qa": "core-qa", "security": "core-security"},
    "tier:high":   {"ceo": "ceo"},
}

POSITIVE_VERDICTS = {"APPROVED", "N/A", "ACK"}


def _get_pr_tier(pr_number: int, repo: str) -> str:
    """Get the PR's tier label."""
    owner, name = repo.split("/", 1)
    try:
        pr = api_get(f"/repos/{owner}/{name}/pulls/{pr_number}")
        for label in pr.get("labels", []):
            name_l = label.get("name", "")
            if name_l in TIER_AGENTS:
                return name_l
    except GiteaError:
        pass
    return "tier:low"  # Default for untagged PRs


def signal_1_comment_scan(pr_number: int, repo: str) -> dict:
    """
    Scan issue + PR comments AND reviews for agent-tag policy gates.
    Matches tag AND author. Filters to tier-relevant agents.
    Returns: {signal, results, verdict}
    """
    owner, name = repo.split("/", 1)

    # Get tier label to determine relevant agents
    tier = _get_pr_tier(pr_number, repo)
    relevant_roles = TIER_AGENTS.get(tier, TIER_AGENTS["tier:low"])

    # Build reverse map: login -> (group, agent_key)
    login_to_group = {}
    for group, login in relevant_roles.items():
        for role, l in AGENT_LOGIN_MAP.items():
            if l == login:
                login_to_group[l] = (group, f"core-{role}")

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

    # Collect APPROVED reviews from agent logins
    try:
        reviews = api_list(f"/repos/{owner}/{name}/pulls/{pr_number}/reviews")
        for r in reviews:
            login = r.get("user", {}).get("login", "")
            if login in login_to_group and r.get("state") == "APPROVED":
                comments.append(
                    {
                        "id": f"review-{r['id']}",
                        "user": {"login": login},
                        "body": f"[{login}-agent] APPROVED",
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
            user_login = c.get("user", {}).get("login", "")
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
            "tier": tier,
            "found": latest,
            "verdict": latest["verdict"] if latest else "MISSING",
        }

    # Compute gate verdict using tier-specific logic:
    # - tier:low / tier:high (OR gate): ANY positive = CLEAR, ANY negative = BLOCKED
    # - tier:medium (AND gate): ALL must be positive = CLEAR, ANY negative = BLOCKED
    verdicts = [f["verdict"] for f in findings.values()]
    if not verdicts:
        gate_verdict = "N/A"
    elif tier in ("tier:low", "tier:high"):
        # OR gate: one positive is enough
        if any(v in POSITIVE_VERDICTS for v in verdicts):
            gate_verdict = "CLEAR"
        elif any(v in ("BLOCKED", "CHANGES_REQUESTED", "COMMENT") for v in verdicts):
            gate_verdict = "BLOCKED"
        else:
            gate_verdict = "INCOMPLETE"
    else:
        # AND gate (tier:medium): all must be positive
        if all(v in POSITIVE_VERDICTS for v in verdicts):
            gate_verdict = "CLEAR"
        elif any(v in ("BLOCKED", "CHANGES_REQUESTED", "COMMENT") for v in verdicts):
            gate_verdict = "BLOCKED"
        else:
            gate_verdict = "INCOMPLETE"

    return {"signal": "agent_tag_comments", "results": findings, "verdict": gate_verdict, "tier": tier}


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
        if r.get("state") == "REQUEST_CHANGES" and not r.get("dismissed", False):
            blocking.append(
                {
                    "review_id": r["id"],
                    "user": r["user"]["login"],
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


# ── Signal 6: CI required-checks awareness ───────────────────────────────────

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
    check_statuses = {}
    for s in combined.get("statuses") or []:
        ctx = s["context"]
        if "gate-check" not in ctx.lower():
            check_statuses[ctx] = s.get("status", "pending")

    # Try to get branch protection for required checks
    required_checks = []
    try:
        protection = api_get(f"/repos/{owner}/{name}/branches/{branch}/protection")
        for check in protection.get("required_status_checks", {}).get("checks", []):
            required_checks.append(check["context"])
    except GiteaError:
        pass  # No protection or no read access

    failing_required = []
    passing_required = []
    for ctx in required_checks:
        state = check_statuses.get(ctx, "null")
        if state == "failure":
            failing_required.append(ctx)
        elif state in ("success", "neutral"):
            passing_required.append(ctx)
        else:
            passing_required.append(f"{ctx} (pending)")

    # NOTE: do NOT use ci_state (combined_state) as a fallback verdict driver.
    # The combined_state is computed over ALL statuses including this
    # gate-check's own prior result. Using it as a fallback creates a
    # self-referential loop: gate-check posts failure → combined_state
    # becomes failure → script re-blocks → posts failure again.
    # The check_statuses dict already excludes gate-check (Bug-1 fix from
    # PR #547). Use failing_required as the sole CI gate; if no required
    # checks are defined on the branch, return CLEAR rather than re-using
    # the combined_state which includes our own status.
    if failing_required:
        verdict = "CI_FAIL"
    elif ci_state == "pending":
        verdict = "CI_PENDING"
    else:
        verdict = "CLEAR"

    return {
        "signal": "ci_checks",
        "combined_state": ci_state,
        "required_checks": required_checks,
        "failing_required": failing_required,
        "passing_required": passing_required,
        "all_check_statuses": check_statuses,
        "verdict": verdict,
    }


# ── Gate evaluation ───────────────────────────────────────────────────────────

VERDICT_ORDER = {"ERROR": 0, "CI_FAIL": 1, "BLOCKED": 2, "STALE-RC": 3, "CI_PENDING": 4, "N/A": 5, "CLEAR": 6}


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
            signal_6_ci(pr_number, repo, branch=base_ref, pr_data=pr),
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
