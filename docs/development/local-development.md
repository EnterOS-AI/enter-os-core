# Local Development

## Workspace Template Images: Local-Build Mode (Issue #63)

OSS contributors who run `molecule-core` locally do **not** need to authenticate to GHCR or AWS ECR. When the `MOLECULE_IMAGE_REGISTRY` env var is **unset**, the platform automatically:

1. Looks up the HEAD sha of `https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-<runtime>` (single API call, no clone).
2. If a local image tagged `molecule-local/workspace-template-<runtime>:<sha12>` already exists, reuses it (cache hit).
3. Otherwise, shallow-clones the repo into `~/.cache/molecule/workspace-template-build/<runtime>/<sha12>/` and runs `docker build --platform=linux/amd64 -t <tag> .`.
4. Hands the SHA-pinned tag to Docker for `ContainerCreate`.

**First-provision build time:** 5–10 min on Apple Silicon (amd64 emulation). Subsequent provisions hit the cache and start in seconds. Cache is invalidated automatically when the template repo's HEAD moves.

**Currently mirrored on Gitea:** `claude-code`, `hermes`, `langgraph`, `autogen`. Other runtimes (`crewai`, `deepagents`, `codex`, `gemini-cli`, `openclaw`) fail with an actionable "not mirrored to Gitea" error pointing at the missing repo.

**Production tenants are unaffected** — every prod tenant sets `MOLECULE_IMAGE_REGISTRY` to its private ECR mirror via Railway env / EC2 user-data, so the SaaS pull path stays identical.

### Environment overrides

| Var | Default | Use case |
|-----|---------|----------|
| `MOLECULE_IMAGE_REGISTRY` | (unset) | Set to a real registry URL to switch from local-build to SaaS-pull mode. |
| `MOLECULE_LOCAL_BUILD_CACHE` | `~/.cache/molecule/workspace-template-build` | Override cache directory. |
| `MOLECULE_LOCAL_TEMPLATE_REPO_PREFIX` | `https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-` | Point at a fork. |
| `MOLECULE_GITEA_TOKEN` | (unset) | Required only if your fork has private template repos. |

### Verifying a switch from the GHCR-retag stopgap

Pre-fix, OSS contributors worked around the suspended GHCR org by manually retagging an `:latest` image. After this change, that workaround is **redundant**: simply unset `MOLECULE_IMAGE_REGISTRY` (or leave it unset), boot the platform, and provision a workspace. Logs will show:

```
Provisioner: local-build mode → using locally-built image molecule-local/workspace-template-claude-code:<sha12> for runtime claude-code
local-build: cloning https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-claude-code → ...
local-build: docker build done in <duration>
```

If you still see `ghcr.io/molecule-ai/...` in the boot log, double-check `env | grep MOLECULE_IMAGE_REGISTRY` — a stale shell export from the pre-fix workaround could keep SaaS-mode active.

## Starting the Stack

```bash
docker compose up
```

This starts:

| Service | Port | Description |
|---------|------|-------------|
| Postgres | internal only | Primary database |
| Redis | internal only | Ephemeral state |
| Platform (Go) | `:8080` | Control plane API |
| Canvas (Next.js) | `:3000` | Visual frontend |
| Langfuse web | `:3001` (host) / `:3000` (internal) | Observability UI |
| Langfuse worker | — | Background processing |
| ClickHouse | — | Langfuse dependency |

Each workspace container is provisioned **on demand** by the platform when a user creates or imports one.

Langfuse uses a dedicated `langfuse` Postgres database. The compose stack creates it automatically before starting the Langfuse service, so it does not conflict with the platform's `molecule` schema.

### Infrastructure Only

To start just Postgres, Redis, and Langfuse (no application code):

```bash
docker compose -f docker-compose.infra.yml up
```

### Optional Profiles

```bash
docker compose --profile multi-provider up  # Add LiteLLM proxy (unified LLM API)
docker compose --profile local-models up    # Add Ollama (local LLM models)
```

## Environment Variables

### Platform (Go)

```
DATABASE_URL=postgres://dev:dev@postgres:5432/molecule?sslmode=prefer
REDIS_URL=redis://redis:6379
PORT=8080
SECRETS_ENCRYPTION_KEY=dev-key-change-in-production
WORKSPACE_DIR=/path/to/molecule-monorepo   # Optional global fallback; prefer per-workspace workspace_dir in org.yaml or API
```

