---
title: "Give Your AI Agent a Git Repository: EnterOS + Cloudflare Artifacts"
date: 2026-04-21
slug: cloudflare-artifacts-molecule-ai
description: "Attach a Cloudflare Artifacts git repository to any EnterOS workspace. Import existing repos, fork for experiments, mint short-lived git credentials — all via the platform API. Git-native storage for AI agents."
tags: [Cloudflare, git, artifacts, AI-agents, workflow, tutorial]
---

# Give Your AI Agent a Git Repository: EnterOS + Cloudflare Artifacts

AI agents write code, generate assets, and produce artifacts. Most of the time, those artifacts live in memory — gone when the session ends. Even persistent agents have to choose between "keep everything in context" (expensive and slow) and "discard everything" (loses the work).

Cloudflare Artifacts changes this. Artifacts is Cloudflare's git-native object storage — git pull and git push semantics, backed by Cloudflare's global network. Think of it as a workspace filesystem that lives on the edge, is versioned by default, and talks git natively.

EnterOS's Artifacts integration attaches a Cloudflare Artifacts repository to any workspace. Your agent gets a git URL. It clones, commits, pushes, and pulls — using the same git workflow your team already knows.

This post covers what the integration does, how to configure it, and what you can build with it.

## Why Git-Native Storage for AI Agents

Most AI agent outputs — code drafts, generated configs, export files, test datasets — are transient. They live in the agent's working memory and evaporate when the session ends. Teams that want durable artifacts usually bolt on object storage (S3), a database, or a file share. All of those introduce a new API surface, new authentication scheme, and a new workflow.

Git-native storage is different because:

- **Agents already know git.** Clone, branch, commit, push. No new primitives to learn.
- **Versioning is structural.** Every change is a commit. Rollback is `git revert`. No "last writer wins" data loss.
- **Collaboration is native.** Fork a repo, experiment, open a PR. The same workflow humans use to collaborate applies to agents.
- **Cloudflare Artifacts is fast.** Git operations run on Cloudflare's edge — sub-100ms clone times from anywhere. No S3 bandwidth bills.
- **Access control is git-native.** Token scoping, branch protection, repo-level permissions. The same model your team already uses.

## API Reference

The integration exposes four endpoints, all behind workspace authentication:

### Attach a repository

```bash
# Create a new empty Artifacts repo linked to this workspace
curl -X POST https://platform.moleculesai.app/workspaces/${WORKSPACE_ID}/artifacts \
  -H "Authorization: Bearer ${WORKSPACE_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "pm-workspace-files",
    "description": "PM workspace — weekly reports and briefs"
  }'
```

```bash
# Or import from an existing Git URL (GitHub, GitLab, etc.)
curl -X POST https://platform.moleculesai.app/workspaces/${WORKSPACE_ID}/artifacts \
  -H "Authorization: Bearer ${WORKSPACE_TOKEN}" \
  -d '{
    "import_url": "https://github.com/acme/sprint-reports.git",
    "import_branch": "main",
    "import_depth": 0
  }'
```

### Get linked repository info

```bash
curl https://platform.moleculesai.app/workspaces/${WORKSPACE_ID}/artifacts \
  -H "Authorization: Bearer ${WORKSPACE_TOKEN}"
```

Returns the repo name, Cloudflare namespace, git remote URL, and creation timestamp.

### Fork the repository

```bash
curl -X POST https://platform.moleculesai.app/workspaces/${WORKSPACE_ID}/artifacts/fork \
  -H "Authorization: Bearer ${WORKSPACE_TOKEN}" \
  -d '{"name": "pm-workspace-files-experiment"}'
```

Creates a new Cloudflare Artifacts repo as a fork of the workspace's current repo. Useful when the agent wants to experiment without touching the canonical version.

### Mint a short-lived git credential

```bash
curl -X POST https://platform.moleculesai.app/workspaces/${WORKSPACE_ID}/artifacts/token \
  -H "Authorization: Bearer ${WORKSPACE_TOKEN}"
```

Returns a temporary git credential (username + password/token) scoped to this workspace's repo. Credentials expire automatically — no long-lived tokens to manage or revoke.

## Use Cases

### The agent that maintains its own documentation

A research agent that reads papers, summarizes findings, and writes notes. Without Artifacts, the notes disappear when the session ends. With Artifacts:

1. Agent clones the research repo on first run
2. Each session: pull latest, add summaries, commit and push
3. Next agent (or the same one, next day): clone and continue from where the last session left off

```bash
git clone https://repo.cf-articles.pages.dev/pm-research.git
# agent work...
git add -A && git commit -m "week 12 research summary" && git push
```

### Fork-before-experiment

A code-review agent that wants to test proposed changes before recommending them:

1. Fork the canonical repo to a temporary workspace
2. Apply the suggested patches
3. Run tests
4. Report results
5. Archive or discard the fork

The fork is a first-class API call — no manual git-fork workflow to script.

### Shared asset library for multi-agent teams

A design-team workspace maintains a shared palette of brand assets. Each agent in the team clones the Artifacts repo, uses the assets, and contributes updates. Because Artifacts is git-native, the history of asset changes is always visible — who changed what, when, and why.

## Security

The integration has two built-in security properties worth noting:

**SSRF protection on import.** Import URLs must use `https://`. The handler rejects `git://`, `http://`, or any other scheme at the router level before the URL is passed to the Cloudflare API. A request with `import_url: "http://internal.corp/repo"` returns a 400 immediately.

**Credential stripping on storage.** When Cloudflare creates a repo, it embeds a write credential in the git remote URL. Before persisting the remote URL to the database, EnterOS strips that credential. The DB stores the credential-free URL; the agent fetches a fresh short-lived token via the `/artifacts/token` endpoint on demand. Credentials are never stored long-term.

**Graceful unavailability.** If `CF_ARTIFACTS_API_TOKEN` or `CF_ARTIFACTS_NAMESPACE` are not configured, every Artifacts endpoint returns a 503 with a clear message: `"Cloudflare Artifacts not configured — set CF_ARTIFACTS_API_TOKEN and CF_ARTIFACTS_NAMESPACE"`. No silent failures or confusing empty responses.

## Getting Started

To use Artifacts in a self-hosted EnterOS deployment, set two environment variables on your platform instance:

```bash
CF_ARTIFACTS_API_TOKEN=your_cloudflare_api_token_with_artifacts_write
CF_ARTIFACTS_NAMESPACE=your_cloudflare_artifacts_namespace
```

Then create or import a repo via the API:

```bash
curl -X POST https://platform.moleculesai.app/workspaces/${WORKSPACE_ID}/artifacts \
  -H "Authorization: Bearer ${WORKSPACE_TOKEN}" \
  -d '{"name": "my-workspace-repo"}'
```

The response includes the git remote URL. Your agent can clone it immediately.

→ [Platform API Reference](/docs/api-protocol/platform-api)
→ [Cloudflare Artifacts Documentation](https://developers.cloudflare.com/artifacts/)

---

*EnterOS is open source. Artifacts support ships in `workspace-server/internal/handlers/artifacts.go` on `main`.*
