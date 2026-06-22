# Mobile Information Architecture (SSOT)

> The single source of truth for what the mobile canvas (`< 640px`, `MobileApp`)
> exposes and how it maps to the desktop user flow. When you add or change a
> desktop workspace-panel tab or a home destination, **classify it here first** —
> mobile is a distinct form factor for a distinct set of jobs, not a shrunk
> desktop. core#2697.

## Principle

Mobile optimizes for the **on-the-go operator**: see status, triage, decide,
and chat. Deep configuration, live terminals, and forensic tooling stay on
desktop. The mobile build mirrors the desktop **user flow** (the things you do
between meetings on a phone), not 1:1 tab parity.

## Navigation model

**Bottom tabs** (`MobileTabId`, max 5): `Agents · Inbox · Canvas · Comms · Me`.
- **Agents** (`MobileHome`) — the agent **hierarchy tree** (parent→child,
  expand/collapse, queue-count badge). Mirrors the desktop ConciergeShell tree.
- **Inbox** (`MobileInbox`) — **Tasks + Approvals** with the approve/reject
  decision flow. Mirrors the desktop home Tasks/Approvals sidebar tabs. *(The
  highest-value mobile destination — decision-on-the-go.)*
- **Canvas** (`MobileCanvas`) — touch-friendly org graph (pan/zoom).
- **Comms** (`MobileComms`) — workspace A2A activity feed.
- **Me** (`MobileMe`) — theme / accent / density prefs.

**Agent detail** (`MobileDetail`) opens from any agent; **Chat** (`MobileChat`)
is full-screen (tab bar hidden), at parity with the desktop `ChatTab`
(multi-send, thinking indicator, banner-clear).

## Desktop workspace-panel tabs → mobile classification

The desktop `WorkspacePanelTabs` has 15 tabs. Classification:

| Desktop tab | Mobile policy | Rationale |
|---|---|---|
| Chat (My Chat + Agent Comms) | **Native** ✓ (`MobileChat`) | core conversation; shipped |
| Tasks / Approvals (home) | **Native** ✓ (`MobileInbox`) | decision-on-the-go; shipped |
| Activity | **Native (view)** | triage feed; present (per-agent) |
| Details | **Native (view)** | status/metadata at a glance |
| Config | **Native (view) → edit later** | view tier/runtime/vars; editing is a later phase |
| Memory | **Native (view)** | inspect; write stays desktop |
| Files | **Native (view/download)** | grab an artifact on the go |
| Plugins (Skills) | **View only** | install/deploy is a desktop action |
| Schedule | **Link out → desktop** | cron editing is precision work |
| Channels | **Link out → desktop** | MCP/plugin binding config |
| Container | **Link out → desktop** | image/resource/env config |
| Terminal | **Desktop only** | live shell — not a phone task |
| Display | **Desktop only** | screen capture stream |
| Traces | **Desktop only** | forensic/debug tooling |
| Audit | **Desktop only** | compliance review at a desk |
| Events | **Desktop only** | raw event stream debugging |

**"Link out / Desktop only"** = mobile shows a short "Open on desktop" affordance
rather than a degraded mini-version. Never cram a desktop power-tool into a phone
screen — that produces exactly the "doesn't follow our design" mismatch this
policy exists to prevent.

## Status (core#2697)

- ✅ **Phase 0** — chat parity (multi-send, thinking indicator, banner-clear).
- ✅ **Phase 1** — Inbox (Tasks/Approvals + decision flow).
- ✅ **Phase 2** — agent hierarchy tree + queue badge.
- ▢ **Phase 4 (follow-on)** — Details/Config/Memory/Files view depth;
  "Open on desktop" affordances for the link-out tabs.

## Open decisions (override here)

- **Inbox placement** — currently a 5th bottom tab; alternative is a header
  inbox button (closer to the desktop sidebar). Tab chosen as the most faithful
  analog of the desktop's peer-to-Agents home destinations.
- The Native/link-out split above is the **recommended default** — adjust any
  row and the implementation follows this doc.
