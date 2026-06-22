# RFC: User Tasks — agent→user action requests

**Status:** Draft — pre-implementation design SSOT. New primitive; normally
needs CTO sign-off before merge (authorized in-session by the CTO for the
concierge build).

**Author:** core-devops (canvas concierge work)
**Related:** RFC #2360 (platform agent / Org Concierge), PR #2385 (canvas redesign)

## Problem

The Org Concierge home has a **Tasks** tab. "Tasks" is meant to be **things an
agent asks the *user* to do** — e.g. "Review the launch draft", "Provide the
Stripe API key", "Confirm the publish date". Today there is **no backend** for
this: the only agent→user mechanisms are

- **Approvals** (`approval_requests`) — sign-off for *destructive* ops only, and
- **`send_message_to_user` / `notify_user`** — unstructured chat messages with no
  state (you can't mark them done, and they don't form a worklist).

So the Tasks tab had to be wired to **schedules** as an interim stand-in, which
is the wrong concept.

## Design

A small structured primitive that mirrors the **approvals** subsystem (same
shape, minus the destructive-gating semantics).

### Data — `user_tasks`

```sql
CREATE TABLE user_tasks (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id UUID NOT NULL,          -- the agent that raised the ask
  title        TEXT NOT NULL,          -- the ask, one line
  detail       TEXT,                   -- optional longer context
  status       TEXT NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending','done','dismissed')),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  resolved_at  TIMESTAMPTZ,
  resolved_by  TEXT
);
CREATE INDEX idx_user_tasks_pending ON user_tasks (status, created_at DESC);
```

### Endpoints (mirror `approvals`)

| Method + path | Auth | Purpose |
|---|---|---|
| `POST /workspaces/:id/user-tasks` | WorkspaceAuth | Agent raises an ask `{title, detail?}` → `201 {user_task_id, status:"pending"}` |
| `GET /workspaces/:id/user-tasks` | WorkspaceAuth | A workspace **reads its own** tasks (any status) |
| `PATCH /workspaces/:id/user-tasks/:taskId` | WorkspaceAuth | A workspace **updates its own** task `{title?, detail?, status?}` (scoped by `workspace_id`) |
| `DELETE /workspaces/:id/user-tasks/:taskId` | WorkspaceAuth | A workspace **deletes its own** task (scoped by `workspace_id`) |
| `GET /user-tasks/pending` | AdminAuth | Cross-workspace pending list for the concierge Tasks tab → `[{id, workspace_id, workspace_name, title, detail, status, created_at}]` |
| `POST /workspaces/:id/user-tasks/:taskId/resolve` | WorkspaceAuth | User marks `{status:"done"|"dismissed", resolved_by?}` → `200` |

**Any** workspace (not just the platform agent) can create and manage its own
tasks; the `:id` workspace scope on update/delete means an agent can only touch
tasks it raised. The Home Tasks list (`/user-tasks/pending`) is org-wide, so
every workspace's asks surface in one place for the user.

`/user-tasks/pending` is AdminAuth + cross-workspace exactly like
`/approvals/pending` (an unauthenticated caller must not enumerate every org's
asks).

### MCP tool — `request_user_action`

Added to the **in-workspace `a2a` MCP** (same place as `send_message_to_user`)
so every agent can raise an ask:

```
request_user_action(title, detail?)   → raise an ask (insert + USER_TASK_REQUESTED)
list_user_tasks()                       → read the asks this workspace raised + status
update_user_task(user_task_id, title?, detail?, status?)  → edit own task
delete_user_task(user_task_id)          → delete own task
```

So every agent (any workspace, via MCP) can create AND manage its own asks —
`request_user_action` is the create; `list_/update_/delete_user_task` are the
read/update/delete, all scoped to tasks the calling workspace raised. None are
gated behind `MOLECULE_MCP_ALLOW_SEND_MESSAGE` (that gate is specific to
`send_message_to_user`); raising/managing an ask is always allowed.

### Events

`USER_TASK_REQUESTED`, `USER_TASK_RESOLVED` — broadcast on the existing
Broadcaster so the canvas updates live (same pattern as `APPROVAL_*`).

### Canvas wiring (PR #2385)

The concierge **Tasks** tab fetches `GET /user-tasks/pending`, renders each as a
task card (title + detail + originating agent), with **Done** / **Dismiss**
buttons calling the resolve endpoint. The tab count badge reflects the pending
count. Replaces the interim schedules wiring.

## SSOT discipline / non-goals

- Reuses the approvals pattern, Broadcaster, and WorkspaceAuth/AdminAuth split —
  no new auth path, no new event bus.
- **Not** an approval/gate: resolving a user-task has no server-side enforcement
  effect; it's a worklist signal. (Destructive gating stays in `approvals`.)
- No `org_id` column; cross-workspace listing joins `workspaces` like approvals.

## Rollout

Phase 0 migration ships idempotently (`IF NOT EXISTS`). Backend + MCP tool +
canvas wiring land together behind the concierge Home (already gated as the new
UI). Full enter-os-core SOP gate applies (tier label + qa-review +
security-review + green CI).
