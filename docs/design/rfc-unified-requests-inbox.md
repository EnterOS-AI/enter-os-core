# RFC: Unified Requests / Inbox subsystem (Tasks + Approvals)

**Status:** proposed (awaiting CTO sign-off) · **Author:** CEO-assistant · **Date:** 2026-06-10

## 1. Motivation
Agents need to ask **users or other agents** to do something (task) or to approve
something (approval), **asynchronously**, with a clear **inbox**, action buttons, a
clarification thread, and accountability (who responded). Today only a thin approval
primitive exists (`create_approval`/`decide_approval` + one-way `notify_user`). This
RFC generalizes that into **one** requests subsystem with `kind ∈ {task, approval}`.

## 2. Data model (unified)
**`requests`**
- `id` uuid
- `kind` enum: `task` | `approval`
- `requester_type` (`user`|`agent`), `requester_id`, `org_id`
- `recipient_type` (`user`|`agent`), `recipient_id`   ← user OR agent
- `title` text, `detail` markdown
- `status` enum: `pending` | `info_requested` | `done` | `rejected` | `approved` | `cancelled`
- `responder_type` (`user`|`agent`), `responder_id`   ← nullable until acted; **who acted**
- `priority` smallint (optional), `created_at`, `updated_at`, `responded_at`

**`request_messages`** (the "More Info / chat about this" thread)
- `id`, `request_id` fk, `author_type`, `author_id`, `body`, `created_at`

## 3. Lifecycle / actions
| kind | buttons | transitions |
|---|---|---|
| task | **Done · Reject · More Info** | pending→done / pending→rejected / pending↔info_requested |
| approval | **Approve · Reject · More Info** | pending→approved / pending→rejected / pending↔info_requested |

Terminal: `done`/`approved`/`rejected`/`cancelled`. **More Info** sets `info_requested`
and appends a `request_messages` row ("chat about this"); either side replies until it
resolves to a terminal action. Every terminal action stamps `responder_id`+`responder_type`.

## 4. Async semantics (non-blocking — core requirement)
- Creating a request returns immediately with `request_id`; the **requester never blocks**
  and keeps working.
- The response is delivered **asynchronously**: a signal is posted to the requester's
  activity/inbox (an `a2a_receive`-style event) carrying `{request_id, status, responder_id,
  thread}`. The requester picks it up on its **next tick** — never sits and waits.
- Recipient **agents** receive/poll their pending inbox; recipient **users** see the
  Tasks/Approvals tabs (live via the existing WebSocket).

## 5. Control-plane API
- `POST /workspaces/{id}/requests` — create (requester = that workspace) `{kind, recipient_type, recipient_id, title, detail, priority?}`
- `GET  /requests?recipient_type=&recipient_id=&status=` — inbox list; org-scoped variant powers the "all agents' incoming" tab view
- `GET  /requests/{id}`
- `POST /requests/{id}/respond` `{action: done|rejected|approved}` — `responder_id` taken from the **authenticated principal** (user session or agent token), not the body
- `POST /requests/{id}/messages` `{body}` — More-Info thread
- `POST /requests/{id}/cancel` — requester withdraws

## 6. MCP tools (agent-facing; unify with existing)
- `create_request(recipient, kind, title, detail)` — supersedes `create_approval` (which stays as a `kind=approval` alias during deprecation)
- `list_inbox(status?)` — the calling agent's **incoming** requests
- `respond_request(request_id, action, message?)` — agent responds to a request addressed to it (done/reject/approve, or open More-Info)
- `add_request_message(request_id, body)` — thread reply
- `check_requests()` — status of requests the agent **sent** (the async pickup)
- User-side actions flow through the canvas UI → `/respond` (captures the user id).

## 7. Canvas UI (Tasks + Approvals tabs — already scaffolded)
- Each tab lists items of that kind, **grouped by requesting agent**, newest first: agent
  name + avatar, title, detail preview, age, status badge.
- Buttons per the table in §3. Click Done/Approve/Reject → `POST /respond` with the
  logged-in user → optimistic update + toast.
- **More Info** expands an inline thread panel ("chat about this"): message list + input →
  `POST /messages`; status → `info_requested`; requester replies appear in-thread.
- "All agents' incoming" = org-scoped list. Live updates over the existing WS.

## 8. Idle-nudge sweep
- A CP periodic worker: for each agent workspace that is **idle** (`active_tasks=0`,
  `status=online`) **and** has ≥1 `pending`/`info_requested` request where it is the
  recipient, older than threshold → inject a **nudge** (notify/tick: "you have M unhandled
  inbox items …") so it processes them via `respond_request`. Covers the "agent forgot /
  didn't use the proper MCP tool" case.
- User-addressed requests pending past threshold → UI badge (+ optional channel notify).
- **Anti-spam:** rate-limit (≤1 nudge per request per hour); stop once the agent acts.

## 9. Responder identity / multi-user (forward-looking)
`responder_id`+`responder_type` stamped on every terminal action; UI shows "Approved by
<name>". Schema already supports multiple users with roles in one space (a request can be
fulfilled by a different person than it was shown to). Per-role routing/permissions is
**out of scope for v1** but the model doesn't preclude it.

## 10. Approvals migration
New `requests` table; **idempotent migration** copies existing `approvals` rows in as
`kind=approval`. `create_approval`/`decide_approval`/`list_pending_approvals` become thin
shims over the requests endpoints for a deprecation window, then removed.

## 11. Phasing (each phase = SOP PR → 2-genuine → merge)
- **P0** RFC sign-off (this doc)
- **P1** CP: `requests`+`request_messages` tables, migration, endpoints, tests (molecule-controlplane)
- **P2** MCP tools + approval shims (core/runtime)
- **P3** Canvas UI: Tasks/Approvals tabs render + buttons + More-Info thread + WS live (app/canvas)
- **P4** idle-nudge worker + user-pending notifications
- **P5** deprecate old approval path; docs

## 12. Recommended defaults on open points (flag if you disagree)
- (a) **Keep approval shims** for one deprecation window (not a hard cut) — safer for in-flight callers.
- (b) Nudge: **idle + pending > 10 min → nudge, max 1/hr per request.**
- (c) v1 = **single recipient per request** (fan-out later).
- (d) v1 = **any user in the org may respond** to a user-addressed request (role-gating later).
