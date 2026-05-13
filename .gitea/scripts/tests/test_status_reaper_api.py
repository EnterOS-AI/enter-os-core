import importlib.util
import json
import pathlib
import urllib.error


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "status-reaper.py"


def load_reaper():
    spec = importlib.util.spec_from_file_location("status_reaper", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(mod)
    mod.API = "https://git.example.test/api/v1"
    mod.GITEA_TOKEN = "test-token"
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
