# RFC: Deliver `molecule-platform-mcp` as an entitlement-gated MCP plugin

- **Status:** Draft — pending CTO sign-off (arch + entitlement change)
- **Author:** devops-engineer (agent)
- **Date:** 2026-06-18
- **Related:** `rfc-platform-agent.md` §5.7, RFC #2843 (delivery decoupling),
  `MCPServerAdaptor` (runtime issue #847), marketplace/entitlement design,
  core PR #3044 (provider-pin seed, orthogonal), template PR #5 (inert).

## 1. Problem

The concierge (org-root `kind=platform` agent) is supposed to be the *Org
Concierge*: a privileged agent that manages the platform through a management
MCP (`molecule-platform-mcp`, 80+ org-admin tools incl. `create_workspace`,
`list_workspaces`). In production today it is **not** that agent.

Observed on prod tenant `test3` (2026-06-18), the concierge introspects as:

- System prompt: `"You are a Claude agent, built on Anthropic's Claude Agent SDK."` — the **generic** default, not the concierge persona.
- MCP servers: **`a2a` only** — the management MCP is **not wired**.
- `create_workspace` / `list_workspaces`: **absent**.

Asked to "spawn a test agent," it used Claude Code's **built-in `Task`
sub-agents** in `/workspace` (~17k tokens) instead of calling `create_workspace`
to create a EnterOS workspace. It is **vanilla Claude Code**, not the concierge.

### Root cause

The concierge identity + MCP wiring are delivered via the **asset/baked
channel** (template `config.yaml` with `prompt_files` + `mcp_servers.yaml`,
optionally a baked `molecule-platform-agent` image). On SaaS this channel does
**not** land: the on-box `/configs/config.yaml` is a **218-byte CP-regenerated
stub** —

```yaml
name: <wsid>
runtime: claude-code
a2a: {port: 8000, streaming: true}
model: 'moonshot/kimi-k2.6'
provider: 'platform'
runtime_config: {model: 'moonshot/kimi-k2.6', provider: 'platform'}
```

— with **no `prompt_files`** (→ generic prompt) and **no `mcp_servers`** (→ no
management MCP). No `concierge.md` or `mcp_servers.yaml` reaches the box. The CP
regenerates this stub on every (re)provision/restart, so even a correct template
`config.yaml` is overwritten.

Meanwhile the **plugin channel works**: the post-online reconcile reliably
installs declared plugins (e.g. the `seo-all` skill) into `/configs`.

## 2. Proposal

Deliver `molecule-platform-mcp` as an **entitlement-gated MCP-server plugin**,
declared by the platform-agent template and installed dynamically post-online —
**not** via the asset relay or a baked image.

This is not new machinery. The runtime already ships **`MCPServerAdaptor`**
(`molecule_runtime/plugins_registry/builtins.py`, issue #847), promoted to a
first-class adaptor after four MCP plugins shipped it (`molecule-firecrawl`
#512, `molecule-github-mcp` #520, `molecule-browser-use` #553, `mcp-connector`).
It deep-merges a plugin's `settings-fragment.json` `mcpServers` block into
`/configs/.claude/settings.json` — exactly where the Claude Code SDK reads MCP
servers.

### Plugin shape

`molecule-platform-mcp` ships a `settings-fragment.json`:

```json
{
  "mcpServers": {
    "molecule-platform": {
      "command": "npx",
      "args": ["-y", "@molecule-ai/mcp-server"],
      "env": {
        "MOLECULE_MCP_MODE": "management",
        "MOLECULE_API_URL": "${MOLECULE_API_URL}",
        "MOLECULE_ORG_API_KEY": "${MOLECULE_ORG_API_KEY}"
      }
    }
  }
}
```

The secret (`MOLECULE_ORG_API_KEY`) is **referenced, never embedded** — core
keeps injecting it into the container env via `conciergePlatformMCPEnv`. The
concierge persona prompt ships as the plugin's rule/`SKILL.md`-style content (or
remains the one small identity asset, per the RFC #2843 carve-out for
`config.yaml` + prompts).

### Wiring

- The platform-agent template declares `molecule-platform-mcp` in
  `workspace_declared_plugins`.
- The post-online reconcile resolves `MCPServerAdaptor` and installs it.
- `command: npx -y @molecule-ai/mcp-server` launches on demand → **no baked
  binary**, so the special `molecule-platform-agent` image is no longer required;
  the standard `claude-code` image + this plugin = concierge.

## 3. Why this is the right channel

1. **Uses the delivery path that works** (plugins) instead of the one that
   doesn't (asset/baked stub).
2. **Consistent with platform direction** — `feedback_skills_are_plugins_dynamic_install`:
   plugins install dynamically post-boot; the asset relay is for small
   identity/config only. An MCP server is a *capability*, so it belongs in the
   plugin channel.
3. **Retires a maintenance burden** — no special concierge image to build, pin,
   repin (a large share of recent incident toil), and MCP updates ship via the
   registry without an image rebuild.

## 4. Security — the load-bearing constraint

The management MCP holds the org-admin token and can create/delete workspaces.
It **MUST** be installable **only** on the org-root `kind=platform` concierge,
**never** on a user workspace. A normal public plugin-install path would be a
privilege-escalation hole.

Requirements:
- **Entitlement gate:** the platform-mcp plugin is installable only for the
  org-root concierge (enforced server-side at install/reconcile, keyed on
  `kind=platform` + org-root, not client-asserted). Ties into the
  marketplace/entitlement design.
- **Secret separation:** the org-admin token stays a core-injected container
  env var; the plugin only references it.
- **Audit:** install of the privileged plugin is logged like any org-admin
  action.

## 5. Migration / rollout

1. Land `molecule-platform-mcp` plugin (settings-fragment + persona content) in
   the registry; entitlement-gate to org-root.
2. Platform-agent template declares it in `workspace_declared_plugins`.
3. Add a **readiness gate**: the "Concierge Creates Workspace" e2e must wait for
   the management MCP to be present before asserting `create_workspace`, so the
   post-online install window doesn't false-red.
4. Re-provision concierges → they install the plugin and gain the management MCP.
5. **Deprecate** the baked `molecule-platform-agent` image and the asset
   `mcp_servers.yaml` path once the plugin path is green on a fresh provision.
6. Keep core PR #3044 (provider-pin seed) — orthogonal; it fixes *responsiveness*
   (model resolution), this RFC fixes *concierge capability* (identity + MCP).

## 6. Open questions for the CTO

1. **Entitlement mechanism** — reuse the marketplace entitlement broker, or a
   simpler core-side "org-root only" gate for this single privileged plugin to
   start?
2. **Persona prompt** — ship as part of this plugin, or keep the concierge
   system prompt in the (small) identity-asset channel and fix *that* delivery
   separately? (The MCP and the prompt are independent failures; this RFC
   primarily fixes the MCP.)
3. **`npx` on first turn vs. pre-bundled** — acceptable cold-start latency, or
   bundle the server in the plugin payload?
4. **Image deprecation** — retire `molecule-platform-agent` entirely, or keep it
   as an offline/self-host fallback (no registry reachability)?

## 7. Non-goals

- The general core↔runtime provider-derivation drift (tracked separately,
  template-claude-code issue #143).
- The CP config-regeneration-to-stub behavior — this RFC routes *around* it for
  the MCP; whether to also stop the stub clobbering `prompt_files` is open Q2.
