"""Tests for `.gitea/scripts/detect-changes.py`."""

from __future__ import annotations

import importlib.util
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[1]
SCRIPT = REPO_ROOT / ".gitea" / "scripts" / "detect-changes.py"


def load_module():
    spec = importlib.util.spec_from_file_location("detect_changes", SCRIPT)
    assert spec is not None
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


def test_ci_profile_classifies_surfaces():
    mod = load_module()

    assert mod.classify("ci", ["workspace-server/internal/handlers/a2a_proxy.go"]) == {
        "platform": True,
        "canvas": False,
        "python": False,
        "scripts": False,
    }
    assert mod.classify("ci", ["canvas/src/app/page.tsx"]) == {
        "platform": False,
        "canvas": True,
        "python": False,
        "scripts": False,
    }
    assert mod.classify("ci", ["tests/e2e/test_model_slug.sh"]) == {
        "platform": False,
        "canvas": False,
        "python": False,
        "scripts": True,
    }
    assert mod.classify("ci", [".gitea/workflows/ci.yml", "README.md"]) == {
        "platform": False,
        "canvas": False,
        "python": False,
        "scripts": False,
    }


def test_handlers_postgres_profile_is_narrower_than_workspace_server():
    mod = load_module()

    assert mod.classify("handlers-postgres", ["workspace-server/internal/handlers/a2a_proxy.go"]) == {
        "handlers": True,
    }
    assert mod.classify("handlers-postgres", ["workspace-server/internal/provisioner/provisioner.go"]) == {
        "handlers": False,
    }


def test_e2e_api_profile_covers_api_inputs():
    mod = load_module()

    assert mod.classify("e2e-api", ["workspace-server/internal/handlers/workspace.go"]) == {
        "api": True,
    }
    assert mod.classify("e2e-api", ["tests/e2e/test_api.sh"]) == {"api": True}
    assert mod.classify("e2e-api", ["canvas/src/app/page.tsx"]) == {"api": False}


def test_fail_open_all_true_for_missing_base():
    mod = load_module()

    assert mod.all_true("ci") == {
        "platform": True,
        "canvas": True,
        "python": True,
        "scripts": True,
    }


def test_fetch_base_prefers_advertised_base_ref(monkeypatch):
    mod = load_module()
    calls: list[list[str]] = []
    exists_checks = 0

    def fake_base_exists(base: str) -> bool:
        nonlocal exists_checks
        exists_checks += 1
        return exists_checks >= 1

    def fake_run_git(args: list[str], *, timeout: int = 30):
        calls.append(args)

        class Result:
            returncode = 0
            stdout = ""
            stderr = ""

        return Result()

    monkeypatch.setattr(mod, "base_exists", fake_base_exists)
    monkeypatch.setattr(mod, "run_git", fake_run_git)

    mod.fetch_base("abc123", "main")

    assert calls == [["fetch", "--depth=1", "origin", "main"]]


def test_fetch_base_falls_back_to_sha_when_ref_fetch_does_not_materialize(monkeypatch):
    mod = load_module()
    calls: list[list[str]] = []

    monkeypatch.setattr(mod, "base_exists", lambda _base: False)

    def fake_run_git(args: list[str], *, timeout: int = 30):
        calls.append(args)

        class Result:
            returncode = 0
            stdout = ""
            stderr = ""

        return Result()

    monkeypatch.setattr(mod, "run_git", fake_run_git)

    mod.fetch_base("abc123", "main")

    assert calls == [
        ["fetch", "--depth=1", "origin", "main"],
        ["fetch", "--depth=1", "origin", "abc123"],
    ]


def test_changed_paths_uses_merge_base_for_pull_request(monkeypatch):
    mod = load_module()
    calls: list[list[str]] = []

    def fake_run_git(args: list[str], *, timeout: int = 30):
        calls.append(args)

        class Result:
            returncode = 0
            stdout = "workspace/agent.py\n"
            stderr = ""

        if args[0] == "merge-base":
            Result.stdout = "merge123\n"
        return Result()

    monkeypatch.setattr(mod, "run_git", fake_run_git)

    assert mod.changed_paths("base123", use_merge_base=True) == ["workspace/agent.py"]
    assert calls == [
        ["merge-base", "base123", "HEAD"],
        ["diff", "--name-only", "merge123", "HEAD"],
    ]


def test_detect_deepens_base_ref_when_pr_merge_base_missing(monkeypatch):
    mod = load_module()
    calls: list[tuple[str, str | None]] = []
    merge_base_calls = 0

    monkeypatch.setattr(mod, "base_exists", lambda _base: True)

    def fake_merge_base(base: str):
        nonlocal merge_base_calls
        merge_base_calls += 1
        if merge_base_calls == 1:
            return None
        return "merge123"

    def fake_deepen_base_ref(base_ref: str):
        calls.append(("deepen", base_ref))

    def fake_changed_paths(base: str, *, use_merge_base: bool):
        calls.append(("changed", str(use_merge_base)))
        return [".gitea/workflows/ci.yml"]

    monkeypatch.setattr(mod, "merge_base", fake_merge_base)
    monkeypatch.setattr(mod, "deepen_base_ref", fake_deepen_base_ref)
    monkeypatch.setattr(mod, "changed_paths", fake_changed_paths)

    assert mod.detect("ci", "pull_request", "base123", "", "main") == {
        "platform": False,
        "canvas": False,
        "python": False,
        "scripts": False,
    }
    assert calls == [("deepen", "main"), ("changed", "True")]
