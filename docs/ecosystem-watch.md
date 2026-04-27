# Ecosystem Watch — Phase 30 Competitive Tracking
**Created by:** PMM
**Date:** 2026-04-27
**Status:** ACTIVE — competitor monitoring in progress
**Phase:** 30 — Remote Workspaces + Cross-Network Federation

---

## Purpose

Track competitor releases and market events that affect Phase 30 positioning. Entries that invalidate a positioning claim trigger an immediate PMM response: file a GitHub issue with label `marketing` and `pmm: positioning update needed — <competitor> shipped <X>`.

---

## Competitor Tracking Matrix

| Competitor | Key product | Last checked | Status | Notes |
|------------|-------------|--------------|--------|-------|
| AWS Agentic / GCP Vertex AI / Azure AI Agent | Managed A2A cloud services | 2026-04-21 | 🔴 IMMINENT | A2A v1.0 shipped March 12. Cloud providers WILL absorb it. Window to position Molecule AI as reference implementation is 72h. |
| LangGraph | A2A-native support | 2026-04-21 | 🔴 WATCH | 3 live PRs shipping A2A (#6645, #7113, #7205). GA expected Q2-Q3 2026. Window to own A2A narrative is NOW. |
| CrewAI | Enterprise agent marketplace | 2026-04-21 | 🔴 WATCH | Only competitor with enterprise agent/tool marketplace today. Molecule needs bundle story before Phase 30. |
| AutoGen (Microsoft) | Multi-agent orchestration | 2026-04-21 | 🟡 MONITOR | No significant A2A or marketplace movement this cycle. |
| OpenAI Agents SDK | SaaS agent platform | 2026-04-21 | 🟡 MONITOR | Proprietary API, not A2A-compatible. No self-hosted option. |
| Google ADK | GCP-native agent framework | 2026-04-21 | 🟡 MONITOR | GCP-only. No cross-cloud A2A. |
| Paperclip | Persistent memory | 2026-04-20 | 🟡 MONITOR | Already tracked. Convergence gap documented. |

---

## Active Positioning Risks

### 🔴 CRITICAL: Cloud Providers About to Absorb A2A v1.0

**Risk:** Linux Foundation A2A v1.0 shipped March 12, 2026. AWS Agentic, GCP Vertex AI Agent Builder, and Azure AI Agent Service will absorb A2A into managed platforms. Once they do, Molecule AI loses the "A2A-native" narrative — it becomes table stakes, not differentiation.

**PMM response:** Issue #1286 is the priority action. Narrative brief draft is ready at `marketing/pmm/issue-1286-a2a-v1-deep-dive-narrative-brief.md` — Marketing Lead reviews → Content Marketer executes.

**Positioning claim:** "Molecule AI is the only multi-agent platform built org-native from the ground up — where the org chart is the agent topology, A2A is the protocol, and the hierarchy enforces governance at every level."

**Mitigation:** Publish A2A v1.0 reference story in next 72h. Narrative brief is drafted — no delay from PMM side.

---

### 🔴 HIGH: LangGraph A2A Convergence (Q2-Q3 2026)

**Risk:** LangGraph ships A2A + graph orchestration + HiTL simultaneously in Q2-Q3 2026. This closes 3 of 7 Phase 30 differentiators:
1. A2A-native peer communication
2. Recursive team expansion  
3. Enterprise workspace isolation

**PMM response:** Window to own A2A narrative is right now. All Phase 30 copy and social must lead with A2A before LangGraph GA.

**Positioning claim at risk:** "Molecule AI is the only agent platform where A2A-native peer communication ships together with workspace isolation."

**Mitigation:** Publish A2A content now. Update battlecard with LangGraph A2A timeline once PRs reach GA.

---

### 🔴 HIGH: CrewAI Marketplace Head Start

**Risk:** CrewAI has an enterprise agent/tool marketplace live today. Molecule AI has no bundle story.

**PMM response:** Flagged in PM brief #1287. Bundle marketplace MVP (issue #1285) is open but not yet shipped.

**Positioning claim at risk:** "Molecule AI fleet management — any agent, any cloud." No counter for "CrewAI has 50+ curated agents in their marketplace."

**Mitigation:** Ship bundle marketplace MVP before Phase 30 GA day. Or fold agent discovery into Phase 30 narrative.

---

## Market Events Log

| Date | Event | Competitor | PMM Action |
|------|-------|-----------|------------|
| 2026-03-12 | **A2A v1.0 officially shipped** — LF, 23.3k stars, 5 official SDKs, 383 community implementations | Linux Foundation / ecosystem | A2A v1.0 is standardized — Molecule AI's native A2A is now a reference implementation story (issue #1286). Position as canonical hosted reference before AWS/GCP/Azure absorb it. |
| 2026-04-23 | **LangGraph PR verification ✅:** #6645, #7113, #7205 still OPEN as of 2026-04-23T17:38Z. A2A native support still in-progress; Molecule AI "live today" positioning intact. Battlecard v0.3 LangGraph counter accurate. | PMM | Confirmed OPEN — moat intact |
| 2026-04-23 | **New feat PRs merged:** #1731 (sweepPhantomBusy — infra reliability), #1730 (45-min gh-token refresh daemon — fixes 60-min git 401 in long sessions), #1702 (SSH-backed file writes for SaaS — fixes 500 on file PUT for SaaS customers). Briefs at launches/pr-1702-*.md and pr-1730-*.md. Release note at blog/2026-04-23-saas-file-api-fix.md. | PMM | All assessed; #1702 most urgent (P1 regression). #1730 routed as reliability improvement. |
| 2026-04-22 | LangGraph PR verification deferred: GH API 401 for external repos. LangGraph PRs #6645, #7113, #7205 still VERIFY. A2A blog uses PR#6645 as governance-gap evidence — if PRs merged, blog claim is stale. | PMM | GH API 401 for external repos — cannot verify |
| 2026-04-21 | Battlecard v0.3 shipped — added A2A live-today vs LangGraph in-progress side-by-side table; LangGraph counters updated to lead with live production status; buyer bottom line added | PMM | Battlecard updated within same cycle as ecosystem check |
| 2026-04-21 | LangGraph PR verification: #6645, #7113, #7205 not found in langchain-ai/langgraph open PR list. Possible merge, close, or re-number. **PMM action:** ecosystem-watch updated with VERIFY flags. Battlecard v0.3 LangGraph status is stale until re-verified. | PMM |
| 2026-04-20 | Chrome DevTools MCP shipped — browser automation now standard MCP tool | MCP ecosystem | Positioned as governance story, not browser story. |

---

## Competitor Feature Tracker

### LangGraph
- A2A support: **OPEN** — PRs #6645, #7113, #7205 still OPEN in langchain-ai/langgraph as of 2026-04-23T17:38Z. Live production claim intact. Expected GA: Q2-Q3 2026.
- Graph orchestration: ✅ Live
- HiTL workflows: **VERIFY** — recent streaming and subgraph PRs (#7559, #7550) do not appear to be HiTL; re-verify
- Self-hosted enterprise: ❌ SaaS-only via LangGraph Studio
- Marketplace: ❌ None
- Source: GitHub langchain-ai/langgraph (verified 2026-04-23 17:38Z) — PRs #6645, #7113, #7205 confirmed OPEN.

### CrewAI
- External agent support: ✅ Secondary path
- Enterprise agent marketplace: ✅ Live
- A2A-native: ❌ Crew-internal only
- Self-hosted: ✅ Open source
- Source: CrewAI docs

### AutoGen (Microsoft)
- Multi-agent orchestration: ✅ Live
- A2A-native: ❌ No standard protocol
- Self-hosted: ✅ Open source
- Enterprise features: 🟡 In progress
- Source: Microsoft AutoGen GitHub

---

## Archive

*(Entries moved here after resolution or after being superseded by newer events)*

---

## Maintenance

- **Check frequency:** Every marketing cycle
- **Trigger:** Any competitor shipping something that invalidates a Phase 30 positioning claim
- **File location:** `docs/ecosystem-watch.md` (origin/main)
- **Last updated by:** PMM | 2026-04-27 (weekly refresh — all competitor versions, stars, and PRs verified current; Clawith v1.9.0 added)

---

*This file must not go stale. If a competitor ships a feature that affects Phase 30 positioning, PMM must act within the same cycle.*
