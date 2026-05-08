#!/usr/bin/env python3
"""check_migration_collisions.py — fail-loud detector for two open PRs adding
the same migration version number.

Why this exists: two PRs targeting staging can each add a migration with the
same numeric prefix (e.g. 044_*.up.sql). Each passes CI independently. They
collide at merge time. Worst-case the second migration silently doesn't apply
and the schema drifts from what the code expects. Caught manually 2026-04-30
during PR #2276 rebase: 044_runtime_image_pins collided with
044_platform_inbound_secret from RFC #2312.

This check runs on every PR and asserts the migration prefixes added by THIS
PR don't collide with:

    1. The base branch's tip (someone else already used this number)
    2. Any other open PR (race-window collision — both pass CI independently)

Exit codes:
    0  — no collisions
    1  — collision detected; output names the conflicting PR(s) for the author

Designed to run from a Gitea Actions PR check. Reads PR metadata via direct
HTTP calls to Gitea's REST API (`/api/v1/`), which on the molecule-ai fleet
lives at https://git.moleculesai.app. Runs in under 10s against a typical PR.

Post-2026-05-06 (Gitea migration, issue #75): the previous version called
the GitHub CLI (``gh pr list``, ``gh pr diff``). On Gitea those calls hit
either the GraphQL endpoint (HTTP 405) or /api/v3 (HTTP 404). This module
now talks to /api/v1 directly via urllib so it works against any Gitea
host without a `gh` install or extra dependencies.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

MIGRATIONS_DIR = "workspace-server/migrations"
MIGRATION_FILE_RE = re.compile(r"^(\d+)_[^/]+\.(up|down)\.sql$")


def _gitea_api_url() -> str:
    """Resolve the Gitea API base URL.

    act_runner forwards github.server_url as GITHUB_SERVER_URL; for the
    molecule-ai fleet that's https://git.moleculesai.app. Append /api/v1
    to get the REST root. Override directly via GITEA_API_URL for tests
    or non-default hosts.
    """
    env_override = os.environ.get("GITEA_API_URL", "").rstrip("/")
    if env_override:
        return env_override
    server = os.environ.get("GITHUB_SERVER_URL", "https://git.moleculesai.app").rstrip("/")
    return f"{server}/api/v1"


def _gitea_token() -> str:
    """Resolve the Gitea token from env. GITEA_TOKEN wins; falls back
    to GITHUB_TOKEN (set by act_runner) and GH_TOKEN (operator habit
    from the GitHub era)."""
    return (
        os.environ.get("GITEA_TOKEN")
        or os.environ.get("GITHUB_TOKEN")
        or os.environ.get("GH_TOKEN")
        or ""
    )


def _gitea_get(path: str, params: dict[str, str] | None = None) -> bytes | None:
    """GET against /api/v1; returns response body or None on HTTP error.

    Errors return None (not raise) because callers handle missing data
    by emitting an actionable workflow message rather than crashing the
    PR check on a transient API blip.
    """
    base = _gitea_api_url()
    qs = ""
    if params:
        qs = "?" + urllib.parse.urlencode(params)
    url = f"{base}/{path.lstrip('/')}{qs}"
    req = urllib.request.Request(url)
    token = _gitea_token()
    if token:
        req.add_header("Authorization", f"token {token}")
    req.add_header("Accept", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:  # noqa: S310
            return resp.read()
    except urllib.error.HTTPError as e:
        sys.stderr.write(f"Gitea API HTTP {e.code} on {path}: {e.reason}\n")
        return None
    except (urllib.error.URLError, TimeoutError) as e:
        sys.stderr.write(f"Gitea API network error on {path}: {e}\n")
        return None


def run(cmd: list[str], check: bool = True) -> str:
    """Run a subprocess and return stdout. Raise on non-zero when check=True."""
    result = subprocess.run(cmd, capture_output=True, text=True)
    if check and result.returncode != 0:
        sys.stderr.write(f"command failed: {' '.join(cmd)}\n{result.stderr}\n")
        sys.exit(1)
    return result.stdout


def migrations_in_diff(base_ref: str, head_ref: str) -> set[int]:
    """Return the set of migration prefixes added or modified between two refs.

    Uses --diff-filter=AM (Added or Modified) so a deleted migration doesn't
    count. Renames (--diff-filter=R) appear as A on the new path and D on the
    old, so we'd catch a renumbering correctly.
    """
    out = run([
        "git", "diff", "--name-only", "--diff-filter=AM",
        f"{base_ref}...{head_ref}", "--", MIGRATIONS_DIR,
    ])
    prefixes: set[int] = set()
    for line in out.splitlines():
        path = Path(line.strip())
        if not path.name:
            continue
        m = MIGRATION_FILE_RE.match(path.name)
        if not m:
            # Files like the workflow_checkpoints.up.sql with non-numeric
            # prefix are intentional — skip without complaint.
            continue
        prefixes.add(int(m.group(1)))
    return prefixes


def migrations_on_ref(ref: str) -> set[int]:
    """Return the set of numeric migration prefixes existing at the given git ref.

    Walks the migrations dir at that ref via `git ls-tree`, not the working
    tree, so it works against any branch / SHA without checking it out.
    """
    out = run([
        "git", "ls-tree", "-r", "--name-only", ref, "--", MIGRATIONS_DIR,
    ])
    prefixes: set[int] = set()
    for line in out.splitlines():
        path = Path(line.strip())
        if not path.name:
            continue
        m = MIGRATION_FILE_RE.match(path.name)
        if not m:
            continue
        prefixes.add(int(m.group(1)))
    return prefixes


def open_prs_with_migration_prefix(
    repo: str, prefix: int, exclude_pr: int
) -> list[dict]:
    """Return open PRs (other than `exclude_pr`) that add a migration with
    `prefix`. Walks open PRs via Gitea's `/repos/{owner}/{repo}/pulls` and
    pulls each one's changed-file list via `/pulls/{n}/files`. The cost is
    bounded by open-PR count, which is small (<100) on this repo. The
    return shape mimics the GitHub CLI's `--json number,headRefName`:
    ``[{"number": int, "headRefName": str}, ...]``.
    """
    body = _gitea_get(
        f"repos/{repo}/pulls",
        {"state": "open", "limit": "50"},
    )
    if body is None:
        # Best-effort: a transient Gitea blip shouldn't fail the PR
        # check (the base-branch collision check runs locally and is
        # the more common failure mode).
        return []
    prs = json.loads(body)
    matches: list[dict] = []
    for pr in prs:
        num = pr["number"]
        if num == exclude_pr:
            continue
        # Gitea returns the head ref under .head.ref (REST shape);
        # GitHub CLI's --json headRefName flattens it. Normalize on
        # the way out so callers see the historical shape.
        head_ref_name = (pr.get("head") or {}).get("ref", "")
        files_body = _gitea_get(f"repos/{repo}/pulls/{num}/files", {"limit": "100"})
        if files_body is None:
            continue
        try:
            files = json.loads(files_body)
        except json.JSONDecodeError:
            continue
        for f in files:
            # Gitea's /pulls/{n}/files returns objects with `.filename`
            # (same as GitHub's REST). Older Gitea versions emit
            # `.name` instead — handle both.
            raw = f.get("filename") or f.get("name") or ""
            path = Path(raw.strip())
            if not path.name:
                continue
            m = MIGRATION_FILE_RE.match(path.name)
            if m and int(m.group(1)) == prefix:
                matches.append({"number": num, "headRefName": head_ref_name})
                break
    return matches


def main() -> int:
    pr_number_env = os.environ.get("PR_NUMBER", "").strip()
    if not pr_number_env:
        sys.stderr.write(
            "PR_NUMBER not set — this script is intended to run from a PR "
            "context. Set PR_NUMBER (e.g. ${{ github.event.pull_request.number }}) "
            "and BASE_REF (target branch) and HEAD_REF (PR head SHA).\n"
        )
        return 1
    pr_number = int(pr_number_env)
    base_ref = os.environ.get("BASE_REF", "origin/staging")
    head_ref = os.environ.get("HEAD_REF", "HEAD")
    # Default kept lowercase to match the Gitea-canonical org name
    # (post-2026-05-06 migration). Tests + workflow context override
    # via GITHUB_REPOSITORY which act_runner sets per-run.
    repo = os.environ.get("GITHUB_REPOSITORY", "molecule-ai/molecule-core")

    added = migrations_in_diff(base_ref, head_ref)
    if not added:
        print("no migrations added or modified by this PR — nothing to check")
        return 0

    print(f"this PR adds/modifies migrations: {sorted(added)}")

    # Collision check 1: base branch already has this prefix on a different
    # filename. This happens when the PR was branched off an old base and
    # didn't rebase — base advanced and another PR landed the same number.
    base_prefixes = migrations_on_ref(base_ref)
    base_collisions = added & base_prefixes
    # Filter to "different filename, same prefix" — same filename means the
    # PR is updating an existing migration in place, which is fine.
    real_base_collisions: set[int] = set()
    for prefix in base_collisions:
        # List filenames at base for this prefix
        out = run([
            "git", "ls-tree", "-r", "--name-only", base_ref, "--",
            MIGRATIONS_DIR,
        ])
        base_names = {
            Path(line).name for line in out.splitlines()
            if (m := MIGRATION_FILE_RE.match(Path(line).name)) and int(m.group(1)) == prefix
        }
        # And in the PR
        diff_out = run([
            "git", "diff", "--name-only", "--diff-filter=AM",
            f"{base_ref}...{head_ref}", "--", MIGRATIONS_DIR,
        ])
        pr_names = {
            Path(line).name for line in diff_out.splitlines()
            if (m := MIGRATION_FILE_RE.match(Path(line).name)) and int(m.group(1)) == prefix
        }
        if pr_names - base_names:
            real_base_collisions.add(prefix)

    # Collision check 2: another open PR claims the same prefix.
    open_pr_collisions: dict[int, list[dict]] = {}
    for prefix in added:
        peers = open_prs_with_migration_prefix(repo, prefix, pr_number)
        if peers:
            open_pr_collisions[prefix] = peers

    if not real_base_collisions and not open_pr_collisions:
        print("no migration version collisions detected")
        return 0

    print()
    print("::error::migration version collision detected")
    if real_base_collisions:
        print(f"::error::these prefixes already exist on {base_ref} with different filenames: "
              f"{sorted(real_base_collisions)}")
        print("::error::rebase onto current base and renumber to the next available prefix")
    for prefix, peers in sorted(open_pr_collisions.items()):
        peer_str = ", ".join(f"#{p['number']} ({p['headRefName']})" for p in peers)
        print(f"::error::migration prefix {prefix:03d} also claimed by open PR(s): {peer_str}")
        print(f"::error::rebase coordination needed — only one PR can land a given prefix; "
              f"renumber yours or theirs")
    return 1


if __name__ == "__main__":
    sys.exit(main())
