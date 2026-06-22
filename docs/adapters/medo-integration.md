# MeDo Integration Design — EnterOS Hackathon (May 20 2026)

**Status:** Design — implementation pending operator sign-off on open questions (§5).  
**Scope:** How the molecule-dev team builds MeDo apps for the "Build with MeDo" hackathon.  
**Key constraint:** MeDo App Builder is an OpenClaw skill on ClawHub (`seiriosPlus/miaoda-app-builder`),
not a REST API. All interactions go through natural-language messages to an OpenClaw workspace.

---

## 1. Architecture Overview

```
CEO / Canvas
    │  A2A task
    ▼
  PM (claude-code)
    │  delegate_task_async → workspace: medo-builder
    ▼
  MeDo Builder workspace  [runtime: openclaw, skill: miaoda-app-builder]
    │  OpenClaw CLI → skill → api.miaoda.cn
    ▼
  MeDo platform (app created / published → URL returned)
    │  result relayed via A2A event_queue
    ▼
  PM → CEO
```

The MeDo Builder workspace is a **dedicated OpenClaw-runtime workspace** inside the
molecule-dev org with the Miaoda App Builder skill pre-installed. PM delegates natural-language
app-build requests to it via `delegate_task_async` and polls for the result (5–8 min latency).

---

## 2. Installing the Miaoda App Builder Skill

### 2.1 API Key

The skill requires `MIAODA_API_KEY` (not `MEDO_API_KEY`).

> ⚠️ **Credential name mismatch**: the global platform secret is currently named `MEDO_API_KEY`.
> The skill's frontmatter declares `primaryEnv: MIAODA_API_KEY`. The MeDo Builder workspace must
> set `MIAODA_API_KEY` — either rename the global secret or add a workspace-level alias.
> See open question §5-A.

Obtain the key from: **MeDo website → Settings → API Keys**. Keys do not expire, but generating
a new one immediately invalidates the previous one.

### 2.2 Installation Query

OpenClaw installs skills by sending a natural-language install message to the agent.
No CLI command is documented on ClawHub — send this message to the OpenClaw workspace on first boot:

```
Install the Miaoda App Builder skill from ClawHub: seiriosPlus/miaoda-app-builder
```

OpenClaw auto-downloads the skill, installs Python runtime deps (`requests`), and makes the skill
available for subsequent messages.

### 2.3 Workspace Config Sketch (`org-templates/medo-builder/workspace.yaml`)

```yaml
name: MeDo Builder
role: Builds and publishes MeDo applications via the Miaoda App Builder OpenClaw skill
runtime: openclaw
tier: 2
required_env:
  - MIAODA_API_KEY          # TODO: resolve name vs platform secret MEDO_API_KEY (§5-A)
  - OPENROUTER_API_KEY      # OpenClaw needs an LLM provider
initial_prompt: |
  You are a MeDo App Builder. On startup:
  1. Install the Miaoda App Builder skill:
     "Install the Miaoda App Builder skill from ClawHub: seiriosPlus/miaoda-app-builder"
  2. Confirm installation succeeded.
  3. Wait for build tasks from PM via A2A.
  When you receive a build task, use natural language to instruct the skill:
  "Create a [description] app and publish it when done."
  App generation takes 5–8 minutes — poll the skill or wait for confirmation before reporting done.
```

---

## 3. A2A Delegation Pattern (5–8 Min Latency)

App generation is asynchronous and slow. PM **must** use `delegate_task_async` + `check_task_status`
rather than `delegate_task` (which has a shorter timeout and will return before the app is ready).

### 3.1 PM Delegation Flow

```python
# Step 1: fire and forget
task = await delegate_task_async(
    workspace_id="medo-builder-workspace-id",
    task="Build a restaurant reservation tool with online booking, menu display, "
         "and contact form. Publish when done and return the URL."
)

# Step 2: poll every 60s (app takes 5–8 min)
while True:
    status = await check_task_status(task_id=task["task_id"])
    if status["status"] in ("completed", "failed"):
        break
    await asyncio.sleep(60)

result_url = status.get("result")  # MeDo app URL on success
```

### 3.2 Invocation Patterns (verified from Baidu doc)

Natural-language messages the MeDo Builder workspace should accept from PM:

| Intent | Message to send to MeDo Builder workspace |
|--------|-------------------------------------------|
| List existing apps | `"Show me my apps"` |
| Create + auto-publish | `"Create a [description] and publish it when done"` |
| Create only | `"Create a [description]"` |
| Modify existing | `"Add a search function to app [name/ID]"` |
| Publish draft | `"Publish this app"` |
| Status check | `"Is the app generation done yet?"` |

---

## 4. Proposed Org Template — `org-templates/medo-builder/`

```
org-templates/medo-builder/
├── org.yaml                    ← minimal single-workspace org (not full team)
├── medo-builder/
│   ├── system-prompt.md        ← MeDo Builder agent persona + delegation rules
│   └── workspace.yaml          ← runtime: openclaw, skill install, env
```

**org.yaml sketch:**

```yaml
name: MeDo Builder
description: Single-workspace org for building MeDo apps (hackathon)
defaults:
  runtime: openclaw
  tier: 2
  required_env: [MIAODA_API_KEY, OPENROUTER_API_KEY]

workspaces:
  - name: MeDo Builder
    role: Builds and publishes MeDo applications via Miaoda App Builder skill
    files_dir: medo-builder
    canvas: { x: 400, y: 300 }
```

The medo-builder workspace is deployed **as a child of the molecule-dev PM** in the hackathon org,
not as a standalone org. Full `org-templates/medo-builder/` implementation is Week 2 scope.

---

## 5. Open Questions (Operator Resolution Required)

| # | Question | Why it blocks |
|---|----------|---------------|
| 5-A | **Credential name**: platform secret is `MEDO_API_KEY`; skill expects `MIAODA_API_KEY`. Rename global secret or add workspace alias? | Workspace boot will fail with "MIAODA_API_KEY not set" |
| 5-B | **Credit cost per app**: Baidu doc mentions a Credit System but content was not rendered. How many credits does create+generate+publish consume? Do we have enough for hackathon testing? | Budget planning |
| 5-C | **Rate limits**: no rate-limit info in docs or ClawHub page. What's the max concurrent app generations per API key? | Parallelism planning |
| 5-D | **Failure recovery**: what happens if the OpenClaw skill process crashes mid-generation (after Confirm & Generate, before Publish)? Is there a way to resume or check status by app ID? | Reliability design |
| 5-E | **Submission format**: does the hackathon judge the published MeDo app URL, the EnterOS org config, or both? | Determines whether we need a polished demo org or just a working app |

---

## 6. Implementation Checklist (Weeks 1–3)

- [x] Week 1: This design doc (`docs/adapters/medo-integration.md`)
- [ ] Week 1: Resolve §5-A (credential name) + obtain API key credits estimate
- [ ] Week 2: `org-templates/medo-builder/` — full system-prompt + workspace.yaml
- [ ] Week 2: Integration test — PM delegates one real app build end-to-end
- [ ] Week 3: Polish demo org; rehearse submission flow; publish hackathon entry
