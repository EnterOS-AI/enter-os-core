import importlib.util
import json
import pathlib
import urllib.error


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "umbrella-reaper.py"


def load_reaper():
    spec = importlib.util.spec_from_file_location("umbrella_reaper", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(mod)
    mod.API = "https://git.example.test/api/v1"
    mod.GITEA_TOKEN = "fixture-token"
    mod.GITEA_HOST = "git.example.test"
    mod.REPO = "owner/repo"
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


def _pr_fixture(number: int, sha: str) -> dict:
    return {"number": number, "head": {"sha": sha}}


def _status_entry(context: str, state: str) -> dict:
    return {"context": context, "status": state}


def test_process_pr_compensates_when_all_sub_jobs_success(monkeypatch):
    mod = load_reaper()
    posted = []

    def fake_post_status(sha, context, description):
        posted.append((sha, context, description))

    monkeypatch.setattr(mod, "post_status", fake_post_status)
    monkeypatch.setattr(
        mod,
        "REQUIRED_SUB_JOBS",
        [
            "CI / Detect changes (pull_request)",
            "CI / Platform (Go) (pull_request)",
        ],
    )

    pr = _pr_fixture(1, "abc123")

    def fake_combined_status(sha):
        return {
            "statuses": [
                _status_entry("CI / all-required (pull_request)", "failure"),
                _status_entry("CI / Detect changes (pull_request)", "success"),
                _status_entry("CI / Platform (Go) (pull_request)", "success"),
            ]
        }

    monkeypatch.setattr(mod, "get_combined_status", fake_combined_status)

    ok = mod.process_pr(pr)
    assert ok is True
    assert len(posted) == 1
    assert posted[0][0] == "abc123"
    assert posted[0][1] == "CI / all-required (pull_request)"
    assert "Compensating status" in posted[0][2]


def test_process_pr_skips_when_umbrella_missing(monkeypatch):
    mod = load_reaper()
    posted = []
    monkeypatch.setattr(mod, "post_status", lambda *a, **k: posted.append(a))
    monkeypatch.setattr(mod, "REQUIRED_SUB_JOBS", ["CI / Platform (Go) (pull_request)"])

    pr = _pr_fixture(2, "def456")

    def fake_combined_status(sha):
        return {
            "statuses": [
                _status_entry("CI / Platform (Go) (pull_request)", "success"),
            ]
        }

    monkeypatch.setattr(mod, "get_combined_status", fake_combined_status)

    ok = mod.process_pr(pr)
    assert ok is True
    assert posted == []


def test_process_pr_skips_when_sub_job_pending(monkeypatch):
    mod = load_reaper()
    posted = []
    monkeypatch.setattr(mod, "post_status", lambda *a, **k: posted.append(a))
    monkeypatch.setattr(
        mod,
        "REQUIRED_SUB_JOBS",
        [
            "CI / Detect changes (pull_request)",
            "CI / Platform (Go) (pull_request)",
        ],
    )

    pr = _pr_fixture(3, "ghi789")

    def fake_combined_status(sha):
        return {
            "statuses": [
                _status_entry("CI / all-required (pull_request)", "failure"),
                _status_entry("CI / Detect changes (pull_request)", "success"),
                _status_entry("CI / Platform (Go) (pull_request)", "pending"),
            ]
        }

    monkeypatch.setattr(mod, "get_combined_status", fake_combined_status)

    ok = mod.process_pr(pr)
    assert ok is True
    assert posted == []


def test_process_pr_skips_when_sub_job_failure(monkeypatch):
    mod = load_reaper()
    posted = []
    monkeypatch.setattr(mod, "post_status", lambda *a, **k: posted.append(a))
    monkeypatch.setattr(
        mod,
        "REQUIRED_SUB_JOBS",
        [
            "CI / Detect changes (pull_request)",
            "CI / Platform (Go) (pull_request)",
        ],
    )

    pr = _pr_fixture(4, "jkl012")

    def fake_combined_status(sha):
        return {
            "statuses": [
                _status_entry("CI / all-required (pull_request)", "failure"),
                _status_entry("CI / Detect changes (pull_request)", "success"),
                _status_entry("CI / Platform (Go) (pull_request)", "failure"),
            ]
        }

    monkeypatch.setattr(mod, "get_combined_status", fake_combined_status)

    ok = mod.process_pr(pr)
    assert ok is True
    assert posted == []


def test_process_pr_returns_false_on_post_failure(monkeypatch):
    mod = load_reaper()

    def fake_post_status(sha, context, description):
        raise mod.ApiError("POST /statuses/abc123 -> HTTP 500: simulated failure")

    monkeypatch.setattr(mod, "post_status", fake_post_status)
    monkeypatch.setattr(
        mod,
        "REQUIRED_SUB_JOBS",
        [
            "CI / Detect changes (pull_request)",
            "CI / Platform (Go) (pull_request)",
        ],
    )

    pr = _pr_fixture(5, "abc123")

    def fake_combined_status(sha):
        return {
            "statuses": [
                _status_entry("CI / all-required (pull_request)", "failure"),
                _status_entry("CI / Detect changes (pull_request)", "success"),
                _status_entry("CI / Platform (Go) (pull_request)", "success"),
            ]
        }

    monkeypatch.setattr(mod, "get_combined_status", fake_combined_status)

    ok = mod.process_pr(pr)
    assert ok is False


def test_main_exits_nonzero_when_any_post_fails(monkeypatch):
    mod = load_reaper()

    monkeypatch.setenv("GITEA_TOKEN", "fixture-token")
    monkeypatch.setenv("GITEA_HOST", "git.example.test")
    monkeypatch.setenv("REPO", "owner/repo")

    monkeypatch.setattr(
        mod,
        "REQUIRED_SUB_JOBS",
        [
            "CI / Detect changes (pull_request)",
            "CI / Platform (Go) (pull_request)",
        ],
    )
    monkeypatch.setattr(
        mod,
        "list_open_prs",
        lambda limit: [
            _pr_fixture(1, "abc123"),
            _pr_fixture(2, "def456"),
        ],
    )

    calls = {"n": 0}

    def fake_combined_status(sha):
        return {
            "statuses": [
                _status_entry("CI / all-required (pull_request)", "failure"),
                _status_entry("CI / Detect changes (pull_request)", "success"),
                _status_entry("CI / Platform (Go) (pull_request)", "success"),
            ]
        }

    monkeypatch.setattr(mod, "get_combined_status", fake_combined_status)

    def fake_post_status(sha, context, description):
        calls["n"] += 1
        if calls["n"] == 2:
            raise mod.ApiError("simulated failure")

    monkeypatch.setattr(mod, "post_status", fake_post_status)

    exit_code = mod.main()
    assert exit_code == 1


def test_main_exits_zero_when_all_posts_succeed(monkeypatch):
    mod = load_reaper()

    monkeypatch.setenv("GITEA_TOKEN", "fixture-token")
    monkeypatch.setenv("GITEA_HOST", "git.example.test")
    monkeypatch.setenv("REPO", "owner/repo")

    monkeypatch.setattr(
        mod,
        "REQUIRED_SUB_JOBS",
        [
            "CI / Detect changes (pull_request)",
            "CI / Platform (Go) (pull_request)",
        ],
    )
    monkeypatch.setattr(
        mod,
        "list_open_prs",
        lambda limit: [_pr_fixture(1, "abc123")],
    )

    def fake_combined_status(sha):
        return {
            "statuses": [
                _status_entry("CI / all-required (pull_request)", "failure"),
                _status_entry("CI / Detect changes (pull_request)", "success"),
                _status_entry("CI / Platform (Go) (pull_request)", "success"),
            ]
        }

    monkeypatch.setattr(mod, "get_combined_status", fake_combined_status)
    monkeypatch.setattr(mod, "post_status", lambda *a, **k: None)

    exit_code = mod.main()
    assert exit_code == 0


def test_dry_run_does_not_post(monkeypatch):
    mod = load_reaper()
    api_calls = []

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        api_calls.append((method, path, body))
        return 200, {"ok": True}

    monkeypatch.setattr(mod, "api", fake_api)
    monkeypatch.setattr(
        mod,
        "REQUIRED_SUB_JOBS",
        [
            "CI / Detect changes (pull_request)",
            "CI / Platform (Go) (pull_request)",
        ],
    )

    pr = _pr_fixture(6, "mno345")

    def fake_combined_status(sha):
        return {
            "statuses": [
                _status_entry("CI / all-required (pull_request)", "failure"),
                _status_entry("CI / Detect changes (pull_request)", "success"),
                _status_entry("CI / Platform (Go) (pull_request)", "success"),
            ]
        }

    monkeypatch.setattr(mod, "get_combined_status", fake_combined_status)
    monkeypatch.setattr(mod, "DRY_RUN", True)

    ok = mod.process_pr(pr)
    assert ok is True
    # DRY_RUN should prevent the POST /statuses call
    assert not any(
        method == "POST" and "/statuses/" in path for method, path, _ in api_calls
    )


def test_duplicate_contexts_use_latest_state(monkeypatch):
    mod = load_reaper()
    posted = []
    monkeypatch.setattr(mod, "post_status", lambda *a, **k: posted.append(a))
    monkeypatch.setattr(
        mod,
        "REQUIRED_SUB_JOBS",
        [
            "CI / Detect changes (pull_request)",
        ],
    )

    pr = _pr_fixture(7, "pqr678")

    def fake_combined_status(sha):
        return {
            "statuses": [
                _status_entry("CI / all-required (pull_request)", "failure"),
                # duplicate: first pending, then success — the loop overwrites
                _status_entry("CI / Detect changes (pull_request)", "pending"),
                _status_entry("CI / Detect changes (pull_request)", "success"),
            ]
        }

    monkeypatch.setattr(mod, "get_combined_status", fake_combined_status)

    ok = mod.process_pr(pr)
    assert ok is True
    assert len(posted) == 1


def test_load_required_sub_jobs_from_ci_yml_pull_request_event():
    mod = load_reaper()
    # UMBRELLA_CONTEXT defaults to pull_request, so derivation should yield
    # the pull_request suffix.
    jobs = mod._load_required_sub_jobs_from_ci_yml(".gitea/workflows")
    assert all(j.endswith(" (pull_request)") for j in jobs)
    assert "CI / Detect changes (pull_request)" in jobs
    assert "CI / Python Lint & Test (pull_request)" in jobs


def test_load_required_sub_jobs_from_ci_yml_push_event(monkeypatch):
    mod = load_reaper()
    monkeypatch.setattr(mod, "UMBRELLA_CONTEXT", "CI / all-required (push)")
    jobs = mod._load_required_sub_jobs_from_ci_yml(".gitea/workflows")
    assert all(j.endswith(" (push)") for j in jobs)
    assert "CI / Detect changes (push)" in jobs


def test_list_open_prs_paginates(monkeypatch):
    mod = load_reaper()
    calls = []

    def fake_api(method, path, *, body=None, query=None, expect_json=True):
        calls.append(query)
        page = int(query.get("page", 1))
        limit = int(query.get("limit", 50))
        if page == 1:
            return 200, [{"number": 1}, {"number": 2}]
        if page == 2:
            return 200, [{"number": 3}]
        return 200, []

    monkeypatch.setattr(mod, "api", fake_api)
    prs = mod.list_open_prs(limit=2)
    assert len(prs) == 3
    assert prs[0]["number"] == 1
    assert prs[2]["number"] == 3
    assert calls[0]["page"] == "1"
    assert calls[1]["page"] == "2"


def test_process_pr_returns_false_on_status_fetch_failure(monkeypatch):
    mod = load_reaper()

    def fake_get_combined_status(sha):
        raise mod.ApiError("GET /statuses/abc123 -> HTTP 500: simulated outage")

    monkeypatch.setattr(mod, "get_combined_status", fake_get_combined_status)
    monkeypatch.setattr(
        mod,
        "REQUIRED_SUB_JOBS",
        ["CI / Detect changes (pull_request)"],
    )

    pr = _pr_fixture(8, "abc123")
    ok = mod.process_pr(pr)
    assert ok is False


def test_process_pr_returns_false_on_missing_statuses_array(monkeypatch):
    mod = load_reaper()

    def fake_get_combined_status(sha):
        return {"state": "success"}  # missing 'statuses' array

    monkeypatch.setattr(mod, "get_combined_status", fake_get_combined_status)
    monkeypatch.setattr(
        mod,
        "REQUIRED_SUB_JOBS",
        ["CI / Detect changes (pull_request)"],
    )

    pr = _pr_fixture(9, "def456")
    ok = mod.process_pr(pr)
    assert ok is False


def test_main_exits_nonzero_when_any_status_read_fails(monkeypatch):
    mod = load_reaper()

    monkeypatch.setenv("GITEA_TOKEN", "fixture-token")
    monkeypatch.setenv("GITEA_HOST", "git.example.test")
    monkeypatch.setenv("REPO", "owner/repo")

    monkeypatch.setattr(
        mod,
        "REQUIRED_SUB_JOBS",
        [
            "CI / Detect changes (pull_request)",
            "CI / Platform (Go) (pull_request)",
        ],
    )
    monkeypatch.setattr(
        mod,
        "list_open_prs",
        lambda limit: [
            _pr_fixture(1, "abc123"),
            _pr_fixture(2, "def456"),
        ],
    )

    def fake_combined_status(sha):
        if sha == "abc123":
            return {
                "statuses": [
                    _status_entry("CI / all-required (pull_request)", "failure"),
                    _status_entry("CI / Detect changes (pull_request)", "success"),
                    _status_entry("CI / Platform (Go) (pull_request)", "success"),
                ]
            }
        raise mod.ApiError("simulated status fetch failure")

    monkeypatch.setattr(mod, "get_combined_status", fake_combined_status)
    monkeypatch.setattr(mod, "post_status", lambda *a, **k: None)

    exit_code = mod.main()
    assert exit_code == 1
