import importlib
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[1] / "gitea-merge-queue.py"
spec = importlib.util.spec_from_file_location("gitea_merge_queue", SCRIPT)
mq = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = mq
spec.loader.exec_module(mq)


def test_latest_statuses_dedupes_by_context_newest_first():
    statuses = [
        {"context": "CI / all-required (pull_request)", "status": "failure"},
        {"context": "sop-checklist / all-items-acked (pull_request_target)", "state": "success"},
        {"context": "CI / all-required (pull_request)", "status": "success"},
    ]

    latest = mq.latest_statuses_by_context(statuses)

    assert latest["CI / all-required (pull_request)"]["status"] == "failure"
    assert latest["sop-checklist / all-items-acked (pull_request_target)"]["state"] == "success"


def test_required_contexts_green_rejects_missing_and_pending():
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "pending"},
    ])

    ok, missing_or_bad = mq.required_contexts_green(
        latest,
        [
            "CI / all-required (pull_request)",
            "sop-checklist / all-items-acked (pull_request_target)",
            "qa-review / approved (pull_request_target)",
        ],
    )

    assert ok is False
    assert missing_or_bad == [
        "sop-checklist / all-items-acked (pull_request_target)=pending",
        "qa-review / approved (pull_request_target)=missing",
    ]


def test_required_contexts_green_rejects_volume_skipped():
    """volume-skipped pending is a partial view, not a genuine soft-fail.

    Per sop-checklist.py:1179-1187, volume_skipped posts pending with a
    '[volume-skipped]' prefix. The merge queue must NOT treat this as an
    acceptable soft-fail — the gate did not finish evaluating.
    """
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {
            "context": "sop-checklist / all-items-acked (pull_request_target)",
            "status": "pending",
            "description": "[volume-skipped] comment-cap=1000 hit; please file ...",
        },
    ])

    ok, missing_or_bad = mq.required_contexts_green(
        latest,
        [
            "CI / all-required (pull_request)",
            "sop-checklist / all-items-acked (pull_request_target)",
        ],
    )

    assert ok is False
    assert "sop-checklist / all-items-acked (pull_request_target)=pending" in missing_or_bad


def test_choose_next_pr_sorts_by_queue_label_timestamp_then_number():
    issues = [
        {
            "number": 12,
            "pull_request": {},
            "labels": [{"name": "merge-queue"}],
            "created_at": "2026-05-13T05:00:00Z",
            "updated_at": "2026-05-13T06:00:00Z",
        },
        {
            "number": 9,
            "pull_request": {},
            "labels": [{"name": "merge-queue"}],
            "created_at": "2026-05-13T04:00:00Z",
            "updated_at": "2026-05-13T07:00:00Z",
        },
        {
            "number": 7,
            "labels": [{"name": "merge-queue"}],
            "created_at": "2026-05-13T03:00:00Z",
        },
    ]

    selected = mq.choose_next_queued_issue(issues, queue_label="merge-queue")

    assert selected["number"] == 9


def test_pr_needs_update_when_base_sha_absent_from_commits():
    commits = [
        {"sha": "head"},
        {"sha": "parent"},
    ]

    assert mq.pr_contains_base_sha(commits, "mainsha") is False
    assert mq.pr_contains_base_sha(commits, "parent") is True


def _ready_kwargs(**overrides):
    """Default kwargs for a fully-ready merge; override per test.

    Includes the uniform governance checks (qa-review, security-review,
    sop-checklist) as required contexts and green statuses, matching the
    behaviour of process_once which merges GOVERNANCE_REQUIRED_CONTEXTS
    with branch-protection contexts.
    """
    base = dict(
        main_status={
            "state": "success",
            "statuses": [{"context": "CI / all-required (push)", "status": "success"}],
        },
        pr_status={
            "state": "success",
            "statuses": [
                {"context": "CI / all-required (pull_request)", "status": "success"},
                {"context": "qa-review / approved (pull_request_target)", "status": "success"},
                {"context": "security-review / approved (pull_request_target)", "status": "success"},
                {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
            ],
        },
        required_contexts=[
            "CI / all-required (pull_request)",
            "qa-review / approved (pull_request_target)",
            "security-review / approved (pull_request_target)",
            "sop-checklist / all-items-acked (pull_request_target)",
        ],
        required_approvals=2,
        approvers={"agent-reviewer-cr2", "agent-researcher"},
        request_changes=[],
        pr_has_current_base=True,
        mergeable=True,
    )
    base.update(overrides)
    return base


def test_merge_decision_requires_main_green_pr_green_and_current_base():
    decision = mq.evaluate_merge_readiness(**_ready_kwargs())

    assert decision.ready is True
    assert decision.action == "merge"
    assert decision.force is False  # no non-required reds present


def test_behind_main_but_mergeable_pr_merges_directly():
    """§SOP-22 (#2358): a behind-main but CONFLICT-FREE PR (mergeable is True)
    merges DIRECTLY — no update step. Branch protection does not require strict
    up-to-date, and calling /update would dismiss the genuine approvals
    (dismiss_stale_approvals), forcing re-review every tick (the throughput
    bottleneck). This replaces the old update-before-merge behavior."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_has_current_base=False, mergeable=True)
    )

    assert decision.ready is True
    assert decision.action == "merge"


def test_behind_main_and_not_mergeable_pr_updates():
    """The /update path is reached ONLY when the PR is NOT mergeable AND its head
    lacks current main — refreshing the branch may resolve a behind-main
    non-conflict; a real conflict 409s and is held (#2352)."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_has_current_base=False, mergeable=False)
    )

    assert decision.ready is False
    assert decision.action == "update"


def test_current_base_but_not_mergeable_pr_waits():
    """Up-to-date with main yet Gitea reports not-mergeable → genuine conflict
    against current main (or still computing). The queue cannot act: WAIT,
    never update (update would not help) and never merge (fail-closed)."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_has_current_base=True, mergeable=False)
    )

    assert decision.ready is False
    assert decision.action == "wait"
    assert "not mergeable" in decision.reason


def test_behind_main_and_mergeable_none_waits_not_update():
    """§SOP-22 (CR2 #2374) — the churn-residual fix. A BEHIND-MAIN PR whose
    mergeability Gitea is STILL COMPUTING (mergeable is None) must WAIT, NOT take
    the /update path. The old code collapsed None→False, so a behind-main +
    None PR returned action="update" → /pulls/{n}/update → dismiss_stale_approvals
    → the exact rebase-churn this change eliminates, fired during the compute
    window. None and False are now DISTINCT: None waits, False updates."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_has_current_base=False, mergeable=None)
    )

    assert decision.ready is False
    assert decision.action == "wait"  # NOT "update" — no churn during compute
    assert "computed" in decision.reason


def test_current_base_and_mergeable_none_waits():
    """Up-to-date with main + mergeable None (still computing) → WAIT (unchanged
    fail-closed; just confirming None is never merged regardless of base)."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_has_current_base=True, mergeable=None)
    )

    assert decision.ready is False
    assert decision.action == "wait"


def test_MergePermissionError_inherits_from_ApiError():
    assert issubclass(mq.MergePermissionError, mq.ApiError)


def test_MergePermissionError_message_preserved():
    exc = mq.MergePermissionError("POST /merge -> HTTP 405: User not allowed")
    assert "405" in str(exc)
    assert "User not allowed" in str(exc)


# --------------------------------------------------------------------------
# Fix 1: merge criterion — genuine approvals on current head + required-only
# --------------------------------------------------------------------------

REVIEWERS = {"agent-reviewer", "agent-researcher", "agent-reviewer-cr2"}


def test_genuine_approvals_counts_two_distinct_on_current_head():
    reviews = [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
    ]
    approvers, rc = mq.genuine_approvals(reviews, headsha="HEAD", reviewer_set=REVIEWERS)
    assert approvers == {"agent-researcher", "agent-reviewer-cr2"}
    assert rc == []


def test_genuine_approvals_ignores_stale_dismissed_and_wrong_head():
    reviews = [
        # stale → not counted
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": True, "dismissed": False, "commit_id": "OLD"},
        # dismissed → not counted
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": True, "commit_id": "HEAD"},
        # commit_id mismatch (prior head) → not counted
        {"state": "APPROVED", "user": {"login": "agent-reviewer"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "OLD"},
    ]
    approvers, rc = mq.genuine_approvals(reviews, headsha="HEAD", reviewer_set=REVIEWERS)
    assert approvers == set()
    assert rc == []


def test_genuine_approvals_ignores_unofficial_and_outsiders():
    reviews = [
        # unofficial → not counted
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": False, "stale": False, "dismissed": False, "commit_id": "HEAD"},
        # outside reviewer set (e.g. CTO-agent / random) → not counted
        {"state": "APPROVED", "user": {"login": "hongming-codex-laptop"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
    ]
    approvers, rc = mq.genuine_approvals(reviews, headsha="HEAD", reviewer_set=REVIEWERS)
    assert approvers == set()


def test_genuine_approvals_latest_review_supersedes_earlier():
    # agent-reviewer-cr2 approved then later requested changes on same head.
    reviews = [
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
        {"state": "REQUEST_CHANGES", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
    ]
    approvers, rc = mq.genuine_approvals(reviews, headsha="HEAD", reviewer_set=REVIEWERS)
    assert approvers == set()
    assert rc == ["agent-reviewer-cr2"]


def test_merge_blocked_when_open_request_changes_on_current_head():
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(request_changes=["agent-reviewer-cr2"])
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert "REQUEST_CHANGES" in decision.reason


def test_merge_blocked_when_insufficient_genuine_approvals():
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(approvers={"agent-researcher"}, required_approvals=2)
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert "insufficient genuine approvals" in decision.reason


def test_governance_red_blocks_merge():
    # Uniform gate: qa-review, security-review, sop-checklist are ALWAYS
    # required. If any of them fail/pending, the PR is blocked.
    pr_status = {
        "state": "failure",
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "failure"},
            {"context": "security-review / approved (pull_request_target)", "status": "pending"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "failure"},
            {"context": "Staging SaaS / e2e (pull_request)", "status": "failure"},
        ],
    }
    decision = mq.evaluate_merge_readiness(**_ready_kwargs(pr_status=pr_status))
    assert decision.ready is False
    assert decision.action == "wait"
    assert "required contexts not green" in decision.reason


def test_non_required_red_does_not_block_merge():
    # Uniform gate flip (CTO #2407): qa-review, security-review, sop-checklist
    # are REQUIRED for ALL PRs. A PR with these failing/pending must NOT be
    # force-mergeable, even if BP-required CI is green and approvals are genuine.
    pr_status = {
        "state": "failure",
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request)", "status": "failure"},
            {"context": "security-review / approved (pull_request)", "status": "pending"},
            {"context": "sop-checklist / all-items-acked (pull_request)", "status": "failure"},
            {"context": "Staging SaaS / e2e (pull_request)", "status": "failure"},
        ],
    }
    decision = mq.evaluate_merge_readiness(**_ready_kwargs(pr_status=pr_status))
    assert decision.ready is False
    assert decision.action == "wait"
    assert "required contexts not green" in decision.reason
    assert decision.force is False


def test_non_required_advisory_red_does_not_block_merge():
    # Governance checks are green; only advisory non-required reds (Staging SaaS)
    # are present → PR is still mergeable with force_merge bypassing the advisory.
    pr_status = {
        "state": "failure",  # combined polluted by advisory non-required reds
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
            {"context": "Staging SaaS / e2e (pull_request)", "status": "failure"},
        ],
    }
    decision = mq.evaluate_merge_readiness(**_ready_kwargs(pr_status=pr_status))
    assert decision.ready is True
    assert decision.action == "merge"
    assert decision.force is True


def test_failing_required_context_blocks_even_with_approvals():
    pr_status = {
        "state": "failure",
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "failure"},
        ],
    }
    decision = mq.evaluate_merge_readiness(**_ready_kwargs(pr_status=pr_status))
    assert decision.ready is False
    assert decision.action == "wait"
    assert decision.force is False
    assert "required contexts not green" in decision.reason


def test_unmergeable_pr_blocks():
    decision = mq.evaluate_merge_readiness(**_ready_kwargs(mergeable=False))
    assert decision.ready is False
    assert decision.action == "wait"
    assert "not mergeable" in decision.reason


# --------------------------------------------------------------------------
# Fix 1 (cont.): required contexts come from branch protection (fail-closed)
# --------------------------------------------------------------------------

def test_parse_branch_protection_uses_status_check_contexts():
    bp = mq.parse_branch_protection({
        "enable_status_check": True,
        "status_check_contexts": [
            "CI / all-required (pull_request)",
            "E2E API Smoke Test / E2E API Smoke Test (pull_request)",
        ],
        "required_approvals": 2,
        "block_on_rejected_reviews": True,
    })
    assert bp.required_contexts == [
        "CI / all-required (pull_request)",
        "E2E API Smoke Test / E2E API Smoke Test (pull_request)",
    ]
    assert bp.required_approvals == 2
    assert bp.block_on_rejected_reviews is True


def test_parse_branch_protection_fail_closed_when_contexts_missing():
    # enable_status_check true but no enumerable contexts → must raise so the
    # queue HOLDS rather than merging against an unverified required set.
    import pytest
    with pytest.raises(mq.BranchProtectionUnavailable):
        mq.parse_branch_protection({
            "enable_status_check": True,
            "status_check_contexts": None,
            "required_approvals": 2,
        })
    with pytest.raises(mq.BranchProtectionUnavailable):
        mq.parse_branch_protection({
            "enable_status_check": True,
            "status_check_contexts": [],
            "required_approvals": 2,
        })


def test_parse_branch_protection_fail_closed_on_non_object():
    import pytest
    with pytest.raises(mq.BranchProtectionUnavailable):
        mq.parse_branch_protection(None)


# --------------------------------------------------------------------------
# Fix 2: HOL — a permanent merge error HOLDS the PR (applies HOLD_LABEL)
# --------------------------------------------------------------------------

def test_process_once_holds_pr_on_permanent_merge_error(monkeypatch):
    """A 405 on merge must apply HOLD_LABEL so the queue advances, not loop."""
    calls = {"hold_label": None, "merge_attempts": 0}

    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", {"merge-queue-hold", "do-not-auto-merge", "wip"})
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)

    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha):
        if sha == main_sha:
            return {"state": "success", "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        return {"state": "success", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: [
        {"number": 100, "pull_request": {}, "labels": [{"name": "merge-queue"}],
         "created_at": "2026-06-01T00:00:00Z"},
    ])
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n, "mergeable": True,
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": main_sha}, {"sha": head_sha}])
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ])

    def fake_merge(pr_number, *, dry_run, force=False):
        calls["merge_attempts"] += 1
        raise mq.MergePermissionError("POST /merge -> HTTP 405: User not allowed to merge PR")
    monkeypatch.setattr(mq, "merge_pull", fake_merge)

    def fake_add_label(pr_number, label_name, *, dry_run):
        calls["hold_label"] = (pr_number, label_name)
    monkeypatch.setattr(mq, "add_label_by_name", fake_add_label)
    monkeypatch.setattr(mq, "post_comment", lambda *a, **k: None)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 1
    # The HOL fix: PR was held, not silently left re-selectable.
    assert calls["hold_label"] == (100, "merge-queue-hold")


# --------------------------------------------------------------------------
# Fix 2 (cont.): mergeable=None is fail-CLOSED — Gitea is still computing the
# conflict check, so the queue must WAIT (re-check next tick), NOT merge. Only
# an explicit mergeable==True proceeds to an autonomous merge.
# --------------------------------------------------------------------------

def _fully_ready_process_once_monkeypatch(monkeypatch, mergeable, calls):
    """Wire process_once so every gate is green EXCEPT the `mergeable` field,
    which is set to the value under test. Records merge attempts in `calls`."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", {"merge-queue-hold", "do-not-auto-merge", "wip"})
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha):
        if sha == main_sha:
            return {"state": "success", "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        return {"state": "success", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: [
        {"number": 102, "pull_request": {}, "labels": [{"name": "merge-queue"}],
         "created_at": "2026-06-01T00:00:00Z"},
    ])
    # mergeable is the value under test; everything else is fully ready.
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n, "mergeable": mergeable,
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": main_sha}, {"sha": head_sha}])
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ])

    def fake_merge(pr_number, *, dry_run, force=False):
        calls["merge_attempts"] += 1
    monkeypatch.setattr(mq, "merge_pull", fake_merge)

    def fake_add_label(pr_number, label_name, *, dry_run):
        calls["hold_label"] = (pr_number, label_name)
    monkeypatch.setattr(mq, "add_label_by_name", fake_add_label)
    monkeypatch.setattr(mq, "update_pull", lambda *a, **k: calls.__setitem__("updated", True))
    monkeypatch.setattr(mq, "post_comment", lambda *a, **k: None)


