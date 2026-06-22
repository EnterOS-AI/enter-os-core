# Glossary — EnterOS terminology vs. ecosystem neighbors

The agent-infra ecosystem has coalesced around a common vocabulary that
nevertheless means different things in different projects. This page
defines how EnterOS uses each term and flags the conflicts you're
most likely to hit when reading adjacent documentation.

Cross-referenced from
[`docs/ecosystem-watch.md`](./ecosystem-watch.md) — when a new project
lands in the watch list with a colliding term, add a row here.

## Core terms

| Term | What we mean | What others mean |
|------|--------------|------------------|
| **harness** | The Claude Code execution environment: `~/.claude/settings.json`, hooks, skills, MCP servers, the surrounding CLI. The layer *around* the agent that enforces behavior before tool calls fire. | **OpenHarness** (`HKUDS/OpenHarness`): a research framework wrapper for studying agent internals — closer to a probe than an enforcement layer. |
| **workspace** | An isolated Docker container running one agent with a role, git state, hook pipeline, runtime image, and per-workspace bearer token. Long-lived — a workspace is provisioned once and stays up. | **Paperclip**: closer to "employee" (a persona with memory). **Hermes**: closer to "project" (a unit of work scoped by time). In EnterOS, persona = role, unit of work = task/delegation. |
| **plugin** | A directory under `plugins/` packaging one or more skills or an MCP server wrapper, installable per-workspace via `POST /workspaces/:id/plugins`. Governed by `plugin.yaml`. | **Langflow**: a visual UI node / component in a flowchart. **CrewAI**: a Python-importable callable registered as a capability. |
| **agent** | A persistent containerized workspace running continuously — an identity with memory, a role, and a schedule. Not a one-shot invocation. | Most frameworks (AutoGPT, LangChain agents, OpenAI Assistants): a stateless function-call loop. No persistence between invocations unless explicitly checkpointed. |
| **flow** | A task execution within a workspace — a request enters, the agent runs tools, emits a response, logs activity. No explicit graph abstraction. | **Langflow**: a directed graph of nodes you author visually. **LangGraph**: a stateful graph of callable nodes. Our "flow" is an imperative timeline, not a graph. |
| **team** | A named cluster of workspaces under a PM . Used for role grouping in Canvas. | **CrewAI**: a "crew" is a sequence of agents that pass a task through a declared order. Our "team" is an org-chart abstraction, not an execution order. |
| **skill** | A directory with `SKILL.md` that an agent invokes via the `Skill` tool. Skills are documentation + optional scripts that teach an agent a recipe. | **Anthropic Skills API**: nearly identical. **CrewAI tool**: closer to our plugin's MCP tool, not our skill. |
| **channel** | An outbound/inbound social integration (Telegram, Slack, …) per-workspace, wired in `workspace_channels`. | Slack's "channel": the container for messages. We use "channel" for the adapter + credentials, not the conversation itself. |
| **runtime** | The execution engine image tag for a workspace: one of `langgraph`, `claude-code`, `openclaw`, `crewai`, `autogen`, `deepagents`, `hermes`. | **LangGraph runtime**: the Python process running the graph. We use "runtime" for the Docker image + adapter pairing, not the inner process. |

## GitHub Awesome Copilot disambiguation

[`github/awesome-copilot`](https://github.com/github/awesome-copilot) (30 k+ ★) uses
four terms that collide directly with EnterOS vocabulary. The scopes are different
enough that reading Copilot documentation while working in this repo causes genuine
confusion. Use this table as a quick reference.

| Term | EnterOS meaning | awesome-copilot meaning |
|------|-----------------|------------------------|
| **Skills** | A directory under the harness with a `SKILL.md` file; injected into the agent's system prompt and invoked with the `Skill` tool (slash-command style). Teaches an agent a reusable recipe. | Instruction + asset bundles that extend GitHub Copilot Chat inside VS Code. Installed per-extension, not per-agent. Closer to our **hooks** + **CLAUDE.md** combined. |
| **Plugins** | A directory under `plugins/` with `plugin.yaml` + optional Python MCP tool modules. Installed per-workspace via the platform API. Extend what an agent can *do* at runtime. | Curated bundles of agent definitions, skill packs, and instructions distributed via the VS Code Marketplace. Higher-level packaging than our plugins — closer to our **org-templates**. |
| **Agents** | A persistent, containerized workspace running one role continuously. Has identity, memory, a git-pinned runtime image, and a scoped bearer token. Long-lived — provisioned once. | GitHub Copilot extensions connected via MCP or the Copilot extension API. Stateless per-session invocations; no persistent container or bearer-token-scoped identity. Closer to our **skills with MCP tools**. |
| **Hooks** | Scripts wired into `~/.claude/settings.json` under `PreToolUse`, `PostToolUse`, `PreCompact`, etc. Fire synchronously inside the Claude Code harness before/after tool calls. | Session-level lifecycle callbacks in GitHub Copilot extensions (e.g., on chat open, on request send). Conceptually similar name; completely different runtime and trigger model. |
| **Instructions** | `CLAUDE.md` (repo-committed) or `/configs/system-prompt.md` (per-workspace container). Shape agent behavior at startup and throughout sessions. | `.github/copilot-instructions.md` — a prompt-injection file that Copilot prepends to every chat context in the repo. Same intent (steer model behavior), different mechanism and scope. |
| **Agentic Workflows** | A2A delegation: one workspace fires `delegate_task` / `delegate_task_async` to peers; tasks route through the team hierarchy via the platform proxy. | Multi-step Copilot orchestrations inside VS Code where Copilot autonomously invokes tools across multiple turns. No persistent inter-agent communication channel. |

**Rule of thumb:** if you are reading an awesome-copilot README and see one of these
terms, mentally substitute the row above before mapping it onto a EnterOS concept.
The naming overlap is historical coincidence — the architectures are distinct.

## Near-miss terms

These don't appear in the table above because we don't use them in the
same sense as the reference project — flagged so you don't confuse the
two:

- **crew** (CrewAI) — we don't use this term; the analogue is **team**.
- **tool** — a LangChain `@tool`-decorated callable, shared vocabulary with most frameworks. No conflict, but "skill" is the user-facing abstraction an agent picks up from `SKILL.md`; "tool" is the thing under the hood.
- **component** (Langflow) — no analogue. We have **plugin** (role-level), **skill** (agent-recipe-level), and **adapter** (runtime-level). A Langflow "component" is visual; ours is structural.
- **pipeline** — overloaded in our repo: CI pipeline, ingestion pipeline, etc. Always use the qualifier.

## When to update this file

- A project enters `docs/ecosystem-watch.md` with a term that collides with ours → add a row.
- We rename an internal term (e.g., "workspace" to "agent") → update **every** row that references it, update `CLAUDE.md`, update `README.md`, update `docs/architecture.md`.
- A user reports confusion reading a cross-project tutorial → add a row with the confusing term.

## Cross-references

- [`README.md`](../README.md) — top-level stack description.
- [`CLAUDE.md`](../CLAUDE.md) — operational vocabulary for agents in this repo.
- [`docs/ecosystem-watch.md`](./ecosystem-watch.md) — source of truth for adjacent-project claims.
- [`docs/architecture.md`](./architecture.md) — deeper definitions for workspace, canvas, platform.
