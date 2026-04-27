"""Pin the agent-card readiness probe to the SDK's canonical path.

main.py's _send_initial_prompt() polls the local A2A server's
well-known agent-card URL to know when it's safe to send the initial
prompt as a self-message. Pre-fix the URL was hardcoded to the pre-1.x
literal; a2a-sdk 1.x renamed the well-known path (the canonical value
lives in `a2a.utils.constants.AGENT_CARD_WELL_KNOWN_PATH`), so the
probe got 404 every attempt and silently fell through to "server not
ready after 30s, skipping" — dropping every workspace's
`initial_prompt` from config.yaml.

The fix is to import the SDK's `AGENT_CARD_WELL_KNOWN_PATH` constant
and use it directly in the probe URL. These tests pin the static
invariants of that fix:

  1. No hardcoded `/.well-known/agent.json` literal anywhere in
     main.py (catches a future contributor reverting to a literal).
  2. The probe URL fstring interpolates `AGENT_CARD_WELL_KNOWN_PATH`
     (catches a "fix" that imports the constant for show but still
     uses a literal in the actual GET).

Note: we deliberately do not assert the constant's value or compare
it against `create_agent_card_routes()` here. The runtime SDK is
mocked in this directory's conftest for the executor-test path, so
any test that imports the real `a2a.utils.constants` would either
collide with the mock or require running in a separate pytest session.
The two static invariants are sufficient: by always following whatever
the SDK constant says, we travel through any rename automatically. The
SDK's own contract that `create_agent_card_routes` mounts at the
constant's value is the SDK's responsibility, not ours.
"""

from __future__ import annotations

import re
from pathlib import Path

WORKSPACE_ROOT = Path(__file__).resolve().parents[1]


def test_main_uses_sdk_constant_for_agent_card_probe():
    """No hardcoded `/.well-known/agent.json` literal anywhere in main.py.

    The SDK constant (AGENT_CARD_WELL_KNOWN_PATH) is the single source
    of truth — string-literal probes drift the moment the SDK renames.
    """
    main = (WORKSPACE_ROOT / "main.py").read_text()

    bad_literal = "/.well-known/agent.json"
    offenders = [
        (lineno, line)
        for lineno, line in enumerate(main.splitlines(), 1)
        if bad_literal in line
    ]
    assert not offenders, (
        f"Found pre-1.x literal {bad_literal!r} in main.py — must use "
        f"the SDK's AGENT_CARD_WELL_KNOWN_PATH constant instead. "
        f"Offending lines: {offenders}"
    )

    assert (
        "AGENT_CARD_WELL_KNOWN_PATH" in main
    ), "main.py must import a2a.utils.constants.AGENT_CARD_WELL_KNOWN_PATH"


def test_probe_loop_uses_constant_in_url_format():
    """Spot-check that the URL fstring in main.py interpolates the
    constant, not a literal. Catches a future "fix" that imports the
    constant for show but still uses a literal in the actual GET."""
    main = (WORKSPACE_ROOT / "main.py").read_text()

    # The probe pattern: `client.get(f"http://127.0.0.1:{port}{...}")`
    # where `{...}` must be `{AGENT_CARD_WELL_KNOWN_PATH}`, not a
    # hardcoded path.
    pattern = re.compile(
        r'client\.get\(f"http://127\.0\.0\.1:\{port\}\{(?P<expr>[^}]+)\}"\)'
    )
    matches = pattern.findall(main)
    assert matches, "no readiness probe pattern found in main.py"
    for expr in matches:
        assert "AGENT_CARD_WELL_KNOWN_PATH" in expr, (
            f"readiness probe URL uses {expr!r} instead of "
            f"AGENT_CARD_WELL_KNOWN_PATH"
        )
