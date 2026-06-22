import importlib.util
import json
import pathlib
import pytest
import urllib.error


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "status-reaper.py"


def load_reaper():
    spec = importlib.util.spec_from_file_location("status_reaper", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(mod)
    mod.API = "https://git.example.test/api/v1"
    mod.GITEA_TOKEN = "fixture-token"
    mod.API_TIMEOUT_SEC = 1
    mod.API_RETRIES = 3
    mod.API_RETRY_SLEEP_SEC = 0
    return mod


class FakeResponse:
    status = 200

    def __init__(self, payload):
        self.payload = payload

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        return False

    def read(self):
        return json.dumps(self.payload).encode("utf-8")


def test_api_retries_transient_timeout(monkeypatch):
    mod = load_reaper()
    calls = {"n": 0}

    def fake_urlopen(req, timeout):
        calls["n"] += 1
        if calls["n"] == 1:
            raise TimeoutError("simulated slow Gitea API")
        return FakeResponse({"ok": True})

    monkeypatch.setattr(mod.urllib.request, "urlopen", fake_urlopen)

    status, body = mod.api("GET", "/repos/o/r/commits")

    assert status == 200
    assert body == {"ok": True}
    assert calls["n"] == 2


def test_api_raises_after_retry_budget(monkeypatch):
    mod = load_reaper()

    def fake_urlopen(req, timeout):
        raise urllib.error.URLError("connection reset")

    monkeypatch.setattr(mod.urllib.request, "urlopen", fake_urlopen)

    try:
        mod.api("GET", "/repos/o/r/commits")
    except mod.ApiError as exc:
        assert "failed after 3 attempts" in str(exc)
    else:
        raise AssertionError("expected ApiError")


def test_reap_compensates_failed_pr_context_when_push_equivalent_passed(monkeypatch):
    mod = load_reaper()
    posted = []

    def fake_post(sha, context, target_url, *, description="", dry_run=False):
        posted.append((sha, context, target_url, description, dry_run))

    monkeypatch.setattr(mod, "post_compensating_status", fake_post)

    counters = mod.reap(
        {"CI": True, "Handlers Postgres Integration": True},
        {
            "statuses": [
                {
                    "context": "CI / Platform (Go) (pull_request)",
                    "status": "failure",
                    "target_url": "https://git.example.test/ci-pr",
                },
                {
                    "context": "CI / Platform (Go) (push)",
                    "status": "success",
                },
                {
                    "context": (
                        "Handlers Postgres Integration / "
                        "Handlers Postgres Integration (pull_request)"
                    ),
                    "status": "failure",
                    "target_url": "https://git.example.test/handlers-pr",
                },
                {
                    "context": (
                        "Handlers Postgres Integration / "
                        "Handlers Postgres Integration (push)"
                    ),
                    "status": "success",
                },
            ],
        },
        "db3b7a93e31adc0cb072a6d177d92dd73275a191",
    )

    assert counters["compensated_pr_shadowed_by_push_success"] == 2
    assert posted == [
        (
            "db3b7a93e31adc0cb072a6d177d92dd73275a191",
            "CI / Platform (Go) (pull_request)",
            "https://git.example.test/ci-pr",
            mod.PR_SHADOW_COMPENSATION_DESCRIPTION,
            False,
        ),
        (
            "db3b7a93e31adc0cb072a6d177d92dd73275a191",
            "Handlers Postgres Integration / Handlers Postgres Integration (pull_request)",
            "https://git.example.test/handlers-pr",
            mod.PR_SHADOW_COMPENSATION_DESCRIPTION,
            False,
        ),
    ]


def test_reap_preserves_failed_pr_context_without_push_success(monkeypatch):
    mod = load_reaper()
    posted = []
    monkeypatch.setattr(
        mod,
        "post_compensating_status",
        lambda sha, context, target_url, *, description="", dry_run=False: posted.append(
            context
        ),
    )

    counters = mod.reap(
        {"CI": True},
        {
            "statuses": [
                {
                    "context": "CI / Platform (Go) (pull_request)",
                    "status": "failure",
                },
                {
                    "context": "CI / Platform (Go) (push)",
                    "status": "failure",
                },
                {
                    "context": "CI / Shellcheck (pull_request)",
                    "status": "failure",
                },
            ],
        },
        "db3b7a93e31adc0cb072a6d177d92dd73275a191",
    )

    assert counters["preserved_pr_without_push_success"] == 2
    assert posted == []


# ---------------------------------------------------------------------------
# Conductor snapshot consumption (operator-config#158 / molecule-core#2502)
# ---------------------------------------------------------------------------

import os
import tempfile
from datetime import datetime, timezone


def _fresh_ts():
    # See test_gitea_merge_queue._fresh_ts: snapshots are only honored within a
    # 10-minute freshness window; a frozen literal ts goes stale and triggers a
    # self-fetch -> "/repos///" crash. Default to NOW.
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def test_get_combined_status_uses_snapshot_when_sha_matches(monkeypatch):
    """When the SHA is an open PR head in the conductor snapshot, get_combined_status
    returns the snapshot data instead of calling the API."""
    mod = load_reaper()
    head_sha = "a" * 40
    snapshot = {
        "ts": _fresh_ts(),
        "repo": "molecule-ai/molecule-core",
        "prs": [
            {
                "number": 99,
                "title": "PR 99",
                "head_sha": head_sha,
                "labels": [],
                "combined_state": "failure",
                "statuses": [
                    {"context": "CI / Platform (Go) (push)", "status": "failure"},
                ],
            }
        ],
    }
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        import importlib
        mod = load_reaper()  # reload to pick up env var
        combined = mod.get_combined_status(head_sha)
        assert combined["state"] == "failure"
        assert len(combined["statuses"]) == 1
        assert combined["statuses"][0]["context"] == "CI / Platform (Go) (push)"
    finally:
        os.unlink(path)


def test_get_combined_status_self_fetches_when_sha_not_in_snapshot(monkeypatch):
    """If the SHA is not in the snapshot, get_combined_status falls back to API."""
    mod = load_reaper()
    snapshot = {
        "ts": _fresh_ts(),
        "repo": "molecule-ai/molecule-core",
        "prs": [
            {"number": 1, "head_sha": "b" * 40, "labels": [],
             "combined_state": "success", "statuses": []},
        ],
    }
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        import importlib
        mod = load_reaper()

        def fake_api(method, path, **kw):
            if path.endswith("/status"):
                return 200, {"state": "success", "statuses": []}
            raise mod.ApiError("unexpected")

        monkeypatch.setattr(mod, "api", fake_api)
        combined = mod.get_combined_status("c" * 40)
        assert combined["state"] == "success"
    finally:
        os.unlink(path)


def test_reap_compensates_governance_shadow_when_target_passed(monkeypatch):
    mod = load_reaper()
    posted = []

    def fake_post(sha, context, target_url, *, description="", dry_run=False):
        posted.append((sha, context, target_url, description, dry_run))

    monkeypatch.setattr(mod, "post_compensating_status", fake_post)

    # sop-checklist has no push trigger, so its failed (pull_request) shadow is
    # noise when the required (pull_request_target) context is green.
    counters = mod.reap(
        {"sop-checklist": False, "qa-review": False, "security-review": False},
        {
            "statuses": [
                {
                    "context": "sop-checklist / all-items-acked (pull_request)",
                    "status": "failure",
                    "target_url": "https://git.example.test/sop-pr",
                },
                {
                    "context": "sop-checklist / all-items-acked (pull_request_target)",
                    "status": "success",
                },
                {
                    "context": "qa-review / approved (pull_request_review)",
                    "status": "failure",
                    "target_url": "https://git.example.test/qa-pr-review",
                },
                {
                    "context": "qa-review / approved (pull_request_target)",
                    "status": "success",
                },
            ],
        },
        "db3b7a93e31adc0cb072a6d177d92dd73275a191",
    )

    assert counters["compensated_governance_shadow"] == 2
    assert counters["preserved_governance_without_target_success"] == 0
    assert posted == [
        (
            "db3b7a93e31adc0cb072a6d177d92dd73275a191",
            "sop-checklist / all-items-acked (pull_request)",
            "https://git.example.test/sop-pr",
            mod.GOVERNANCE_SHADOW_COMPENSATION_DESCRIPTION,
            False,
        ),
        (
            "db3b7a93e31adc0cb072a6d177d92dd73275a191",
            "qa-review / approved (pull_request_review)",
            "https://git.example.test/qa-pr-review",
            mod.GOVERNANCE_SHADOW_COMPENSATION_DESCRIPTION,
            False,
        ),
    ]


def test_reap_preserves_governance_shadow_when_target_missing_or_failed(monkeypatch):
    mod = load_reaper()
    posted = []
    monkeypatch.setattr(
        mod,
        "post_compensating_status",
        lambda sha, context, target_url, *, description="", dry_run=False: posted.append(
            context
        ),
    )

    counters = mod.reap(
        {"sop-checklist": False},
        {
            "statuses": [
                {
                    "context": "sop-checklist / all-items-acked (pull_request)",
                    "status": "failure",
                },
                # target context failed → preserve the shadow as a real signal.
                {
                    "context": "sop-checklist / all-items-acked (pull_request_target)",
                    "status": "failure",
                },
            ],
        },
        "db3b7a93e31adc0cb072a6d177d92dd73275a191",
    )

    assert counters["compensated_governance_shadow"] == 0
    assert counters["preserved_governance_without_target_success"] == 1
    assert posted == []


def test_reap_preserves_ci_pull_request_failure_even_when_target_passed(monkeypatch):
    mod = load_reaper()
    posted = []
    monkeypatch.setattr(
        mod,
        "post_compensating_status",
        lambda sha, context, target_url, *, description="", dry_run=False: posted.append(
            context
        ),
    )

    # A CI workflow that also has a push trigger is NOT a governance shadow;
    # its (pull_request) failure is an independent gate signal and must be
    # preserved even if a (pull_request_target) variant happens to be green.
    counters = mod.reap(
        {"CI": True},
        {
            "statuses": [
                {
                    "context": "CI / Platform (Go) (pull_request)",
                    "status": "failure",
                },
                {
                    "context": "CI / Platform (Go) (pull_request_target)",
                    "status": "success",
                },
            ],
        },
        "db3b7a93e31adc0cb072a6d177d92dd73275a191",
    )

    assert counters["compensated_governance_shadow"] == 0
    assert counters["preserved_pr_without_push_success"] == 1
    assert posted == []


def test_reap_preserves_non_governance_no_push_shadow_when_target_passed(monkeypatch):
    mod = load_reaper()
    posted = []
    monkeypatch.setattr(
        mod,
        "post_compensating_status",
        lambda sha, context, target_url, *, description="", dry_run=False: posted.append(
            context
        ),
    )

    # A no-push workflow that is NOT in the governance allowlist must be
    # preserved even when its (pull_request_target) variant is green.
    counters = mod.reap(
        {"custom-audit": False},
        {
            "statuses": [
                {
                    "context": "custom-audit / check (pull_request_review)",
                    "status": "failure",
                },
                {
                    "context": "custom-audit / check (pull_request_target)",
                    "status": "success",
                },
            ],
        },
        "db3b7a93e31adc0cb072a6d177d92dd73275a191",
    )

    assert counters["compensated_governance_shadow"] == 0
    assert counters["compensated"] == 0
    assert posted == []


def test_reap_compensates_retired_sop_tier_check_shadow_when_target_passed(monkeypatch):
    mod = load_reaper()
    posted = []

    def fake_post(sha, context, target_url, *, description="", dry_run=False):
        posted.append((sha, context, target_url, description, dry_run))

    monkeypatch.setattr(mod, "post_compensating_status", fake_post)

    counters = mod.reap(
        {"sop-tier-check": False},
        {
            "statuses": [
                {
                    "context": "sop-tier-check / tier-verify (pull_request)",
                    "status": "failure",
                    "target_url": "https://git.example.test/tier-pr",
                },
                {
                    "context": "sop-tier-check / tier-verify (pull_request_target)",
                    "status": "success",
                },
            ],
        },
        "db3b7a93e31adc0cb072a6d177d92dd73275a191",
    )

    assert counters["compensated_governance_shadow"] == 1
    assert counters["preserved_governance_without_target_success"] == 0
    assert posted == [
        (
            "db3b7a93e31adc0cb072a6d177d92dd73275a191",
            "sop-tier-check / tier-verify (pull_request)",
            "https://git.example.test/tier-pr",
            mod.GOVERNANCE_SHADOW_COMPENSATION_DESCRIPTION,
            False,
        ),
    ]


def test_reap_compensates_retired_sop_tier_check_when_missing_from_trigger_map(monkeypatch):
    """The retired sop-tier-check workflow file is intentionally removed, so the
    real workflow trigger map will not contain it. It must still be compensatable
    because it is explicitly allowlisted as a retired governance shadow."""
    mod = load_reaper()
    posted = []

    def fake_post(sha, context, target_url, *, description="", dry_run=False):
        posted.append((sha, context, target_url, description, dry_run))

    monkeypatch.setattr(mod, "post_compensating_status", fake_post)

    counters = mod.reap(
        # Deliberately omit sop-tier-check from the trigger map.
        {},
        {
            "statuses": [
                {
                    "context": "sop-tier-check / tier-verify (pull_request)",
                    "status": "failure",
                    "target_url": "https://git.example.test/tier-pr",
                },
                {
                    "context": "sop-tier-check / tier-verify (pull_request_target)",
                    "status": "success",
                },
            ],
        },
        "db3b7a93e31adc0cb072a6d177d92dd73275a191",
    )

    assert counters["compensated_governance_shadow"] == 1
    assert counters["preserved_governance_without_target_success"] == 0
    assert posted == [
        (
            "db3b7a93e31adc0cb072a6d177d92dd73275a191",
            "sop-tier-check / tier-verify (pull_request)",
            "https://git.example.test/tier-pr",
            mod.GOVERNANCE_SHADOW_COMPENSATION_DESCRIPTION,
            False,
        ),
    ]


def test_reap_preserves_active_governance_shadow_when_missing_from_trigger_map(monkeypatch):
    """Active governance workflows must be explicitly known-no-push in the trigger
    map. If the parser/discovery misses them, the reaper must fail-closed and
    preserve their shadow rather than auto-green it."""
    mod = load_reaper()
    posted = []
    monkeypatch.setattr(
        mod,
        "post_compensating_status",
        lambda sha, context, target_url, *, description="", dry_run=False: posted.append(
            context
        ),
    )

    counters = mod.reap(
        # Deliberately omit qa-review from the trigger map.
        {},
        {
            "statuses": [
                {
                    "context": "qa-review / approved (pull_request_review)",
                    "status": "failure",
                },
                {
                    "context": "qa-review / approved (pull_request_target)",
                    "status": "success",
                },
            ],
        },
        "db3b7a93e31adc0cb072a6d177d92dd73275a191",
    )

    assert counters["compensated_governance_shadow"] == 0
    assert counters["compensated"] == 0
    assert posted == []


@pytest.mark.parametrize(
    "context",
    [
        "gate-check-v3 / gate (pull_request)",
        "reserved-path-review / check (pull_request_review)",
        "lint-required-no-paths / lint (pull_request)",
        "lint-required-context-exists-in-bp / lint (pull_request_review)",
        "audit-force-merge / audit (pull_request)",
        "status-reaper / reap (pull_request_review)",
        "umbrella-reaper / reap (pull_request)",
    ],
)
def test_reap_preserves_named_non_governance_no_push_shadows(context, monkeypatch):
    """Real merge-control/lint/audit workflows that are NOT in the governance
    allowlist must be preserved even when they have no push trigger and their
    (pull_request_target) variant is green. Auto-greening these would mask real
    failures."""
    mod = load_reaper()
    posted = []
    monkeypatch.setattr(
        mod,
        "post_compensating_status",
        lambda sha, context, target_url, *, description="", dry_run=False: posted.append(
            context
        ),
    )

    workflow_name = context.split(" / ", 1)[0]
    counters = mod.reap(
        {workflow_name: False},
        {
            "statuses": [
                {
                    "context": context,
                    "status": "failure",
                },
                {
                    "context": context.replace(
                        " (pull_request)", " (pull_request_target)"
                    ).replace(" (pull_request_review)", " (pull_request_target)"),
                    "status": "success",
                },
            ],
        },
        "db3b7a93e31adc0cb072a6d177d92dd73275a191",
    )

    assert counters["compensated_governance_shadow"] == 0
    assert counters["compensated"] == 0
    assert posted == []
