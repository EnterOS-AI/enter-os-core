# Contributing to Molecule AI

Thanks for your interest in contributing to Molecule AI! This guide covers the
development workflow, conventions, and how to get your changes merged.

## Getting Started

### Prerequisites

- **Go 1.25+** — platform backend
- **Node.js 20+** — canvas frontend
- **Python 3.11+** — workspace runtime
- **Docker** — infrastructure services (Postgres, Redis)
- **Git** — with hooks path set to `.githooks`
- **jq** — parses `manifest.json` during `setup.sh` to clone the
  template/plugin registry. Install via `brew install jq` (macOS) or
  `apt install jq` (Debian). Without it, setup.sh prints a note and
  leaves the registry dirs empty (recoverable by installing jq and
  re-running).

### Setup

```bash
# Clone the repo
git clone https://github.com/Molecule-AI/molecule-core.git
cd molecule-core

# Install git hooks
git config core.hooksPath .githooks

# Copy and edit .env (generate ADMIN_TOKEN + SECRETS_ENCRYPTION_KEY)
cp .env.example .env

# Start infrastructure (Postgres, Redis, Langfuse, Temporal)
./infra/scripts/setup.sh

# Build and run the platform — applies pending migrations on first boot
cd workspace-server
go run ./cmd/server

# In a separate terminal, run the canvas
cd canvas
npm install
npm run dev
```

### Environment Variables

Copy `.env.example` to `.env` and fill in your values:
```bash
cp .env.example .env
```

See `CLAUDE.md` for a full list of environment variables and their purposes.

## What goes where (content vs code)

