#!/usr/bin/env python3
"""Build the molecule-ai-workspace-runtime PyPI package from monorepo workspace/.

Monorepo workspace/ is the single source-of-truth for runtime code. The PyPI
package is a publish-time mirror produced by this script, NOT a parallel
editable copy. Anyone editing the runtime should edit workspace/, never the
sibling molecule-ai-workspace-runtime repo.

What this does
--------------
1. Copies workspace/ source into build/molecule_runtime/ (note the rename:
   bare modules become a real Python package).
2. Rewrites top-level imports so e.g. `from a2a_client import X` becomes
   `from molecule_runtime.a2a_client import X`. The rewrite is regex-based
   on a closed allowlist of modules — third-party imports like `from a2a.X`
   (the a2a-sdk package) are left alone because the regex is anchored on
   exact module names.
3. Writes a pyproject.toml with the requested version + the README + the
   py.typed marker.
4. Leaves the build dir ready for `python -m build` to produce a wheel/sdist.

Usage
-----
  scripts/build_runtime_package.py --version 0.1.6 --out /tmp/runtime-build
  cd /tmp/runtime-build && python -m build
  python -m twine upload dist/*

The publish workflow (.github/workflows/publish-runtime.yml) drives this
on every `runtime-v*` tag push.
"""

from __future__ import annotations

import argparse
import re
import shutil
import sys
from pathlib import Path

# Top-level Python modules in workspace/ that become molecule_runtime.X.
# Anything imported as `from <name> import` or `import <name>` (where <name>
# matches one of these) gets rewritten to use the package prefix.
#
# Closed list (not "every .py we copy") because a typo in workspace/ would
# otherwise leak into a wrong rewrite. The set is asserted against
# `workspace/*.py` at build time — if the disk contents drift from this
# list (new module added, old one removed), the build fails loud instead
# of silently shipping unrewritten imports. That gap caused 0.1.16 to
# ship `from transcript_auth import ...` (unrewritten — module added
# without updating this set), which broke every workspace startup with
# `ModuleNotFoundError: No module named 'transcript_auth'`.
TOP_LEVEL_MODULES = {
    "a2a_cli",
    "a2a_client",
    "a2a_executor",
    "a2a_mcp_server",
    "a2a_tools",
    "adapter_base",
    "agent",
    "agents_md",
    "boot_routes",
    "card_helpers",
    "config",
    "configs_dir",
    "consolidation",
    "coordinator",
    "event_log",
    "events",
    "executor_helpers",
    "heartbeat",
    "inbox",
    "initial_prompt",
    "internal_chat_uploads",
    "internal_file_read",
    "main",
    "mcp_cli",
    "molecule_ai_status",
    "not_configured_handler",
    "platform_auth",
    "platform_inbound_auth",
    "plugins",
    "preflight",
    "prompt",
    "runtime_wedge",
    "shared_runtime",
    "smoke_mode",
    "transcript_auth",
    "watcher",
}

# Subdirectory packages — these are already real packages (they have or will
# have __init__.py) so the rewrite is `from <pkg>` → `from molecule_runtime.<pkg>`.
SUBPACKAGES = {
    "adapters",
    "builtin_tools",
    "lib",
    "platform_tools",
    "plugins_registry",
    "policies",
    "skill_loader",
}

# Files in workspace/ NOT included in the published package. These are
# build artifacts, dev scripts, or monorepo-only scaffolding.
EXCLUDE_FILES = {
    "Dockerfile",
    "build-all.sh",
    "rebuild-runtime-images.sh",
    "entrypoint.sh",
    "pytest.ini",
    "requirements.txt",
    # Note: adapter_base.py, agents_md.py, hermes_executor.py, shared_runtime.py
    # are kept (referenced by adapters/__init__.py and other modules); they get
    # their imports rewritten via TOP_LEVEL_MODULES. Excluding them broke the
    # smoke-test install with `ModuleNotFoundError: adapter_base`.
}

EXCLUDE_DIRS = {
    "__pycache__",
    "tests",
    "molecule_audit",  # only used by tests; not on production import path
    "scripts",
}


