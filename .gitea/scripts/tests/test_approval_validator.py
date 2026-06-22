#!/usr/bin/env python3
"""
Mutation-verified unit tests for the SSOT fail-closed approval predicate
in _approval_validator.py (SEV-1 internal#812).

Each test asserts REJECTION explicitly. A reviewer who weakens the
predicate — e.g., by removing the commit_id check, by reintroducing the
"no commit_id is accepted" escape hatch, by changing `!=` to `==` in the
head comparison, or by allowing official == false — will trip these
tests in CI.

Run:
  cd .gitea/scripts
  python3 -m unittest tests.test_approval_validator -v
  # or
  python3 tests/test_approval_validator.py
"""

from __future__ import annotations

import os
import sys
import unittest

# Same-dir import — test lives next to _approval_validator.py
sys.path.insert(
    0,
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
)
from _approval_validator import (  # noqa: E402
    classify_reviews,
    is_genuine_approval,
    is_official_current_head,
    is_open_request_changes,
)

HEAD = "0123456789abcdef0123456789abcdef01234567"
OTHER_HEAD = "fedcba9876543210fedcba9876543210fedcba98"


def _review(
    *,
    state: str = "APPROVED",
    official: bool = True,
    dismissed: bool = False,
    stale: bool = False,
    commit_id: object = HEAD,
    user: str = "reviewer-1",
    body: str = "",
) -> dict:
    """Build a minimal review row shaped like the Gitea reviews API."""
    return {
        "id": 1,
        "user": {"login": user, "id": 1},
        "body": body,
        "state": state,
        "official": official,
        "dismissed": dismissed,
        "stale": stale,
        "commit_id": commit_id,
    }


# ---------------------------------------------------------------------------
# Hard contract: every fail-closed branch must reject
# ---------------------------------------------------------------------------


class IsOfficialCurrentHeadFailClosed(unittest.TestCase):
    """is_official_current_head is the common predicate. EVERY condition
    is mandatory. The tests below assert REJECTION for every possible
    failure of any condition."""

    def test_accepts_canonical_review(self):
        self.assertTrue(is_official_current_head(_review(), HEAD))

    def test_rejects_non_dict(self):
        for bad in [None, "string", 42, [], (), object()]:
            with self.subTest(bad=bad):
                self.assertFalse(is_official_current_head(bad, HEAD))

    def test_rejects_when_official_is_false(self):
        for v in [False, None, 0, "false"]:
            with self.subTest(v=v):
                self.assertFalse(
                    is_official_current_head(_review(official=v), HEAD)
                )

    def test_rejects_when_dismissed(self):
        for v in [True, "true", 1]:
            with self.subTest(v=v):
                self.assertFalse(
                    is_official_current_head(_review(dismissed=v), HEAD)
                )

    def test_rejects_when_stale(self):
        for v in [True, "true", 1]:
            with self.subTest(v=v):
                self.assertFalse(
                    is_official_current_head(_review(stale=v), HEAD)
                )

    def test_rejects_when_commit_id_missing(self):
        """FAIL-CLOSED #1: missing commit_id is REJECTED.
        This is the spoof signature that closed #843 (with CR2 + Researcher
        both flagging it)."""
        for bad in [None, "", 0, False, [], {}, ()]:
            with self.subTest(commit_id=bad):
                self.assertFalse(
                    is_official_current_head(_review(commit_id=bad), HEAD),
                    f"commit_id={bad!r} must reject (fail-closed)",
                )

    def test_rejects_when_commit_id_wrong_type(self):
        for bad in [123, 1.5, True, ["abc"], {"sha": HEAD}, ("tuple",)]:
            with self.subTest(commit_id=bad):
                self.assertFalse(
                    is_official_current_head(_review(commit_id=bad), HEAD)
                )

    def test_rejects_when_commit_id_stale(self):
        """FAIL-CLOSED #2: present-but-wrong commit_id is REJECTED. Stale
        reviews on a previous head cannot count."""
        self.assertFalse(
            is_official_current_head(_review(commit_id=OTHER_HEAD), HEAD)
        )

    def test_rejects_when_head_missing(self):
        for bad in [None, "", 0, False]:
            with self.subTest(head=bad):
                self.assertFalse(
                    is_official_current_head(_review(), bad)
                )

    def test_rejects_when_head_wrong_type(self):
        self.assertFalse(is_official_current_head(_review(), 123))
        self.assertFalse(is_official_current_head(_review(), ["x"]))


