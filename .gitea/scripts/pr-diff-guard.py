#!/usr/bin/env python3
"""PR diff-size / destructive-diff guard.

Implements core#2875: block stale branches whose head has drifted into a
massive destructive diff against current main (e.g., PR #1100: 481 files
changed, ~55k deletions). The guard runs on every PR and fails loudly when
any of the configured thresholds are exceeded.

The check compares the PR head against the merge base of the target branch,
so rebasing a stale branch to a clean, narrow diff will clear the guard.
"""

from __future__ import annotations

import os
import subprocess
import sys


PROTECTED_PATHS = (
    ".gitea/workflows/",
    ".gitea/scripts/",
    "tests/e2e/",
    "workspace-server/internal/handlers/",
    "workspace-server/internal/provisioner/",
    "workspace-server/internal/middleware/",
    "canvas/src/",
)

DEFAULT_MAX_CHANGED_FILES = int(os.environ.get("DIFFGUARD_MAX_CHANGED_FILES", "100"))
DEFAULT_MAX_DELETIONS = int(os.environ.get("DIFFGUARD_MAX_DELETIONS", "5000"))
DEFAULT_MAX_INSERTIONS = int(os.environ.get("DIFFGUARD_MAX_INSERTIONS", "10000"))


def git(*args: str) -> str:
    result = subprocess.run(
        ["git", *args],
        capture_output=True,
        text=True,
        check=True,
    )
    return result.stdout


def main() -> int:
    base_ref = os.environ.get("PR_BASE_REF", os.environ.get("GITHUB_BASE_REF", "main"))
    head_sha = os.environ.get("PR_HEAD_SHA", os.environ.get("GITHUB_SHA", ""))

    if not head_sha:
        # In a pull_request workflow, GITHUB_SHA is the merge commit. Use the
        # PR head ref instead when available.
        head_sha = os.environ.get("GITHUB_EVENT_PULL_REQUEST_HEAD_SHA", "HEAD")
    if not head_sha:
        head_sha = "HEAD"

    # Ensure base ref is available.
    try:
        git("rev-parse", f"origin/{base_ref}")
    except subprocess.CalledProcessError:
        git("fetch", "origin", base_ref)

    # Find merge base so the diff reflects only what the PR added, not main
    # drift since the branch was created. If no merge base exists (e.g.,
    # unrelated-history branch), fall back to the base ref itself — the guard
    # should still catch a massive destructive diff.
    try:
        merge_base = git("merge-base", f"origin/{base_ref}", head_sha).strip()
    except subprocess.CalledProcessError:
        print(f"::warning::no merge base with origin/{base_ref}; falling back to direct diff")
        merge_base = f"origin/{base_ref}"

    # Diff stat.
    numstat = git("diff", "--numstat", f"{merge_base}..{head_sha}").strip()
    changed_files = 0
    insertions = 0
    deletions = 0
    for line in numstat.splitlines():
        parts = line.split()
        if len(parts) < 3:
            continue
        add, rem = parts[0], parts[1]
        if add == "-" or rem == "-":
            continue  # binary
        insertions += int(add)
        deletions += int(rem)
        changed_files += 1

    # Deleted files and protected-path deletions.
    name_status = git("diff", "--name-status", f"{merge_base}..{head_sha}").strip()
    deleted_files: list[str] = []
    protected_deletions: list[str] = []
    for line in name_status.splitlines():
        if not line:
            continue
        status, path = line.split("\t", 1)
        if status.startswith("D"):
            deleted_files.append(path)
            if any(path.startswith(p) for p in PROTECTED_PATHS):
                protected_deletions.append(path)

    # Evaluate thresholds.
    failures: list[str] = []
    if changed_files > DEFAULT_MAX_CHANGED_FILES:
        failures.append(
            f"changed files ({changed_files}) exceeds threshold ({DEFAULT_MAX_CHANGED_FILES})"
        )
    if insertions > DEFAULT_MAX_INSERTIONS:
        failures.append(
            f"insertions (+{insertions}) exceeds threshold ({DEFAULT_MAX_INSERTIONS})"
        )
    if deletions > DEFAULT_MAX_DELETIONS:
        failures.append(
            f"deletions (-{deletions}) exceeds threshold ({DEFAULT_MAX_DELETIONS})"
        )
    if protected_deletions:
        failures.append(
            f"deleted {len(protected_deletions)} protected path(s): "
            + ", ".join(protected_deletions[:10])
        )

    # Report.
    print(f"Diff guard: {changed_files} files changed, +{insertions}/-{deletions} lines")
    print(f"Deleted files: {len(deleted_files)}")
    if protected_deletions:
        print(f"Protected-path deletions: {len(protected_deletions)}")

    if failures:
        print("::error::PR diff guard failed:")
        for f in failures:
            print(f"  - {f}")
        print(
            "If this diff is intentional, split the PR or request a threshold override from the PM."
        )
        return 1

    print("Diff guard passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