### Canvas (Next.js)

```
NEXT_PUBLIC_PLATFORM_URL=http://localhost:8080
NEXT_PUBLIC_WS_URL=ws://localhost:8080/ws
```

### Workspace Runtime

```
WORKSPACE_ID=           # assigned by platform on provision
WORKSPACE_CONFIG_PATH=  # path to config folder inside container
MODEL_PROVIDER=         # e.g. anthropic:claude-sonnet-4-6
TIER=                   # 1, 2, 3, or 4
PLATFORM_URL=           # http://platform:8080
PARENT_ID=              # set by platform during team expansion (empty for top-level)
ANTHROPIC_API_KEY=      # or OPENAI_API_KEY, etc.
LANGFUSE_HOST=          # http://langfuse-web:3000 (internal container port; host-mapped to :3001)
LANGFUSE_PUBLIC_KEY=
LANGFUSE_SECRET_KEY=
LANGSMITH_TRACING=true  # LangGraph reads this to enable tracing
```

## Technology Versions

```
Go              1.25+ (go.mod)
Python          3.11+
Node.js         22+
Next.js         15
React Flow      12   (@xyflow/react)
a2a-sdk         0.3+ (A2A server SDK, install with a2a-sdk[http-server])
langfuse        3.x  (self-hosted Docker)
Postgres        16
Redis           7
Docker Compose  2.x
```

## Running Tests

### Unit Tests

```bash
cd workspace-server && go test -race ./...               # Go tests with race detection (695 tests)
cd canvas && npm test                            # Vitest tests (357 tests)
cd workspace && python -m pytest -v     # Workspace runtime tests (1140 tests)
cd sdk/python && python -m pytest -v             # SDK tests (121 tests)
cd mcp-server && npm test                        # MCP server tests (97 Jest tests)
```

### Integration Tests

```bash
bash tests/e2e/test_api.sh                 # 62 API tests (quick local verify; Phase 30.1 bearer-auth aware; also runs in CI)
bash tests/e2e/test_a2a_e2e.sh             # 22 A2A e2e tests (requires platform + 2 agents)
bash tests/e2e/test_activity_e2e.sh        # 25 activity/task E2E tests (requires platform + 1 agent)
bash tests/e2e/test_comprehensive_e2e.sh   # 67 endpoint/memory/bundle/approval checks
```

All E2E scripts share `tests/e2e/_lib.sh` helpers and are shellcheck-clean (enforced in CI). See [`./testing-e2e.md`](./testing-e2e.md) for auth prerequisites and what CI runs.

### CI Pipeline

GitHub Actions runs automatically on push to `main` and on PRs (`.github/workflows/ci.yml`):
- **platform-build** — Go build, vet, `go test -race` with coverage profiling (25% baseline threshold; setup-go uses module cache)
- **canvas-build** — npm build, `vitest run` (no `--passWithNoTests` -- tests must exist and pass)
- **mcp-server-build** — npm build
- **python-lint** — `pytest --cov=. --cov-report=term-missing` (pytest-cov enabled)
- **e2e-api** (added 2026-04-13) — Postgres + Redis service containers, migrations applied via `docker exec`, `tests/e2e/test_api.sh` must pass 62/62
- **shellcheck** (added 2026-04-13) — lints every `tests/e2e/*.sh`

Postgres and Redis are not exposed to the host -- use `docker compose exec postgres psql` or `docker compose exec redis redis-cli` for direct access.

## Utility Scripts

| Script | Purpose |
|--------|---------|
| `infra/scripts/setup.sh` | Initialize the local environment |
| `infra/scripts/nuke.sh` | Tear down and clean up everything |
| `bundle-compile.sh` | Compile workspace config folders into `.bundle.json` files |
| `test_api.sh` | Run 62 platform API integration tests |
| `test_a2a_e2e.sh` | Run 22 A2A end-to-end tests |
| `test_activity_e2e.sh` | Run 25 activity/task E2E tests |
| `setup-org.sh` | Create default 15-agent org hierarchy (PM + Marketing/Research/Dev teams, all Claude Code) |

## Related Docs

- [Architecture](../architecture/architecture.md) — System overview
- [Observability](./observability.md) — Langfuse details