# ---------------------------------------------------------------------------
# is_genuine_approval
# ---------------------------------------------------------------------------


class IsGenuineApprovalContract(unittest.TestCase):
    def test_accepts_canonical_approval(self):
        self.assertTrue(
            is_genuine_approval(_review(state="APPROVED"), headsha=HEAD)
        )

    def test_rejects_non_approved_states(self):
        for state in ("REQUEST_CHANGES", "COMMENT", "PENDING", "DISMISSED", "approve", "", "bogus"):
            with self.subTest(state=state):
                self.assertFalse(
                    is_genuine_approval(_review(state=state), headsha=HEAD)
                )

    def test_rejects_case_coerced_approved_states(self):
        """EXACT-ENUM fail-closed (RCs 9849/9851/9852): Gitea always emits
        the canonical UPPERCASE "APPROVED". A lowercase/mixed-case/padded
        value is the signature of a forged row and MUST be rejected, not
        coerced via .upper() into an accepted APPROVED. Each of these was
        ACCEPTED before the exact-enum fix."""
        for state in (
            "approved", "Approved", "ApProVeD", "APPROVED ", " APPROVED",
            "approved\n", "\tAPPROVED",
        ):
            with self.subTest(state=state):
                self.assertFalse(
                    is_genuine_approval(_review(state=state), headsha=HEAD),
                    f"case-coerced/padded state {state!r} must NOT count as "
                    "a genuine approval",
                )

    def test_rejects_non_official_approval(self):
        """Comment-based / non-official 'APPROVED' is REJECTED.
        PM: 'reject comment-based / non-official reviews'."""
        self.assertFalse(
            is_genuine_approval(
                _review(state="APPROVED", official=False), headsha=HEAD
            )
        )

    def test_rejects_dismissed_approval(self):
        self.assertFalse(
            is_genuine_approval(
                _review(state="APPROVED", dismissed=True), headsha=HEAD
            )
        )

    def test_rejects_stale_head_approval(self):
        """commit_id != head is REJECTED. Stale-on-old-head approvals cannot
        count, even if they were official and not dismissed."""
        self.assertFalse(
            is_genuine_approval(
                _review(state="APPROVED", commit_id=OTHER_HEAD), headsha=HEAD
            )
        )

    def test_rejects_missing_commit_id_approval(self):
        """FAIL-CLOSED #3: the SEV-1 case. A APPROVED review with NO
        commit_id is the spoof-bug signature. Reject."""
        for bad in [None, "", 0, False]:
            with self.subTest(commit_id=bad):
                self.assertFalse(
                    is_genuine_approval(
                        _review(state="APPROVED", commit_id=bad), headsha=HEAD
                    ),
                    f"missing commit_id={bad!r} must reject",
                )

    def test_reviewer_set_filters_users(self):
        self.assertTrue(
            is_genuine_approval(
                _review(user="alice"),
                headsha=HEAD,
                reviewer_set={"alice", "bob"},
            )
        )
        self.assertFalse(
            is_genuine_approval(
                _review(user="carol"),
                headsha=HEAD,
                reviewer_set={"alice", "bob"},
            )
        )

    def test_reviewer_set_none_skips_check(self):
        # None means "no team filter at this layer" (e.g., review-check.sh
        # applies its own team-membership probe separately).
        self.assertTrue(
            is_genuine_approval(
                _review(user="anyone"),
                headsha=HEAD,
                reviewer_set=None,
            )
        )


