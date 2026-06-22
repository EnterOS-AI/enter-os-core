# Workspace Runtime

The `workspace/` directory is EnterOS's unified runtime image. Every provisioned workspace starts from this image, loads its own config, selects a runtime adapter, registers an Agent Card, exposes A2A, and joins the platform heartbeat/activity loop.

## Runtime Matrix In Current `main`

Current `main` ships four maintained adapters:

- `claude-code`
- `codex`
- `hermes`
- `openclaw`

This is the merged runtime surface today. Branch-level experiments such as NemoClaw are separate and should be treated as roadmap/WIP, not merged support.

Adapter-specific behavior is documented in [Agent Runtime Adapters](./cli-runtime.md).

## What The Runtime Is Responsible For

- loading `config.yaml`
- running preflight checks before the workspace goes live
- selecting an adapter based on `runtime`
- loading local skills plus plugin-mounted shared rules/skills
- constructing an Agent Card
- serving A2A over HTTP
- registering with the platform and sending heartbeats
- reporting activity and task state
- proxying durable memory tools through the v2 memory plugin
- hot-reloading skills while the workspace is running

## Environment Model

Common runtime environment variables:

```bash
WORKSPACE_ID=ws-123
WORKSPACE_CONFIG_PATH=/configs
PLATFORM_URL=http://platform:8080
PARENT_ID=
LANGFUSE_HOST=http://langfuse-web:3000
LANGFUSE_PUBLIC_KEY=...
LANGFUSE_SECRET_KEY=...
```

Important behavior:

- `WORKSPACE_CONFIG_PATH` points at the mounted config directory for that workspace.
- Memory MCP tools route through the platform's v2 memory plugin (see Memory Architecture doc); there is no per-workspace memory env var anymore — the plugin sidecar is provisioned at the tenant EC2 boundary.

## Startup Sequence

At a high level, `workspace/main.py` does this:

1. Initialize telemetry.
2. Load `config.yaml`.
3. Run preflight validation.
4. Build the heartbeat loop.
5. Resolve the adapter from `config.runtime`.
6. Let the adapter run `setup()` and build an executor.
7. Build the Agent Card from loaded skills and runtime config.
8. Register the workspace with `POST /registry/register`.
9. Start heartbeats.
10. Start the skill watcher when skills are configured.
11. Serve the A2A app through Uvicorn.

## Core Runtime Pieces

| File | Responsibility |
|---|---|
| `main.py` | Entry point, adapter bootstrap, Agent Card registration, heartbeat startup, initial prompt execution |
| `config.py` | Parses `config.yaml` into the runtime config dataclasses |
| `adapters/` | Adapter registry and adapter implementations |
| `claude_sdk_executor.py` | `ClaudeSDKExecutor` — Claude Code runtime via `claude-agent-sdk` (replaces subprocess) |
| `executor_helpers.py` | Shared helpers for all executors: memory, delegation, heartbeat, system prompt, error sanitization |
| `a2a_executor.py` | Shared LangGraph execution bridge and current-task reporting |
| `cli_executor.py` | `CLIAgentExecutor` — subprocess executor for Codex, Ollama, custom runtimes |
| `skills/loader.py` | Parses `SKILL.md`, loads tool modules, returns loaded skill metadata |
| `skills/watcher.py` | Hot reload path for skill changes |
| `plugins.py` | Scans mounted plugins for shared rules, prompt fragments, and extra skills |
| `tools/memory.py` | Agent memory tools (route through the platform's v2 memory plugin via the workspace-server proxy) |
| `coordinator.py` | Coordinator-only delegation path for team leads |

## Skills, Plugins, And Hot Reload

The runtime combines three sources of capability:

1. **workspace-local skills** from `skills/<skill>/SKILL.md`
2. **plugin-mounted rules and shared skills** from `/plugins`
3. **built-in tools** like delegation, approval, memory, sandbox, and telemetry helpers

Hot reload matters because the runtime is designed to keep a workspace alive while its capability surface evolves:

- edit `SKILL.md`
- add/remove skill files
- update tool modules
- modify config prompt references

The watcher rescans the skill package, rebuilds the agent tool surface, and updates the Agent Card so peers and the canvas reflect the new capabilities.

## Memory Integration

The runtime keeps the agent-facing contract stable:

- `commit_memory(content, scope)` — legacy MCP name, routed through the
  v2 plugin's scope→namespace shim
- `commit_memory_v2(content, namespace)` — direct v2 surface
- `search_memory(query, namespace?)` — v2 plugin search with FTS +
  semantic scoring when the plugin declares the capability

All writes land in the workspace's `workspace:<workspace_id>` namespace
unless the agent passes an explicit one. Cross-workspace namespaces
(`team:<root>`, `org:<root>`) follow the platform's namespace ACL
(`internal/memory/namespace/resolver.go`). There is no per-workspace
memory env var on the runtime side — the plugin lives on the tenant
EC2 at `localhost:9100`, spawned by `entrypoint-tenant.sh` when
`MEMORY_PLUGIN_URL` is present in the tenant-server's env (CP
user-data injects it during tenant provisioning). The workspace-server
proxies all memory calls through that sidecar.