def test_process_once_waits_when_mergeable_is_none(monkeypatch):
    """FAIL-CLOSED: mergeable=None means Gitea is still computing the conflict
    check. The queue must NOT merge this tick; it waits and re-checks next tick.
    Critically: this is transient, so the PR is NOT hold-labelled (it stays
    queued and re-selectable)."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=None, calls=calls)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    # The bug regression: a None mergeable must NEVER trigger an autonomous merge.
    assert calls["merge_attempts"] == 0
    # Transient, not permanent — must NOT be held/dequeued; retried next tick.
    assert calls["hold_label"] is None
    assert calls["updated"] is False


def test_process_once_waits_when_mergeable_field_absent(monkeypatch):
    """Some Gitea versions omit the `mergeable` field entirely. Absent must be
    treated the same as None — fail-closed, wait, no merge."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    # Reuse the ready wiring but drop the mergeable key from the pull payload.
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=None, calls=calls)
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n,  # no "mergeable" key at all
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 0
    assert calls["hold_label"] is None


def test_process_once_merges_when_mergeable_is_true(monkeypatch):
    """The decisive case: an explicit mergeable==True (with every other gate
    green) DOES proceed to the autonomous merge."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=True, calls=calls)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 1
    assert calls["hold_label"] is None


def test_process_once_behind_main_mergeable_none_waits_no_update(monkeypatch):
    """§SOP-22 (CR2 #2374) — end-to-end churn-residual regression. A BEHIND-MAIN
    PR (commits do NOT contain main_sha) whose mergeability Gitea is STILL
    COMPUTING (mergeable=None) must WAIT: process_once returns 0 and NEVER calls
    update_pull (which dismisses genuine approvals via dismiss_stale_approvals)
    NOR merge_pull NOR hold. The old None→False collapse routed this exact case
    into the /update path → approval-dismissing rebase churn during the compute
    window. This proves the durable churn elimination: no update, approvals
    preserved, re-checked next tick."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=None, calls=calls)
    # Make the head BEHIND main: commits do NOT contain main_sha. This is the
    # case the bug missed (the prior None test had current base, masking it).
    behind_head = "a" * 40
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": behind_head}])

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["updated"] is False  # NO /update → approvals NOT dismissed
    assert calls["merge_attempts"] == 0  # never merge on an unknown
    assert calls["hold_label"] is None  # transient → not held, retried next tick


# --------------------------------------------------------------------------
# §SOP-22: DIRECT-MERGE throughput fix (#2358). A conflict-free 2-genuine PR
# merges WITHOUT a pre-merge /update call, so its approvals are NOT dismissed by
# dismiss_stale_approvals. The merge bar (2-genuine-on-current-head +
# BP-required green + mergeable + no RC + opt-out) is UNCHANGED; only the
# unnecessary update-before-merge churn is removed. The /update path survives
# for the genuine case it is needed (not-mergeable + behind-main), where a real
# conflict 409s and is held per #2352. mergeable=None stays fail-closed.
# --------------------------------------------------------------------------


def test_process_once_merges_conflict_free_pr_without_update(monkeypatch):
    """§SOP-22(a) — the core throughput fix. A conflict-free, fully-approved PR
    merges WITHOUT update_pull ever being called. The old behavior called
    /update first whenever the head lacked current main, which dismissed the 2
    genuine approvals (dismiss_stale_approvals) and forced re-review every tick.
    Assert update_pull is NOT invoked and merge_pull IS invoked."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=True, calls=calls)
    # Make the head BEHIND main: commits do NOT contain main_sha. Under the old
    # logic this alone forced an update_pull; under the fix it merges directly.
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": head_sha}])

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 1  # merged directly
    assert calls["updated"] is False  # NO update_pull → approvals NOT dismissed
    assert calls["hold_label"] is None


def test_process_once_behind_main_conflict_free_merges_directly(monkeypatch):
    """§SOP-22(b) — explicit behind-main + conflict-free case: it still merges
    directly (branch protection does not require strict up-to-date)."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=True, calls=calls)
    behind_head = "a" * 40
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": behind_head}])

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 1
    assert calls["updated"] is False


def test_process_once_pauses_when_main_not_green_no_direct_merge(monkeypatch):
    """§SOP-22 backstop — the serialized safety that makes direct-merge safe:
    when main's required push contexts are NOT green (e.g. a prior direct merge
    introduced a semantic main-break caught by post-merge main CI), the queue
    PAUSES — it does NOT merge the next PR onto an unverified/red main."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=True, calls=calls)
    main_sha = "b" * 40

    def red_main_combined(sha):
        if sha == main_sha:
            return {"state": "failure",
                    "statuses": [{"context": "CI / all-required (push)", "status": "failure"}]}
        return {"state": "success",
                "statuses": [{"context": "CI / all-required (pull_request)", "status": "success"}]}
    monkeypatch.setattr(mq, "get_combined_status", red_main_combined)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 0  # paused — no merge onto red main
    assert calls["updated"] is False


def test_direct_merge_bar_unchanged_behind_main(monkeypatch):
    """§SOP-22(d) — the merge bar is UNCHANGED on the new direct-merge path. A
    behind-main + conflict-free PR is still rejected (no merge) when ANY gate
    fails: insufficient genuine approvals, red required context, open
    REQUEST_CHANGES, or opt-out label. Direct-merge removes the update churn, it
    does NOT weaken the bar — fail-closed on every gate."""
    head_sha = "a" * 40
    behind_main = dict(pr_has_current_base=False, mergeable=True)

    # <2 genuine approvals → wait, not merge.
    d = mq.evaluate_merge_readiness(
        **_ready_kwargs(approvers={"agent-researcher"}, **behind_main)
    )
    assert d.action == "wait" and d.ready is False

    # Red required context → wait, not merge.
    red_required = {"state": "failure", "statuses": [
        {"context": "CI / all-required (pull_request)", "status": "failure"}]}
    d = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_status=red_required, **behind_main)
    )
    assert d.action == "wait" and d.ready is False

    # Open REQUEST_CHANGES on current head → wait, not merge.
    d = mq.evaluate_merge_readiness(
        **_ready_kwargs(request_changes=["agent-reviewer-cr2"], **behind_main)
    )
    assert d.action == "wait" and d.ready is False


# --------------------------------------------------------------------------
# Fix 3: status fetch is fail-closed (failed fetch != green)
# --------------------------------------------------------------------------

def test_status_fetch_failure_is_fail_closed(monkeypatch):
    """If the PR head status fetch raises, the PR is skipped — never merged."""
    merged = {"called": False}

    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", {"merge-queue-hold", "do-not-auto-merge", "wip"})
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2, block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha):
        if sha == main_sha:
            return {"state": "success",
                    "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        # PR head status fetch fails — fail-closed must propagate.
        raise mq.ApiError("GET /commits/HEAD/status -> HTTP 502: bad gateway")
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: [
        {"number": 101, "pull_request": {}, "labels": [{"name": "merge-queue"}],
         "created_at": "2026-06-01T00:00:00Z"},
    ])
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n, "mergeable": True,
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": main_sha}, {"sha": head_sha}])
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [])

    def fake_merge(*a, **k):
        merged["called"] = True
    monkeypatch.setattr(mq, "merge_pull", fake_merge)

    # process_once lets the ApiError propagate; main() swallows it as a tick
    # no-op. Either way, no merge happens.
    import pytest
    with pytest.raises(mq.ApiError):
        mq.process_once(dry_run=False)
    assert merged["called"] is False


# --------------------------------------------------------------------------
# Pagination: api_paginated loops pages and is fail-closed on page errors
# --------------------------------------------------------------------------

def test_api_paginated_loops_pages_until_partial(monkeypatch):
    """api_paginated fetches all pages and stops when a page is < page_size."""
    calls = []

    def fake_api(method, path, *, query=None, **kw):
        page = int((query or {}).get("page", "1"))
        limit = int((query or {}).get("limit", "50"))
        calls.append((page, limit))
        if page == 1:
            return 200, [{"number": 1}, {"number": 2}]
        if page == 2:
            return 200, [{"number": 3}]
        return 200, []

    monkeypatch.setattr(mq, "api", fake_api)
    results = mq.api_paginated("GET", "/repos/o/r/issues", page_size=2)
    assert len(results) == 3
    assert results[0]["number"] == 1
    assert results[1]["number"] == 2
    assert results[2]["number"] == 3
    assert calls == [(1, 2), (2, 2)]


def test_api_paginated_raises_on_non_list(monkeypatch):
    """A page that returns a dict instead of list is an error."""
    def fake_api(method, path, *, query=None, **kw):
        return 200, {"message": "not found"}

    monkeypatch.setattr(mq, "api", fake_api)
    with pytest.raises(mq.ApiError):
        mq.api_paginated("GET", "/repos/o/r/issues")


def test_get_combined_status_propagates_paginated_statuses_error(monkeypatch):
    """If the paginated /statuses enrichment raises, the error propagates
    (fail-closed — we do NOT silently fall back to an incomplete status set)."""
    monkeypatch.setattr(mq, "OWNER", "o")
    monkeypatch.setattr(mq, "NAME", "r")

    def fake_api(method, path, *, query=None, **kw):
        if path.endswith("/status"):
            return 200, {"state": "success", "statuses": [{"context": "c1", "status": "success", "id": 1}]}
        if path.endswith("/statuses"):
            raise mq.ApiError("GET /statuses -> HTTP 502")
        raise mq.ApiError(f"unexpected {path}")

    monkeypatch.setattr(mq, "api", fake_api)
    with pytest.raises(mq.ApiError, match="GET /statuses"):
        mq.get_combined_status("a" * 40)


def test_process_once_holds_tick_when_branch_protection_unavailable(monkeypatch):
    """BP enumeration failure → HOLD the whole tick (no merge, rc 0)."""
    merged = {"called": False}
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")

    def fake_bp(branch):
        raise mq.BranchProtectionUnavailable("403 forbidden")
    monkeypatch.setattr(mq, "get_branch_protection", fake_bp)
    monkeypatch.setattr(mq, "merge_pull", lambda *a, **k: merged.__setitem__("called", True))

    rc = mq.process_once(dry_run=False)
    assert rc == 0
    assert merged["called"] is False


# --------------------------------------------------------------------------
# Fix 4 (issue #2352): a persistent 409-conflict-on-update HOLDS the PR and
# the queue ADVANCES — it does not retry the conflicted PR forever (HOL).
# --------------------------------------------------------------------------

def test_BranchUpdateConflictError_inherits_from_ApiError():
    assert issubclass(mq.BranchUpdateConflictError, mq.ApiError)


def test_update_pull_raises_conflict_error_on_409(monkeypatch):
    """A 409 from the /update endpoint becomes BranchUpdateConflictError so
    process_once can HOLD-and-advance rather than letting it propagate as a
    plain ApiError (which would leave the PR queued and re-selectable)."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")

    def fake_api(method, path, **kwargs):
        raise mq.ApiError("POST /pulls/1409/update -> HTTP 409: merge conflict")
    monkeypatch.setattr(mq, "api", fake_api)

    import pytest
    with pytest.raises(mq.BranchUpdateConflictError) as exc_info:
        mq.update_pull(1409, dry_run=False)
    assert "409" in str(exc_info.value)