This repo is scoped to **code** (canvas, workspace, workspace-server, related
infra). Public content (blog posts, marketing copy, OG images, SEO briefs,
DevRel demos) lives in [`Molecule-AI/docs`](https://github.com/Molecule-AI/docs).
The `Block forbidden paths` CI gate fails any PR that writes to `marketing/`
or other removed paths — open against `Molecule-AI/docs` instead.

| Content type | Target |
|---|---|
| Blog posts | `Molecule-AI/docs` → `content/blog/<YYYY-MM-DD-slug>/` |
| Doc pages | `Molecule-AI/docs` → `content/docs/` |
| Marketing copy / PMM positioning | `Molecule-AI/docs` → `marketing/` |
| OG images, visual assets | `Molecule-AI/docs` → `app/` or `marketing/` |
| SEO briefs | `Molecule-AI/docs` → `marketing/` |
| DevRel demos (runnable code) | Standalone repo under `Molecule-AI/`, OR embedded in `Molecule-AI/docs` |
| Launch checklists, internal tracking | GitHub Issues — **not** committed files |
| Engineering docs (`docs/adr/`, `docs/architecture/`, `docs/incidents/`) | This repo (internal, not published) |
| Live product pages (e.g. `canvas/src/app/pricing/page.tsx`) | This repo (these are app code, not marketing copy) |

If a PR fails the `Block forbidden paths` check, the contents belong in
`Molecule-AI/docs`. No CI drag, no Canvas E2E, content lands in minutes.

## Development Workflow

### Branch Naming

Use prefixed branches:
- `feat/` — new features
- `fix/` — bug fixes
- `chore/` — maintenance, deps, CI
- `docs/` — documentation only

**Never push directly to `main`.** All changes go through pull requests.

### Commits

Write concise commit messages that focus on the "why":
```
fix(canvas): prevent infinite re-render on WebSocket reconnect

The useEffect dependency array included the entire nodes object,
causing a render loop when any node position changed.
```

### Pull Requests

- Keep PRs focused — one concern per PR
- Include a test plan in the PR description
- PRs are merged with **merge commits** (not squash or rebase)

#### Auto-merge & the "extra commit" trap

**Two system guards protect against pushing commits after auto-merge has been enabled.** Don't try to work around them — they exist because we shipped a half-merged PR on 2026-04-27 (`#2174` merged with only its first commit; the second was orphaned on a branch GitHub had already deleted).

1. **Repo-wide:** "Automatically delete head branches" is on. Once a PR merges, the branch is deleted server-side. Any subsequent `git push` to that branch fails with `remote rejected — no such branch`.

2. **CI:** the `pr-guards` workflow (calling [molecule-ci `disable-auto-merge-on-push`](https://github.com/Molecule-AI/molecule-ci/blob/main/.github/workflows/disable-auto-merge-on-push.yml)) fires on every push to an open PR. If auto-merge was already enabled, it's disabled and a comment is posted. You must explicitly re-enable after verifying the new commit.

**Workflow rules that follow from the guards:**
- Push **all** commits before running `gh pr merge --auto`.
- If you realize you need another commit after enabling auto-merge: push it, then **re-run** `gh pr merge --auto` — the guard will already have disabled it. The disable + re-enable is the verification step.
- For changes that depend on each other across PRs (e.g. a build-script change + a workflow that consumes it), prefer a **stack** of PRs (PR-B branched off PR-A's branch, opened only after PR-A is in queue) over amending one in-flight PR.

### Running Tests

```bash
# Go (platform)
cd workspace-server && go test -race ./...

# Canvas (Next.js)
cd canvas && npm test

# Workspace runtime (Python)
cd workspace && python -m pytest -v

# E2E API tests (requires running platform)
bash tests/e2e/test_api.sh
```

### Pre-commit Hooks

The `.githooks/pre-commit` hook enforces:
- `'use client'` directive on React hook files
- Dark theme only (no white/light CSS classes)
- No SQL injection patterns (`fmt.Sprintf` with SQL)
- No leaked secrets (`sk-ant-`, `ghp_`, `AKIA`)

Fix violations before committing — the hook will reject the commit.

### CI Pipeline

CI runs on GitHub Actions with a self-hosted runner. External contributors:
PRs from forks will not trigger CI automatically. A maintainer will review
and run CI manually.

| Job | What it checks |
|-----|---------------|
| platform-build | Go build + vet + `go test -race` |
| canvas-build | npm build + vitest |
| python-lint | pytest with coverage |
| e2e-api | Full API test suite (62 tests) |
| shellcheck | Shell script linting |

## Code Style

### Go (Platform)
- Standard `gofmt` formatting
- `go vet` must pass
- No `fmt.Sprintf` in SQL queries (use parameterized queries)
- Prefer function injection over import cycles

### TypeScript (Canvas)
- Strict mode enabled
- No `any` types (use `unknown` or proper types)
- Use `ConfirmDialog` component, never native `confirm/alert/prompt`
- Dark theme only — no white/light CSS classes

### Python (Workspace Runtime)
- Type hints on public functions
- pytest for all tests

## External integrations

Code in this repo lands in molecule-core. Some related runtime artifacts
live in their own repos:

- [`Molecule-AI/molecule-ai-workspace-runtime`](https://github.com/Molecule-AI/molecule-ai-workspace-runtime) — Python adapter SDK (`molecule_runtime`) that runs inside containerized Molecule workspaces. Bridges Claude Code SDK / hermes / langgraph / etc. → A2A queue.
- [`Molecule-AI/molecule-sdk-python`](https://github.com/Molecule-AI/molecule-sdk-python) — `A2AServer` + `RemoteAgentClient` for external agents that register over the public `/registry/register` flow.
- [`Molecule-AI/molecule-mcp-claude-channel`](https://github.com/Molecule-AI/molecule-mcp-claude-channel) — Claude Code channel plugin. Bridges A2A traffic into a running Claude Code session via MCP `notifications/claude/channel`. Polling-based (no tunnel required); install with `claude --channels plugin:molecule@Molecule-AI/molecule-mcp-claude-channel`.

When extending the **A2A surface** in molecule-core (`workspace-server/internal/handlers/a2a_proxy.go` etc.), consider whether the change has a downstream impact on the runtime SDK or the channel plugin — they're versioned independently but share the wire shape.

## Architecture Overview

See `CLAUDE.md` for detailed architecture documentation, including:
- Component diagram (Platform, Canvas, Workspace Runtime)
- Key architectural patterns
- Database schema and migrations
- API route reference

## Reporting Issues

Use GitHub Issues with a clear title and reproduction steps. Include:
- What you expected
- What actually happened
- Platform/OS version
- Relevant logs or screenshots

## Security

If you discover a security vulnerability, please report it privately via
GitHub Security Advisories rather than opening a public issue.

## License

By contributing, you agree that your contributions will be licensed under the
same [Business Source License 1.1](LICENSE) that covers this project.
