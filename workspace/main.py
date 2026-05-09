"""Workspace runtime entry point.

Loads config -> discovers adapter -> setup -> create executor -> wrap in A2A -> register -> heartbeat.
"""

import asyncio
import json
import os
import socket

import httpx
import uvicorn
# KI-009 a2a-sdk v1 migration: A2AStarletteApplication removed; use Starlette route factory
from a2a.server.routes import create_agent_card_routes, create_jsonrpc_routes
from a2a.server.request_handlers import DefaultRequestHandler
from a2a.server.tasks import InMemoryTaskStore
from a2a.types import AgentCard, AgentCapabilities, AgentSkill, AgentInterface
from starlette.applications import Starlette

from adapters import get_adapter, AdapterConfig
from agents_md import generate_agents_md
from config import load_config
from heartbeat import HeartbeatLoop
from preflight import run_preflight, render_preflight_report
from builtin_tools.awareness_client import get_awareness_config
import uuid as _uuid

from builtin_tools.telemetry import setup_telemetry, make_trace_middleware
from policies.namespaces import resolve_awareness_namespace


from initial_prompt import (
    mark_initial_prompt_attempted,
    resolve_initial_prompt_marker,
)
from platform_auth import auth_headers, self_source_headers


def get_machine_ip() -> str:  # pragma: no cover
    """Get the machine's IP for A2A discovery."""
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.connect(("8.8.8.8", 80))
        ip = s.getsockname()[0]
        s.close()
        return ip
    except Exception:
        return "127.0.0.1"


# Re-exported from transcript_auth for the inline /transcript handler.
# Separate module keeps the security-critical gate import-light + unit-testable.
from transcript_auth import transcript_authorized as _transcript_authorized