def test_update_pull_reraises_non_409_errors(monkeypatch):
    """Non-409 update failures (e.g. 500) are NOT conflicts; they must NOT be
    swallowed as a hold — they re-raise as the original ApiError so the tick is
    a transient no-op and the PR is retried next tick."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")

    def fake_api(method, path, **kwargs):
        raise mq.ApiError("POST /pulls/1409/update -> HTTP 500: server error")
    monkeypatch.setattr(mq, "api", fake_api)

    import pytest
    with pytest.raises(mq.ApiError) as exc_info:
        mq.update_pull(1409, dry_run=False)
    # Re-raised as plain ApiError, NOT the conflict subclass.
    assert not isinstance(exc_info.value, mq.BranchUpdateConflictError)
    assert "500" in str(exc_info.value)


def _stale_pr_update_409_monkeypatch(monkeypatch, queued_issues, calls):
    """Wire process_once so the selected PR needs an update (head does NOT
    contain main) and the /update call returns a 409 conflict. Everything else
    is green. Records merge attempts and the applied hold label in `calls`."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", {"merge-queue-hold", "do-not-auto-merge", "wip"})
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha):
        if sha == main_sha:
            return {"state": "success", "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        return {"state": "success", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    # Scan-loop process_once enumerates candidates via list_candidate_issues.
    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: queued_issues)
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n, "mergeable": False,
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })
    # NOTE: mergeable is False (real conflict) AND commits do NOT contain
    # main_sha → pr_has_current_base is False → decision.action == "update".
    # Under the #2358 direct-merge fix the update path is reached ONLY when the
    # PR is NOT mergeable; a mergeable=True behind-main PR would merge directly,
    # so this fixture sets mergeable=False to exercise the #2352 409-on-update
    # hold path.
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": head_sha}])
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ])

    def fake_update(pr_number, *, dry_run):
        calls["update_attempts"] += 1
        raise mq.BranchUpdateConflictError(
            "POST /pulls/%d/update -> HTTP 409: merge conflict" % pr_number
        )
    monkeypatch.setattr(mq, "update_pull", fake_update)

    def fake_merge(pr_number, *, dry_run, force=False):
        calls["merge_attempts"] += 1
    monkeypatch.setattr(mq, "merge_pull", fake_merge)

    def fake_add_label(pr_number, label_name, *, dry_run):
        calls["hold_label"] = (pr_number, label_name)
        calls.setdefault("holds", []).append((pr_number, label_name))
    monkeypatch.setattr(mq, "add_label_by_name", fake_add_label)
    monkeypatch.setattr(mq, "post_comment", lambda *a, **k: None)


