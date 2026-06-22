# Code Sandbox

The code sandbox isolates agent-generated code execution — specifically the `run_code` tool that executes dynamically generated scripts. Not user-submitted code (there is no user code submission in EnterOS) — the agent's own generated code is what needs sandboxing.

## What Gets Sandboxed

| | Runs in | Why |
|---|---------|-----|
| Agent-generated code execution | Sandbox | e.g. "write and run this script" |
| pip installs from skill requirements | Sandbox | Untrusted package code |
| Filesystem writes outside `/memory` and `/configs` | Sandbox | Prevent container escape |
| `SKILL.md` loading | Workspace container | Just file reads |
| LangChain `@tool` functions | Workspace container | Just Python function calls |
| A2A HTTP calls to peers | Workspace container | Network calls to known endpoints |
| Platform heartbeat/registry calls | Workspace container | Known endpoints |

The sandbox only activates when the agent calls a `run_code` tool that executes dynamic code. Regular skill tools — API calls, file reads, data processing — run directly in the workspace container without sandbox overhead.

## Configuration

```yaml
# config.yaml
tier: 3
sandbox:
  backend: docker    # docker | firecracker | e2b | none
  memory_limit: 256m
  cpu_limit: 0.5
  network: false
  timeout: 30s
```

## Sandbox by Tier

| Tier | `sandbox.backend` | Reason |
|------|--------------------|--------|
| 1, 2 | `none` | No `run_code` tool available — tools are just API calls |
| 3 | `docker` (MVP), `firecracker` or `e2b` (production) | Agent can generate and run code |
| 4 | `none` | Full-host access tier — no extra sandbox boundary is added by default |

Tier 4 doesn't add a second sandbox by default because the workspace already runs with host-level privileges. If you need isolated code execution at that tier, treat it as an explicit defense-in-depth decision rather than an assumption baked into the current provisioner.

## How It Works (Tier 3)

Each code execution spawns a throwaway container:

1. Agent calls `run_code(code="import pandas as pd; ...")`
2. Sandbox creates a temporary Docker container (Docker-in-Docker)
3. Container runs with: network disabled, memory capped, read-only filesystem, CPU limited
4. Code executes inside the throwaway container
5. Output (stdout, stderr, return value) is captured
6. Throwaway container is destroyed immediately after

```python
@tool(description="Execute code safely")
async def run_code(code: str) -> dict:
    result = docker.run(
        image="python:3.11-slim",
        command=["python", "-c", code],
        remove=True,
        network_disabled=True,
        mem_limit="256m",
        read_only=True,
    )
    return {"output": result.output}
```

The workspace container itself is never at risk — the generated code can't escape the sandbox.

## Backends

### docker (MVP)

Docker-in-Docker. The workspace container runs Docker and spawns child containers for code execution. Simple, works everywhere Docker is available.

### firecracker

MicroVM-based isolation. Faster cold starts than Docker, with a stronger boundary than standard containers. Better for production workloads with many concurrent code executions.

### e2b

Cloud-hosted sandboxes via [E2B](https://e2b.dev). No local Docker needed. The workspace sends code to E2B's API and gets results back. Good for hosted deployments where you don't want to manage Docker-in-Docker.

## Key Properties

- Skill code never changes — only the backend config
- Each execution is isolated — no shared state between runs
- Containers are destroyed after every run
- Network is disabled by default (can be enabled per-sandbox if needed)
- Memory is capped to prevent resource exhaustion

## Related Docs

- [Workspace Tiers](../architecture/workspace-tiers.md) — Which tiers need sandboxing
- [Config Format](../agent-runtime/config-format.md) — Sandbox configuration in `config.yaml`
- [Provisioner](../architecture/provisioner.md) — Container deployment details
- [Skills](../agent-runtime/skills.md) — Skill tools that may use the sandbox
