"""Console-script entry point for the ``molecule-mcp`` universal MCP server.

Validates required environment BEFORE importing the heavy
``a2a_mcp_server`` module — that module triggers a ``RuntimeError`` at
import time when ``WORKSPACE_ID`` is unset (a2a_client.py:22), and
console-script entry-point shims surface it as an ugly traceback. This
wrapper catches the missing-env case early and prints actionable help
to stderr so an operator running ``molecule-mcp`` for the first time
gets the right pointer in the first 3 lines of output instead of a
20-line traceback.

Existing in-container usage (``python -m molecule_runtime.a2a_mcp_server``
or direct import) is unaffected — those paths bypass this wrapper. Only
the external-runtime ``molecule-mcp`` console script routes through here.
"""
from __future__ import annotations

import os
import sys
from pathlib import Path


def _print_missing_env_help(missing: list[str], have_token_file: bool) -> None:
    print("molecule-mcp: missing required environment.\n", file=sys.stderr)
    print("Set the following before running molecule-mcp:", file=sys.stderr)
    print("  WORKSPACE_ID                — your workspace UUID (from canvas)", file=sys.stderr)
    print(
        "  PLATFORM_URL                — base URL of your Molecule platform "
        "(e.g. https://your-tenant.staging.moleculesai.app)",
        file=sys.stderr,
    )
    if not have_token_file:
        print(
            "  MOLECULE_WORKSPACE_TOKEN    — bearer token for this workspace "
            "(canvas → Tokens tab)",
            file=sys.stderr,
        )
    print("", file=sys.stderr)
    print(f"Currently missing: {', '.join(missing)}", file=sys.stderr)


def main() -> None:
    """Entry point for the ``molecule-mcp`` console script.

    Returns nothing — calls ``sys.exit`` on validation failure or on
    normal completion of the underlying MCP server loop.
    """
    missing: list[str] = []
    if not os.environ.get("WORKSPACE_ID", "").strip():
        missing.append("WORKSPACE_ID")
    if not os.environ.get("PLATFORM_URL", "").strip():
        missing.append("PLATFORM_URL")
    # Token can come from env OR file — only flag when both are absent.
    # Mirrors platform_auth.get_token's resolution order (file-first,
    # env-fallback).
    configs_dir = Path(os.environ.get("CONFIGS_DIR", "/configs"))
    has_token_file = (configs_dir / ".auth_token").is_file()
    has_token_env = bool(os.environ.get("MOLECULE_WORKSPACE_TOKEN", "").strip())
    if not has_token_file and not has_token_env:
        missing.append("MOLECULE_WORKSPACE_TOKEN (or CONFIGS_DIR/.auth_token)")

    if missing:
        _print_missing_env_help(missing, have_token_file=has_token_file)
        sys.exit(2)

    # Env is valid — safe to import the heavy module now. Importing
    # earlier would trigger a2a_client.py:22's module-level RuntimeError
    # before our friendly help reaches the user.
    from a2a_mcp_server import cli_main
    cli_main()


if __name__ == "__main__":  # pragma: no cover
    main()
