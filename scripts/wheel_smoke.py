#!/usr/bin/env python3
"""Smoke-test an installed molecule-ai-workspace-runtime wheel.

Runs the same invariant assertions in two workflows:
  * publish-runtime.yml — after building dist/*.whl, before PyPI upload
  * runtime-prbuild-compat.yml — after building the PR's wheel, before merge

Splitting the smoke across two inline heredocs let PR-time and publish-time
drift apart. After 2026-04 we kept hitting publish-time failures for
regressions a PR-time check could have caught. One script, both gates.

Failure here intentionally exits non-zero so the workflow's `run:` step fails.
Each block prints a single ✓ line on success so the GH summary log stays
readable; assertion errors propagate with their own message.

Run directly: `python scripts/wheel_smoke.py` after `pip install <wheel>`.
"""

import os
import sys


def smoke_imports_and_invariants() -> None:
    """Module imports + stable contract assertions.

    Importing main_sync by name is the strongest pre-PyPI gate we have for
    import-rewrite mistakes (the 0.1.16 incident, where main.py loaded but
    main_sync was missing because the build script dropped a re-export).
    """
    from molecule_runtime.main import main_sync  # noqa: F401
    from molecule_runtime import a2a_client, a2a_tools  # noqa: F401
    from molecule_runtime.builtin_tools import memory  # noqa: F401
    from molecule_runtime.adapters import get_adapter, BaseAdapter, AdapterConfig

    # cli_main + mcp_cli.main are the molecule-mcp console-script entry
    # points — the external-runtime universal MCP path. Same regression
    # class as the 0.1.16 main_sync incident: a silent rename or missed
    # rewrite here would break every external operator's MCP install on
    # the next wheel publish. Pin both names because pyproject points
    # at mcp_cli.main, which then imports a2a_mcp_server.cli_main.
    from molecule_runtime.a2a_mcp_server import cli_main  # noqa: F401
    from molecule_runtime.mcp_cli import main as mcp_cli_main  # noqa: F401
    assert callable(cli_main), "a2a_mcp_server.cli_main must be callable"
    assert callable(mcp_cli_main), "mcp_cli.main must be callable"

    # inbox.activate / get_state / start_poller_thread form the inbound
    # delivery path for the standalone molecule-mcp wrapper. mcp_cli.main
    # imports + activates these at startup; if a wheel ships without
    # them, the standalone agent silently loses the wait_for_message /
    # inbox_peek / inbox_pop tools and reverts to outbound-only.
    from molecule_runtime.inbox import (  # noqa: F401
        InboxState,
        activate as inbox_activate,
        get_state as inbox_get_state,
        start_poller_thread as inbox_start_poller_thread,
    )
    assert callable(inbox_activate), "inbox.activate must be callable"
    assert callable(inbox_get_state), "inbox.get_state must be callable"
    assert callable(inbox_start_poller_thread), "inbox.start_poller_thread must be callable"

    assert a2a_client._A2A_ERROR_PREFIX, "a2a_client missing error sentinel"
    assert callable(get_adapter), "adapters.get_adapter must be callable"
    assert hasattr(BaseAdapter, "name"), "BaseAdapter interface broken"
    assert hasattr(AdapterConfig, "__init__"), "AdapterConfig dataclass missing"
    print("✓ module imports + invariants OK")


def smoke_agent_card_call_shape() -> None:
    """Construct AgentCard with the EXACT kwargs main.py uses.

    Pure imports don't catch field-shape regressions in upstream SDKs that
    only surface at construction time. Two bugs of this exact class shipped
    since the a2a-sdk 1.0 migration:
      - state_transition_history=True (#2179)
      - supported_protocols=[...] (the protobuf field is supported_interfaces;
        every workspace boot crashed with `ValueError: Protocol message
        AgentCard has no "supported_protocols" field`)

    main.py and this block MUST stay in lockstep — adding a kwarg there
    without mirroring it here is the regression vector.
    """
    from a2a.types import AgentCard, AgentCapabilities, AgentSkill, AgentInterface

    AgentCard(
        name="smoke-agent",
        description="wheel-smoke: AgentCard call-shape",
        version="0.0.0-smoke",
        supported_interfaces=[
            AgentInterface(protocol_binding="https://a2a.g/v1", url="http://localhost:8080"),
        ],
        capabilities=AgentCapabilities(
            streaming=True,
            push_notifications=False,
        ),
        skills=[
            AgentSkill(
                id="smoke-skill",
                name="Smoke",
                description="no-op",
                tags=["smoke"],
                examples=["noop"],
            ),
        ],
        default_input_modes=["text/plain", "application/json"],
        default_output_modes=["text/plain", "application/json"],
    )
    print("✓ AgentCard call-shape smoke passed")


def smoke_well_known_path_alignment() -> None:
    """The SDK's published constant must match the path it actually mounts.

    main.py polls AGENT_CARD_WELL_KNOWN_PATH to detect server readiness. If
    the constant and create_agent_card_routes() drift, every workspace's
    initial_prompt silently drops (probe 404s, falls through to "skipping").
    This was the #2193 incident class.
    """
    from a2a.types import AgentCard
    from a2a.utils.constants import AGENT_CARD_WELL_KNOWN_PATH
    from a2a.server.routes import create_agent_card_routes

    mounted_paths = [
        getattr(r, "path", None)
        for r in create_agent_card_routes(
            AgentCard(
                name="wk-smoke",
                description="well-known mount alignment",
                version="0.0.0-smoke",
            )
        )
    ]
    assert AGENT_CARD_WELL_KNOWN_PATH in mounted_paths, (
        f"AGENT_CARD_WELL_KNOWN_PATH ({AGENT_CARD_WELL_KNOWN_PATH!r}) is NOT among "
        f"paths mounted by create_agent_card_routes ({mounted_paths!r}). The SDK "
        "constant and its own route factory have drifted — workspace probes will "
        "404 forever, silently dropping every workspace initial_prompt."
    )
    print(f"✓ well-known mount alignment OK ({AGENT_CARD_WELL_KNOWN_PATH})")


def smoke_message_helper() -> None:
    """new_text_message is the v1.x rename of new_agent_text_message.

    main.py and a2a_executor.py call new_text_message in hot paths; if the
    import breaks, every reply errors with ImportError before the message
    even leaves the workspace. Importing here catches a future v2.x rename
    at publish time.
    """
    from a2a.helpers import new_text_message

    msg = new_text_message("smoke")
    assert msg is not None, "new_text_message returned None"
    print("✓ message helper import + call OK")


def main() -> int:
    # main.py validates WORKSPACE_ID at module-import time via platform_auth.
    # Set placeholders so the smoke doesn't trip on the env-var guard.
    os.environ.setdefault("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    os.environ.setdefault("PLATFORM_URL", "http://localhost:8080")

    smoke_imports_and_invariants()
    smoke_agent_card_call_shape()
    smoke_well_known_path_alignment()
    smoke_message_helper()
    print("✓ wheel smoke passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