def build_import_rewriter() -> re.Pattern:
    """Compile a single regex matching all import statements that need
    rewriting. The match groups capture the keyword + module name so the
    replacement preserves whitespace and trailing punctuation.

    Modules included: TOP_LEVEL_MODULES ∪ SUBPACKAGES.

    The negative-lookahead on `\\.` in the suffix prevents matching
    `from a2a.server.X import Y` against bare `a2a` (which isn't in our
    set, but the principle matters for any future short module name that
    happens to be a prefix of a real package name).
    """
    names = sorted(TOP_LEVEL_MODULES | SUBPACKAGES)
    alt = "|".join(re.escape(n) for n in names)
    # Matches:
    #   from <name>(\.|\s|import)
    #   import <name>(\s|$|,)
    # And captures the keyword + name so we can re-emit with prefix.
    pattern = (
        r"(?m)^(?P<indent>\s*)"          # leading whitespace (preserved)
        r"(?P<kw>from|import)\s+"        # 'from' or 'import'
        r"(?P<mod>" + alt + r")"          # the module name
        r"(?P<rest>[\s.,]|$)"            # what follows: '.subpath', ' import …', ',', whitespace, EOL
    )
    return re.compile(pattern)


def rewrite_imports(text: str, regex: re.Pattern) -> str:
    """Replace bare imports with package-prefixed ones.

    `import X`           → `import molecule_runtime.X as X`  (preserve binding)
    `from X import Y`    → `from molecule_runtime.X import Y`
    `from X.sub import Y` → `from molecule_runtime.X.sub import Y`

    Rejects `import X as Y` because the rewrite would produce
    `import molecule_runtime.X as X as Y`, a syntax error. The PR #2433
    incident shipped this exact pattern past `Python Lint & Test` (which
    runs against pre-rewrite source) but blew up the wheel-smoke gate.
    Detecting it here turns the silent build failure into a build-time
    error with a clear path: use `from X import …` or plain `import X`.
    """
    def repl(m: re.Match) -> str:
        indent, kw, mod, rest = m.group("indent"), m.group("kw"), m.group("mod"), m.group("rest")
        if kw == "from":
            # `from X` or `from X.sub` — always safe to prefix.
            return f"{indent}from molecule_runtime.{mod}{rest}"
        # `import X` — preserve the binding name `X` (callers do `X.foo`)
        # by aliasing. `import X.sub` is uncommon for our modules and would
        # need a different binding form, but isn't used in workspace/ today.
        if rest.startswith("."):
            # `import X.sub` — rewrite as `import molecule_runtime.X.sub` and
            # leave the trailing dot pattern intact for the rest of the line.
            return f"{indent}import molecule_runtime.{mod}{rest}"
        # Detect `import X as Y` — the regex's `rest` group captures only
        # the immediate following char (whitespace, comma, or EOL), so we
        # have to peek at the surrounding line context. The match start is
        # at the line's `import` keyword; everything after the matched
        # name on the same line is what the source author wrote.
        line_start = text.rfind("\n", 0, m.start()) + 1
        line_end = text.find("\n", m.end())
        if line_end == -1:
            line_end = len(text)
        line_after = text[m.end() - len(rest):line_end]
        # Strip comments from consideration so `import X  # noqa` doesn't trip.
        line_after_no_comment = line_after.split("#", 1)[0]
        if re.search(r"^\s*as\s+\w+", line_after_no_comment):
            raise ValueError(
                f"rewrite_imports: cannot rewrite 'import {mod} as <alias>' on a "
                f"workspace module — the regex would produce "
                f"'import molecule_runtime.{mod} as {mod} as <alias>', invalid syntax. "
                f"Use 'from {mod} import …' or plain 'import {mod}' instead. "
                f"Offending line: {text[line_start:line_end]!r}"
            )
        # Plain `import X` — alias preserves the local name.
        return f"{indent}import molecule_runtime.{mod} as {mod}{rest}"
    return regex.sub(repl, text)