# ---------------------------------------------------------------------------
# is_open_request_changes
# ---------------------------------------------------------------------------


class IsOpenRequestChangesContract(unittest.TestCase):
    def test_accepts_canonical_request_changes(self):
        self.assertTrue(
            is_open_request_changes(
                _review(state="REQUEST_CHANGES"), headsha=HEAD
            )
        )

    def test_rejects_non_request_changes_states(self):
        for state in ("APPROVED", "COMMENT", "PENDING", "DISMISSED"):
            with self.subTest(state=state):
                self.assertFalse(
                    is_open_request_changes(
                        _review(state=state), headsha=HEAD
                    )
                )

    def test_rejects_case_coerced_request_changes_states(self):
        """EXACT-ENUM fail-closed: a lowercase/mixed-case "request_changes"
        must NOT be coerced into an open-block match. Before the exact-enum
        fix, .upper() accepted these as REQUEST_CHANGES."""
        for state in (
            "request_changes", "Request_Changes", "REQUEST_CHANGES ",
            " REQUEST_CHANGES", "request_changes\n",
        ):
            with self.subTest(state=state):
                self.assertFalse(
                    is_open_request_changes(
                        _review(state=state), headsha=HEAD
                    ),
                    f"case-coerced/padded state {state!r} must NOT count as "
                    "an open REQUEST_CHANGES",
                )

    def test_rejects_when_dismissed(self):
        self.assertFalse(
            is_open_request_changes(
                _review(state="REQUEST_CHANGES", dismissed=True), headsha=HEAD
            )
        )

    def test_rejects_when_stale_head(self):
        self.assertFalse(
            is_open_request_changes(
                _review(state="REQUEST_CHANGES", commit_id=OTHER_HEAD),
                headsha=HEAD,
            )
        )

    def test_rejects_when_missing_commit_id(self):
        for bad in [None, "", 0]:
            with self.subTest(commit_id=bad):
                self.assertFalse(
                    is_open_request_changes(
                        _review(state="REQUEST_CHANGES", commit_id=bad),
                        headsha=HEAD,
                    )
                )


# ---------------------------------------------------------------------------
# classify_reviews — the merge-queue consumer
# ---------------------------------------------------------------------------