def test_process_once_holds_pr_on_409_conflict_on_update(monkeypatch):
    """The #2352 regression: a queued PR whose /update returns 409 must get the
    HOLD_LABEL (so the queue advances) and must NOT be merged. Without the fix
    the 409 propagated, the PR stayed queued, and the next tick re-selected the
    SAME conflicted PR forever (head-of-line block)."""
    calls = {"update_attempts": 0, "merge_attempts": 0, "hold_label": None}
    _stale_pr_update_409_monkeypatch(
        monkeypatch,
        queued_issues=[
            {"number": 1409, "pull_request": {}, "labels": [{"name": "merge-queue"}],
             "created_at": "2026-06-01T00:00:00Z"},
        ],
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["update_attempts"] == 1
    # Held, not merged — fail-closed.
    assert calls["hold_label"] == (1409, "merge-queue-hold")
    assert calls["merge_attempts"] == 0


def test_queue_advances_past_held_conflicted_pr(monkeypatch):
    """End-to-end HOL proof for #2352 under the scan-loop architecture: PR #1409
    (oldest) hits a 409-on-update and is HELD (HOLD_LABEL applied); once held it
    carries an opt-out label so it is excluded from candidate selection and can
    never re-block the queue. The 409-conflict hold (#2354) and the
    scan-through-skip (#2356) coexist: a held conflicted PR is both held AND no
    longer a candidate, so newer ready PRs behind it are unblocked."""
    calls = {"update_attempts": 0, "merge_attempts": 0, "hold_label": None}
    conflicted = {"number": 1409, "pull_request": {},
                  "labels": [{"name": "merge-queue"}],
                  "created_at": "2026-06-01T00:00:00Z"}
    next_ready = {"number": 1500, "pull_request": {},
                  "labels": [{"name": "merge-queue"}],
                  "created_at": "2026-06-02T00:00:00Z"}
    _stale_pr_update_409_monkeypatch(
        monkeypatch,
        queued_issues=[conflicted, next_ready],
        calls=calls,
    )

    # Tick 1: oldest (#1409) is selected, 409-on-update → held, then the scan
    # CONTINUES to #1500 (which also 409s in this fixture and is likewise held).
    # The key #2352 property: the conflicted oldest PR is held and does NOT stop
    # the scan from advancing past it.
    rc = mq.process_once(dry_run=False)
    assert rc == 0
    assert (1409, "merge-queue-hold") in calls["holds"]
    assert calls["merge_attempts"] == 0  # held, not merged — fail-closed

    # Simulate the label now present on #1409 (as the real hold would persist).
    conflicted["labels"] = [{"name": "merge-queue"}, {"name": "merge-queue-hold"}]

    # Next selection: the scan-loop candidate selector must SKIP the now-held
    # #1409 (HOLD_LABEL is in OPT_OUT_LABELS) and surface the next ready
    # candidate #1500 — the held PR no longer head-of-line blocks. The legacy
    # opt-IN selector (choose_next_queued_issue) honours the same hold.
    opt_out = {"merge-queue-hold", "do-not-auto-merge", "wip"}
    remaining = mq.choose_candidate_issues(
        [conflicted, next_ready],
        queue_label="merge-queue",
        opt_out_labels=opt_out,
        auto_discover=True,
    )
    assert [i["number"] for i in remaining] == [1500]
    selected = mq.choose_next_queued_issue(
        [conflicted, next_ready],
        queue_label="merge-queue",
        hold_label="merge-queue-hold",
    )
    assert selected is not None
    assert selected["number"] == 1500


# --------------------------------------------------------------------------
# §SOP-22: AUTO-DISCOVERY (opt-OUT, label-optional). The queue must be
# self-sustaining — a ready PR is considered/merged with NO `merge-queue`
# label, while opt-out labels (merge-queue-hold / do-not-auto-merge / wip) and
# drafts are skipped. The merge bar (approvals/required-green/mergeable) is
# unchanged; only candidate selection changes.
# --------------------------------------------------------------------------

OPT_OUT = {"merge-queue-hold", "do-not-auto-merge", "wip"}


def _issue(number, labels, *, created="2026-06-01T00:00:00Z", draft=False, is_pr=True):
    pr = {"draft": draft} if is_pr else None
    out = {
        "number": number,
        "labels": [{"name": n} for n in labels],
        "created_at": created,
    }
    if pr is not None:
        out["pull_request"] = pr
    return out


def test_auto_discover_selects_unlabeled_ready_pr():
    """A ready PR with NO merge-queue label is auto-considered (the autonomy fix:
    agents cannot self-label because their token lacks write:issue)."""
    issues = [_issue(50, labels=[])]  # no merge-queue label at all
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert selected is not None
    assert selected["number"] == 50


def test_auto_discover_skips_opt_out_labels():
    """Each opt-out label keeps a PR OUT of autonomous merging (the human escape
    hatch). A PR carrying any of them is never selected even though it is open."""
    for optout in OPT_OUT:
        issues = [_issue(60, labels=[optout])]
        selected = mq.choose_next_candidate_issue(
            issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
        )
        assert selected is None, f"{optout!r} should opt the PR out"


def test_auto_discover_skips_opt_out_even_when_queue_labeled():
    """An opt-out label beats the merge-queue label: a held/wip PR that also
    carries merge-queue is still skipped."""
    issues = [_issue(61, labels=["merge-queue", "wip"])]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert selected is None


def test_auto_discover_skips_drafts():
    issues = [_issue(62, labels=[], draft=True)]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert selected is None


def test_auto_discover_skips_non_pull_issues():
    """A plain issue (no pull_request key) is never a merge candidate."""
    issues = [_issue(63, labels=[], is_pr=False)]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert selected is None


def test_auto_discover_oldest_first_skipping_opt_out():
    """Selection is FIFO (oldest created_at first), and the opt-out PR is passed
    over for the next-oldest eligible PR."""
    issues = [
        _issue(70, labels=["do-not-auto-merge"], created="2026-06-01T01:00:00Z"),
        _issue(71, labels=[], created="2026-06-01T02:00:00Z"),
        _issue(72, labels=["merge-queue"], created="2026-06-01T03:00:00Z"),
    ]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert selected["number"] == 71  # 70 opted out, 71 is next-oldest eligible


def test_opt_in_mode_requires_queue_label():
    """AUTO_DISCOVER off restores legacy opt-IN: only merge-queue-labeled PRs are
    candidates; an unlabeled ready PR is NOT selected."""
    issues = [
        _issue(80, labels=[], created="2026-06-01T01:00:00Z"),
        _issue(81, labels=["merge-queue"], created="2026-06-01T02:00:00Z"),
    ]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=False
    )
    assert selected["number"] == 81


def test_opt_in_mode_still_honours_opt_out():
    """Even in opt-IN mode, an opt-out label on a queue-labeled PR skips it."""
    issues = [_issue(82, labels=["merge-queue", "merge-queue-hold"])]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=False
    )
    assert selected is None


def test_list_candidate_issues_omits_label_filter_when_auto_discover(monkeypatch):
    """The auto-discovery listing must NOT pass a `labels` filter (so unlabeled
    PRs are enumerated); the opt-IN listing must keep filtering by QUEUE_LABEL."""
    captured = {}

    def fake_api(method, path, *, query=None, **kw):
        captured["query"] = dict(query or {})
        return 200, []

    monkeypatch.setattr(mq, "api", fake_api)
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")

    mq.list_candidate_issues(auto_discover=True)
    assert "labels" not in captured["query"]
    assert captured["query"].get("type") == "pulls"

    mq.list_candidate_issues(auto_discover=False)
    assert captured["query"].get("label") == "merge-queue"


def _wire_ready_process_once(monkeypatch, *, issues, pr_payload, calls):
    """Wire process_once fully green EXCEPT candidate selection / pull payload,
    which the caller supplies to exercise auto-discovery end-to-end."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", OPT_OUT)
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2, block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha):
        if sha == main_sha:
            return {"state": "success", "statuses": [
                {"context": "CI / all-required (push)", "status": "success"},
            ]}
        return {"state": "success", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)
    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: issues)
    monkeypatch.setattr(mq, "get_pull", lambda n: dict(pr_payload, number=n))
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": main_sha}, {"sha": head_sha}])
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ])

    def fake_merge(pr_number, *, dry_run, force=False):
        calls["merged"] = pr_number
    monkeypatch.setattr(mq, "merge_pull", fake_merge)
    monkeypatch.setattr(mq, "update_pull", lambda *a, **k: calls.__setitem__("updated", True))
    monkeypatch.setattr(mq, "post_comment", lambda *a, **k: None)
    monkeypatch.setattr(mq, "add_label_by_name", lambda *a, **k: None)
    return main_sha, head_sha


def test_process_once_auto_merges_unlabeled_ready_pr(monkeypatch):
    """End-to-end: a fully-ready PR with NO merge-queue label is auto-merged.
    This is the core autonomy fix — no human/agent labeling required."""
    calls = {"merged": None, "updated": False}
    head_sha = "a" * 40
    _wire_ready_process_once(
        monkeypatch,
        issues=[_issue(90, labels=[])],  # NO merge-queue label
        pr_payload={
            "state": "open", "mergeable": True, "draft": False,
            "base": {"ref": "main", "repo_id": 1},
            "head": {"sha": head_sha, "repo_id": 1},
            "labels": [],
        },
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merged"] == 90  # merged despite no merge-queue label


def test_process_once_skips_opt_out_labeled_pr(monkeypatch):
    """A fully-ready PR carrying an opt-out label is NOT merged (skipped)."""
    for optout in OPT_OUT:
        calls = {"merged": None, "updated": False}
        head_sha = "a" * 40
        _wire_ready_process_once(
            monkeypatch,
            issues=[_issue(91, labels=[optout])],
            pr_payload={
                "state": "open", "mergeable": True, "draft": False,
                "base": {"ref": "main", "repo_id": 1},
                "head": {"sha": head_sha, "repo_id": 1},
                "labels": [{"name": optout}],
            },
            calls=calls,
        )
        rc = mq.process_once(dry_run=False)
        assert rc == 0
        assert calls["merged"] is None, f"{optout!r} PR must not be merged"


def test_process_once_does_not_merge_unapproved_pr(monkeypatch):
    """A not-ready PR (only one genuine approval) is auto-considered but NOT
    merged — auto-discovery does not lower the merge bar."""
    calls = {"merged": None, "updated": False}
    head_sha = "a" * 40
    main_sha, _ = _wire_ready_process_once(
        monkeypatch,
        issues=[_issue(92, labels=[])],
        pr_payload={
            "state": "open", "mergeable": True, "draft": False,
            "base": {"ref": "main", "repo_id": 1},
            "head": {"sha": head_sha, "repo_id": 1},
            "labels": [],
        },
        calls=calls,
    )
    # Only ONE genuine approval → below the required 2.
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ])

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merged"] is None


def test_process_once_does_not_merge_red_required_pr(monkeypatch):
    """A not-ready PR (required context red) is auto-considered but NOT merged."""
    calls = {"merged": None, "updated": False}
    head_sha = "a" * 40
    main_sha = "b" * 40
    _wire_ready_process_once(
        monkeypatch,
        issues=[_issue(93, labels=[])],
        pr_payload={
            "state": "open", "mergeable": True, "draft": False,
            "base": {"ref": "main", "repo_id": 1},
            "head": {"sha": head_sha, "repo_id": 1},
            "labels": [],
        },
        calls=calls,
    )

    # Required PR context is FAILURE; main stays green.
    def fake_combined(sha):
        if sha == main_sha:
            return {"state": "success",
                    "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        return {"state": "failure",
                "statuses": [{"context": "CI / all-required (pull_request)", "status": "failure"}]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merged"] is None


def test_process_once_does_not_merge_unmergeable_pr(monkeypatch):
    """A not-ready PR (mergeable False = conflicts) is auto-considered but NOT
    merged."""
    calls = {"merged": None, "updated": False}
    head_sha = "a" * 40
    _wire_ready_process_once(
        monkeypatch,
        issues=[_issue(94, labels=[])],
        pr_payload={
            "state": "open", "mergeable": False, "draft": False,
            "base": {"ref": "main", "repo_id": 1},
            "head": {"sha": head_sha, "repo_id": 1},
            "labels": [],
        },
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merged"] is None


# --------------------------------------------------------------------------
# §SOP-22 (cont.): HEAD-OF-LINE (HOL) — a non-ready auto-discovered candidate
# must NOT block the newer ready PRs behind it. The queue SCANS THROUGH the
# FIFO candidate list, skipping `wait` candidates (REQUEST_CHANGES, mergeable
# != True, insufficient genuine approvals, or red required CI), and merges the
# first ready PR in the SAME tick. (Regression for the #1519-style false
# candidate the reviewer caught: open + unlabeled + mergeable=false + current-
# head official REQUEST_CHANGES + <2 genuine approvals.)
# --------------------------------------------------------------------------

MAIN_SHA = "b" * 40


def _wire_multi_candidate_process_once(monkeypatch, *, issues, pulls, reviews, calls):
    """Wire process_once for MULTIPLE candidates, dispatching get_pull /
    get_pull_reviews / head-status BY PR NUMBER so each candidate can have a
    different readiness. `pulls` maps number -> pull payload; `reviews` maps
    number -> reviews list. Main is green; each PR head status is green."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", OPT_OUT)
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2, block_on_rejected_reviews=True,
    ))
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: MAIN_SHA)

    def fake_combined(sha):
        if sha == MAIN_SHA:
            return {"state": "success", "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        return {"state": "success", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: issues)
    monkeypatch.setattr(mq, "get_pull", lambda n: dict(pulls[n], number=n))
    # Each PR head contains current main (so no candidate needs an update; the
    # only differentiator is readiness). head sha is the pull's own head.
    monkeypatch.setattr(
        mq, "get_pull_commits",
        lambda n: [{"sha": MAIN_SHA}, {"sha": pulls[n]["head"]["sha"]}],
    )
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: reviews[n])

    def fake_merge(pr_number, *, dry_run, force=False):
        calls.setdefault("merged", [])
        calls["merged"].append(pr_number)
    monkeypatch.setattr(mq, "merge_pull", fake_merge)
    monkeypatch.setattr(mq, "update_pull", lambda *a, **k: calls.__setitem__("updated", True))
    monkeypatch.setattr(mq, "post_comment", lambda *a, **k: None)
    monkeypatch.setattr(mq, "add_label_by_name", lambda *a, **k: None)


