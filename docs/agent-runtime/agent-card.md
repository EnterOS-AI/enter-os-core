# Agent Card

Every workspace publishes an Agent Card at `/.well-known/agent-card.json`. This is a standard A2A document that describes the workspace's identity, capabilities, and how to communicate with it.

## Example

```json
{
  "name": "Reno Stars SEO Agent",
  "description": "Bilingual EN/ZH SEO page builder for Vancouver renovation companies",
  "version": "1.2.0",
  "url": "https://seo-agent.acme-demo.example.com",
  "capabilities": {
    "streaming": true,
    "pushNotifications": true
  },
  "defaultInputModes": ["text/plain", "application/json"],
  "defaultOutputModes": ["text/plain", "application/json", "text/html"],
  "skills": [
    {
      "id": "generate_seo_page",
      "name": "Generate SEO Landing Page",
      "description": "Creates a bilingual EN/ZH Next.js page targeting a renovation keyword",
      "tags": ["seo", "bilingual", "nextjs"],
      "examples": ["Generate a page targeting 'kitchen renovation Vancouver'"]
    }
  ],
  "supportedInterfaces": [
    { "protocol": "JSONRPC", "url": "https://seo-agent.acme-demo.example.com/a2a" }
  ]
}
```

## How the Agent Card Is Used

### By the Platform
- The platform reads the card when a workspace registers
- Stores the full card in Postgres (`workspaces.agent_card` JSONB column)
- Caches the workspace URL in Redis for fast resolution

### By the Canvas
- The canvas renders node UI **directly from the card**
- Node title comes from `name`
- Skill badges come from `skills[].name`
- Input/output port types come from `defaultInputModes` / `defaultOutputModes`

### By Peer Workspaces (Automatic Skill Injection)

When a workspace builds its system prompt, it pulls Agent Cards from all reachable peer workspaces (siblings, children, parent) and appends their skill descriptions. This way a calling workspace (e.g. Business Core) knows what it can ask the SEO agent to do without any manual wiring.

The mechanism:
1. On startup, the workspace queries the platform for peer Agent Cards via `GET /registry/:id/peers`
2. Skill descriptions from those cards are appended to the agent's system prompt
3. When a peer updates its card (`AGENT_CARD_UPDATED` event), the workspace rebuilds its system prompt automatically

This means adding a new skill to a workspace makes it immediately available to all reachable peers — no manual wiring, no restarts.

## Lifecycle

1. Workspace container starts
2. Agent Card is generated from the workspace config
3. A2A server publishes it at `/.well-known/agent-card.json`
4. Workspace sends `POST /registry/register` with the card
5. Platform stores the card in Postgres
6. Canvas reads the card and renders the node

## Live Updates

When a workspace's skills change at runtime:
1. Workspace rescans skills folder, rebuilds Agent Card
2. Workspace sends `POST /registry/update-card` with the new card
3. Platform stores the updated card, writes `AGENT_CARD_UPDATED` event
4. Platform broadcasts via WebSocket to **all subscribers** — both canvas clients and peer workspaces

Both canvas clients and workspace agents subscribe to the same platform WebSocket at `/ws`. The platform filters events server-side using `X-Workspace-ID` — each workspace only receives events about workspaces it can communicate with (via `CanCommunicate()`). Canvas clients receive all events (no workspace ID header).

When a peer workspace receives `AGENT_CARD_UPDATED`, it rebuilds its system prompt automatically. When the canvas receives it, it updates the node's skill badges.

## Related Docs

- [Core Concepts](../product/core-concepts.md) — What an Agent Card is
- [A2A Protocol](../api-protocol/a2a-protocol.md) — How the card fits into A2A
- [Workspace Runtime](./workspace-runtime.md) — How the card is generated at startup
- [Canvas UI](../frontend/canvas.md) — How the card drives node rendering