class ClassifyReviewsContract(unittest.TestCase):
    def test_basic_approvers_and_request_changes(self):
        reviews = [
            _review(user="alice", state="APPROVED", commit_id=HEAD),
            _review(user="bob", state="REQUEST_CHANGES", commit_id=HEAD),
        ]
        approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
        self.assertEqual(approvers, {"alice"})
        self.assertEqual(request_changes, ["bob"])

    def test_reviewer_set_filters_early(self):
        reviews = [
            _review(user="alice", state="APPROVED", commit_id=HEAD),
            _review(user="carol", state="APPROVED", commit_id=HEAD),
        ]
        approvers, _ = classify_reviews(
            reviews, headsha=HEAD, reviewer_set={"alice"}
        )
        self.assertEqual(approvers, {"alice"})

    def test_latest_review_per_user_wins(self):
        # alice's REQUEST_CHANGES (latest) supersedes her earlier APPROVED.
        reviews = [
            _review(user="alice", state="APPROVED", commit_id=HEAD),
            _review(user="alice", state="REQUEST_CHANGES", commit_id=HEAD),
        ]
        approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
        self.assertNotIn("alice", approvers)
        self.assertIn("alice", request_changes)

    def test_stale_head_approval_excluded(self):
        reviews = [
            _review(user="alice", state="APPROVED", commit_id=OTHER_HEAD),
        ]
        approvers, _ = classify_reviews(reviews, headsha=HEAD)
        self.assertEqual(approvers, set())

    def test_missing_commit_id_approval_excluded(self):
        """The SEV-1 fail-open surface. APPROVED + no commit_id → must NOT
        count toward approvers, even with stale=False/dismissed=False."""
        reviews = [
            _review(user="alice", state="APPROVED", commit_id=None),
            _review(user="bob", state="APPROVED", commit_id=""),
        ]
        approvers, _ = classify_reviews(reviews, headsha=HEAD)
        self.assertEqual(approvers, set())

    def test_dismissed_approval_excluded(self):
        reviews = [
            _review(user="alice", state="APPROVED", dismissed=True, commit_id=HEAD),
        ]
        approvers, _ = classify_reviews(reviews, headsha=HEAD)
        self.assertEqual(approvers, set())

    def test_non_official_approval_excluded(self):
        reviews = [
            _review(user="alice", state="APPROVED", official=False, commit_id=HEAD),
        ]
        approvers, _ = classify_reviews(reviews, headsha=HEAD)
        self.assertEqual(approvers, set())

    def test_comment_state_excluded(self):
        reviews = [
            _review(user="alice", state="COMMENT", commit_id=HEAD),
        ]
        approvers, _ = classify_reviews(reviews, headsha=HEAD)
        self.assertEqual(approvers, set())

    def test_case_coerced_approved_not_counted(self):
        """EXACT-ENUM via the reducer: a lowercase 'approved' (otherwise
        valid official current-head row) must NOT be counted as an approver.
        Before the fix, classify_reviews coerced it via .upper()."""
        for state in ("approved", "Approved", "APPROVED "):
            with self.subTest(state=state):
                reviews = [
                    _review(user="alice", state=state, commit_id=HEAD),
                ]
                approvers, request_changes = classify_reviews(
                    reviews, headsha=HEAD
                )
                self.assertEqual(approvers, set())
                self.assertEqual(request_changes, [])

    def test_case_coerced_request_changes_not_silently_dropped(self):
        """EXACT-ENUM via the reducer: a lowercase 'request_changes' must be
        rejected (not coerced into a block). Crucially, it must NOT silently
        erase a SAME-USER genuine current-head REQUEST_CHANGES posted
        earlier — the case-variant later row is invalid and is ignored, so
        the genuine block stands."""
        reviews = [
            _review(user="bob", state="REQUEST_CHANGES", commit_id=HEAD),
            _review(user="bob", state="request_changes", commit_id=HEAD),
        ]
        approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
        self.assertIn("bob", request_changes)
        self.assertNotIn("bob", approvers)

    def test_stale_head_request_changes_excluded(self):
        # A REQUEST_CHANGES on a previous head must NOT block the current head.
        reviews = [
            _review(user="bob", state="REQUEST_CHANGES", commit_id=OTHER_HEAD),
        ]
        _, request_changes = classify_reviews(reviews, headsha=HEAD)
        self.assertEqual(request_changes, [])

    # -----------------------------------------------------------------
    # VALIDATE-BEFORE-REDUCE regression tests (SEV-1 internal#812 follow-up).
    #
    # The bug: classify_reviews reduced to the LATEST row per user FIRST and
    # validated AFTER. A later INVALID row (a COMMENT, or APPROVED/
    # REQUEST_CHANGES with a null/old commit_id) from the same user could
    # overwrite a genuine current-head review — masking an approval or
    # ERASING a REQUEST_CHANGES block. The fix validates before the reduce,
    # so an invalid later row is never eligible to be a user's "latest".
    # -----------------------------------------------------------------

    def test_genuine_approval_not_masked_by_later_comment(self):
        """A genuine current-head APPROVED followed by a LATER COMMENT from
        the SAME user must STILL count as an approval. A later non-
        APPROVED/RC row (COMMENT) must not erase the approval. This is the
        reduce-before-validate masking bug."""
        reviews = [
            _review(user="alice", state="APPROVED", commit_id=HEAD),
            _review(user="alice", state="COMMENT", commit_id=HEAD),
        ]
        approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
        self.assertIn("alice", approvers)
        self.assertEqual(request_changes, [])

    def test_genuine_approval_not_masked_by_later_null_commit_id(self):
        """A genuine current-head APPROVED followed by a LATER APPROVED with
        a null commit_id (the spoof/invalid signature) from the SAME user
        must STILL count. The invalid later row must be ignored, not allowed
        to overwrite the valid earlier approval."""
        for bad in [None, ""]:
            with self.subTest(commit_id=bad):
                reviews = [
                    _review(user="alice", state="APPROVED", commit_id=HEAD),
                    _review(user="alice", state="APPROVED", commit_id=bad),
                ]
                approvers, _ = classify_reviews(reviews, headsha=HEAD)
                self.assertIn(
                    "alice", approvers,
                    f"later invalid commit_id={bad!r} must not mask the "
                    "genuine current-head approval",
                )

    def test_genuine_approval_not_masked_by_later_stale_commit_id(self):
        """A genuine current-head APPROVED followed by a LATER APPROVED on a
        STALE (old) head from the SAME user must STILL count toward
        approvers — the stale later row is invalid and must be ignored."""
        reviews = [
            _review(user="alice", state="APPROVED", commit_id=HEAD),
            _review(user="alice", state="APPROVED", commit_id=OTHER_HEAD),
        ]
        approvers, _ = classify_reviews(reviews, headsha=HEAD)
        self.assertIn("alice", approvers)

    def test_request_changes_not_erased_by_later_comment(self):
        """A genuine current-head REQUEST_CHANGES followed by a LATER COMMENT
        from the SAME user must STILL block. The later invalid row must not
        erase the REQUEST_CHANGES — this is the worse, silently-evaporating-
        block variant of the bug."""
        reviews = [
            _review(user="bob", state="REQUEST_CHANGES", commit_id=HEAD),
            _review(user="bob", state="COMMENT", commit_id=HEAD),
        ]
        approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
        self.assertIn("bob", request_changes)
        self.assertNotIn("bob", approvers)

    def test_request_changes_not_erased_by_later_null_commit_id(self):
        """A genuine current-head REQUEST_CHANGES followed by a LATER
        REQUEST_CHANGES with a null/old commit_id from the SAME user must
        STILL block. The invalid later row must be ignored, not allowed to
        relocate the user's verdict off the current head."""
        for bad in [None, "", OTHER_HEAD]:
            with self.subTest(commit_id=bad):
                reviews = [
                    _review(user="bob", state="REQUEST_CHANGES", commit_id=HEAD),
                    _review(user="bob", state="REQUEST_CHANGES", commit_id=bad),
                ]
                _, request_changes = classify_reviews(reviews, headsha=HEAD)
                self.assertIn(
                    "bob", request_changes,
                    f"later invalid commit_id={bad!r} must not erase the "
                    "genuine current-head REQUEST_CHANGES block",
                )

    def test_request_changes_not_erased_by_later_approved_invalid(self):
        """A genuine current-head REQUEST_CHANGES followed by a LATER
        INVALID APPROVED (null commit_id) from the SAME user must STILL
        block AND must NOT count the user as an approver. The invalid
        approval must not flip a real block into a pass."""
        reviews = [
            _review(user="bob", state="REQUEST_CHANGES", commit_id=HEAD),
            _review(user="bob", state="APPROVED", commit_id=None),
        ]
        approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
        self.assertIn("bob", request_changes)
        self.assertNotIn("bob", approvers)

    def test_genuine_request_changes_still_supersedes_genuine_approval(self):
        """Sanity: a genuine LATER current-head REQUEST_CHANGES still
        supersedes an earlier genuine APPROVED from the same user (the
        valid-row supersession we MUST preserve — only INVALID later rows
        are ignored). Guards against an over-correction that ignores all
        later rows."""
        reviews = [
            _review(user="alice", state="APPROVED", commit_id=HEAD),
            _review(user="alice", state="REQUEST_CHANGES", commit_id=HEAD),
        ]
        approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
        self.assertNotIn("alice", approvers)
        self.assertIn("alice", request_changes)

    def test_genuine_approval_still_supersedes_genuine_request_changes(self):
        """Sanity: a genuine LATER current-head APPROVED supersedes an
        earlier genuine REQUEST_CHANGES from the same user."""
        reviews = [
            _review(user="alice", state="REQUEST_CHANGES", commit_id=HEAD),
            _review(user="alice", state="APPROVED", commit_id=HEAD),
        ]
        approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
        self.assertIn("alice", approvers)
        self.assertEqual(request_changes, [])

    def test_two_valid_approvers_plus_one_invalid_later_row(self):
        """Two distinct users with valid current-head approvals + a third
        user whose ONLY genuine approval is followed by an invalid later
        row → all three real approvers are counted; the invalid later row
        does not drop the third user."""
        reviews = [
            _review(user="alice", state="APPROVED", commit_id=HEAD),
            _review(user="bob", state="APPROVED", commit_id=HEAD),
            _review(user="carol", state="APPROVED", commit_id=HEAD),
            _review(user="carol", state="COMMENT", commit_id=HEAD),
        ]
        approvers, request_changes = classify_reviews(reviews, headsha=HEAD)
        self.assertEqual(approvers, {"alice", "bob", "carol"})
        self.assertEqual(request_changes, [])


