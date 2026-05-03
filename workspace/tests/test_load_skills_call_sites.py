"""Static-AST audit gate for ``load_skills(...)`` call sites (#119 PR-4).

Declarative skill-compat — see ``skill_loader/loader.py:_normalize_runtime_field``
+ the unit tests at ``tests/test_skills_loader.py:test_load_skills_*`` —
only kicks in when callers thread ``current_runtime=`` through the call.
A new caller that forgets the kwarg silently force-loads
runtime-incompatible skills (no AttributeError surfaces, just a slow
runtime crash on the first tool invocation).

Today's call sites — ``adapter_base._common_setup`` (workspace + plugin
skill dirs) and ``main._on_skill_reload`` via ``SkillsWatcher`` — all
pass it. The unit tests pin the *behavior* of the kwarg; this gate
pins the *coverage* of the kwarg across every workspace-runtime
caller, so a future call site cannot silently regress the contract.

Why static AST and not behavior:
- Cheap: scans the same files CI already builds.
- Catches new call sites pre-merge — even ones that haven't shipped
  to a template yet.
- Same-shape pattern as PR-5 audit-coverage gate (#150) for
  tenant_resources audit-write coverage.

To intentionally bypass the gate (e.g. a one-off REPL helper that
genuinely doesn't have a runtime), add the call's source-file path
to ``_ALLOWED_BARE_CALLERS`` with a why-comment.
"""

from __future__ import annotations

import ast
from pathlib import Path

import pytest

WORKSPACE_DIR = Path(__file__).parent.parent

# Files exempt from the gate. Empty by design — every production caller
# should have a current_runtime. Add an entry only with an inline
# justification (test fixture, throwaway script, etc.).
_ALLOWED_BARE_CALLERS: dict[str, str] = {}


def _iter_workspace_python_files() -> list[Path]:
    """Walk workspace/ for .py files, skipping tests, vendored deps,
    and caches. The gate only applies to RUNTIME code — test files
    legitimately call load_skills without current_runtime to exercise
    the absent-kwarg fallback path (test_load_skills_no_current_runtime
    _loads_everything)."""
    skip_dirs = {"__pycache__", "tests", ".pytest_cache", "node_modules"}
    out: list[Path] = []
    for path in WORKSPACE_DIR.rglob("*.py"):
        if any(part in skip_dirs for part in path.relative_to(WORKSPACE_DIR).parts):
            continue
        out.append(path)
    return out


def _find_load_skills_calls(tree: ast.AST) -> list[ast.Call]:
    """Return every Call node whose function is named ``load_skills``.
    Matches both ``load_skills(...)`` (bare) and
    ``module.load_skills(...)`` (attribute access) so a future
    ``from skill_loader import loader; loader.load_skills(...)`` is
    caught too."""
    calls: list[ast.Call] = []
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        fn = node.func
        if isinstance(fn, ast.Name) and fn.id == "load_skills":
            calls.append(node)
        elif isinstance(fn, ast.Attribute) and fn.attr == "load_skills":
            calls.append(node)
    return calls


def _has_current_runtime_kwarg(call: ast.Call) -> bool:
    return any(kw.arg == "current_runtime" for kw in call.keywords)


def test_every_runtime_load_skills_call_passes_current_runtime():
    """Every ``load_skills(...)`` call site under workspace/ (excluding
    tests) MUST pass ``current_runtime=`` so declarative skill-compat
    filtering kicks in. Catches a new caller that forgets the kwarg
    pre-merge instead of letting it ship a silent regression."""
    violations: list[tuple[Path, int]] = []

    for py in _iter_workspace_python_files():
        rel = py.relative_to(WORKSPACE_DIR.parent).as_posix()
        if rel in _ALLOWED_BARE_CALLERS:
            continue

        try:
            tree = ast.parse(py.read_text(), filename=str(py))
        except SyntaxError:
            # Vendored/generated file we can't parse — out of scope.
            continue

        for call in _find_load_skills_calls(tree):
            if not _has_current_runtime_kwarg(call):
                violations.append((py.relative_to(WORKSPACE_DIR.parent), call.lineno))

    if violations:
        formatted = "\n".join(f"  {path}:{line}" for path, line in violations)
        pytest.fail(
            "load_skills(...) called without current_runtime= at:\n"
            f"{formatted}\n\n"
            "Pass current_runtime=type(self).name() (or the runtime string from "
            "config) so SKILL.md frontmatter `runtime: [...]` filtering applies. "
            "If this caller genuinely cannot supply a runtime, add the file path "
            "to _ALLOWED_BARE_CALLERS in this test with a why-comment."
        )


def test_known_call_sites_present():
    """Defense-in-depth — pin that the audit actually covers the call
    sites we know about. If a refactor moves them, this test fails
    loudly so the maintainer doesn't quietly lose coverage. Sibling
    pattern to test_snapshot_has_required_methods in
    test_adapter_base_signature.py."""
    expected_callers = {
        "workspace/adapter_base.py",
        "workspace/skill_loader/watcher.py",
    }
    found: set[str] = set()

    for py in _iter_workspace_python_files():
        rel = py.relative_to(WORKSPACE_DIR.parent).as_posix()
        if rel not in expected_callers:
            continue
        try:
            tree = ast.parse(py.read_text(), filename=str(py))
        except SyntaxError:
            continue
        if _find_load_skills_calls(tree):
            found.add(rel)

    missing = expected_callers - found
    assert not missing, (
        f"Expected load_skills caller(s) missing from audit scope: {sorted(missing)}.\n"
        "Either the file moved (update the expected set) or load_skills is no "
        "longer called from these sites (also update the expected set + audit "
        "the new caller pattern)."
    )
