# RFC: Agent Liveness — the no-hang guarantee

**Status:** proposed (CTO design-approved; awaiting sign-off to build) · **Author:** CEO-assistant · **Date:** 2026-06-10

## 1. Motivation
An agent can hang and sit **dead for hours while still reporting `online`**, with no
self-recovery. Two real cases today:
- **JRS SEO agent:** `npx vercel` blocked on an interactive prompt at 08:24Z; the agent
  produced **zero activity for ~2.5h**, held `active_tasks=1`, and every inbound message
  300s-timed-out. Only a manual soft-restart recovered it.
- **agents-team reviewers:** the same blocking-A2A pressure (`300002ms` `timeout awaiting
  response headers`) overloaded the tenant host (sustained 524s) and throttled review
  throughput fleet-wide.

Three independent gaps allow this, and **fixing any one alone is insufficient**:
1. **Unbounded tool execution** — a subprocess (`npx vercel`) can block forever.
2. **Synchronous blocking A2A** — inbound `message/send` blocks the caller up to 300s; with
   a slow/hung agent these pile up and starve the workspace-server.
3. **Liveness keyed on status, not activity** — `online` comes from the registry heartbeat,
   not real responsiveness, so a hung agent stays green ("clean telemetry ≠ healthy").

**Goal:** no agent stays silently hung; bounded worst-case recovery (~12–17 min, not hours);
no inbound-message loss; works for **every tenant**, not just agents-team.

## 2. Design — three layers

### L1 — Bounded tool execution (runtime)
Every tool / subprocess call gets a **hard timeout** (default **300 s**, per-tool override).
On expiry: kill the **process group** (not just the leader), and return a structured
`{error:"tool_timeout", detail:"… killed after Ns"}` so the agent loop continues instead of
blocking. Known-interactive CLIs (`vercel`, `gh`, `npm`, `git` credential prompts) run
**non-interactive** — inject `--yes`/`--no-input`, run with **stdin closed** — so they can't
block on a prompt at all.
- **Where:** the runtime's tool-execution wrapper (the Bash/subprocess path in
  `molecule_runtime`).
- **Config:** `MOLECULE_TOOL_TIMEOUT_S` (default 300), optional per-tool map.
- **Effect:** kills the trigger class — a subprocess can't wedge an agent if it can't run
  forever.

### L2 — Non-blocking A2A (runtime + platform)
Inbound `message/send` **enqueues and returns `202 accepted` immediately**; the agent drains
its queue on its next tick. `MOLECULE_A2A_NONBLOCKING=true` already exists (runtime#112,
shipped on image 0.3.13) — this layer is the **fleet rollout** (re-provision onto 0.3.13 with
the flag) plus making it the default.
- **Effect:** stops the 300s pileup + host overload; inbound is never lost, just deferred.

### L3 — Stall watchdog → probe → restart (platform, ALL tenants)
A per-tenant periodic sweep in the workspace-server (covers every org, unlike today's
operator-only agents-team script):
- **Detect:** workspace with `active_tasks > 0` **AND** `last_activity_at < now() −
  STALE_AFTER` (default **12 min**) **AND** not in cooldown → **suspected-stalled**.
- **Probe:** inject a liveness message — *"You've had no activity for N min while marked
  busy. Reply (any activity) or you'll be restarted in M min."*
- **Escalate:** if still no activity after `PROBE_GRACE` (default **5 min**) →
  **soft-restart** (existing-volume). The probe separates *slow-but-alive* (resumes activity,
  left alone) from *deadlocked* (can't process the probe → restarted).
- **Audit + anti-flap:** every transition logged; no re-probe/restart within a cooldown
  (default 30 min) to avoid restart loops; never act on `paused`/`hibernated`.
- **State machine:** `healthy → suspected_stalled →(activity)→ healthy` |
  `→(silence > grace)→ restarting → healthy`.

This is the platform-enforced version of "verify by real activity." It **supersedes** the
operator `agent-health-watchdog` (which only fires on `status=failed` and explicitly treats
`active=0` as fine — it would never have caught the JRS `active=1 but silent` signature) and
extends coverage to all tenants.

## 3. Data model
- `workspaces.last_activity_at TIMESTAMPTZ` — stamped on every `activity_logs` insert (or a
  cheap trigger / write-through in the activity writer). Drives L3.
- Optional `workspace_liveness_events` audit table (`workspace_id, event, at`) for
  probe/restart history. (Can defer; structured logs suffice for v1.)

## 4. Config / defaults (override any)
| Knob | Default |
|---|---|
| `MOLECULE_TOOL_TIMEOUT_S` (L1) | 300 |
| non-interactive CLI enforcement (L1) | on |
| `MOLECULE_A2A_NONBLOCKING` (L2) | true (rollout) |
| stall-after (L3) | 12 min |
| probe-grace (L3) | 5 min |
| restart cooldown (L3) | 30 min |
| sweep cadence (L3) | 3 min |

Worst-case recovery = stall-after + probe-grace ≈ **17 min** (vs. today's *unbounded*).

## 5. Phasing (each = SOP PR → 2-genuine → merge)
- **A1** — L1 tool-call timeouts + non-interactive CLI (runtime). Highest leverage, smallest
  blast radius; lands first.
- **A2** — L3 stall watchdog + probe→restart + `last_activity_at` (platform / workspace-server).
- **A3** — L2 non-blocking A2A fleet rollout (flag default + re-provision — the gated op,
  JRS-first then fleet).

## 6. Interactions & non-goals
- **vs. the requests/inbox P4 idle-nudge:** different population — P4 nudges *idle* agents
  with *pending inbox items*; L3 probes *busy-but-silent* agents. Complementary; share the
  `last_activity_at` signal.
- **vs. operator watchdog:** L3 generalizes + replaces it (platform-side, all tenants,
  activity-based not status-based). Keep the operator script until A2 ships, then retire.
- **Non-goals:** not a perf profiler; not killing legitimately-long *tasks* — L1 bounds a
  single *tool call*, not the whole task; L3 gives the agent a chance to answer the probe
  before any restart, so a long-but-alive task that still emits activity is never touched.

## 7. Open questions / risks
- **False positive on a genuinely long, silent tool call** (e.g. a 20-min build emitting no
  interim output): mitigated by (a) per-tool L1 timeout tuning, (b) the L3 probe — the agent
  can answer "still working" to defer restart. If a tool legitimately needs >300s with zero
  output, raise its per-tool timeout. Worth confirming which tools that affects.
- **`last_activity_at` write overhead** on the hot activity path — use a lightweight
  write-through, not a synchronous extra round-trip.
- **Probe delivery to a deadlocked agent** won't be processed — by design; that's what the
  escalate-to-restart handles.
