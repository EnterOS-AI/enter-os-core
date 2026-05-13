#!/usr/bin/env python3
"""Extract changed-file list from a Gitea push event's commits JSON array.

Each commit in a push event has `added`, `removed`, and `modified` file lists.
This script aggregates all of them and prints unique filenames one per line.

Usage:
    push-commits-diff-files.py < COMMITS_JSON

Exits 0 always (caller handles empty output as "no files").
"""
from __future__ import annotations

import sys
import json


def main() -> None:
    try:
        data = json.load(sys.stdin)
    except Exception:
        sys.exit(0)  # Don't fail the step — treat malformed JSON as empty

    if not isinstance(data, list):
        sys.exit(0)

    files: set[str] = set()
    for commit in data:
        if not isinstance(commit, dict):
            continue
        for key in ("added", "removed", "modified"):
            for f in commit.get(key) or []:
                if isinstance(f, str) and f:
                    files.add(f)

    if files:
        sys.stdout.write("\n".join(sorted(files)))
        sys.stdout.write("\n")


if __name__ == "__main__":
    main()
