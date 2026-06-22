"""Unit tests for lint_no_coe_on_required — fixture catch + clean."""
import importlib.util
import os
import textwrap

HERE = os.path.dirname(__file__)
SCRIPT = os.path.join(HERE, "..", ".gitea", "scripts", "lint_no_coe_on_required.py")
spec = importlib.util.spec_from_file_location("lint_no_coe_on_required", SCRIPT)
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


def _wf(tmp_path, name, body):
    d = tmp_path / ".gitea" / "workflows"
    d.mkdir(parents=True, exist_ok=True)
    (d / name).write_text(textwrap.dedent(body))


def _allow(tmp_path, contexts):
    (tmp_path / ".gitea").mkdir(parents=True, exist_ok=True)
    (tmp_path / ".gitea" / "required-contexts.txt").write_text("\n".join(contexts) + "\n")


def test_coe_on_required_job_flagged(tmp_path):
    _wf(tmp_path, "ci.yml", """\
        name: CI
        on: [pull_request]
        jobs:
          all-required:
            runs-on: ubuntu-latest
            continue-on-error: true
            steps:
              - run: echo gate
    """)
    ctxs = mod.job_contexts(str(tmp_path / ".gitea" / "workflows"))
    info = ctxs["CI / all-required"]
    assert info[2] is True  # continue-on-error detected


def test_coe_string_true_flagged(tmp_path):
    _wf(tmp_path, "ci.yml", """\
        name: CI
        on: [pull_request]
        jobs:
          gate:
            runs-on: ubuntu-latest
            continue-on-error: "true"
            steps:
              - run: echo hi
    """)
    ctxs = mod.job_contexts(str(tmp_path / ".gitea" / "workflows"))
    assert ctxs["CI / gate"][2] is True


def test_required_job_without_coe_clean(tmp_path):
    _wf(tmp_path, "ci.yml", """\
        name: CI
        on: [pull_request]
        jobs:
          all-required:
            runs-on: ubuntu-latest
            steps:
              - run: echo gate
    """)
    ctxs = mod.job_contexts(str(tmp_path / ".gitea" / "workflows"))
    assert ctxs["CI / all-required"][2] is False


def test_named_job_context_uses_name_not_key(tmp_path):
    _wf(tmp_path, "e2e.yml", """\
        name: E2E API Smoke Test
        on: [pull_request]
        jobs:
          e2e-api:
            name: E2E API Smoke Test
            runs-on: ubuntu-latest
            steps:
              - run: echo hi
    """)
    ctxs = mod.job_contexts(str(tmp_path / ".gitea" / "workflows"))
    assert "E2E API Smoke Test / E2E API Smoke Test" in ctxs


def test_strip_event_suffix():
    assert mod.strip_event("CI / all-required (pull_request)") == "CI / all-required"
    assert mod.strip_event("ci / build (push)") == "ci / build"
    assert mod.strip_event("X / y") == "X / y"


def test_allowlist_load(tmp_path):
    _allow(tmp_path, ["# comment", "CI / all-required", "  ci / build (push)  "])
    got = mod.load_required_allowlist(str(tmp_path / ".gitea" / "required-contexts.txt"))
    assert got == {"CI / all-required", "ci / build"}
