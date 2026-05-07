---
title: "Running A2A v1.0 in Production: What the Migration Actually Looks Like"
description: "We migrated Molecule AI's entire agent fleet from a2a-sdk 0.3.x to v1.0 last week. Here's the real diff — four breaking changes, six files, eight smoke scenarios — and what we learned running A2A at scale before most teams have started."
date: 2026-04-24
slug: a2a-v1-production-reference
tags: [a2a, sdk, migration, production, multi-agent, protocol]
keywords: [A2A v1.0 migration, a2a-sdk production, multi-agent protocol, agent fleet, A2A breaking changes, agent SDK upgrade]
canonical: https://docs.molecule.ai/blog/a2a-v1-production-reference
---

<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Article",
  "headline": "Running A2A v1.0 in Production: What the Migration Actually Looks Like",
  "description": "We migrated Molecule AI's entire agent fleet from a2a-sdk 0.3.x to v1.0. Here's the real diff — four breaking changes, six files, eight smoke scenarios — and what we learned.",
  "author": { "@type": "Organization", "name": "Molecule AI" },
  "datePublished": "2026-04-24",
  "publisher": { "@type": "Organization", "name": "Molecule AI", "logo": { "@type": "ImageObject", "url": "https://molecule.ai/logo.png" } }
}
</script>

# Running A2A v1.0 in Production: What the Migration Actually Looks Like

Most organizations writing about A2A v1.0 are writing about what it *will* enable. We're writing about what it *required* — because we've been running it in production since before the Linux Foundation ratified it, and we just completed the migration from `a2a-sdk` 0.3.x to 1.0.0 across our full agent fleet.

This is a practitioner post. No pitch, no benchmark theater. Here's the real diff: four breaking changes, six files, eight smoke test scenarios, and what we learned.

---

## The Context: A2A at Scale in a Production Fleet

Molecule AI is a multi-agent orchestration platform. Every capability in the product — PM, Dev, Research, Marketing — runs as a discrete A2A-speaking workspace. The platform itself is the coordination layer: task delegation, inter-agent communication, fleet health, and audit attribution all route through the A2A protocol.

The fleet is always-on. Agents wake, accept delegations, complete tasks, and go idle — continuously. At peak, we're running concurrent delegations across six to eight active workspaces per session, with each workspace capable of spawning sub-delegations to sibling agents. The a2a-sdk sits at the center of this: every task dispatch, every `delegate_task` call, every heartbeat touches it.

When `a2a-sdk` 1.0.0 shipped with breaking changes, we had no option to defer. With Phase 34 GA targeting April 30, the migration needed to land on `staging` before April 25 — giving us a five-day buffer to validate before launch.

---

## What Changed in a2a-sdk 1.0.0

The SDK's breaking changes were intentional improvements, not incidental rewrites. Here's what actually moved.

### 1. Server bootstrap: `A2AStarletteApplication` is gone

In 0.3.x, you bootstrapped an A2A server with a single class:

```python
# 0.3.x
from a2a.server.apps import A2AStarletteApplication
app = A2AStarletteApplication(agent_executor=executor, agent_card=card)
```

In 1.0.0, this class was replaced by a Starlette route factory pattern. The `AgentCard` schema also changed — `capabilities` moved from a flat list to a structured object with typed fields:

```python
# 1.0.0
from a2a.server.apps import create_a2a_app
from a2a.types import AgentCard, AgentCapabilities

card = AgentCard(
    name="my-agent",
    capabilities=AgentCapabilities(streaming=True, push_notifications=False)
)
app = create_a2a_app(agent_executor=executor, agent_card=card)
```

Why it's better: the factory pattern makes it easier to compose A2A apps into larger ASGI trees — useful if your agent also serves a health check endpoint or a management API on the same process.

### 2. Part construction: positional constructor removed

In 0.3.x, you could pass a `TextPart` positionally:

```python
# 0.3.x
from a2a.types import Part, TextPart
part = Part(TextPart(text="hello"))
```

In 1.0.0, `Part` uses keyword arguments only:

```python
# 1.0.0
part = Part(text="hello")
```

This is a clean-up: the positional form was ambiguous when `Part` was extended to support `data` and `file` variants. The keyword form is unambiguous and IDE-friendly.

### 3. `TaskState` enum: string constant replaced

```python
# 0.3.x
from a2a.types import TaskState
state = TaskState.canceled

# 1.0.0
from a2a.types import TASK_STATE_CANCELED
state = TASK_STATE_CANCELED
```

The shift from enum member to module-level constant is a minor ergonomic change that aligns with how other A2A state constants are referenced across the SDK. The actual string value is unchanged — this is a rename, not a semantic change.

