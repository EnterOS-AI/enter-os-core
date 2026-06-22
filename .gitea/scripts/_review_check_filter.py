#!/usr/bin/env python3
"""
Helper for review-check.sh: applies the SSOT approval predicate to a
PR's reviews and prints the candidate approver logins on stdout (one per
line, de-duplicated, author excluded).

review-check.sh uses this in place of its previous inline jq filter so the
predicate is single-sourced. The jq filter is gone; if you want to change
the predicate, edit .gitea/scripts/_approval_validator.py, not this file.

Usage:
  python3 _review_check_filter.py <reviews.json> <head-sha> <author-login>

Output:
  - Candidate approver logins, one per line, de-duplicated, sorted.
  - Excludes `author-login` (the PR author cannot approve their own PR).
  - Empty output → review-check.sh interprets as "no candidates" and exits 1
    after the team-membership probe.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

# Same-dir import — script lives next to _approval_validator.py
sys.path.insert(0, str(Path(__file__).resolve().parent))
from _approval_validator import is_genuine_approval  # noqa: E402


def main(argv: list[str]) -> int:
    if len(argv) != 4:
        print(
            f"usage: {argv[0] if argv else '_review_check_filter.py'} "
            "<reviews.json> <head-sha> <author-login>",
            file=sys.stderr,
        )
        return 2
    reviews_path = Path(argv[1])
    headsha = argv[2]
    author = argv[3]

    try:
        reviews = json.loads(reviews_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        print(f"::error::could not read reviews JSON: {exc}", file=sys.stderr)
        return 2
    if not isinstance(reviews, list):
        print("::error::reviews JSON was not a list", file=sys.stderr)
        return 2

    candidates: set[str] = set()
    for review in reviews:
        # We pass reviewer_set=None here because review-check.sh applies its
        # own team-membership probe (CURL_AUTH_FILE + 200/204/403/404 logic)
        # separately. The SSOT predicate enforces only the fail-closed
        # commit_id / state / official / dismissed / stale contract here.
        if not is_genuine_approval(review, headsha=headsha, reviewer_set=None):
            continue
        user = (review.get("user") or {}).get("login")
        if not isinstance(user, str) or not user:
            continue
        if user == author:
            continue
        candidates.add(user)

    for user in sorted(candidates):
        print(user)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
