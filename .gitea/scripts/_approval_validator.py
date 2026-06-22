#!/usr/bin/env python3
"""
SSOT fail-closed approval validator (SEV-1 internal#812).

This module is the SINGLE source of truth for whether a Gitea review counts
as a "genuine" approval. Both consumers must call into it — they MUST NOT
duplicate the predicate:

  - .gitea/scripts/gitea-merge-queue.py (Python) — imports directly.
  - .gitea/scripts/review-check.sh (bash, jq) — calls the Python helper
    at .gitea/scripts/_review_check_filter.py, which in turn calls this
    module. There is no separate jq / bash copy of the predicate; a
    reviewer who wants to weaken the gate has to weaken this one file.

# The fail-closed contract

A review counts as a GENUINE APPROVED on the current head ONLY IF ALL hold:

  1. state == "APPROVED"
  2. official == true
  3. dismissed != true
  4. stale != true
  5. commit_id is present and equals the PR's current head SHA

ANY failure of any of the above → REJECT.

# The bug this fixes

The previous gitea-merge-queue.py predicate had a `if isinstance(commit_id,
str) and commit_id and headsha:` guard that *skipped* the commit_id check
when the review carried no commit_id. The previous review-check.sh jq
filter required `commit_id == $head`, which is also implicitly fail-closed
on missing commit_id (null != head), but only one of the two consumers
behaved correctly — a code-drift trap.

Both behaviors are now defined here, as a single fail-closed predicate.
A MISSING commit_id is the Gitea row signature of a spoofed or pre-commit
review: a real reviewer cannot have submitted against a commit that
doesn't exist. Accepting these is exactly the fail-open that SEV-1
internal#812 describes and the re-opened path that closed #843 (with CR2
+ Researcher both flagging it) addresses.

# Mutation-resistance

The unit tests in tests/test_approval_validator.py assert rejection
explicitly for each fail-closed case (missing commit_id, stale head,
non-official, dismissed, etc.). A reviewer who tries to weaken the
predicate by removing the commit_id check, by re-introducing the
"no commit_id is accepted" escape hatch, or by changing `!=` to `==`
in the head comparison will trip those tests in CI.
"""

from __future__ import annotations

from typing import Iterable, Optional, Tuple

# ---------------------------------------------------------------------------
# Canonical Gitea review-state enum (EXACT match -- no case coercion).
# ---------------------------------------------------------------------------
#
# Gitea's reviews API emits review.state as one of a fixed set of
# UPPERCASE string constants: "APPROVED", "REQUEST_CHANGES",
# "REQUEST_REVIEW", "COMMENT", "PENDING", "DISMISSED" (verified
# against the live API across real molecule-core PRs). They are ALWAYS
# uppercase on the wire.
#
# FAIL-CLOSED: we compare review.state to these constants with EXACT
# equality. The previous code used str(state or "").upper(), which
# coerced a lowercase/mixed-case "approved" or "request_changes" into
# the canonical value and ACCEPTED it. A real Gitea row never carries a
# lowercase state, so a case-variant value is the signature of a
# hand-forged / spoofed row, not a legitimate review. Coercing it was a
# residual fail-open (SEV-1 internal#812, RCs 9849/9851/9852). We reject
# anything that is not byte-for-byte the canonical constant.
STATE_APPROVED = "APPROVED"
STATE_REQUEST_CHANGES = "REQUEST_CHANGES"


# ---------------------------------------------------------------------------
# Shared predicate — fail-closed on every condition
# ---------------------------------------------------------------------------


def is_official_current_head(review: object, headsha: object) -> bool:
    """Common predicate: review is official, not dismissed, not stale, and
    bound to the PR's current head SHA. EVERY condition is mandatory and
    fail-closed. Both is_genuine_approval and is_open_request_changes build
    on this so the rule cannot drift between the two cases.

    `official` is checked with `is not True` (NOT `not review.get("official")`).
    The latter is truthy on the string "false" or the integer 1, which is
    exactly the fail-open surface we are closing here — a non-boolean
    pass-through is treated as official. Gitea emits a real boolean, so
    the stricter check rejects anything that isn't literally True.
    """
    if not isinstance(review, dict):
        return False
    if review.get("official") is not True:
        return False
    if review.get("dismissed"):
        return False
    if review.get("stale"):
        return False
    commit_id = review.get("commit_id")
    # FAIL-CLOSED: a missing/empty/non-string commit_id is REJECTED. The
    # previous code had `if isinstance(commit_id, str) and commit_id and
    # headsha:` which SKIPPED the check when the review carried no
    # commit_id. That was the spoof-bug surface.
    if not isinstance(commit_id, str) or not commit_id:
        return False
    # FAIL-CLOSED: a present-but-wrong commit_id is also REJECTED. Stale
    # reviews (on a previous head) cannot count.
    if not isinstance(headsha, str) or not headsha or commit_id != headsha:
        return False
    return True


# ---------------------------------------------------------------------------
# Per-verdict predicates
# ---------------------------------------------------------------------------