async def main():  # pragma: no cover
    workspace_id = os.environ.get("WORKSPACE_ID", "")
    if not workspace_id:
        raise SystemExit("FATAL: WORKSPACE_ID env var is not set. Aborting.")
    config_path = os.environ.get("WORKSPACE_CONFIG_PATH", "/configs")
    # Docker-aware default — host.docker.internal resolves the platform service
    # from inside the Docker network mesh; falls back to localhost for local dev.
    if os.path.exists("/.dockerenv") or os.environ.get("DOCKER_VERSION"):
        platform_url = os.environ.get("PLATFORM_URL", "http://host.docker.internal:8080")
    else:
        platform_url = os.environ.get("PLATFORM_URL", "http://localhost:8080")
    awareness_config = get_awareness_config()

    # 0. Initialise OpenTelemetry (no-op if packages not installed)
    setup_telemetry(service_name=workspace_id)

    # 0a. Fix /workspace perms before any agent code runs. Docker ships
    # named volumes as root:root 755 — without this the non-root agent
    # user can't write files the user asked it to produce, and the
    # "agent → file → user downloads" flow dead-ends at a bash "permission
    # denied". Best-effort: no-ops silently if molecule-runtime itself
    # isn't root (template's own start.sh should have handled it there).
    from executor_helpers import ensure_workspace_writable
    ensure_workspace_writable()

    # 1. Load config
    config = load_config(config_path)
    port = config.a2a.port
    preflight = run_preflight(config, config_path)
    render_preflight_report(preflight)

    # 1a. Generate AGENTS.md so peer agents and discovery tools can see this
    # workspace's identity, role, endpoint, and capabilities immediately.
    try:
        generate_agents_md(config_path, "/workspace/AGENTS.md")
    except Exception as _agents_md_err:  # pragma: no cover
        print(f"Warning: AGENTS.md generation failed (non-fatal): {_agents_md_err}")
    if not preflight.ok:
        raise SystemExit(1)
    if awareness_config:
        awareness_namespace = resolve_awareness_namespace(
            workspace_id,
            awareness_config.get("namespace", ""),
        )
        print(f"Awareness enabled for namespace: {awareness_namespace}")

    # 1.5  Initialise governance adapter (no-op if disabled or package absent)
    from builtin_tools.governance import initialize_governance
    if config.governance.enabled:
        await initialize_governance(config.governance)
        print(f"Governance: Microsoft Agent Governance Toolkit enabled (mode={config.governance.policy_mode})")
    else:
        print("Governance: disabled (set governance.enabled: true in config.yaml to activate)")

    # 2. Create heartbeat (passed to adapter for task tracking).
    # interval is sourced from observability.heartbeat_interval_seconds
    # in config.yaml — clamped to [5, 300] at parse time. Operators
    # who want a faster crash-detection signal lower it; ones who want
    # to reduce platform write load raise it.
    heartbeat = HeartbeatLoop(
        platform_url,
        workspace_id,
        interval_seconds=config.observability.heartbeat_interval_seconds,
    )

    # 3. Get adapter for this runtime
    runtime = config.runtime or "langgraph"
    adapter_cls = get_adapter(runtime)  # Raises KeyError if unknown — no silent fallback

    adapter = adapter_cls()
    print(f"Runtime: {runtime} ({adapter.display_name()})")

    # 3a. Wire pluggable event-log backend from config.observability.event_log.
    # Default config.yaml sets backend=memory; operators set "disabled" to
    # opt out without removing append-call sites from adapter code.
    from event_log import create_event_log
    adapter.event_log = create_event_log(
        backend=config.observability.event_log.backend,
        ttl_seconds=config.observability.event_log.ttl_seconds,
        max_entries=config.observability.event_log.max_entries,
    )

    # 4. Build adapter config
    adapter_config = AdapterConfig(
        model=config.model,
        system_prompt=None,  # Adapter builds its own prompt
        tools=config.skills,  # Skill names from config.yaml
        runtime_config=vars(config.runtime_config) if config.runtime_config else {},
        config_path=config_path,
        workspace_id=workspace_id,
        prompt_files=config.prompt_files,
        a2a_port=port,
        heartbeat=heartbeat,
    )

    # 5. Build the AgentCard *before* adapter.setup() so /.well-known/agent-card.json
    # is reachable as soon as uvicorn binds, regardless of whether the adapter
    # has working LLM credentials. Decoupling readiness ("is the workspace up?")
    # from configuration ("can it actually answer?") means a workspace with a
    # missing/rotated key stays REACHABLE — canvas can render a clear
    # "agent not configured" error instead of "stuck booting forever," and
    # operators can deprovision/redeploy normally. Skills built from
    # config.skills (static names from config.yaml) up front; richer metadata
    # from the adapter's loaded_skills swaps in below if setup() succeeds.
    machine_ip = os.environ.get("HOSTNAME", get_machine_ip())
    workspace_url = f"http://{machine_ip}:{port}"

    # v1: AgentCard.url removed; put url+protocol in supported_interfaces instead.
    # v1: AgentCapabilities.inputModes/outputModes removed; move to AgentCard.default_*.
    # v1: pushNotifications → push_notifications (Pydantic field name)
    #
    # AgentCard's protocol message uses `supported_interfaces` (plural,
    # interfaces — see a2a-sdk types/a2a_pb2.pyi:189). The 0.3.x→1.0
    # migration in #1974 originally used `supported_protocols`, which
    # the protobuf doesn't expose at all — every workspace boot since
    # then crashed with `ValueError: Protocol message AgentCard has no
    # "supported_protocols" field`. The crash didn't surface in the
    # publish-runtime smoke because the smoke only IMPORTS
    # molecule_runtime.main, never CALLS the AgentCard constructor.
    # Don't rename back.
    agent_card = AgentCard(
        name=config.name,
        description=config.description or config.name,
        version=config.version,
        supported_interfaces=[
            AgentInterface(protocol_binding="https://a2a.g/v1", url=workspace_url)
        ],
        capabilities=AgentCapabilities(
            streaming=config.a2a.streaming,
            push_notifications=config.a2a.push_notifications,
            # Note: state_transition_history (a 0.x capability flag) was
            # removed in a2a-sdk 1.0. Per the SDK's own
            # a2a/compat/v0_3/conversions.py: "No longer supported in
            # v1.0". The capability is now universal — Task.history is
            # always available and tasks/get accepts historyLength via
            # apply_history_length(). Don't add this kwarg back.
        ),
        # Static skill stubs from config.yaml; replaced with rich metadata
        # below if adapter.setup() loads skills successfully.
        skills=[
            AgentSkill(id=name, name=name, description=name, tags=[], examples=[])
            for name in (config.skills or [])
        ],
        default_input_modes=["text/plain", "application/json"],
        default_output_modes=["text/plain", "application/json"],
    )

    # 6. Setup adapter and create executor
    # On failure: log + continue. The card route stays mounted (above);
    # the JSON-RPC route below returns -32603 "agent not configured" until
    # the operator fixes credentials and redeploys. Heartbeat keeps running
    # so the platform sees the workspace as reachable-but-misconfigured
    # rather than crash-looping.
    adapter_ready = False
    adapter_error: str | None = None
    executor = None
    try:
        await adapter.setup(adapter_config)
        executor = await adapter.create_executor(adapter_config)

        # 6a. Boot-smoke short-circuit (issue #2275): if MOLECULE_SMOKE_MODE
        # is set, exercise the executor's full import tree by calling
        # execute() once with stub deps + a short timeout. Skips platform
        # registration + uvicorn entirely. Returns process exit code.
        from smoke_mode import is_smoke_mode, run_executor_smoke
        if is_smoke_mode():
            exit_code = await run_executor_smoke(executor)
            if hasattr(heartbeat, "stop"):
                try:
                    await heartbeat.stop()
                except Exception:  # noqa: BLE001
                    pass
            raise SystemExit(exit_code)

        # 6b. Restore from pre-stop snapshot if one exists (GH#1391).
        # The snapshot is scrubbed before being written, so secrets are
        # already redacted — restore_state must not re-expose them.
        from lib.pre_stop import read_snapshot
        snapshot = read_snapshot()
        if snapshot:
            try:
                adapter.restore_state(snapshot)
                print(
                    f"Pre-stop snapshot restored: task={snapshot.get('current_task', '')!r}, "
                    f"uptime={snapshot.get('uptime_seconds', 0)}s"
                )
            except Exception as restore_err:
                print(f"Warning: snapshot restore failed (continuing): {restore_err}")

        # 6c. Swap rich skill metadata into the card now that setup() loaded
        # them. In-place mutation: a2a-sdk's create_agent_card_routes serialises
        # the card on each request, so the route mounted below sees the update.
        # Isolated via card_helpers.enrich_card_skills — a malformed
        # loaded_skills shape (e.g., a future adapter that doesn't follow
        # the .metadata convention) is logged + swallowed instead of
        # propagating up to the outer except, where it would silently
        # degrade an OK boot to the not-configured state.
        from card_helpers import enrich_card_skills
        enrich_card_skills(agent_card, getattr(adapter, "loaded_skills", None))
        adapter_ready = True
    except SystemExit:
        # Smoke-mode exit signal — propagate untouched.
        raise
    except Exception as setup_err:  # noqa: BLE001
        adapter_error = f"{type(setup_err).__name__}: {setup_err}"
        print(
            f"WARNING: adapter.setup() failed — workspace will serve agent-card "
            f"but JSON-RPC will return -32603 until configuration is fixed. "
            f"Reason: {adapter_error}",
            flush=True,
        )
        # Heartbeat keeps running so the platform marks the workspace as
        # reachable-but-misconfigured. Operators can then redeploy with the
        # correct env vars without having to chase a crash-loop.

    # 6.5. Initialise Temporal durable execution wrapper (optional). Only
    # meaningful when an executor exists; skipped on misconfigured boots.
    if adapter_ready:
        from builtin_tools.temporal_workflow import create_wrapper as _create_temporal_wrapper
        temporal_wrapper = _create_temporal_wrapper()
        await temporal_wrapper.start()

    # 7. Wrap in A2A.
    #
    # Route assembly is in workspace/boot_routes.py so the contract —
    # card always mounted, JSON-RPC route swaps based on adapter state
    # (DefaultRequestHandler when executor is non-None, not_configured
    # handler returning -32603 otherwise) — is unit-testable with
    # Starlette's TestClient. main.py is `# pragma: no cover` so without
    # this extraction a future refactor that re-coupled card + setup()
    # would silently bypass PR #2756. tests/test_boot_routes.py pins
    # the four-branch contract.
    from boot_routes import build_routes
    app = Starlette(routes=build_routes(agent_card, executor, adapter_error))

    # 8. Register with platform
    # When adapter.setup() failed, advertise via configuration_status so
    # the platform/canvas can render "configured: false, reason: …" instead
    # of a confused "ready but silent" state.
    loaded_skills = getattr(adapter, "loaded_skills", None) or []
    agent_card_dict = {
        "name": config.name,
        "description": config.description,
        "version": config.version,
        "url": workspace_url,
        "skills": [
            {
                "id": s.metadata.id,
                "name": s.metadata.name,
                "description": s.metadata.description,
                "tags": s.metadata.tags,
            }
            for s in loaded_skills
        ] if adapter_ready else [
            {"id": n, "name": n, "description": n, "tags": []}
            for n in (config.skills or [])
        ],
        "capabilities": {
            "streaming": config.a2a.streaming,
            "pushNotifications": config.a2a.push_notifications,
        },
        "configuration_status": "ready" if adapter_ready else "not_configured",
        **({"configuration_error": adapter_error} if adapter_error else {}),
    }

    async with httpx.AsyncClient(timeout=10.0) as client:
        try:
            resp = await client.post(
                f"{platform_url}/registry/register",
                json={
                    "id": workspace_id,
                    "url": workspace_url,
                    "agent_card": agent_card_dict,
                },
                headers=auth_headers(),
            )
            print(f"Registered with platform: {resp.status_code}")
            # Phase 30.1 — capture the auth token issued at first register.
            # The platform only mints one on first register per workspace,
            # so a subsequent restart gets an empty auth_token and we
            # keep using the on-disk copy from the original issuance.
            if resp.status_code == 200:
                try:
                    body = resp.json()
                    tok = body.get("auth_token")
                    if tok:
                        from platform_auth import save_token
                        save_token(tok)
                        print(f"Saved workspace auth token (prefix={tok[:8]}…)")
                    # RFC #2312 PR-F: persist platform_inbound_secret if the
                    # platform supplied one. Idempotent — writing the same
                    # value over an existing file is harmless. Required for
                    # SaaS where there's no persistent /configs volume; on
                    # Docker mode it overwrites the value the provisioner
                    # already wrote at workspace creation.
                    inbound = body.get("platform_inbound_secret")
                    if inbound:
                        from platform_inbound_auth import save_inbound_secret
                        save_inbound_secret(inbound)
                        print(f"Saved platform_inbound_secret (prefix={inbound[:8]}…)")
                except Exception as parse_exc:
                    print(f"Warning: couldn't parse register response for token: {parse_exc}")
        except Exception as e:
            print(f"Warning: failed to register with platform: {e}")

    # 9. Start heartbeat
    heartbeat.start()

    # 9b. Start skills hot-reload watcher (background task)
    # When a skill file changes the watcher reloads the skill module and calls
    # back into the adapter so the next A2A request uses the updated tools.
    # Skipped on misconfigured boots — adapter has no executor / tool registry
    # to swap into, so reloading skills would NPE on the agent rebuild path.
    if adapter_ready and config.skills:
        try:
            from skill_loader.watcher import SkillsWatcher

            def _on_skill_reload(updated_skill):
                """Rebuild the LangGraph agent when a skill changes in-place."""
                if not hasattr(adapter, "loaded_skills"):
                    return
                # Replace the matching skill in the adapter's skill list
                adapter.loaded_skills = [
                    updated_skill if s.metadata.id == updated_skill.metadata.id else s
                    for s in adapter.loaded_skills
                ]
                # Rebuild the agent's tool list from updated skills
                if hasattr(adapter, "all_tools") and hasattr(adapter, "system_prompt"):
                    from builtin_tools.approval import request_approval
                    from builtin_tools.delegation import delegate_task, delegate_task_async, check_task_status
                    from builtin_tools.memory import commit_memory, recall_memory
                    from builtin_tools.sandbox import run_code
                    # Core platform tools mirror adapter_base.all_tools — must
                    # match the platform_tools registry names so docs and tools
                    # never drift.
                    base_tools = [
                        delegate_task, delegate_task_async, check_task_status,
                        request_approval, commit_memory, recall_memory, run_code,
                    ]
                    skill_tools = []
                    for sk in adapter.loaded_skills:
                        skill_tools.extend(sk.tools)
                    adapter.all_tools = base_tools + skill_tools
                    # Rebuild compiled agent so next ainvoke picks up new tools
                    try:
                        from agent import create_agent
                        new_agent = create_agent(
                            config.model, adapter.all_tools, adapter.system_prompt
                        )
                        executor.agent = new_agent
                        print(f"Skills hot-reload: '{updated_skill.metadata.id}' reloaded — "
                              f"{len(updated_skill.tools)} tool(s)")
                    except Exception as rebuild_err:
                        print(f"Skills hot-reload: agent rebuild failed: {rebuild_err}")

            skills_watcher = SkillsWatcher(
                config_path=config_path,
                skill_names=config.skills,
                on_reload=_on_skill_reload,
                current_runtime=runtime,
            )
            asyncio.create_task(skills_watcher.start())
            print(f"Skills hot-reload enabled for: {config.skills}")
        except Exception as e:
            print(f"Warning: skills watcher could not start: {e}")

    # 10. Run A2A server
    print(f"Workspace {workspace_id} starting on port {port}")
    # Wrap the ASGI app with W3C TraceContext extraction middleware so incoming
    # A2A HTTP requests propagate their trace context into _incoming_trace_context.
    # v1: Starlette app is constructed directly; no build() step needed
    starlette_app = app

    # Add /transcript route — exposes the most-recent agent session log
    # (claude-code reads ~/.claude/projects/<cwd>/<session>.jsonl). Other
    # runtimes return supported:false.
    from starlette.responses import JSONResponse
    from starlette.routing import Route

    async def _transcript_handler(request):
        # Require workspace bearer token — the same token issued at registration
        # and stored in /configs/.auth_token. Any container on molecule-core-net
        # could otherwise read the full session log. Closes #287.
        #
        # #328: fail CLOSED when the token file is unavailable. get_token()
        # returns None during the bootstrap window (first register hasn't
        # completed), if /configs/.auth_token was deleted, or on OSError.
        # The old `if expected:` guard treated all three cases as "skip
        # auth" — an unauthenticated container on the same Docker network
        # could read the entire session log during that window. Deny
        # instead. The platform's TranscriptHandler acquires the token
        # during registration, so once the bootstrap completes it always
        # has a valid credential to present.
        from platform_auth import get_token
        if not _transcript_authorized(get_token(), request.headers.get("Authorization", "")):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        try:
            since = int(request.query_params.get("since", "0"))
            limit = int(request.query_params.get("limit", "100"))
        except (TypeError, ValueError):
            return JSONResponse({"error": "since and limit must be integers"}, status_code=400)
        # Isolate adapter call: misconfigured boots leave the adapter
        # partially-initialised, and a future adapter override of
        # transcript_lines might assume setup() ran. Surface a 503 with
        # a clear reason instead of letting the exception propagate to
        # Starlette's 500 handler — same pattern as the not-configured
        # JSON-RPC route (PR #2756). BaseAdapter.transcript_lines's
        # default returns {"supported": false} so today's 4 adapters
        # never trigger this branch; this is the safety net.
        try:
            result = await adapter.transcript_lines(since=since, limit=limit)
        except Exception as transcript_err:  # noqa: BLE001
            return JSONResponse(
                {
                    "error": "transcript unavailable",
                    "detail": f"{type(transcript_err).__name__}: {transcript_err}",
                },
                status_code=503,
            )
        return JSONResponse(result)

    starlette_app.add_route("/transcript", _transcript_handler, methods=["GET"])

    # /internal/* — platform→workspace forward calls (RFC #2312). Auth
    # is the per-workspace platform_inbound_secret in
    # /configs/.platform_inbound_secret, distinct from the outbound
    # workspace_auth_token used by /transcript above.
    from internal_chat_uploads import ingest_handler as _internal_chat_uploads_ingest
    starlette_app.add_route(
        "/internal/chat/uploads/ingest",
        _internal_chat_uploads_ingest,
        methods=["POST"],
    )
    from internal_file_read import file_read_handler as _internal_file_read
    starlette_app.add_route(
        "/internal/file/read",
        _internal_file_read,
        methods=["GET"],
    )

    built_app = make_trace_middleware(starlette_app)

    # uvicorn expects the level name in lowercase ("debug" / "info" /
    # "warning" / "error" / "critical"). config.observability.log_level
    # is uppercased at parse time (config.py.load_config) for the
    # Python ``logging`` module's convention; lower it here so both
    # consumers get the form they expect from one source of truth.
    # An ``LOG_LEVEL`` env var still wins as an ops-side debugging
    # override — set it on the workspace process to bypass YAML
    # without a config edit + restart cycle.
    uvicorn_log_level = os.environ.get("LOG_LEVEL", config.observability.log_level).lower()
    server_config = uvicorn.Config(
        built_app,
        host="0.0.0.0",
        port=port,
        log_level=uvicorn_log_level,
    )
    server = uvicorn.Server(server_config)

    # 10b. Schedule initial_prompt self-message after server is ready.
    # Only runs on first boot — creates a marker file to prevent re-execution on restart.
    # Skipped on misconfigured boots: the self-message would route through the
    # platform back to /, hit the -32603 not-configured handler, and consume
    # the marker for a fire that can't actually run. Wait until the operator
    # fixes credentials and the workspace redeploys with adapter_ready=True.
    initial_prompt_task = None
    initial_prompt_marker = resolve_initial_prompt_marker(config_path)
    if adapter_ready and config.initial_prompt and not os.path.exists(initial_prompt_marker):
        # Write the marker UP FRONT (#71): if the prompt later crashes or
        # times out, we do NOT replay on next boot — that created a
        # ProcessError cascade where every message kept crashing. Operators
        # can always re-send via chat. Log loudly if the marker write
        # fails so the situation is visible.
        if not mark_initial_prompt_attempted(initial_prompt_marker):
            print(
                f"Initial prompt: WARNING — could not write marker at "
                f"{initial_prompt_marker}; this boot may replay if it crashes.",
                flush=True,
            )
        async def _send_initial_prompt():
            """Wait for server to be ready, then send initial_prompt as self-message."""
            # Wait for the A2A server to accept connections.
            # Use the SDK's own constant for the well-known path so this
            # probe and the route mounted by create_agent_card_routes()
            # never drift apart. Pre-fix this hardcoded the pre-1.x
            # well-known path string; a2a-sdk 1.x renamed it (the
            # canonical value lives in a2a.utils.constants now), so
            # the probe got 404 every attempt and fell through to
            # "server not ready after 30s, skipping" even though the
            # server was actually serving fine. Net effect: every
            # workspace silently dropped its `initial_prompt`.
            from a2a.utils.constants import AGENT_CARD_WELL_KNOWN_PATH
            ready = False
            for attempt in range(30):
                await asyncio.sleep(1)
                try:
                    async with httpx.AsyncClient(timeout=5.0) as client:
                        resp = await client.get(f"http://127.0.0.1:{port}{AGENT_CARD_WELL_KNOWN_PATH}")
                        if resp.status_code == 200:
                            ready = True
                            break
                except Exception:
                    continue

            if not ready:
                print("Initial prompt: server not ready after 30s, skipping", flush=True)
                return

            # Send initial prompt through the platform A2A proxy (not directly to self).
            # The proxy logs an a2a_receive with source_id=NULL (canvas-style),
            # broadcasts A2A_RESPONSE via WebSocket so the chat shows both the
            # prompt (as user message) and the response (as agent message).
            # Uses urllib in a thread to avoid asyncio/httpx streaming hangs.
            import json as _json
            import urllib.request

            def _do_send_sync():
                import time as _time
                payload = _json.dumps({
                    "method": "message/send",
                    "params": {
                        "message": {
                            "role": "user",
                            "messageId": f"initial-{_uuid.uuid4().hex[:8]}",
                            "parts": [{"kind": "text", "text": config.initial_prompt}],
                        },
                    },
                }).encode()

                # #220: include platform bearer token so the request isn't
                # silently rejected once any workspace has a live token on
                # file. Without this, initial_prompt 401s in multi-tenant
                # mode exactly like /registry/register did in #215.
                # X-Workspace-ID via self_source_headers() so the platform
                # tags the row source=agent — without it the canvas's
                # My Chat tab renders the initial_prompt as if the user
                # had typed it. See platform_auth.py for the full
                # explanation.
                headers = {
                    "Content-Type": "application/json",
                    **self_source_headers(workspace_id),
                }

                # Retry with backoff — the platform proxy may not be able to
                # reach us yet (container networking takes a moment to settle).
                max_retries = 5
                for attempt in range(max_retries):
                    try:
                        req = urllib.request.Request(
                            f"{platform_url}/workspaces/{workspace_id}/a2a",
                            data=payload,
                            headers=headers,
                        )
                        with urllib.request.urlopen(req, timeout=600) as resp:
                            resp.read()
                        print(f"Initial prompt: completed (status={resp.status})", flush=True)
                        break
                    except Exception as e:
                        if attempt < max_retries - 1:
                            delay = 2 ** attempt  # 1, 2, 4, 8, 16 seconds
                            print(f"Initial prompt: attempt {attempt + 1} failed ({e}), retrying in {delay}s...", flush=True)
                            _time.sleep(delay)
                        else:
                            print(f"Initial prompt: failed after {max_retries} attempts — {e}", flush=True)
                            return

                # Marker was already written up front (#71). Nothing to do here.

            print("Initial prompt: sending via platform proxy...", flush=True)
            loop = asyncio.get_event_loop()
            loop.run_in_executor(None, _do_send_sync)

        initial_prompt_task = asyncio.create_task(_send_initial_prompt())

    # 10c. Idle loop — reflection-on-completion / backlog-pull pattern.
    # Fires config.idle_prompt every config.idle_interval_seconds while the
    # workspace has no active task. This turns every role from "waits for cron"
    # into "self-wakes when idle" — the Hermes/Letta shape from today's
    # multi-framework survey (see docs/ecosystem-watch.md). Cost collapses to
    # event-driven in practice: the idle check is local (no LLM call, just
    # heartbeat.active_tasks==0), and the prompt only fires when there's
    # actually nothing to do. Gated on idle_prompt being non-empty so existing
    # workspaces upgrade opt-in — set idle_prompt in org.yaml defaults or
    # per-workspace to enable.
    idle_loop_task = None
    # Skipped on misconfigured boots — the self-fire would route to the
    # -32603 handler in a tight loop and consume cycles for no useful work.
    if adapter_ready and config.idle_prompt:
        # Idle-fire HTTP timeout. Kept tight relative to the fire cadence so a
        # hung platform doesn't accumulate dangling requests — a fire that
        # takes longer than the idle interval itself is almost certainly stuck.
        IDLE_FIRE_TIMEOUT_SECONDS = max(60, min(300, config.idle_interval_seconds))
        # Initial settle delay — never longer than 60s so cold-start races
        # don't stall the first fire, and never shorter than the configured
        # interval (short intervals shouldn't fire instantly on boot either).
        IDLE_INITIAL_SETTLE_SECONDS = min(config.idle_interval_seconds, 60)

        async def _run_idle_loop():
            """Self-sends config.idle_prompt periodically when the workspace is idle."""
            await asyncio.sleep(IDLE_INITIAL_SETTLE_SECONDS)

            import json as _json
            from urllib import request as _urlreq, error as _urlerr

            while True:
                try:
                    await asyncio.sleep(config.idle_interval_seconds)
                except asyncio.CancelledError:
                    return

                # Local idle check — no platform API call, no LLM call.
                # heartbeat.active_tasks == 0 means no in-flight work.
                if heartbeat.active_tasks > 0:
                    continue

                # Self-post the idle prompt via the platform A2A proxy (same
                # path as initial_prompt). The agent's own concurrency control
                # rejects if the workspace becomes busy between this check and
                # the post — that's the expected safety valve.
                payload = _json.dumps({
                    "method": "message/send",
                    "params": {
                        "message": {
                            "role": "user",
                            "messageId": f"idle-{_uuid.uuid4().hex[:8]}",
                            "parts": [{"kind": "text", "text": config.idle_prompt}],
                        },
                    },
                }).encode()

                def _post_sync():
                    # Returns (status_code, error_type) so the caller logs the
                    # actual outcome instead of a bare "post failed" line.
                    # #220: include auth_headers() on every idle fire. Without
                    # this, the idle loop 401s in multi-tenant mode.
                    # self_source_headers() adds X-Workspace-ID so the
                    # platform classifies the idle fire as source=agent
                    # rather than user-typed canvas input.
                    headers = {
                        "Content-Type": "application/json",
                        **self_source_headers(workspace_id),
                    }
                    try:
                        req = _urlreq.Request(
                            f"{platform_url}/workspaces/{workspace_id}/a2a",
                            data=payload,
                            headers=headers,
                        )
                        with _urlreq.urlopen(req, timeout=IDLE_FIRE_TIMEOUT_SECONDS) as resp:
                            resp.read()
                            return resp.status, None
                    except _urlerr.HTTPError as e:
                        return e.code, type(e).__name__
                    except _urlerr.URLError as e:
                        return None, f"URLError: {e.reason}"
                    except Exception as e:  # pragma: no cover — catch-all safety net
                        return None, type(e).__name__

                print(
                    f"Idle loop: firing (active_tasks=0, interval={config.idle_interval_seconds}s, "
                    f"timeout={IDLE_FIRE_TIMEOUT_SECONDS}s)",
                    flush=True,
                )
                loop_ref = asyncio.get_running_loop()

                def _log_result(future):
                    try:
                        status, err = future.result()
                        if err:
                            print(
                                f"Idle loop: post failed — status={status} err={err}",
                                flush=True,
                            )
                        else:
                            print(f"Idle loop: post ok status={status}", flush=True)
                    except Exception as e:  # pragma: no cover
                        print(f"Idle loop: executor callback crashed — {e}", flush=True)

                fut = loop_ref.run_in_executor(None, _post_sync)
                fut.add_done_callback(_log_result)

        idle_loop_task = asyncio.create_task(_run_idle_loop())

    try:
        await server.serve()
    finally:
        # 10d. Pre-stop serialization — GH#1391.
        # Capture in-memory state before the container exits so it survives
        # intentional pause and unplanned restart. All content is scrubbed
        # via lib.snapshot_scrub before being written to the config volume.
        try:
            from lib.pre_stop import build_snapshot, write_snapshot
            adapter_state = adapter.pre_stop_state() if adapter else {}
            snapshot = build_snapshot(heartbeat, adapter_state)
            write_snapshot(snapshot)
        except Exception as pre_stop_err:
            print(f"Warning: pre-stop serialization failed (continuing): {pre_stop_err}")

        # Cancel initial prompt if still running
        if initial_prompt_task and not initial_prompt_task.done():
            initial_prompt_task.cancel()
        # Cancel idle loop if running
        if idle_loop_task and not idle_loop_task.done():
            idle_loop_task.cancel()
        # Gracefully stop the Temporal worker background task on shutdown
        await temporal_wrapper.stop()


def main_sync():  # pragma: no cover
    """Synchronous entry point for the `molecule-runtime` console script.

    Declared in scripts/build_runtime_package.py as the wheel's entry-point
    target (`molecule-runtime = "molecule_runtime.main:main_sync"`). Removed
    silently during the pre-monorepo consolidation, which broke every
    workspace startup against 0.1.16/0.1.17/0.1.18 with `ImportError:
    cannot import name 'main_sync'`. The .github/workflows/runtime-pin-compat.yml
    smoke step is the regression gate.
    """
    asyncio.run(main())


if __name__ == "__main__":  # pragma: no cover
    main_sync()
