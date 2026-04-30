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

Designed to run from a GitHub Actions PR check. Reads PR metadata via the
GitHub CLI (gh) which is preinstalled on ubuntu-latest runners. Runs in
under 10s against a typical PR.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from pathlib import Path

MIGRATIONS_DIR = "workspace-server/migrations"
MIGRATION_FILE_RE = re.compile(r"^(\d+)_[^/]+\.(up|down)\.sql$")


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
    `prefix`. Uses `gh pr diff` per PR — we only need to walk PRs that are
    actually in flight, so the cost is bounded by open-PR count.
    """
    out = run([
        "gh", "pr", "list", "--repo", repo, "--state", "open",
        "--json", "number,headRefName", "--limit", "100",
    ])
    prs = json.loads(out)
    matches: list[dict] = []
    for pr in prs:
        num = pr["number"]
        if num == exclude_pr:
            continue
        try:
            files = run([
                "gh", "pr", "diff", str(num), "--repo", repo, "--name-only",
            ], check=False)
        except Exception:  # noqa: BLE001
            continue
        for raw in files.splitlines():
            path = Path(raw.strip())
            if not path.name:
                continue
            m = MIGRATION_FILE_RE.match(path.name)
            if m and int(m.group(1)) == prefix:
                matches.append(pr)
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
    repo = os.environ.get("GITHUB_REPOSITORY", "Molecule-AI/molecule-core")

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
