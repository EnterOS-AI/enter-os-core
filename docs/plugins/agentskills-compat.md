# Molecule AI plugins and the agentskills.io standard

> **TL;DR** — every skill inside a Molecule AI plugin is a spec-compliant
> [agentskills.io](https://agentskills.io) skill, which means the same
> skill directory is installable in **Claude Code, Cursor, GitHub Copilot,
> VS Code, OpenAI Codex, Gemini CLI, Amp, OpenCode, OpenHands, Letta,
> Goose, Roo Code, Kiro, Factory, Ona, Junie**, and ~20 other agent
> products that ship the standard today. Molecule AI adds a *plugin*
> superset on top: a bundle of skills + rules + per-runtime adapters,
> so the same plugin can orchestrate across a team of agents running on
> different LLM runtimes.

## The two layers

```
plugins/my-plugin/                        ← Molecule AI bundle (our layer)
├── plugin.yaml                           ← Molecule AI manifest: name, version,
│                                            runtimes, adapters, description
├── rules/*.md                            ← Molecule AI-only: always-on prose
│                                            appended to the runtime memory file
├── skills/                               ← agentskills.io layer starts here
│   ├── <skill-name-1>/
│   │   ├── SKILL.md                      ← agentskills spec: frontmatter + body
│   │   ├── scripts/                      ← optional, executable code
│   │   ├── references/                   ← optional, deep-dive docs
│   │   └── assets/                       ← optional, templates/data
│   └── <skill-name-2>/
│       └── SKILL.md
└── adapters/                             ← Molecule AI-only: per-runtime installers
    ├── claude_code.py
    └── deepagents.py
```

The boundary is clean:

- **Everything under `skills/<name>/`** follows the spec. A skill-aware
  tool that doesn't know what Molecule AI is can consume it as-is.
- **Everything above `skills/`** is our superset — bundle metadata,
  cross-runtime install logic, always-on rules.

## What the spec defines (and what we follow exactly)

Per [agentskills.io/specification](https://agentskills.io/specification):

| Spec requirement | How Molecule AI enforces it |
|---|---|
| Skill is a directory with `SKILL.md` at the root | `skills/<name>/SKILL.md` |
| Directory name matches frontmatter `name` | Enforced by `molecule_plugin validate` |
| `name`: 1–64 chars, lowercase + hyphens, no consecutive or edge hyphens | Regex-validated |
| `description`: 1–1024 chars, covers what+when | Length-validated |
| `license`, `compatibility`, `metadata`, `allowed-tools` optional | Passed through unchanged |
| `scripts/`, `references/`, `assets/` optional dirs | Skill loader reads all three |
| Progressive disclosure (metadata → body → sub-files) | Claude Code reads it natively; other runtimes load via plugin adaptor |

## Where we extend the spec (bundle layer)

The spec doesn't address bundling, cross-runtime installation, or
always-on rules. That's what `plugin.yaml` adds:

```yaml
# plugins/my-plugin/plugin.yaml
name: my-plugin
version: 1.0.0
description: Bundle of related skills + rules for <use case>.
author: your-name
tags: [example]

# Declared supported workspace runtimes — each must have a matching
# adapters/<runtime>.py file, or the install falls through to raw-drop.
runtimes:
  - claude_code
  - deepagents

# Optional — these are document hints, not enforced by the spec.
# The skills list is informational; the skill loader discovers everything
# under skills/ regardless.
skills:
  - my-skill-a
  - my-skill-b

# Optional — always-on markdown files appended to the runtime memory file
# (CLAUDE.md on Claude Code and DeepAgents). The spec has no always-on tier.
rules:
  - rules/conventions.md
```

### Rules vs skills

- **A skill** is activated on demand — the agent reads its `name` and
  `description` at startup, then loads the body when the task matches.
- **A rule** is always-on — its text is appended to the runtime's
  memory file (CLAUDE.md) so the agent sees it on every turn.

Rules are a Molecule AI-specific extension. If we ever need to represent a
rule as a spec-compliant skill (e.g. for distribution to a non-Molecule AI
tool), write it as a skill whose `description` explicitly says "apply
continuously in this codebase" — the tool will decide whether to honor it.

### Per-runtime adapters

The spec leaves install semantics to the host tool. Molecule AI's plugin
adapters (`plugins/<name>/adapters/<runtime>.py`) bridge the gap for
runtimes that don't read `SKILL.md` natively. For most plugins the
built-in `AgentskillsAdaptor` covers the common shape (copy skills to
`/configs/skills/`, append rules to CLAUDE.md). See
[plugins_registry](../../workspace/plugins_registry/__init__.py)
for the resolution order.

## Validator

Run before publishing a plugin:

```bash
python -m molecule_plugin validate plugins/my-plugin
```

Checks:

1. `plugin.yaml` parses and declares known runtimes.
2. Every `skills/<name>/SKILL.md`:
   - has valid frontmatter
   - `name` matches the directory name
   - `name` matches the spec regex (lowercase, hyphens, length)
   - `description` is 1–1024 chars
   - optional fields (`license`, `compatibility`, `metadata`,
     `allowed-tools`) conform to spec types

CI runs this against every first-party plugin on every PR, so spec drift
is caught before merge.

## Publishing a skill to agentskills-compatible tools

Any `skills/<name>/` directory from a Molecule AI plugin is a valid standalone
skill. To publish it for Cursor / Codex / Goose / etc. users:

1. Copy `plugins/my-plugin/skills/<name>/` into a new repo.
2. Validate: `python -m molecule_plugin validate .` (or `skills-ref validate`
   from the upstream [agentskills/agentskills](https://github.com/agentskills/agentskills) repo).
3. Publish the repo; users install according to their tool's docs.

The skill will use default activation semantics in each tool. Molecule AI's
plugin bundle (runtimes, adapters, rules) is not needed — it only matters
if the skill is installed inside Molecule AI.

## Hermes runtime compatibility (issue #852)

As of 2026-04-18, `hermes` has been added to the `runtimes:` field in
the five SKILL.md-only first-party plugins. agentskills.io v0.8.0
confirmed that SKILL.md-only plugins are natively hermes-compatible via
**raw-drop** (no adapter file required). Hook-based plugins remain
`claude_code`-only — they rely on harness-level hooks that hermes does
not expose.

| Plugin | Before | After |
|---|---|---|
| `ecc` | `[claude_code, deepagents]` | `[claude_code, deepagents, hermes]` |
| `superpowers` | `[claude_code, deepagents]` | `[claude_code, deepagents, hermes]` |
| `molecule-dev` | `[claude_code, deepagents]` | `[claude_code, deepagents, hermes]` |
| `molecule-skill-cron-learnings` | `[claude_code]` | `[claude_code, hermes]` |
| `molecule-skill-update-docs` | `[claude_code]` | `[claude_code, hermes]` |

Companion PRs:
- [molecule-ai-plugin-ecc#2](https://git.moleculesai.app/molecule-ai/molecule-ai-plugin-ecc/pull/2)
- [molecule-ai-plugin-superpowers#2](https://git.moleculesai.app/molecule-ai/molecule-ai-plugin-superpowers/pull/2)
- [molecule-ai-plugin-molecule-dev#2](https://git.moleculesai.app/molecule-ai/molecule-ai-plugin-molecule-dev/pull/2)
- [molecule-ai-plugin-molecule-skill-cron-learnings#2](https://git.moleculesai.app/molecule-ai/molecule-ai-plugin-molecule-skill-cron-learnings/pull/2)
- [molecule-ai-plugin-molecule-skill-update-docs#2](https://git.moleculesai.app/molecule-ai/molecule-ai-plugin-molecule-skill-update-docs/pull/2)

Security note: Security Auditor was offline at time of change. Self-assessed
as non-security-impacting — adding `hermes` to a string list in `plugin.yaml`
creates no new tool surface or execution path.

## Why this matters strategically

- **Zero-cost distribution.** Every skill we ship to Molecule AI users is
  automatically installable in ~35 other agent products, no rewrite.
- **We're visible in the spec ecosystem.** Our plugin directory becomes
  discoverable alongside Anthropic's own example skills. If the spec
  adds new fields, we inherit them for free.
- **Our moat stays intact.** Multi-agent orchestration, A2A, per-runtime
  adapters, and the visual canvas — none of this is in scope for the
  spec and is unlikely to be. That's where Molecule AI differentiates.
