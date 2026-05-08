# Running a Gemini CLI Workspace on Molecule AI

Molecule AI now ships a `gemini-cli` runtime adapter alongside the existing `claude-code` adapter. This tutorial walks you from zero to a running Gemini agent workspace in under five minutes.

## What you'll need

- A Molecule AI account with at least one provisioned tenant
- A Google `GEMINI_API_KEY` (get one at [aistudio.google.com](https://aistudio.google.com))
- The Molecule AI CLI (`pip install molecule-ai`)

## Setup (10 steps)

```bash
# 1. Install / upgrade the CLI
pip install --upgrade molecule-ai

# 2. Authenticate
molecule auth login

# 3. Store your Gemini API key as a global secret
molecule secrets set GEMINI_API_KEY="YOUR_KEY_HERE" --global

# 4. Create a gemini-cli workspace
molecule workspace create my-gemini-agent --runtime gemini-cli

# 5. Confirm it's running (status → "ready" within ~30 s)
molecule workspace status my-gemini-agent

# 6. Send your first task
molecule workspace run my-gemini-agent "Summarise the last 5 git commits in this repo"

# 7. View the streamed response
molecule workspace logs my-gemini-agent --follow

# 8. Check the agent's memory file (GEMINI.md)
molecule workspace exec my-gemini-agent cat GEMINI.md

# 9. Delegate a cross-workspace task to your new Gemini peer
molecule workspace run orchestrator "delegate_task my-gemini-agent 'Draft release notes for v1.4'"

# 10. Tear down when done
molecule workspace delete my-gemini-agent
```

## Expected output

After step 5 you should see:
```
my-gemini-agent  gemini-cli  ready   ord   2026-04-16T06:30:00Z
```

After step 6, Gemini CLI streams its reasoning and final answer directly to stdout. The agent uses `GEMINI.md` (seeded from your workspace's `system-prompt.md`) as persistent context — equivalent to `CLAUDE.md` for Claude Code workspaces.

## How it works

Molecule AI's `gemini-cli` adapter mirrors the battle-tested `claude-code` pattern: a Docker image installs `@google/gemini-cli` globally, and `CLIAgentExecutor` drives the subprocess. Because Gemini CLI reads MCP config from `~/.gemini/settings.json` rather than accepting a `--mcp-config` flag, the adapter's `setup()` method merges the A2A MCP server definition into that file at boot — preserving any user-defined tools.

## Multi-provider teams

The real power surfaces when you mix runtimes on the same Molecule AI tenant. Your orchestrator workspace can delegate tasks to both `claude-code` and `gemini-cli` workers simultaneously using `delegate_task_async`, then synthesize results — all through the same A2A protocol. This is provider diversity at the infrastructure layer, not at the application layer.

## Related

- PR #379: [feat(adapters): add gemini-cli runtime adapter](https://git.moleculesai.app/molecule-ai/molecule-core/pull/379)
- [Multi-provider Hermes docs](../architecture/hermes.md)
- [Workspace runtimes reference](../reference/runtimes.md)
