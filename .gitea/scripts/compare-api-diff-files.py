#!/usr/bin/env python3
"""Extract changed-file list from Gitea Compare API JSON response.

Gitea Compare API returns changed files nested inside commits, not at the
top level:
    {"commits": [{"files": [{"filename": "path/to/file"}]}]}

Usage:
    compare-api-diff-files.py < API_RESPONSE.json

Exits 0 with filenames on stdout, one per line.
Exits 1 on malformed input (caller should handle as "no files").
"""
from __future__ import annotations

import sys
import json


def main() -> None:
    try:
        data = json.load(sys.stdin)
    except Exception:
        sys.exit(1)

    filenames: list[str] = []
    for commit in data.get("commits", []):
        for f in commit.get("files", []):
            fn = f.get("filename", "")
            if fn:
                filenames.append(fn)

    if filenames:
        sys.stdout.write("\n".join(filenames))
        sys.stdout.write("\n")
    # else: empty stdout = no files, caller treats as empty list


if __name__ == "__main__":
    main()
