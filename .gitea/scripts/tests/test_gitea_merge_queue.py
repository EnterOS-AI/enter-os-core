import importlib.util
import sys
from pathlib import Path


SCRIPT = Path(__file__).resolve().parents[1] / "gitea-merge-queue.py"
spec = importlib.util.spec_from_file_location("gitea_merge_queue", SCRIPT)
mq = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = mq
spec.loader.exec_module(mq)


def test_latest_statuses_dedupes_by_context_newest_first():
    statuses = [
        {"context": "CI / all-required (pull_request)", "status": "failure"},
        {"context": "sop-checklist / all-items-acked (pull_request)", "state": "success"},
        {"context": "CI / all-required (pull_request)", "status": "success"},
    ]

    latest = mq.latest_statuses_by_context(statuses)

    assert latest["CI / all-required (pull_request)"]["status"] == "failure"
    assert latest["sop-checklist / all-items-acked (pull_request)"]["state"] == "success"


def test_required_contexts_green_rejects_missing_and_pending():
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {"context": "sop-checklist / all-items-acked (pull_request)", "status": "pending"},
    ])

    ok, missing_or_bad = mq.required_contexts_green(
        latest,
        [
            "CI / all-required (pull_request)",
            "sop-checklist / all-items-acked (pull_request)",
            "qa-review / approved (pull_request)",
        ],
    )

    assert ok is False
    assert missing_or_bad == [
        "sop-checklist / all-items-acked (pull_request)=pending",
        "qa-review / approved (pull_request)=missing",
    ]


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


def test_merge_decision_requires_main_green_pr_green_and_current_base():
    required = ["CI / all-required (pull_request)"]
    main_status = {"state": "success", "statuses": []}
    pr_status = {
        "state": "success",
        "statuses": [{"context": "CI / all-required (pull_request)", "status": "success"}],
    }

    decision = mq.evaluate_merge_readiness(
        main_status=main_status,
        pr_status=pr_status,
        required_contexts=required,
        pr_has_current_base=True,
    )

    assert decision.ready is True
    assert decision.action == "merge"


def test_merge_decision_updates_stale_pr_before_merge():
    decision = mq.evaluate_merge_readiness(
        main_status={"state": "success", "statuses": []},
        pr_status={"state": "success", "statuses": [{"context": "CI / all-required (pull_request)", "status": "success"}]},
        required_contexts=["CI / all-required (pull_request)"],
        pr_has_current_base=False,
    )

    assert decision.ready is False
    assert decision.action == "update"