def _two_approvals(head_sha):
    return [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ]


def test_hol_unready_oldest_does_not_block_newer_ready_pr(monkeypatch):
    """The OLDEST auto-discovered candidate is NOT ready (mergeable=false). The
    queue must SKIP it and merge the NEWER ready PR in the SAME tick — no HOL."""
    calls = {"updated": False}
    old_head, new_head = "a" * 40, "c" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(500, labels=[], created="2026-06-01T01:00:00Z"),  # oldest, NOT ready
            _issue(501, labels=[], created="2026-06-01T02:00:00Z"),  # newer, READY
        ],
        pulls={
            500: {"state": "open", "mergeable": False, "draft": False,  # conflict
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": old_head, "repo_id": 1}, "labels": []},
            501: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": new_head, "repo_id": 1}, "labels": []},
        },
        reviews={500: _two_approvals(old_head), 501: _two_approvals(new_head)},
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    # The newer ready PR merged; the non-ready oldest did not block it.
    assert calls.get("merged") == [501]


def test_hol_1519_style_false_candidate_never_merged_and_never_blocks(monkeypatch):
    """Live #1519 repro: oldest, open, UNLABELED, but mergeable=false + a
    current-head official REQUEST_CHANGES + only ONE genuine approval. It must
    NEVER be merged and must NEVER block the newer ready PR behind it."""
    calls = {"updated": False}
    false_head, ready_head = "a" * 40, "c" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(1519, labels=[], created="2026-05-20T00:00:00Z"),  # oldest false candidate
            _issue(2000, labels=[], created="2026-06-01T00:00:00Z"),  # newer, READY
        ],
        pulls={
            1519: {"state": "open", "mergeable": False, "draft": False,
                   "base": {"ref": "main", "repo_id": 1},
                   "head": {"sha": false_head, "repo_id": 1}, "labels": []},
            2000: {"state": "open", "mergeable": True, "draft": False,
                   "base": {"ref": "main", "repo_id": 1},
                   "head": {"sha": ready_head, "repo_id": 1}, "labels": []},
        },
        reviews={
            1519: [
                # one genuine approval (below 2) ...
                {"state": "APPROVED", "user": {"login": "agent-researcher"},
                 "official": True, "stale": False, "dismissed": False, "commit_id": false_head},
                # ... plus a current-head official REQUEST_CHANGES (human action needed)
                {"state": "REQUEST_CHANGES", "user": {"login": "agent-reviewer"},
                 "official": True, "stale": False, "dismissed": False, "commit_id": false_head},
            ],
            2000: _two_approvals(ready_head),
        },
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    # #1519 is never merged; the ready PR behind it merges this same tick.
    assert calls.get("merged") == [2000]
    assert 1519 not in calls.get("merged", [])


def test_hol_unready_red_required_ci_is_skipped_for_ready_pr(monkeypatch):
    """A candidate whose required CI is RED is skipped (not waited-on) so the
    newer ready PR merges in the same tick."""
    calls = {"updated": False}
    red_head, ready_head = "a" * 40, "c" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(600, labels=[], created="2026-06-01T01:00:00Z"),  # required CI red
            _issue(601, labels=[], created="2026-06-01T02:00:00Z"),  # ready
        ],
        pulls={
            600: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": red_head, "repo_id": 1}, "labels": []},
            601: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": ready_head, "repo_id": 1}, "labels": []},
        },
        reviews={600: _two_approvals(red_head), 601: _two_approvals(ready_head)},
        calls=calls,
    )
    # PR 600's required PR context is FAILURE; 601 (and main) stay green.
    def fake_combined(sha):
        if sha == MAIN_SHA:
            return {"state": "success",
                    "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        state = "failure" if sha == red_head else "success"
        return {"state": state,
                "statuses": [
                    {"context": "CI / all-required (pull_request)", "status": state},
                    {"context": "qa-review / approved (pull_request_target)", "status": "success"},
                    {"context": "security-review / approved (pull_request_target)", "status": "success"},
                    {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
                ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls.get("merged") == [601]


def test_hol_all_candidates_unready_merges_nothing(monkeypatch):
    """If EVERY candidate is non-ready, the queue merges nothing (fail-closed)
    and does not loop — it simply finds no actionable PR this tick."""
    calls = {"updated": False}
    h1, h2 = "a" * 40, "c" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(700, labels=[], created="2026-06-01T01:00:00Z"),  # RC
            _issue(701, labels=[], created="2026-06-01T02:00:00Z"),  # unmergeable
        ],
        pulls={
            700: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": h1, "repo_id": 1}, "labels": []},
            701: {"state": "open", "mergeable": False, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": h2, "repo_id": 1}, "labels": []},
        },
        reviews={
            700: _two_approvals(h1) + [
                {"state": "REQUEST_CHANGES", "user": {"login": "agent-reviewer"},
                 "official": True, "stale": False, "dismissed": False, "commit_id": h1},
            ],
            701: _two_approvals(h2),
        },
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls.get("merged") is None  # nothing merged; no HOL loop


def test_opt_out_draft_label_excludes_candidate():
    """The literal `draft` label is now an opt-out label (added to the default
    OPT_OUT_LABELS), independent of Gitea draft STATE — a human can opt a PR out
    by labeling it `draft` without converting it to a draft PR."""
    # `draft` must be in the shipped default opt-out set.
    assert "draft" in mq.OPT_OUT_LABELS
    opt_out = OPT_OUT | {"draft"}
    issues = [_issue(800, labels=["draft"], draft=False)]  # label only, not draft STATE
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=opt_out, auto_discover=True
    )
    assert selected is None


def test_choose_candidate_issues_returns_full_fifo_list_skipping_opt_out():
    """choose_candidate_issues returns ALL eligible candidates oldest-first (so
    process_once can scan past non-ready ones), skipping opt-out/draft/non-PR."""
    issues = [
        _issue(72, labels=["merge-queue"], created="2026-06-01T03:00:00Z"),
        _issue(70, labels=["do-not-auto-merge"], created="2026-06-01T01:00:00Z"),  # opt-out
        _issue(71, labels=[], created="2026-06-01T02:00:00Z"),
        _issue(73, labels=[], draft=True, created="2026-06-01T00:30:00Z"),         # draft
        _issue(74, labels=[], is_pr=False, created="2026-06-01T00:00:00Z"),        # not a PR
    ]
    ordered = mq.choose_candidate_issues(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert [i["number"] for i in ordered] == [71, 72]  # FIFO, opt-out/draft/non-PR dropped


def test_process_once_defensive_skip_when_pull_payload_opted_out(monkeypatch):
    """If the listing missed an opt-out label but the authoritative pull payload
    carries it (stale listing race), process_once must still skip the merge."""
    calls = {"merged": None, "updated": False}
    head_sha = "a" * 40
    _wire_ready_process_once(
        monkeypatch,
        issues=[_issue(95, labels=[])],  # listing shows no opt-out
        pr_payload={
            "state": "open", "mergeable": True, "draft": False,
            "base": {"ref": "main", "repo_id": 1},
            "head": {"sha": head_sha, "repo_id": 1},
            "labels": [{"name": "do-not-auto-merge"}],  # live pull is opted out
        },
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merged"] is None


# ---------------------------------------------------------------------------
# readiness-enumeration + post-batch summary
# ---------------------------------------------------------------------------

def test_enumerate_readiness_evaluates_all_candidates(monkeypatch):
    """enumerate_readiness returns every candidate's state, not stopping at
    the first actionable one."""
    old_head, new_head = "a" * 40, "c" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(500, labels=[], created="2026-06-01T01:00:00Z"),
            _issue(501, labels=[], created="2026-06-01T02:00:00Z"),
        ],
        pulls={
            500: {"state": "open", "mergeable": False, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": old_head, "repo_id": 1}, "labels": []},
            501: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": new_head, "repo_id": 1}, "labels": []},
        },
        reviews={500: _two_approvals(old_head), 501: _two_approvals(new_head)},
        calls={},
    )

    entries = mq.enumerate_readiness(dry_run=False)

    assert len(entries) == 2
    by_num = {e.pr_number: e for e in entries}
    assert by_num[500].decision is not None
    assert by_num[500].decision.ready is False
    assert by_num[501].decision is not None
    assert by_num[501].decision.ready is True


def test_enumerate_readiness_includes_ineligible_pr(monkeypatch):
    """enumerate_readiness marks fork / wrong-base PRs as ineligible
    (decision=None) while still evaluating the rest."""
    head = "a" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(600, labels=[], created="2026-06-01T01:00:00Z"),
            _issue(601, labels=[], created="2026-06-01T02:00:00Z"),
        ],
        pulls={
            600: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": head, "repo_id": 2}, "labels": []},  # fork
            601: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": head, "repo_id": 1}, "labels": []},
        },
        reviews={600: _two_approvals(head), 601: _two_approvals(head)},
        calls={},
    )

    entries = mq.enumerate_readiness(dry_run=False)

    by_num = {e.pr_number: e for e in entries}
    assert by_num[600].decision is None
    assert "not merge-eligible" in by_num[600].reason
    assert by_num[601].decision is not None
    assert by_num[601].decision.ready is True


def test_enumerate_readiness_fail_closed_on_api_error(monkeypatch):
    """If get_pull raises for one candidate, that candidate is recorded as
    unverifiable; other candidates are still evaluated."""
    head = "a" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(700, labels=[], created="2026-06-01T01:00:00Z"),
            _issue(701, labels=[], created="2026-06-01T02:00:00Z"),
        ],
        pulls={
            700: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": head, "repo_id": 1}, "labels": []},
            701: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": head, "repo_id": 1}, "labels": []},
        },
        reviews={700: _two_approvals(head), 701: _two_approvals(head)},
        calls={},
    )

    original_get_pull = mq.get_pull
    def failing_get_pull(n):
        if n == 700:
            raise mq.ApiError("simulated API failure")
        return original_get_pull(n)
    monkeypatch.setattr(mq, "get_pull", failing_get_pull)

    entries = mq.enumerate_readiness(dry_run=False)

    by_num = {e.pr_number: e for e in entries}
    assert by_num[700].decision is None
    assert "unverifiable" in by_num[700].reason
    assert by_num[701].decision is not None
    assert by_num[701].decision.ready is True