def is_genuine_approval(
    review: object,
    *,
    headsha: str,
    reviewer_set: Optional[Iterable[str]] = None,
) -> bool:
    """Return True iff `review` is a genuine APPROVED on the current head.

    When `reviewer_set` is provided, the review's `user.login` must be in
    the set (the merge-queue uses this to count only "recognised"
    reviewers for the 2-genuine floor; review-check.sh applies its own
    team-membership probe separately and so does not pass a set).
    """
    if not isinstance(review, dict):
        return False
    # EXACT-ENUM (fail-closed): no .upper()/.strip() coercion. A
    # case-variant or whitespace-padded state is a forged row and is
    # rejected, not normalised into APPROVED.
    if review.get("state") != STATE_APPROVED:
        return False
    if not is_official_current_head(review, headsha):
        return False
    if reviewer_set is not None:
        user = (review.get("user") or {}).get("login")
        if not isinstance(user, str) or user not in set(reviewer_set):
            return False
    return True


def is_open_request_changes(review: object, *, headsha: str) -> bool:
    """Return True iff `review` is an open official REQUEST_CHANGES on the
    current head. Same fail-closed contract as is_genuine_approval —
    a missing commit_id is REJECTED, not silently treated as 'still
    blocking the merge from an old head'.
    """
    if not isinstance(review, dict):
        return False
    # EXACT-ENUM (fail-closed): same contract as is_genuine_approval. A
    # lowercase/mixed-case "request_changes" must NOT be coerced into a
    # block-erasing match; an exact REQUEST_CHANGES is required.
    if review.get("state") != STATE_REQUEST_CHANGES:
        return False
    if not is_official_current_head(review, headsha):
        return False
    return True


# ---------------------------------------------------------------------------
# Consumer-facing reducer (returns the two call sites need)
# ---------------------------------------------------------------------------


def classify_reviews(
    reviews: Iterable[object],
    *,
    headsha: str,
    reviewer_set: Optional[Iterable[str]] = None,
) -> Tuple[set[str], list[str]]:
    """Reduce a PR's reviews to (approvers, request_changes) on the CURRENT head.

    approvers: distinct logins whose LATEST official review on the current
        head is APPROVED.
    request_changes: distinct logins whose LATEST official review on the
        current head is REQUEST_CHANGES.

    Gitea returns reviews oldest-first. We keep the latest *VALID*
    submission per user (later VALID entries overwrite earlier ones; an
    invalid later row — a COMMENT, or a review with a null/old commit_id —
    is ignored and can NOT overwrite or erase a genuine review). See the
    inline VALIDATE-BEFORE-REDUCE note below for the exploit this closes.
    """
    reviewer_set_set = set(reviewer_set) if reviewer_set is not None else None

    # VALIDATE-BEFORE-REDUCE (SEV-1 internal#812 follow-up).
    #
    # The earlier implementation reduced FIRST (latest row per user, keyed
    # only on state in {APPROVED, REQUEST_CHANGES}) and validated the single
    # surviving row AFTER. That is reduce-before-validate, and it is
    # exploitable: a user posts a genuine current-head APPROVED (or
    # REQUEST_CHANGES), then posts a LATER row that fails the fail-closed
    # predicate (a COMMENT, or an APPROVED with a null/old commit_id). The
    # later INVALID row overwrote the genuine one in latest_by_user, so a
    # real approval was masked, and — worse — a real current-head
    # REQUEST_CHANGES could be erased and the block silently evaporate.
    #
    # The fix: filter to VALID reviews FIRST (each row must pass
    # is_official_current_head AND carry an APPROVED/REQUEST_CHANGES state),
    # and only then reduce to the latest VALID review per user. An invalid
    # later row is never eligible to become a user's "latest" state, so it
    # cannot overwrite or erase a genuine review. A user's verdict is the
    # state of their latest VALID (official, current-head, non-dismissed,
    # non-stale, commit_id-present-and-matching) review.
    latest_valid_by_user: dict = {}
    for review in reviews:
        if not isinstance(review, dict):
            continue
        user = (review.get("user") or {}).get("login")
        if not isinstance(user, str):
            continue
        if reviewer_set_set is not None and user not in reviewer_set_set:
            continue
        # EXACT-ENUM (fail-closed): exact constants only, no coercion. A
        # case-coerced row must not become eligible to overwrite/erase a
        # genuine per-user verdict in the reduce below.
        state = review.get("state")
        if state not in (STATE_APPROVED, STATE_REQUEST_CHANGES):
            continue
        # Fail-closed predicate BEFORE the reduce: official, not dismissed,
        # not stale, commit_id present AND == head. Invalid rows are dropped
        # here and so can never become the per-user "latest".
        if not is_official_current_head(review, headsha):
            continue
        latest_valid_by_user[user] = review

    approvers: set[str] = set()
    request_changes: list[str] = []
    for user, review in latest_valid_by_user.items():
        # Each surviving review already passed is_official_current_head, so
        # the state alone determines the verdict. We still go through the
        # per-verdict SSOT predicates so the rule cannot drift.
        if is_genuine_approval(review, headsha=headsha, reviewer_set=None):
            approvers.add(user)
        elif is_open_request_changes(review, headsha=headsha):
            request_changes.append(user)
    return approvers, request_changes
