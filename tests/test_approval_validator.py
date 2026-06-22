"""Tests for `.gitea/scripts/_approval_validator.py`.

Locks the fail-closed review contract used by both the merge queue and
review-check.sh. The regression in molecule-core#3066 showed that a
dismissed/superseded REQUEST_CHANGES review could block an otherwise-ready
PR when the reducer did not (a) take the latest review per user and (b)
explicitly drop `dismissed` / `official=false` rows.

Run:
    python3 -m pytest tests/test_approval_validator.py -v
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

# Import the private validator module directly; it has no env-var side effects
# at import time.
SCRIPT_DIR = Path(__file__).resolve().parent.parent / ".gitea" / "scripts"
sys.path.insert(0, str(SCRIPT_DIR))

from _approval_validator import (  # noqa: E402
    classify_reviews,
    is_genuine_approval,
    is_open_request_changes,
)

HEAD = "48eddb08" * 5  # 40-char placeholder sha
OLD_HEAD = "11111111" * 5


def _review(
    *,
    user: str,
    state: str,
    commit_id: str = HEAD,
    official: bool = True,
    dismissed: bool = False,
    stale: bool = False,
) -> dict:
    return {
        "id": 1,
        "user": {"login": user},
        "state": state,
        "commit_id": commit_id,
        "official": official,
        "dismissed": dismissed,
        "stale": stale,
    }


def test_classify_reviews_takes_latest_per_user() -> None:
    reviews = [
        _review(user="agent-researcher", state="REQUEST_CHANGES"),
        _review(user="agent-researcher", state="APPROVED"),
        _review(user="agent-reviewer-cr2", state="APPROVED"),
    ]
    approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
    assert approvers == {"agent-researcher", "agent-reviewer-cr2"}
    assert request_changes == []


def test_classify_reviews_excludes_dismissed_request_changes() -> None:
    reviews = [
        _review(
            user="agent-researcher",
            state="REQUEST_CHANGES",
            dismissed=True,
            official=False,
        ),
        _review(user="agent-researcher", state="APPROVED"),
        _review(user="agent-reviewer-cr2", state="APPROVED"),
    ]
    approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
    assert approvers == {"agent-researcher", "agent-reviewer-cr2"}
    assert request_changes == []


def test_classify_reviews_excludes_stale_head() -> None:
    reviews = [
        _review(user="agent-researcher", state="REQUEST_CHANGES", commit_id=OLD_HEAD),
        _review(user="agent-researcher", state="APPROVED"),
    ]
    approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
    assert approvers == {"agent-researcher"}
    assert request_changes == []


def test_classify_reviews_excludes_official_false() -> None:
    reviews = [
        _review(user="agent-researcher", state="APPROVED", official=False),
        _review(user="agent-reviewer-cr2", state="APPROVED"),
    ]
    approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
    assert approvers == {"agent-reviewer-cr2"}
    assert request_changes == []


def test_classify_reviews_request_changes_supersedes_earlier_approval() -> None:
    reviews = [
        _review(user="agent-researcher", state="APPROVED"),
        _review(user="agent-researcher", state="REQUEST_CHANGES"),
    ]
    approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
    assert approvers == set()
    assert request_changes == ["agent-researcher"]


def test_classify_reviews_respects_reviewer_set() -> None:
    reviews = [
        _review(user="agent-researcher", state="APPROVED"),
        _review(user="unrecognised-human", state="APPROVED"),
    ]
    approvers, request_changes = classify_reviews(
        reviews,
        headsha=HEAD,
        reviewer_set={"agent-researcher"},
    )
    assert approvers == {"agent-researcher"}
    assert request_changes == []


def test_is_genuine_approval_fail_closed_on_dismissed() -> None:
    review = _review(user="u", state="APPROVED", dismissed=True)
    assert is_genuine_approval(review, headsha=HEAD) is False


def test_is_open_request_changes_fail_closed_on_official_false() -> None:
    review = _review(user="u", state="REQUEST_CHANGES", official=False)
    assert is_open_request_changes(review, headsha=HEAD) is False


def test_classify_reviews_empty_input() -> None:
    assert classify_reviews([], headsha=HEAD) == (set(), [])
