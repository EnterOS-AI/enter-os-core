# System Prompt Structure

When a workspace agent starts (or rebuilds its prompt), the system prompt is assembled in a specific order: **specific to general** — the agent's own identity first, then what it can do, then what it can delegate.

## Assembly Order

```
1. Prompt files               — agent's core identity and instructions (system-prompt.md or custom list)
2. Parent context             — shared files from direct parent (if child workspace)
3. Platform rules & prompts   — plugin rules, coordinator prompt
4. Skill instructions         — SKILL.md body from each loaded skill
5. Reachable workspace cards  — Agent Card skill descriptions from peers
6. Delegation failure handling — always appended
```

### 1. system-prompt.md

The workspace's core identity. Written by the workspace author. Defines the agent's role, personality, and high-level instructions.

Example for a Developer PM:
```markdown
You are a Developer PM. You coordinate a development team to build features.
When you receive a task, break it into sub-tasks and delegate to your team.
Always review work before reporting completion to the caller.
```

### 2. Team-shared knowledge (on demand)

Team-scoped knowledge is no longer injected at boot. The previous
`shared_context` field + `GET /workspaces/{parent_id}/shared-context`
fetch was removed; agents now pull team-shared knowledge on demand via
memory v2's `team:<id>` namespace using the `recall_memory` MCP tool.

This shifts cost from "every boot, always" to "only when the agent
asks", and lets team members write to the shared store from anywhere
that can resolve the namespace (canvas Memory tab, agent
`commit_memory`, admin import). For large blob-shaped artefacts (full
architecture docs, brand assets, PDFs) see RFC #2789 (platform-owned
shared file storage).

### 3. Skill Instructions

The body (non-frontmatter) content of each `SKILL.md` from loaded skills. Appended in the order listed in `config.yaml`. This tells the agent what it can do directly.

### 3. Reachable Workspace Agent Cards

Skill descriptions from Agent Cards of all reachable workspaces (siblings, sub-workspaces, parent). This tells the agent what it can delegate.

The workspace queries the platform for reachable peers and injects their capabilities:

```python
async def build_system_prompt(config_path: Path) -> str:
    # 1. own identity
    prompt = load_markdown(config_path / "system-prompt.md")

    # 2. own skills
    for skill in loaded_skills:
        prompt += f"\n\n## Skill: {skill.name}\n{skill.instructions}"

    # 3. reachable workspace capabilities
    peers = await platform.get(f"/registry/{WORKSPACE_ID}/peers")
    for peer in peers:
        card = peer["agent_card"]
        prompt += f"\n\n## Available Workspace: {card['name']}\n"
        prompt += f"Description: {card['description']}\n"
        for skill in card.get("skills", []):
            prompt += f"- {skill['name']}: {skill['description']}\n"

    return prompt
```

## When the Prompt Is Rebuilt

The workspace subscribes to the platform WebSocket (with `X-Workspace-ID` header) and receives filtered events about reachable peers. The system prompt is rebuilt automatically when any of these events occur:

| Trigger | Source | What changed |
|---------|--------|-------------|
| Startup | — | Initial build |
| File watcher detects config change | Local filesystem | Own skills, model, or system prompt changed |
| `AGENT_CARD_UPDATED` received | Platform WebSocket | A peer's capabilities changed |
| `WORKSPACE_ONLINE` / `WORKSPACE_OFFLINE` | Platform WebSocket | A peer came online or went offline |
| `WORKSPACE_EXPANDED` | Platform WebSocket | A peer expanded into a team |
| `WORKSPACE_REMOVED` | Platform WebSocket | A peer was deleted |

## Delegation Behavior

On each new task, the agent checks the registry for its sub-workspaces and decides what to delegate for best efficiency. The agent uses the injected capability descriptions to make routing decisions — no explicit routing config is needed.

For example, if Developer PM's prompt includes:
```
## Available Workspace: Frontend Agent
- Build React Components: Creates React components from Figma designs

## Available Workspace: Backend Agent
- Build API Endpoints: Creates REST API endpoints with validation

## Available Workspace: QA Agent
- Run Test Suite: Executes automated tests and reports results
```

Then when Developer PM receives "build the login feature", it naturally delegates the UI to Frontend, the API to Backend, and testing to QA — based purely on the skill descriptions in its prompt.

## Human-in-the-Loop (Hierarchical Approval)

LangGraph natively supports pausing an agent to ask for human approval. Molecule AI extends this with **hierarchical escalation** — an agent can pause and escalate approval up the workspace hierarchy.

### How It Works

When an agent encounters something that requires approval (a destructive action, an expensive operation, or something outside its stated authority):

1. The agent pauses its current task
2. The agent sends an approval request **up to its parent workspace** via A2A
3. The parent workspace's agent decides:
   - **Approve** — the child resumes
   - **Deny** — the child aborts or takes an alternative action
   - **Escalate** — the parent doesn't have authority either, so it escalates to **its** parent
4. Escalation continues up the hierarchy until either a workspace approves/denies or the request reaches the **root workspace**
5. The root workspace (top of the org chart) surfaces the request to the **human user** via the canvas UI

### Example Flow

```
Frontend Agent wants to delete the production database schema
      |
      v
Pauses, asks Developer PM for approval (parent)
      |
      v
Developer PM: "This is too destructive, I need to escalate"
      |
      v
Asks Business Core for approval (its parent)
      |
      v
Business Core is the root — surfaces to human on canvas
      |
      v
Human approves or denies via canvas UI
      |
      v
Decision flows back down: Business Core -> Developer PM -> Frontend Agent
```

### What Triggers Escalation

The agent's system prompt defines what actions are within its authority and what requires approval. This is part of the workspace author's configuration — not hardcoded. Typical patterns:

- Destructive actions (deleting data, removing infrastructure)
- Actions above a cost threshold
- Actions not explicitly authorized in the system prompt
- First-time actions the agent hasn't done before

## Related Docs

- [Skills](./skills.md) — Skill package structure
- [Agent Card](./agent-card.md) — How cards drive delegation
- [Workspace Runtime](./workspace-runtime.md) — Where prompt is assembled
- [Communication Rules](../api-protocol/communication-rules.md) — Who can talk to whom
