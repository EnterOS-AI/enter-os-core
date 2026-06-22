# Communication Rules

The hierarchy IS the topology. There is no manual connection wiring — communication is derived automatically from the parent/child structure.

## The Rules

| Direction | Allowed? | Example |
|-----------|----------|---------|
| Sibling <-> sibling | Yes | Marketing <-> Developer PM |
| Parent -> child (PM delegates down) | Yes | Developer PM -> Frontend Agent |
| Child -> parent (report up to PM) | Yes | Frontend Agent -> Developer PM |
| Skip levels (grandchild -> grandparent) | No | Frontend Agent -> Business Core |
| Cross-team (different parents) | No | Frontend Agent -> Operations |

## Visual Example

```
Business Core
+-- Marketing          <--can talk--> Developer PM
+-- Developer PM       <--can talk--> Operations
|   +-- Frontend       <--can talk--> Backend
|   +-- Backend        <--can talk--> QA PM
|   +-- QA PM
|       +-- Auto Test  <--can talk--> Manual Review
|       +-- Manual Review
+-- Operations
```

- Developer PM can talk to Marketing and Operations (siblings) AND down to Frontend, Backend, QA PM (its children)
- Frontend can only talk to Backend and QA PM (siblings) and up to Developer PM (its parent)
- Frontend **cannot** talk to Marketing, Business Core, or Operations
- Auto Test can only talk to Manual Review (sibling) and up to QA PM (its parent)

## Access Check

The platform validates every discovery request with a hierarchy check:

```go
func CanCommunicate(callerID, targetID string) bool {
    caller := db.GetWorkspace(callerID)
    target := db.GetWorkspace(targetID)

    // siblings — same parent (including root-level where both have no parent)
    if caller.ParentID != nil && target.ParentID != nil &&
       *caller.ParentID == *target.ParentID {
        return true
    }
    // root-level siblings — both have no parent
    if caller.ParentID == nil && target.ParentID == nil {
        return true
    }

    // parent talking to child
    if caller.ID == target.ParentID {
        return true
    }

    // child talking up to parent
    if target.ID == caller.ParentID {
        return true
    }

    return false
}
```

`GET /registry/discover/:id` reads the caller's identity from the `X-Workspace-ID` header, runs `CanCommunicate()`, and returns **403 Forbidden** if the caller isn't allowed.

## Peer Discovery

Instead of a connections table, the platform derives reachable workspaces from the hierarchy:

```
GET /registry/:id/peers
```

Returns: siblings + children + parent (all workspaces this one can communicate with).

```python
async def get_reachable_workspaces(workspace_id: str) -> list:
    ws = db.GetWorkspace(workspace_id)
    reachable = []

    # siblings — same parent
    if ws.parent_id:
        siblings = db.GetChildren(ws.parent_id)
        reachable += [s for s in siblings if s.id != workspace_id]

    # children — own sub-workspaces
    children = db.GetChildren(workspace_id)
    reachable += children

    # parent — can talk up
    if ws.parent_id:
        parent = db.GetWorkspace(ws.parent_id)
        reachable.append(parent)

    return reachable
```

## What This Replaces

The hierarchy-based model removes several components:

| Removed | Replaced by |
|---------|-------------|
| `workspace_connections` table | `parent_id` on `workspaces` table |
| `CONNECTION_CREATED` / `CONNECTION_REMOVED` events | `WORKSPACE_EXPANDED` / `WORKSPACE_COLLAPSED` events |
| `/topology/connect` endpoint | Nesting via drag-into on canvas |
| Canvas edge drawing UI | Edges auto-rendered from hierarchy |
| Workspace whitelist table | `CanCommunicate()` hierarchy check |
| Bundle connection definitions | Bundle `sub_workspaces` array |

## Canvas Behavior

- **No edge drawing.** Users don't wire workspaces — they **nest** them
- Edges render **automatically** from parent/child relationships
- The visual is a true **org chart**, not a flowchart
- Dragging a workspace **inside** another workspace nests it as a sub-workspace

## Why This Is Better

The org chart IS the access control policy. Simpler schema, simpler security, simpler canvas. No configuration drift between "who should talk to whom" and "who can talk to whom."

## Related Docs

- [Team Expansion](../agent-runtime/team-expansion.md) — How nesting works
- [System Prompt Structure](../agent-runtime/system-prompt-structure.md) — How peer capabilities are injected
- [A2A Protocol](./a2a-protocol.md) — Discovery flow
- [Platform API](./platform-api.md) — Endpoint reference
