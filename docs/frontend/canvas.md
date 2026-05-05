# Canvas UI (Next.js Frontend)

The canvas is Molecule AI's operational UI. It is not just a graph viewer. It is the place where teams deploy workspaces, inspect live state, configure runtimes, browse files, watch activity, and chat with agents.

## Stack

- Next.js 15
- React Flow (`@xyflow/react`)
- Zustand
- WebSocket for live updates

## What The Canvas Represents

Each node is a **workspace role**, not a task node.

The node surface includes:

- workspace name
- tier
- status
- runtime/skill summary from the Agent Card
- current task / active work indicators

Molecule AI deliberately avoids explicit edge drawing. The hierarchy comes from `parent_id`, and nested rendering makes the org chart visible directly.

## First-Run Experience

Fresh canvases currently use two onboarding surfaces:

### Empty state

The center panel shows:

- up to six template cards from `GET /templates`
- a `+ Create blank workspace` action

Creating from this panel auto-selects the new workspace and opens the `Chat` tab.

### Onboarding wizard

A dismissible bottom-left wizard walks first-time users through:

1. creating a workspace
2. opening `Config`
3. setting an API key
4. opening `Chat`

The wizard tracks completion in local storage.

## Core Interactions

### Drag to nest

Users create hierarchy by dragging one node over another:

- overlap detection happens during drag
- valid targets highlight
- dropping updates `parent_id`
- dropping back on empty canvas removes the parent

This is how Molecule AI encodes teams: by hierarchy, not manually drawn edges.

### Right-click actions

Workspace context menu actions include:

- open `Details`, `Chat`, or `Terminal`
- restart
- duplicate
- export bundle
- expand to team / collapse team
- extract from team
- delete

### Template palette

The left palette lists templates from `GET /templates` and supports importing an agent folder as a new template.

### Bundle workflow

The canvas supports:

- bundle export
- bundle import by drag-and-drop
- duplicate via export + re-import

## Real-Time Model

The frontend uses a hybrid model:

1. initial hydration over HTTP
2. incremental updates over WebSocket

Important live event flows:

- structure events update the canvas store
- `TASK_UPDATED` updates task banners
- `AGENT_CARD_UPDATED` refreshes capability badges
- `A2A_RESPONSE` delivers browser-initiated chat responses instantly

Chat is now **WebSocket-first**, with polling kept as a recovery fallback instead of the primary response path.

## Side Panel

Selecting a workspace opens the right-side panel. The panel is resizable and currently includes **10 tabs**:

| Tab | Current role |
|---|---|
| `Chat` | Two sub-tabs: **My Chat** (user↔agent, `source=canvas`) and **Agent Comms** (agent↔agent A2A traffic, `source=agent`). WebSocket response delivery, activity-based history |
| `Activity` | Rich activity feed for A2A send/receive, task updates, logs, skill promotion, and full-trace entry points |
| `Details` | Basic metadata, runtime/skill summary, restart, peer list, delete |
| `Skills` | Read-only skill and capability display from the Agent Card |
| `Terminal` | WebSocket shell into the running workspace container |
| `Config` | Structured editor for `config.yaml`, runtime, skills, A2A, delegation, sandbox, secrets, and raw YAML |
| `Files` | Workspace file browser/editor for `/configs`, `/workspace`, `/home`, and `/plugins` |
| `Memory` | Key/value workspace memory view with TTL-capable entries |
| `Traces` | Langfuse traces |
| `Events` | Workspace-scoped structure events |

### Panel banners

The panel header area can show:

- **Needs Restart** when config/files/secrets changed but the workspace has not restarted yet
- **Current Task** when the runtime heartbeat reports in-flight work

## Config And Secrets UX

The `Config` tab now reflects the current platform model more closely than older docs did:

- structured sections for general config, runtime, skills/tools, A2A, delegation, and sandbox
- raw YAML toggle for direct editing
- merged workspace/global secrets view
- explicit `This Workspace` vs `Global (All Workspaces)` secret scopes
- workspace-level overrides over global keys
- editable Agent Card JSON

This is one of the most important recent UX shifts: global provider keys are no longer just a backend concept, they are visible and manageable from the panel.

## Files UX

The `Files` tab supports multiple roots: `/configs`, `/workspace`, `/home`, `/plugins`.

`/configs` is the main editable path. When the container is offline, the platform falls back to the host-side template/config directory when possible.

### Lazy Loading

The file tree uses **lazy loading** to avoid fetching the entire filesystem at once:

- Initial load: `GET /workspaces/:id/files?root=/workspace&depth=1` — returns only top-level entries
- Expanding a folder: `GET ...&path=.claude&depth=1` — fetches that folder's immediate children on demand
- A loading indicator ("…") shows on the folder arrow while fetching
- Expanded grandchildren are preserved when re-loading a parent directory
- The `buildTree()` utility deduplicates directory nodes when both a dir entry and its nested children exist in the flat file list

### Input Validation

- `path` parameter is validated against path traversal (`../` blocked)
- `depth` must be 1–5 (invalid values return 400)
- Shell arguments are quoted to handle paths with spaces or special characters

## Team Visualization

Expanded teams render as embedded child cards inside the parent node:

- children are hidden as top-level React Flow nodes
- nested cards show status, tier, skill summary, and descendant counts
- child cards are selectable
- children can be extracted back out of the team

This keeps the visual model aligned with Molecule AI's main idea: the org chart is the topology.

## Error Handling

The canvas includes:

- an application-wide React error boundary
- a hydration error banner with retry
- reconnecting WebSocket behavior
- toast notifications for user actions

So the UI now exposes more operational failure state directly instead of silently failing on first load.

## Related Docs

- [Quickstart](../quickstart.md)
- [Platform API](../api-protocol/platform-api.md)
- [Workspace Runtime](../agent-runtime/workspace-runtime.md)