# ---------------------------------------------------------------------------
# Mutation-resistance smoke checks
#
# These tests document the mutations a reviewer would have to apply to
# weaken the gate. They are not synthetic; they verify that the
# predicate is structured so each known-softening mutation would also
# fail at least one other test in this file. We can't actually mutate
# the source in CI, but these tests are explicit about the mutations
# that would slip through, and the suite is dense enough that any
# loosening of the predicate will fail multiple cases.
# ---------------------------------------------------------------------------


class MutationResistance(unittest.TestCase):
    def test_documented_mutation_remove_commit_id_check_fails(self):
        """If a reviewer removes the commit_id check (e.g., reverts to
        the pre-fix `if isinstance(commit_id, str) and commit_id and
        headsha:` guard, or replaces `commit_id != headsha` with True),
        the missing-commit_id tests above (test_rejects_when_commit_id_missing
        in IsOfficialCurrentHeadFailClosed, test_rejects_missing_commit_id_approval
        in IsGenuineApprovalContract, test_missing_commit_id_approval_excluded
        in ClassifyReviewsContract) would all fail. The reviewer would have
        to weaken all three test categories to slip the SEV-1 surface in."""
        # Sanity: every missing-commit_id case is a False today.
        for bad in [None, "", 0, False]:
            with self.subTest(commit_id=bad):
                self.assertFalse(
                    is_official_current_head(_review(commit_id=bad), HEAD)
                )
                self.assertFalse(
                    is_genuine_approval(
                        _review(commit_id=bad), headsha=HEAD
                    )
                )

    def test_documented_mutation_change_neq_to_eq_fails(self):
        """If a reviewer changes `commit_id != headsha` to `commit_id == headsha`
        in the head comparison (inverting the check), the stale-head tests
        (test_rejects_when_commit_id_stale, test_stale_head_approval_excluded)
        would fail because the wrong head would now match."""
        self.assertFalse(
            is_official_current_head(_review(commit_id=OTHER_HEAD), HEAD)
        )

    def test_documented_mutation_drop_official_check_fails(self):
        """If a reviewer drops the `if not review.get('official')` check, the
        non-official tests (test_rejects_when_official_is_false,
        test_rejects_non_official_approval, test_non_official_approval_excluded)
        would all fail."""
        self.assertFalse(
            is_genuine_approval(
                _review(state="APPROVED", official=False), headsha=HEAD
            )
        )


if __name__ == "__main__":
    unittest.main()
