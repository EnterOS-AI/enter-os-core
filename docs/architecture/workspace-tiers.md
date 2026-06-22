# Workspace Tiers

Four tiers control the security boundary for each workspace. Higher tiers get more system access but less isolation.

## Tier Overview

| Tier | Name | Container Flags | Use Case |
|------|------|----------------|----------|
| 1 | Sandboxed | Readonly rootfs, tmpfs /tmp, no `/workspace` mount | SEO, marketing, analysis — text processing only |
| 2 | Standard | Resource limits (512 MiB, 1 CPU), normal Docker + `/workspace` | Most agents — can read/write the codebase |
| 3 | Privileged | `--privileged` + host PID, Docker network | Dev team — privileged access with inter-container discovery |
| 4 | Full Access | Privileged + host network + Docker socket | DevOps, orchestrator — full host machine access |

## T1 — Sandboxed

Pure text/data processing. Docker container with no workspace mount — the agent can only see its own `/configs` directory. Readonly root filesystem with tmpfs at `/tmp` for scratch space. Used for agents that don't need codebase access (content writers, analysts, researchers).

## T2 — Standard (Default)

Normal Docker container with `/workspace` mounted (read-write) and resource limits applied (512 MiB memory, 1 CPU). The agent can read and modify the codebase. Used for most development and coordination agents. Still containerized — no host access beyond the bind-mounted directories. Unknown or zero tier values also default to T2 behavior for safety.

## T3 — Privileged

Privileged Docker container with:
- `--privileged` — full device access, can run Docker-in-Docker
- `--pid=host` — can see host processes

Stays on the Docker network (not host network) so containers can still reach each other by name. Host networking would conflict with Docker networks and cause port collisions when multiple T3 containers run simultaneously.

Used for dev team agents that need elevated privileges but still participate in inter-container A2A communication.

## T4 — Full Access

Everything from T3 plus:
- `--network=host` — shares host network stack (can bind ports, access localhost services)
- Docker socket mount (`/var/run/docker.sock`) — can manage other containers

Used for DevOps agents, system administration, and orchestrator agents that need to interact with the host machine directly. The container has near-VM-level access to the host.

## How Tiers Work

- The tier is stored in both the database (`workspaces.tier`) and `config.yaml`
- The provisioner reads the tier via `ApplyTierConfig()` and sets Docker flags accordingly
- The canvas shows a tier badge on each node (T1/T2/T3/T4)
- From A2A's perspective, **all tiers look identical** — same protocol, same Agent Card, same message format
- Tier changes take effect on next restart

## Related Docs

- [Provisioner](./provisioner.md) — How tiers affect deployment
- [Architecture](./architecture.md) — Where tiers fit in the system