def test_print_post_batch_summary_counts_correctly(capsys):
    entries = [
        mq.ReadinessEntry(pr_number=1, decision=mq.MergeDecision(True, "merge", "ready"), reason="ready"),
        mq.ReadinessEntry(pr_number=2, decision=mq.MergeDecision(False, "wait", "CI red"), reason="CI red"),
        mq.ReadinessEntry(pr_number=3, decision=None, reason="draft"),
    ]
    mq.print_post_batch_summary(entries)
    captured = capsys.readouterr()
    out = captured.out
    assert "total_candidates=3" in out
    assert "ready=1" in out
    assert "waiting=1" in out
    assert "ineligible/unverifiable=1" in out
    assert "PR #1: state=ready" in out
    assert "PR #2: state=waiting" in out
    assert "PR #3: state=ineligible" in out


# ---------------------------------------------------------------------------
# Conductor snapshot consumption (operator-config#158 / molecule-core#2502)
# ---------------------------------------------------------------------------

import json
import os
import tempfile


def _fresh_ts():
    # Conductor snapshots are only honored within a 10-minute freshness window
    # (load_conductor_snapshot in gitea-merge-queue.py). A frozen literal ts
    # goes stale the moment wall-clock passes that window, silently dropping the
    # snapshot and self-fetching -> empty OWNER/NAME -> "/repos///" crash. Default
    # to NOW so the snapshot is fresh whenever the suite runs. Tests that want a
    # STALE snapshot pass ts= explicitly (test_load_conductor_snapshot_ignores_stale_snapshot).
    from datetime import datetime, timezone
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _make_snapshot(prs, ts=None):
    return {"ts": ts if ts is not None else _fresh_ts(),
            "repo": "molecule-ai/molecule-core", "prs": prs}


def test_list_candidate_issues_uses_snapshot_when_present(monkeypatch):
    """When CONDUCTOR_SNAPSHOT_FILE is present and fresh, list_candidate_issues
    returns the snapshot PRs instead of hitting the API."""
    snapshot = _make_snapshot([
        {"number": 10, "title": "PR 10", "head_sha": "a" * 40,
         "labels": ["merge-queue"],
         "combined_state": "success", "statuses": []},
        {"number": 20, "title": "PR 20", "head_sha": "b" * 40,
         "labels": [],
         "combined_state": "success", "statuses": []},
    ])
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        # reload so load_conductor_snapshot sees the env var
        candidates = mq.list_candidate_issues(auto_discover=True)
        assert len(candidates) == 2
        assert [c["number"] for c in candidates] == [10, 20]
    finally:
        os.unlink(path)


def test_list_queued_issues_uses_snapshot_label_filter(monkeypatch):
    """list_queued_issues (opt-IN mode) filters the snapshot by QUEUE_LABEL."""
    snapshot = _make_snapshot([
        {"number": 11, "title": "Labeled", "head_sha": "a" * 40,
         "labels": ["merge-queue"], "combined_state": "success", "statuses": []},
        {"number": 22, "title": "Unlabeled", "head_sha": "b" * 40,
         "labels": [], "combined_state": "success", "statuses": []},
    ])
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
        queued = mq.list_queued_issues()
        assert len(queued) == 1
        assert queued[0]["number"] == 11
    finally:
        os.unlink(path)


def test_get_combined_status_uses_snapshot_when_sha_matches(monkeypatch):
    """get_combined_status returns snapshot data when the SHA is an open PR head."""
    head_sha = "c" * 40
    snapshot = _make_snapshot([
        {"number": 30, "title": "PR 30", "head_sha": head_sha,
         "labels": [],
         "combined_state": "failure",
         "statuses": [
             {"context": "CI / all-required (pull_request)", "status": "failure"},
         ]},
    ])
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        combined = mq.get_combined_status(head_sha)
        assert combined["state"] == "failure"
        assert len(combined["statuses"]) == 1
        assert combined["statuses"][0]["context"] == "CI / all-required (pull_request)"
        assert combined["statuses"][0]["status"] == "failure"
    finally:
        os.unlink(path)


def test_get_combined_status_self_fetches_when_sha_not_in_snapshot(monkeypatch):
    """If the SHA is not in the snapshot, get_combined_status falls back to API."""
    snapshot = _make_snapshot([
        {"number": 40, "head_sha": "d" * 40, "labels": [],
         "combined_state": "success", "statuses": []},
    ])
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        monkeypatch.setattr(mq, "OWNER", "o")
        monkeypatch.setattr(mq, "NAME", "r")

        def fake_api(method, path, **kw):
            if path.endswith("/status"):
                return 200, {"state": "success", "statuses": [{"context": "c1", "status": "success"}]}
            if path.endswith("/statuses"):
                return 200, []
            raise mq.ApiError("unexpected")

        monkeypatch.setattr(mq, "api", fake_api)
        combined = mq.get_combined_status("e" * 40)
        assert combined["state"] == "success"
    finally:
        os.unlink(path)


def test_load_conductor_snapshot_ignores_stale_snapshot(monkeypatch):
    """A snapshot older than 10 minutes is treated as absent (self-fetch)."""
    from datetime import datetime, timezone, timedelta
    old_ts = (datetime.now(timezone.utc) - timedelta(minutes=15)).strftime("%Y-%m-%dT%H:%M:%SZ")
    snapshot = _make_snapshot([{"number": 50, "head_sha": "f" * 40, "labels": []}], ts=old_ts)
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        assert mq.load_conductor_snapshot() is None
    finally:
        os.unlink(path)


# --------------------------------------------------------------------------
# Fix 3: non-main base PRs are skipped loudly/observably, not silently.
# core#2548 stopped the per-tick PR-comment flood; the skip must still be
# observable in workflow logs and must not affect legitimate main-targeted PRs.
# --------------------------------------------------------------------------

def _make_pr(**overrides) -> dict:
    base = {
        "state": "open",
        "draft": False,
        "labels": [],
        "base": {"ref": "main", "repo_id": 1},
        "head": {"repo_id": 1, "sha": "a" * 40},
    }
    base.update(overrides)
    return base


def test_early_skip_reason_skips_non_main_base_observably():
    """A stacked PR whose base is not main is skipped with an observable reason
    and NO PR comment (the observable path is the workflow notice)."""
    pr = _make_pr(base={"ref": "feature/stacked", "repo_id": 1})
    reason, comment = mq._early_skip_reason(pr, watch_branch="main")
    assert reason == "base is not `main`"
    assert comment is None


def test_early_skip_reason_does_not_skip_main_targeted_pr():
    """A legitimate main-targeted PR is not skipped for base reasons."""
    pr = _make_pr()
    reason, comment = mq._early_skip_reason(pr, watch_branch="main")
    assert reason is None
    assert comment is None


def test_early_skip_reason_skips_fork_pr_with_comment():
    """Fork PRs are skipped AND receive a PR comment (not silent)."""
    pr = _make_pr(head={"repo_id": 2, "sha": "a" * 40})
    reason, comment = mq._early_skip_reason(pr, watch_branch="main")
    assert reason == "fork PR"
    assert comment is not None
    assert "fork PRs are not supported" in comment


def test_early_skip_reason_skips_closed_draft_and_opt_out():
    """Other early-skip classes produce observable reasons and no comment."""
    assert mq._early_skip_reason(_make_pr(state="closed"), watch_branch="main") == ("not open", None)
    assert mq._early_skip_reason(_make_pr(draft=True), watch_branch="main") == ("draft", None)
    assert mq._early_skip_reason(
        _make_pr(labels=[{"name": "do-not-auto-merge"}]), watch_branch="main"
    ) == ("opt-out label", None)