See [Memory Architecture](../architecture/memory.md) for the full
backend story.

## Coordinator Enforcement

`coordinator.py` is not a generic “smart agent” mode. It is intentionally strict:

- coordinators delegate
- coordinators synthesize
- coordinators do not quietly do the child work themselves

This matters because EnterOS wants hierarchy to remain operationally real, not cosmetic.


## Remote Agent Registration (External Workspaces)

External workspaces run outside the platform's Docker infrastructure — on your laptop, a cloud VM, an on-prem server, or a CI/CD agent. They register via the platform API and send heartbeats to stay live on the canvas.

### How it differs from Docker workspaces

| | Docker workspace | External workspace |
|---|---|---|
| Provisioning | Platform spins up a container | You provide the machine; platform just tracks it |
| Liveness | Docker health sweep | Heartbeat TTL (90s offline threshold) |
| Registration | Automatic at container start | Manual: `POST /workspaces` + `POST /registry/register` |
| Token | Inherited from container env | Minted at registration, shown once |
| Secrets | Baked in image or env var | Pulled from platform at boot via `GET /workspaces/:id/secrets` |

### Registration flow

**1. Create the workspace:**

```bash
curl -X POST http://localhost:8080/workspaces \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-remote-agent",
    "runtime": "external",
    "external": true,
    "url": "https://my-agent.example.com/a2a",
    "parent_id": "ws-pm-123"
  }'
```

Returns `{ "id": "ws-xyz", "platform_url": "http://localhost:8080" }`.

**2. Register the agent with the platform:**

```bash
curl -X POST http://localhost:8080/registry/register \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <admin-token>" \
  -d '{
    "workspace_id": "ws-xyz",
    "name": "my-remote-agent",
    "description": "Runs on a cloud VM in us-east-1",
    "skills": ["research", "summarization"],
    "url": "https://my-agent.example.com/a2a"
  }'
```

The platform returns a 256-bit bearer token — save it, it is shown only once.

**3. Pull secrets at boot:**

```bash
curl http://localhost:8080/workspaces/ws-xyz/secrets \
  -H "Authorization: Bearer <your-token>"
```

Returns `{ "ANTHROPIC_API_KEY": "...", "OPENAI_API_KEY": "..." }`. No credentials baked into images or env files.

**4. Send heartbeats every 30 seconds:**

```bash
curl -X POST http://localhost:8080/registry/heartbeat \
  -H "Authorization: Bearer <your-token>" \
  -H "Content-Type: application/json" \
  -d '{
    "workspace_id": "ws-xyz",
    "status": "online",
    "task": "analyzing Q1 sales data",
    "error_rate": 0.0
  }'
```

If the platform misses two consecutive heartbeats, the workspace shows offline on the canvas.

**5. A2A with `X-Workspace-ID` header:**

When sending A2A messages to sibling or parent workspaces, include the header so the platform can verify mutual auth:

