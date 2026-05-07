# Skill Catalog

Skills extend what a workspace agent can do — from browser automation
and TTS to research tools and custom API integrations. This page covers
available skill types, how to install them, and how to manage their
versions.

> **Note:** Molecule AI does not ship a hosted skill marketplace. All
> skills are installed from local packages, GitHub URLs, or community
> bundles. See [Skill Lifecycle](#lifecycle) for how to publish and
> distribute skills within your org.

## Available Skill Types

The skills ecosystem covers the same capabilities as Hermes Tool Gateway
and more:

| Category | Skill | What it does | Provider options |
|----------|-------|-------------|-----------------|
| **Browser** | `browser-automation` | Chrome DevTools Protocol via MCP — navigate, query DOM, screenshot, fill forms. Same engine as Hermes' built-in browser tool. | Built-in (CDP); swap via skill version |
| **TTS** | `tts` | Text-to-speech generation. Streams audio to output. | OpenAI, ElevenLabs, or self-hosted |
| **Image gen** | `image-generation` | Generates images from text prompts. | OpenAI DALL·E, Stability AI, or self-hosted |
| **Web search** | `web-search` | Structured web search with result parsing. | Brave, SerpAPI, or custom |
| **Research** | `arxiv-research` | Searches and summarizes arXiv papers. | Community bundle |
| **Code** | `code-analysis` | Static analysis, diff review, complexity scoring. | Built-in |
| **SEO** | `seo-audit` | Lighthouse audit + GSC keyword extraction. | Built-in |
| **Social** | `social-post` | Formats and posts to social channels. | Built-in |

All skills are open source. Source is visible — inspect the `SKILL.md`
and `tools/` before installing.

## Installing a Skill

### From the built-in catalog

```bash
# Install browser automation
molecule skills install browser-automation

# Install TTS with a specific provider
molecule skills install tts --provider openai

# Install a specific version
molecule skills install browser-automation --version 1.2.0
```

### From GitHub

```bash
molecule skills install \
  https://github.com/acme/molecule-skills/tree/main/browser-automation
```

### From a community bundle

Community skills are hosted on GitHub and referenced by slug:

```bash
molecule skills install arxiv-research --from community
```

Community skills are reviewed by the Molecule AI team before being
listed. Submit a skill for review by opening a PR against
[`molecule-ai/skills`](https://git.moleculesai.app/molecule-ai/skills).

## Installing via config.yaml

Skills can also be declared in the workspace config file:

```yaml
skills:
  - name: browser-automation
    source: builtin
  - name: tts
    source: builtin
    config:
      provider: openai
  - name: arxiv-research
    source: community
```

On workspace boot, the runtime validates each skill and loads the
`SKILL.md` + tools into the agent's context.

## Version Management

Skills are versioned with semantic versioning. Pin to a known-good
release to prevent unexpected behavior changes:

```bash
# Pin to a specific version
molecule skills install tts --version 1.1.0

# Upgrade to latest
molecule skills upgrade tts

# View installed version
molecule skills list
```

Upgrading is safe — the skill loader validates the new package on
installation. If the new version has breaking changes, the workspace logs
a warning and keeps the previous version active until you restart.

## Custom Skills

Write a skill for your team's specific workflow:

```bash
# Scaffold a new skill
molecule skills init my-custom-skill
```

This creates:

```
skills/my-custom-skill/
+-- SKILL.md              # instructions + frontmatter
+-- tools/
|   +-- my_tool.py        # MCP tool using @tool decorator
+-- examples/             # few-shot examples
+-- templates/            # reference files
```

See [Skills Reference](../agent-runtime/skills.md) for the full
`SKILL.md` format and frontmatter schema.

## Skill Lifecycle

```
Author writes SKILL.md + tools/
      |
      v
Install into workspace (local or GitHub)
      |
      v
Workspace loads skill on next boot / hot-reload
      |
      v
Agent sees skill in tool context
      |
      v
(Optional) Publish to org bundle or community
```

**Publishing to your org:** Bundle skills with workspace templates so
every new workspace in a role gets the same capability set:

```bash
molecule skills bundle my-custom-skill --output ./org-templates/my-role/
```

**Publishing to the community:** Open a PR against
[`molecule-ai/skills`](https://git.moleculesai.app/molecule-ai/skills) with a
complete skill package. Community skills are reviewed for security and
correctness before listing.

## Removing a Skill

```bash
molecule skills uninstall browser-automation
```

Or remove from `config.yaml` and trigger a hot-reload by touching the
file:

```bash
touch /configs/config.yaml
```

The workspace detects the change, rescans skills, and updates the Agent
Card within ~3 seconds.

## Troubleshooting

**Skill not found:** Check the skill name matches the catalog exactly.
Skill names are lowercase with hyphens (`browser-automation`, not
`browser_automation` or `BrowserAutomation`).

**Skill loads but tools are missing:** Verify the `tools/` folder
contains valid Python files with `@tool`-decorated functions. See
[Skills Reference — Tool Interface](../agent-runtime/skills.md#tool-interface).

**Provider auth error:** Ensure the required environment variable (e.g.
`OPENAI_API_KEY`) is set in the workspace config or secrets.

## Related Docs

- [Skills Reference](../agent-runtime/skills.md) — Full SKILL.md format,
  frontmatter schema, and tool interface
- [Config Format](../agent-runtime/config-format.md) — How skills are
  declared in `config.yaml`
- [Plugin System](../plugins/overview.md) — Installing full plugin
  packages (skills + MCP servers + shared rules)
- [Remote Agent Tutorial](../tutorials/register-remote-agent.md) —
  Installing skills on remote (external) agents