def test_evaluate_candidate_non_main_base_skips_without_comment(capsys, monkeypatch):
    """End-to-end: _evaluate_candidate skips a non-main base PR, emits a workflow
    notice, and does NOT post a PR comment."""
    pr = _make_pr(number=42, base={"ref": "feature/stacked", "repo_id": 1})
    monkeypatch.setattr(mq, "get_pull", lambda _n: pr)

    comments_posted = []
    monkeypatch.setattr(mq, "post_comment", lambda n, b, *, dry_run: comments_posted.append((n, b, dry_run)))

    decision, ctx = mq._evaluate_candidate(
        {"number": 42},
        main_sha="m" * 40,
        main_status={"state": "success", "statuses": []},
        required_contexts=[],
        required_approvals=2,
        dry_run=False,
    )

    assert decision is None
    assert ctx["pr_number"] == 42
    assert comments_posted == []
    captured = capsys.readouterr()
    assert "base is not `main`" in captured.out
    assert "skipping" in captured.out


def test_evaluate_candidate_main_base_proceeds_to_merge_bar(monkeypatch):
    """A main-targeted PR is not early-skipped; it reaches the merge-readiness
    evaluation (which, with no statuses/approvals in this minimal mock, waits)."""
    pr = _make_pr(number=99, base={"ref": "main", "repo_id": 1})
    monkeypatch.setattr(mq, "get_pull", lambda _n: pr)
    monkeypatch.setattr(mq, "get_pull_commits", lambda _n: [{"sha": "m" * 40}])
    monkeypatch.setattr(mq, "get_combined_status", lambda _sha: {"state": "success", "statuses": []})
    monkeypatch.setattr(mq, "get_pull_reviews", lambda _n: [])

    decision, ctx = mq._evaluate_candidate(
        {"number": 99},
        main_sha="m" * 40,
        main_status={"state": "success", "statuses": []},
        required_contexts=[],
        required_approvals=2,
        dry_run=False,
    )

    assert decision is not None
    assert ctx["pr_number"] == 99
    assert decision.ready is False


# =============================================================================
# Runtime-bump exemption (RFC internal#131 PR-A; spec 9c2e9c88)
# =============================================================================
# These tests pin the is_runtime_bump_exempt() predicate. Each guard and
# condition has a positive case (the rule fires) and a negative case (the
# rule does not fire because the input is missing or wrong). A happy-path
# test pins the all-conditions-pass → exempt outcome. dup-close has its
# own test for the "newer wins" semantics.
# =============================================================================


def _bump_pr(
    *,
    author: str = "bump-bot",
    head_ref: str = "runtime-bump/claude-code/v1.2.3",
    labels: list[dict] | None = None,
) -> dict:
    """Build a minimal PR dict shaped like a bump-bot runtime-bump PR."""
    return {
        "number": 1234,
        "state": "open",
        "user": {"login": author},
        "head": {"ref": head_ref, "sha": "a" * 40},
        "base": {"ref": "main"},
        "labels": labels or [],
    }


def _runtime_version_patch(
    added: str = "claude-code@v1.2.3",
    removed: str = "claude-code@v1.2.2",
) -> str:
    """Build a minimal unified-diff patch string for .runtime-version."""
    return (
        f"--- a/.runtime-version\n"
        f"+++ b/.runtime-version\n"
        f"@@ -1,1 +1,1 @@\n"
        f"-{removed}\n"
        f"+{added}\n"
    )


def _runtime_version_file(
    *,
    added: str = "claude-code@v1.2.3",
    removed: str = "claude-code@v1.2.2",
) -> dict:
    """Build a single-file .runtime-version diff entry."""
    return {
        "filename": ".runtime-version",
        "status": "modified",
        "additions": 1,
        "deletions": 1,
        "changes": 2,
        "patch": _runtime_version_patch(added=added, removed=removed),
    }


def _set_allowlist(monkeypatch, *runtimes: str) -> None:
    """Set RUNTIME_BUMP_EXEMPT_TEMPLATES for the test."""
    monkeypatch.setattr(mq, "RUNTIME_BUMP_EXEMPT_TEMPLATES", set(runtimes))


# ---- GUARD 1: author must be bump-bot ----

def test_runtime_bump_exempt_rejects_non_bump_bot_author():
    pr = _bump_pr(author="alice")
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "author=" in reason
    assert "bump-bot" in reason


def test_runtime_bump_exempt_rejects_missing_user_field():
    pr = _bump_pr()
    pr.pop("user", None)
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "author=" in reason


# ---- GUARD 2: diff must be exactly .runtime-version with 1 added + 1 removed ----

def test_runtime_bump_exempt_rejects_no_runtime_version_file():
    pr = _bump_pr()
    pr_files = [{"filename": "README.md", "patch": "--- a/README\n+++ b/README\n"}]
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=pr_files,
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert ".runtime-version" in reason
    assert "does not touch" in reason


def test_runtime_bump_exempt_rejects_multi_file_diff():
    pr = _bump_pr()
    pr_files = [
        _runtime_version_file(),
        {"filename": "go.mod", "patch": "--- a/go.mod\n+++ b/go.mod\n"},
    ]
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=pr_files,
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "1 other file" in reason


def test_runtime_bump_exempt_rejects_multi_line_runtime_version_diff():
    pr = _bump_pr()
    # Patch with 2 added lines (one of which is the version, one is blank).
    pr_files = [{
        "filename": ".runtime-version",
        "patch": (
            "--- a/.runtime-version\n"
            "+++ b/.runtime-version\n"
            "@@ -1,2 +1,2 @@\n"
            "-claude-code@v1.2.2\n"
            "-# old comment\n"
            "+claude-code@v1.2.3\n"
            "+# new comment\n"
        ),
    }]
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=pr_files,
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "2 added" in reason or "added" in reason


def test_runtime_bump_exempt_rejects_empty_patch_text():
    pr = _bump_pr()
    pr_files = [{"filename": ".runtime-version", "patch": ""}]
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=pr_files,
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "patch" in reason


# ---- GUARD 3: no active release-candidate ----

def test_runtime_bump_exempt_rejects_active_release_candidate():
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=True,
    )
    assert exempt is False
    assert "release-candidate" in reason or "rc" in reason.lower()


# ---- CONDITION 1: ==SSOT ----

def test_runtime_bump_exempt_rejects_unverifiable_latest_tag():
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag=None,
        rc_active=False,
    )
    assert exempt is False
    assert "==SSOT" in reason or "unverifiable" in reason


def test_runtime_bump_exempt_rejects_ssot_mismatch():
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="claude-code@v1.2.3", removed="claude-code@v1.2.2")],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.4",  # bump value != latest tag
        rc_active=False,
    )
    assert exempt is False
    assert "==SSOT" in reason
    # ==SSOT compares the VERSION part of the .runtime-version line
    # to the latest tag (not the full `<runtime>@<version>` string).
    assert "v1.2.3" in reason
    assert "v1.2.4" in reason


# ---- CONDITION 2a: GATE-ADEQUACY (CI side) ----

def test_runtime_bump_exempt_rejects_no_runtime_smoke_context(monkeypatch):
    # The smoke check fires AFTER the allowlist check in my implementation,
    # so to land on the smoke check we need the runtime on the allowlist.
    _set_allowlist(monkeypatch, "claude-code")
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["CI / all-required (pull_request)"],  # no runtime smoke
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "runtime-provision" in reason or "smoke" in reason


def test_runtime_bump_exempt_rejects_empty_required_contexts():
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=[],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "empty" in reason


def test_runtime_bump_exempt_accepts_case_insensitive_smoke_substring(monkeypatch):
    """The smoke-context substring match is case-INsensitive: a required
    context named `CI / RUNTIME-PROVISION-SMOKE (pull_request)` (uppercase
    "RUNTIME-PROVISION-SMOKE") should still satisfy the gate when
    RUNTIME_PROVISION_SMOKE_CONTEXTS contains the lowercase form. The
    runtime must also be on the allowlist for the exemption to actually
    fire — this test sets the allowlist so the smoke check is the one
    under test."""
    _set_allowlist(monkeypatch, "claude-code")
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="claude-code@v0.5.1", removed="claude-code@v0.5.0")],
        required_contexts=["CI / RUNTIME-PROVISION-SMOKE (pull_request)"],
        latest_runtime_v_tag="v0.5.1",
        rc_active=False,
    )
    # All guards + conditions satisfied: exempt=True. The smoke match
    # being case-insensitive is what lets the uppercase required context
    # satisfy the lowercase substring list.
    assert exempt is True, f"expected exempt (smoke match should be case-insensitive), got: {reason}"
    assert "v0.5.0" in reason
    assert "v0.5.1" in reason


# ---- CONDITION 2b: GATE-ADEQUACY (template allowlist) ----

def test_runtime_bump_exempt_rejects_runtime_not_on_allowlist(monkeypatch):
    _set_allowlist(monkeypatch, "hermes")  # claude-code not on allowlist
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "claude-code" in reason
    assert "allowlist" in reason


def test_runtime_bump_exempt_rejects_unparseable_runtime_name():
    """If the .runtime-version value is not in '<runtime>@<version>'
    format, the function cannot determine the runtime name → fail-closed."""
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="v1.2.3", removed="v1.2.2")],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "format" in reason or "<runtime>@<version>" in reason


# ---- CONDITION 3: EXCLUDE-BREAKING ----

def test_runtime_bump_exempt_rejects_semver_major(monkeypatch):
    _set_allowlist(monkeypatch, "claude-code")
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="claude-code@v2.0.0", removed="claude-code@v1.9.9")],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v2.0.0",
        rc_active=False,
    )
    assert exempt is False
    assert "MAJOR" in reason


def test_runtime_bump_exempt_rejects_breaking_label(monkeypatch):
    _set_allowlist(monkeypatch, "claude-code")
    # Use a 0.x version so the semver-MAJOR check doesn't fire first —
    # we want to specifically test the breaking-label rejection.
    pr = _bump_pr(labels=[{"name": "breaking"}])
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(
            added="claude-code@v0.5.2", removed="claude-code@v0.5.1"
        )],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v0.5.2",
        rc_active=False,
    )
    assert exempt is False
    assert "breaking" in reason.lower()