def copy_tree_filtered(src: Path, dst: Path) -> list[Path]:
    """Copy src/ → dst/ skipping EXCLUDE_FILES + EXCLUDE_DIRS. Returns the
    list of .py files copied so the caller can run the import rewrite over
    them in one pass."""
    py_files: list[Path] = []
    if dst.exists():
        shutil.rmtree(dst)
    dst.mkdir(parents=True)
    for entry in src.iterdir():
        if entry.is_dir():
            if entry.name in EXCLUDE_DIRS:
                continue
            sub_py = copy_tree_filtered(entry, dst / entry.name)
            py_files.extend(sub_py)
        else:
            if entry.name in EXCLUDE_FILES:
                continue
            shutil.copy2(entry, dst / entry.name)
            if entry.suffix == ".py":
                py_files.append(dst / entry.name)
    return py_files


PYPROJECT_TEMPLATE = """\
[build-system]
requires = ["setuptools>=68.0", "wheel"]
build-backend = "setuptools.build_meta"

[project]
name = "molecule-ai-workspace-runtime"
version = "{version}"
description = "Molecule AI workspace runtime — shared infrastructure for all agent adapters"
requires-python = ">=3.11"
license = {{text = "BSL-1.1"}}
readme = "README.md"
dependencies = [
    "a2a-sdk[http-server]>=1.0.0,<2.0",
    "httpx>=0.27.0",
    "uvicorn>=0.30.0",
    "starlette>=0.38.0",
    "websockets>=12.0",
    "pyyaml>=6.0",
    "langchain-core>=0.3.0",
    "opentelemetry-api>=1.24.0",
    "opentelemetry-sdk>=1.24.0",
    "opentelemetry-exporter-otlp-proto-http>=1.24.0",
    "temporalio>=1.7.0",
]

[project.scripts]
molecule-runtime = "molecule_runtime.main:main_sync"
molecule-mcp = "molecule_runtime.mcp_cli:main"

[tool.setuptools.packages.find]
where = ["."]
include = ["molecule_runtime*"]

[tool.setuptools.package-data]
"molecule_runtime" = ["py.typed"]
"""