```bash
curl -X POST http://localhost:8080/workspaces/ws-pm-123/a2a \
  -H "Authorization: Bearer <your-token>" \
  -H "X-Workspace-ID: ws-xyz" \
  -H "Content-Type: application/json" \
  -d '{"type": "status_report", "payload": {...}}'
```

### Behind NAT — Cloudflare Tunnel / ngrok

If the agent machine has no public IP, use an outbound tunnel:

```bash
# ngrok
ngrok http 8000 --url https://my-agent.ngrok.io

# Cloudflare Tunnel
cloudflared tunnel run --token <token>

# Register the tunnel URL (not localhost)
curl -X POST http://localhost:8080/registry/update-card \
  -H "Authorization: Bearer <your-token>" \
  -d '{"workspace_id": "ws-xyz", "url": "https://my-agent.ngrok.io/a2a"}'
```

The agent initiates the outbound WebSocket to the platform — no inbound ports need to be opened on the firewall.

### Revocation and re-registration

To revoke and re-register:

```bash
# Delete the workspace
curl -X DELETE http://localhost:8080/workspaces/ws-xyz \
  -H "Authorization: Bearer <admin-token>"

# Create fresh (new workspace_id, new token)
```

Re-registration with the same `workspace_id` does not issue a new token — use the token saved from first registration.

### Related docs

- Full step-by-step: [External Agent Registration Guide](../guides/external-agent-registration.md)
- Tutorial with CI/CD examples: [Register a Remote Agent](../tutorials/register-remote-agent.md)
- API reference: [Registry and Heartbeat](../api-protocol/registry-and-heartbeat.md)

## A2A And Registration

Each workspace exposes an A2A server, builds an Agent Card, and registers with the platform. The platform is used for:

- discovery
- liveness
- event fanout
- proxying browser-initiated A2A calls

But the long-term collaboration model remains direct workspace-to-workspace communication via A2A.

## Known Limitations

### Playwright / browser system libs are not installed

The base `molecule-ai-workspace-runtime` image (`workspace/Dockerfile`) is built on `python:3.11-slim` with Node.js 22, git, and `gh` — about 500 MB. It deliberately **does not** include the system libraries Chromium needs (`libnss3`, `libatk-bridge2.0-0`, `libxkbcommon0`, `libcups2`, `libdrm2`, `libxcomposite1`, `libxdamage1`, `libxrandr2`, `libgbm1`, `libpango-1.0-0`, `libasound2`, etc.). Adding them would inflate the image by ~200–250 MB (~40%) for every workspace, even though only frontend / QA workspaces ever launch a browser.

Practical consequences:

- `npx playwright test` (and any other Chromium-driven E2E tooling) **will fail at browser launch** when run from inside an in-container workspace agent.
- The error surface is missing-shared-object messages such as `error while loading shared libraries: libnss3.so` or `Host system is missing dependencies to run browsers`.
- Unit and integration tests (Vitest, Jest, etc.) that don't spawn a real browser are unaffected.

Recommended workflow:

1. **Run E2E in CI**, not in-container. The Gitea Actions self-hosted runner (and the GitHub Actions runner used by mirror repos) has the full Playwright dep set installed and is the supported surface for E2E. Push a branch, let CI run the suite.
2. **Local debugging** of a single failing spec is best done on a developer laptop with `npx playwright install-deps` run once.
3. **In-container iteration** on test logic itself is fine — write specs, lint them, type-check them — just don't expect `playwright test` to actually launch a browser.

If a particular workspace role genuinely needs in-container E2E (a dedicated QA template, for instance), the right place to layer Playwright deps is in a **role-specific adapter template image** that does `FROM molecule-ai-workspace-runtime:<tag>` and adds `RUN npx playwright install-deps`. Open a request against `molecule-ai-workspace-runtime` if you need this template stamped.

Tracking issue: [molecule-ai/molecule-app#7](https://git.moleculesai.app/molecule-ai/molecule-app/issues/7).

## Related Docs

- [Agent Runtime Adapters](./cli-runtime.md)
- [Skills](./skills.md)
- [Config Format](./config-format.md)
- [System Prompt Structure](./system-prompt-structure.md)
- [Memory Architecture](../architecture/memory.md)