### 4. `a2a.utils` → `a2a.helpers`

```python
# 0.3.x
from a2a.utils import build_text_artifact

# 1.0.0
from a2a.helpers import build_text_artifact
```

Module rename. The function signatures are identical; only the import path changed.

---

## The Migration: Six Files, One PR

All four breaking changes were contained in six files:

| File | Changes |
|------|---------|
| `workspace/main.py` | `A2AStarletteApplication` → route factory + `AgentCard` restructure |
| `workspace/a2a_executor.py` | `Part(TextPart(...))` → `Part(text=...)`, `TaskState.canceled` → `TASK_STATE_CANCELED`, `a2a.utils` → `a2a.helpers` |
| `workspace/hermes_executor.py` | Enum rename + helpers import |
| `workspace/google-adk/adapter.py` | Enum rename + helpers import |
| `workspace/cli_executor.py` | `a2a.utils` → `a2a.helpers` |
| `workspace/tests/conftest.py` | Mock stub updated to `a2a.helpers` |

Total: one PR (`fix/a2a-sdk-v1-migration`), merged to `staging` as commit `35bcad92`. No test failures. No behavior regression.

The migration was deliberately narrow — touching only the A2A bootstrap, part construction, enum references, and import paths. We made no structural changes to executor logic, task handling, or delegation routing in the same PR. This is the right call for a breaking-change migration: keep the semantic diff minimal so any regression is immediately attributable to the SDK change, not to coincidental refactoring.

---

## Validating the Migration: Eight Smoke Scenarios

Before merging, we ran eight smoke scenarios (S-1 through S-8) designed to exercise each layer of the A2A stack under the new SDK:

- **S-1 — Server starts and card is discoverable:** `GET /agent-card` returns a valid `AgentCard` with typed capabilities.
- **S-2 — Task submission accepted:** `POST /tasks` with a `TextPart` payload returns a `202 Accepted` with a task ID.
- **S-3 — Task state transitions:** Task progresses through `submitted → working → completed` without state machine errors.
- **S-4 — Canceled task handling:** Cancellation request sets `TASK_STATE_CANCELED` correctly and is reflected in the task status response.
- **S-5 — Helpers import resolves:** `build_text_artifact` and related helpers resolve from `a2a.helpers` with no `ImportError`.
- **S-6 — Part keyword construction:** `Part(text=...)` constructs cleanly; `Part(data=...)` also resolves for binary payloads.
- **S-7 — Delegation round-trip:** Full `delegate_task` cycle from a peer workspace completes end-to-end through the upgraded executor.
- **S-8 — Concurrent delegation under load:** Five concurrent delegations across two workspaces complete without race conditions or dropped tasks.

All eight passed on `staging` before the PR merge. S-7 and S-8 are the high-value tests — they're the ones that would catch a regression in the bootstrap or part construction that only surfaces under real inter-agent traffic.

---

## What We'd Do Differently

**Pin the SDK version explicitly in every executor.**  
We found two executor files where `a2a-sdk` was listed as a loose dependency (`>=0.3.0`). When 1.0.0 shipped with breaking changes, those executors silently picked up the new version on the next `pip install`. For a library with a breaking change boundary at 1.0, lock the version (`==0.3.x` before migration; `==1.0.0` after) and treat the upgrade as a deliberate event, not a passive update.

**Test the `AgentCard` schema change separately.**  
The `A2AStarletteApplication` removal and the `AgentCard` restructure should have been two separate test cases. We caught the `AgentCard` schema issue during S-1 — but it would have been cleaner as an explicit pre-migration test rather than a discovery during smoke.

**Migrate mock stubs at the same time as production code.**  
`workspace/tests/conftest.py` was the last file we touched because it wasn't an executor. But stubs that patch `a2a.utils` will throw `ModuleNotFoundError` the moment the production code migrates to `a2a.helpers` and tests run. Update stubs in the same PR, same commit, as the production migration.

---

## What's Next

The `staging` branch is now on `a2a-sdk` 1.0.0. The `main` branch still carries the 0.3.x code — a `staging→main` sync PR is in progress to land the migration on `main` before Phase 34 GA on April 30.

If you're running `a2a-sdk` 0.3.x and planning the 1.0.0 migration, this post is the reference. The four breaking changes are well-contained, the migration is a single PR, and the eight smoke scenarios above will tell you whether the upgrade is clean before you merge.

Questions? The [A2A protocol spec](https://github.com/google-a2a/a2a-specification) is the authoritative source. For Molecule AI's production A2A implementation, see [External Agent Registration](https://docs.molecule.ai/docs/guides/external-agent-registration) or open an issue in the [molecule-core](https://git.moleculesai.app/molecule-ai/molecule-core) repo.