README_TEMPLATE = """\
# molecule-ai-workspace-runtime

Shared workspace runtime for [Molecule AI](https://github.com/Molecule-AI/molecule-core)
agent adapters. Installed by every workspace template image
(`workspace-template-claude-code`, `-langgraph`, `-hermes`, etc.) to provide
A2A delegation, heartbeat, memory, plugin loading, and skill management.

This package is **published from the molecule-core monorepo `workspace/`
directory** by the `publish-runtime` GitHub Actions workflow on every
`runtime-v*` tag push. **Do not edit this package directly** — edit
`workspace/` in the monorepo.

## External-runtime MCP server (`molecule-mcp`)

Operators running an agent outside the platform's container fleet
(any runtime that supports MCP stdio — Claude Code, hermes, codex,
etc.) can install this wheel and run the universal MCP server
locally:

```sh
pip install molecule-ai-workspace-runtime
WORKSPACE_ID=<uuid> \\
  PLATFORM_URL=https://<tenant>.staging.moleculesai.app \\
  MOLECULE_WORKSPACE_TOKEN=<bearer> \\
  molecule-mcp
```

That exposes the same 8 platform tools (`delegate_task`, `list_peers`,
`send_message_to_user`, `commit_memory`, etc.) that container-bound
runtimes already get via the workspace's auto-spawned MCP. Register
the binary in your agent's MCP config (e.g. Claude Code's
`claude mcp add molecule -- molecule-mcp` with the env above).

The token comes from the canvas → Tokens tab. Restarting an external
workspace from the canvas no longer revokes the token (PR #2412), so
operator tokens persist across status nudges.

See [`docs/workspace-runtime-package.md`](https://github.com/Molecule-AI/molecule-core/blob/main/docs/workspace-runtime-package.md)
for the publish flow and architecture.
"""


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, help="Package version, e.g. 0.1.6")
    parser.add_argument("--out", required=True, type=Path, help="Build output directory (will be wiped)")
    parser.add_argument("--source", type=Path, default=Path(__file__).resolve().parent.parent / "workspace",
                        help="Path to monorepo workspace/ directory (default: ../workspace from this script)")
    args = parser.parse_args()

    src = args.source.resolve()
    out = args.out.resolve()
    if not src.is_dir():
        print(f"error: source not a directory: {src}", file=sys.stderr)
        return 2

    # Drift gate: assert TOP_LEVEL_MODULES matches workspace/*.py.
    # Without this, a new top-level module added to workspace/ ships
    # with unrewritten `from <name> import` statements that explode at
    # runtime with ModuleNotFoundError. (See 0.1.16 transcript_auth
    # incident — closed list silently went stale.)
    on_disk_modules = {
        f.stem for f in src.glob("*.py")
        if f.stem not in {"__init__", "conftest"}
    }
    missing = on_disk_modules - TOP_LEVEL_MODULES
    stale = TOP_LEVEL_MODULES - on_disk_modules
    if missing or stale:
        print("error: TOP_LEVEL_MODULES drifted from workspace/*.py contents:", file=sys.stderr)
        if missing:
            print(f"  in workspace/ but NOT in TOP_LEVEL_MODULES (will ship un-rewritten): {sorted(missing)}", file=sys.stderr)
        if stale:
            print(f"  in TOP_LEVEL_MODULES but NOT in workspace/ (no-op, but misleading): {sorted(stale)}", file=sys.stderr)
        print("  Edit scripts/build_runtime_package.py:TOP_LEVEL_MODULES to match.", file=sys.stderr)
        return 3

    # Same drift gate for SUBPACKAGES — catches the inverse class of
    # bug where a workspace/ subdirectory is referenced by main.py
    # (`from lib.pre_stop import ...`) but is either missing from
    # SUBPACKAGES (so the rewriter doesn't qualify the import) or
    # accidentally listed in EXCLUDE_DIRS (so the directory itself
    # isn't shipped). 0.1.16-0.1.19 had `lib` in EXCLUDE_DIRS while
    # main.py imported from it — `ModuleNotFoundError: No module
    # named 'lib'` at every workspace startup.
    on_disk_subpkgs = {
        d.name for d in src.iterdir()
        if d.is_dir()
        and d.name not in EXCLUDE_DIRS
        and d.name not in {"__pycache__"}
        and (d / "__init__.py").exists()
    }
    sub_missing = on_disk_subpkgs - SUBPACKAGES
    sub_stale = SUBPACKAGES - on_disk_subpkgs
    if sub_missing or sub_stale:
        print("error: SUBPACKAGES drifted from workspace/ subdirectories:", file=sys.stderr)
        if sub_missing:
            print(f"  in workspace/ but NOT in SUBPACKAGES (will ship un-rewritten or be excluded): {sorted(sub_missing)}", file=sys.stderr)
        if sub_stale:
            print(f"  in SUBPACKAGES but NOT in workspace/ (no-op, but misleading): {sorted(sub_stale)}", file=sys.stderr)
        print("  Edit scripts/build_runtime_package.py:SUBPACKAGES + EXCLUDE_DIRS to match.", file=sys.stderr)
        return 3

    pkg_dir = out / "molecule_runtime"
    print(f"[build] source: {src}")
    print(f"[build] output: {out}")
    print(f"[build] package: {pkg_dir}")

    if out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True)

    py_files = copy_tree_filtered(src, pkg_dir)
    print(f"[build] copied {len(py_files)} .py files")

    # Ensure top-level package marker exists. workspace/ doesn't have one
    # (it's not a package in monorepo), but the published artifact must.
    init = pkg_dir / "__init__.py"
    if not init.exists():
        init.write_text('"""Molecule AI workspace runtime."""\n')

    # Touch py.typed so type-checkers in adapter consumers see the package
    # as typed. Empty file is the convention.
    (pkg_dir / "py.typed").touch()

    # Rewrite imports in every .py file we copied + the new __init__.py.
    regex = build_import_rewriter()
    rewrites = 0
    for f in [*py_files, init]:
        original = f.read_text()
        rewritten = rewrite_imports(original, regex)
        if rewritten != original:
            f.write_text(rewritten)
            rewrites += 1
    print(f"[build] rewrote imports in {rewrites} files")

    # Emit pyproject.toml + README at build root.
    (out / "pyproject.toml").write_text(PYPROJECT_TEMPLATE.format(version=args.version))
    (out / "README.md").write_text(README_TEMPLATE)

    print(f"[build] done. To publish:")
    print(f"  cd {out}")
    print(f"  python -m build")
    print(f"  python -m twine upload dist/*")
    return 0


if __name__ == "__main__":
    sys.exit(main())