def test_runtime_bump_exempt_rejects_unparseable_semver(monkeypatch):
    _set_allowlist(monkeypatch, "claude-code")
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="claude-code@not-a-version", removed="claude-code@v1.2.2")],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="not-a-version",
        rc_active=False,
    )
    assert exempt is False
    assert "semver" in reason or "parse" in reason.lower()


# ---- Happy path: all guards + conditions pass → exempt ----

def test_runtime_bump_exempt_happy_path(monkeypatch):
    _set_allowlist(monkeypatch, "claude-code")
    # Use 0.x to avoid the semver-MAJOR exclusion.
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(
            added="claude-code@v0.5.3", removed="claude-code@v0.5.2"
        )],
        required_contexts=["CI / all-required (pull_request)", "runtime-provision-smoke"],
        latest_runtime_v_tag="v0.5.3",
        rc_active=False,
    )
    assert exempt is True, f"expected exempt, got: {reason}"
    assert "claude-code" in reason
    assert "v0.5.2" in reason
    assert "v0.5.3" in reason


def test_runtime_bump_exempt_happy_path_with_v_prefix(monkeypatch):
    """Versions with a leading 'v' (e.g. 'v1.2.3') parse correctly."""
    _set_allowlist(monkeypatch, "claude-code")
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="claude-code@v0.5.1", removed="claude-code@v0.5.0")],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v0.5.1",
        rc_active=False,
    )
    assert exempt is True, f"expected exempt, got: {reason}"


# ---- Fail-closed contracts ----

def test_runtime_bump_exempt_treats_non_dict_file_entry_as_unverifiable():
    """If the pr_files list contains a non-dict entry (e.g. None or a
    string), the structural invariant is broken → fail-closed to not-exempt."""
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(), "garbage"],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "non-dict" in reason or "unverifiable" in reason


def test_runtime_bump_exempt_treats_empty_files_as_no_diff():
    """An empty pr_files list (e.g. when the API call failed) means we
    have no .runtime-version → fail-closed to not-exempt."""
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert ".runtime-version" in reason


# ---- evaluate_merge_readiness honours runtime_bump_exempt ----

def test_evaluate_merge_readiness_skips_approvals_check_when_exempt():
    """When runtime_bump_exempt=True, required_approvals is effectively 0
    → even with 0 approvers the PR is NOT blocked on the approvals check."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(
            approvers=set(),  # zero approvers
            required_approvals=2,
            runtime_bump_exempt=True,
        )
    )
    # Approvals gate is bypassed; PR is mergeable (main + CI + mergeable=True).
    assert decision.ready is True
    assert decision.action == "merge"


def test_evaluate_merge_readiness_still_enforces_approvals_when_not_exempt():
    """When runtime_bump_exempt=False (default), zero approvers blocks."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(
            approvers=set(),
            required_approvals=2,
            # runtime_bump_exempt omitted → defaults to False
        )
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert "insufficient genuine approvals" in decision.reason


# ---- dup-close: keep newest, close older ----

def test_dup_close_superseded_bump_prs_keeps_newest_closes_older():
    """Two PRs with the same title (bump-bot republish): keep the one with
    the latest updated_at, close the older one."""
    new_pr = {
        "number": 200,
        "title": "runtime: claude-code bump to v1.2.3",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T18:00:00Z",
    }
    old_pr = {
        "number": 199,
        "title": "runtime: claude-code bump to v1.2.3",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T12:00:00Z",
    }
    closed: list[int] = []
    comments: list[tuple[int, str]] = []

    def fake_close(pr_number, *, dry_run):
        closed.append(pr_number)

    def fake_comment(pr_number, body, *, dry_run):
        comments.append((pr_number, body))

    result = mq.dup_close_superseded_bump_prs(
        [new_pr, old_pr],
        close_fn=fake_close,
        comment_fn=fake_comment,
        dry_run=False,
    )
    assert result == [199]
    assert 199 in closed
    assert 200 not in closed
    # The old PR gets a comment explaining the close.
    assert any(c[0] == 199 for c in comments)
    assert any("#200" in c[1] for c in comments)


def test_dup_close_superseded_bump_prs_no_op_on_single_pr():
    """A single bump PR is not a duplicate → no close, no comment."""
    pr = {
        "number": 300,
        "title": "runtime: claude-code bump to v1.2.4",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T18:00:00Z",
    }
    closed: list[int] = []
    comments: list[tuple[int, str]] = []

    def fake_close(pr_number, *, dry_run):
        closed.append(pr_number)

    def fake_comment(pr_number, body, *, dry_run):
        comments.append((pr_number, body))

    result = mq.dup_close_superseded_bump_prs(
        [pr],
        close_fn=fake_close,
        comment_fn=fake_comment,
        dry_run=False,
    )
    assert result == []
    assert closed == []
    assert comments == []


def test_dup_close_superseded_bump_prs_ignores_non_bump_bot_authors():
    """A non-bump-bot PR is not a candidate for dup-close (it may be a
    hand-edit and should not be auto-closed)."""
    bump_pr = {
        "number": 400,
        "title": "runtime: claude-code bump to v1.2.5",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T18:00:00Z",
    }
    hand_pr = {
        "number": 401,
        "title": "runtime: claude-code bump to v1.2.5",
        "user": {"login": "agent-dev-a"},
        "state": "open",
        "updated_at": "2026-06-14T12:00:00Z",
    }
    closed: list[int] = []

    def fake_close(pr_number, *, dry_run):
        closed.append(pr_number)

    def fake_comment(pr_number, body, *, dry_run):
        pass

    result = mq.dup_close_superseded_bump_prs(
        [bump_pr, hand_pr],
        close_fn=fake_close,
        comment_fn=fake_comment,
        dry_run=False,
    )
    assert result == []  # the hand PR is ignored, no dup to close


# ---- Helpers: _parse_bump_pr_title ----

def test_parse_bump_pr_title_format_1_runtime_dash_version():
    """Format 1: 'runtime:<name>-v<ver>' — the dash form used by some
    bump-bot configs (no space, no 'bump to' verb)."""
    assert mq._parse_bump_pr_title("runtime:claude-code-v1.2.3") == ("claude-code", "v1.2.3")
    assert mq._parse_bump_pr_title("runtime:kimi-cli-v0.5.0") == ("kimi-cli", "v0.5.0")


def test_parse_bump_pr_title_format_2_bump_to():
    """Format 2: '<name> bump to <ver>' — the verb form."""
    assert mq._parse_bump_pr_title("claude-code bump to v1.2.3") == ("claude-code", "v1.2.3")
    # Version without leading 'v' should be normalized to leading 'v'.
    assert mq._parse_bump_pr_title("claude-code bump to 1.2.3") == ("claude-code", "v1.2.3")


def test_parse_bump_pr_title_hybrid_runtime_prefix_and_bump_to():
    """Hybrid: 'runtime: <name> bump to <ver>' — the prefix is decorative;
    the body uses the verb form. The runtime name is the segment between
    the prefix and the verb, the version is what comes after 'bump to'."""
    assert mq._parse_bump_pr_title("runtime: claude-code bump to v1.2.3") == ("claude-code", "v1.2.3")


def test_parse_bump_pr_title_normalizes_version_across_formats():
    """Same (runtime, version) from different title formats MUST produce
    the same bucket key — otherwise dedup would fragment across formats
    and the auto-merge exemption would still race stale vs fresh CI."""
    a = mq._parse_bump_pr_title("runtime:claude-code-v1.2.3")
    b = mq._parse_bump_pr_title("claude-code bump to v1.2.3")
    c = mq._parse_bump_pr_title("claude-code bump to 1.2.3")
    d = mq._parse_bump_pr_title("runtime: claude-code bump to v1.2.3")
    assert a == b == c == d, (
        f"dedup key must be stable across title formats; got: "
        f"a={a} b={b} c={c} d={d}"
    )


def test_parse_bump_pr_title_unrecognized_returns_none():
    """Titles that don't match any bump-bot format return None — the
    caller treats these as singleton buckets (no spurious dedup)."""
    assert mq._parse_bump_pr_title("fix typo in docs") is None
    assert mq._parse_bump_pr_title("") is None
    assert mq._parse_bump_pr_title("runtime:") is None  # prefix only
    assert mq._parse_bump_pr_title("runtime: claude-code") is None  # no version
    assert mq._parse_bump_pr_title("claude-code bumped v1.2.3") is None  # wrong verb
    # Non-semver version is not a bump-bot format
    assert mq._parse_bump_pr_title("claude-code bump to latest") is None


def test_dup_close_superseded_bump_prs_separate_runtime_versions():
    """Two bump-bot PRs with DIFFERENT (runtime, version) tuples are not
    dupes — they land in separate buckets and are not closed. Pins
    that the parser actually distinguishes versions."""
    pr_v123 = {
        "number": 500,
        "title": "runtime: claude-code bump to v1.2.3",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T18:00:00Z",
    }
    pr_v124 = {
        "number": 501,
        "title": "runtime: claude-code bump to v1.2.4",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T18:00:00Z",
    }
    closed: list[int] = []
    def fake_close(pr_number, *, dry_run): closed.append(pr_number)
    def fake_comment(pr_number, body, *, dry_run): pass
    result = mq.dup_close_superseded_bump_prs(
        [pr_v123, pr_v124],
        close_fn=fake_close,
        comment_fn=fake_comment,
        dry_run=False,
    )
    assert result == []
    assert closed == []


# ---- Helpers: _is_active_release_candidate ----

def test_is_active_release_candidate_detects_rc_label():
    prs = [
        {
            "number": 1,
            "state": "open",
            "labels": [{"name": "release-candidate"}],
        },
    ]
    assert mq._is_active_release_candidate(prs, pr_number=999) is True


def test_is_active_release_candidate_ignores_closed_prs():
    prs = [
        {
            "number": 1,
            "state": "closed",
            "labels": [{"name": "release-candidate"}],
        },
    ]
    assert mq._is_active_release_candidate(prs, pr_number=999) is False


def test_is_active_release_candidate_ignores_self():
    prs = [
        {
            "number": 999,
            "state": "open",
            "labels": [{"name": "release-candidate"}],
        },
    ]
    # The PR being evaluated is excluded from the RC check.
    assert mq._is_active_release_candidate(prs, pr_number=999) is False


def test_is_active_release_candidate_returns_false_when_no_rc_labels():
    prs = [
        {
            "number": 1,
            "state": "open",
            "labels": [{"name": "bug"}, {"name": "enhancement"}],
        },
    ]
    assert mq._is_active_release_candidate(prs, pr_number=999) is False
