# Skills

A skill is a package that gives an agent knowledge, instructions, and optionally executable tools. Skills are the primary way to customize what a workspace agent can do.

The skill package shape follows the same `SKILL.md`/frontmatter conventions used by [ClawHub](https://clawhub.ai/)-style skills, but this runtime keeps the lifecycle local: skills are installed, audited, published, and hot-reloaded inside a workspace rather than managed through a separate registry layer.

## Skill Package Structure

```
skills/generate-seo-page/
+-- SKILL.md              # always present -- instructions + frontmatter metadata
+-- links.yaml            # optional -- reference URLs
+-- examples/             # optional -- few-shot examples
+-- templates/            # optional -- reference files
+-- tools/                # optional -- executable MCP tools
|   +-- write_page.py     # MCP tool -- writes file to Next.js repo
|   +-- check_gsc.py      # MCP tool -- queries Search Console API
|   +-- translate_zh.py   # MCP tool -- translates EN to ZH
+-- .clawhubignore        # optional -- files to exclude from publish
```

## The Two Parts

| Part | Purpose |
|------|---------|
| `SKILL.md` | Tells the agent **what to do** and **how to think** (+ metadata in frontmatter) |
| `tools/` | Gives the agent **executable actions** to take |

## SKILL.md Format

`SKILL.md` uses Markdown with YAML frontmatter. The frontmatter declares metadata and runtime requirements. The Markdown body contains the agent's instructions.

```markdown
---
name: generate-seo-page
description: Generates bilingual EN/ZH SEO landing pages for renovation keywords
version: 1.0.0
metadata:
  openclaw:
    requires:
      env: [GSC_CLIENT_ID, GSC_CLIENT_SECRET]
      bins: []
    primaryEnv: GSC_CLIENT_ID
    emoji: "🔍"
    homepage: https://github.com/example/seo-skills
---

# Generate SEO Landing Page

You are an SEO specialist. When asked to generate a page, follow these steps:

1. Research the target keyword using Google Search Console
2. Analyze top-ranking competitors
3. Generate a bilingual EN/ZH Next.js page
4. Write the page to the repo using the `write_page` tool

## Guidelines
- Title tag: 50-60 characters, keyword at the front
- Meta description: 150-160 characters
- ...
```

### Frontmatter Fields

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Skill identifier (lowercase, URL-safe: `^[a-z0-9][a-z0-9-]*$`) |
| `description` | Yes | Short summary (used in UI and search) |
| `version` | Yes | Semantic version |
| `runtime` | No | Adapter compatibility list — see [Runtime Compatibility](#runtime-compatibility) below. Defaults to `["*"]` (universal). |
| `tags` | No | List of category tags surfaced in the skill catalog |
| `examples` | No | List of example prompts injected as few-shot context |
| `metadata.openclaw.requires.env` | No | Environment variables the skill needs |
| `metadata.openclaw.requires.bins` | No | CLI binaries required (all must exist) |
| `metadata.openclaw.requires.anyBins` | No | CLI binaries (at least one must exist) |
| `metadata.openclaw.requires.config` | No | Config file paths the skill reads |
| `metadata.openclaw.primaryEnv` | No | Main credential environment variable |
| `metadata.openclaw.emoji` | No | Display emoji for UI |
| `metadata.openclaw.homepage` | No | Documentation or project URL |
| `metadata.openclaw.os` | No | OS restrictions (e.g. `["darwin", "linux"]`) |
| `metadata.openclaw.install` | No | Dependency install specs (`brew`, `node`, `go`, `uv`) |

The `metadata.openclaw` section can also be aliased as `metadata.clawdbot` or `metadata.clawdis`.

### Runtime Compatibility

A skill that depends on a runtime-specific tool — e.g. uses a Claude Code-only `Bash` tool, or hermes-agent's sub-agent registry — should declare which adapters it supports via the `runtime` field:

```markdown
---
name: claude-bash-helper
description: Wraps Claude Code's Bash tool with retries
runtime: [claude-code]
---
```

When a workspace boots with a different adapter, the skill loader logs a `Skipping skill ...: runtime=[...] not compatible with current=...` line and the skill is omitted from the agent's tool set. The runtime never sees the broken skill — no AttributeError, no "tool not found" surprise on the first invocation.

Accepted shapes:

| Value | Meaning |
|-------|---------|
| Absent / `["*"]` | Universal — loads into every adapter (default) |
| `["claude-code"]` | Loads only into the `claude-code` adapter |
| `[claude-code, hermes]` | Loads into either of these adapters |
| `claude-code` | String shorthand — normalized to `["claude-code"]` |

Match values come from each adapter's `name()` method (the same string that goes in `config.yaml`'s `runtime:` field). A malformed value (e.g. `runtime: 123`) logs a warning and falls back to universal — the skill is never silently dropped on invalid input.

This shape mirrors hermes-agent's declarative skill-compat model. Adopting the same convention keeps cross-runtime skill packages portable: a skill author writes one `SKILL.md` and the workspace picks the right subset at boot.

## Skill Types

A skill can range from pure context to pure tools:

| Type | Contents | Example |
|------|----------|---------|
| Pure context skill | Just `SKILL.md` | "How to write good SEO content" |
| Hybrid skill | `SKILL.md` + `tools/` | "How to generate a page" + `write_page.py` + `check_gsc.py` |
| Pure tool skill | Just `tools/` | A calculator, an API wrapper, a file processor |

All three are valid. The agent decides when to call the tools based on the instructions in `SKILL.md`.

## Tool Interface

Tools inside a skill use the standard LangChain `@tool` decorator. The skill loader introspects each module and collects anything decorated with `@tool`.

Example tool file:

```python
# skills/generate-seo-page/tools/write_page.py

from langchain_core.tools import tool

@tool
async def write_page(path: str, content: str) -> dict:
    """Write a Next.js page to the repo."""
    # writes file to filesystem, commits to git, etc.
    ...
    return {"success": True, "page_path": path}
```

The `@tool` decorator handles:
- Registering the function as a callable tool with the LangGraph agent
- Extracting the function name, docstring, and type hints as the tool schema
- Making the tool available to the LLM with proper parameter descriptions

## Skill Loader

The workspace runtime loads skills at startup based on `config.yaml`:

```python
from langchain_core.tools import tool as tool_decorator
import importlib, inspect

def load_tools(tools_path: Path) -> list:
    """Introspect tool modules and collect @tool-decorated functions."""
    tools = []
    for py_file in tools_path.glob("*.py"):
        module = importlib.import_module(py_file.stem)
        for name, obj in inspect.getmembers(module):
            if hasattr(obj, "tool") or isinstance(obj, BaseTool):
                tools.append(obj)
    return tools

def load_skill(skill_path: Path) -> Skill:
    return Skill(
        metadata=load_frontmatter(skill_path / "SKILL.md"),
        instructions=load_markdown(skill_path / "SKILL.md"),
        examples=load_examples(skill_path / "examples"),
        links=load_links(skill_path / "links.yaml"),
        tools=load_tools(skill_path / "tools")
    )
```

Skills listed in `config.yaml` are loaded by folder name:

```yaml
skills:
  - generate-seo-page
  - audit-seo-page
  - keyword-research
```

The loader looks for each folder under `skills/` in the workspace config directory.

## How Skills Reach the Agent

1. Frontmatter metadata is parsed for requirements validation (env vars, binaries)
2. `SKILL.md` body instructions are appended to the agent's system prompt
3. Tools from `tools/` are registered as MCP tools available to the agent
4. Examples from `examples/` are injected as few-shot context
5. Links from `links.yaml` are included as reference material

The agent reads the combined instructions and knows what tools it has. It decides when and how to use them.

## Live Reload

Skills are **live-reloadable at runtime** — no container restart needed.

A file watcher monitors the entire workspace config directory. Any change — skill added, removed, `SKILL.md` edited, `system-prompt.md` edited, `config.yaml` modified — triggers a debounced reload (2 seconds after last change to handle rapid multi-file writes like `git pull`). See [Config Format — Hot-Reload Behavior](./config-format.md#hot-reload-behavior) for which config fields are hot-reloadable.

Reload does three things:
1. Rescans skills folder, rebuilds Agent Card
2. Pushes updated card to platform (`POST /registry/update-card`) -> platform broadcasts `AGENT_CARD_UPDATED` -> peer workspaces rebuild their system prompts
3. Rebuilds own system prompt with new skills

**Adding a skill to a running workspace from the canvas:**

```
User drops skill onto workspace node on canvas
      |
      v
Platform copies skill files into workspace container volume
      |
      v
File watcher detects changes (~2s debounce)
      |
      v
Workspace rescans skills folder, rebuilds Agent Card
      |
      v
POST /registry/update-card -> platform stores new card
      |
      v
Platform broadcasts AGENT_CARD_UPDATED to all peer workspaces
      |
      v
All peer workspaces rebuild their system prompts
      |
      v
Canvas updates node to show new skill badge
```

Live in ~3 seconds, zero restart.

## Skill Audit

You can audit a workspace's configured skills without starting a new backend or registry:

```bash
molecli agent skill audit <workspace-id>
```

The audit is intentionally local and file-based. It checks the workspace's `config.yaml`, then validates each listed skill package under `skills/<name>/` for:

- `SKILL.md` presence
- YAML frontmatter parsing
- required frontmatter fields: `name`, `description`, `version`

Use this as a lightweight hygiene check before publishing, bundling, or reusing a skill. It is not a marketplace or remote registry.

## Skill Install and Publish

The CLI also exposes a thin local workflow for moving skills between a workspace and your machine:

```bash
molecli agent skill install <workspace-id> <local-skill-dir>
molecli agent skill publish <workspace-id> <skill-name> --to <output-dir>
```

- `install` copies a local skill folder into a workspace and updates `config.yaml`
- `publish` exports a workspace skill from the bundle endpoint into a local directory

Both commands stay intentionally small and reuse the existing workspace Files API and bundle export path. They are convenience wrappers, not a separate skill registry.

## Skill Promotion Loop

When the agent sees the same workflow succeed repeatedly, it should compress that workflow into memory first and then promote it into a skill without waiting for a later review pass. Hermes-style promotion stays intentionally thin: the runtime records the promotion as a signal, and the actual skill package lifecycle remains a separate skill-management concern rather than a second hidden control plane.

The handoff is:

1. `memory-curation` decides the workflow is durable
2. The memory packet sets `promote_to_skill = true`
3. The packet also carries a `repetition_signal` proving the workflow has repeated cleanly
4. `skill-authoring` turns that packet into a narrow `SKILL.md`
5. The existing skill loader / hot-reload path picks up the skill package when it has been created through the normal skill lifecycle

This is intentionally a local runtime signal, not a remote registry or human approval queue. The goal is to keep the promotion observable and narrowly scoped, while avoiding a custom auto-generation layer that would duplicate the skill system itself.

For observability, the workspace writes a `skill_promotion` activity when a promotion packet is committed, and then sends a lightweight heartbeat with `current_task = "Skill promotion: ..."` so the canvas can treat the promotion as an explicit in-flight task.

## ClawHub Compatibility

### Using ClawHub-Style Skills

```bash
npx clawhub@latest install <skill-name>
```

ClawHub skills are context-only (no `tools/` folder). They work in Molecule AI as pure context skills — the `SKILL.md` instructions get appended to the agent's system prompt.

### Reusing ClawHub-Style Skills

ClawHub-style skills can be reused here as pure context skills. The `tools/` folder and its MCP tools are included as supporting files when present. Note that `tools/` only execute inside the Molecule AI runtime — outside this runtime, the same `SKILL.md` instructions are still useful, but execution remains local.

**Constraints for ClawHub-style bundles:**
- Only text-based files are allowed (no binaries)
- Maximum total bundle size: 50MB
- Skill slug must be lowercase and URL-safe: `^[a-z0-9][a-z0-9-]*$`
- All published skills use MIT-0 licensing

## Related Docs

- [Workspace Runtime](./workspace-runtime.md) — Where skills are loaded
- [Config Format](./config-format.md) — How skills are referenced in `config.yaml`
- [Bundle System](./bundle-system.md) — How skills are inlined into bundles
