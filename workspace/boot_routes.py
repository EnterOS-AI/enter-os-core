"""Build the Starlette routes for a workspace from its (card, adapter
state) pair.

Pairs with PR #2756, which decoupled ``/.well-known/agent-card.json`` from
``adapter.setup()`` failure. main.py was the only consumer and was
``# pragma: no cover`` — so the wiring (card-route mounted unconditionally,
JSON-RPC route swapped between DefaultRequestHandler and the
not-configured handler based on ``adapter_ready``) had no pytest coverage.

A future refactor that re-couples the two would silently bypass PR #2756
and shipped the original "stuck booting forever" UX again. That gap is
what closes here: extract the route-assembly into a pure function whose
behaviour is unit-testable with Starlette's TestClient, and have main.py
call it. Issue molecule-core#2761.
"""
from __future__ import annotations

from typing import Any

from starlette.routing import Route

from not_configured_handler import make_not_configured_handler

# Heavy a2a-sdk imports are lazy: deferred to inside build_routes so
# tests that exercise only the not-configured branch (no executor) don't
# need a2a.server.request_handlers / routes stubbed in their conftest.
# Production boot pays the import cost once, on workspace startup.


def build_routes(
    agent_card: Any,
    executor: Any | None,
    adapter_error: str | None,
) -> list:
    """Return the list of Starlette routes for this workspace.

    Always mounts ``/.well-known/agent-card.json`` from ``agent_card``.

    JSON-RPC route at ``/`` swaps based on adapter state:

    * ``executor`` is non-None → ``DefaultRequestHandler`` with the
      executor (production happy-path).
    * ``executor`` is None → ``not_configured_handler`` returning JSON-RPC
      ``-32603`` with ``adapter_error`` in ``error.data``. The
      workspace stays REACHABLE (operator can introspect, deprovision,
      redeploy with corrected env) instead of crash-looping invisibly.

    The two branches are mutually exclusive — caller passes one or the
    other, never both. Test coverage at ``tests/test_boot_routes.py``
    pins the contract.
    """
    from a2a.server.routes import create_agent_card_routes

    routes: list = []
    routes.extend(create_agent_card_routes(agent_card))

    if executor is not None:
        from a2a.server.request_handlers import DefaultRequestHandler
        from a2a.server.routes import create_jsonrpc_routes
        from a2a.server.tasks import InMemoryTaskStore

        handler = DefaultRequestHandler(
            agent_executor=executor,
            task_store=InMemoryTaskStore(),
            agent_card=agent_card,
        )
        # enable_v0_3_compat=True is the JSON-RPC wire-compat path: clients
        # using v0.3-shaped payloads (`"role": "user"` lowercase + camelCase
        # Pydantic field names) can talk to us without re-deploying.
        # Outbound payloads must also use v0.3 shape — see main.py's
        # original comment block for the full a2a-sdk 1.x migration note.
        routes.extend(
            create_jsonrpc_routes(
                request_handler=handler,
                rpc_url="/",
                enable_v0_3_compat=True,
            )
        )
    else:
        routes.append(
            Route("/", make_not_configured_handler(adapter_error), methods=["POST"])
        )

    return routes
